package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestParseAgyUsageJSON(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	raw := []byte(`{
		"status": "SUCCESS",
		"num_turns": 0,
		"command": {
			"name": "/usage",
			"data": {
				"groups": [
					{
						"name": "Gemini Models",
						"description": "Models within this group: Gemini 3 Pro, Gemini 3 Flash",
						"buckets": [
							{
								"id": "gemini-5h",
								"name": "5-Hour Window",
								"window": "5h",
								"remaining_fraction": 0.72,
								"reset_time": "2026-06-15T14:30:00Z"
							},
							{
								"id": "gemini-weekly",
								"name": "Weekly Limit",
								"window": "weekly",
								"remaining_fraction": 0.90,
								"reset_time": "1970-01-01T00:00:00Z"
							}
						]
					},
					{
						"name": "Claude and GPT models",
						"description": "Models within this group: Claude Sonnet, GPT-5",
						"buckets": [
							{
								"id": "partner-5h",
								"window": "5h",
								"remaining_fraction": 0.25,
								"reset_time": "2026-06-14T00:00:00Z"
							}
						]
					}
				]
			}
		}
	}`)

	snap, err := parseAgyUsageJSON(raw, now)
	if err != nil {
		t.Fatalf("parseAgyUsageJSON failed: %v", err)
	}
	if !snap.Available || snap.Adapter != protocol.AdapterAgy {
		t.Fatalf("expected available agy snapshot, got %+v", snap)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("expected 3 windows, got %d (%+v)", len(snap.Windows), snap.Windows)
	}

	w0 := snap.Windows[0]
	if w0.Window != "5h" || w0.Group != "Gemini Models" || w0.UsedPercent != 28 || w0.RemainingPercent != 72 {
		t.Fatalf("unexpected w0: %+v", w0)
	}
	wantReset0 := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC).UnixMilli()
	if w0.ResetsAt != wantReset0 {
		t.Fatalf("expected w0 resetsAt=%d, got %d", wantReset0, w0.ResetsAt)
	}
	if len(w0.ModelHints) != 2 || w0.ModelHints[0] != "Gemini 3 Pro" || w0.ModelHints[1] != "Gemini 3 Flash" {
		t.Fatalf("unexpected w0 modelHints: %+v", w0.ModelHints)
	}

	// Weekly -> "7d", and 1970-01-01T00:00:00Z -> resetsAt == 0
	w1 := snap.Windows[1]
	if w1.Window != "7d" || w1.UsedPercent != 10 || w1.RemainingPercent != 90 || w1.ResetsAt != 0 {
		t.Fatalf("unexpected w1 (1970 epoch boundary): %+v", w1)
	}

	// Past reset_time -> resetsAt == 0
	w2 := snap.Windows[2]
	if w2.Window != "5h" || w2.Group != "Claude and GPT models" || w2.UsedPercent != 75 || w2.RemainingPercent != 25 || w2.ResetsAt != 0 {
		t.Fatalf("unexpected w2 (past reset_time boundary): %+v", w2)
	}

	// Non-SUCCESS status -> unavailable or error
	failSnap, failErr := parseAgyUsageJSON([]byte(`{"status":"ERROR","error":"not logged in"}`), now)
	if failErr == nil && failSnap.Available {
		t.Fatalf("expected error or unavailable snapshot for ERROR status, got %+v", failSnap)
	}
}

func TestParseClaudeUsageJSON(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	raw := []byte(`{
		"five_hour": {
			"utilization": 34.5,
			"resets_at": "2026-06-15T13:00:00Z"
		},
		"seven_day": {
			"utilization": 18.0,
			"resets_at": "2026-06-20T00:00:00Z"
		},
		"seven_day_opus": {
			"utilization": 62.5,
			"resets_at": "1970-01-01T00:00:00Z"
		},
		"seven_day_sonnet": {
			"utilization": 12.0,
			"resets_at": "2026-06-20T00:00:00Z"
		}
	}`)

	snap, err := parseClaudeUsageJSON(raw, now)
	if err != nil {
		t.Fatalf("parseClaudeUsageJSON failed: %v", err)
	}
	if !snap.Available || snap.Adapter != protocol.AdapterClaude {
		t.Fatalf("expected available claude snapshot, got %+v", snap)
	}
	if len(snap.Windows) != 4 {
		t.Fatalf("expected 4 windows, got %d (%+v)", len(snap.Windows), snap.Windows)
	}
	if snap.Windows[0].Window != "5h" || snap.Windows[0].UsedPercent != 34.5 || snap.Windows[0].RemainingPercent != 65.5 {
		t.Fatalf("unexpected five_hour window: %+v", snap.Windows[0])
	}
	want5hReset := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC).UnixMilli()
	if snap.Windows[0].ResetsAt != want5hReset {
		t.Fatalf("expected 5h resetsAt=%d, got %d", want5hReset, snap.Windows[0].ResetsAt)
	}
	if snap.Windows[1].Window != "7d" || snap.Windows[1].UsedPercent != 18 || snap.Windows[1].RemainingPercent != 82 {
		t.Fatalf("unexpected seven_day window: %+v", snap.Windows[1])
	}
	if snap.Windows[2].Group != "Opus" || snap.Windows[2].ResetsAt != 0 || snap.Windows[2].UsedPercent != 62.5 {
		t.Fatalf("unexpected seven_day_opus window: %+v", snap.Windows[2])
	}
	if snap.Windows[3].Group != "Sonnet" || snap.Windows[3].UsedPercent != 12 {
		t.Fatalf("unexpected seven_day_sonnet window: %+v", snap.Windows[3])
	}
}

func TestParseCodexRateLimitsJSON(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	futureSec := now.Add(3 * time.Hour).Unix()
	weekSec := now.Add(5 * 24 * time.Hour).Unix()

	raw, _ := json.Marshal(map[string]any{
		"rateLimits": map[string]any{
			"limitId": "codex",
			"primary": map[string]any{
				"usedPercent":        20,
				"windowDurationMins": 10080,
				"resetsAt":           weekSec,
			},
			"secondary": nil,
		},
		"rateLimitsByLimitId": map[string]any{
			"codex": map[string]any{
				"limitId": "codex",
				"primary": map[string]any{
					"usedPercent":        20,
					"windowDurationMins": 10080,
					"resetsAt":           weekSec,
				},
			},
			"codex_spark": map[string]any{
				"limitId":   "codex_spark",
				"limitName": "Codex Spark",
				"primary": map[string]any{
					"usedPercent":        45,
					"windowDurationMins": 300,
					"resetsAt":           futureSec,
				},
				"secondary": map[string]any{
					"usedPercent":        15,
					"windowDurationMins": 10080,
					"resetsAt":           0,
				},
			},
		},
	})

	snap, err := parseCodexRateLimitsJSON(raw, now)
	if err != nil {
		t.Fatalf("parseCodexRateLimitsJSON failed: %v", err)
	}
	if !snap.Available || snap.Adapter != protocol.AdapterCodex {
		t.Fatalf("expected available codex snapshot, got %+v", snap)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("expected 3 deduped windows, got %d (%+v)", len(snap.Windows), snap.Windows)
	}
	// First bucket is primary codex (windowDurationMins=10080 -> "7d", Unix seconds -> ms)
	if snap.Windows[0].Window != "7d" || snap.Windows[0].UsedPercent != 20 || snap.Windows[0].RemainingPercent != 80 {
		t.Fatalf("unexpected codex primary window: %+v", snap.Windows[0])
	}
	if snap.Windows[0].ResetsAt != weekSec*1000 {
		t.Fatalf("expected resetsAt=%d ms, got %d", weekSec*1000, snap.Windows[0].ResetsAt)
	}
	// Spark 5h and 7d windows
	if snap.Windows[1].Group != "Codex Spark" || snap.Windows[1].Window != "5h" || snap.Windows[1].UsedPercent != 45 || snap.Windows[1].ResetsAt != futureSec*1000 {
		t.Fatalf("unexpected spark 5h window: %+v", snap.Windows[1])
	}
	if snap.Windows[2].Group != "Codex Spark" || snap.Windows[2].Window != "7d" || snap.Windows[2].UsedPercent != 15 || snap.Windows[2].ResetsAt != 0 {
		t.Fatalf("unexpected spark 7d window: %+v", snap.Windows[2])
	}
}

func TestQuotaCollector_TTLCacheAndSingleFlight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binaries are unix-only")
	}
	tmp := t.TempDir()
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	// Fake agy CLI that counts invocations and returns /usage JSON.
	agyCountFile := filepath.Join(tmp, "agy.count")
	fakeAgy := filepath.Join(tmp, "agy")
	agyScript := `#!/bin/sh
sleep 0.05
echo x >> "` + agyCountFile + `"
cat <<'EOF'
{"status":"SUCCESS","num_turns":0,"command":{"name":"/usage","data":{"groups":[{"name":"Gemini Models","description":"Models within this group: Gemini 3 Pro","buckets":[{"id":"g5h","window":"5h","remaining_fraction":0.8,"reset_time":"2026-06-15T14:00:00Z"}]}]}}}
EOF
`
	if err := os.WriteFile(fakeAgy, []byte(agyScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake codex CLI implementing minimal app-server JSON-RPC handshake.
	codexCountFile := filepath.Join(tmp, "codex.count")
	fakeCodex := filepath.Join(tmp, "codex")
	futureSec := strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)
	codexScript := `#!/bin/sh
echo x >> "` + codexCountFile + `"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"codex-test"}}'
      ;;
    *'"method":"account/rateLimits/read"'*)
      echo '{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":30,"windowDurationMins":300,"resetsAt":` + futureSec + `}}}}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(fakeCodex, []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake Claude OAuth HTTP server verifying headers and counting requests.
	var claudeHits atomic.Int32
	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeHits.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-claude-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-beta") != claudeOAuthBetaHeader {
			http.Error(w, "missing beta header", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":22,"resets_at":"2026-06-15T12:00:00Z"},"seven_day":{"utilization":11,"resets_at":"2026-06-20T00:00:00Z"}}`))
	}))
	defer claudeSrv.Close()

	qc := NewQuotaCollector(QuotaCollectorConfig{
		TTL: 2 * time.Minute,
		ResolveMainCLI: func(adp protocol.Adapter) string {
			switch adp {
			case protocol.AdapterAgy:
				return fakeAgy
			case protocol.AdapterCodex:
				return fakeCodex
			default:
				return ""
			}
		},
		ClaudeTokenFn:  func(context.Context) string { return "test-claude-token" },
		ClaudeUsageURL: claudeSrv.URL,
		NowFn:          func() time.Time { return now },
	})

	ctx := context.Background()
	// Concurrent single-flight collection for agy should only invoke fakeAgy ONCE.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := qc.CollectOne(ctx, protocol.AdapterAgy, false)
			if !snap.Available || len(snap.Windows) != 1 || snap.Windows[0].UsedPercent != 20 {
				t.Errorf("unexpected concurrent agy snapshot: %+v", snap)
			}
		}()
	}
	wg.Wait()

	countLines := func(path string) int {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(b)))
	}
	if n := countLines(agyCountFile); n != 1 {
		t.Fatalf("expected single-flight to invoke fake agy once, got %d", n)
	}

	// CollectAll (force=false) reuses agy cache and fetches claude + codex once.
	all := qc.CollectAll(ctx, nil, false)
	if len(all) != 3 {
		t.Fatalf("expected 3 adapter snapshots, got %d", len(all))
	}
	for _, s := range all {
		if !s.Available || len(s.Windows) == 0 {
			t.Fatalf("expected available snapshot for %s, got %+v", s.Adapter, s)
		}
	}
	if n := countLines(agyCountFile); n != 1 {
		t.Fatalf("expected agy cache hit during CollectAll(force=false), got %d invocations", n)
	}
	if claudeHits.Load() != 1 || countLines(codexCountFile) != 1 {
		t.Fatalf("expected 1 hit each for claude (%d) and codex (%d)", claudeHits.Load(), countLines(codexCountFile))
	}

	// Second CollectAll(force=false) within TTL should make ZERO new invocations.
	_ = qc.CollectAll(ctx, nil, false)
	if countLines(agyCountFile) != 1 || claudeHits.Load() != 1 || countLines(codexCountFile) != 1 {
		t.Fatalf("expected TTL cache hits across all adapters")
	}

	// Snapshot returns all 3 without invoking anything.
	snaps := qc.Snapshot()
	if len(snaps) != 3 {
		t.Fatalf("expected 3 cached snapshots, got %d", len(snaps))
	}

	// CollectAll(force=true) refreshes all 3 adapters.
	_ = qc.CollectAll(ctx, nil, true)
	if countLines(agyCountFile) != 2 || claudeHits.Load() != 2 || countLines(codexCountFile) != 2 {
		t.Fatalf("expected force=true to refresh all 3 adapters (agy=%d claude=%d codex=%d)",
			countLines(agyCountFile), claudeHits.Load(), countLines(codexCountFile))
	}
}
