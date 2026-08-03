package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func serve(t *testing.T, in string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	s := NewServer(&QueryTool{Workspace: "test"}, strings.NewReader(in), &out,
		slog.New(slog.DiscardHandler))
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("reply is not JSON: %q", line)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestInitializeHandshake(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`+"\n")
	if len(msgs) != 1 {
		t.Fatalf("got %d replies, want 1", len(msgs))
	}
	res, _ := msgs[0]["result"].(map[string]any)
	// A client's version is echoed when we know it, so the session negotiates
	// rather than forcing ours.
	if got := res["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's", got)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability must be advertised")
	}
}

func TestUnknownProtocolVersionFallsBackToOurs(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`+"\n")
	res := msgs[0]["result"].(map[string]any)
	if got := res["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %v, want %v", got, protocolVersion)
	}
}

// Replying to a notification is a protocol violation some clients treat as
// fatal, and notifications/initialized arrives in every session.
func TestNotificationsGetNoReply(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`+"\n")
	if len(msgs) != 0 {
		t.Fatalf("got %d replies to notifications, want 0: %v", len(msgs), msgs)
	}
}

func TestUnknownNotificationStillGetsNoReply(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","method":"notifications/somethingNew"}`+"\n")
	if len(msgs) != 0 {
		t.Fatalf("got %d replies, want 0", len(msgs))
	}
}

func TestUnknownRequestGetsMethodNotFound(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`+"\n")
	if len(msgs) != 1 {
		t.Fatalf("got %d replies, want 1", len(msgs))
	}
	e, ok := msgs[0]["error"].(map[string]any)
	if !ok {
		t.Fatal("want an error reply")
	}
	if int(e["code"].(float64)) != codeMethodNotFound {
		t.Errorf("code = %v, want %d", e["code"], codeMethodNotFound)
	}
}

func TestToolsListDescribesTheTool(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	res := msgs[0]["result"].(map[string]any)
	tools := res["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != toolName {
		t.Errorf("name = %v", tool["name"])
	}
	schema := tool["inputSchema"].(map[string]any)
	req := schema["required"].([]any)
	if len(req) != 1 || req[0] != "question" {
		t.Errorf("required = %v, want [question]", req)
	}
	// The agent must be told these are sources rather than an answer, or the
	// traceability the whole design exists for is discarded at the last step.
	desc, _ := tool["description"].(string)
	for _, want := range []string{"citations", "not an answer", "[n]"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description should mention %q", want)
		}
	}
}

// Malformed input must not kill the session — a client that sends one bad
// frame should be able to carry on.
func TestParseErrorDoesNotEndTheSession(t *testing.T) {
	msgs := serve(t, "{not json\n"+`{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n")
	if len(msgs) != 2 {
		t.Fatalf("got %d replies, want 2", len(msgs))
	}
	if _, ok := msgs[0]["error"]; !ok {
		t.Error("first reply should be a parse error")
	}
	if _, ok := msgs[1]["result"]; !ok {
		t.Error("session should continue after a bad frame")
	}
}

// stdout is the protocol channel: every line must be a complete JSON object,
// or the client's framing breaks.
func TestEveryLineIsOneJSONObject(t *testing.T) {
	msgs := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	for _, m := range msgs {
		if m["jsonrpc"] != "2.0" {
			t.Errorf("missing jsonrpc version: %v", m)
		}
		if _, ok := m["id"]; !ok {
			t.Errorf("response must carry an id: %v", m)
		}
	}
}
