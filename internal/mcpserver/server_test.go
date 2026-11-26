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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMCPServer_EndToEnd5ToolsAndSocketRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "runner.sock")

	var mu sync.Mutex
	seenCalls := map[string]map[string]any{}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})

	mux := http.NewServeMux()
	for _, action := range []string{"spawn", "list", "send", "read", "cancel"} {
		act := action
		mux.HandleFunc("/delegate/"+act, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Orbit-Delegate-Token") != "tok-secret-123" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seenCalls[act] = body
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"action": act,
				"echo":   body,
			})
		})
	}
	httpSrv := &http.Server{Handler: mux}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	backend := NewSocketBackend(sockPath, "tok-secret-123")
	var stderrBuf bytes.Buffer
	srv := New(backend, &stderrBuf)

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"orbit_spawn_child","arguments":{"goal":"Build parser","title":"Parser","role":"backend","project_id":"demo-app","adapter":"codex","model":"gpt-x","use_worktree":true,"notify_parent":true}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"orbit_list_children","arguments":{"include_targets":true}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"orbit_send_child","arguments":{"child_task_id":"tsk_c1","message":"Add tests","mode":"steer"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"orbit_read_child","arguments":{"child_task_id":"tsk_c1","limit":5}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"orbit_cancel_child","arguments":{"child_task_id":"tsk_c1","reason":"done"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"orbit_spawn_child","arguments":{}}}`,
	}

	var stdoutBuf bytes.Buffer
	err = srv.ServeStdio(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &stdoutBuf)
	if err != nil {
		t.Fatalf("ServeStdio error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	// 10 requests minus 1 notification = 9 responses
	if len(lines) != 9 {
		t.Fatalf("expected 9 JSON-RPC responses, got %d:\n%s", len(lines), stdoutBuf.String())
	}

	// Verify initialize (id=1)
	var initResp struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil || initResp.ID != 1 {
		t.Fatalf("bad initialize response: %s", lines[0])
	}

	// Verify tools/list (id=3) has strictly 5 tools
	var listResp struct {
		ID     int `json:"id"`
		Result struct {
			Tools []mcpToolDef `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &listResp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(listResp.Result.Tools) != 5 {
		t.Fatalf("expected strictly 5 tools, got %d", len(listResp.Result.Tools))
	}
	wantNames := []string{
		"orbit_spawn_child",
		"orbit_list_children",
		"orbit_send_child",
		"orbit_read_child",
		"orbit_cancel_child",
	}
	for i, w := range wantNames {
		if listResp.Result.Tools[i].Name != w {
			t.Fatalf("tool[%d] = %q, want %q", i, listResp.Result.Tools[i].Name, w)
		}
	}

	// Verify all 5 delegate actions reached the Unix socket with properly mapped fields
	mu.Lock()
	defer mu.Unlock()
	if seenCalls["spawn"]["goal"] != "Build parser" || seenCalls["spawn"]["projectId"] != "demo-app" || seenCalls["spawn"]["role"] != "backend" {
		t.Fatalf("unexpected spawn call payload: %+v", seenCalls["spawn"])
	}
	if seenCalls["list"]["includeTargets"] != true {
		t.Fatalf("unexpected list call payload: %+v", seenCalls["list"])
	}
	if seenCalls["send"]["childTaskId"] != "tsk_c1" || seenCalls["send"]["mode"] != "steer" {
		t.Fatalf("unexpected send call payload: %+v", seenCalls["send"])
	}
	if seenCalls["read"]["childTaskId"] != "tsk_c1" || seenCalls["read"]["limit"] != float64(5) {
		t.Fatalf("unexpected read call payload: %+v", seenCalls["read"])
	}
	if seenCalls["cancel"]["childTaskId"] != "tsk_c1" || seenCalls["cancel"]["reason"] != "done" {
		t.Fatalf("unexpected cancel call payload: %+v", seenCalls["cancel"])
	}

	// Verify id=9 (missing goal) returned isError=true without crashing
	var errToolResp struct {
		ID     int `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[8]), &errToolResp); err != nil || !errToolResp.Result.IsError {
		t.Fatalf("expected isError=true for missing goal, got: %s", lines[8])
	}
}

func TestMCPServer_ContentLengthFraming(t *testing.T) {
	srv := New(nil, &bytes.Buffer{})
	msg := `{"jsonrpc":"2.0","id":42,"method":"ping"}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(msg), msg)
	var out bytes.Buffer
	if err := srv.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("ServeStdio content-length error: %v", err)
	}
	if !strings.HasPrefix(out.String(), "Content-Length:") || !strings.Contains(out.String(), `"id":42`) {
		t.Fatalf("unexpected Content-Length output: %q", out.String())
	}
}

func TestMCPServer_FrameBoundsAndUnexpectedEOF(t *testing.T) {
	// 1. Oversize newline-delimited frame (> maxFrameBytes) must be rejected before unbounded allocation
	t.Run("oversize_line_rejected", func(t *testing.T) {
		big := `{"jsonrpc":"2.0","id":1,"method":"ping","pad":"` + strings.Repeat("a", maxFrameBytes+64) + "\"}\n"
		_, _, err := readFrame(bufio.NewReader(strings.NewReader(big)))
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
			t.Fatalf("expected oversize line error, got: %v", err)
		}
	})

	// 2. Oversize Content-Length (> maxFrameBytes) must be rejected BEFORE allocating buffer
	t.Run("oversize_content_length_rejected", func(t *testing.T) {
		input := fmt.Sprintf("Content-Length: %d\r\n\r\n{}", maxFrameBytes+1)
		_, _, err := readFrame(bufio.NewReader(strings.NewReader(input)))
		if err == nil || !strings.Contains(err.Error(), "invalid Content-Length") {
			t.Fatalf("expected invalid Content-Length error, got: %v", err)
		}
	})

	// 3. Zero Content-Length rejected
	t.Run("zero_content_length_rejected", func(t *testing.T) {
		_, _, err := readFrame(bufio.NewReader(strings.NewReader("Content-Length: 0\r\n\r\n")))
		if err == nil || !strings.Contains(err.Error(), "invalid Content-Length") {
			t.Fatalf("expected invalid Content-Length error for 0, got: %v", err)
		}
	})

	// 4. Oversize header block (> maxHeaderBytes) rejected
	t.Run("oversize_headers_rejected", func(t *testing.T) {
		hdr := "Content-Length: 2\r\nX-Pad: " + strings.Repeat("h", maxHeaderBytes+128) + "\r\n\r\n{}"
		_, _, err := readFrame(bufio.NewReader(strings.NewReader(hdr)))
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
			t.Fatalf("expected oversize header error, got: %v", err)
		}
	})

	// 5. Clean EOF at frame boundary returns io.EOF (ServeStdio exits cleanly with nil)
	t.Run("clean_eof_boundary", func(t *testing.T) {
		srv := New(nil, &bytes.Buffer{})
		var out bytes.Buffer
		if err := srv.ServeStdio(context.Background(), strings.NewReader("\n\n"), &out); err != nil {
			t.Fatalf("expected nil on clean EOF, got: %v", err)
		}
	})

	// 6. Mid-header EOF returns io.ErrUnexpectedEOF
	t.Run("mid_header_unexpected_eof", func(t *testing.T) {
		_, _, err := readFrame(bufio.NewReader(strings.NewReader("Content-Length: 10\r\nX-Foo: bar")))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected io.ErrUnexpectedEOF mid-header, got: %v", err)
		}
	})

	// 7. Mid-body EOF returns io.ErrUnexpectedEOF (and ServeStdio propagates error instead of nil)
	t.Run("mid_body_unexpected_eof", func(t *testing.T) {
		srv := New(nil, &bytes.Buffer{})
		var out bytes.Buffer
		err := srv.ServeStdio(context.Background(), strings.NewReader("Content-Length: 50\r\n\r\n{\"jsonrpc\":\"2.0\""), &out)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected io.ErrUnexpectedEOF mid-body, got: %v", err)
		}
	})
}
