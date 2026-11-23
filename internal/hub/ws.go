package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const wsPageSize = 50

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

// Broadcast notifies connected subscribers when any non-raw event was committed.
func (b *EventBroker) Broadcast(events ...protocol.EventRecord) {
	for _, ev := range events {
		if ev.Visibility != protocol.VisibilityRawACP {
			b.Notify()
			return
		}
	}
}

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	_, statusCode, errMsg := s.validateWSRequest(r)
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

	// Subscribe BEFORE locking replayHead so any commit happening during replay triggers a wakeup.
	wakeCh, unsub := s.broker.Subscribe()
	defer unsub()

	// Drain client frames to process pings/closes promptly.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Phase 1: Paginated replay of ALL committed events in (sinceSeq, replayHead]
	replayHead, err := s.store.MaxEventSeq(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "replay head query failed")
		return
	}

	lastSentSeq := sinceSeq
	for lastSentSeq < replayHead {
		batch, err := s.store.ListEventsPage(ctx, lastSentSeq, replayHead, wsPageSize)
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "replay page query failed")
			return
		}
		if len(batch) == 0 {
			break
		}
		if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
			Type:          protocol.WSMessageReplay,
			Events:        batch,
			ReplayHeadSeq: replayHead,
			ServerTime:    time.Now().UnixMilli(),
		}); err != nil {
			return
		}
		lastSentSeq = batch[len(batch)-1].Seq
	}
	if replayHead > lastSentSeq {
		lastSentSeq = replayHead
	}

	if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
		Type:          protocol.WSMessageReplayEnd,
		ReplayHeadSeq: replayHead,
		ServerTime:    time.Now().UnixMilli(),
	}); err != nil {
		return
	}

	// Helper: drain all newly committed SQLite events with seq > lastSentSeq in paginated ascending order
	drainLive := func() error {
		for {
			batch, err := s.store.ListEventsPage(ctx, lastSentSeq, 0, wsPageSize)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				return nil
			}
			for i := range batch {
				ev := batch[i]
				if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
					Type:       protocol.WSMessageEvent,
					Event:      &ev,
					ServerTime: time.Now().UnixMilli(),
				}); err != nil {
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
			_ = conn.Close(websocket.StatusNormalClosure, "closing")
			return
		case _, ok := <-wakeCh:
			if !ok {
				return
			}
			if err := drainLive(); err != nil {
				return
			}
		case <-ticker.C:
			if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
				Type:       protocol.WSMessagePing,
				ServerTime: time.Now().UnixMilli(),
			}); err != nil {
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
