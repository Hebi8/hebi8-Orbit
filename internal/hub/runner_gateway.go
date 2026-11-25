package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type outboundFrame struct {
	data    []byte
	onStart func() error
	onFail  func(writeErr error)
	errCh   chan error
}

type runnerConn struct {
	machineID        string
	runnerInstanceID string
	metaMu            sync.RWMutex
	adapters          []protocol.Adapter
	hasExplicitReport bool
	capabilities      []string
	rpcMu            sync.Mutex
	pendingRPC       map[string]chan protocol.RunnerRPCResultFrame
	conn             *websocket.Conn
	sendCh           chan outboundFrame
	done             chan struct{}
	closeOnce        sync.Once
}

func newRunnerConn(machineID, runnerInstanceID string, adapters []protocol.Adapter, capabilities []string, conn *websocket.Conn) *runnerConn {
	return &runnerConn{
		machineID:         machineID,
		runnerInstanceID:  runnerInstanceID,
		adapters:          append([]protocol.Adapter(nil), adapters...),
		hasExplicitReport: slices.Contains(capabilities, protocol.RunnerCapAgentsV1),
		capabilities:      append([]string(nil), capabilities...),
		pendingRPC:        map[string]chan protocol.RunnerRPCResultFrame{},
		conn:              conn,
		sendCh:            make(chan outboundFrame, 256),
		done:              make(chan struct{}),
	}
}

func (rc *runnerConn) setAdapters(adapters []protocol.Adapter) {
	rc.metaMu.Lock()
	rc.adapters = append([]protocol.Adapter(nil), adapters...)
	rc.hasExplicitReport = true
	rc.metaMu.Unlock()
}

func (rc *runnerConn) hasExplicitAdapterReport() bool {
	rc.metaMu.RLock()
	defer rc.metaMu.RUnlock()
	return rc.hasExplicitReport
}

func (rc *runnerConn) getAdapters() []protocol.Adapter {
	rc.metaMu.RLock()
	defer rc.metaMu.RUnlock()
	if len(rc.adapters) == 0 {
		return nil
	}
	return append([]protocol.Adapter(nil), rc.adapters...)
}

func (rc *runnerConn) getCapabilities() []string {
	rc.metaMu.RLock()
	defer rc.metaMu.RUnlock()
	if len(rc.capabilities) == 0 {
		return nil
	}
	return append([]string(nil), rc.capabilities...)
}

func (rc *runnerConn) hasCapability(cap string) bool {
	rc.metaMu.RLock()
	defer rc.metaMu.RUnlock()
	return slices.Contains(rc.capabilities, cap)
}

func (rc *runnerConn) resolveRPC(res protocol.RunnerRPCResultFrame) {
	rc.rpcMu.Lock()
	ch, ok := rc.pendingRPC[res.RequestID]
	if ok {
		delete(rc.pendingRPC, res.RequestID)
	}
	rc.rpcMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
		close(ch)
	}
}

func (rc *runnerConn) sendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case <-rc.done:
		return errors.New("runner connection closed")
	case rc.sendCh <- outboundFrame{data: data}:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("runner send queue full")
	}
}

func (rc *runnerConn) sendJSONSync(v any, onStart func() error, onFail func(error)) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	frame := outboundFrame{
		data:    data,
		onStart: onStart,
		onFail:  onFail,
		errCh:   errCh,
	}
	select {
	case <-rc.done:
		return errors.New("runner connection closed")
	case rc.sendCh <- frame:
	case <-time.After(5 * time.Second):
		return errors.New("runner send queue full")
	}
	select {
	case err := <-errCh:
		return err
	case <-rc.done:
		return errors.New("runner connection closed before frame write")
	case <-time.After(12 * time.Second):
		return errors.New("runner frame write timed out")
	}
}

func (rc *runnerConn) close(code websocket.StatusCode, reason string) {
	rc.closeOnce.Do(func() {
		close(rc.done)
		rc.rpcMu.Lock()
		for id, ch := range rc.pendingRPC {
			select {
			case ch <- protocol.RunnerRPCResultFrame{RequestID: id, OK: false, Error: "runner disconnected"}:
			default:
			}
			close(ch)
		}
		rc.pendingRPC = map[string]chan protocol.RunnerRPCResultFrame{}
		rc.rpcMu.Unlock()
		_ = rc.conn.Close(code, reason)
	})
}

// RunnerGateway tracks live WebSocket sessions for local_ws and remote_ws runners.
type RunnerGateway struct {
	mu    sync.RWMutex
	conns map[string]*runnerConn
}

func NewRunnerGateway() *RunnerGateway {
	return &RunnerGateway{
		conns: map[string]*runnerConn{},
	}
}

func (g *RunnerGateway) register(rc *runnerConn) *runnerConn {
	g.mu.Lock()
	prev := g.conns[rc.machineID]
	g.conns[rc.machineID] = rc
	g.mu.Unlock()
	return prev
}

func (g *RunnerGateway) unregister(rc *runnerConn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.conns[rc.machineID]; ok && cur == rc {
		delete(g.conns, rc.machineID)
		return true
	}
	return false
}

func (g *RunnerGateway) get(machineID string) *runnerConn {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.conns[machineID]
}

// IsConnected reports whether a runner for machineID currently has an active WS link.
func (g *RunnerGateway) IsConnected(machineID string) bool {
	return g.get(machineID) != nil
}

// ActiveRunnerInstanceID returns the live runnerInstanceId for machineID if connected.
func (g *RunnerGateway) ActiveRunnerInstanceID(machineID string) string {
	rc := g.get(machineID)
	if rc == nil {
		return ""
	}
	return rc.runnerInstanceID
}

// ReportedAdapters returns the live adapters reported by the connected runner for machineID.
func (g *RunnerGateway) ReportedAdapters(machineID string) []protocol.Adapter {
	rc := g.get(machineID)
	if rc == nil {
		return nil
	}
	return rc.getAdapters()
}

// HasExplicitAdapterReport reports whether the connected runner explicitly reported its agent readiness
// (so an empty ReportedAdapters slice means "0 agents ready", not "legacy runner didn't report").
func (g *RunnerGateway) HasExplicitAdapterReport(machineID string) bool {
	rc := g.get(machineID)
	return rc != nil && rc.hasExplicitAdapterReport()
}

// Capabilities returns the negotiated control-plane capabilities advertised by machineID.
func (g *RunnerGateway) Capabilities(machineID string) []string {
	rc := g.get(machineID)
	if rc == nil {
		return nil
	}
	return rc.getCapabilities()
}

// HasCapability reports whether machineID is connected and advertises capName.
func (g *RunnerGateway) HasCapability(machineID, capName string) bool {
	rc := g.get(machineID)
	return rc != nil && rc.hasCapability(capName)
}

// DisconnectMachine closes any live WebSocket link for machineID immediately (e.g. after removal/revocation).
func (g *RunnerGateway) DisconnectMachine(machineID, reason string) {
	if rc := g.get(machineID); rc != nil {
		rc.close(websocket.StatusPolicyViolation, reason)
	}
}

// SendConfig pushes a runner.config hot-update frame to machineID.
func (g *RunnerGateway) SendConfig(machineID string, frame protocol.RunnerConfigFrame) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgConfig
	return rc.sendJSON(frame)
}

// CallRPC invokes a control-plane operation on machineID and waits for the correlated runner.rpc_result.
func (g *RunnerGateway) CallRPC(
	ctx context.Context,
	machineID, op string,
	args map[string]any,
	timeout time.Duration,
) (protocol.RunnerRPCResultFrame, error) {
	rc := g.get(machineID)
	if rc == nil {
		return protocol.RunnerRPCResultFrame{}, fmt.Errorf("runner %s not connected", machineID)
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	reqID := "rpc_" + ulid.Make().String()
	ch := make(chan protocol.RunnerRPCResultFrame, 1)
	rc.rpcMu.Lock()
	select {
	case <-rc.done:
		rc.rpcMu.Unlock()
		return protocol.RunnerRPCResultFrame{}, fmt.Errorf("runner %s closed", machineID)
	default:
		rc.pendingRPC[reqID] = ch
	}
	rc.rpcMu.Unlock()

	cleanup := func() {
		rc.rpcMu.Lock()
		delete(rc.pendingRPC, reqID)
		rc.rpcMu.Unlock()
	}

	if err := rc.sendJSON(protocol.RunnerRPCFrame{
		Type:      protocol.RunnerMsgRPC,
		RequestID: reqID,
		Op:        op,
		Args:      args,
	}); err != nil {
		cleanup()
		return protocol.RunnerRPCResultFrame{}, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res, ok := <-ch:
		if !ok {
			return protocol.RunnerRPCResultFrame{}, fmt.Errorf("runner %s disconnected during %s", machineID, op)
		}
		return res, nil
	case <-ctx.Done():
		cleanup()
		return protocol.RunnerRPCResultFrame{}, ctx.Err()
	case <-timer.C:
		cleanup()
		return protocol.RunnerRPCResultFrame{}, fmt.Errorf("runner rpc %s timed out after %s", op, timeout)
	}
}

// DispatchAttempt sends an attempt.dispatch frame to machineID if connected.
func (g *RunnerGateway) DispatchAttempt(machineID string, frame protocol.AttemptDispatchFrame) (string, error) {
	return g.DispatchAttemptWithHook(machineID, frame, nil)
}

// DispatchAttemptWithHook sends an attempt.dispatch frame synchronously via the runner's serialized
// write pump, running onStart right before conn.Write so dispatch binding and subsequent cancel frames
// are strictly ordered on the wire (B10).
func (g *RunnerGateway) DispatchAttemptWithHook(machineID string, frame protocol.AttemptDispatchFrame, onStart func(runnerInstanceID string) error) (string, error) {
	rc := g.get(machineID)
	if rc == nil {
		return "", fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgAttemptDispatch
	var startHook func() error
	if onStart != nil {
		instID := rc.runnerInstanceID
		startHook = func() error {
			return onStart(instID)
		}
	}
	if err := rc.sendJSONSync(frame, startHook, nil); err != nil {
		return "", err
	}
	return rc.runnerInstanceID, nil
}

// CancelAttempt sends an attempt.cancel frame to machineID if connected.
func (g *RunnerGateway) CancelAttempt(machineID string, frame protocol.AttemptCancelFrame) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgAttemptCancel
	return rc.sendJSON(frame)
}

// SendApprovalDecision pushes an approval.decision frame synchronously to machineID if connected.
// Optional onStart/onFail callbacks align delivery_attempted accounting with the real WS write boundary (B6).
func (g *RunnerGateway) SendApprovalDecision(machineID string, frame protocol.ApprovalDecisionFrame) error {
	return g.SendApprovalDecisionWithHooks(machineID, frame, nil, nil)
}

// SendApprovalDecisionWithHooks pushes an approval.decision frame and invokes onStart right before
// writing to the live WS socket and onFail if the write directly errors.
func (g *RunnerGateway) SendApprovalDecisionWithHooks(
	machineID string,
	frame protocol.ApprovalDecisionFrame,
	onStart func() error,
	onFail func(error),
) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgApprovalDecide
	return rc.sendJSONSync(frame, onStart, onFail)
}

// SendBoardMessage pushes a board.message frame to machineID for an active swarm_monitoring attempt (B16).
func (g *RunnerGateway) SendBoardMessage(machineID string, frame protocol.BoardMessageDispatchFrame) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgBoardMessage
	return rc.sendJSONSync(frame, nil, nil)
}

// CloseSession sends a session.close frame to machineID if connected.
func (g *RunnerGateway) CloseSession(machineID string, frame protocol.SessionCloseFrame) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgSessionClose
	return rc.sendJSON(frame)
}

// CloseAll terminates all active runner WebSocket connections cleanly on Hub shutdown.
func (g *RunnerGateway) CloseAll() {
	g.mu.Lock()
	list := make([]*runnerConn, 0, len(g.conns))
	for _, rc := range g.conns {
		list = append(list, rc)
	}
	g.conns = map[string]*runnerConn{}
	g.mu.Unlock()
	for _, rc := range list {
		rc.close(websocket.StatusGoingAway, "hub shutting down")
	}
}

func (s *Server) handleRunnerWS(w http.ResponseWriter, r *http.Request) {
	// B5: Unconditionally reject browser handshakes carrying an Origin header on /ws/runner.
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		writeAPIError(w, http.StatusForbidden, "ERR_ORIGIN_FORBIDDEN", "browser Origin header is forbidden on /ws/runner")
		return
	}

	// B5: Require Bearer token on ALL runner connections (both local_ws and remote_ws) before upgrade.
	if extractBearerToken(r.Header.Get("Authorization")) == "" {
		writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "runner bearer token required")
		return
	}

	// Pre-upgrade auth check when machineId is declared in header or query param
	claimedMachine := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	if claimedMachine == "" {
		claimedMachine = strings.TrimSpace(r.URL.Query().Get("machineId"))
	}
	if claimedMachine != "" {
		if _, statusCode, errMsg := s.verifyRunnerAuth(r, claimedMachine); statusCode != http.StatusOK {
			code := "ERR_UNAUTHORIZED"
			if statusCode == http.StatusForbidden {
				code = "ERR_FORBIDDEN"
			}
			writeAPIError(w, statusCode, code, errMsg)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 20) // 16 MiB max frame for large outbox batches
	defer conn.CloseNow()

	// 1. Read runner.hello within 10 seconds
	helloCtx, cancelHello := context.WithTimeout(r.Context(), 10*time.Second)
	_, rawHello, err := conn.Read(helloCtx)
	cancelHello()
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected runner.hello")
		return
	}

	var hello protocol.RunnerHelloFrame
	if err := json.Unmarshal(rawHello, &hello); err != nil || hello.Type != protocol.RunnerMsgHello {
		_ = conn.Close(websocket.StatusPolicyViolation, "first frame must be runner.hello")
		return
	}
	if hello.ProtocolVersion != 1 || strings.TrimSpace(hello.MachineID) == "" || strings.TrimSpace(hello.RunnerInstanceID) == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid runner.hello fields")
		return
	}
	if claimedMachine != "" && hello.MachineID != claimedMachine {
		_ = conn.Close(websocket.StatusPolicyViolation, "runner.hello machineId mismatch")
		return
	}

	// Re-verify machine auth binding against actual hello.MachineID
	if _, statusCode, errMsg := s.verifyRunnerAuth(r, hello.MachineID); statusCode != http.StatusOK {
		_ = conn.Close(websocket.StatusPolicyViolation, errMsg)
		return
	}

	// 2. Reconcile runner liveness, outbox watermarks, and approval states in a single BEGIN IMMEDIATE tx
	recRes, err := s.store.ReconcileRunnerHello(r.Context(), hello, s.leaseTTL.Milliseconds())
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "reconcile failed")
		return
	}
	if len(recRes.Events) > 0 {
		s.broker.Broadcast(recRes.Events...)
	}

	initialAdapters := hello.Adapters
	if len(hello.AgentProbes) > 0 {
		ready := make([]protocol.Adapter, 0, len(hello.AgentProbes))
		for _, p := range hello.AgentProbes {
			if p.State == protocol.AgentStateReady && p.Enabled {
				ready = append(ready, p.Adapter)
			}
		}
		initialAdapters = ready
	}
	rc := newRunnerConn(hello.MachineID, hello.RunnerInstanceID, initialAdapters, hello.Capabilities, conn)
	if prev := s.runners.register(rc); prev != nil && prev != rc {
		prev.close(websocket.StatusGoingAway, "replaced by newer runner connection")
	}

	defer func() {
		rc.close(websocket.StatusNormalClosure, "closing")
		if s.runners.unregister(rc) {
			offCtx, cancelOff := context.WithTimeout(context.Background(), 2*time.Second)
			if ev, err := s.store.SetMachineStatus(offCtx, hello.MachineID, protocol.MachineOffline); err == nil && ev != nil {
				s.broker.Broadcast(*ev)
			}
			cancelOff()
		}
	}()

	// 3. Send hub.welcome as the very first frame BEFORE starting dispatch of pending leased attempts.
	// B6: Mark non-expired redelivered approvals as delivery_attempted right before writing hub.welcome.
	// If marking delivery_attempted fails, STOP and close the connection rather than sending without recording!
	var redeliverIDs []string
	for _, ap := range recRes.Reconcile.RedeliverApprovals {
		if ap.ApprovalID != "" && !ap.Expired {
			redeliverIDs = append(redeliverIDs, ap.ApprovalID)
		}
	}
	if len(redeliverIDs) > 0 {
		if err := s.store.MarkApprovalsDeliveryAttempted(r.Context(), redeliverIDs); err != nil {
			_ = conn.Close(websocket.StatusInternalError, "failed to record approval delivery attempt")
			return
		}
	}
	var cfgPayload *protocol.RunnerConfigPayload
	if agentCfg, rev, err := s.store.GetMachineAgentSettings(r.Context(), hello.MachineID); err == nil && len(agentCfg) > 0 {
		cfgPayload = &protocol.RunnerConfigPayload{
			ConfigRev: rev,
			Agents:    agentCfg,
		}
	}
	welcome := protocol.HubWelcomeFrame{
		Type:                protocol.RunnerMsgHubWelcome,
		ServerTime:          time.Now().UnixMilli(),
		HeartbeatIntervalMs: s.heartbeatInterval.Milliseconds(),
		LeaseTtlMs:          s.leaseTTL.Milliseconds(),
		Capabilities:        []string{protocol.RunnerCapAgentsV1, protocol.RunnerCapFSV1},
		Config:              cfgPayload,
		Reconcile:           recRes.Reconcile,
	}
	if err := writeRunnerRawJSON(r.Context(), conn, welcome); err != nil {
		return
	}

	// 4. Start serialized write pump for all subsequent frames
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-rc.done:
				return
			case msg := <-rc.sendCh:
				if msg.onStart != nil {
					if err := msg.onStart(); err != nil {
						if msg.errCh != nil {
							msg.errCh <- err
						}
						continue
					}
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg.data)
				wcancel()
				if err != nil && msg.onFail != nil {
					msg.onFail(err)
				}
				if msg.errCh != nil {
					msg.errCh <- err
				}
				if err != nil {
					return
				}
			}
		}
	}()

	// 5. Auto-consume any steered tasks whose cancelled attempts settled during reconcile (B9),
	//    drain any durable pending_queue_drain tasks (B9 crash recovery), and dispatch leased attempts.
	for _, notice := range recRes.SteerTasksToConsume {
		s.consumeAndDispatchNextPendingInput(ctx, notice.TaskID, notice.SteerInputID)
	}
	s.drainPendingQueues(ctx)
	s.dispatchPendingLeasedForMachine(ctx, hello.MachineID)

	// 6. Read loop for runner.heartbeat and attempt.outbox_batch
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var hdr protocol.RunnerFrameHeader
		if err := json.Unmarshal(data, &hdr); err != nil {
			continue
		}

		switch hdr.Type {
		case protocol.RunnerMsgHeartbeat:
			var hb protocol.RunnerHeartbeatFrame
			if err := json.Unmarshal(data, &hb); err != nil {
				continue
			}
			cancelIDs, err := s.store.RenewMachineLeases(
				ctx,
				hello.MachineID,
				hello.RunnerInstanceID,
				hb.LiveAttemptLeaseTokens,
				hb.Resources,
				hb.Adapters,
				time.Now().UnixMilli(),
				s.leaseTTL.Milliseconds(),
			)
			if err == nil {
				// B10: Re-deliver attempt.cancel for any live attempt with cancel_requested=1
				for _, cid := range cancelIDs {
					if attRec, err := s.store.GetAttemptRecord(ctx, cid); err == nil {
						_ = rc.sendJSON(protocol.AttemptCancelFrame{
							Type:       protocol.RunnerMsgAttemptCancel,
							AttemptID:  attRec.AttemptID,
							LeaseToken: attRec.LeaseToken,
						})
					}
				}
			}

		case protocol.RunnerMsgOutboxBatch:
			var batch protocol.AttemptOutboxBatchFrame
			if err := json.Unmarshal(data, &batch); err != nil {
				continue
			}
			ingestRes, err := s.store.IngestOutboxBatch(ctx, hello.MachineID, hello.RunnerInstanceID, batch)
			if err != nil {
				continue
			}
			if len(ingestRes.Events) > 0 {
				s.broker.Broadcast(ingestRes.Events...)
			}
			_ = rc.sendJSON(protocol.HubOutboxAckFrame{
				Type:           protocol.RunnerMsgOutboxAck,
				AttemptID:      batch.AttemptID,
				AckedOutboxSeq: ingestRes.AckedOutboxSeq,
				AcceptedMode:   ingestRes.AcceptedMode,
			})

			// B10: If an active attempt sending outbox frames has persisted cancel_requested=1, re-send cancel.
			if ingestRes.CancelRequested {
				_ = rc.sendJSON(protocol.AttemptCancelFrame{
					Type:       protocol.RunnerMsgAttemptCancel,
					AttemptID:  batch.AttemptID,
					LeaseToken: batch.LeaseToken,
				})
			}

			// Decoupled Ryugi registration for any delivery carrying .ryugi/artifacts/
			for _, dlvNotice := range ingestRes.PublishedDeliveries {
				if len(dlvNotice.RyugiArtifacts) > 0 {
					dlvID := dlvNotice.DeliveryID
					s.bgWG.Add(1)
					go func() {
						defer s.bgWG.Done()
						regCtx, cancelReg := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancelReg()
						_, _ = s.ryugi.RegisterDelivery(regCtx, dlvID)
					}()
				}
			}

			// B9: Auto-pop next queued input on succeeded OR on steer-triggered cancellation (never human Stop).
			if ingestRes.AutoConsumeQueue && ingestRes.TaskID != "" {
				s.consumeAndDispatchNextPendingInput(ctx, ingestRes.TaskID, ingestRes.SteerInputID)
			}

		case protocol.RunnerMsgAgentsReport:
			var rep protocol.RunnerAgentsReportFrame
			if err := json.Unmarshal(data, &rep); err != nil {
				continue
			}
			// Reject stale agent reports produced against an older Hub config revision (#3)
			if _, curRev, revErr := s.store.GetMachineAgentSettings(ctx, hello.MachineID); revErr == nil && curRev > 0 && rep.ConfigRev < curRev {
				continue
			}
			ready := make([]protocol.Adapter, 0, len(rep.Agents))
			for _, p := range rep.Agents {
				if p.State == protocol.AgentStateReady && p.Enabled {
					ready = append(ready, p.Adapter)
				}
			}
			rc.setAdapters(ready)
			if ev, err := s.store.SaveMachineAgentProbes(ctx, hello.MachineID, rep.Agents, nil); err == nil && ev != nil {
				s.broker.Broadcast(*ev)
			}

		case protocol.RunnerMsgRPCResult:
			var rpcRes protocol.RunnerRPCResultFrame
			if err := json.Unmarshal(data, &rpcRes); err != nil {
				continue
			}
			rc.resolveRPC(rpcRes)
		}
	}

	cancel()
	rc.close(websocket.StatusNormalClosure, "read loop exited")
	writeWG.Wait()
}

func writeRunnerRawJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(wctx, websocket.MessageText, data)
}
