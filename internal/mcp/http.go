package mcp

import (
	"context"
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

	"github.com/coreos/go-oidc/v3/oidc"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const (
	defaultHTTPAddress          = "127.0.0.1:8080"
	defaultHTTPEndpoint         = "/mcp"
	defaultHTTPMaxRequestBytes  = 1 << 20
	defaultHTTPMaxConcurrent    = 8
	defaultHTTPRequestTimeout   = 2 * time.Minute
	defaultHTTPShutdownTimeout  = 30 * time.Second
	defaultPrincipalIdleTimeout = 15 * time.Minute
	defaultMaxCallerStates      = 128

	// googleIssuerURL is the fixed, non-configurable Google OIDC issuer.
	// Authenticated HTTP mode supports Google as the sole identity provider,
	// so there is no operator-supplied authorization-server setting to get
	// wrong or to silently point at something other than Google.
	googleIssuerURL = "https://accounts.google.com"
)

// HTTPOptions configures the Streamable HTTP adapter. The Go process is
// deliberately a loopback-only backend by default; CertFile/KeyFile let it
// terminate TLS itself for direct remote/local-network use, and a reverse
// proxy remains an option for anything more elaborate.
type HTTPOptions struct {
	Address              string
	Endpoint             string
	ResourceURI          string
	GoogleClientID       string
	AllowedEmails        []string
	CertFile             string
	KeyFile              string
	AllowedOrigins       []string
	AllowedHosts         []string
	TrustedProxyCIDRs    []string
	MaxRequestBytes      int64
	MaxConcurrent        int
	RequestTimeout       time.Duration
	ShutdownTimeout      time.Duration
	PrincipalIdleTimeout time.Duration
	MaxCallerStates      int
	// googleIssuer and googleHTTPClient are test-only overrides: production
	// always verifies against the real googleIssuerURL over the default HTTP
	// client. Tests point these at a local fake OIDC provider.
	googleIssuer     string
	googleHTTPClient *http.Client
	Readiness        func(context.Context) error
}

// HTTPServer is one fixed-workspace MCP resource server, optionally
// authenticated. Tool state is partitioned by OAuth subject when
// authenticated. It never reads a workspace selector
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

// NewHTTPServer validates the complete boundary before a listener can be
// opened. GoogleClientID/AllowedEmails are committed-configuration-safe (no
// secret is required to verify a Google ID token); leaving GoogleClientID
// empty runs the server unauthenticated on loopback for local development.
func NewHTTPServer(ctx context.Context, base *QueryTool, opts HTTPOptions) (*HTTPServer, error) {
	if base == nil || base.Service == nil || strings.TrimSpace(base.Workspace) == "" {
		return nil, fmt.Errorf("mcp http requires a fixed workspace retrieval tool")
	}
	if err := normalizeHTTPOptions(&opts); err != nil {
		return nil, err
	}

	s := &HTTPServer{opts: opts, base: base, states: make(map[string]*callerState)}
	transport := sdkmcp.NewStreamableHTTPHandler(s.serverForRequest, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		DisableLocalhostProtection:   true,
		Logger:                       nil,
		SessionTimeout:               opts.PrincipalIdleTimeout,
		MaxRequestBodyBytes:          opts.MaxRequestBytes,
		PropagateRequestCancellation: true,
	})

	// Google auth is optional. Skip authentication if not configured.
	var handler http.Handler
	metadataPath := ""
	if opts.GoogleClientID != "" {
		verifier, err := newGoogleVerifier(ctx, opts)
		if err != nil {
			return nil, err
		}
		metadataURL, mp, err := protectedResourceMetadataLocation(opts.ResourceURI)
		if err != nil {
			return nil, err
		}
		metadataPath = mp
		handler = mcpauth.RequireBearerToken(verifier.Verify, &mcpauth.RequireBearerTokenOptions{
			ResourceMetadataURL: metadataURL,
			ClockSkew:           5 * time.Second,
		})(s.limitRate(transport))
	} else {
		// No Google auth - allow all requests (local development)
		handler = s.limitRate(transport)
	}
	// Bound and time out token verification as well as MCP execution. The
	// envelope remains outermost so invalid Host, Origin, and forwarding
	// headers never reach token verification at all.
	protected := s.limitConcurrency(s.timeout(handler))

	mux := http.NewServeMux()
	mux.Handle(opts.Endpoint, protected)
	if metadataPath != "" {
		mux.HandleFunc(metadataPath, s.serveProtectedResourceMetadata)
	}
	mux.HandleFunc("/livez", healthOK)
	mux.HandleFunc("/readyz", s.serveReady)

	handler = s.secureEnvelope(mux)
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
		return fmt.Errorf("mcp http backend must bind to an explicit loopback address; use a reverse proxy or SSH tunnel for remote access")
	}
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return fmt.Errorf("mcp http tls requires both a certificate file and a key file")
	}

	// Google auth is optional. When configured, the resource boundary and
	// allowlist are validated strictly; when not, this is local unauthenticated
	// loopback development and those checks do not apply. Either way, the
	// shared limits below apply — they are not conditional on auth.
	var resource *url.URL
	if opts.GoogleClientID != "" {
		if opts.googleIssuer == "" {
			opts.googleIssuer = googleIssuerURL
		}
		resource, err = url.Parse(opts.ResourceURI)
		if err != nil || resource.Scheme != "https" || resource.Host == "" || resource.Fragment != "" || resource.RawQuery != "" {
			return fmt.Errorf("mcp resource URI must be an absolute HTTPS URI without query or fragment")
		}
		if resource.Path != opts.Endpoint {
			return fmt.Errorf("mcp resource URI path must equal endpoint %q", opts.Endpoint)
		}
		if len(opts.AllowedEmails) == 0 {
			return fmt.Errorf("mcp google oauth requires at least one allowed email")
		}
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
	if len(opts.AllowedHosts) == 0 && resource != nil {
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
		Resource: s.opts.ResourceURI, AuthorizationServers: []string{s.opts.googleIssuer},
		ScopesSupported: []string{"openid", "email"}, BearerMethodsSupported: []string{"header"},
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
// on cancellation. With CertFile/KeyFile configured this terminates TLS
// itself; otherwise it serves plain HTTP, e.g. behind a reverse proxy that
// terminates TLS instead.
func (s *HTTPServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return err
	}
	defer s.closeStates()
	done := make(chan error, 1)
	if s.opts.CertFile != "" {
		go func() { done <- s.httpServer.ServeTLS(listener, s.opts.CertFile, s.opts.KeyFile) }()
	} else {
		go func() { done <- s.httpServer.Serve(listener) }()
	}
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
			// Host and Origin allowlists are only optional in the genuinely
			// unauthenticated local-dev case (no Google auth configured). An
			// authenticated deployment always enforces them, even if the
			// operator left AllowedHosts/AllowedOrigins unset — AllowedHosts
			// defaults from ResourceURI's host in that case (see
			// normalizeHTTPOptions), so it is never actually empty when
			// GoogleClientID is set.
			localDev := s.opts.GoogleClientID == ""
			if !(localDev && len(s.opts.AllowedHosts) == 0) && !slices.Contains(s.opts.AllowedHosts, r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if hasForwardedHeaders(r.Header) && !remoteInPrefixes(r.RemoteAddr, trusted) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			origins := r.Header.Values("Origin")
			if !(localDev && len(s.opts.AllowedOrigins) == 0) && (len(origins) > 1 || (len(origins) == 1 && !slices.Contains(s.opts.AllowedOrigins, origins[0]))) {
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

// googleVerifier verifies bearer tokens as Google-issued OIDC ID tokens: a
// signature/issuer/audience/expiry check via go-oidc against Google's
// published JWKS (fetched and cached by the library, not looked up per
// request against a confidential introspection endpoint — Google ID tokens
// are self-contained, so there is no secret to hold on the resource-server
// side), followed by an application-level allowlist check on the verified
// email claim.
type googleVerifier struct {
	verifier *oidc.IDTokenVerifier
	allowed  map[string]bool
}

func newGoogleVerifier(ctx context.Context, opts HTTPOptions) (*googleVerifier, error) {
	if opts.googleHTTPClient != nil {
		ctx = oidc.ClientContext(ctx, opts.googleHTTPClient)
	}
	provider, err := oidc.NewProvider(ctx, opts.googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("google oidc discovery: %w", err)
	}
	allowed := make(map[string]bool, len(opts.AllowedEmails))
	for _, email := range opts.AllowedEmails {
		allowed[strings.ToLower(strings.TrimSpace(email))] = true
	}
	return &googleVerifier{
		verifier: provider.Verifier(&oidc.Config{ClientID: opts.GoogleClientID}),
		allowed:  allowed,
	}, nil
}

func (v *googleVerifier) Verify(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
	idToken, err := v.verifier.Verify(ctx, token)
	if err != nil {
		return nil, invalidToken()
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Email == "" {
		return nil, invalidToken()
	}
	if !claims.EmailVerified || !v.allowed[strings.ToLower(claims.Email)] {
		return nil, invalidToken()
	}
	return &mcpauth.TokenInfo{Expiration: idToken.Expiry, UserID: idToken.Issuer + "\x00" + idToken.Subject}, nil
}

func invalidToken() error { return fmt.Errorf("%w", mcpauth.ErrInvalidToken) }
