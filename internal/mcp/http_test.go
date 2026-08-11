package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testGoogleClientID is the fixed audience every test server configures as
// its GoogleClientID; issued tokens use it as their "aud" unless a test
// deliberately mismatches it.
const testGoogleClientID = "test-mcp-client.apps.googleusercontent.com"

// testAllowedEmails is the fixed allowlist every test server configures.
// Tests exercising "not authorized" pass an email outside this list.
var testAllowedEmails = []string{
	"caller-a@example.test", "caller-b@example.test", "caller@example.test",
	"valid@example.test", "first@example.test", "second@example.test",
}

// tokenClaims controls what issue mints; issue's defaults are what a real
// Google ID token for an allowlisted caller looks like, and tests override
// only the field(s) under test.
type tokenClaims struct {
	subject       string
	email         string
	emailVerified bool
	audience      string
	issuer        string
	issuedAt      time.Time
	expiry        time.Time
}

// testGoogleServer is a fake Google OIDC provider: it serves OIDC discovery
// and a JWKS over HTTPS, and mints ID tokens signed with the same key it
// publishes, so the real production verifier (newGoogleVerifier, built on
// go-oidc) can be exercised end-to-end without contacting the real Google.
type testGoogleServer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	keyID  string

	mu        sync.Mutex
	jwksCalls int
}

func newTestGoogleServer(t *testing.T) *testGoogleServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &testGoogleServer{key: key, keyID: "test-key"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 g.server.URL,
			"jwks_uri":               g.server.URL + "/keys",
			"authorization_endpoint": g.server.URL + "/auth",
			"token_endpoint":         g.server.URL + "/token",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.jwksCalls++
		g.mu.Unlock()
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: g.keyID, Algorithm: "RS256", Use: "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *testGoogleServer) jwksFetchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.jwksCalls
}

// issue mints a signed ID token for a caller that should be accepted: an
// allowlisted email, verified, the configured audience and issuer, and a
// five-minute lifetime.
func (g *testGoogleServer) issue(subject, email string) string {
	return g.issueCustom(tokenClaims{
		subject: subject, email: email, emailVerified: true,
		audience: testGoogleClientID, issuer: g.server.URL,
		issuedAt: time.Now(), expiry: time.Now().Add(5 * time.Minute),
	})
}

func (g *testGoogleServer) issueCustom(c tokenClaims) string {
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       &jose.JSONWebKey{Key: g.key, KeyID: g.keyID, Algorithm: "RS256", Use: "sig"},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	claims := map[string]any{
		"iss": c.issuer, "sub": c.subject, "aud": c.audience,
		"email": c.email, "email_verified": c.emailVerified,
		"iat": c.issuedAt.Unix(), "exp": c.expiry.Unix(),
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		panic(err)
	}
	return token
}

func newHTTPTestServer(t *testing.T, google *testGoogleServer, retriever Retriever) *HTTPServer {
	t.Helper()
	resource := "https://mcp.example.test/mcp"
	server, err := NewHTTPServer(context.Background(), &QueryTool{Service: retriever, Workspace: "synthetic"}, testGoogleHTTPOptions(google, HTTPOptions{
		ResourceURI: resource, AllowedOrigins: []string{"https://app.example.test"},
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)
	return server
}

func newUnauthenticatedHTTPTestServer(t *testing.T, retriever Retriever) *HTTPServer {
	t.Helper()
	server, err := NewHTTPServer(context.Background(), &QueryTool{Service: retriever, Workspace: "synthetic"}, HTTPOptions{
		AllowedHosts: []string{"mcp.example.test"}, AllowedOrigins: []string{"https://app.example.test"},
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)
	return server
}

// testGoogleHTTPOptions layers the fake-provider test hooks (issuer override,
// http client, client ID, allowlist) onto whatever the caller already set.
// http_test.go is part of package mcp, so it can reach the unexported
// googleIssuer/googleHTTPClient test-injection fields directly.
func testGoogleHTTPOptions(google *testGoogleServer, base HTTPOptions) HTTPOptions {
	opts := base
	opts.GoogleClientID = testGoogleClientID
	opts.AllowedEmails = testAllowedEmails
	opts.googleIssuer = google.server.URL
	opts.googleHTTPClient = google.server.Client()
	return opts
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

func TestHTTPRejectsUnauthenticatedTokens(t *testing.T) {
	google := newTestGoogleServer(t)
	valid := google.issue("subject-a", "caller-a@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})

	unauthenticated := mcpRequest(t, server, "", "2026-07-28", "server/discover", "", nil)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	if got := unauthenticated.Header().Get("WWW-Authenticate"); !strings.Contains(got, "resource_metadata=") {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	authorized := mcpRequest(t, server, valid, "2026-07-28", "server/discover", "", nil)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", authorized.Code, authorized.Body.String())
	}

	malformed := mcpRequest(t, server, "not-a-jwt", "2026-07-28", "server/discover", "", nil)
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("malformed token status = %d, body=%s", malformed.Code, malformed.Body.String())
	}
}

func TestHTTPUnauthenticatedCurrentProtocolDiscoveryAndToolCall(t *testing.T) {
	server := newUnauthenticatedHTTPTestServer(t, &stubRetriever{result: syntheticResult()})

	discover := mcpRequest(t, server, "", "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("unauthenticated discover status = %d, body=%s", discover.Code, discover.Body.String())
	}
	call := mcpRequest(t, server, "not-a-token", "2026-07-28", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "synthetic"},
	})
	page, isError := decodeHTTPToolPage(t, call)
	if isError || len(page.Packets) == 0 {
		t.Fatalf("unauthenticated tool result isError=%v packets=%d", isError, len(page.Packets))
	}

	server.statesMu.Lock()
	_, hasAnonymousState := server.states[anonymousCallerID]
	stateCount := len(server.states)
	server.statesMu.Unlock()
	if !hasAnonymousState || stateCount != 1 {
		t.Fatalf("anonymous caller states = %d, has anonymous = %v", stateCount, hasAnonymousState)
	}
}

func TestHTTPUnauthenticatedModeRetainsBoundaryChecks(t *testing.T) {
	base := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	if _, err := NewHTTPServer(context.Background(), base, HTTPOptions{Address: "0.0.0.0:8080"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unauthenticated non-loopback error = %v", err)
	}

	server := newUnauthenticatedHTTPTestServer(t, base.Service)
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
		req.Host = "mcp.example.test"
		return req
	}
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"wrong host", func(r *http.Request) { r.Host = "mcp.example.test.evil.test" }},
		{"wrong origin", func(r *http.Request) { r.Header.Set("Origin", "https://app.example.test.evil.test") }},
		{"untrusted forwarded header", func(r *http.Request) {
			r.Header.Set("X-Forwarded-Host", "mcp.example.test")
			r.RemoteAddr = "192.0.2.10:1234"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newRequest()
			test.mutate(req)
			recorder := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
		})
	}
}

func TestHTTPTokenClaimsAreValidated(t *testing.T) {
	google := newTestGoogleServer(t)
	now := time.Now()

	tokens := map[string]string{
		"wrong-audience": google.issueCustom(tokenClaims{
			subject: "caller", email: "caller@example.test", emailVerified: true,
			audience: "someone-else.apps.googleusercontent.com", issuer: google.server.URL,
			issuedAt: now, expiry: now.Add(5 * time.Minute),
		}),
		"expired": google.issueCustom(tokenClaims{
			subject: "caller", email: "caller@example.test", emailVerified: true,
			audience: testGoogleClientID, issuer: google.server.URL,
			issuedAt: now.Add(-10 * time.Minute), expiry: now.Add(-5 * time.Minute),
		}),
		"wrong-issuer": google.issueCustom(tokenClaims{
			subject: "caller", email: "caller@example.test", emailVerified: true,
			audience: testGoogleClientID, issuer: "https://not-google.example.test",
			issuedAt: now, expiry: now.Add(5 * time.Minute),
		}),
		"unverified-email": google.issueCustom(tokenClaims{
			subject: "caller", email: "caller@example.test", emailVerified: false,
			audience: testGoogleClientID, issuer: google.server.URL,
			issuedAt: now, expiry: now.Add(5 * time.Minute),
		}),
		"unauthorized-email": google.issueCustom(tokenClaims{
			subject: "caller", email: "attacker@example.test", emailVerified: true,
			audience: testGoogleClientID, issuer: google.server.URL,
			issuedAt: now, expiry: now.Add(5 * time.Minute),
		}),
	}
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})

	for name, token := range tokens {
		recorder := mcpRequest(t, server, token, "2026-07-28", "server/discover", "", nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want %d, body=%s", name, recorder.Code, http.StatusUnauthorized, recorder.Body.String())
		}
	}
}

func TestHTTPOriginHostAndProxyValidationPrecedesAuth(t *testing.T) {
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})

	baseBody := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`
	makeRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(baseBody))
		req.Host = "mcp.example.test"
		req.Header.Set("Authorization", "Bearer "+valid)
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
			req := makeRequest()
			test.mutate(req)
			recorder := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d, body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestHTTPProtectedResourceMetadata(t *testing.T) {
	google := newTestGoogleServer(t)
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})
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
	if len(servers) != 1 || servers[0] != google.server.URL {
		t.Fatalf("authorization_servers = %v", servers)
	}
}

func TestHTTPCurrentProtocolListsSharedTools(t *testing.T) {
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})

	discover := mcpRequest(t, server, valid, "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("discover status = %d, body=%s", discover.Code, discover.Body.String())
	}
	list := mcpRequest(t, server, valid, "2026-07-28", "tools/list", "", nil)
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
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})

	initialize := mcpRequest(t, server, valid, "2025-11-25", "initialize", "", map[string]any{
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
	fixation.Header.Set("Authorization", "Bearer "+valid)
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
	notification.Header.Set("Authorization", "Bearer "+valid)
	notification.Header.Set("Content-Type", "application/json")
	notification.Header.Set("Accept", "application/json, text/event-stream")
	notification.Header.Set("MCP-Protocol-Version", "2025-11-25")
	notificationRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(notificationRecorder, notification)
	if notificationRecorder.Code != http.StatusAccepted || notificationRecorder.Body.Len() != 0 {
		t.Fatalf("initialized notification status = %d, body=%s", notificationRecorder.Code, notificationRecorder.Body.String())
	}

	list := mcpRequest(t, server, valid, "2025-11-25", "tools/list", "", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "search_synthetic") {
		t.Fatalf("legacy tools/list status = %d, body=%s", list.Code, list.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "http://backend/mcp", nil)
	get.Host = "mcp.example.test"
	get.Header.Set("Authorization", "Bearer "+valid)
	getRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy standalone GET status = %d", getRecorder.Code)
	}
}

func TestHTTPCurrentProtocolRejectsHeaderBodyMismatch(t *testing.T) {
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})
	recorder := mcpRequest(t, server, valid, "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "synthetic"},
	})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":-32020`) {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPContinuationIsCallerBoundAndResponseBounded(t *testing.T) {
	google := newTestGoogleServer(t)
	callerA := google.issue("subject-a", "caller-a@example.test")
	callerARenewed := google.issue("subject-a", "caller-a@example.test")
	callerB := google.issue("subject-b", "caller-b@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticTextResult(strings.Repeat("large synthetic evidence 🙂\n", 5500))})

	search := mcpRequest(t, server, callerA, "2026-07-28", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "large synthetic evidence"},
	})
	page, isError := decodeHTTPToolPage(t, search)
	if isError || page.Complete || page.NextCursor == nil {
		t.Fatalf("first page complete=%v cursor=%v isError=%v", page.Complete, page.NextCursor, isError)
	}
	if search.Body.Len() > absoluteToolResponseBytes {
		t.Fatalf("search response = %d bytes", search.Body.Len())
	}

	foreign := mcpRequest(t, server, callerB, "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
	})
	_, foreignError := decodeHTTPToolPage(t, foreign)
	if !foreignError {
		t.Fatal("another caller reused a continuation cursor")
	}

	pages := 1
	for !page.Complete {
		response := mcpRequest(t, server, callerARenewed, "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
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
	google := newTestGoogleServer(t)
	callerA := google.issue("subject-a", "caller-a@example.test")
	callerB := google.issue("subject-b", "caller-b@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticTextResult(strings.Repeat("bounded evidence\n", 5500))})
	server.opts.MaxCallerStates = 1

	search := mcpRequest(t, server, callerA, "2026-07-28", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "bounded evidence"},
	})
	page, isError := decodeHTTPToolPage(t, search)
	if isError || page.NextCursor == nil {
		t.Fatalf("search cursor=%v isError=%v", page.NextCursor, isError)
	}

	discover := mcpRequest(t, server, callerB, "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("second caller status = %d, body=%s", discover.Code, discover.Body.String())
	}
	server.statesMu.Lock()
	stateCount := len(server.states)
	server.statesMu.Unlock()
	if stateCount != 1 {
		t.Fatalf("caller state count = %d, want 1", stateCount)
	}

	evicted := mcpRequest(t, server, callerA, "2026-07-28", "tools/call", "read_synthetic_evidence", map[string]any{
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
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_synthetic","arguments":{"question":%q}}}`, strings.Repeat("x", defaultHTTPMaxRequestBytes))
	req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", strings.NewReader(body))
	req.Host = "mcp.example.test"
	req.Header.Set("Authorization", "Bearer "+valid)
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
	google := newTestGoogleServer(t)
	valid := google.issue("caller", "caller@example.test")
	retriever := &blockingRetriever{started: make(chan struct{})}
	server := newHTTPTestServer(t, google, retriever)

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
	req.Header.Set("Authorization", "Bearer "+valid)
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

// TestHTTPConcurrencyBoundsToolCalls exercises limitConcurrency (MaxConcurrent)
// directly at the tool-call layer: a slow in-flight call occupies the sole
// concurrency slot, so a second concurrent call is rejected with 503 rather
// than queued. Unlike the introspection-based design this replaces, verifying
// a Google ID token's signature against an already-cached JWKS is a local,
// effectively instant operation with no comparable "slow verification"
// scenario to simulate, so this test covers the same middleware from the
// tool-execution side instead.
func TestHTTPConcurrencyBoundsToolCalls(t *testing.T) {
	google := newTestGoogleServer(t)
	first := google.issue("subject-a", "first@example.test")
	second := google.issue("subject-b", "second@example.test")
	retriever := &blockingRetriever{started: make(chan struct{})}
	server, err := NewHTTPServer(context.Background(), &QueryTool{Service: retriever, Workspace: "synthetic"}, testGoogleHTTPOptions(google, HTTPOptions{
		ResourceURI: "https://mcp.example.test/mcp", MaxConcurrent: 1, RequestTimeout: time.Second,
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeStates)

	params := func(question string) map[string]any {
		return map[string]any{
			"name": "search_synthetic", "arguments": map[string]any{"question": question},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "synthetic-client", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		}
	}
	newReq := func(ctx context.Context, token, question string) *http.Request {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params(question)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "http://backend/mcp", bytes.NewReader(body)).WithContext(ctx)
		req.Host = "mcp.example.test"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "search_synthetic")
		return req
	}

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		server.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), newReq(ctx, first, "first"))
		close(firstDone)
	}()
	select {
	case <-retriever.started:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}

	secondRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(secondRecorder, newReq(context.Background(), second, "second"))
	if secondRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent call status = %d, body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first call did not release the handler")
	}
}

func TestHTTPStartupRefusesInvalidOptions(t *testing.T) {
	ctx := context.Background()
	base := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	google := newTestGoogleServer(t)
	complete := testGoogleHTTPOptions(google, HTTPOptions{ResourceURI: "https://mcp.example.test/mcp"})

	badBind := complete
	badBind.Address = "0.0.0.0:8080"
	if _, err := NewHTTPServer(ctx, base, badBind); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback error = %v", err)
	}
	missingEmails := complete
	missingEmails.AllowedEmails = nil
	if _, err := NewHTTPServer(ctx, base, missingEmails); err == nil || !strings.Contains(err.Error(), "allowed email") {
		t.Fatalf("missing allowed emails error = %v", err)
	}
	insecureResource := complete
	insecureResource.ResourceURI = "http://mcp.example.test/mcp"
	if _, err := NewHTTPServer(ctx, base, insecureResource); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure resource URI error = %v", err)
	}
	invalidProxy := complete
	invalidProxy.TrustedProxyCIDRs = []string{"not-a-cidr"}
	if _, err := NewHTTPServer(ctx, base, invalidProxy); err == nil || !strings.Contains(err.Error(), "trusted proxy CIDR") {
		t.Fatalf("invalid proxy error = %v", err)
	}
	invalidHost := complete
	invalidHost.AllowedHosts = []string{"mcp.example.test/path"}
	if _, err := NewHTTPServer(ctx, base, invalidHost); err == nil || !strings.Contains(err.Error(), "allowed host") {
		t.Fatalf("invalid host error = %v", err)
	}
	invalidStateLimit := complete
	invalidStateLimit.MaxCallerStates = -1
	if _, err := NewHTTPServer(ctx, base, invalidStateLimit); err == nil || !strings.Contains(err.Error(), "max caller states") {
		t.Fatalf("invalid caller state limit error = %v", err)
	}
	certWithoutKey := complete
	certWithoutKey.CertFile = "/tmp/does-not-matter.pem"
	if _, err := NewHTTPServer(ctx, base, certWithoutKey); err == nil || !strings.Contains(err.Error(), "certificate file and a key file") {
		t.Fatalf("cert without key error = %v", err)
	}
}

func TestHTTPReadinessDoesNotExposeDependency(t *testing.T) {
	google := newTestGoogleServer(t)
	server := newHTTPTestServer(t, google, &stubRetriever{result: syntheticResult()})
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
