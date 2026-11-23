package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

// ErrSyncPaused is returned when Ryugi syncing/acking is paused during a maintenance/backup window.
var ErrSyncPaused = errors.New("ryugi sync is paused during maintenance window")

// RyugiRunner executes ryugi CLI commands (replaceable in unit tests).
type RyugiRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRyugiRunner runs the real ryugi binary with optional isolated RYUGI_SERVER_HOME.
type ExecRyugiRunner struct {
	Bin        string
	ServerHome string
}

func (e ExecRyugiRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := e.Bin
	if bin == "" {
		bin = "ryugi"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	env := os.Environ()
	if e.ServerHome != "" {
		env = setEnvVar(env, "RYUGI_SERVER_HOME", e.ServerHome)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if stdout.Len() > 0 && bytes.HasPrefix(bytes.TrimSpace(stdout.Bytes()), []byte("{")) {
			return stdout.Bytes(), fmt.Errorf("ryugi %s failed: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("ryugi %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
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

// RyugiBridge manages frozen snapshot registration and single-flight annotation polling
// with strict SQLite-commit-before-receipt-ack semantics.
type RyugiBridge struct {
	store        *store.Store
	broker       *EventBroker
	runner       RyugiRunner
	dataDir      string
	host         string
	urlHost      string
	port         int
	publicOrigin string
	pollInterval time.Duration
	isPausedFn   func() bool

	mu       sync.Mutex
	inFlight map[string]*singleFlightEntry
	syncOpMu sync.RWMutex // held for read during SyncBinding (fetch->commit->ack); DrainInFlight acquires write lock
}

type singleFlightEntry struct {
	mu sync.Mutex
}

// RyugiBridgeConfig configures a RyugiBridge instance.
type RyugiBridgeConfig struct {
	Store        *store.Store
	Broker       *EventBroker
	Runner       RyugiRunner
	DataDir      string
	Host         string
	URLHost      string
	Port         int
	PublicOrigin string
	PollInterval time.Duration
	IsPaused     func() bool
}

func NewRyugiBridge(cfg RyugiBridgeConfig) *RyugiBridge {
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 6 * time.Second
	}
	return &RyugiBridge{
		store:        cfg.Store,
		broker:       cfg.Broker,
		runner:       cfg.Runner,
		dataDir:      cfg.DataDir,
		host:         strings.TrimSpace(cfg.Host),
		urlHost:      strings.TrimSpace(cfg.URLHost),
		port:         cfg.Port,
		publicOrigin: cfg.PublicOrigin,
		pollInterval: interval,
		isPausedFn:   cfg.IsPaused,
		inFlight:     map[string]*singleFlightEntry{},
	}
}

func (b *RyugiBridge) isPaused() bool {
	return b.isPausedFn != nil && b.isPausedFn()
}

// DrainInFlight blocks until every currently executing SyncBinding (read -> SQLite COMMIT -> ack)
// finishes. Callers MUST set the paused state BEFORE calling DrainInFlight so no new syncs begin.
func (b *RyugiBridge) DrainInFlight() {
	b.syncOpMu.Lock()
	b.syncOpMu.Unlock()
}

// CalibrateActiveBindings scans all active=1 Ryugi bindings at startup and idempotently
// calibrates permissions on each existing frozen_project_dir without re-exporting or touching comments.
func (b *RyugiBridge) CalibrateActiveBindings(ctx context.Context) error {
	records, err := b.store.ListRyugiBindings(ctx, "", true)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	var errs []string
	for _, rec := range records {
		key := rec.DeliveryID + "\x00" + rec.FrozenProjectDir
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, err := gitops.CalibrateFrozenSnapshot(rec.FrozenProjectDir, rec.DeliveryID); err != nil {
			errs = append(errs, fmt.Sprintf("%s (%s): %v", rec.DeliveryID, rec.FrozenProjectDir, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("calibrate active ryugi bindings: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (b *RyugiBridge) bindingGate(bindingID string) *singleFlightEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.inFlight[bindingID]
	if !ok {
		e = &singleFlightEntry{}
		b.inFlight[bindingID] = e
	}
	return e
}

// RewriteRyugiURL preserves the full path, raw query (including ?key=...), and fragment
// from rawURL while rewriting only the origin (scheme + host[:port]) when publicOrigin is set.
func RewriteRyugiURL(rawURL, publicOrigin string) string {
	rawURL = strings.TrimSpace(rawURL)
	publicOrigin = strings.TrimSpace(publicOrigin)
	if rawURL == "" || publicOrigin == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	pub, err := url.Parse(publicOrigin)
	if err != nil || pub.Scheme == "" || pub.Host == "" {
		return rawURL
	}
	u.Scheme = pub.Scheme
	u.Host = pub.Host
	return u.String()
}

type ryugiCLIEnvelope struct {
	Schema  string          `json:"schema"`
	Command string          `json:"command"`
	Outcome string          `json:"outcome"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"httpStatus"`
	} `json:"error"`
}

type ryugiStartData struct {
	URL      string `json:"url"`
	LocalURL string `json:"localUrl"`
	Artifact string `json:"artifact"`
	Status   string `json:"status"`
}

type ryugiPage struct {
	Returned   int     `json:"returned"`
	Total      int     `json:"total"`
	HasMore    bool    `json:"hasMore"`
	NextCursor *string `json:"nextCursor"`
	Snapshot   string  `json:"snapshot"`
}

type ryugiThreadMessage struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Name   string `json:"name"`
	Text   string `json:"text"`
	At     string `json:"at"`
}

type ryugiThreadItem struct {
	ID        string               `json:"id"`
	Anchor    map[string]any       `json:"anchor"`
	Status    string               `json:"status"`
	StatusBy  string               `json:"statusBy"`
	WaitingOn string               `json:"waitingOn"`
	Messages  []ryugiThreadMessage `json:"messages"`
}

type ryugiSection struct {
	Items    []json.RawMessage `json:"items"`
	Evidence string            `json:"evidence"`
	Snapshot string            `json:"snapshot"`
	Receipt  string            `json:"receipt"`
	Page     ryugiPage         `json:"page"`
	// Collected per-page receipts (all unique non-empty receipts across paginated calls)
	Receipts []string `json:"-"`
}

type ryugiCommentListData struct {
	Artifact string                  `json:"artifact"`
	Sections map[string]ryugiSection `json:"sections"`
	Items    []json.RawMessage       `json:"items"`
	Snapshot string                  `json:"snapshot"`
	Receipt  string                  `json:"receipt"`
	Page     *ryugiPage              `json:"page"`
}

type ryugiChangesData struct {
	Artifact string            `json:"artifact"`
	Snapshot string            `json:"snapshot"`
	Receipt  string            `json:"receipt"`
	Empty    bool              `json:"empty"`
	Diff     string            `json:"diff"`
	Blocks   []json.RawMessage `json:"blocks"`
	Page     *ryugiPage        `json:"page"`
}

// RegisterDelivery exports a frozen snapshot for deliveryID and registers each
// .ryugi/artifacts/<type>/<slug> with the Ryugi sidecar. Emits delivery.ryugi_updated
// in the same DB transaction and broadcasts a WS notification so clients refresh URLs live.
func (b *RyugiBridge) RegisterDelivery(ctx context.Context, deliveryID string) ([]protocol.RyugiBindingView, error) {
	dlv, _, _, proj, err := b.store.GetDeliveryDetail(ctx, deliveryID)
	if err != nil {
		return nil, err
	}

	locators := dlv.Manifest.RyugiArtifacts
	if len(locators) == 0 {
		discovered, err := gitops.DiscoverRyugiArtifactsInMirror(ctx, proj.MirrorBarePath, dlv.HeadSHA)
		if err != nil {
			return nil, err
		}
		locators = discovered
	}
	if len(locators) == 0 {
		return nil, nil
	}

	frozenDir := filepath.Join(b.dataDir, "deliveries", deliveryID)
	exported, err := gitops.ExportFrozenSnapshot(ctx, proj.MirrorBarePath, dlv.HeadSHA, deliveryID, frozenDir)
	if err != nil {
		_, ev, _ := b.store.SaveRyugiBindings(ctx, deliveryID, frozenDir, protocol.RyugiSyncFailed, nil)
		if ev != nil && b.broker != nil {
			b.broker.Broadcast(*ev)
		}
		return nil, fmt.Errorf("export frozen snapshot: %w", err)
	}
	if len(exported) == 0 {
		return nil, nil
	}

	var bindings []protocol.RyugiBindingView
	var regErrors []string
	for _, art := range exported {
		args := []string{"start", "--background", "--no-open"}
		if b.host != "" {
			args = append(args, "--host", b.host)
		}
		if b.urlHost != "" {
			args = append(args, "--url-host", b.urlHost)
		}
		if b.port > 0 {
			args = append(args, "--port", strconv.Itoa(b.port))
		}
		args = append(args, "--project-dir", frozenDir, "--artifact", art.Locator, "--format", "json")

		out, runErr := b.runner.Run(ctx, args...)
		env, parseErr := parseRyugiEnvelope(out)
		if parseErr != nil {
			if runErr != nil {
				regErrors = append(regErrors, fmt.Sprintf("%s: %v", art.Locator, runErr))
			} else {
				regErrors = append(regErrors, fmt.Sprintf("%s: %v", art.Locator, parseErr))
			}
			continue
		}
		if env.Outcome == "error" || (runErr != nil && env.Data == nil) {
			msg := "ryugi start error"
			if env.Error != nil {
				msg = env.Error.Code + ": " + env.Error.Message
			} else if runErr != nil {
				msg = runErr.Error()
			}
			regErrors = append(regErrors, fmt.Sprintf("%s: %s", art.Locator, msg))
			continue
		}

		var sd ryugiStartData
		if err := json.Unmarshal(env.Data, &sd); err != nil {
			regErrors = append(regErrors, fmt.Sprintf("%s: decode start data: %v", art.Locator, err))
			continue
		}
		rawURL := sd.URL
		if rawURL == "" {
			rawURL = sd.LocalURL
		}
		openURL := RewriteRyugiURL(rawURL, b.publicOrigin)
		bindings = append(bindings, protocol.RyugiBindingView{
			DeliveryID:        deliveryID,
			ProjectID:         dlv.ProjectID,
			TaskID:            dlv.TaskID,
			FrozenProjectDir:  frozenDir,
			ArtifactLocator:   art.Locator,
			DerivedArtifactID: art.DerivedArtifactID,
			OpenURL:           openURL,
			Active:            true,
		})
	}

	if len(regErrors) > 0 && len(bindings) == 0 {
		_, ev, _ := b.store.SaveRyugiBindings(ctx, deliveryID, frozenDir, protocol.RyugiSyncFailed, nil)
		if ev != nil && b.broker != nil {
			b.broker.Broadcast(*ev)
		}
		return nil, fmt.Errorf("ryugi register failed: %s", strings.Join(regErrors, "; "))
	}

	syncState := protocol.RyugiSyncReady
	if len(regErrors) > 0 {
		syncState = protocol.RyugiSyncFailed
	}
	saved, ev, err := b.store.SaveRyugiBindings(ctx, deliveryID, frozenDir, syncState, bindings)
	if err != nil {
		return nil, err
	}
	if ev != nil && b.broker != nil {
		b.broker.Broadcast(*ev)
	}
	if len(regErrors) > 0 {
		return saved, fmt.Errorf("partial ryugi registration errors: %s", strings.Join(regErrors, "; "))
	}
	return saved, nil
}

// BindingSyncResult summarizes a single binding sync pass.
type BindingSyncResult struct {
	NewAnnotations int
	ReceiptsAcked  int
	Errors         []string
}

// SyncDelivery ensures registration (if pending/failed) and runs single-flight sync on all bindings of deliveryID.
func (b *RyugiBridge) SyncDelivery(ctx context.Context, deliveryID string) (protocol.RyugiSyncResponse, error) {
	if b.isPaused() {
		return protocol.RyugiSyncResponse{}, ErrSyncPaused
	}

	dlv, _, _, _, err := b.store.GetDeliveryDetail(ctx, deliveryID)
	if err != nil {
		return protocol.RyugiSyncResponse{}, err
	}

	resp := protocol.RyugiSyncResponse{
		DeliveryID:     deliveryID,
		RyugiSyncState: dlv.RyugiSyncState,
		RyugiBindings:  []protocol.RyugiBindingView{},
	}

	if dlv.RyugiSyncState == protocol.RyugiSyncPending || dlv.RyugiSyncState == protocol.RyugiSyncFailed {
		if _, regErr := b.RegisterDelivery(ctx, deliveryID); regErr != nil {
			resp.Errors = append(resp.Errors, regErr.Error())
		}
	}

	records, err := b.store.ListRyugiBindings(ctx, deliveryID, false)
	if err != nil {
		return resp, err
	}

	for _, rec := range records {
		res, syncErr := b.SyncBinding(ctx, rec.BindingID)
		if syncErr != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %v", rec.ArtifactLocator, syncErr))
			continue
		}
		resp.BindingsSynced++
		resp.NewAnnotations += res.NewAnnotations
		resp.ReceiptsAcked += res.ReceiptsAcked
		for _, e := range res.Errors {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %s", rec.ArtifactLocator, e))
		}
	}

	updatedDlv, bindings, _, _, err := b.store.GetDeliveryDetail(ctx, deliveryID)
	if err == nil {
		resp.RyugiSyncState = updatedDlv.RyugiSyncState
		resp.RyugiBindings = bindings
	}
	return resp, nil
}

// SyncBinding runs a single-flight sync for one binding_id.
// Holds syncOpMu.RLock across the full read -> SQLite COMMIT -> ack section so gc-pause can drain
// all in-flight syncs cleanly. Preserves every distinct page receipt across pagination, fails fast
// on malformed thread payloads or human messages missing stable msg.ID, surfaces document changes
// errors verbatim, and tracks monotonic revisions for A -> B -> A state changes.
func (b *RyugiBridge) SyncBinding(ctx context.Context, bindingID string) (BindingSyncResult, error) {
	b.syncOpMu.RLock()
	defer b.syncOpMu.RUnlock()

	if b.isPaused() {
		return BindingSyncResult{}, ErrSyncPaused
	}

	gate := b.bindingGate(bindingID)
	gate.mu.Lock()
	defer gate.mu.Unlock()

	if b.isPaused() {
		return BindingSyncResult{}, ErrSyncPaused
	}

	records, err := b.store.ListRyugiBindings(ctx, "", false)
	if err != nil {
		return BindingSyncResult{}, err
	}
	var rec *store.RyugiBindingRecord
	for i := range records {
		if records[i].BindingID == bindingID {
			rec = &records[i]
			break
		}
	}
	if rec == nil {
		return BindingSyncResult{}, store.ErrNotFound
	}

	// 1. Fetch comment list (--sections all --detail) + paginate all pages collecting every page receipt
	sections, err := b.fetchAllCommentSections(ctx, rec.FrozenProjectDir, rec.ArtifactLocator)
	if err != nil {
		return BindingSyncResult{}, err
	}

	var result BindingSyncResult
	var incoming []store.IncomingRyugiAnnotation
	var threadsReceipts []string
	newHashes := make(map[string]store.StateHashEntry, len(rec.LastStateHashes))
	for k, v := range rec.LastStateHashes {
		newHashes[k] = v
	}

	// 1a. Threads (receipted): strict decoding — any malformed thread aborts commit & ack!
	if thSec, ok := sections["threads"]; ok {
		threadsReceipts = thSec.Receipts
		for idx, rawItem := range thSec.Items {
			var th ryugiThreadItem
			if err := json.Unmarshal(rawItem, &th); err != nil {
				return BindingSyncResult{}, fmt.Errorf("decode thread item #%d failed (aborting ack to preserve unread): %w", idx, err)
			}
			if th.ID == "" {
				return BindingSyncResult{}, fmt.Errorf("decode thread item #%d missing id (aborting ack)", idx)
			}
			if th.Status == "withdrawn" || th.Status == "draft" {
				continue
			}
			for msgIdx, msg := range th.Messages {
				if msg.Author != "human" {
					continue
				}
				text := strings.TrimSpace(msg.Text)
				if text == "" {
					continue
				}
				msgID := strings.TrimSpace(msg.ID)
				if msgID == "" {
					return BindingSyncResult{}, fmt.Errorf("thread %s human message #%d missing stable id (aborting ack to preserve unread)", th.ID, msgIdx)
				}
				anchor := protocol.AnnotationAnchor{
					Artifact: rec.ArtifactLocator,
					Section:  "threads",
					ThreadID: th.ID,
				}
				if blockID, ok := th.Anchor["blockId"].(string); ok {
					anchor.BlockID = blockID
				}
				if quote, ok := th.Anchor["quote"].(string); ok {
					anchor.Quote = quote
				}
				if elemID, ok := th.Anchor["elementId"].(string); ok {
					anchor.ElementID = elemID
				} else if boardID, ok := th.Anchor["boardId"].(string); ok {
					anchor.ElementID = boardID
				}
				if page, ok := th.Anchor["page"].(string); ok {
					anchor.Page = page
				}
				incoming = append(incoming, store.IncomingRyugiAnnotation{
					ExternalKey: fmt.Sprintf("ryugi:%s:threads:%s:%s", rec.BindingID, th.ID, msgID),
					Anchor:      anchor,
					Body:        text,
				})
			}
		}
	}

	// 1b. Unreceipted state sections: asks / groups / rounds
	// Tracks {hash, rev} per item so A -> B -> A emits v1, v2, v3 without collision.
	for _, secName := range []string{"asks", "groups", "rounds"} {
		sec, ok := sections[secName]
		if !ok {
			continue
		}
		for idx, rawItem := range sec.Items {
			var obj map[string]any
			if err := json.Unmarshal(rawItem, &obj); err != nil {
				return BindingSyncResult{}, fmt.Errorf("decode %s item #%d failed: %w", secName, idx, err)
			}
			stateStr, _ := obj["state"].(string)
			statusStr, _ := obj["status"].(string)
			effectiveState := stateStr
			if effectiveState == "" {
				effectiveState = statusStr
			}
			if !isSubmittedState(secName, effectiveState) {
				continue
			}

			itemID := firstStringField(obj, "id", "questionId", "groupId", "roundId")
			if itemID == "" {
				continue
			}
			contentHash := canonicalJSONHash(obj)
			hashKey := secName + ":" + itemID
			prev := newHashes[hashKey]
			if prev.Hash == contentHash {
				continue
			}
			nextRev := prev.Rev + 1
			newHashes[hashKey] = store.StateHashEntry{
				Hash: contentHash,
				Rev:  nextRev,
			}

			body := formatStateSectionBody(secName, itemID, obj)
			anchor := protocol.AnnotationAnchor{
				Artifact: rec.ArtifactLocator,
				Section:  secName,
				Verdict:  effectiveState,
			}
			if blockID, ok := obj["blockId"].(string); ok {
				anchor.BlockID = blockID
			}
			if elemID, ok := obj["boardId"].(string); ok {
				anchor.ElementID = elemID
			}
			incoming = append(incoming, store.IncomingRyugiAnnotation{
				ExternalKey: fmt.Sprintf("ryugi:%s:%s:%s:v%d", rec.BindingID, secName, itemID, nextRev),
				Anchor:      anchor,
				Body:        body,
			})
		}
	}

	// 2. Document / slides changes (receipted): surface any error verbatim in result.Errors (never swallow!)
	var changesReceipts []string
	if isDocumentLikeArtifact(rec.ArtifactLocator) {
		diffText, chReceipts, chErr := b.fetchDocumentChanges(ctx, rec.FrozenProjectDir, rec.ArtifactLocator)
		if chErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("changes sync failed: %v", chErr))
		} else {
			changesReceipts = chReceipts
			if strings.TrimSpace(diffText) != "" {
				diffHash := sha256Hex(diffText)
				incoming = append(incoming, store.IncomingRyugiAnnotation{
					ExternalKey: fmt.Sprintf("ryugi:%s:changes:%s", rec.BindingID, diffHash[:16]),
					Anchor: protocol.AnnotationAnchor{
						Artifact: rec.ArtifactLocator,
						Section:  "changes",
					},
					Body: "Document edits:\n" + strings.TrimSpace(diffText),
				})
			}
		}
	}

	// 3. COMMIT TO SQLITE FIRST (atomic BEGIN IMMEDIATE transaction)
	lastThreadRcpt := lastNonEmpty(threadsReceipts)
	lastChangesRcpt := lastNonEmpty(changesReceipts)
	inserted, emitted, err := b.store.ApplyRyugiSyncBatch(
		ctx,
		*rec,
		incoming,
		newHashes,
		lastThreadRcpt,
		lastChangesRcpt,
	)
	if err != nil {
		// NEVER ack any receipt if SQLite commit failed!
		return BindingSyncResult{}, fmt.Errorf("commit ryugi sync batch: %w", err)
	}
	result.NewAnnotations = inserted

	if b.broker != nil && len(emitted) > 0 {
		b.broker.Broadcast(emitted...)
	}

	// 4. ONLY AFTER SQLITE COMMIT SUCCEEDS, ack EVERY collected page receipt
	for _, rcpt := range threadsReceipts {
		if err := b.ackReceipt(ctx, "comment", rec.FrozenProjectDir, rec.ArtifactLocator, rcpt); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.ReceiptsAcked++
	}
	for _, rcpt := range changesReceipts {
		if err := b.ackReceipt(ctx, "changes", rec.FrozenProjectDir, rec.ArtifactLocator, rcpt); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.ReceiptsAcked++
	}

	return result, nil
}

func appendUniqueReceipt(list []string, rcpt string) []string {
	rcpt = strings.TrimSpace(rcpt)
	if rcpt == "" {
		return list
	}
	for _, existing := range list {
		if existing == rcpt {
			return list
		}
	}
	return append(list, rcpt)
}

func lastNonEmpty(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1]
}

func (b *RyugiBridge) fetchAllCommentSections(ctx context.Context, frozenDir, locator string) (map[string]ryugiSection, error) {
	out, runErr := b.runner.Run(ctx,
		"comment", "list",
		"--sections", "all",
		"--detail",
		"--project-dir", frozenDir,
		"--artifact", locator,
		"--format", "json",
	)
	env, parseErr := parseRyugiEnvelope(out)
	if parseErr != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, parseErr
	}
	if env.Outcome == "error" {
		if env.Error != nil {
			return nil, fmt.Errorf("ryugi comment list: %s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("ryugi comment list error")
	}

	var data ryugiCommentListData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("decode comment list data: %w", err)
	}
	if data.Sections == nil {
		data.Sections = map[string]ryugiSection{}
	}

	// Paginate any section with page.nextCursor using the same snapshot token, collecting EVERY page's receipt
	for secName, sec := range data.Sections {
		sec.Receipts = appendUniqueReceipt(sec.Receipts, sec.Receipt)
		cursor := ""
		if sec.Page.NextCursor != nil {
			cursor = strings.TrimSpace(*sec.Page.NextCursor)
		}
		snapshot := sec.Snapshot
		if snapshot == "" {
			snapshot = sec.Page.Snapshot
		}
		for cursor != "" {
			args := []string{
				"comment", "list",
				"--section", secName,
				"--detail",
				"--project-dir", frozenDir,
				"--artifact", locator,
				"--cursor", cursor,
				"--format", "json",
			}
			if snapshot != "" {
				args = append(args, "--snapshot", snapshot)
			}
			pOut, pRunErr := b.runner.Run(ctx, args...)
			pEnv, pParseErr := parseRyugiEnvelope(pOut)
			if pParseErr != nil {
				if pRunErr != nil {
					return nil, pRunErr
				}
				return nil, pParseErr
			}
			if pEnv.Outcome == "error" {
				if pEnv.Error != nil {
					return nil, fmt.Errorf("ryugi comment list page (%s): %s: %s", secName, pEnv.Error.Code, pEnv.Error.Message)
				}
				return nil, fmt.Errorf("ryugi comment list page (%s) failed", secName)
			}

			var pageData ryugiCommentListData
			if err := json.Unmarshal(pEnv.Data, &pageData); err != nil {
				return nil, fmt.Errorf("decode comment list page (%s): %w", secName, err)
			}

			var nextSec ryugiSection
			if s2, ok := pageData.Sections[secName]; ok {
				nextSec = s2
			} else {
				nextSec.Items = pageData.Items
				nextSec.Receipt = pageData.Receipt
				if pageData.Page != nil {
					nextSec.Page = *pageData.Page
				}
			}
			sec.Items = append(sec.Items, nextSec.Items...)
			sec.Receipts = appendUniqueReceipt(sec.Receipts, nextSec.Receipt)
			if nextSec.Page.NextCursor != nil && strings.TrimSpace(*nextSec.Page.NextCursor) != "" && *nextSec.Page.NextCursor != cursor {
				cursor = strings.TrimSpace(*nextSec.Page.NextCursor)
			} else {
				cursor = ""
			}
		}
		data.Sections[secName] = sec
	}
	return data.Sections, nil
}

func (b *RyugiBridge) fetchDocumentChanges(ctx context.Context, frozenDir, locator string) (string, []string, error) {
	out, runErr := b.runner.Run(ctx,
		"changes",
		"--detail",
		"--project-dir", frozenDir,
		"--artifact", locator,
		"--format", "json",
	)
	env, parseErr := parseRyugiEnvelope(out)
	if parseErr != nil {
		if runErr != nil {
			return "", nil, runErr
		}
		return "", nil, parseErr
	}
	if env.Outcome == "error" {
		if env.Error != nil {
			return "", nil, fmt.Errorf("ryugi changes: %s: %s", env.Error.Code, env.Error.Message)
		}
		return "", nil, fmt.Errorf("ryugi changes failed")
	}

	var data ryugiChangesData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", nil, fmt.Errorf("decode ryugi changes: %w", err)
	}
	var receipts []string
	receipts = appendUniqueReceipt(receipts, data.Receipt)
	if data.Empty {
		return "", receipts, nil
	}

	var diffBuf strings.Builder
	diffBuf.WriteString(data.Diff)

	cursor := ""
	snapshot := data.Snapshot
	if data.Page != nil && data.Page.NextCursor != nil {
		cursor = strings.TrimSpace(*data.Page.NextCursor)
		if snapshot == "" {
			snapshot = data.Page.Snapshot
		}
	}
	for cursor != "" {
		args := []string{
			"changes",
			"--detail",
			"--project-dir", frozenDir,
			"--artifact", locator,
			"--cursor", cursor,
			"--format", "json",
		}
		if snapshot != "" {
			args = append(args, "--snapshot", snapshot)
		}
		pOut, pRunErr := b.runner.Run(ctx, args...)
		pEnv, pParseErr := parseRyugiEnvelope(pOut)
		if pParseErr != nil {
			if pRunErr != nil {
				return "", nil, pRunErr
			}
			return "", nil, pParseErr
		}
		if pEnv.Outcome == "error" {
			if pEnv.Error != nil {
				return "", nil, fmt.Errorf("ryugi changes page: %s: %s", pEnv.Error.Code, pEnv.Error.Message)
			}
			return "", nil, fmt.Errorf("ryugi changes page failed")
		}
		var pageData ryugiChangesData
		if err := json.Unmarshal(pEnv.Data, &pageData); err != nil {
			return "", nil, err
		}
		if pageData.Diff != "" {
			if diffBuf.Len() > 0 {
				diffBuf.WriteString("\n")
			}
			diffBuf.WriteString(pageData.Diff)
		}
		receipts = appendUniqueReceipt(receipts, pageData.Receipt)
		if pageData.Page != nil && pageData.Page.NextCursor != nil && strings.TrimSpace(*pageData.Page.NextCursor) != "" && *pageData.Page.NextCursor != cursor {
			cursor = strings.TrimSpace(*pageData.Page.NextCursor)
		} else {
			cursor = ""
		}
	}
	return diffBuf.String(), receipts, nil
}

func (b *RyugiBridge) ackReceipt(ctx context.Context, kind, frozenDir, locator, receipt string) error {
	var args []string
	if kind == "changes" {
		args = []string{"changes", "ack", "--project-dir", frozenDir, "--artifact", locator, "--receipt", receipt, "--format", "json"}
	} else {
		args = []string{"comment", "ack", "--project-dir", frozenDir, "--artifact", locator, "--receipt", receipt, "--format", "json"}
	}
	out, runErr := b.runner.Run(ctx, args...)
	env, parseErr := parseRyugiEnvelope(out)
	if parseErr != nil {
		if runErr != nil {
			return fmt.Errorf("ryugi %s ack: %w", kind, runErr)
		}
		return fmt.Errorf("ryugi %s ack parse: %w", kind, parseErr)
	}
	if env.Outcome == "error" {
		if env.Error != nil {
			return fmt.Errorf("ryugi %s ack: %s: %s", kind, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("ryugi %s ack failed", kind)
	}
	return nil
}

// StartPoller starts the periodic active=1 binding poller until ctx is canceled.
// Respects IsPaused so backup maintenance windows freeze polling & acking cleanly.
func (b *RyugiBridge) StartPoller(ctx context.Context, wg *sync.WaitGroup) {
	if b.pollInterval <= 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(b.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if b.isPaused() {
					continue
				}
				records, err := b.store.ListRyugiBindings(ctx, "", true)
				if err != nil {
					continue
				}
				for _, rec := range records {
					if ctx.Err() != nil || b.isPaused() {
						break
					}
					_, _ = b.SyncBinding(ctx, rec.BindingID)
				}
			}
		}
	}()
}

func parseRyugiEnvelope(raw []byte) (ryugiCLIEnvelope, error) {
	var env ryugiCLIEnvelope
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return env, fmt.Errorf("empty ryugi CLI output")
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return env, fmt.Errorf("invalid ryugi CLI JSON: %w", err)
	}
	return env, nil
}

func isDocumentLikeArtifact(locator string) bool {
	return strings.HasPrefix(locator, "document/") || strings.HasPrefix(locator, "slides/")
}

func isSubmittedState(section, state string) bool {
	s := strings.ToLower(strings.TrimSpace(state))
	if s == "" || s == "draft" || s == "withdrawn" || s == "open" || s == "empty" || s == "pending" || s == "unanswered" {
		return false
	}
	switch section {
	case "asks":
		return s == "submitted" || s == "settled"
	case "groups":
		return s == "chosen" || s == "passed" || s == "rejected" || s == "submitted"
	case "rounds":
		return s == "submitted" || s == "completed"
	default:
		return false
	}
}

func firstStringField(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func formatStateSectionBody(section, itemID string, obj map[string]any) string {
	switch section {
	case "asks":
		choice, _ := obj["choice"].(string)
		text, _ := obj["text"].(string)
		state, _ := obj["state"].(string)
		val := choice
		if text != "" {
			if val != "" {
				val += " — " + text
			} else {
				val = text
			}
		}
		if val == "" {
			val = state
		}
		return fmt.Sprintf("[ask:%s] %s (%s)", itemID, val, state)
	case "groups":
		choice := firstStringField(obj, "choice", "boardId", "verdict", "state", "status")
		note := firstStringField(obj, "text", "note")
		if note != "" {
			return fmt.Sprintf("[group:%s] %s — %s", itemID, choice, note)
		}
		return fmt.Sprintf("[group:%s] %s", itemID, choice)
	case "rounds":
		state := firstStringField(obj, "state", "status")
		summary := firstStringField(obj, "summary", "text")
		if summary != "" {
			return fmt.Sprintf("[round:%s] %s — %s", itemID, state, summary)
		}
		return fmt.Sprintf("[round:%s] %s", itemID, state)
	default:
		return fmt.Sprintf("[%s:%s]", section, itemID)
	}
}

func canonicalJSONHash(v any) string {
	b, _ := json.Marshal(v)
	return sha256Hex(string(b))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SortedKeys returns deterministic map keys.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
