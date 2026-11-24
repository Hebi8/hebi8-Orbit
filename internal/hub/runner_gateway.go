package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type runnerConn struct {
	machineID        string
	runnerInstanceID string
	conn             *websocket.Conn
	sendCh           chan []byte
	done             chan struct{}
	closeOnce        sync.Once
}

func newRunnerConn(machineID, runnerInstanceID string, conn *websocket.Conn) *runnerConn {
	return &runnerConn{
		machineID:        machineID,
		runnerInstanceID: runnerInstanceID,
		conn:             conn,
		sendCh:           make(chan []byte, 256),
		done:             make(chan struct{}),
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
	case rc.sendCh <- data:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("runner send queue full")
	}
}

func (rc *runnerConn) close(code websocket.StatusCode, reason string) {
	rc.closeOnce.Do(func() {
		close(rc.done)
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

// DispatchAttempt sends an attempt.dispatch frame to machineID if connected.
func (g *RunnerGateway) DispatchAttempt(machineID string, frame protocol.AttemptDispatchFrame) (string, error) {
	rc := g.get(machineID)
	if rc == nil {
		return "", fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgAttemptDispatch
	if err := rc.sendJSON(frame); err != nil {
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

// SendApprovalDecision pushes an approval.decision frame to machineID if connected.
func (g *RunnerGateway) SendApprovalDecision(machineID string, frame protocol.ApprovalDecisionFrame) error {
	rc := g.get(machineID)
	if rc == nil {
		return fmt.Errorf("runner %s not connected", machineID)
	}
	frame.Type = protocol.RunnerMsgApprovalDecide
	return rc.sendJSON(frame)
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
	} else if !isLoopbackAddr(r.RemoteAddr) && extractBearerToken(r.Header.Get("Authorization")) == "" {
		writeAPIError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "runner bearer token required")
		return
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

	rc := newRunnerConn(hello.MachineID, hello.RunnerInstanceID, conn)
	if prev := s.runners.register(rc); prev != nil && prev != rc {
		prev.close(websocket.StatusGoingAway, "replaced by newer runner connection")
	}

	defer func() {
		rc.close(websocket.StatusNormalClosure, "closing")
		if s.runners.unregister(rc) {
			offCtx, cancelOff := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.store.SetMachineStatus(offCtx, hello.MachineID, protocol.MachineOffline)
			cancelOff()
		}
	}()

	// 3. Send hub.welcome as the very first frame BEFORE starting dispatch of pending leased attempts
	welcome := protocol.HubWelcomeFrame{
		Type:                protocol.RunnerMsgHubWelcome,
		ServerTime:          time.Now().UnixMilli(),
		HeartbeatIntervalMs: s.heartbeatInterval.Milliseconds(),
		LeaseTtlMs:          s.leaseTTL.Milliseconds(),
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
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// 5. Dispatch any queued/leased attempts waiting for this machine
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
			_ = s.store.RenewMachineLeases(
				ctx,
				hello.MachineID,
				hello.RunnerInstanceID,
				hb.LiveAttemptLeaseTokens,
				hb.Resources,
				time.Now().UnixMilli(),
				s.leaseTTL.Milliseconds(),
			)

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

			// When an attempt succeeds, automatically pop & dispatch the next queued task_pending_input
			if ingestRes.AttemptSucceeded && ingestRes.TaskID != "" {
				s.consumeAndDispatchNextPendingInput(ctx, ingestRes.TaskID)
			}
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
