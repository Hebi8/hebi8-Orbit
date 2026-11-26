package store

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

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

var (
	ErrNotFound             = errors.New("not found")
	ErrConflict             = errors.New("conflict")
	ErrOwnershipMismatch    = errors.New("ownership mismatch")
	ErrBlobMismatch         = errors.New("blob missing or size mismatch")
	ErrDeliveryVerifyFailed = errors.New("delivery verification failed")
	ErrForbidden            = errors.New("forbidden")
	ErrVerificationRequired = errors.New("verification required")
	ErrInvalidInput         = errors.New("invalid input")
	ErrTaskActive             = errors.New("task has an active attempt")
	ErrTaskArchived           = errors.New("task is archived")
	ErrDeliveryCleanupPending = errors.New("delivery snapshot cleanup pending")
	ErrMaxSubsessionDepth     = errors.New("maximum sub-session depth exceeded")
	ErrPANoSubsessions        = errors.New("PA/Hebi8 sessions cannot spawn sub-sessions")
	ErrCheckpointLocked       = errors.New("delivery review checkpoint is locked after a subsequent message")
	ErrRepoIdentityConflict   = errors.New("repo identity conflict")
)

// Store manages the single-writer SQLite WAL pool and a separate read-only query pool.
type Store struct {
	path string
	rw   *sql.DB
	ro   *sql.DB
}

// Open initializes (or opens) the SQLite database at dbPath with WAL mode,
// foreign keys, busy timeout, single-writer BEGIN IMMEDIATE, and a read-only pool.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db parent dir: %w", err)
	}

	rwDSN := buildFileURI(absPath, map[string]string{
		"_txlock": "immediate",
	}, []string{
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"temp_store(MEMORY)",
		"cache_size(-32000)",
	})

	rw, err := sql.Open("sqlite3", rwDSN)
	if err != nil {
		return nil, fmt.Errorf("open write sqlite pool: %w", err)
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)

	if err := rw.PingContext(ctx); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("ping write sqlite pool: %w", err)
	}

	// Ensure PRAGMAs are applied explicitly and apply schema migrations before opening RO pool.
	if _, err := rw.ExecContext(ctx, `
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA foreign_keys = ON;
		PRAGMA busy_timeout = 5000;
		PRAGMA temp_store = MEMORY;
		PRAGMA cache_size = -32000;
	`); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("apply write pragmas: %w", err)
	}

	if err := backupBeforeV5Migration(ctx, rw, absPath); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("backup before v5 migration: %w", err)
	}

	if err := backupBeforeV6Migration(ctx, rw, absPath); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("backup before v6 migration: %w", err)
	}

	if err := backupBeforeV7Migration(ctx, rw, absPath); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("backup before v7 migration: %w", err)
	}

	if err := repairDanglingAttemptForeignKeys(ctx, rw, absPath); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("repair dangling attempt foreign keys: %w", err)
	}

	if err := applySchema(ctx, rw); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	if err := applyMigrationV7Atomic(ctx, rw); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("apply v7 migration: %w", err)
	}

	roDSN := buildFileURI(absPath, map[string]string{
		"mode": "ro",
	}, []string{
		"busy_timeout(5000)",
		"foreign_keys(ON)",
		"temp_store(MEMORY)",
		"cache_size(-32000)",
	})

	ro, err := sql.Open("sqlite3", roDSN)
	if err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("open read sqlite pool: %w", err)
	}
	ro.SetMaxOpenConns(4)
	ro.SetMaxIdleConns(2)

	if err := ro.PingContext(ctx); err != nil {
		_ = ro.Close()
		_ = rw.Close()
		return nil, fmt.Errorf("ping read sqlite pool: %w", err)
	}

	return &Store{
		path: absPath,
		rw:   rw,
		ro:   ro,
	}, nil
}

func buildFileURI(absPath string, params map[string]string, pragmas []string) string {
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(absPath),
	}
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, BuildSchemaDDL()); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (1, ?)
	`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO pa_state (pa_id, last_read_event_seq, focus_pointers_json, updated_at)
		VALUES ('default', 0, '[]', ?)
	`, now); err != nil {
		return err
	}

	// Smooth versioned migration v2 (Phase 1): safe on both fresh DBs and existing Phase 0 DBs
	var hasV2 int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&hasV2); err != nil {
		return err
	}
	if hasV2 == 0 {
		if _, err := tx.ExecContext(ctx, BuildMigrationV2DDL()); err != nil {
			return fmt.Errorf("apply v2 ddl: %w", err)
		}
		if err := ensureColumnTx(ctx, tx, "pa_state", "active_pa_thread_id", "TEXT REFERENCES pa_threads(pa_thread_id)"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "task_pending_inputs", "position", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "task_pending_inputs", "source", "TEXT NOT NULL DEFAULT 'human'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "task_pending_inputs", "body", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "task_pending_inputs", "updated_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "agent_sessions", "locked", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "runner_instance_id", "TEXT"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_pending_inputs SET updated_at = created_at WHERE updated_at = 0;
			CREATE INDEX IF NOT EXISTS idx_task_pending_inputs_order
				ON task_pending_inputs(task_id, position ASC, created_at ASC)
				WHERE state = 'queued';
		`); err != nil {
			return fmt.Errorf("backfill task_pending_inputs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, applied_at) VALUES (2, ?)
		`, now); err != nil {
			return err
		}
	}

	// Smooth versioned migration v3 (Phase 1 round-1 hardening): safe on fresh, v1, and v2 DBs.
	var hasV3 int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 3`).Scan(&hasV3); err != nil {
		return err
	}
	if hasV3 == 0 {
		if _, err := tx.ExecContext(ctx, BuildMigrationV3DDL()); err != nil {
			return fmt.Errorf("apply v3 ddl: %w", err)
		}
		if err := ensureColumnTx(ctx, tx, "approvals", "delivery_attempted", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		// B6: Legacy v1/v2 approved_pending_delivery rows lacked delivery_attempted tracking;
		// conservatively backfill them to 1 so an upgrade + runner restart enters uncertain/needs_verification
		// rather than silently marking potentially-executed approvals as expired.
		if _, err := tx.ExecContext(ctx, `
			UPDATE approvals SET delivery_attempted = 1
			WHERE state = 'approved_pending_delivery' AND delivery_attempted = 0
		`); err != nil {
			return fmt.Errorf("backfill legacy approval delivery_attempted: %w", err)
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "cancel_requested", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "cancel_reason_kind", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "steer_input_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "desired_config_json", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "required_resources_json", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "anchor_json", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "adapters_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
		// S8: Backfill empty body on legacy Phase 0 task_pending_inputs from payload_json.prompt
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_pending_inputs
			SET body = COALESCE(NULLIF(json_extract(payload_json, '$.prompt'), ''), '')
			WHERE body = '' AND json_valid(payload_json)
		`); err != nil {
			return fmt.Errorf("backfill legacy pending input body: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, applied_at) VALUES (3, ?)
		`, now); err != nil {
			return err
		}
	}

	// Smooth versioned migration v4 (Phase 1 round-2 hardening):
	// - B9: durable tasks.pending_queue_drain + pending_steer_input_id across Hub restarts
	// - NEW-1: server-only task_pending_inputs.route_json separated from user-editable payload_json
	// - NEW-3: pa_action_receipts.param_hash for atomic PA write idempotency verification
	var hasV4 int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 4`).Scan(&hasV4); err != nil {
		return err
	}
	if hasV4 == 0 {
		if err := ensureColumnTx(ctx, tx, "tasks", "pending_queue_drain", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "tasks", "pending_steer_input_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "task_pending_inputs", "route_json", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "pa_action_receipts", "param_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, applied_at) VALUES (4, ?)
		`, now); err != nil {
			return err
		}
	}

	// Smooth versioned migration v5 (onboarding, dynamic pairing, credentials, per-machine agents, UI projects).
	var hasV5 int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&hasV5); err != nil {
		return err
	}
	if hasV5 == 0 {
		if _, err := tx.ExecContext(ctx, BuildMigrationV5DDL()); err != nil {
			return fmt.Errorf("apply v5 ddl: %w", err)
		}
		if err := ensureColumnTx(ctx, tx, "machines", "agents_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "display_alias", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "runner_caps_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "projects", "source", "TEXT NOT NULL DEFAULT 'yaml'"); err != nil {
			return err
		}
		// One-time existing-environment detection: if this database already has tasks,
		// non-scratch projects, or >=2 machines, mark onboarding done so existing setups
		// are never intercepted after upgrade.
		var taskCount, projCount, machCount int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&taskCount)
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE project_id != ? AND archived = 0`, protocol.ScratchProjectID).Scan(&projCount)
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM machines`).Scan(&machCount)
		if taskCount > 0 || projCount > 0 || machCount >= 2 {
			val, _ := json.Marshal(map[string]any{
				"done":   true,
				"reason": "migrated",
				"at":     now,
			})
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO hub_settings (key, value_json, updated_at)
				VALUES ('onboarding_done', ?, ?)
			`, string(val), now); err != nil {
				return fmt.Errorf("mark migrated onboarding_done: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, applied_at) VALUES (5, ?)
		`, now); err != nil {
			return err
		}
	}

	// Smooth versioned migration v6 (v3.2 3-level project model, device roles, duty priority, plain-folder changes).
	var hasV6 int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&hasV6); err != nil {
		return err
	}
	if hasV6 == 0 {
		if _, err := tx.ExecContext(ctx, BuildMigrationV6DDL()); err != nil {
			return fmt.Errorf("apply v6 ddl: %w", err)
		}
		if err := ensureColumnTx(ctx, tx, "projects", "system_prompt", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "projects", "shared_env_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "role", "TEXT NOT NULL DEFAULT 'full'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "os_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "machines", "allowed_roots_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "registry_machines", "role", "TEXT NOT NULL DEFAULT 'full'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "pairing_codes", "role", "TEXT NOT NULL DEFAULT 'full'"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "tasks", "part_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "tasks", "location_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "part_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "location_id", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "isolation", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumnTx(ctx, tx, "attempts", "folder_changes_json", "TEXT"); err != nil {
			return err
		}
		// Backfill role for existing ssh_outbound targets as restricted workers.
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines SET role = 'worker' WHERE transport_mode = 'ssh_outbound' AND role = 'full'
		`); err != nil {
			return fmt.Errorf("backfill ssh_outbound machine role: %w", err)
		}
		if err := backfillV6ProjectsTx(ctx, tx, now); err != nil {
			return fmt.Errorf("backfill v6 projects: %w", err)
		}
		if err := ensureV6NetworkDefaultsTx(ctx, tx, now); err != nil {
			return fmt.Errorf("ensure v6 network defaults: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, applied_at) VALUES (6, ?)
		`, now); err != nil {
			return err
		}
	}

	// Idempotently ensure pending_delivery_cleanups and its BEFORE INSERT guard trigger exist on both fresh and existing DBs.
	// Note: child_parent_notifications is created strictly inside applyMigrationV7Atomic so v6->v7 upgrades roll back cleanly.
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS pending_delivery_cleanups (
			delivery_id TEXT PRIMARY KEY,
			created_at  INTEGER NOT NULL
		);
		CREATE TRIGGER IF NOT EXISTS trg_deliveries_block_pending_cleanup
		BEFORE INSERT ON deliveries
		WHEN EXISTS (SELECT 1 FROM pending_delivery_cleanups WHERE delivery_id = NEW.delivery_id)
		BEGIN
			SELECT RAISE(ABORT, 'ERR_DELIVERY_CLEANUP_PENDING');
		END;
	`); err != nil {
		return fmt.Errorf("ensure pending_delivery_cleanups table/trigger: %w", err)
	}

	// Idempotently ensure the built-in virtual _scratch project exists on every startup.
	if err := ensureScratchProjectTx(ctx, tx, now); err != nil {
		return err
	}

	return tx.Commit()
}

func ensureColumnTx(ctx context.Context, tx *sql.Tx, table, column, colDef string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if !found {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, colDef)); err != nil {
			return fmt.Errorf("alter table %s add column %s: %w", table, column, err)
		}
	}
	return nil
}

func ensureScratchProjectTx(ctx context.Context, tx *sql.Tx, now int64) error {
	virtuals := []struct {
		id      string
		name    string
		repoID  string
		defMode string
	}{
		{
			id:     protocol.ScratchProjectID,
			name:   protocol.ScratchProjectDisplayName,
			repoID: protocol.ScratchProjectRepoIdentity,
		},
		{
			id:      protocol.PAProjectID,
			name:    protocol.PAProjectDisplayName,
			repoID:  protocol.PAProjectRepoIdentity,
			defMode: "auto",
		},
	}
	for _, vp := range virtuals {
		cfg := protocol.ProjectConfig{
			DisplayName:       vp.name,
			RepoIdentity:      vp.repoID,
			DefaultBranch:     "main",
			DefaultCapability: "primary-dev",
			DefaultAdapter:    protocol.AdapterAgy,
			IDEScheme:         protocol.IDECursor,
			Anchors:           map[string]protocol.ProjectAnchorConfig{},
		}
		if vp.defMode != "" {
			cfg.DefaultModelConfig.Mode = vp.defMode
		}
		cfgJSON, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO projects (
				project_id, display_name, repo_identity, default_branch,
				mirror_bare_path, config_json, archived, updated_at
			) VALUES (?, ?, ?, 'main', '', ?, 0, ?)
			ON CONFLICT(project_id) DO UPDATE SET
				display_name = excluded.display_name,
				repo_identity = excluded.repo_identity,
				archived = 0,
				updated_at = excluded.updated_at
		`, vp.id, vp.name, vp.repoID, string(cfgJSON), now)
		if err != nil {
			return fmt.Errorf("ensure virtual project %s: %w", vp.id, err)
		}
	}
	return nil
}

// Close closes both read-only and read-write SQLite pools cleanly.
func (s *Store) Close() error {
	var err1, err2 error
	if s.ro != nil {
		err1 = s.ro.Close()
	}
	if s.rw != nil {
		err2 = s.rw.Close()
	}
	return errors.Join(err1, err2)
}

// WriteDB exposes the single-writer *sql.DB for low-level tests and migrations.
func (s *Store) WriteDB() *sql.DB { return s.rw }

// ReadDB exposes the read-only *sql.DB pool.
func (s *Store) ReadDB() *sql.DB { return s.ro }

// Path returns the resolved database file path.
func (s *Store) Path() string { return s.path }

// WithWriteTx runs fn inside a single-writer BEGIN IMMEDIATE transaction.
func (s *Store) WithWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// WithReadTx runs fn inside a consistent read-only snapshot transaction.
func (s *Store) WithReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// BackupTo creates a consistent online hot backup of the WAL database at destPath
// using ncruces/go-sqlite3 native BackupInit / BackupStep.
func (s *Store) BackupTo(ctx context.Context, destPath string) error {
	absDest, err := filepath.Abs(destPath)
	if err != nil {
		return fmt.Errorf("resolve backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDest), 0o755); err != nil {
		return fmt.Errorf("mkdir backup parent: %w", err)
	}

	tmpPath := fmt.Sprintf("%s.tmp-%s", absDest, ulid.Make().String())
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	conn, err := s.ro.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire read conn for backup: %w", err)
	}
	defer conn.Close()

	dstURI := buildFileURI(tmpPath, nil, nil)
	err = conn.Raw(func(driverConn any) error {
		dc, ok := driverConn.(driver.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver conn type %T", driverConn)
		}
		backup, err := dc.Raw().BackupInit("main", dstURI)
		if err != nil {
			return fmt.Errorf("backup init: %w", err)
		}
		defer backup.Close()

		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			done, err := backup.Step(100)
			if err != nil {
				return fmt.Errorf("backup step: %w", err)
			}
			if done {
				break
			}
		}
		return backup.Close()
	})
	if err != nil {
		return err
	}

	if err := os.Rename(tmpPath, absDest); err != nil {
		return fmt.Errorf("finalize backup file: %w", err)
	}
	return nil
}

func newID(prefix string) string {
	return prefix + "_" + ulid.Make().String()
}

// backupBeforeV5Migration takes a consistent VACUUM INTO hot backup once before applying v5
// on an existing on-disk database (schema_migrations exists and version 5 has not yet run).
func backupBeforeV5Migration(ctx context.Context, db *sql.DB, absPath string) error {
	if absPath == "" || absPath == ":memory:" {
		return nil
	}
	var tableExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableExists); err != nil {
		return fmt.Errorf("check schema_migrations table: %w", err)
	}
	if tableExists == 0 {
		return nil
	}
	var hasV5 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&hasV5); err != nil {
		return fmt.Errorf("check schema version 5: %w", err)
	}
	if hasV5 > 0 {
		return nil
	}
	bakPath := absPath + ".pre-v5.bak"
	if _, err := os.Stat(bakPath); err == nil {
		return nil
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", bakPath, time.Now().UnixNano())
	_ = os.Remove(tmpPath)
	escaped := filepath.ToSlash(tmpPath)
	// SQLite VACUUM INTO takes a string literal path; single quotes escaped defensively.
	escaped = fmt.Sprintf("'%s'", replaceAllSingleQuotes(escaped))
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+escaped); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("vacuum pre-v5 backup: %w", err)
	}
	if err := os.Rename(tmpPath, bakPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize pre-v5 backup: %w", err)
	}
	return nil
}

// backupBeforeV6Migration takes a consistent VACUUM INTO hot backup once before applying v6
// on an existing on-disk database (schema_migrations exists and version 6 has not yet run).
func backupBeforeV6Migration(ctx context.Context, db *sql.DB, absPath string) error {
	if absPath == "" || absPath == ":memory:" {
		return nil
	}
	var tableExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableExists); err != nil {
		return fmt.Errorf("check schema_migrations table: %w", err)
	}
	if tableExists == 0 {
		return nil
	}
	var hasV6 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&hasV6); err != nil {
		return fmt.Errorf("check schema version 6: %w", err)
	}
	if hasV6 > 0 {
		return nil
	}
	bakPath := absPath + ".pre-v6.bak"
	if _, err := os.Stat(bakPath); err == nil {
		return nil
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", bakPath, time.Now().UnixNano())
	_ = os.Remove(tmpPath)
	escaped := filepath.ToSlash(tmpPath)
	escaped = fmt.Sprintf("'%s'", replaceAllSingleQuotes(escaped))
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+escaped); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("vacuum pre-v6 backup: %w", err)
	}
	if err := os.Rename(tmpPath, bakPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize pre-v6 backup: %w", err)
	}
	return nil
}

// backupBeforeV7Migration takes a consistent VACUUM INTO hot backup once before applying v7
// (or before rebuilding legacy attempts table for TriggerChildResult) on an existing on-disk database.
func backupBeforeV7Migration(ctx context.Context, db *sql.DB, absPath string) error {
	if absPath == "" || absPath == ":memory:" {
		return nil
	}
	var tableExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableExists); err != nil {
		return fmt.Errorf("check schema_migrations table: %w", err)
	}
	if tableExists == 0 {
		return nil
	}
	var hasV7 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&hasV7); err != nil {
		return fmt.Errorf("check schema version 7: %w", err)
	}
	var attSQL sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='attempts'`).Scan(&attSQL)
	needsAttemptsRebuild := attSQL.Valid && !strings.Contains(attSQL.String, "'"+string(protocol.TriggerChildResult)+"'")
	if hasV7 > 0 && !needsAttemptsRebuild {
		return nil
	}
	bakPath := absPath + ".pre-v7.bak"
	if _, err := os.Stat(bakPath); err == nil {
		return nil
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", bakPath, time.Now().UnixNano())
	_ = os.Remove(tmpPath)
	escaped := filepath.ToSlash(tmpPath)
	escaped = fmt.Sprintf("'%s'", replaceAllSingleQuotes(escaped))
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+escaped); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("vacuum pre-v7 backup: %w", err)
	}
	if err := os.Rename(tmpPath, bakPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize pre-v7 backup: %w", err)
	}
	return nil
}

func replaceAllSingleQuotes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
