// Package mcp exposes the read path as a Model Context Protocol tool over
// newline-delimited JSON-RPC on stdio. Retrieval returns cited source evidence;
// answer generation remains in the MCP client.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync"
)

const (
	protocolVersion       = "2025-11-25"
	maxRequestMessageSize = 8 * 1024 * 1024
	maxRequestIDBytes     = 256
)

// Every listed revision is exercised by the protocol tests. The method set is
// deliberately small and backwards compatible across these final revisions.
var supportedProtocolVersions = []string{
	"2024-11-05",
	"2025-03-26",
	"2025-06-18",
	protocolVersion,
}

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

type inputFrame struct {
	data []byte
	err  error
}

// Server speaks MCP over one reader/writer pair. The lifecycle and active
// request maps are session-local; no request can select or mutate Tool's fixed
// workspace.
type Server struct {
	Tool *QueryTool
	Log  *slog.Logger

	in  io.Reader
	out io.Writer

	writeMu sync.Mutex
	stateMu sync.Mutex

	initialized bool
	ready       bool
	seenIDs     map[string]struct{}
	active      map[string]context.CancelFunc
	activeWG    sync.WaitGroup
}

func NewServer(tool *QueryTool, in io.Reader, out io.Writer, log *slog.Logger) *Server {
	return &Server{
		Tool: tool, Log: log, in: in, out: out,
		seenIDs: make(map[string]struct{}), active: make(map[string]context.CancelFunc),
	}
}

// Serve runs until stdin closes or ctx is cancelled. Tool calls run
// concurrently so the scanner can receive cancellation notifications while a
// model or database call is in flight. All responses remain write-serialized.
func (s *Server) Serve(ctx context.Context) error {
	frames := make(chan inputFrame, 1)
	go s.scan(ctx, frames)

	for {
		select {
		case <-ctx.Done():
			if closer, ok := s.in.(io.Closer); ok {
				_ = closer.Close()
			}
			s.shutdownActive()
			return nil
		case frame, ok := <-frames:
			if !ok {
				s.shutdownActive()
				return nil
			}
			if frame.err != nil {
				s.shutdownActive()
				return fmt.Errorf("mcp read: %w", frame.err)
			}
			s.accept(ctx, frame.data)
		}
	}
}

func (s *Server) scan(ctx context.Context, frames chan<- inputFrame) {
	defer close(frames)
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRequestMessageSize)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case frames <- inputFrame{data: line}:
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		if ctx.Err() != nil {
			return
		}
		select {
		case frames <- inputFrame{err: err}:
		case <-ctx.Done():
		}
	}
}

func (s *Server) accept(ctx context.Context, line []byte) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return
	}
	if line[0] != '{' {
		if json.Valid(line) {
			s.replyError(nil, codeInvalidRequest, "invalid JSON-RPC request")
		} else {
			s.replyError(nil, codeParseError, "parse error")
		}
		return
	}

	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.Log.Warn("mcp: unparsable message", "error", err)
		s.replyError(nil, codeParseError, "parse error")
		return
	}
	key, rpcErr := s.validateEnvelope(&req)
	if rpcErr != nil {
		if !req.isNotification() {
			s.replyError(validResponseID(req.ID), rpcErr.Code, rpcErr.Message)
		}
		return
	}
	s.handle(ctx, &req, key)
}

func (s *Server) validateEnvelope(req *request) (string, *rpcError) {
	if req.JSONRPC != "2.0" || req.Method == "" {
		return "", &rpcError{Code: codeInvalidRequest, Message: "invalid JSON-RPC request"}
	}
	if req.isNotification() {
		return "", nil
	}
	key, err := requestIDKey(req.ID)
	if err != nil {
		return "", &rpcError{Code: codeInvalidRequest, Message: "request id must be a string or integer"}
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if _, duplicate := s.seenIDs[key]; duplicate {
		return "", &rpcError{Code: codeInvalidRequest, Message: "request id was already used"}
	}
	s.seenIDs[key] = struct{}{}
	return key, nil
}

func (s *Server) handle(ctx context.Context, req *request, key string) {
	if req.isNotification() {
		switch req.Method {
		case "notifications/initialized":
			s.markReady()
		case "notifications/cancelled":
			s.cancel(req.Params)
		}
		return
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		s.replyError(req.ID, codeInvalidRequest, "notification method must not have a request id")
		return
	}

	switch req.Method {
	case "initialize":
		result, rpcErr := s.initialize(req.Params)
		if rpcErr != nil {
			s.replyError(req.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		s.reply(req.ID, result)

	case "ping":
		s.reply(req.ID, map[string]any{})

	case "tools/list":
		if !s.isReady() {
			s.replyError(req.ID, codeInvalidRequest, "server is not initialized")
			return
		}
		if rpcErr := validateToolsListParams(req.Params); rpcErr != nil {
			s.replyError(req.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		s.reply(req.ID, map[string]any{"tools": s.Tool.DescribeAll()})

	case "tools/call":
		if !s.isReady() {
			s.replyError(req.ID, codeInvalidRequest, "server is not initialized")
			return
		}
		s.launchToolCall(ctx, req, key)

	default:
		s.replyError(req.ID, codeMethodNotFound, "method not found")
	}
}

type initializeParams struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ClientInfo      map[string]json.RawMessage `json:"clientInfo"`
	Meta            json.RawMessage            `json:"_meta,omitempty"`
}

func (s *Server) initialize(raw json.RawMessage) (map[string]any, *rpcError) {
	var params initializeParams
	if err := decodeStrict(raw, &params); err != nil || params.ProtocolVersion == "" ||
		params.Capabilities == nil || params.ClientInfo == nil ||
		requiredString(params.ClientInfo, "name") == "" || requiredString(params.ClientInfo, "version") == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "initialize params do not match the MCP schema"}
	}

	version := protocolVersion
	for _, supported := range supportedProtocolVersions {
		if params.ProtocolVersion == supported {
			version = supported
			break
		}
	}

	s.stateMu.Lock()
	if s.initialized {
		s.stateMu.Unlock()
		return nil, &rpcError{Code: codeInvalidRequest, Message: "server is already initialized"}
	}
	s.initialized = true
	s.ready = false
	s.stateMu.Unlock()
	s.Log.Info("mcp session initialized", "protocol_version", version)

	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name": "pocket-advisor", "title": "Pocket Advisor Evidence", "version": "1",
		},
		"instructions": "This server returns private workspace evidence, not generated answers. Cite complete result-scoped references. When complete=false, call continuation_tool with exactly next_cursor until complete=true. Do not answer from general knowledge when no evidence is returned.",
	}, nil
}

func requiredString(object map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := object[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return strings.TrimSpace(value)
}

func (s *Server) markReady() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.initialized {
		s.ready = true
	}
}

func (s *Server) isReady() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.ready
}

func validateToolsListParams(raw json.RawMessage) *rpcError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var params struct {
		Cursor *string         `json:"cursor,omitempty"`
		Meta   json.RawMessage `json:"_meta,omitempty"`
	}
	if err := decodeStrict(raw, &params); err != nil {
		return &rpcError{Code: codeInvalidParams, Message: "tools/list params do not match the MCP schema"}
	}
	if params.Cursor != nil {
		return &rpcError{Code: codeInvalidParams, Message: "tools list is not paginated"}
	}
	return nil
}

func (s *Server) launchToolCall(parent context.Context, req *request, key string) {
	callCtx, cancel := context.WithCancel(parent)
	s.stateMu.Lock()
	s.active[key] = cancel
	s.activeWG.Add(1)
	s.stateMu.Unlock()

	params := append(json.RawMessage(nil), req.Params...)
	id := append(json.RawMessage(nil), req.ID...)
	go func() {
		defer s.activeWG.Done()
		defer cancel()
		defer func() {
			s.stateMu.Lock()
			delete(s.active, key)
			s.stateMu.Unlock()
		}()

		result, err := s.Tool.Call(callCtx, params)
		if callCtx.Err() != nil {
			return
		}
		if err != nil {
			var unknown *unknownToolError
			if errors.As(err, &unknown) {
				s.replyError(id, codeInvalidParams, "unknown tool")
				return
			}
			var args *argumentError
			if !errors.As(err, &args) {
				s.Log.Error("mcp: tool call failed", "kind", toolErrorKind(err))
			}
			s.reply(id, errorResult(err))
			return
		}
		s.reply(id, result)
	}()
}

func toolErrorKind(err error) string {
	var size *resultSizeError
	var lines *resultLineError
	if errors.As(err, &size) {
		return "result_too_large"
	}
	if errors.As(err, &lines) {
		return "result_too_many_lines"
	}
	return "retrieval_unavailable"
}

type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

func (s *Server) cancel(raw json.RawMessage) {
	var params cancelledParams
	if err := decodeStrict(raw, &params); err != nil {
		return
	}
	key, err := requestIDKey(params.RequestID)
	if err != nil {
		return
	}
	s.stateMu.Lock()
	cancel := s.active[key]
	s.stateMu.Unlock()
	if cancel != nil {
		s.Log.Info("mcp request cancelled")
		cancel()
	}
}

func (s *Server) shutdownActive() {
	s.stateMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.stateMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.activeWG.Wait()
	if s.Tool != nil {
		s.Tool.closeSnapshots()
	}
}

func requestIDKey(raw json.RawMessage) (string, error) {
	if len(raw) > maxRequestIDBytes {
		return "", fmt.Errorf("request id is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch id := value.(type) {
	case string:
		return "s:" + id, nil
	case json.Number:
		text := id.String()
		if strings.ContainsAny(text, ".eE") {
			return "", fmt.Errorf("request id is not an integer")
		}
		integer := new(big.Int)
		if _, ok := integer.SetString(text, 10); !ok {
			return "", fmt.Errorf("request id is not an integer")
		}
		return "n:" + integer.String(), nil
	default:
		return "", fmt.Errorf("unsupported request id")
	}
}

func validResponseID(raw json.RawMessage) json.RawMessage {
	if _, err := requestIDKey(raw); err != nil {
		return nil
	}
	return raw
}

func (s *Server) reply(id json.RawMessage, result any) {
	if len(id) == 0 {
		return
	}
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyError(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *Server) write(resp response) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.Log.Error("mcp: encode response failed", "error", err)
		return
	}
	if len(encoded)+1 > absoluteToolResponseBytes {
		resp.Result = nil
		resp.Error = &rpcError{Code: codeInternalError, Message: "response exceeded the server safety limit"}
		encoded, err = json.Marshal(resp)
		if err != nil {
			s.Log.Error("mcp: encode bounded error response failed", "error", err)
			return
		}
		s.Log.Error("mcp: bounded oversized response", "kind", "response_too_large")
	}
	encoded = append(encoded, '\n')
	if _, err := s.out.Write(encoded); err != nil {
		s.Log.Error("mcp: write failed", "error", err)
	}
}
