package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultProtocolVersion = "2024-11-05"
	serverName             = "orbit"
	serverVersion          = "0.3.2"
	maxHeaderBytes         = 16 * 1024
	maxFrameBytes          = 4 * 1024 * 1024
)

// DelegateBackend executes the 5 Orbit sub-session delegation operations against the local Runner socket.
type DelegateBackend interface {
	CallDelegate(ctx context.Context, action string, payload map[string]any) ([]byte, error)
}

// SocketBackend calls Orbit Runner's Unix domain socket /delegate/<action> endpoints with X-Orbit-Delegate-Token.
type SocketBackend struct {
	SockPath      string
	DelegateToken string
	HTTPClient    *http.Client
}

// NewSocketBackend creates a SocketBackend talking over sockPath using delegateToken.
func NewSocketBackend(sockPath, delegateToken string) *SocketBackend {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &SocketBackend{
		SockPath:      sockPath,
		DelegateToken: delegateToken,
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// CallDelegate posts payload to /delegate/<action> on the Runner Unix socket.
func (b *SocketBackend) CallDelegate(ctx context.Context, action string, payload map[string]any) ([]byte, error) {
	if strings.TrimSpace(b.SockPath) == "" {
		return nil, errors.New("ORBIT_RUNNER_SOCK is not set")
	}
	if strings.TrimSpace(b.DelegateToken) == "" {
		return nil, errors.New("ORBIT_DELEGATE_TOKEN is not set")
	}
	if payload == nil {
		payload = map[string]any{}
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/"+action, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orbit-Delegate-Token", b.DelegateToken)

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call runner socket /delegate/%s: %w", action, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("delegate %s failed (%d): %s", action, resp.StatusCode, msg)
	}
	return raw, nil
}

// Server implements a lightweight JSON-RPC 2.0 stdio MCP server exposing exactly 5 Orbit sub-session tools.
type Server struct {
	backend DelegateBackend
	stderr  io.Writer
}

// New creates a new Orbit MCP stdio server.
func New(backend DelegateBackend, stderr io.Writer) *Server {
	if stderr == nil {
		stderr = io.Discard
	}
	return &Server{
		backend: backend,
		stderr:  stderr,
	}
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolDefinitions returns the strict 5-tool catalog exposed by h8o mcp-serve.
func ToolDefinitions() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "orbit_spawn_child",
			Description: "Spawn an observable Orbit child session (max depth = 1; child sessions cannot spawn grandchildren, and PA/Hebi8 sessions cannot attach child sessions). Results are delivered back automatically when it finishes.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal":          map[string]any{"type": "string", "description": "Clear goal and acceptance criteria for the child session"},
					"title":         map[string]any{"type": "string", "description": "Short display title"},
					"role":          map[string]any{"type": "string", "description": "Optional role label (e.g. reviewer, backend, researcher)"},
					"project_id":    map[string]any{"type": "string", "description": "Target project ID (defaults to parent project)"},
					"machine_id":    map[string]any{"type": "string", "description": "Optional target machine ID"},
					"adapter":       map[string]any{"type": "string", "enum": []string{"agy", "claude", "codex"}, "description": "Optional agent adapter"},
					"model":         map[string]any{"type": "string", "description": "Optional model override"},
					"effort":        map[string]any{"type": "string", "description": "Optional thought/effort level"},
					"mode":          map[string]any{"type": "string", "description": "Optional permission mode (ask|auto|full)"},
					"use_worktree":  map[string]any{"type": "boolean", "description": "Override Git worktree isolation"},
					"notify_parent": map[string]any{"type": "boolean", "description": "Auto-report completion to parent (default true)"},
				},
				"required": []string{"goal"},
			},
		},
		{
			Name:        "orbit_list_children",
			Description: "List child sessions spawned from this top-level session, or peer sibling sessions sharing the same parent when called from a child session (with optional available machines and projects for top-level sessions).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_archived": map[string]any{"type": "boolean", "description": "Include archived child or sibling sessions"},
					"include_targets":  map[string]any{"type": "boolean", "description": "Also return compact available machines and projects (top-level sessions only)"},
				},
			},
		},
		{
			Name:        "orbit_send_child",
			Description: "Send a follow-up message to a child session (from its parent) or a same-parent sibling session (from a peer child); queue behind active turn or steer immediately.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child_task_id": map[string]any{"type": "string", "description": "Target child or same-parent sibling task ID"},
					"message":       map[string]any{"type": "string", "description": "Instruction to send"},
					"mode":          map[string]any{"type": "string", "enum": []string{"queue", "steer"}, "description": "Delivery mode (default queue)"},
				},
				"required": []string{"child_task_id", "message"},
			},
		},
		{
			Name:        "orbit_read_child",
			Description: "Read compact per-turn transcript (prompts, replies, tool counts, delivery stats) of a child session or a same-parent sibling session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child_task_id": map[string]any{"type": "string", "description": "Target child or same-parent sibling task ID"},
					"limit":         map[string]any{"type": "integer", "description": "Max recent turns to return (default 10)"},
				},
				"required": []string{"child_task_id"},
			},
		},
		{
			Name:        "orbit_cancel_child",
			Description: "Cancel an active attempt on a direct child session (parent sessions only; sibling sessions cannot cancel each other).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child_task_id": map[string]any{"type": "string", "description": "Target direct child task ID"},
					"reason":        map[string]any{"type": "string", "description": "Optional cancellation reason"},
				},
				"required": []string{"child_task_id"},
			},
		},
		{
			Name:        "orbit_board_read",
			Description: "Read recent messages and member states from the shared Board of this task group (accessible by the top-level parent session and its direct child sessions).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"before_seq": map[string]any{"type": "integer", "description": "Optional pagination cursor: return messages strictly before this seq"},
					"limit":      map[string]any{"type": "integer", "description": "Max recent board messages to return (default 50)"},
				},
			},
		},
		{
			Name:        "orbit_board_post",
			Description: "Post a progress update, decision, finding, or shared context message to the shared Board of this task group (parent and direct child sessions).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"body":           map[string]any{"type": "string", "description": "Message body to post to the shared Board"},
					"target_mention": map[string]any{"type": "string", "description": "Optional @mention targeting a sibling title/taskId or @parent"},
				},
				"required": []string{"body"},
			},
		},
	}
}

// ServeStdio reads JSON-RPC 2.0 MCP frames from r and writes responses to w until EOF or ctx cancellation.
// Supports both standard MCP newline-delimited JSON and Content-Length LSP-style framing.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		payload, framed, err := readFrame(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			fmt.Fprintf(s.stderr, "h8o mcp-serve: read frame error: %v\n", err)
			return err
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			resp := jsonRPCResponse{
				JSONRPC: "2.0",
				Error:   &jsonRPCError{Code: -32700, Message: "parse error: " + err.Error()},
			}
			if wErr := writeFrame(w, resp, framed); wErr != nil {
				return wErr
			}
			continue
		}

		resp, hasResp := s.handleRequest(ctx, req)
		if !hasResp {
			continue
		}
		if err := writeFrame(w, resp, framed); err != nil {
			return err
		}
	}
}

func (s *Server) handleRequest(ctx context.Context, req jsonRPCRequest) (jsonRPCResponse, bool) {
	isNotification := len(bytes.TrimSpace(req.ID)) == 0 || bytes.Equal(bytes.TrimSpace(req.ID), []byte("null"))

	switch req.Method {
	case "initialize":
		protoVer := defaultProtocolVersion
		if len(req.Params) > 0 {
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &p) == nil && strings.TrimSpace(p.ProtocolVersion) != "" {
				protoVer = strings.TrimSpace(p.ProtocolVersion)
			}
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protoVer,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    serverName,
					"version": serverVersion,
				},
			},
		}, true

	case "notifications/initialized", "initialized", "notifications/cancelled":
		return jsonRPCResponse{}, false

	case "ping":
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}, !isNotification

	case "tools/list":
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": ToolDefinitions(),
			},
		}, true

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &jsonRPCError{Code: -32602, Message: "invalid params: " + err.Error()},
			}, true
		}
		text, isErr := s.executeTool(ctx, params.Name, params.Arguments)
		result := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		}
		if isErr {
			result["isError"] = true
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}, true

	default:
		if isNotification {
			return jsonRPCResponse{}, false
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32601, Message: "method not found: " + req.Method},
		}, true
	}
}

func (s *Server) executeTool(ctx context.Context, name string, args map[string]any) (string, bool) {
	if s.backend == nil {
		return "orbit delegate backend is not configured", true
	}
	if args == nil {
		args = map[string]any{}
	}

	var (
		action  string
		payload map[string]any
	)

	switch name {
	case "orbit_spawn_child":
		goal := strArg(args, "goal")
		if goal == "" {
			return "goal is required", true
		}
		action = "spawn"
		payload = map[string]any{"goal": goal}
		if v := strArg(args, "title"); v != "" {
			payload["title"] = v
		}
		if v := strArg(args, "role"); v != "" {
			payload["role"] = v
		}
		if v := strArg(args, "project_id", "projectId"); v != "" {
			payload["projectId"] = v
		}
		if v := strArg(args, "machine_id", "machineId"); v != "" {
			payload["machineId"] = v
		}
		if v := strArg(args, "adapter"); v != "" {
			payload["adapter"] = v
		}
		cfg := map[string]string{}
		if v := strArg(args, "model"); v != "" {
			cfg["model"] = v
		}
		if v := strArg(args, "effort", "thought_level"); v != "" {
			cfg["thought_level"] = v
		}
		if v := strArg(args, "mode"); v != "" {
			cfg["mode"] = v
		}
		if len(cfg) > 0 {
			payload["config"] = cfg
		}
		if v, ok := boolArg(args, "use_worktree", "useWorktree"); ok {
			payload["useWorktree"] = v
		}
		if v, ok := boolArg(args, "notify_parent", "notifyParent"); ok {
			payload["notifyParent"] = v
		}

	case "orbit_list_children":
		action = "list"
		payload = map[string]any{}
		if v, ok := boolArg(args, "include_archived", "includeArchived"); ok {
			payload["includeArchived"] = v
		}
		if v, ok := boolArg(args, "include_targets", "includeTargets"); ok {
			payload["includeTargets"] = v
		}

	case "orbit_send_child":
		childID := strArg(args, "child_task_id", "childTaskId")
		msg := strArg(args, "message")
		if childID == "" || msg == "" {
			return "child_task_id and message are required", true
		}
		action = "send"
		payload = map[string]any{
			"childTaskId": childID,
			"message":     msg,
		}
		if v := strArg(args, "mode"); v != "" {
			payload["mode"] = v
		}

	case "orbit_read_child":
		childID := strArg(args, "child_task_id", "childTaskId")
		if childID == "" {
			return "child_task_id is required", true
		}
		action = "read"
		payload = map[string]any{
			"childTaskId": childID,
		}
		if n, ok := intArg(args, "limit"); ok && n > 0 {
			payload["limit"] = n
		}

	case "orbit_cancel_child":
		childID := strArg(args, "child_task_id", "childTaskId")
		if childID == "" {
			return "child_task_id is required", true
		}
		action = "cancel"
		payload = map[string]any{
			"childTaskId": childID,
		}
		if v := strArg(args, "reason"); v != "" {
			payload["reason"] = v
		}

	case "orbit_board_read":
		action = "board-read"
		payload = map[string]any{}
		if n, ok := intArg(args, "before_seq", "beforeSeq"); ok && n > 0 {
			payload["beforeSeq"] = n
		}
		if n, ok := intArg(args, "limit"); ok && n > 0 {
			payload["limit"] = n
		}

	case "orbit_board_post":
		body := strArg(args, "body", "message")
		if body == "" {
			return "body is required", true
		}
		action = "board-post"
		payload = map[string]any{
			"body": body,
		}
		if v := strArg(args, "target_mention", "targetMention", "target"); v != "" {
			payload["targetMention"] = v
		}

	default:
		return fmt.Sprintf("unknown tool %q", name), true
	}

	raw, err := s.backend.CallDelegate(ctx, action, payload)
	if err != nil {
		fmt.Fprintf(s.stderr, "h8o mcp-serve: tool %s error: %v\n", name, err)
		return err.Error(), true
	}
	return string(bytes.TrimSpace(raw)), false
}

func strArg(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func boolArg(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func intArg(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		}
	}
	return 0, false
}

func readLineBounded(br *bufio.Reader, maxBytes int) (string, bool, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > maxBytes {
			return "", false, fmt.Errorf("input line exceeds maximum size of %d bytes", maxBytes)
		}
		buf = append(buf, frag...)
		if err == nil {
			return string(buf), false, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(buf) == 0 {
				return "", true, io.EOF
			}
			return string(buf), true, nil
		}
		return "", false, err
	}
}

func readFrame(br *bufio.Reader) ([]byte, bool, error) {
	for {
		line, hitEOF, err := readLineBounded(br, maxFrameBytes)
		if err != nil {
			return nil, false, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if hitEOF {
				return nil, false, io.EOF
			}
			continue
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			return []byte(trimmed), false, nil
		}
		// Content-Length header framing fallback with strict header and body size caps.
		if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
			headerBytes := len(line)
			if headerBytes > maxHeaderBytes {
				return nil, true, fmt.Errorf("MCP headers exceed maximum size of %d bytes", maxHeaderBytes)
			}
			lengthStr := strings.TrimSpace(trimmed[len("content-length:"):])
			n, convErr := strconv.Atoi(lengthStr)
			if convErr != nil || n <= 0 || n > maxFrameBytes {
				return nil, true, fmt.Errorf("invalid Content-Length %q (allowed 1..%d)", lengthStr, maxFrameBytes)
			}
			if hitEOF {
				return nil, true, io.ErrUnexpectedEOF
			}
			// Drain remaining headers until blank line
			for {
				remainingHeader := maxHeaderBytes - headerBytes
				if remainingHeader <= 0 {
					return nil, true, fmt.Errorf("MCP headers exceed maximum size of %d bytes", maxHeaderBytes)
				}
				hLine, hEOF, hErr := readLineBounded(br, remainingHeader)
				if hErr != nil {
					if errors.Is(hErr, io.EOF) {
						return nil, true, io.ErrUnexpectedEOF
					}
					return nil, true, hErr
				}
				headerBytes += len(hLine)
				if strings.TrimSpace(hLine) == "" {
					break
				}
				if hEOF {
					return nil, true, io.ErrUnexpectedEOF
				}
			}
			buf := make([]byte, n)
			if _, rErr := io.ReadFull(br, buf); rErr != nil {
				if errors.Is(rErr, io.EOF) {
					return nil, true, io.ErrUnexpectedEOF
				}
				return nil, true, rErr
			}
			return buf, true, nil
		}
		if hitEOF {
			return []byte(trimmed), false, nil
		}
	}
}

func writeFrame(w io.Writer, resp jsonRPCResponse, contentLengthFramed bool) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if contentLengthFramed {
		if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
