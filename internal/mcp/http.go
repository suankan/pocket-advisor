package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultHTTPAddress         = "127.0.0.1:8080"
	defaultHTTPEndpoint        = "/mcp"
	defaultHTTPMaxRequestBytes = 1 << 20
	defaultHTTPMaxConcurrent   = 8
	defaultHTTPRequestTimeout  = 2 * time.Minute
	defaultHTTPShutdownTimeout = 30 * time.Second
)

// HTTPOptions configures the Streamable HTTP adapter: a plain, unauthenticated
// HTTP service bound to loopback. TLS, authentication, and public exposure
// belong to whatever gateway is later put in front of it, not to this process.
type HTTPOptions struct {
	Address         string
	Endpoint        string
	MaxRequestBytes int64
	MaxConcurrent   int
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	Readiness       func(context.Context) error
}

// HTTPServer is one fixed-workspace MCP server. It never reads a workspace
// selector from HTTP or MCP input.
type HTTPServer struct {
	opts HTTPOptions
	tool *QueryTool

	httpServer *http.Server
}

// NewHTTPServer validates the options before a listener can be opened.
func NewHTTPServer(base *QueryTool, opts HTTPOptions) (*HTTPServer, error) {
	if base == nil || base.Service == nil || strings.TrimSpace(base.Workspace) == "" {
		return nil, fmt.Errorf("mcp http requires a fixed workspace retrieval tool")
	}
	if err := normalizeHTTPOptions(&opts); err != nil {
		return nil, err
	}

	s := &HTTPServer{opts: opts, tool: base}
	server := newSDKServer(base)
	transport := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		Logger:                       nil,
		MaxRequestBodyBytes:          opts.MaxRequestBytes,
		PropagateRequestCancellation: true,
	})

	mux := http.NewServeMux()
	mux.Handle(opts.Endpoint, s.limitConcurrency(s.timeout(transport)))
	mux.HandleFunc("/livez", healthOK)
	mux.HandleFunc("/readyz", s.serveReady)

	s.httpServer = &http.Server{
		Addr:              opts.Address,
		Handler:           mux,
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
	// Nothing here authenticates a caller, so the listener stays on this
	// machine. Exposing it further is a gateway's job.
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mcp http must bind to an explicit loopback address; expose it beyond this machine through a gateway")
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
	return nil
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
// on cancellation.
func (s *HTTPServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return err
	}
	defer s.tool.closeSnapshots()
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
