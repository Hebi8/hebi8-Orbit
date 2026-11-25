package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// PendingDelivery persists fixed checkpoint metadata locally before pushing so network retries
// ALWAYS push the exact same deliveryId and headSha (never re-running commit-tree).
type PendingDelivery struct {
	DeliveryID          string
	AttemptID           string
	LeaseToken          string
	TaskID              string
	ProjectID           string
	RepoRoot            string
	MirrorBarePath      string
	ReviewBaseSHA       string
	HeadSHA             string
	Summary             string
	Manifest            protocol.DeliveryManifest
	StagedBlobs         []gitops.PackagedBlob
	IsFinal             bool
	TerminalState       protocol.AttemptState
	TerminalErrorCode   string
	TerminalErrorDetail string
	Pushed              bool
	Emitted             bool
	CreatedAt           int64
}

// LocalOutbox is a single-writer WAL SQLite store (ncruces/go-sqlite3) for strictly ordered
// per-attempt outbox frames, handled approval receipts, and immutable pending deliveries.
type LocalOutbox struct {
	db *sql.DB
}

// OpenLocalOutbox initializes (or opens) the runner's local SQLite outbox database.
func OpenLocalOutbox(ctx context.Context, dbPath string) (*LocalOutbox, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve outbox db path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir outbox db dir: %w", err)
	}

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open outbox sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping outbox sqlite: %w", err)
	}

	ddl := `
CREATE TABLE IF NOT EXISTS attempt_cursors (
  attempt_id  TEXT PRIMARY KEY,
  lease_token TEXT NOT NULL,
  next_seq    INTEGER NOT NULL DEFAULT 1,
  acked_seq   INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox_items (
  attempt_id   TEXT NOT NULL,
  outbox_seq   INTEGER NOT NULL,
  event_id     TEXT NOT NULL UNIQUE,
  frame_type   TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  ts           INTEGER NOT NULL,
  lease_token  TEXT NOT NULL,
  PRIMARY KEY (attempt_id, outbox_seq)
);

CREATE INDEX IF NOT EXISTS idx_outbox_attempt_seq ON outbox_items(attempt_id, outbox_seq ASC);

CREATE TABLE IF NOT EXISTS handled_approvals (
  decision_id TEXT PRIMARY KEY,
  approval_id TEXT NOT NULL,
  attempt_id  TEXT NOT NULL,
  outcome     TEXT NOT NULL,
  option_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_handled_approvals_time ON handled_approvals(created_at DESC);

CREATE TABLE IF NOT EXISTS pending_deliveries (
  delivery_id           TEXT PRIMARY KEY,
  attempt_id            TEXT NOT NULL,
  lease_token           TEXT NOT NULL DEFAULT '',
  task_id               TEXT NOT NULL,
  project_id            TEXT NOT NULL,
  repo_root             TEXT NOT NULL,
  mirror_bare_path      TEXT NOT NULL,
  review_base_sha       TEXT NOT NULL,
  head_sha              TEXT NOT NULL,
  summary               TEXT NOT NULL,
  manifest_json         TEXT NOT NULL,
  blobs_json            TEXT NOT NULL DEFAULT '[]',
  is_final              INTEGER NOT NULL DEFAULT 0,
  terminal_state        TEXT NOT NULL DEFAULT '',
  terminal_error_code   TEXT NOT NULL DEFAULT '',
  terminal_error_detail TEXT NOT NULL DEFAULT '',
  pushed                INTEGER NOT NULL DEFAULT 0,
  emitted               INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cancelled_attempts (
  attempt_id  TEXT PRIMARY KEY,
  lease_token TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply outbox ddl: %w", err)
	}
	// Additive upgrade for existing runner-outbox sqlite files
	for _, alter := range []string{
		`ALTER TABLE pending_deliveries ADD COLUMN lease_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_deliveries ADD COLUMN blobs_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE pending_deliveries ADD COLUMN terminal_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_deliveries ADD COLUMN terminal_error_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_deliveries ADD COLUMN terminal_error_detail TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, alter); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate outbox: %w", err)
		}
	}
	return &LocalOutbox{db: db}, nil
}

func (o *LocalOutbox) Close() error {
	if o == nil || o.db == nil {
		return nil
	}
	return o.db.Close()
}

// Enqueue assigns the next strictly contiguous outboxSeq (1, 2, 3...) for attemptID and persists it.
func (o *LocalOutbox) Enqueue(
	ctx context.Context,
	attemptID, leaseToken, eventID string,
	frameType protocol.OutboxFrameType,
	payload map[string]any,
) (protocol.OutboxItem, error) {
	var item protocol.OutboxItem
	now := time.Now().UnixMilli()
	if payload == nil {
		payload = map[string]any{}
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return item, err
	}

	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return item, err
	}
	defer tx.Rollback()

	var nextSeq int64
	err = tx.QueryRowContext(ctx, `SELECT next_seq FROM attempt_cursors WHERE attempt_id = ?`, attemptID).Scan(&nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		nextSeq = 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_cursors (attempt_id, lease_token, next_seq, acked_seq, updated_at)
			VALUES (?, ?, 2, 0, ?)
		`, attemptID, leaseToken, now); err != nil {
			return item, err
		}
	} else if err != nil {
		return item, err
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempt_cursors SET next_seq = ?, lease_token = ?, updated_at = ? WHERE attempt_id = ?
		`, nextSeq+1, leaseToken, now, attemptID); err != nil {
			return item, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_items (attempt_id, outbox_seq, event_id, frame_type, payload_json, ts, lease_token)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, attemptID, nextSeq, eventID, string(frameType), string(payloadBytes), now, leaseToken); err != nil {
		return item, fmt.Errorf("insert outbox item: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return item, err
	}

	item = protocol.OutboxItem{
		OutboxSeq: nextSeq,
		EventID:   eventID,
		FrameType: frameType,
		Payload:   payload,
		TS:        now,
	}
	return item, nil
}

// AttemptOutboxSummary identifies an attempt that currently has unacked items waiting to flush.
type AttemptOutboxSummary struct {
	AttemptID  string
	LeaseToken string
	AckedSeq   int64
}

// ListAttemptsWithUnacked returns all attempts that still have persisted outbox rows.
func (o *LocalOutbox) ListAttemptsWithUnacked(ctx context.Context) ([]AttemptOutboxSummary, error) {
	rows, err := o.db.QueryContext(ctx, `
		SELECT c.attempt_id, c.lease_token, c.acked_seq
		FROM attempt_cursors c
		WHERE EXISTS (
			SELECT 1 FROM outbox_items i WHERE i.attempt_id = c.attempt_id AND i.outbox_seq > c.acked_seq
		)
		ORDER BY c.updated_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttemptOutboxSummary
	for rows.Next() {
		var s AttemptOutboxSummary
		if err := rows.Scan(&s.AttemptID, &s.LeaseToken, &s.AckedSeq); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReadUnacked returns up to limit ordered outbox items with outbox_seq > afterSeq.
func (o *LocalOutbox) ReadUnacked(ctx context.Context, attemptID string, afterSeq int64, limit int) (string, []protocol.OutboxItem, error) {
	if limit <= 0 {
		limit = 50
	}
	var leaseToken string
	err := o.db.QueryRowContext(ctx, `SELECT lease_token FROM attempt_cursors WHERE attempt_id = ?`, attemptID).Scan(&leaseToken)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}

	rows, err := o.db.QueryContext(ctx, `
		SELECT outbox_seq, event_id, frame_type, payload_json, ts
		FROM outbox_items
		WHERE attempt_id = ? AND outbox_seq > ?
		ORDER BY outbox_seq ASC
		LIMIT ?
	`, attemptID, afterSeq, limit)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()

	var items []protocol.OutboxItem
	for rows.Next() {
		var it protocol.OutboxItem
		var fType, payloadJSON string
		if err := rows.Scan(&it.OutboxSeq, &it.EventID, &fType, &payloadJSON, &it.TS); err != nil {
			return "", nil, err
		}
		it.FrameType = protocol.OutboxFrameType(fType)
		it.Payload = map[string]any{}
		_ = json.Unmarshal([]byte(payloadJSON), &it.Payload)
		items = append(items, it)
	}
	return leaseToken, items, rows.Err()
}

// AckUpTo advances the local acked_seq watermark and trims all confirmed items <= ackedSeq.
func (o *LocalOutbox) AckUpTo(ctx context.Context, attemptID string, ackedSeq int64) error {
	now := time.Now().UnixMilli()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE attempt_cursors SET acked_seq = MAX(acked_seq, ?), updated_at = ? WHERE attempt_id = ?
	`, ackedSeq, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM outbox_items WHERE attempt_id = ? AND outbox_seq <= ?
	`, attemptID, ackedSeq); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordHandledApproval persists a locally applied approval decision for decisionId idempotency
// and runner.hello reconnect reconciliation.
func (o *LocalOutbox) RecordHandledApproval(ctx context.Context, decisionID, approvalID, attemptID string, outcome protocol.ApprovalState, optionID string) error {
	now := time.Now().UnixMilli()
	_, err := o.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO handled_approvals (decision_id, approval_id, attempt_id, outcome, option_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, decisionID, approvalID, attemptID, string(outcome), optionID, now)
	return err
}

// HasCursor reports whether attemptID has ever written to this Runner's local outbox (B10).
func (o *LocalOutbox) HasCursor(ctx context.Context, attemptID string) (bool, error) {
	var exists string
	err := o.db.QueryRowContext(ctx, `SELECT attempt_id FROM attempt_cursors WHERE attempt_id = ?`, attemptID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HasUnackedItems reports whether attemptID still has any unacknowledged outbox rows (B3, S1).
func (o *LocalOutbox) HasUnackedItems(ctx context.Context, attemptID string) (bool, error) {
	var cnt int
	err := o.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM outbox_items i
		JOIN attempt_cursors c ON c.attempt_id = i.attempt_id
		WHERE i.attempt_id = ? AND i.outbox_seq > c.acked_seq
	`, attemptID).Scan(&cnt)
	return cnt > 0, err
}

// HasUnemittedDeliveriesForAttempt reports whether attemptID still has pending deliveries waiting to emit.
func (o *LocalOutbox) HasUnemittedDeliveriesForAttempt(ctx context.Context, attemptID string) (bool, error) {
	var cnt int
	err := o.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_deliveries WHERE attempt_id = ? AND emitted = 0
	`, attemptID).Scan(&cnt)
	return cnt > 0, err
}

// LookupHandledApproval checks if decisionID has already been applied locally (B8 includes AttemptID).
func (o *LocalOutbox) LookupHandledApproval(ctx context.Context, decisionID string) (protocol.HandledApprovalDecision, bool, error) {
	var hd protocol.HandledApprovalDecision
	var outcome string
	err := o.db.QueryRowContext(ctx, `
		SELECT approval_id, attempt_id, decision_id, outcome FROM handled_approvals WHERE decision_id = ?
	`, decisionID).Scan(&hd.ApprovalID, &hd.AttemptID, &hd.DecisionID, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return hd, false, nil
	}
	if err != nil {
		return hd, false, err
	}
	hd.Outcome = protocol.ApprovalState(outcome)
	return hd, true, nil
}

// ListRecentHandledApprovals returns recent locally handled decisions for runner.hello (B8 includes AttemptID).
func (o *LocalOutbox) ListRecentHandledApprovals(ctx context.Context, limit int) ([]protocol.HandledApprovalDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := o.db.QueryContext(ctx, `
		SELECT approval_id, attempt_id, decision_id, outcome
		FROM handled_approvals
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.HandledApprovalDecision{}
	for rows.Next() {
		var hd protocol.HandledApprovalDecision
		var outcome string
		if err := rows.Scan(&hd.ApprovalID, &hd.AttemptID, &hd.DecisionID, &outcome); err != nil {
			return nil, err
		}
		hd.Outcome = protocol.ApprovalState(outcome)
		out = append(out, hd)
	}
	return out, rows.Err()
}

// SavePendingDelivery persists immutable delivery metadata prior to git push / blob upload.
func (o *LocalOutbox) SavePendingDelivery(ctx context.Context, d PendingDelivery) error {
	manifestBytes, err := json.Marshal(d.Manifest)
	if err != nil {
		return err
	}
	blobsBytes, err := json.Marshal(d.StagedBlobs)
	if err != nil {
		return err
	}
	if d.StagedBlobs == nil {
		blobsBytes = []byte("[]")
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().UnixMilli()
	}
	isFinal := 0
	if d.IsFinal {
		isFinal = 1
	}
	_, err = o.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO pending_deliveries (
			delivery_id, attempt_id, lease_token, task_id, project_id, repo_root, mirror_bare_path,
			review_base_sha, head_sha, summary, manifest_json, blobs_json, is_final,
			terminal_state, terminal_error_code, terminal_error_detail, pushed, emitted, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)
	`, d.DeliveryID, d.AttemptID, d.LeaseToken, d.TaskID, d.ProjectID, d.RepoRoot, d.MirrorBarePath,
		d.ReviewBaseSHA, d.HeadSHA, d.Summary, string(manifestBytes), string(blobsBytes), isFinal,
		string(d.TerminalState), d.TerminalErrorCode, d.TerminalErrorDetail, d.CreatedAt)
	return err
}

// GetPendingDelivery fetches one locally persisted pending delivery by deliveryID (B13).
func (o *LocalOutbox) GetPendingDelivery(ctx context.Context, deliveryID string) (PendingDelivery, bool, error) {
	var d PendingDelivery
	var mJSON, bJSON, termState string
	var isFinal, pushed, emitted int
	err := o.db.QueryRowContext(ctx, `
		SELECT delivery_id, attempt_id, lease_token, task_id, project_id, repo_root, mirror_bare_path,
		       review_base_sha, head_sha, summary, manifest_json, blobs_json, is_final,
		       terminal_state, terminal_error_code, terminal_error_detail, pushed, emitted, created_at
		FROM pending_deliveries
		WHERE delivery_id = ?
	`, deliveryID).Scan(
		&d.DeliveryID, &d.AttemptID, &d.LeaseToken, &d.TaskID, &d.ProjectID, &d.RepoRoot, &d.MirrorBarePath,
		&d.ReviewBaseSHA, &d.HeadSHA, &d.Summary, &mJSON, &bJSON, &isFinal,
		&termState, &d.TerminalErrorCode, &d.TerminalErrorDetail, &pushed, &emitted, &d.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.IsFinal = isFinal != 0
	d.TerminalState = protocol.AttemptState(termState)
	d.Pushed = pushed != 0
	d.Emitted = emitted != 0
	_ = json.Unmarshal([]byte(mJSON), &d.Manifest)
	if bJSON != "" {
		_ = json.Unmarshal([]byte(bJSON), &d.StagedBlobs)
	}
	return d, true, nil
}

// UpdatePendingDeliveryFinal upgrades an unemitted delivery's is_final flag when retried with --final.
func (o *LocalOutbox) UpdatePendingDeliveryFinal(ctx context.Context, deliveryID string, isFinal bool) error {
	v := 0
	if isFinal {
		v = 1
	}
	_, err := o.db.ExecContext(ctx, `UPDATE pending_deliveries SET is_final = MAX(is_final, ?) WHERE delivery_id = ?`, v, deliveryID)
	return err
}

func (o *LocalOutbox) MarkDeliveryPushed(ctx context.Context, deliveryID string) error {
	_, err := o.db.ExecContext(ctx, `UPDATE pending_deliveries SET pushed = 1 WHERE delivery_id = ?`, deliveryID)
	return err
}

func (o *LocalOutbox) MarkDeliveryEmitted(ctx context.Context, deliveryID string) error {
	_, err := o.db.ExecContext(ctx, `UPDATE pending_deliveries SET emitted = 1 WHERE delivery_id = ?`, deliveryID)
	return err
}

// EmitPendingDeliveryAtomically writes delivery_published + optional terminal state frame + emitted=1
// inside ONE local SQLite transaction so crashes can never leave emitted=0 or split final state (B13).
func (o *LocalOutbox) EmitPendingDeliveryAtomically(
	ctx context.Context,
	deliveryID, attemptID, leaseToken, dlvEventID string,
	dlvPayload map[string]any,
	emitFinal bool,
	finalEventID string,
	finalPayload map[string]any,
) (bool, error) {
	now := time.Now().UnixMilli()
	dlvBytes, err := json.Marshal(dlvPayload)
	if err != nil {
		return false, err
	}
	var finalBytes []byte
	if emitFinal {
		finalBytes, err = json.Marshal(finalPayload)
		if err != nil {
			return false, err
		}
	}

	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var alreadyEmitted int
	if err := tx.QueryRowContext(ctx, `SELECT emitted FROM pending_deliveries WHERE delivery_id = ?`, deliveryID).Scan(&alreadyEmitted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if alreadyEmitted != 0 {
		return false, nil
	}

	var nextSeq int64
	addCount := int64(1)
	if emitFinal {
		addCount = 2
	}
	err = tx.QueryRowContext(ctx, `SELECT next_seq FROM attempt_cursors WHERE attempt_id = ?`, attemptID).Scan(&nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		nextSeq = 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_cursors (attempt_id, lease_token, next_seq, acked_seq, updated_at)
			VALUES (?, ?, ?, 0, ?)
		`, attemptID, leaseToken, 1+addCount, now); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempt_cursors SET next_seq = ?, lease_token = ?, updated_at = ? WHERE attempt_id = ?
		`, nextSeq+addCount, leaseToken, now, attemptID); err != nil {
			return false, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_items (attempt_id, outbox_seq, event_id, frame_type, payload_json, ts, lease_token)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, attemptID, nextSeq, dlvEventID, string(protocol.OutboxFrameDeliveryPublished), string(dlvBytes), now, leaseToken); err != nil {
		return false, fmt.Errorf("insert atomic delivery_published: %w", err)
	}

	if emitFinal {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox_items (attempt_id, outbox_seq, event_id, frame_type, payload_json, ts, lease_token)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, attemptID, nextSeq+1, finalEventID, string(protocol.OutboxFrameState), string(finalBytes), now, leaseToken); err != nil {
			return false, fmt.Errorf("insert atomic final state: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE pending_deliveries SET pushed = 1, emitted = 1 WHERE delivery_id = ?`, deliveryID); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ListUnemittedDeliveries returns pending deliveries that haven't finished push + outbox emission yet.
func (o *LocalOutbox) ListUnemittedDeliveries(ctx context.Context) ([]PendingDelivery, error) {
	rows, err := o.db.QueryContext(ctx, `
		SELECT delivery_id, attempt_id, lease_token, task_id, project_id, repo_root, mirror_bare_path,
		       review_base_sha, head_sha, summary, manifest_json, blobs_json, is_final,
		       terminal_state, terminal_error_code, terminal_error_detail, pushed, emitted, created_at
		FROM pending_deliveries
		WHERE emitted = 0
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingDelivery
	for rows.Next() {
		var d PendingDelivery
		var mJSON, bJSON, termState string
		var isFinal, pushed, emitted int
		if err := rows.Scan(
			&d.DeliveryID, &d.AttemptID, &d.LeaseToken, &d.TaskID, &d.ProjectID, &d.RepoRoot, &d.MirrorBarePath,
			&d.ReviewBaseSHA, &d.HeadSHA, &d.Summary, &mJSON, &bJSON, &isFinal,
			&termState, &d.TerminalErrorCode, &d.TerminalErrorDetail, &pushed, &emitted, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		d.IsFinal = isFinal != 0
		d.TerminalState = protocol.AttemptState(termState)
		d.Pushed = pushed != 0
		d.Emitted = emitted != 0
		_ = json.Unmarshal([]byte(mJSON), &d.Manifest)
		if bJSON != "" {
			_ = json.Unmarshal([]byte(bJSON), &d.StagedBlobs)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordCancelledAttempt durably remembers that attemptID was cancelled before or during dispatch (B10).
func (o *LocalOutbox) RecordCancelledAttempt(ctx context.Context, attemptID, leaseToken string) error {
	if strings.TrimSpace(attemptID) == "" {
		return nil
	}
	_, err := o.db.ExecContext(ctx, `
		INSERT INTO cancelled_attempts (attempt_id, lease_token, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET lease_token = COALESCE(NULLIF(excluded.lease_token, ''), cancelled_attempts.lease_token)
	`, attemptID, leaseToken, time.Now().UnixMilli())
	return err
}

// IsAttemptCancelled reports whether attemptID was already cancelled on this runner (B10).
func (o *LocalOutbox) IsAttemptCancelled(ctx context.Context, attemptID string) (bool, error) {
	if strings.TrimSpace(attemptID) == "" {
		return false, nil
	}
	var cnt int
	if err := o.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cancelled_attempts WHERE attempt_id = ?`, attemptID).Scan(&cnt); err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// HasUnemittedFinalDelivery reports whether attemptID has a locally persisted final delivery
// still awaiting blob upload / mirror push retry (B13).
func (o *LocalOutbox) HasUnemittedFinalDelivery(ctx context.Context, attemptID string) (bool, error) {
	var cnt int
	if err := o.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_deliveries
		WHERE attempt_id = ? AND is_final = 1 AND emitted = 0
	`, attemptID).Scan(&cnt); err != nil {
		return false, err
	}
	return cnt > 0, nil
}
