package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

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
	Prompt(ctx context.Context, sid acp.SessionId, promptText string) (acp.PromptResponse, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
}

// AgyDriverConfig configures launching `agy acp --stdio --process-per-session`.
type AgyDriverConfig struct {
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

func NewAgyAcpDriver(cfg AgyDriverConfig) *AgyAcpDriver {
	if strings.TrimSpace(cfg.Bin) == "" {
		cfg.Bin = "agy"
	}
	if len(cfg.Args) == 0 {
		cfg.Args = []string{"acp", "--stdio", "--process-per-session"}
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

func (d *AgyAcpDriver) Start(ctx context.Context, onUpdate SessionUpdateHandler, onPerm PermissionHandler) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	d.onUpdate = onUpdate
	d.onPerm = onPerm

	cmd := exec.Command(d.cfg.Bin, d.cfg.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = d.buildEnv()
	cmd.Stderr = &d.stderrBuf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("agy stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("agy stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("start %s: %w", d.cfg.Bin, err)
	}

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
		killProcessGroup(cmd)
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
	d.supportsMetaSP = true // Verified on agy 1.5.14+ via _meta.antigravity.systemPrompt.append
	return nil
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
	env = append(env, d.cfg.ExtraEnv...)
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
	req := acp.NewSessionRequest{
		Cwd:        absCWD,
		McpServers: []acp.McpServer{}, // Empty list preserves 20k tool budget
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
	if err != nil {
		return resp, err
	}
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
	resp, err := conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  sid,
		Cwd:        absCWD,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return resp, err
	}
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

	resp, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  sid,
		Cwd:        absCWD,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return resp, err
	}
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
	b.driver.mu.Unlock()
	if handler == nil {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
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

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(150 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
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
