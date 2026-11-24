package runner

import (
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type chunkKey struct {
	attemptID string
	kind      protocol.EventKind
}

type chunkBuffer struct {
	leaseToken  string
	unsolicited bool
	text        strings.Builder
	timer       *time.Timer
}

// ChunkFlushFunc is invoked whenever a coalesced chunk window expires or is explicitly flushed.
type ChunkFlushFunc func(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool)

// ChunkCoalescer merges consecutive agent_message_chunk and agent_thought_chunk updates
// over a 200ms sliding window before writing a single raw_acp Outbox item.
type ChunkCoalescer struct {
	window  time.Duration
	onFlush ChunkFlushFunc

	mu      sync.Mutex
	closed  bool
	buffers map[chunkKey]*chunkBuffer
}

func NewChunkCoalescer(window time.Duration, onFlush ChunkFlushFunc) *ChunkCoalescer {
	if window <= 0 {
		window = 200 * time.Millisecond
	}
	return &ChunkCoalescer{
		window:  window,
		onFlush: onFlush,
		buffers: map[chunkKey]*chunkBuffer{},
	}
}

func (c *ChunkCoalescer) Append(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}

	key := chunkKey{attemptID: attemptID, kind: kind}
	buf, ok := c.buffers[key]
	if !ok {
		buf = &chunkBuffer{
			leaseToken:  leaseToken,
			unsolicited: unsolicited,
		}
		c.buffers[key] = buf
	}
	buf.text.WriteString(text)
	if unsolicited {
		buf.unsolicited = true
	}
	if buf.timer != nil {
		buf.timer.Stop()
	}
	buf.timer = time.AfterFunc(c.window, func() {
		c.flushKey(key)
	})
}

func (c *ChunkCoalescer) flushKey(key chunkKey) {
	c.mu.Lock()
	buf, ok := c.buffers[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.buffers, key)
	if buf.timer != nil {
		buf.timer.Stop()
	}
	text := buf.text.String()
	leaseToken := buf.leaseToken
	unsolicited := buf.unsolicited
	c.mu.Unlock()

	if text != "" && c.onFlush != nil {
		c.onFlush(key.attemptID, leaseToken, key.kind, text, unsolicited)
	}
}

// FlushAttempt synchronously flushes all buffered message/thought chunks for attemptID
// so no raw_acp chunks reorder past state transitions or delivery publication.
func (c *ChunkCoalescer) FlushAttempt(attemptID string) {
	type pendingFlush struct {
		kind        protocol.EventKind
		leaseToken  string
		text        string
		unsolicited bool
	}
	var list []pendingFlush

	c.mu.Lock()
	for k, buf := range c.buffers {
		if k.attemptID != attemptID {
			continue
		}
		delete(c.buffers, k)
		if buf.timer != nil {
			buf.timer.Stop()
		}
		if buf.text.Len() > 0 {
			list = append(list, pendingFlush{
				kind:        k.kind,
				leaseToken:  buf.leaseToken,
				text:        buf.text.String(),
				unsolicited: buf.unsolicited,
			})
		}
	}
	c.mu.Unlock()

	if c.onFlush == nil {
		return
	}
	for _, p := range list {
		c.onFlush(attemptID, p.leaseToken, p.kind, p.text, p.unsolicited)
	}
}

// Close stops all pending timers and flushes remaining buffers cleanly.
func (c *ChunkCoalescer) Close() {
	c.mu.Lock()
	c.closed = true
	for k, buf := range c.buffers {
		if buf.timer != nil {
			buf.timer.Stop()
		}
		delete(c.buffers, k)
	}
	c.mu.Unlock()
}
