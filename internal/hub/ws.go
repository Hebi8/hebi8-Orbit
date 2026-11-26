package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const wsPageSize = 50

var errWSRevoked = errors.New("client websocket revoked")

// EventBroker is a pure signal-wake fan-out hub backed by SQLite's monotonic events table.
// Each subscriber receives a coalescing capacity-1 wakeup signal; actual events are always
// read sequentially from SQLite in strict ascending seq order, eliminating dropped frames,
// out-of-order concurrent commit skips, and page truncation.
type EventBroker struct {
	mu   sync.RWMutex
	subs map[chan struct{}]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{
		subs: map[chan struct{}]struct{}{},
	}
}

func (b *EventBroker) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

// Notify wakes all connected WebSocket clients to read newly committed SQLite events.
func (b *EventBroker) Notify() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
			// Signal already pending; coalesced cleanly without losing any DB event
		}
	}
}

// Broadcast notifies connected subscribers when any non-raw event (or live acp.usage update) was committed.
func (b *EventBroker) Broadcast(events ...protocol.EventRecord) {
	for _, ev := range events {
		if ev.Visibility != protocol.VisibilityRawACP || ev.Kind == protocol.EventACPUsage {
			b.Notify()
			return
		}
	}
}

// projectWSEvent converts raw acp.usage records into lightweight non-raw notifications for client WS.
func projectWSEvent(ev protocol.EventRecord) protocol.EventRecord {
	if ev.Kind != protocol.EventACPUsage {
		return ev
	}
	slim := make(map[string]any, 2)
	if v, ok := ev.Payload["used"]; ok {
		slim["used"] = v
	}
	if v, ok := ev.Payload["size"]; ok {
		slim["size"] = v
	}
	ev.Visibility = protocol.VisibilityTimeline
	ev.Payload = slim
	return ev
}

type clientWSConn struct {
	conn          *websocket.Conn
	cancel        context.CancelFunc
	writeMu       sync.Mutex
	writeCtx      context.Context
	writeCancel   context.CancelFunc
	clientTokenID string
	revoked       atomic.Bool
	closeDone     chan struct{}
}

func (c *clientWSConn) writeData(msg protocol.WSServerMessage, insideHook func()) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.revoked.Load() {
		return errWSRevoked
	}
	if insideHook != nil {
		insideHook()
	}
	if c.revoked.Load() {
		return errWSRevoked
	}
	return writeWSJSON(c.writeCtx, c.conn, msg)
}

// SetWSBeforeReplayWriteHook installs a deterministic synchronization hook invoked right before
// each replay page is passed to writeData (used by tests to verify zero post-revoke writes).
func (s *Server) SetWSBeforeReplayWriteHook(fn func()) {
	s.clientWSMu.Lock()
	s.wsBeforeReplayWriteHook = fn
	s.clientWSMu.Unlock()
}

func (s *Server) getWSBeforeReplayWriteHook() func() {
	s.clientWSMu.Lock()
	fn := s.wsBeforeReplayWriteHook
	s.clientWSMu.Unlock()
	return fn
}

// SetWSInsideWriteHook installs a deterministic test hook invoked while clientWSConn.writeMu is held
// inside writeData, used to verify concurrent revokes wait for the same write barrier.
func (s *Server) SetWSInsideWriteHook(fn func()) {
	s.clientWSMu.Lock()
	s.wsInsideWriteHook = fn
	s.clientWSMu.Unlock()
}

func (s *Server) getWSInsideWriteHook() func() {
	s.clientWSMu.Lock()
	fn := s.wsInsideWriteHook
	s.clientWSMu.Unlock()
	return fn
}

func (s *Server) registerClientWS(ctx context.Context, c *clientWSConn) bool {
	s.clientWSMu.Lock()
	if s.clientWSConns == nil {
		s.clientWSConns = map[*clientWSConn]struct{}{}
	}
	s.clientWSConns[c] = struct{}{}
	s.clientWSMu.Unlock()

	if c.clientTokenID != "" {
		active, err := s.store.IsClientTokenActive(ctx, c.clientTokenID)
		if err != nil || !active {
			s.unregisterClientWS(c)
			return false
		}
	}
	return true
}

func (s *Server) unregisterClientWS(c *clientWSConn) {
	s.clientWSMu.Lock()
	delete(s.clientWSConns, c)
	s.clientWSMu.Unlock()
}

func (s *Server) revokeClientTokenSessions(clientTokenID string) {
	clientTokenID = strings.TrimSpace(clientTokenID)
	if clientTokenID == "" {
		return
	}
	s.revokeWSTicketsForClientToken(clientTokenID)

	for {
		s.clientWSMu.Lock()
		if waitCh, busy := s.inFlightRevokes[clientTokenID]; busy {
			s.clientWSMu.Unlock()
			<-waitCh
			continue
		}
		var targets []*clientWSConn
		for c := range s.clientWSConns {
			if c.clientTokenID == clientTokenID {
				targets = append(targets, c)
				delete(s.clientWSConns, c)
				// 1. Synchronously mark revoked and cancel both query ctx and writeCtx under clientWSMu.
				c.revoked.Store(true)
				c.cancel()
				if c.writeCancel != nil {
					c.writeCancel()
				}
			}
		}
		if len(targets) == 0 {
			s.clientWSMu.Unlock()
			return
		}
		if s.inFlightRevokes == nil {
			s.inFlightRevokes = map[string]chan struct{}{}
		}
		doneCh := make(chan struct{})
		s.inFlightRevokes[clientTokenID] = doneCh
		s.clientWSMu.Unlock()

		// 2. Synchronization barrier: wait for every in-flight writeData() to release writeMu
		//    BEFORE clearing inFlightRevokes and returning (so concurrent revokes also wait!).
		for _, c := range targets {
			c.writeMu.Lock()
			c.writeMu.Unlock()
			// 3. Flush 1008 Policy Violation close frame asynchronously (1s fallback CloseNow).
			go func(target *clientWSConn) {
				defer close(target.closeDone)
				done := make(chan struct{})
				go func() {
					_ = target.conn.Close(websocket.StatusPolicyViolation, "client token revoked")
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(1 * time.Second):
					_ = target.conn.CloseNow()
				}
			}(c)
		}

		s.clientWSMu.Lock()
		delete(s.inFlightRevokes, clientTokenID)
		s.clientWSMu.Unlock()
		close(doneCh)
	}
}

func (s *Server) closeAllClientWS() {
	s.clientWSMu.Lock()
	targets := make([]*clientWSConn, 0, len(s.clientWSConns))
	for c := range s.clientWSConns {
		targets = append(targets, c)
	}
	s.clientWSConns = map[*clientWSConn]struct{}{}
	s.clientWSMu.Unlock()

	for _, c := range targets {
		c.cancel()
		_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
		_ = c.conn.CloseNow()
	}
}

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	_, clientTokenID, statusCode, errMsg := s.validateWSRequest(r)
	if statusCode != http.StatusOK {
		code := "ERR_ORIGIN_FORBIDDEN"
		if statusCode == http.StatusUnauthorized {
			code = "ERR_UNAUTHORIZED"
		}
		writeAPIError(w, statusCode, code, errMsg)
		return
	}

	var sinceSeq int64
	if raw := r.URL.Query().Get("sinceSeq"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_SINCE_SEQ", "sinceSeq must be a non-negative integer")
			return
		}
		sinceSeq = parsed
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writeCtx, writeCancel := context.WithCancel(r.Context())
	defer writeCancel()

	wsEntry := &clientWSConn{
		conn:          conn,
		cancel:        cancel,
		writeCtx:      writeCtx,
		writeCancel:   writeCancel,
		clientTokenID: clientTokenID,
		closeDone:     make(chan struct{}),
	}
	if !s.registerClientWS(ctx, wsEntry) {
		_ = conn.Close(websocket.StatusPolicyViolation, "client token revoked")
		return
	}
	defer func() {
		s.unregisterClientWS(wsEntry)
		if wsEntry.revoked.Load() {
			// Wait for the async 1008 Policy Violation close frame to finish writing before defer conn.CloseNow().
			select {
			case <-wsEntry.closeDone:
			case <-time.After(1200 * time.Millisecond):
			}
		}
	}()

	// Subscribe BEFORE locking replayHead so any commit happening during replay triggers a wakeup.
	wakeCh, unsub := s.broker.Subscribe()
	defer unsub()

	// Drain client frames to process pings/closes promptly using r.Context() (NOT app ctx) so
	// synchronous app ctx cancellation on revoke does not trigger coder/websocket's hard-EOF
	// read-abort before our 1008 Policy Violation close frame is flushed.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}()

	// Phase 1: Paginated replay of ALL committed events in (sinceSeq, replayHead]
	if wsEntry.revoked.Load() || ctx.Err() != nil {
		return
	}
	replayHead, err := s.store.MaxEventSeq(ctx)
	if err != nil {
		if wsEntry.revoked.Load() || ctx.Err() != nil {
			return
		}
		_ = conn.Close(websocket.StatusInternalError, "replay head query failed")
		return
	}

	lastSentSeq := sinceSeq
	for lastSentSeq < replayHead {
		if wsEntry.revoked.Load() || ctx.Err() != nil {
			return
		}
		batch, err := s.store.ListEventsPage(ctx, lastSentSeq, replayHead, wsPageSize)
		if err != nil {
			if wsEntry.revoked.Load() || ctx.Err() != nil {
				return
			}
			_ = conn.Close(websocket.StatusInternalError, "replay page query failed")
			return
		}
		if len(batch) == 0 {
			break
		}
		if wsEntry.revoked.Load() || ctx.Err() != nil {
			return
		}
		if hook := s.getWSBeforeReplayWriteHook(); hook != nil {
			hook()
		}
		for i := range batch {
			batch[i] = projectWSEvent(batch[i])
		}
		if err := wsEntry.writeData(protocol.WSServerMessage{
			Type:          protocol.WSMessageReplay,
			Events:        batch,
			ReplayHeadSeq: replayHead,
			ServerTime:    time.Now().UnixMilli(),
		}, s.getWSInsideWriteHook()); err != nil {
			return
		}
		lastSentSeq = batch[len(batch)-1].Seq
	}
	if wsEntry.revoked.Load() || ctx.Err() != nil {
		return
	}
	if replayHead > lastSentSeq {
		lastSentSeq = replayHead
	}

	if err := wsEntry.writeData(protocol.WSServerMessage{
		Type:          protocol.WSMessageReplayEnd,
		ReplayHeadSeq: replayHead,
		ServerTime:    time.Now().UnixMilli(),
	}, nil); err != nil {
		return
	}

	// Helper: drain all newly committed SQLite events with seq > lastSentSeq in paginated ascending order
	drainLive := func() error {
		for {
			if wsEntry.revoked.Load() || ctx.Err() != nil {
				return context.Canceled
			}
			batch, err := s.store.ListEventsPage(ctx, lastSentSeq, 0, wsPageSize)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				return nil
			}
			for i := range batch {
				if wsEntry.revoked.Load() || ctx.Err() != nil {
					return context.Canceled
				}
				ev := projectWSEvent(batch[i])
				if err := wsEntry.writeData(protocol.WSServerMessage{
					Type:       protocol.WSMessageEvent,
					Event:      &ev,
					ServerTime: time.Now().UnixMilli(),
				}, nil); err != nil {
					return err
				}
				lastSentSeq = ev.Seq
			}
			if len(batch) < wsPageSize {
				return nil
			}
		}
	}

	// Catch up anything committed while replay pages were streaming
	if err := drainLive(); err != nil {
		return
	}

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	// Phase 2: Signal-driven SQLite tailing
	for {
		select {
		case <-ctx.Done():
			if !wsEntry.revoked.Load() {
				_ = conn.Close(websocket.StatusNormalClosure, "closing")
			}
			return
		case _, ok := <-wakeCh:
			if !ok {
				return
			}
			if err := drainLive(); err != nil {
				return
			}
		case <-ticker.C:
			if wsEntry.revoked.Load() || ctx.Err() != nil {
				return
			}
			if err := wsEntry.writeData(protocol.WSServerMessage{
				Type:       protocol.WSMessagePing,
				ServerTime: time.Now().UnixMilli(),
			}, nil); err != nil {
				return
			}
		}
	}
}

func writeWSJSON(ctx context.Context, conn *websocket.Conn, msg protocol.WSServerMessage) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(wctx, websocket.MessageText, data)
}
