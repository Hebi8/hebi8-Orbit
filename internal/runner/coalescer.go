package runner

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type attemptChunkBuffer struct {
	leaseToken  string
	kind        protocol.EventKind
	unsolicited bool
	text        strings.Builder
	gen         uint64
	timer       *time.Timer
}

// ChunkFlushFunc is invoked whenever a coalesced chunk window expires or is explicitly flushed.
type ChunkFlushFunc func(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool)

// ChunkCoalescer merges consecutive same-kind agent_message_chunk or agent_thought_chunk updates
// over a fixed 60ms deadline window while guaranteeing strict per-attempt chronological order
// across kind switches, unsolicited switches, tool calls, state transitions, and shutdown (B15).
// Lock ordering discipline: ALWAYS acquire flushMu before mu.
type ChunkCoalescer struct {
	window  time.Duration
	onFlush ChunkFlushFunc

	flushMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	gen     uint64
	buffers map[string]*attemptChunkBuffer
}

func NewChunkCoalescer(window time.Duration, onFlush ChunkFlushFunc) *ChunkCoalescer {
	if window <= 0 {
		window = 60 * time.Millisecond
	}
	return &ChunkCoalescer{
		window:  window,
		onFlush: onFlush,
		buffers: map[string]*attemptChunkBuffer{},
	}
}

func (c *ChunkCoalescer) Append(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool) {
	if text == "" {
		return
	}

	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}

	var toFlush *attemptChunkBuffer
	buf := c.buffers[attemptID]
	if buf != nil && (buf.kind != kind || buf.unsolicited != unsolicited) {
		toFlush = buf
		delete(c.buffers, attemptID)
		if toFlush.timer != nil {
			toFlush.timer.Stop()
		}
		buf = nil
	}
	c.mu.Unlock()

	if toFlush != nil && toFlush.text.Len() > 0 && c.onFlush != nil {
		c.onFlush(attemptID, toFlush.leaseToken, toFlush.kind, toFlush.text.String(), toFlush.unsolicited)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}

	buf = c.buffers[attemptID]
	if buf == nil {
		c.gen++
		gen := c.gen
		buf = &attemptChunkBuffer{
			leaseToken:  leaseToken,
			kind:        kind,
			unsolicited: unsolicited,
			gen:         gen,
		}
		// Fixed deadline from segment start (NEVER reset on subsequent appends -> prevents starvation).
		buf.timer = time.AfterFunc(c.window, func() {
			c.flushAttemptGen(attemptID, gen)
		})
		c.buffers[attemptID] = buf
	}
	buf.text.WriteString(text)
}

func (c *ChunkCoalescer) flushAttemptGen(attemptID string, gen uint64) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	buf := c.buffers[attemptID]
	if buf == nil || buf.gen != gen {
		c.mu.Unlock()
		return
	}
	delete(c.buffers, attemptID)
	if buf.timer != nil {
		buf.timer.Stop()
	}
	text := buf.text.String()
	leaseToken := buf.leaseToken
	kind := buf.kind
	unsolicited := buf.unsolicited
	c.mu.Unlock()

	if text != "" && c.onFlush != nil {
		c.onFlush(attemptID, leaseToken, kind, text, unsolicited)
	}
}

// FlushAttempt synchronously flushes any buffered message/thought chunk for attemptID
// so no raw_acp chunks reorder past tool calls, approvals, state transitions, or delivery publication.
func (c *ChunkCoalescer) FlushAttempt(attemptID string) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	buf := c.buffers[attemptID]
	if buf != nil {
		delete(c.buffers, attemptID)
		if buf.timer != nil {
			buf.timer.Stop()
		}
	}
	c.mu.Unlock()

	if buf != nil && buf.text.Len() > 0 && c.onFlush != nil {
		c.onFlush(attemptID, buf.leaseToken, buf.kind, buf.text.String(), buf.unsolicited)
	}
}

// Close stops all pending timers and synchronously flushes ALL remaining buffered segments
// so tail output is never dropped on shutdown.
func (c *ChunkCoalescer) Close() {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	type pending struct {
		attemptID string
		buf       *attemptChunkBuffer
	}
	var drained []pending

	c.mu.Lock()
	c.closed = true
	for attemptID, buf := range c.buffers {
		if buf.timer != nil {
			buf.timer.Stop()
		}
		drained = append(drained, pending{attemptID: attemptID, buf: buf})
		delete(c.buffers, attemptID)
	}
	c.mu.Unlock()

	sort.Slice(drained, func(i, j int) bool {
		return drained[i].attemptID < drained[j].attemptID
	})
	if c.onFlush == nil {
		return
	}
	for _, item := range drained {
		if item.buf.text.Len() > 0 {
			c.onFlush(item.attemptID, item.buf.leaseToken, item.buf.kind, item.buf.text.String(), item.buf.unsolicited)
		}
	}
}
