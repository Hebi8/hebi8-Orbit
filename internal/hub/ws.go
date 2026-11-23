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

// EventBroker fans out newly committed non-raw events to connected /ws/client sockets.
type EventBroker struct {
	mu   sync.RWMutex
	subs map[chan protocol.EventRecord]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{
		subs: map[chan protocol.EventRecord]struct{}{},
	}
}

func (b *EventBroker) Subscribe() (chan protocol.EventRecord, func()) {
	ch := make(chan protocol.EventRecord, 256)
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

func (b *EventBroker) Broadcast(events ...protocol.EventRecord) {
	if len(events) == 0 {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ev := range events {
		if ev.Visibility == protocol.VisibilityRawACP {
			continue
		}
		for ch := range b.subs {
			select {
			case ch <- ev:
			default:
				// Slow consumer drops frame and will resync on reconnect via sinceSeq
			}
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

	// Origin has already been strictly validated above; skip coder/websocket's built-in host check
	// so reverse-proxied Tailscale HTTPS origins work cleanly against 127.0.0.1 listen addresses.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Subscribe BEFORE querying replay snapshot so live events produced during replay are buffered.
	subCh, unsub := s.broker.Subscribe()
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

	replayEvents, replayHead, err := s.store.ListEventsAfter(ctx, sinceSeq, 1000)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "replay query failed")
		return
	}

	if len(replayEvents) > 0 {
		if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
			Type:          protocol.WSMessageReplay,
			Events:        replayEvents,
			ReplayHeadSeq: replayHead,
			ServerTime:    time.Now().UnixMilli(),
		}); err != nil {
			return
		}
	}

	if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
		Type:          protocol.WSMessageReplayEnd,
		ReplayHeadSeq: replayHead,
		ServerTime:    time.Now().UnixMilli(),
	}); err != nil {
		return
	}

	lastSentSeq := replayHead
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "closing")
			return
		case ev, ok := <-subCh:
			if !ok {
				return
			}
			if ev.Seq <= lastSentSeq {
				continue
			}
			evCopy := ev
			if err := writeWSJSON(ctx, conn, protocol.WSServerMessage{
				Type:       protocol.WSMessageEvent,
				Event:      &evCopy,
				ServerTime: time.Now().UnixMilli(),
			}); err != nil {
				return
			}
			lastSentSeq = ev.Seq
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
