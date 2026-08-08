package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type tokenRecord struct {
	active bool
	sub    string
	aud    string
	scope  string
	issuer string
	exp    time.Time
	iat    time.Time
}

type testAuthorizationServer struct {
	server *httptest.Server
	mu     sync.Mutex
	tokens map[string]tokenRecord
	calls  int
}

func newTestAuthorizationServer(t *testing.T) *testAuthorizationServer {
	t.Helper()
	as := &testAuthorizationServer{tokens: make(map[string]tokenRecord)}
	as.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.mu.Lock()
		defer as.mu.Unlock()
		as.calls++
		if user, secret, ok := r.BasicAuth(); !ok || user != "resource-server" || secret != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		record := as.tokens[r.Form.Get("token")]
		response := map[string]any{"active": record.active}
		if record.active {
			response["sub"] = record.sub
			response["aud"] = record.aud
			response["scope"] = record.scope
			response["exp"] = record.exp.Unix()
			response["iat"] = record.iat.Unix()
			issuer := record.issuer
			if issuer == "" {
				issuer = as.server.URL
			}
			response["iss"] = issuer
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(as.server.Close)
	return as
}

func (as *testAuthorizationServer) issue(token, subject, audience, scope string) {
	now := time.Now().Truncate(time.Second)
	as.mu.Lock()
	as.tokens[token] = tokenRecord{active: true, sub: subject, aud: audience, scope: scope, iat: now, exp: now.Add(5 * time.Minute)}
	as.mu.Unlock()
}

func (as *testAuthorizationServer) revoke(token string) {
	as.mu.Lock()
	record := as.tokens[token]
	record.active = false
	as.tokens[token] = record
	as.mu.Unlock()
}

func (as *testAuthorizationServer) callCount() int {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.calls
}

func newHTTPTestServer(t *testing.T, as *testAuthorizationServer, retriever Retriever) *HTTPServer {
	t.Helper()
	resource := "https://mcp.example.test/mcp"
	server, err := NewHTTPServer(&QueryTool{Service: retriever, Workspace: "synthetic"}, HTTPOptions{
		ResourceURI: resource, AuthorizationServer: as.server.URL,
		IntrospectionEndpoint: as.server.URL, IntrospectionClientID: "resource-server", IntrospectionSecret: "secret",
		introspectionHTTPClient: as.server.Client(), AllowedOrigins: []string{"https://app.example.test"},
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)
	return server
}

func mcpRequest(t *testing.T, server *HTTPServer, token, version, method, name string, params map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if version == "2026-07-28" {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    version,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "synthetic-client", "version": "1"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", bytes.NewReader(body))
	req.Host = "mcp.example.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if version == "2026-07-28" {
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestHTTPRejectsUnauthenticatedAndRevokedTokens(t *testing.T) {
	as := newTestAuthorizationServer(t)
	resource := "https://mcp.example.test/mcp"
	as.issue("valid", "caller-a", resource, defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	unauthenticated := mcpRequest(t, server, "", "2026-07-28", "server/discover", "", nil)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	if got := unauthenticated.Header().Get("WWW-Authenticate"); !strings.Contains(got, "resource_metadata=") || !strings.Contains(got, `scope="`+defaultHTTPScope+`"`) {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	authorized := mcpRequest(t, server, "valid", "2026-07-28", "server/discover", "", nil)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", authorized.Code, authorized.Body.String())
	}
	as.revoke("valid")
	revoked := mcpRequest(t, server, "valid", "2026-07-28", "server/discover", "", nil)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d, body=%s", revoked.Code, revoked.Body.String())
	}
}

func TestHTTPTokenAudienceScopeAndLifetime(t *testing.T) {
	as := newTestAuthorizationServer(t)
	resource := "https://mcp.example.test/mcp"
	as.issue("wrong-audience", "caller", "https://other.example.test/mcp", defaultHTTPScope)
	as.issue("wrong-scope", "caller", resource, "other:scope")
	as.issue("too-long", "caller", resource, defaultHTTPScope)
	as.issue("expired", "caller", resource, defaultHTTPScope)
	as.issue("wrong-issuer", "caller", resource, defaultHTTPScope)
	as.mu.Lock()
	long := as.tokens["too-long"]
	long.exp = long.iat.Add(time.Hour)
	as.tokens["too-long"] = long
	expired := as.tokens["expired"]
	expired.iat = expired.iat.Add(-10 * time.Minute)
	expired.exp = expired.iat.Add(5 * time.Minute)
	as.tokens["expired"] = expired
	wrongIssuer := as.tokens["wrong-issuer"]
	wrongIssuer.issuer = "https://other-issuer.example.test"
	as.tokens["wrong-issuer"] = wrongIssuer
	as.mu.Unlock()
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	for token, want := range map[string]int{
		"wrong-audience": http.StatusUnauthorized,
		"wrong-scope":    http.StatusForbidden,
		"too-long":       http.StatusUnauthorized,
		"expired":        http.StatusUnauthorized,
		"wrong-issuer":   http.StatusUnauthorized,
	} {
		recorder := mcpRequest(t, server, token, "2026-07-28", "server/discover", "", nil)
		if recorder.Code != want {
			t.Errorf("%s status = %d, want %d, body=%s", token, recorder.Code, want, recorder.Body.String())
		}
	}
}

func TestHTTPOriginHostAndProxyValidationPrecedesOAuth(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	baseBody := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`
	makeRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(baseBody))
		req.Host = "mcp.example.test"
		req.Header.Set("Authorization", "Bearer valid")
		return req
	}

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"malformed origin", func(r *http.Request) { r.Header.Set("Origin", "://bad") }, http.StatusForbidden},
		{"duplicate origin", func(r *http.Request) {
			r.Header.Add("Origin", "https://app.example.test")
			r.Header.Add("Origin", "https://app.example.test")
		}, http.StatusForbidden},
		{"deceptive origin", func(r *http.Request) { r.Header.Set("Origin", "https://app.example.test.evil.test") }, http.StatusForbidden},
		{"wrong host", func(r *http.Request) { r.Host = "mcp.example.test.evil.test" }, http.StatusForbidden},
		{"dns rebinding host ipv4", func(r *http.Request) { r.Host = "127.0.0.1:8080" }, http.StatusForbidden},
		{"dns rebinding host ipv6", func(r *http.Request) { r.Host = "[::1]:8080" }, http.StatusForbidden},
		{"untrusted forwarded", func(r *http.Request) {
			r.Header.Set("X-Forwarded-Host", "mcp.example.test")
			r.RemoteAddr = "192.0.2.10:1234"
		}, http.StatusForbidden},
		{"duplicate authorization", func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer second")
		}, http.StatusBadRequest},
		{"conflicting body framing", func(r *http.Request) {
			r.TransferEncoding = []string{"chunked"}
		}, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := as.callCount()
			req := makeRequest()
			test.mutate(req)
			recorder := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d, body=%s", recorder.Code, test.want, recorder.Body.String())
			}
			if as.callCount() != before {
				t.Fatal("invalid envelope reached OAuth introspection")
			}
		})
	}
}

func TestHTTPProtectedResourceMetadata(t *testing.T) {
	as := newTestAuthorizationServer(t)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})
	req := httptest.NewRequest(http.MethodGet, "http://backend/.well-known/oauth-protected-resource/mcp", nil)
	req.Host = "mcp.example.test"
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var metadata map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["resource"] != "https://mcp.example.test/mcp" {
		t.Fatalf("resource = %v", metadata["resource"])
	}
	servers, _ := metadata["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != as.server.URL {
		t.Fatalf("authorization_servers = %v", servers)
	}
}

func TestHTTPCurrentProtocolListsSharedTools(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	discover := mcpRequest(t, server, "valid", "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("discover status = %d, body=%s", discover.Code, discover.Body.String())
	}
	list := mcpRequest(t, server, "valid", "2026-07-28", "tools/list", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, body=%s", list.Code, list.Body.String())
	}
	for _, name := range []string{"search_synthetic", "read_synthetic_evidence"} {
		if !strings.Contains(list.Body.String(), name) {
			t.Errorf("tools/list omitted %s: %s", name, list.Body.String())
		}
	}
}

func TestHTTPLegacyOpenCodeProtocolNegotiates(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	initialize := mcpRequest(t, server, "valid", "2025-11-25", "initialize", "", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "opencode", "version": "1.18.15"},
	})
	if initialize.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body=%s", initialize.Code, initialize.Body.String())
	}
	if !strings.Contains(initialize.Body.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("initialize response = %s", initialize.Body.String())
	}
	if sessionID := initialize.Header().Get("Mcp-Session-Id"); sessionID != "" {
		t.Fatalf("stateless legacy response issued session %q", sessionID)
	}
	fixation := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	fixation.Host = "mcp.example.test"
	fixation.Header.Set("Authorization", "Bearer valid")
	fixation.Header.Set("Content-Type", "application/json")
	fixation.Header.Set("Accept", "application/json, text/event-stream")
	fixation.Header.Set("MCP-Protocol-Version", "2025-11-25")
	fixation.Header.Set("Mcp-Session-Id", "attacker-selected-session")
	fixationRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(fixationRecorder, fixation)
	if fixationRecorder.Code != http.StatusOK || !strings.Contains(fixationRecorder.Body.String(), "search_synthetic") {
		t.Fatalf("non-authoritative session header status = %d, body=%s", fixationRecorder.Code, fixationRecorder.Body.String())
	}
	notificationBody := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	notification := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(notificationBody))
	notification.Host = "mcp.example.test"
	notification.Header.Set("Authorization", "Bearer valid")
	notification.Header.Set("Content-Type", "application/json")
	notification.Header.Set("Accept", "application/json, text/event-stream")
	notification.Header.Set("MCP-Protocol-Version", "2025-11-25")
	notificationRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(notificationRecorder, notification)
	if notificationRecorder.Code != http.StatusAccepted || notificationRecorder.Body.Len() != 0 {
		t.Fatalf("initialized notification status = %d, body=%s", notificationRecorder.Code, notificationRecorder.Body.String())
	}

	list := mcpRequest(t, server, "valid", "2025-11-25", "tools/list", "", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "search_synthetic") {
		t.Fatalf("legacy tools/list status = %d, body=%s", list.Code, list.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "http://backend/mcp", nil)
	get.Host = "mcp.example.test"
	get.Header.Set("Authorization", "Bearer valid")
	getRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy standalone GET status = %d", getRecorder.Code)
	}
}

func TestHTTPCurrentProtocolRejectsHeaderBodyMismatch(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})
	recorder := mcpRequest(t, server, "valid", "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "synthetic"},
	})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":-32020`) {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPContinuationIsCallerBoundAndResponseBounded(t *testing.T) {
	as := newTestAuthorizationServer(t)
	resource := "https://mcp.example.test/mcp"
	as.issue("caller-a", "subject-a", resource, defaultHTTPScope)
	as.issue("caller-a-renewed", "subject-a", resource, defaultHTTPScope)
	as.issue("caller-b", "subject-b", resource, defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticTextResult(strings.Repeat("large synthetic evidence 🙂\n", 5500))})

	search := mcpRequest(t, server, "caller-a", "2026-07-28", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "large synthetic evidence"},
	})
	page, isError := decodeHTTPToolPage(t, search)
	if isError || page.Complete || page.NextCursor == nil {
		t.Fatalf("first page complete=%v cursor=%v isError=%v", page.Complete, page.NextCursor, isError)
	}
	if search.Body.Len() > absoluteToolResponseBytes {
		t.Fatalf("search response = %d bytes", search.Body.Len())
	}

	foreign := mcpRequest(t, server, "caller-b", "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
	})
	_, foreignError := decodeHTTPToolPage(t, foreign)
	if !foreignError {
		t.Fatal("another OAuth subject reused a continuation cursor")
	}

	pages := 1
	for !page.Complete {
		response := mcpRequest(t, server, "caller-a-renewed", "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
			"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
		})
		if response.Body.Len() > absoluteToolResponseBytes {
			t.Fatalf("page %d response = %d bytes", pages+1, response.Body.Len())
		}
		var callError bool
		page, callError = decodeHTTPToolPage(t, response)
		if callError {
			t.Fatalf("page %d returned tool error", pages+1)
		}
		pages++
		if pages > 20 {
			t.Fatal("continuation did not terminate")
		}
	}
	if pages < 3 {
		t.Fatalf("large evidence used only %d pages", pages)
	}
}

func TestHTTPCallerStateEvictionInvalidatesContinuation(t *testing.T) {
	as := newTestAuthorizationServer(t)
	resource := "https://mcp.example.test/mcp"
	as.issue("caller-a", "subject-a", resource, defaultHTTPScope)
	as.issue("caller-b", "subject-b", resource, defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticTextResult(strings.Repeat("bounded evidence\n", 5500))})
	server.opts.MaxCallerStates = 1

	search := mcpRequest(t, server, "caller-a", "2026-07-28", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "bounded evidence"},
	})
	page, isError := decodeHTTPToolPage(t, search)
	if isError || page.NextCursor == nil {
		t.Fatalf("search cursor=%v isError=%v", page.NextCursor, isError)
	}

	discover := mcpRequest(t, server, "caller-b", "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("second caller status = %d, body=%s", discover.Code, discover.Body.String())
	}
	server.statesMu.Lock()
	stateCount := len(server.states)
	server.statesMu.Unlock()
	if stateCount != 1 {
		t.Fatalf("caller state count = %d, want 1", stateCount)
	}

	evicted := mcpRequest(t, server, "caller-a", "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
	})
	_, evictedError := decodeHTTPToolPage(t, evicted)
	if !evictedError {
		t.Fatal("evicted caller reused a continuation cursor")
	}
}

func decodeHTTPToolPage(t *testing.T, recorder *httptest.ResponseRecorder) (*EvidencePage, bool) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("tool status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var page EvidencePage
	if len(envelope.Result.StructuredContent) > 0 {
		if err := json.Unmarshal(envelope.Result.StructuredContent, &page); err != nil {
			t.Fatal(err)
		}
	}
	return &page, envelope.Result.IsError
}

func TestHTTPRejectsOversizedBody(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_synthetic","arguments":{"question":%q}}}`, strings.Repeat("x", defaultHTTPMaxRequestBytes))
	req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(body))
	req.Host = "mcp.example.test"
	req.Header.Set("Authorization", "Bearer valid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(recorder.Result().Body)
		t.Fatalf("status = %d, body=%s", recorder.Code, body)
	}
}

func TestHTTPDisconnectCancelsCurrentProtocolToolCall(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	retriever := &blockingRetriever{started: make(chan struct{})}
	server := newHTTPTestServer(t, as, retriever)

	params := map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "cancel me"},
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "synthetic-client", "version": "1"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", bytes.NewReader(body)).WithContext(ctx)
	req.Host = "mcp.example.test"
	req.Header.Set("Authorization", "Bearer valid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "search_synthetic")

	done := make(chan struct{})
	go func() {
		server.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-retriever.started:
	case <-time.After(time.Second):
		t.Fatal("retrieval did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnected request did not release the handler")
	}
}

func TestHTTPStartupRefusesNonLoopbackOrIncompleteAuth(t *testing.T) {
	base := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	complete := HTTPOptions{
		ResourceURI: "https://mcp.example.test/mcp", AuthorizationServer: "https://auth.example.test/realms/pocket-advisor",
		IntrospectionEndpoint: "https://auth.example.test/realms/pocket-advisor/protocol/openid-connect/token/introspect",
		IntrospectionClientID: "resource-server", IntrospectionSecret: "secret",
	}
	badBind := complete
	badBind.Address = "0.0.0.0:8080"
	if _, err := NewHTTPServer(base, badBind); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback error = %v", err)
	}
	missingSecret := complete
	missingSecret.IntrospectionSecret = ""
	if _, err := NewHTTPServer(base, missingSecret); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing secret error = %v", err)
	}
	invalidProxy := complete
	invalidProxy.TrustedProxyCIDRs = []string{"not-a-cidr"}
	if _, err := NewHTTPServer(base, invalidProxy); err == nil || !strings.Contains(err.Error(), "trusted proxy CIDR") {
		t.Fatalf("invalid proxy error = %v", err)
	}
	invalidHost := complete
	invalidHost.AllowedHosts = []string{"mcp.example.test/path"}
	if _, err := NewHTTPServer(base, invalidHost); err == nil || !strings.Contains(err.Error(), "allowed host") {
		t.Fatalf("invalid host error = %v", err)
	}
	invalidScope := complete
	invalidScope.RequiredScope = "pocket-advisor:retrieve other:scope"
	if _, err := NewHTTPServer(base, invalidScope); err == nil || !strings.Contains(err.Error(), "one OAuth scope") {
		t.Fatalf("invalid scope error = %v", err)
	}
	invalidStateLimit := complete
	invalidStateLimit.MaxCallerStates = -1
	if _, err := NewHTTPServer(base, invalidStateLimit); err == nil || !strings.Contains(err.Error(), "max caller states") {
		t.Fatalf("invalid caller state limit error = %v", err)
	}
}

func TestHTTPIntrospectionRedirectIsNotFollowed(t *testing.T) {
	var targetCalls int
	var authorizationServer *httptest.Server
	authorizationServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"active":false}`)
			return
		}
		http.Redirect(w, r, authorizationServer.URL+"/target", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(authorizationServer.Close)
	client := authorizationServer.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return nil }
	server, err := NewHTTPServer(&QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}, HTTPOptions{
		ResourceURI: "https://mcp.example.test/mcp", AuthorizationServer: authorizationServer.URL,
		IntrospectionEndpoint: authorizationServer.URL, IntrospectionClientID: "resource-server", IntrospectionSecret: "secret",
		introspectionHTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)

	response := mcpRequest(t, server, "redirected", "2026-07-28", "server/discover", "", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if targetCalls != 0 {
		t.Fatalf("introspection redirect followed %d times", targetCalls)
	}
}

func TestHTTPConcurrencyAndTimeoutBoundIntrospection(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	authorizationServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"active":false}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(authorizationServer.Close)
	server, err := NewHTTPServer(&QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}, HTTPOptions{
		ResourceURI: "https://mcp.example.test/mcp", AuthorizationServer: authorizationServer.URL,
		IntrospectionEndpoint: authorizationServer.URL, IntrospectionClientID: "resource-server", IntrospectionSecret: "secret",
		introspectionHTTPClient: authorizationServer.Client(), MaxConcurrent: 1, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- mcpRequest(t, server, "first", "2026-07-28", "server/discover", "", nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first introspection did not start")
	}
	second := mcpRequest(t, server, "second", "2026-07-28", "server/discover", "", nil)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent authorization status = %d, body=%s", second.Code, second.Body.String())
	}
	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusUnauthorized {
			t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first authorization did not finish")
	}

	timeoutStarted := make(chan struct{}, 1)
	timeoutRelease := make(chan struct{})
	timeoutAuthorizationServer := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		timeoutStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-timeoutRelease:
		}
	}))
	t.Cleanup(timeoutAuthorizationServer.Close)
	timeoutServer, err := NewHTTPServer(&QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}, HTTPOptions{
		ResourceURI: "https://mcp.example.test/mcp", AuthorizationServer: timeoutAuthorizationServer.URL,
		IntrospectionEndpoint: timeoutAuthorizationServer.URL, IntrospectionClientID: "resource-server", IntrospectionSecret: "secret",
		introspectionHTTPClient: timeoutAuthorizationServer.Client(), RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(timeoutServer.closeStates)
	begin := time.Now()
	timedOut := mcpRequest(t, timeoutServer, "slow", "2026-07-28", "server/discover", "", nil)
	close(timeoutRelease)
	if timedOut.Code != http.StatusUnauthorized {
		t.Fatalf("timed-out authorization status = %d, body=%s", timedOut.Code, timedOut.Body.String())
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("authorization timeout took %s", elapsed)
	}
	select {
	case <-timeoutStarted:
	default:
		t.Fatal("timed authorization never reached introspection")
	}
}

func TestHTTPReadinessDoesNotExposeDependency(t *testing.T) {
	as := newTestAuthorizationServer(t)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})
	server.opts.Readiness = func(context.Context) error { return fmt.Errorf("private database endpoint and workspace") }
	req := httptest.NewRequest(http.MethodGet, "http://backend/readyz", nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "database") || strings.Contains(recorder.Body.String(), "workspace") {
		t.Fatalf("readiness leaked dependency: %s", recorder.Body.String())
	}
}
