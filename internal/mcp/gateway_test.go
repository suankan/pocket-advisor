package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// newGatewayProxy stands up an HTTPS reverse proxy that mirrors the deployed
// Caddy sidecar: it terminates TLS, keeps the client Host (Caddy's
// `header_up Host {host}`), stamps X-Forwarded-Proto https, and bounds the
// request body to 1 MB. This exercises the real HTTPServer behind the same
// trusted-proxy contract the chart applies, without a cluster or Keycloak.
func newGatewayProxy(t *testing.T, backendURL string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		Transport: http.DefaultTransport,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Caddy `header_up Host {host}`: preserve the client Host so the
			// backend's allowed-host check sees the public name, not the pod IP.
			req.Header.Set("X-Forwarded-Proto", "https")
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		proxy.ServeHTTP(w, r)
	})
	server := httptest.NewUnstartedServer(handler)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func gatewayPost(t *testing.T, proxy *httptest.Server, token, version, method, name string, params map[string]any) *http.Response {
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
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "mcp.example.test"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", version)
	if version == "2026-07-28" {
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeGatewayPage(t *testing.T, resp *http.Response) (*EvidencePage, bool) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tool status = %d, body=%s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
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

// TestHTTPGatewayMirrorsCaddyForLegacyClient drives a 2025-11-25 client
// (OpenCode 1.18.15 compatibility) through the TLS-terminated proxy and
// asserts statelessness, host/origin enforcement, multi-page continuation
// within the response bound, and caller isolation.
func TestHTTPGatewayMirrorsCaddyForLegacyClient(t *testing.T) {
	as := newTestAuthorizationServer(t)
	resource := "https://mcp.example.test/mcp"
	as.issue("caller-a", "subject-a", resource, defaultHTTPScope)
	as.issue("caller-b", "subject-b", resource, defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticTextResult(strings.Repeat("large synthetic evidence 🙂\n", 5500))})

	backend := httptest.NewServer(server.httpServer.Handler)
	defer backend.Close()
	proxy := newGatewayProxy(t, backend.URL)
	defer proxy.Close()

	initialize := gatewayPost(t, proxy, "caller-a", "2025-11-25", "initialize", "", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "opencode", "version": "1.18.15"},
	})
	if initialize.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(initialize.Body)
		t.Fatalf("initialize status = %d body=%s", initialize.StatusCode, body)
	}
	if initialize.Header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("stateless legacy gateway issued session %q", initialize.Header.Get("Mcp-Session-Id"))
	}
	initialize.Body.Close()

	list := gatewayPost(t, proxy, "caller-a", "2025-11-25", "tools/list", "", nil)
	if list.StatusCode != http.StatusOK || !contains(list, "search_synthetic") {
		body, _ := io.ReadAll(list.Body)
		t.Fatalf("legacy tools/list status = %d, body=%s", list.StatusCode, body)
	}
	list.Body.Close()

	search := gatewayPost(t, proxy, "caller-a", "2025-11-25", "tools/call", "search_synthetic", map[string]any{
		"name": "search_synthetic", "arguments": map[string]any{"question": "large synthetic evidence"},
	})
	page, isError := decodeGatewayPage(t, search)
	if isError || page.Complete || page.NextCursor == nil {
		t.Fatalf("first page complete=%v cursor=%v isError=%v", page.Complete, page.NextCursor, isError)
	}

	foreign := gatewayPost(t, proxy, "caller-b", "2025-11-25", "tools/call", "read_synthetic_evidence", map[string]any{
		"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
	})
	_, foreignError := decodeGatewayPage(t, foreign)
	if !foreignError {
		t.Fatal("another OAuth subject reused a continuation cursor through the gateway")
	}

	pages := 1
	for !page.Complete {
		response := gatewayPost(t, proxy, "caller-a", "2025-11-25", "tools/call", "read_synthetic_evidence", map[string]any{
			"name": "read_synthetic_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
		})
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > absoluteToolResponseBytes {
			t.Fatalf("gateway page %d response = %d bytes", pages+1, len(raw))
		}
		var envelope struct {
			Result struct {
				StructuredContent json.RawMessage `json:"structuredContent"`
				IsError           bool            `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.IsError {
			t.Fatalf("gateway page %d returned tool error", pages+1)
		}
		if err := json.Unmarshal(envelope.Result.StructuredContent, &page); err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > 20 {
			t.Fatal("gateway continuation did not terminate")
		}
	}
	if pages < 3 {
		t.Fatalf("gateway large evidence used only %d pages", pages)
	}
}

// TestHTTPGatewayRejectsInvalidHostBeforeAuth confirms a Host that is not the
// public name is refused by the backend even though the trusted proxy adds
// forwarding metadata.
func TestHTTPGatewayRejectsInvalidHostBeforeAuth(t *testing.T) {
	as := newTestAuthorizationServer(t)
	as.issue("valid", "caller", "https://mcp.example.test/mcp", defaultHTTPScope)
	server := newHTTPTestServer(t, as, &stubRetriever{result: syntheticResult()})

	backend := httptest.NewServer(server.httpServer.Handler)
	defer backend.Close()
	proxy := newGatewayProxy(t, backend.URL)
	defer proxy.Close()

	before := as.callCount()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "mcp.example.test.evil.test"
	req.Header.Set("Authorization", "Bearer valid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid host status = %d", resp.StatusCode)
	}
	if as.callCount() != before {
		t.Fatal("gateway forwarded invalid host to OAuth introspection")
	}
}

func contains(resp *http.Response, substr string) bool {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), substr)
}
