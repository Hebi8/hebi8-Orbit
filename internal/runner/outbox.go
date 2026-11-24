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
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// PendingDelivery persists fixed checkpoint metadata locally before pushing so network retries
// ALWAYS push the exact same deliveryId and headSha (never re-running commit-tree).
type PendingDelivery struct {
	DeliveryID     string
	AttemptID      string
	TaskID         string
	ProjectID      string
	RepoRoot       string
	MirrorBarePath string
	ReviewBaseSHA  string
	HeadSHA        string
	Summary        string
	Manifest       protocol.DeliveryManifest
	IsFinal        bool
	Pushed         bool
	Emitted        bool
	CreatedAt      int64
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
  delivery_id      TEXT PRIMARY KEY,
  attempt_id       TEXT NOT NULL,
  task_id          TEXT NOT NULL,
  project_id       TEXT NOT NULL,
  repo_root        TEXT NOT NULL,
  mirror_bare_path TEXT NOT NULL,
  review_base_sha  TEXT NOT NULL,
  head_sha         TEXT NOT NULL,
  summary          TEXT NOT NULL,
  manifest_json    TEXT NOT NULL,
  is_final         INTEGER NOT NULL DEFAULT 0,
  pushed           INTEGER NOT NULL DEFAULT 0,
  emitted          INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL
);
`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply outbox ddl: %w", err)
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

// LookupHandledApproval checks if decisionID has already been applied locally.
func (o *LocalOutbox) LookupHandledApproval(ctx context.Context, decisionID string) (protocol.HandledApprovalDecision, bool, error) {
	var hd protocol.HandledApprovalDecision
	var outcome string
	err := o.db.QueryRowContext(ctx, `
		SELECT approval_id, decision_id, outcome FROM handled_approvals WHERE decision_id = ?
	`, decisionID).Scan(&hd.ApprovalID, &hd.DecisionID, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return hd, false, nil
	}
	if err != nil {
		return hd, false, err
	}
	hd.Outcome = protocol.ApprovalState(outcome)
	return hd, true, nil
}

// ListRecentHandledApprovals returns recent locally handled decisions for runner.hello.
func (o *LocalOutbox) ListRecentHandledApprovals(ctx context.Context, limit int) ([]protocol.HandledApprovalDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := o.db.QueryContext(ctx, `
		SELECT approval_id, decision_id, outcome
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
		if err := rows.Scan(&hd.ApprovalID, &hd.DecisionID, &outcome); err != nil {
			return nil, err
		}
		hd.Outcome = protocol.ApprovalState(outcome)
		out = append(out, hd)
	}
	return out, rows.Err()
}

// SavePendingDelivery persists immutable delivery metadata prior to git push.
func (o *LocalOutbox) SavePendingDelivery(ctx context.Context, d PendingDelivery) error {
	manifestBytes, err := json.Marshal(d.Manifest)
	if err != nil {
		return err
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
			delivery_id, attempt_id, task_id, project_id, repo_root, mirror_bare_path,
			review_base_sha, head_sha, summary, manifest_json, is_final, pushed, emitted, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)
	`, d.DeliveryID, d.AttemptID, d.TaskID, d.ProjectID, d.RepoRoot, d.MirrorBarePath,
		d.ReviewBaseSHA, d.HeadSHA, d.Summary, string(manifestBytes), isFinal, d.CreatedAt)
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

// ListUnemittedDeliveries returns pending deliveries that haven't finished push + outbox emission yet.
func (o *LocalOutbox) ListUnemittedDeliveries(ctx context.Context) ([]PendingDelivery, error) {
	rows, err := o.db.QueryContext(ctx, `
		SELECT delivery_id, attempt_id, task_id, project_id, repo_root, mirror_bare_path,
		       review_base_sha, head_sha, summary, manifest_json, is_final, pushed, emitted, created_at
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
		var mJSON string
		var isFinal, pushed, emitted int
		if err := rows.Scan(
			&d.DeliveryID, &d.AttemptID, &d.TaskID, &d.ProjectID, &d.RepoRoot, &d.MirrorBarePath,
			&d.ReviewBaseSHA, &d.HeadSHA, &d.Summary, &mJSON, &isFinal, &pushed, &emitted, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		d.IsFinal = isFinal != 0
		d.Pushed = pushed != 0
		d.Emitted = emitted != 0
		_ = json.Unmarshal([]byte(mJSON), &d.Manifest)
		out = append(out, d)
	}
	return out, rows.Err()
}
