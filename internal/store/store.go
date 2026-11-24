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
	ErrForbidden            = errors.New("forbidden")
	ErrVerificationRequired = errors.New("verification required")
	ErrInvalidInput         = errors.New("invalid input")
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

	if err := applySchema(ctx, rw); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
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
	cfg := protocol.ProjectConfig{
		DisplayName:       protocol.ScratchProjectDisplayName,
		RepoIdentity:      protocol.ScratchProjectRepoIdentity,
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		IDEScheme:         protocol.IDECursor,
		Anchors:           map[string]protocol.ProjectAnchorConfig{},
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
	`, protocol.ScratchProjectID, protocol.ScratchProjectDisplayName, protocol.ScratchProjectRepoIdentity, string(cfgJSON), now)
	if err != nil {
		return fmt.Errorf("ensure _scratch project: %w", err)
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
