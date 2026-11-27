package runner

import (
	"sync"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestChunkCoalescerDefaultWindowIs60ms(t *testing.T) {
	var mu sync.Mutex
	var flushed []string
	var flushedAt time.Time

	c := NewChunkCoalescer(0, func(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool) {
		mu.Lock()
		flushed = append(flushed, text)
		flushedAt = time.Now()
		mu.Unlock()
	})
	defer c.Close()

	if c.window != 60*time.Millisecond {
		t.Fatalf("expected default coalescer window 60ms, got %v", c.window)
	}

	start := time.Now()
	c.Append("att_default_win", "tok_1", protocol.EventACPMessageBatch, "hello ", false)
	c.Append("att_default_win", "tok_1", protocol.EventACPMessageBatch, "world", false)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(flushed)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 || flushed[0] != "hello world" {
		t.Fatalf("expected single coalesced flush 'hello world', got %+v", flushed)
	}
	if elapsed := flushedAt.Sub(start); elapsed < 45*time.Millisecond {
		t.Fatalf("coalescer flushed too early (%v < 45ms)", elapsed)
	}
}
