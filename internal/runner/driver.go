package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	acp "github.com/coder/acp-go-sdk"
)

// PermissionWriteMeta carries the ACP JSON-RPC request ID and a channel signaled strictly AFTER
// the JSON-RPC permission response bytes finish writing to the underlying ACP stdio Writer (B6).
type PermissionWriteMeta struct {
	RPCID       string
	WriteDoneCh <-chan error
}

type permWriteMetaContextKey struct{}
type sessionMCPServersContextKey struct{}
type sessionMCPAttachReportContextKey struct{}

// WithSessionMCPServers attaches optional MCP servers to be injected into ACP NewSession/ResumeSession/LoadSession calls.
func WithSessionMCPServers(ctx context.Context, servers []acp.McpServer) context.Context {
	if len(servers) == 0 {
		return ctx
	}
	cp := append([]acp.McpServer(nil), servers...)
	return context.WithValue(ctx, sessionMCPServersContextKey{}, cp)
}

// SessionMCPServersFromContext returns MCP servers attached via WithSessionMCPServers, if any.
func SessionMCPServersFromContext(ctx context.Context) []acp.McpServer {
	if ctx == nil {
		return nil
	}
	if servers, ok := ctx.Value(sessionMCPServersContextKey{}).([]acp.McpServer); ok && len(servers) > 0 {
		return append([]acp.McpServer(nil), servers...)
	}
	return nil
}

// WithSessionMCPAttachReport attaches a bool pointer that ACP drivers set to true ONLY when
// NewSession/ResumeSession/LoadSession succeeded with the requested Orbit MCP servers attached
// (and false when omitted or downgraded via fallback).
func WithSessionMCPAttachReport(ctx context.Context, attached *bool) context.Context {
	if attached == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionMCPAttachReportContextKey{}, attached)
}

// ReportSessionMCPAttached records whether Orbit MCP servers were actually attached to the opened session.
func ReportSessionMCPAttached(ctx context.Context, attached bool) {
	if ctx == nil {
		return
	}
	if ptr, ok := ctx.Value(sessionMCPAttachReportContextKey{}).(*bool); ok && ptr != nil {
		*ptr = attached
	}
}

// isMCPUnsupportedError returns true ONLY when the upstream ACP agent explicitly rejected mcpServers
// due to unsupported capability or invalid params — NEVER on timeouts, cancellations, EOF, transport errors,
// or runtime MCP subprocess startup failures (e.g. failed to start / spawn / exit status / permission denied).
func isMCPUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, nonCapabilitySignal := range []string{
		"context canceled",
		"context deadline exceeded",
		"timed out",
		"timeout",
		"broken pipe",
		"connection reset",
		"connection closed",
		"closed pipe",
		"peer connection closed",
		"eof",
		"failed to start",
		"failed to launch",
		"failed to connect",
		"failed to spawn",
		"failed to initialize",
		"spawn",
		"exit status",
		"exited",
		"no such file",
		"executable",
		"permission denied",
		"handshake",
		"econnrefused",
		"enoent",
	} {
		if strings.Contains(msg, nonCapabilitySignal) {
			return false
		}
	}
	hasParamReject := false
	for _, rejectSignal := range []string{
		"invalid params",
		"-32602",
		"unknown field",
		"unrecognized",
		"not supported",
		"unsupported",
	} {
		if strings.Contains(msg, rejectSignal) {
			hasParamReject = true
			break
		}
	}
	if !hasParamReject {
		return false
	}
	if strings.Contains(msg, "mcpserver") ||
		strings.Contains(msg, "mcp_server") ||
		strings.Contains(msg, "mcp server") {
		return true
	}
	for _, tok := range strings.FieldsFunc(msg, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		if tok == "mcp" {
			return true
		}
	}
	return false
}

// WithPermissionWriteMeta attaches PermissionWriteMeta to a permission handler context.
func WithPermissionWriteMeta(ctx context.Context, meta PermissionWriteMeta) context.Context {
	return context.WithValue(ctx, permWriteMetaContextKey{}, meta)
}

// PermissionWriteMetaFromContext extracts PermissionWriteMeta if provided by the ACP driver.
func PermissionWriteMetaFromContext(ctx context.Context) (PermissionWriteMeta, bool) {
	if ctx == nil {
		return PermissionWriteMeta{}, false
	}
	meta, ok := ctx.Value(permWriteMetaContextKey{}).(PermissionWriteMeta)
	return meta, ok
}

type permRPCEntry struct {
	rpcID       string
	writeDoneCh chan error
}

type rpcPermTracker struct {
	mu          sync.Mutex
	pendingKeys map[string][]permRPCEntry
	waitersByID map[string]chan error
	closed      bool
}

func newRPCPermTracker() *rpcPermTracker {
	return &rpcPermTracker{
		pendingKeys: map[string][]permRPCEntry{},
		waitersByID: map[string]chan error{},
	}
}

func (t *rpcPermTracker) observeInboundLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ToolCall  struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"toolCall"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.Method != "session/request_permission" {
		return
	}
	rpcID := strings.TrimSpace(string(msg.ID))
	if rpcID == "" || rpcID == "null" {
		return
	}
	key := msg.Params.SessionID + "|" + msg.Params.ToolCall.ToolCallID
	ch := make(chan error, 1)

	t.mu.Lock()
	if !t.closed {
		t.pendingKeys[key] = append(t.pendingKeys[key], permRPCEntry{rpcID: rpcID, writeDoneCh: ch})
		t.waitersByID[rpcID] = ch
	} else {
		ch <- io.ErrClosedPipe
		close(ch)
	}
	t.mu.Unlock()
}

func (t *rpcPermTracker) claim(sessionID, toolCallID string) (PermissionWriteMeta, bool) {
	key := sessionID + "|" + toolCallID
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.pendingKeys[key]
	if len(list) == 0 {
		return PermissionWriteMeta{}, false
	}
	entry := list[0]
	if len(list) == 1 {
		delete(t.pendingKeys, key)
	} else {
		t.pendingKeys[key] = list[1:]
	}
	return PermissionWriteMeta{RPCID: entry.rpcID, WriteDoneCh: entry.writeDoneCh}, true
}

func (t *rpcPermTracker) observeOutboundLine(line []byte, writeErr error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.Method != "" {
		return
	}
	rpcID := strings.TrimSpace(string(msg.ID))
	if rpcID == "" || rpcID == "null" {
		return
	}
	t.mu.Lock()
	ch, ok := t.waitersByID[rpcID]
	if ok {
		delete(t.waitersByID, rpcID)
	}
	t.mu.Unlock()
	if ok {
		ch <- writeErr
		close(ch)
	}
}

func (t *rpcPermTracker) closeAll(err error) {
	if err == nil {
		err = io.ErrClosedPipe
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	waiters := t.waitersByID
	t.waitersByID = map[string]chan error{}
	t.pendingKeys = map[string][]permRPCEntry{}
	t.mu.Unlock()
	for _, ch := range waiters {
		ch <- err
		close(ch)
	}
}

type observedReadCloser struct {
	underlying io.ReadCloser
	tracker    *rpcPermTracker
	buf        []byte
}

func (r *observedReadCloser) Read(p []byte) (int, error) {
	n, err := r.underlying.Read(p)
	if n > 0 {
		r.buf = append(r.buf, p[:n]...)
		for {
			idx := bytes.IndexByte(r.buf, '\n')
			if idx < 0 {
				break
			}
			line := r.buf[:idx]
			r.tracker.observeInboundLine(line)
			r.buf = r.buf[idx+1:]
		}
	}
	return n, err
}

func (r *observedReadCloser) Close() error {
	return r.underlying.Close()
}

type observedWriteCloser struct {
	mu         sync.Mutex
	underlying io.WriteCloser
	tracker    *rpcPermTracker
	buf        []byte
}

func (w *observedWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Execute the real underlying stdio Write FIRST so observers fire strictly AFTER bytes are written.
	n, err := w.underlying.Write(p)
	chunk := p[:n]
	if err != nil {
		chunk = p
	}
	if len(chunk) > 0 {
		w.buf = append(w.buf, chunk...)
		for {
			idx := bytes.IndexByte(w.buf, '\n')
			if idx < 0 {
				break
			}
			line := w.buf[:idx]
			w.tracker.observeOutboundLine(line, err)
			w.buf = w.buf[idx+1:]
		}
	}
	return n, err
}

func (w *observedWriteCloser) Close() error {
	w.tracker.closeAll(io.ErrClosedPipe)
	return w.underlying.Close()
}

// SessionUpdateHandler receives persistent session/update notifications across prompt turns & swarm wakes.
type SessionUpdateHandler func(ctx context.Context, note acp.SessionNotification) error

// PermissionHandler handles ACP session/request_permission RPCs from the agent.
type PermissionHandler func(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)

// AcpAdapterDriver abstracts an upstream standard ACP process (agy, claude-agent-acp, codex-acp, or FakeAcpDriver).
type AcpAdapterDriver interface {
	Start(ctx context.Context, onUpdate SessionUpdateHandler, onPerm PermissionHandler) error
	Close(ctx context.Context) error
	SupportsResume() bool
	SupportsLoad() bool
	SupportsMetaSystemPrompt() bool
	IsSessionLiveInMemory(sid acp.SessionId) bool
	NewSession(ctx context.Context, cwd, systemPromptAppend string) (acp.NewSessionResponse, error)
	ResumeSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.ResumeSessionResponse, error)
	LoadSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.LoadSessionResponse, error)
	CloseSession(ctx context.Context, sid acp.SessionId) error
	SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) (acp.SetSessionConfigOptionResponse, error)
	SetSessionMode(ctx context.Context, sid acp.SessionId, modeID string) (acp.SetSessionModeResponse, error)
	Prompt(ctx context.Context, sid acp.SessionId, promptText string) (acp.PromptResponse, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
}

// AgyDriverConfig configures launching an upstream standard ACP process
// (`agy acp --stdio --process-per-session`, `claude-agent-acp`, or `codex-acp`).
type AgyDriverConfig struct {
	Adapter       protocol.Adapter
	Bin           string
	Args          []string
	RunnerSock    string
	OrbitBinDir   string
	RunnerVersion string
	ExtraEnv      []string
}

// AgyAcpDriver implements AcpAdapterDriver over `github.com/coder/acp-go-sdk` v0.20.0.
type AgyAcpDriver struct {
	cfg AgyDriverConfig

	mu             sync.Mutex
	cmd            *exec.Cmd
	conn           *acp.ClientSideConnection
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	customStdin    io.WriteCloser
	customStdout   io.ReadCloser
	permTracker    *rpcPermTracker
	stderrBuf      bytes.Buffer
	started        bool
	supportsResume bool
	supportsLoad   bool
	supportsClose  bool
	supportsMetaSP bool

	liveSessions    map[acp.SessionId]bool
	loadingSessions map[acp.SessionId]bool
	onUpdate        SessionUpdateHandler
	onPerm          PermissionHandler
}

// NewAdapterDriver constructs an ACP driver configured with sensible defaults for agy, claude, or codex.
func NewAdapterDriver(adapter protocol.Adapter, cfg AgyDriverConfig) *AgyAcpDriver {
	cfg.Adapter = adapter
	switch adapter {
	case protocol.AdapterClaude:
		if strings.TrimSpace(cfg.Bin) == "" {
			cfg.Bin = "claude-agent-acp"
		}
	case protocol.AdapterCodex:
		if strings.TrimSpace(cfg.Bin) == "" {
			cfg.Bin = "codex-acp"
		}
	default:
		cfg.Adapter = protocol.AdapterAgy
		if strings.TrimSpace(cfg.Bin) == "" {
			cfg.Bin = "agy"
		}
		if len(cfg.Args) == 0 {
			cfg.Args = []string{"acp", "--stdio", "--process-per-session"}
		}
	}
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = "0.2.0"
	}
	return &AgyAcpDriver{
		cfg:             cfg,
		liveSessions:    map[acp.SessionId]bool{},
		loadingSessions: map[acp.SessionId]bool{},
	}
}

func NewAgyAcpDriver(cfg AgyDriverConfig) *AgyAcpDriver {
	if cfg.Adapter != "" {
		return NewAdapterDriver(cfg.Adapter, cfg)
	}
	return NewAdapterDriver(protocol.AdapterAgy, cfg)
}

// NewStreamAcpDriver constructs an AgyAcpDriver bound to pre-existing peerInput/peerOutput streams
// (e.g. io.Pipe() to an in-process acp.NewAgentSideConnection for real wire-protocol tests).
func NewStreamAcpDriver(cfg AgyDriverConfig, peerInput io.WriteCloser, peerOutput io.ReadCloser) *AgyAcpDriver {
	d := NewAgyAcpDriver(cfg)
	d.customStdin = peerInput
	d.customStdout = peerOutput
	return d
}

func (d *AgyAcpDriver) Start(ctx context.Context, onUpdate SessionUpdateHandler, onPerm PermissionHandler) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	d.onUpdate = onUpdate
	d.onPerm = onPerm

	var (
		cmd       *exec.Cmd
		rawStdin  io.WriteCloser
		rawStdout io.ReadCloser
		err       error
	)

	if d.customStdin != nil && d.customStdout != nil {
		rawStdin = d.customStdin
		rawStdout = d.customStdout
	} else {
		cmd = exec.Command(d.cfg.Bin, d.cfg.Args...)
		setProcessGroup(cmd)
		cmd.Env = d.buildEnv()
		cmd.Stderr = &d.stderrBuf

		rawStdin, err = cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("agy stdin pipe: %w", err)
		}
		rawStdout, err = cmd.StdoutPipe()
		if err != nil {
			_ = rawStdin.Close()
			return fmt.Errorf("agy stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			_ = rawStdin.Close()
			_ = rawStdout.Close()
			return fmt.Errorf("start %s: %w", d.cfg.Bin, err)
		}
	}

	tracker := newRPCPermTracker()
	stdin := &observedWriteCloser{underlying: rawStdin, tracker: tracker}
	stdout := &observedReadCloser{underlying: rawStdout, tracker: tracker}

	d.permTracker = tracker
	bridge := &acpClientBridge{driver: d}
	conn := acp.NewClientSideConnection(bridge, stdin, stdout)

	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	initResp, err := conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{
				ReadTextFile:  false,
				WriteTextFile: false,
			},
			Terminal: false,
		},
		ClientInfo: &acp.Implementation{
			Name:    "hebi8-orbit-runner",
			Version: d.cfg.RunnerVersion,
		},
	})
	if err != nil {
		_ = stdin.Close()
		if cmd != nil {
			killProcessGroup(cmd)
		}
		return fmt.Errorf("acp initialize: %w (stderr=%s)", err, strings.TrimSpace(d.stderrBuf.String()))
	}

	d.cmd = cmd
	d.conn = conn
	d.stdin = stdin
	d.stdout = stdout
	d.started = true
	d.supportsResume = initResp.AgentCapabilities.SessionCapabilities.Resume != nil
	d.supportsLoad = initResp.AgentCapabilities.LoadSession
	d.supportsClose = initResp.AgentCapabilities.SessionCapabilities.Close != nil
	d.supportsMetaSP = supportsAntigravityMetaSystemPrompt(d.cfg.Adapter, initResp.AgentCapabilities.Meta)
	return nil
}

func supportsAntigravityMetaSystemPrompt(adapter protocol.Adapter, meta any) bool {
	if metaMap, ok := meta.(map[string]any); ok {
		if ag, ok := metaMap["antigravity"].(map[string]any); ok {
			if sp, ok := ag["systemPrompt"].(map[string]any); ok {
				if appendOK, ok := sp["append"].(bool); ok {
					return appendOK
				}
			}
		}
		// Explicitly non-antigravity ACP servers (e.g. claude-agent-acp, codex-acp) don't advertise antigravity meta
		return false
	}
	// Fallback for minimal mock agents in tests that omit _meta on agy
	return adapter == "" || adapter == protocol.AdapterAgy
}

func (d *AgyAcpDriver) buildEnv() []string {
	env := os.Environ()
	if d.cfg.RunnerSock != "" {
		env = setEnvVar(env, "ORBIT_RUNNER_SOCK", d.cfg.RunnerSock)
	}
	if d.cfg.OrbitBinDir != "" {
		curPath := os.Getenv("PATH")
		if curPath == "" {
			env = setEnvVar(env, "PATH", d.cfg.OrbitBinDir)
		} else {
			env = setEnvVar(env, "PATH", d.cfg.OrbitBinDir+string(os.PathListSeparator)+curPath)
		}
	}
	for _, kv := range d.cfg.ExtraEnv {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			env = setEnvVar(env, kv[:idx], kv[idx+1:])
		}
	}
	return env
}

func (d *AgyAcpDriver) Close(ctx context.Context) error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return nil
	}
	live := make([]acp.SessionId, 0, len(d.liveSessions))
	for sid := range d.liveSessions {
		live = append(live, sid)
	}
	conn := d.conn
	cmd := d.cmd
	stdin := d.stdin
	d.started = false
	d.liveSessions = map[acp.SessionId]bool{}
	d.mu.Unlock()

	// Best-effort close all live worker sessions before shutting down frontend process
	if conn != nil && d.supportsClose {
		for _, sid := range live {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, _ = conn.CloseSession(cctx, acp.CloseSessionRequest{SessionId: sid})
			cancel()
		}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			killProcessGroup(cmd)
			<-done
		}
	}
	return nil
}

func (d *AgyAcpDriver) SupportsResume() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.supportsResume
}

func (d *AgyAcpDriver) SupportsLoad() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.supportsLoad
}

func (d *AgyAcpDriver) SupportsMetaSystemPrompt() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.supportsMetaSP
}

func (d *AgyAcpDriver) IsSessionLiveInMemory(sid acp.SessionId) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveSessions[sid]
}

func (d *AgyAcpDriver) getConn() (*acp.ClientSideConnection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.conn == nil {
		return nil, errors.New("acp driver not started")
	}
	return d.conn, nil
}

func (d *AgyAcpDriver) NewSession(ctx context.Context, cwd, systemPromptAppend string) (acp.NewSessionResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	mcpServers := SessionMCPServersFromContext(ctx)
	if mcpServers == nil {
		mcpServers = []acp.McpServer{}
	}
	req := acp.NewSessionRequest{
		Cwd:        absCWD,
		McpServers: mcpServers,
	}
	if strings.TrimSpace(systemPromptAppend) != "" {
		meta, err := acp.NewMeta(map[string]any{
			"antigravity": map[string]any{
				"systemPrompt": map[string]any{
					"append": systemPromptAppend,
				},
			},
		})
		if err != nil {
			return acp.NewSessionResponse{}, err
		}
		req.Meta = meta
	}

	resp, err := conn.NewSession(ctx, req)
	mcpAttached := err == nil && len(mcpServers) > 0
	if err != nil && len(mcpServers) > 0 && isMCPUnsupportedError(err) {
		log.Printf("runner: NewSession (%s) rejected Orbit MCP server (%v); retrying without MCP servers", d.cfg.Adapter, err)
		req.McpServers = []acp.McpServer{}
		resp, err = conn.NewSession(ctx, req)
		mcpAttached = false
	}
	if err != nil {
		ReportSessionMCPAttached(ctx, false)
		return resp, err
	}
	ReportSessionMCPAttached(ctx, mcpAttached)
	d.mu.Lock()
	d.liveSessions[resp.SessionId] = true
	d.mu.Unlock()
	return resp, nil
}

func (d *AgyAcpDriver) ResumeSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.ResumeSessionResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	mcpServers := SessionMCPServersFromContext(ctx)
	if mcpServers == nil {
		mcpServers = []acp.McpServer{}
	}
	req := acp.ResumeSessionRequest{
		SessionId:  sid,
		Cwd:        absCWD,
		McpServers: mcpServers,
	}
	resp, err := conn.ResumeSession(ctx, req)
	mcpAttached := err == nil && len(mcpServers) > 0
	if err != nil && len(mcpServers) > 0 && isMCPUnsupportedError(err) {
		log.Printf("runner: ResumeSession (%s) rejected Orbit MCP server (%v); retrying without MCP servers", d.cfg.Adapter, err)
		req.McpServers = []acp.McpServer{}
		resp, err = conn.ResumeSession(ctx, req)
		mcpAttached = false
	}
	if err != nil {
		ReportSessionMCPAttached(ctx, false)
		return resp, err
	}
	ReportSessionMCPAttached(ctx, mcpAttached)
	d.mu.Lock()
	d.liveSessions[sid] = true
	d.mu.Unlock()
	return resp, nil
}

func (d *AgyAcpDriver) LoadSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.LoadSessionResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	// Mark session as loading so historical replay frames from session/load are swallowed
	d.mu.Lock()
	d.loadingSessions[sid] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.loadingSessions, sid)
		d.mu.Unlock()
	}()

	mcpServers := SessionMCPServersFromContext(ctx)
	if mcpServers == nil {
		mcpServers = []acp.McpServer{}
	}
	req := acp.LoadSessionRequest{
		SessionId:  sid,
		Cwd:        absCWD,
		McpServers: mcpServers,
	}
	resp, err := conn.LoadSession(ctx, req)
	mcpAttached := err == nil && len(mcpServers) > 0
	if err != nil && len(mcpServers) > 0 && isMCPUnsupportedError(err) {
		log.Printf("runner: LoadSession (%s) rejected Orbit MCP server (%v); retrying without MCP servers", d.cfg.Adapter, err)
		req.McpServers = []acp.McpServer{}
		resp, err = conn.LoadSession(ctx, req)
		mcpAttached = false
	}
	if err != nil {
		ReportSessionMCPAttached(ctx, false)
		return resp, err
	}
	ReportSessionMCPAttached(ctx, mcpAttached)
	d.mu.Lock()
	d.liveSessions[sid] = true
	d.mu.Unlock()
	return resp, nil
}

func (d *AgyAcpDriver) CloseSession(ctx context.Context, sid acp.SessionId) error {
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: sid})
	d.mu.Lock()
	delete(d.liveSessions, sid)
	d.mu.Unlock()
	return err
}

func (d *AgyAcpDriver) SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) (acp.SetSessionConfigOptionResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	return conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sid,
			ConfigId:  acp.SessionConfigId(configID),
			Value:     acp.SessionConfigValueId(value),
		},
	})
}

func (d *AgyAcpDriver) SetSessionMode(ctx context.Context, sid acp.SessionId, modeID string) (acp.SetSessionModeResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	return conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: sid,
		ModeId:    acp.SessionModeId(modeID),
	})
}

func (d *AgyAcpDriver) Prompt(ctx context.Context, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
	conn, err := d.getConn()
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock(promptText)},
	})
}

func (d *AgyAcpDriver) Cancel(ctx context.Context, sid acp.SessionId) error {
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	return conn.Cancel(ctx, acp.CancelNotification{SessionId: sid})
}

// acpClientBridge implements acp.Client for AgyAcpDriver.
type acpClientBridge struct {
	driver *AgyAcpDriver
}

func (b *acpClientBridge) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	b.driver.mu.Lock()
	loading := b.driver.loadingSessions[params.SessionId]
	handler := b.driver.onUpdate
	b.driver.mu.Unlock()
	if loading || handler == nil {
		return nil
	}
	return handler(ctx, params)
}

func (b *acpClientBridge) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	b.driver.mu.Lock()
	handler := b.driver.onPerm
	tracker := b.driver.permTracker
	b.driver.mu.Unlock()
	if handler == nil {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	if tracker != nil {
		if meta, ok := tracker.claim(string(params.SessionId), string(params.ToolCall.ToolCallId)); ok {
			ctx = WithPermissionWriteMeta(ctx, meta)
		}
	}
	return handler(ctx, params)
}

func (b *acpClientBridge) CompleteElicitation(ctx context.Context, params acp.CompleteElicitationNotification) error {
	return nil
}

func (b *acpClientBridge) CreateElicitation(ctx context.Context, params acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, errors.New("elicitation not supported")
}

func (b *acpClientBridge) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("client fs.readTextFile not enabled")
}

func (b *acpClientBridge) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("client fs.writeTextFile not enabled")
}

func (b *acpClientBridge) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("client terminal not enabled")
}

func (b *acpClientBridge) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("client terminal not enabled")
}

func (b *acpClientBridge) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("client terminal not enabled")
}

func (b *acpClientBridge) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("client terminal not enabled")
}

func (b *acpClientBridge) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("client terminal not enabled")
}

func setEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, prefix+val)
			replaced = true
		} else {
			out = append(out, kv)
		}
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}
