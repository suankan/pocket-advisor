package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const (
	defaultHTTPAddress          = "127.0.0.1:8080"
	defaultHTTPEndpoint         = "/mcp"
	defaultHTTPScope            = "pocket-advisor:retrieve"
	defaultHTTPMaxRequestBytes  = 1 << 20
	defaultHTTPMaxConcurrent    = 8
	defaultHTTPRequestTimeout   = 2 * time.Minute
	defaultHTTPShutdownTimeout  = 30 * time.Second
	defaultPrincipalIdleTimeout = 15 * time.Minute
	defaultMaxCallerStates      = 128
	defaultMaxTokenLifetime     = 15 * time.Minute
)

// HTTPOptions configures the authenticated Streamable HTTP adapter. The Go
// process is deliberately a loopback-only backend: the selected Caddy sidecar
// is the sole network listener and TLS boundary in a deployed workload.
type HTTPOptions struct {
	Address                 string
	Endpoint                string
	ResourceURI             string
	AuthorizationServer     string
	IntrospectionEndpoint   string
	IntrospectionClientID   string
	IntrospectionSecret     string
	RequiredScope           string
	AllowedOrigins          []string
	AllowedHosts            []string
	TrustedProxyCIDRs       []string
	MaxRequestBytes         int64
	MaxConcurrent           int
	RequestTimeout          time.Duration
	ShutdownTimeout         time.Duration
	PrincipalIdleTimeout    time.Duration
	MaxCallerStates         int
	MaxTokenLifetime        time.Duration
	introspectionHTTPClient *http.Client
	Readiness               func(context.Context) error
}

// HTTPServer is one fixed-workspace authenticated MCP resource server. Tool
// state is partitioned by OAuth subject. It never reads a workspace selector
// from HTTP, OAuth, or MCP input.
type HTTPServer struct {
	opts HTTPOptions
	base *QueryTool

	httpServer *http.Server
	statesMu   sync.Mutex
	states     map[string]*callerState
}

type callerState struct {
	server   *sdkmcp.Server
	tool     *QueryTool
	lastUsed time.Time
}

// NewHTTPServer validates the complete authenticated boundary before a
// listener can be opened. The introspection secret is supplied by the
// environment or a mounted Kubernetes Secret, never committed configuration.
func NewHTTPServer(base *QueryTool, opts HTTPOptions) (*HTTPServer, error) {
	if base == nil || base.Service == nil || strings.TrimSpace(base.Workspace) == "" {
		return nil, fmt.Errorf("mcp http requires a fixed workspace retrieval tool")
	}
	if err := normalizeHTTPOptions(&opts); err != nil {
		return nil, err
	}
	verifier, err := newIntrospectionVerifier(opts)
	if err != nil {
		return nil, err
	}

	s := &HTTPServer{opts: opts, base: base, states: make(map[string]*callerState)}
	transport := sdkmcp.NewStreamableHTTPHandler(s.serverForRequest, &sdkmcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The SDK's default localhost DNS-rebinding guard rejects any request
		// whose Host is not loopback when the server binds loopback. This
		// deployment binds loopback on purpose: Caddy terminates TLS and
		// forwards the public Host to 127.0.0.1. Origin, Host, and forwarded
		// header validation are enforced by secureEnvelope against an explicit
		// allowlist and trusted-proxy set, so the SDK guard is redundant and
		// must be disabled or it would refuse every real request.
		DisableLocalhostProtection: true,
		// Generic protocol logs can include values that are private in this
		// service. Pocket Advisor emits its own bounded operational events.
		Logger:                       nil,
		SessionTimeout:               opts.PrincipalIdleTimeout,
		MaxRequestBodyBytes:          opts.MaxRequestBytes,
		PropagateRequestCancellation: true,
	})

	metadataURL, metadataPath, err := protectedResourceMetadataLocation(opts.ResourceURI)
	if err != nil {
		return nil, err
	}
	authorized := mcpauth.RequireBearerToken(verifier.Verify, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
		Scopes:              []string{opts.RequiredScope},
		ClockSkew:           5 * time.Second,
	})(s.limitRate(transport))
	// Bound and time out authorization-server work as well as MCP execution.
	// The envelope remains outermost so invalid Host, Origin, and forwarding
	// headers never consume an introspection request.
	protected := s.limitConcurrency(s.timeout(authorized))

	mux := http.NewServeMux()
	mux.Handle(opts.Endpoint, protected)
	mux.HandleFunc(metadataPath, s.serveProtectedResourceMetadata)
	mux.HandleFunc("/livez", healthOK)
	mux.HandleFunc("/readyz", s.serveReady)

	handler := s.secureEnvelope(mux)
	s.httpServer = &http.Server{
		Addr:              opts.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	return s, nil
}

func normalizeHTTPOptions(opts *HTTPOptions) error {
	if opts.Address == "" {
		opts.Address = defaultHTTPAddress
	}
	if opts.Endpoint == "" {
		opts.Endpoint = defaultHTTPEndpoint
	}
	if !strings.HasPrefix(opts.Endpoint, "/") || strings.ContainsAny(opts.Endpoint, "?#") {
		return fmt.Errorf("mcp http endpoint must be an absolute path")
	}
	host, _, err := net.SplitHostPort(opts.Address)
	if err != nil {
		return fmt.Errorf("mcp http address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mcp http backend must bind to an explicit loopback address behind the authenticated TLS gateway")
	}
	resource, err := url.Parse(opts.ResourceURI)
	if err != nil || resource.Scheme != "https" || resource.Host == "" || resource.Fragment != "" || resource.RawQuery != "" {
		return fmt.Errorf("mcp resource URI must be an absolute HTTPS URI without query or fragment")
	}
	if resource.Path != opts.Endpoint {
		return fmt.Errorf("mcp resource URI path must equal endpoint %q", opts.Endpoint)
	}
	issuer, err := url.Parse(opts.AuthorizationServer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.RawQuery != "" || issuer.Fragment != "" {
		return fmt.Errorf("mcp authorization server must be an absolute HTTPS issuer URI")
	}
	introspection, err := url.Parse(opts.IntrospectionEndpoint)
	if err != nil || introspection.Scheme != "https" || introspection.Host == "" || introspection.RawQuery != "" || introspection.Fragment != "" {
		return fmt.Errorf("mcp introspection endpoint must be an absolute HTTPS URI")
	}
	if opts.IntrospectionClientID == "" || opts.IntrospectionSecret == "" {
		return fmt.Errorf("mcp introspection client id and secret are required")
	}
	if opts.RequiredScope == "" {
		opts.RequiredScope = defaultHTTPScope
	}
	if fields := strings.Fields(opts.RequiredScope); len(fields) != 1 || fields[0] != opts.RequiredScope {
		return fmt.Errorf("mcp required scope must be one OAuth scope token")
	}
	if opts.MaxRequestBytes == 0 {
		opts.MaxRequestBytes = defaultHTTPMaxRequestBytes
	}
	if opts.MaxRequestBytes < 1 || opts.MaxRequestBytes > maxRequestMessageSize {
		return fmt.Errorf("mcp http max request bytes must be between 1 and %d", maxRequestMessageSize)
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = defaultHTTPMaxConcurrent
	}
	if opts.MaxConcurrent < 1 || opts.MaxConcurrent > 128 {
		return fmt.Errorf("mcp http max concurrent must be between 1 and 128")
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = defaultHTTPRequestTimeout
	}
	if opts.RequestTimeout < time.Second || opts.RequestTimeout > 5*time.Minute {
		return fmt.Errorf("mcp http request timeout must be between one second and five minutes")
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = defaultHTTPShutdownTimeout
	}
	if opts.ShutdownTimeout < time.Second || opts.ShutdownTimeout > 5*time.Minute {
		return fmt.Errorf("mcp http shutdown timeout must be between one second and five minutes")
	}
	if opts.PrincipalIdleTimeout == 0 {
		opts.PrincipalIdleTimeout = defaultPrincipalIdleTimeout
	}
	if opts.PrincipalIdleTimeout < time.Minute || opts.PrincipalIdleTimeout > time.Hour {
		return fmt.Errorf("mcp http caller idle timeout must be between one minute and one hour")
	}
	if opts.MaxCallerStates == 0 {
		opts.MaxCallerStates = defaultMaxCallerStates
	}
	if opts.MaxCallerStates < 1 || opts.MaxCallerStates > 4096 {
		return fmt.Errorf("mcp http max caller states must be between 1 and 4096")
	}
	if opts.MaxTokenLifetime == 0 {
		opts.MaxTokenLifetime = defaultMaxTokenLifetime
	}
	if opts.MaxTokenLifetime < time.Minute || opts.MaxTokenLifetime > defaultMaxTokenLifetime {
		return fmt.Errorf("mcp http maximum token lifetime must be between one and fifteen minutes")
	}
	if len(opts.AllowedHosts) == 0 {
		opts.AllowedHosts = []string{resource.Host}
	}
	for _, allowedHost := range opts.AllowedHosts {
		if allowedHost == "" || strings.ContainsAny(allowedHost, "/?#@ \t\r\n") {
			return fmt.Errorf("allowed host %q must be an exact HTTP Host value", allowedHost)
		}
	}
	for _, origin := range opts.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return fmt.Errorf("allowed origin %q: %w", origin, err)
		}
	}
	for _, raw := range opts.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(raw); err != nil {
			return fmt.Errorf("trusted proxy CIDR %q: %w", raw, err)
		}
	}
	return nil
}

func (s *HTTPServer) serverForRequest(r *http.Request) *sdkmcp.Server {
	info := mcpauth.TokenInfoFromContext(r.Context())
	if info == nil || info.UserID == "" {
		return nil
	}
	now := time.Now()
	s.statesMu.Lock()
	defer s.statesMu.Unlock()
	for id, state := range s.states {
		if now.Sub(state.lastUsed) > s.opts.PrincipalIdleTimeout {
			state.tool.closeSnapshots()
			delete(s.states, id)
		}
	}
	state := s.states[info.UserID]
	if state == nil {
		if len(s.states) >= s.opts.MaxCallerStates {
			var oldestID string
			var oldestTime time.Time
			for id, candidate := range s.states {
				if oldestID == "" || candidate.lastUsed.Before(oldestTime) {
					oldestID, oldestTime = id, candidate.lastUsed
				}
			}
			s.states[oldestID].tool.closeSnapshots()
			delete(s.states, oldestID)
		}
		tool := s.base.forCaller()
		state = &callerState{tool: tool, server: newSDKServer(tool)}
		s.states[info.UserID] = state
	}
	state.lastUsed = now
	return state.server
}

func newSDKServer(tool *QueryTool) *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name: "pocket-advisor", Title: "Pocket Advisor Evidence", Version: "1",
	}, &sdkmcp.ServerOptions{
		Instructions: "This server returns private workspace evidence, not generated answers. Cite complete result-scoped references. When complete=false, call continuation_tool with exactly next_cursor until complete=true.",
		Logger:       nil,
		Capabilities: &sdkmcp.ServerCapabilities{},
	})
	for _, definition := range tool.DescribeAll() {
		definition := definition
		readOnly, destructive, idempotent, openWorld := definition.Annotations.ReadOnlyHint, definition.Annotations.DestructiveHint, definition.Annotations.IdempotentHint, definition.Annotations.OpenWorldHint
		sdkTool := &sdkmcp.Tool{
			Name: definition.Name, Title: definition.Title, Description: definition.Description,
			InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
			Annotations: &sdkmcp.ToolAnnotations{
				ReadOnlyHint: readOnly, DestructiveHint: &destructive,
				IdempotentHint: idempotent, OpenWorldHint: &openWorld,
			},
		}
		server.AddTool(sdkTool, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			if req.Params == nil || len(req.Params.InputResponses) != 0 || req.Params.RequestState != "" {
				return sdkErrorResult(&argumentError{message: "task-augmented execution is not supported"}), nil
			}
			raw, err := json.Marshal(rawCallParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
			if err != nil {
				return nil, err
			}
			result, err := tool.Call(ctx, raw)
			if err != nil {
				return sdkErrorResult(err), nil
			}
			return sdkResult(result), nil
		})
	}
	return server
}

func sdkResult(result CallToolResult) *sdkmcp.CallToolResult {
	content := make([]sdkmcp.Content, 0, len(result.Content))
	for _, item := range result.Content {
		content = append(content, &sdkmcp.TextContent{Text: item.Text})
	}
	return &sdkmcp.CallToolResult{Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError}
}

func sdkErrorResult(err error) *sdkmcp.CallToolResult { return sdkResult(errorResult(err)) }

func (s *HTTPServer) serveProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	metadata := oauthex.ProtectedResourceMetadata{
		Resource: s.opts.ResourceURI, AuthorizationServers: []string{s.opts.AuthorizationServer},
		ScopesSupported: []string{s.opts.RequiredScope}, BearerMethodsSupported: []string{"header"},
		ResourceName: "Pocket Advisor Evidence",
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(metadata)
}

func protectedResourceMetadataLocation(resourceURI string) (metadataURL, metadataPath string, err error) {
	u, err := url.Parse(resourceURI)
	if err != nil {
		return "", "", err
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	metadataPath = "/.well-known/oauth-protected-resource" + path
	if path == "" {
		metadataPath = "/.well-known/oauth-protected-resource"
	}
	return u.Scheme + "://" + u.Host + metadataPath, metadataPath, nil
}

func healthOK(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

func (s *HTTPServer) serveReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Readiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.opts.Readiness(ctx); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"unavailable"}`+"\n")
			return
		}
	}
	healthOK(w, r)
}

// Serve opens the validated loopback listener and drains in-flight HTTP work
// on cancellation. Caddy owns the externally reachable TLS socket.
func (s *HTTPServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return err
	}
	defer s.closeStates()
	done := make(chan error, 1)
	go func() { done <- s.httpServer.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		err := s.httpServer.Shutdown(shutdownCtx)
		if err != nil {
			_ = s.httpServer.Close()
			return fmt.Errorf("mcp http shutdown: %w", err)
		}
		return nil
	}
}

func (s *HTTPServer) closeStates() {
	s.statesMu.Lock()
	defer s.statesMu.Unlock()
	for id, state := range s.states {
		state.tool.closeSnapshots()
		delete(s.states, id)
	}
}

func (s *HTTPServer) secureEnvelope(next http.Handler) http.Handler {
	trusted := make([]netip.Prefix, 0, len(s.opts.TrustedProxyCIDRs))
	for _, raw := range s.opts.TrustedProxyCIDRs {
		prefix, _ := netip.ParsePrefix(raw) // validated by normalizeHTTPOptions
		trusted = append(trusted, prefix)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/livez" && r.URL.Path != "/readyz" {
			if r.URL.Path == s.opts.Endpoint && hasAmbiguousMCPHeaders(r) {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if !slices.Contains(s.opts.AllowedHosts, r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if hasForwardedHeaders(r.Header) && !remoteInPrefixes(r.RemoteAddr, trusted) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			origins := r.Header.Values("Origin")
			if len(origins) > 1 || (len(origins) == 1 && !slices.Contains(s.opts.AllowedOrigins, origins[0])) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hasAmbiguousMCPHeaders(r *http.Request) bool {
	for _, name := range []string{
		"Authorization", "Content-Type", "Accept", "MCP-Protocol-Version", "Mcp-Method", "Mcp-Name", "Mcp-Session-Id",
	} {
		if len(r.Header.Values(name)) > 1 {
			return true
		}
	}
	// net/http rejects this framing on a real listener. Keep the same rule at
	// the handler boundary so tests and any future in-process adapter cannot
	// accidentally accept an ambiguous body.
	return r.ContentLength >= 0 && len(r.TransferEncoding) != 0
}

func validateOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an exact HTTP(S) origin")
	}
	return nil
}

func hasForwardedHeaders(h http.Header) bool {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}

func remoteInPrefixes(remote string, prefixes []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *HTTPServer) timeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *HTTPServer) limitConcurrency(next http.Handler) http.Handler {
	sem := make(chan struct{}, s.opts.MaxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	})
}

// limitRate applies a deliberately small per-caller fixed window. It is a
// backstop for accidental loops; concurrency and upstream gateway limits are
// the primary resource bounds.
func (s *HTTPServer) limitRate(next http.Handler) http.Handler {
	type window struct {
		start time.Time
		count int
	}
	var mu sync.Mutex
	windows := make(map[string]window)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := mcpauth.TokenInfoFromContext(r.Context())
		if info == nil || info.UserID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		now := time.Now()
		mu.Lock()
		current, exists := windows[info.UserID]
		if !exists && len(windows) >= s.opts.MaxCallerStates {
			var oldestID string
			var oldestTime time.Time
			for id, candidate := range windows {
				if oldestID == "" || candidate.start.Before(oldestTime) {
					oldestID, oldestTime = id, candidate.start
				}
			}
			delete(windows, oldestID)
		}
		if current.start.IsZero() || now.Sub(current.start) >= time.Minute {
			current = window{start: now}
		}
		current.count++
		windows[info.UserID] = current
		allowed := current.count <= 120
		mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type introspectionVerifier struct {
	endpoint, clientID, secret, issuer, resource, scope string
	maxLifetime                                         time.Duration
	http                                                *http.Client
}

func newIntrospectionVerifier(opts HTTPOptions) (*introspectionVerifier, error) {
	client := opts.introspectionHTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	if clientCopy.Timeout == 0 || clientCopy.Timeout > 10*time.Second {
		clientCopy.Timeout = 10 * time.Second
	}
	// Never forward confidential resource-server credentials to a redirect
	// target. An introspection endpoint is a configured, exact HTTPS URL.
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &introspectionVerifier{
		endpoint: opts.IntrospectionEndpoint, clientID: opts.IntrospectionClientID,
		secret: opts.IntrospectionSecret, issuer: opts.AuthorizationServer,
		resource: opts.ResourceURI, scope: opts.RequiredScope, maxLifetime: opts.MaxTokenLifetime,
		http: &clientCopy,
	}, nil
}

type introspectionResponse struct {
	Active    bool            `json:"active"`
	Scope     string          `json:"scope"`
	Subject   string          `json:"sub"`
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Expires   int64           `json:"exp"`
	IssuedAt  int64           `json:"iat"`
	NotBefore int64           `json:"nbf"`
}

func (v *introspectionVerifier) Verify(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, invalidToken()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(v.clientID, v.secret)
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, invalidToken()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, invalidToken()
	}
	var out introspectionResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&out); err != nil || !out.Active || out.Subject == "" || out.Expires == 0 || out.IssuedAt == 0 {
		return nil, invalidToken()
	}
	now := time.Now()
	expires := time.Unix(out.Expires, 0)
	issued := time.Unix(out.IssuedAt, 0)
	if !expires.After(now) || issued.After(now.Add(5*time.Second)) || expires.Sub(issued) > v.maxLifetime {
		return nil, invalidToken()
	}
	if out.NotBefore != 0 && time.Unix(out.NotBefore, 0).After(now.Add(5*time.Second)) {
		return nil, invalidToken()
	}
	if subtle.ConstantTimeCompare([]byte(out.Issuer), []byte(v.issuer)) != 1 {
		return nil, invalidToken()
	}
	audiences, err := stringClaim(out.Audience)
	if err != nil || !slices.Contains(audiences, v.resource) {
		return nil, invalidToken()
	}
	scopes := strings.Fields(out.Scope)
	if !slices.Contains(scopes, v.scope) {
		// Return valid token information and let RequireBearerToken produce the
		// specification's 403 insufficient_scope challenge.
		return &mcpauth.TokenInfo{Scopes: scopes, Expiration: expires, UserID: v.issuer + "\x00" + out.Subject}, nil
	}
	return &mcpauth.TokenInfo{Scopes: scopes, Expiration: expires, UserID: v.issuer + "\x00" + out.Subject}, nil
}

func stringClaim(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func invalidToken() error { return fmt.Errorf("%w", mcpauth.ErrInvalidToken) }
