package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

func initializeMessage(id int, version string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"synthetic-client","version":"1.0"}}}`, id, version)
}

func readyMessages(version string, rest ...string) string {
	messages := []string{
		initializeMessage(1, version),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}
	messages = append(messages, rest...)
	return strings.Join(messages, "\n") + "\n"
}

func serve(t *testing.T, input string) []map[string]any {
	t.Helper()
	var output bytes.Buffer
	server := NewServer(
		&QueryTool{Workspace: "synthetic"}, strings.NewReader(input), &output,
		slog.New(slog.DiscardHandler),
	)
	if err := server.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return decodeMessages(t, output.String())
}

func decodeMessages(t *testing.T, output string) []map[string]any {
	t.Helper()
	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("reply is not JSON: %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	return messages
}

func TestEverySupportedProtocolVersionNegotiates(t *testing.T) {
	for _, version := range supportedProtocolVersions {
		t.Run(version, func(t *testing.T) {
			messages := serve(t, initializeMessage(1, version)+"\n")
			if len(messages) != 1 {
				t.Fatalf("got %d replies, want 1", len(messages))
			}
			result := messages[0]["result"].(map[string]any)
			if got := result["protocolVersion"]; got != version {
				t.Errorf("protocolVersion = %v, want %s", got, version)
			}
			capabilities := result["capabilities"].(map[string]any)
			tools := capabilities["tools"].(map[string]any)
			if got := tools["listChanged"]; got != false {
				t.Errorf("tools.listChanged = %v, want false", got)
			}
		})
	}
}

func TestUnknownProtocolVersionNegotiatesLatest(t *testing.T) {
	messages := serve(t, initializeMessage(1, "2099-01-01")+"\n")
	result := messages[0]["result"].(map[string]any)
	if got := result["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", got, protocolVersion)
	}
}

func TestInitializeRequiresSchemaFields(t *testing.T) {
	messages := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`+"\n")
	assertErrorCode(t, messages[0], codeInvalidParams)
}

func TestOperationsRequireInitializedNotification(t *testing.T) {
	input := initializeMessage(1, protocolVersion) + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	messages := serve(t, input)
	if len(messages) != 2 {
		t.Fatalf("got %d replies, want 2", len(messages))
	}
	assertErrorCode(t, messages[1], codeInvalidRequest)
}

func TestNotificationsGetNoReply(t *testing.T) {
	messages := serve(t, readyMessages(protocolVersion,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`,
		`{"jsonrpc":"2.0","method":"notifications/something_new"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
	))
	if len(messages) != 1 {
		t.Fatalf("got %d replies, want only initialize response", len(messages))
	}
}

func TestNotificationMethodWithRequestIDIsInvalid(t *testing.T) {
	messages := serve(t, initializeMessage(1, protocolVersion)+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"notifications/initialized"}`+"\n")
	if len(messages) != 2 {
		t.Fatalf("got %d replies, want 2", len(messages))
	}
	assertErrorCode(t, messages[1], codeInvalidRequest)
}

func TestUnknownRequestGetsMethodNotFound(t *testing.T) {
	messages := serve(t, readyMessages(protocolVersion,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
	))
	assertErrorCode(t, messages[1], codeMethodNotFound)
}

func TestToolsListDescribesTypedReadOnlyTools(t *testing.T) {
	messages := serve(t, readyMessages(protocolVersion,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	))
	result := messages[1]["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	for index, toolName := range []string{"search", "read_evidence"} {
		tool := tools[index].(map[string]any)
		if tool["name"] != toolName || tool["title"] == "" {
			t.Errorf("unexpected identity: name=%v title=%v", tool["name"], tool["title"])
		}
		input := tool["inputSchema"].(map[string]any)
		if input["additionalProperties"] != false {
			t.Error("input schema must reject unknown arguments")
		}
		output := tool["outputSchema"].(map[string]any)
		if output["$schema"] != jsonSchema202012 {
			t.Errorf("output schema dialect = %v", output["$schema"])
		}
		annotations := tool["annotations"].(map[string]any)
		for key, want := range map[string]bool{
			"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
		} {
			if got := annotations[key]; got != want {
				t.Errorf("annotations.%s = %v, want %v", key, got, want)
			}
		}
	}
	description := tools[0].(map[string]any)["description"].(string)
	for _, want := range []string{"evidence, not an answer", "R0123456789ab:E1", "general knowledge", "complete=false"} {
		if !strings.Contains(description, want) {
			t.Errorf("description should mention %q", want)
		}
	}
}

func TestToolsListRejectsCursor(t *testing.T) {
	messages := serve(t, readyMessages(protocolVersion,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"cursor":"next"}}`,
	))
	assertErrorCode(t, messages[1], codeInvalidParams)
}

func TestToolsCallReturnsStructuredContentOverProtocol(t *testing.T) {
	stub := &stubRetriever{result: syntheticResult()}
	server, output := protocolServer(stub)
	server.accept(context.Background(), []byte(initializeMessage(1, protocolVersion)))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"question":"synthetic question"}}}`))
	server.activeWG.Wait()

	messages := decodeMessages(t, output.String())
	if len(messages) != 2 {
		t.Fatalf("got %d replies, want initialize and tool result", len(messages))
	}
	result := messages[1]["result"].(map[string]any)
	if result["isError"] != false {
		t.Errorf("isError = %v", result["isError"])
	}
	structured := result["structuredContent"].(map[string]any)
	packets := structured["packets"].([]any)
	reference := packets[0].(map[string]any)["reference"].(string)
	if !strings.HasPrefix(reference, structured["result_id"].(string)+":E1") {
		t.Errorf("structured packet = %v", packets[0])
	}
	content := result["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), "["+reference+"]") {
		t.Error("text fallback does not carry the structured packet reference")
	}
}

func TestInvalidToolArgumentsReturnToolError(t *testing.T) {
	server, output := protocolServer(&stubRetriever{result: syntheticResult()})
	server.accept(context.Background(), []byte(initializeMessage(1, protocolVersion)))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"question":""}}}`))
	server.activeWG.Wait()

	messages := decodeMessages(t, output.String())
	result := messages[1]["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}
	if _, exists := result["structuredContent"]; exists {
		t.Error("tool error must not pretend to satisfy the evidence output schema")
	}
}

func TestUnknownToolReturnsProtocolError(t *testing.T) {
	server, output := protocolServer(&stubRetriever{result: syntheticResult()})
	server.accept(context.Background(), []byte(initializeMessage(1, protocolVersion)))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"other_tool","arguments":{"question":"synthetic"}}}`))
	server.activeWG.Wait()

	messages := decodeMessages(t, output.String())
	assertErrorCode(t, messages[1], codeInvalidParams)
}

func TestParseAndInvalidRequestErrorsDoNotEndSession(t *testing.T) {
	input := "{not json\n" +
		`[]` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"
	messages := serve(t, input)
	if len(messages) != 3 {
		t.Fatalf("got %d replies, want 3", len(messages))
	}
	assertErrorCode(t, messages[0], codeParseError)
	assertErrorCode(t, messages[1], codeInvalidRequest)
	if _, ok := messages[2]["result"]; !ok {
		t.Error("session should continue after malformed frames")
	}
}

func TestRequestIDsMustBeValidAndUnique(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":null,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":4,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":4,"method":"ping"}` + "\n"
	messages := serve(t, input)
	assertErrorCode(t, messages[0], codeInvalidRequest)
	if messages[0]["id"] != nil {
		t.Errorf("invalid id response id = %v, want null", messages[0]["id"])
	}
	assertErrorCode(t, messages[2], codeInvalidRequest)
}

func TestToolCallCanBeCancelledWithoutResponse(t *testing.T) {
	reader, writer := io.Pipe()
	var output lockedBuffer
	retriever := &blockingRetriever{started: make(chan struct{})}
	server := NewServer(
		&QueryTool{Workspace: "synthetic", Service: retriever}, reader, &output,
		slog.New(slog.DiscardHandler),
	)
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()

	writeLine(t, writer, initializeMessage(1, protocolVersion))
	writeLine(t, writer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	writeLine(t, writer, `{"jsonrpc":"2.0","id":"query-1","method":"tools/call","params":{"name":"search","arguments":{"question":"synthetic question"}}}`)
	select {
	case <-retriever.started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool call did not start")
	}
	writeLine(t, writer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"query-1","reason":"synthetic cancellation"}}`)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
	messages := decodeMessages(t, output.String())
	if len(messages) != 1 {
		t.Fatalf("got %d replies, want only initialize response: %v", len(messages), messages)
	}
}

func TestContextCancellationUnblocksStdioReader(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	server := NewServer(
		&QueryTool{Workspace: "synthetic"}, reader, io.Discard,
		slog.New(slog.DiscardHandler),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not stop the server")
	}
}

func TestEveryOutputLineIsOneJSONObject(t *testing.T) {
	messages := serve(t, readyMessages(protocolVersion,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	))
	for _, message := range messages {
		if message["jsonrpc"] != "2.0" {
			t.Errorf("missing jsonrpc version: %v", message)
		}
		if _, ok := message["id"]; !ok {
			t.Errorf("response must carry an id: %v", message)
		}
	}
}

func TestLargeToolResponseStaysBelowAbsoluteClientLimit(t *testing.T) {
	stub := &stubRetriever{result: syntheticTextResult(strings.Repeat("large synthetic evidence 🙂\n", 5000))}
	server, output := protocolServer(stub)
	server.accept(context.Background(), []byte(initializeMessage(1, protocolVersion)))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	server.accept(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"question":"large"}}}`))
	server.activeWG.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d protocol responses", len(lines))
	}
	if len(lines[1])+1 > absoluteToolResponseBytes {
		t.Errorf("tool JSON-RPC response = %d bytes, absolute limit = %d", len(lines[1])+1, absoluteToolResponseBytes)
	}
}

func assertErrorCode(t *testing.T, message map[string]any, want int) {
	t.Helper()
	rpc, ok := message["error"].(map[string]any)
	if !ok {
		t.Fatalf("want error response, got %v", message)
	}
	if got := int(rpc["code"].(float64)); got != want {
		t.Errorf("error code = %d, want %d", got, want)
	}
}

func writeLine(t *testing.T, writer io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(writer, line+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func protocolServer(service Retriever) (*Server, *lockedBuffer) {
	output := &lockedBuffer{}
	server := NewServer(
		&QueryTool{Workspace: "synthetic", Service: service}, strings.NewReader(""), output,
		slog.New(slog.DiscardHandler),
	)
	return server, output
}

type blockingRetriever struct{ started chan struct{} }

func (r *blockingRetriever) Query(ctx context.Context, _ retrieval.Request) (*retrieval.Result, error) {
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
