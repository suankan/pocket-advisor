package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testHost = "127.0.0.1:8080"

func newHTTPTestServer(t *testing.T, retriever Retriever) *HTTPServer {
	t.Helper()
	server, err := NewHTTPServer(&QueryTool{Service: retriever, Workspace: "synthetic"}, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.tool.closeSnapshots)
	return server
}

func currentProtocolMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "synthetic-client", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

func newMCPRequest(t *testing.T, ctx context.Context, version, method, name string, params map[string]any) *http.Request {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if version == "2026-07-28" {
		params["_meta"] = currentProtocolMeta()
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/mcp", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if version == "2026-07-28" {
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	return req
}

func mcpRequest(t *testing.T, server *HTTPServer, version, method, name string, params map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, newMCPRequest(t, context.Background(), version, method, name, params))
	return recorder
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

func TestHTTPCurrentProtocolDiscoveryListsAndCallsTools(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})

	discover := mcpRequest(t, server, "2026-07-28", "server/discover", "", nil)
	if discover.Code != http.StatusOK {
		t.Fatalf("discover status = %d, body=%s", discover.Code, discover.Body.String())
	}
	list := mcpRequest(t, server, "2026-07-28", "tools/list", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, body=%s", list.Code, list.Body.String())
	}
	for _, name := range []string{"search", "read_evidence"} {
		if !strings.Contains(list.Body.String(), name) {
			t.Errorf("tools/list omitted %s: %s", name, list.Body.String())
		}
	}
	call := mcpRequest(t, server, "2026-07-28", "tools/call", "search", map[string]any{
		"name": "search", "arguments": map[string]any{"question": "synthetic"},
	})
	page, isError := decodeHTTPToolPage(t, call)
	if isError || len(page.Packets) == 0 {
		t.Fatalf("tool result isError=%v packets=%d", isError, len(page.Packets))
	}
}

func TestHTTPLegacyOpenCodeProtocolNegotiates(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})

	initialize := mcpRequest(t, server, "2025-11-25", "initialize", "", map[string]any{
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
	fixation := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	fixation.Header.Set("Content-Type", "application/json")
	fixation.Header.Set("Accept", "application/json, text/event-stream")
	fixation.Header.Set("MCP-Protocol-Version", "2025-11-25")
	fixation.Header.Set("Mcp-Session-Id", "client-selected-session")
	fixationRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(fixationRecorder, fixation)
	if fixationRecorder.Code != http.StatusOK || !strings.Contains(fixationRecorder.Body.String(), "search") {
		t.Fatalf("non-authoritative session header status = %d, body=%s", fixationRecorder.Code, fixationRecorder.Body.String())
	}
	notification := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	notification.Header.Set("Content-Type", "application/json")
	notification.Header.Set("Accept", "application/json, text/event-stream")
	notification.Header.Set("MCP-Protocol-Version", "2025-11-25")
	notificationRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(notificationRecorder, notification)
	if notificationRecorder.Code != http.StatusAccepted || notificationRecorder.Body.Len() != 0 {
		t.Fatalf("initialized notification status = %d, body=%s", notificationRecorder.Code, notificationRecorder.Body.String())
	}

	list := mcpRequest(t, server, "2025-11-25", "tools/list", "", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "search") {
		t.Fatalf("legacy tools/list status = %d, body=%s", list.Code, list.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/mcp", nil)
	getRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy standalone GET status = %d", getRecorder.Code)
	}
}

func TestHTTPCurrentProtocolRejectsHeaderBodyMismatch(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})
	recorder := mcpRequest(t, server, "2026-07-28", "tools/call", "read_evidence", map[string]any{
		"name": "search", "arguments": map[string]any{"question": "synthetic"},
	})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":-32020`) {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPContinuationIsResponseBounded(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticTextResult(strings.Repeat("large synthetic evidence 🙂\n", 5500))})

	search := mcpRequest(t, server, "2026-07-28", "tools/call", "search", map[string]any{
		"name": "search", "arguments": map[string]any{"question": "large synthetic evidence"},
	})
	page, isError := decodeHTTPToolPage(t, search)
	if isError || page.Complete || page.NextCursor == nil {
		t.Fatalf("first page complete=%v cursor=%v isError=%v", page.Complete, page.NextCursor, isError)
	}
	if search.Body.Len() > absoluteToolResponseBytes {
		t.Fatalf("search response = %d bytes", search.Body.Len())
	}

	pages := 1
	for !page.Complete {
		response := mcpRequest(t, server, "2026-07-28", "tools/call", "read_evidence", map[string]any{
			"name": "read_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
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

// The SDK's DNS-rebinding guard stays on: a request that reached a loopback
// listener under a non-loopback Host name is a browser being steered at this
// machine by someone else's DNS record.
func TestHTTPRejectsDNSRebinding(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}
	for _, test := range []struct {
		host string
		want int
	}{
		{testHost, http.StatusOK},
		{"localhost:8080", http.StatusOK},
		{"rebound.example.test", http.StatusForbidden},
	} {
		ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, net.Addr(local))
		req := newMCPRequest(t, ctx, "2026-07-28", "server/discover", "", nil)
		req.Host = test.host
		recorder := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(recorder, req)
		if recorder.Code != test.want {
			t.Errorf("Host %q status = %d, want %d, body=%s", test.host, recorder.Code, test.want, recorder.Body.String())
		}
	}
}

func TestHTTPRejectsOversizedBody(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"question":%q}}}`, strings.Repeat("x", defaultHTTPMaxRequestBytes))
	req := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/mcp", strings.NewReader(body))
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
	retriever := &blockingRetriever{started: make(chan struct{})}
	server := newHTTPTestServer(t, retriever)

	ctx, cancel := context.WithCancel(context.Background())
	req := newMCPRequest(t, ctx, "2026-07-28", "tools/call", "search", map[string]any{
		"name": "search", "arguments": map[string]any{"question": "cancel me"},
	})
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

// A slow in-flight call occupies the sole concurrency slot, so a second
// concurrent call is rejected with 503 rather than queued.
func TestHTTPConcurrencyBoundsToolCalls(t *testing.T) {
	retriever := &blockingRetriever{started: make(chan struct{})}
	server, err := NewHTTPServer(&QueryTool{Service: retriever, Workspace: "synthetic"}, HTTPOptions{
		MaxConcurrent: 1, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.tool.closeSnapshots)
	call := func(ctx context.Context, question string) *http.Request {
		return newMCPRequest(t, ctx, "2026-07-28", "tools/call", "search", map[string]any{
			"name": "search", "arguments": map[string]any{"question": question},
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		server.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), call(ctx, "first"))
		close(firstDone)
	}()
	select {
	case <-retriever.started:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}

	secondRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(secondRecorder, call(context.Background(), "second"))
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
	base := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	for _, test := range []struct {
		name string
		opts HTTPOptions
		want string
	}{
		{"all interfaces", HTTPOptions{Address: "0.0.0.0:8080"}, "loopback"},
		{"host name", HTTPOptions{Address: "localhost:8080"}, "loopback"},
		{"relative endpoint", HTTPOptions{Endpoint: "mcp"}, "absolute path"},
		{"concurrency", HTTPOptions{MaxConcurrent: -1}, "max concurrent"},
		{"timeout", HTTPOptions{RequestTimeout: time.Millisecond}, "request timeout"},
	} {
		if _, err := NewHTTPServer(base, test.opts); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", test.name, err, test.want)
		}
	}
	if _, err := NewHTTPServer(&QueryTool{Service: base.Service}, HTTPOptions{}); err == nil {
		t.Error("a tool without a fixed workspace was accepted")
	}
}

func TestHTTPReadinessDoesNotExposeDependency(t *testing.T) {
	server := newHTTPTestServer(t, &stubRetriever{result: syntheticResult()})
	server.opts.Readiness = func(context.Context) error { return fmt.Errorf("private database endpoint and workspace") }
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/readyz", nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "database") || strings.Contains(recorder.Body.String(), "workspace") {
		t.Fatalf("readiness leaked dependency: %s", recorder.Body.String())
	}
}
