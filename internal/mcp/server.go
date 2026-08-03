// Package mcp exposes the read path as a Model Context Protocol tool over
// stdio (retrieval-design.md §6.1).
//
// This is the surface through which answer generation happens. Retrieval
// returns cited source passages; an agent calls this tool, reads them, and
// writes the answer itself. Nothing in this repository generates prose — a
// small local model misattributes who said what, which for a family-law corpus
// is the failure that makes output unusable.
//
// The protocol is JSON-RPC 2.0 framed as newline-delimited JSON on stdin and
// stdout. It is implemented directly rather than through an SDK: three methods
// is less code than a dependency, and it keeps the transport boundary visible.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// protocolVersion is what this server implements. A client asking for a
// version we know is echoed back; anything else gets this one, and the client
// decides whether to proceed.
const protocolVersion = "2024-11-05"

var knownVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// JSON-RPC 2.0 error codes used here.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether no response may be sent. A notification has
// no id; replying to one is a protocol violation that some clients treat as
// fatal.
func (r *request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server speaks MCP over a reader/writer pair.
//
// Writes are serialised: stdout carries the protocol and nothing else, so
// interleaved output would corrupt the stream rather than merely look untidy.
// Everything diagnostic goes to Log, which writes to a file.
type Server struct {
	Tool *QueryTool
	Log  *slog.Logger

	in  io.Reader
	out io.Writer
	mu  sync.Mutex
}

func NewServer(tool *QueryTool, in io.Reader, out io.Writer, log *slog.Logger) *Server {
	return &Server{Tool: tool, Log: log, in: in, out: out}
}

// Serve runs until stdin closes or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	// Packets carry whole documents, so a reply can be large; the default
	// 64KB scanner limit would truncate a request mid-object.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.Log.Warn("mcp: unparsable message", "error", err)
			s.replyError(nil, codeParseError, "parse error")
			continue
		}
		s.handle(ctx, &req)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp read: %w", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req *request) {
	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.initialize(req.Params))

	case "notifications/initialized", "notifications/cancelled":
		// Notifications get no response, ever.

	case "ping":
		s.reply(req.ID, map[string]any{})

	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": []any{s.Tool.Describe()}})

	case "tools/call":
		result, err := s.Tool.Call(ctx, req.Params)
		if err != nil {
			// A failed search is reported as tool content with isError, not as
			// a protocol error: the agent should see what went wrong and can
			// still answer "I could not search", rather than the call vanishing.
			s.Log.Error("mcp: tool call failed", "error", err)
			s.reply(req.ID, errorResult(err))
			return
		}
		s.reply(req.ID, result)

	default:
		if req.isNotification() {
			return
		}
		s.replyError(req.ID, codeMethodNotFound, "unknown method: "+req.Method)
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)

	version := protocolVersion
	if knownVersions[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    "pocket-advisor",
			"version": "1",
		},
	}
}

func (s *Server) reply(id json.RawMessage, result any) {
	if len(id) == 0 {
		return // notification
	}
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) write(resp response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	enc := json.NewEncoder(s.out)
	if err := enc.Encode(resp); err != nil {
		s.Log.Error("mcp: write failed", "error", err)
	}
}
