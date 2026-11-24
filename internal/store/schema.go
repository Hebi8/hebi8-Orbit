package store

import (
	"fmt"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// BuildSchemaDDL constructs the complete SQLite schema using internal/protocol
// enum sets as the single source of truth for CHECK constraints and partial unique indexes.
func BuildSchemaDDL() string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
  project_id          TEXT PRIMARY KEY,
  display_name        TEXT NOT NULL,
  repo_identity       TEXT NOT NULL,
  default_branch      TEXT NOT NULL DEFAULT 'main',
  mirror_bare_path    TEXT NOT NULL,
  config_json         TEXT NOT NULL,
  archived            INTEGER NOT NULL DEFAULT 0,
  updated_at          INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS machines (
  machine_id          TEXT PRIMARY KEY,
  transport_mode      TEXT NOT NULL CHECK (transport_mode IN (%s)),
  status              TEXT NOT NULL CHECK (status IN (%s)),
  capabilities_json   TEXT NOT NULL,
  resources_json      TEXT NOT NULL DEFAULT '{}',
  runner_version      TEXT,
  last_seen_at        INTEGER,
  updated_at          INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
  task_id             TEXT PRIMARY KEY,
  project_id          TEXT NOT NULL REFERENCES projects(project_id),
  parent_task_id      TEXT REFERENCES tasks(task_id),
  title               TEXT NOT NULL,
  goal                TEXT NOT NULL,
  required_capability TEXT NOT NULL,
  required_resources  TEXT NOT NULL DEFAULT '[]',
  preferred_adapter   TEXT NOT NULL DEFAULT 'agy' CHECK (preferred_adapter IN (%s)),
  desired_config_json TEXT NOT NULL DEFAULT '{}',
  state               TEXT NOT NULL CHECK (state IN (%s)),
  waiting_reason      TEXT,
  active_attempt_id   TEXT,
  latest_delivery_id  TEXT,
  created_by          TEXT NOT NULL CHECK (created_by IN (%s)),
  idempotency_key     TEXT UNIQUE,
  version             INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  archived_at         INTEGER,
  UNIQUE (task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_project_state ON tasks(project_id, state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(parent_task_id) WHERE parent_task_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_sessions (
  wb_session_id       TEXT PRIMARY KEY,
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  machine_id          TEXT NOT NULL REFERENCES machines(machine_id),
  adapter             TEXT NOT NULL CHECK (adapter IN (%s)),
  provider_session_id TEXT,
  cwd                 TEXT NOT NULL,
  desired_config_json TEXT NOT NULL,
  system_prompt_hash  TEXT,
  runtime_state       TEXT NOT NULL CHECK (runtime_state IN (%s)),
  worker_pid          INTEGER,
  last_activity_at    INTEGER NOT NULL,
  created_at          INTEGER NOT NULL,
  closed_at           INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
  UNIQUE (wb_session_id, task_id, machine_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_sessions_task ON agent_sessions(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_machine_live ON agent_sessions(machine_id, runtime_state);

CREATE TABLE IF NOT EXISTS attempts (
  attempt_id          TEXT PRIMARY KEY,
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  attempt_no          INTEGER NOT NULL,
  machine_id          TEXT NOT NULL REFERENCES machines(machine_id),
  adapter             TEXT NOT NULL CHECK (adapter IN (%s)),
  wb_session_id       TEXT REFERENCES agent_sessions(wb_session_id),
  trigger_kind        TEXT NOT NULL CHECK (trigger_kind IN (%s)),
  trigger_payload_json TEXT NOT NULL DEFAULT '{}',
  worktree_path       TEXT,
  input_delivery_id   TEXT,
  base_sha            TEXT,
  output_delivery_id  TEXT,
  target_env          TEXT,
  deploy_phase        TEXT CHECK (deploy_phase IN (%s)),
  state               TEXT NOT NULL CHECK (state IN (%s)),
  resume_state        TEXT CHECK (resume_state IN (%s)),
  completion_source   TEXT CHECK (completion_source IN (%s)),
  lease_token         TEXT NOT NULL,
  lease_expires_at    INTEGER NOT NULL,
  last_acked_outbox   INTEGER NOT NULL DEFAULT 0,
  error_code          TEXT,
  error_detail        TEXT,
  token_usage_json    TEXT,
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
  UNIQUE (task_id, attempt_no),
  UNIQUE (attempt_id, task_id, project_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_active_per_task
  ON attempts(task_id)
  WHERE state IN (%s);

CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_active_per_session
  ON attempts(wb_session_id)
  WHERE wb_session_id IS NOT NULL
    AND state IN (%s);

CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_running_deploy_per_env
  ON attempts(target_env)
  WHERE adapter = 'ssh_deploy' AND target_env IS NOT NULL
    AND state IN (%s);

CREATE INDEX IF NOT EXISTS idx_attempts_machine_active
  ON attempts(machine_id, state)
  WHERE state IN (%s);

CREATE TABLE IF NOT EXISTS task_pending_inputs (
  input_id            TEXT PRIMARY KEY,
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN (%s)),
  payload_json        TEXT NOT NULL,
  state               TEXT NOT NULL CHECK (state IN (%s)),
  created_at          INTEGER NOT NULL,
  dispatched_attempt_id TEXT REFERENCES attempts(attempt_id),
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_task_pending_inputs_open ON task_pending_inputs(task_id, created_at) WHERE state = 'queued';

CREATE TABLE IF NOT EXISTS deliveries (
  delivery_id         TEXT PRIMARY KEY,
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  attempt_id          TEXT NOT NULL,
  source_machine_id   TEXT NOT NULL REFERENCES machines(machine_id),
  repo_identity       TEXT NOT NULL,
  base_sha            TEXT NOT NULL,
  head_sha            TEXT NOT NULL,
  git_ref             TEXT NOT NULL UNIQUE,
  kind                TEXT NOT NULL CHECK (kind IN (%s)),
  summary             TEXT NOT NULL,
  manifest_json       TEXT NOT NULL,
  verified_machines_json TEXT NOT NULL DEFAULT '{}',
  frozen_snapshot_dir TEXT,
  ryugi_sync_state    TEXT NOT NULL DEFAULT 'none' CHECK (ryugi_sync_state IN (%s)),
  late_arrival        INTEGER NOT NULL DEFAULT 0,
  pinned              INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  FOREIGN KEY (attempt_id, task_id, project_id) REFERENCES attempts(attempt_id, task_id, project_id),
  UNIQUE (delivery_id, task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_deliveries_task ON deliveries(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_deliveries_project_sha ON deliveries(project_id, head_sha);

CREATE TABLE IF NOT EXISTS ryugi_bindings (
  binding_id          TEXT PRIMARY KEY,
  delivery_id         TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  frozen_project_dir  TEXT NOT NULL,
  artifact_locator    TEXT NOT NULL,
  derived_artifact_id TEXT NOT NULL UNIQUE,
  open_url            TEXT NOT NULL,
  last_thread_receipt TEXT,
  last_changes_receipt TEXT,
  last_state_hashes   TEXT NOT NULL DEFAULT '{}',
  active              INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  FOREIGN KEY (delivery_id, task_id, project_id) REFERENCES deliveries(delivery_id, task_id, project_id),
  UNIQUE (delivery_id, artifact_locator)
);

CREATE TABLE IF NOT EXISTS annotations (
  annotation_id       TEXT PRIMARY KEY,
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  delivery_id         TEXT NOT NULL,
  source              TEXT NOT NULL CHECK (source IN (%s)),
  external_key        TEXT UNIQUE,
  anchor_json         TEXT NOT NULL,
  body                TEXT NOT NULL,
  status              TEXT NOT NULL CHECK (status IN (%s)),
  triggered_attempt_id TEXT REFERENCES attempts(attempt_id),
  resolution_delivery_id TEXT REFERENCES deliveries(delivery_id),
  resolution_note     TEXT,
  created_at          INTEGER NOT NULL,
  resolved_at         INTEGER,
  FOREIGN KEY (delivery_id, task_id, project_id) REFERENCES deliveries(delivery_id, task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_annotations_task_status ON annotations(task_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_annotations_delivery ON annotations(delivery_id, created_at);

CREATE TABLE IF NOT EXISTS approvals (
  approval_id         TEXT PRIMARY KEY,
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  attempt_id          TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN (%s)),
  action_name         TEXT NOT NULL,
  target_env          TEXT NOT NULL,
  input_sha           TEXT NOT NULL,
  payload_json        TEXT NOT NULL,
  payload_sha256      TEXT NOT NULL,
  acp_rpc_id          TEXT,
  runner_instance_id  TEXT,
  options_json        TEXT NOT NULL,
  requested_by        TEXT NOT NULL CHECK (requested_by IN (%s)),
  state               TEXT NOT NULL CHECK (state IN (%s)),
  decision_id         TEXT UNIQUE,
  selected_option_id  TEXT,
  decided_by          TEXT CHECK (decided_by IN (%s)),
  decision_note       TEXT,
  expires_at          INTEGER NOT NULL,
  created_at          INTEGER NOT NULL,
  decided_at          INTEGER,
  consumed_at         INTEGER,
  FOREIGN KEY (attempt_id, task_id, project_id) REFERENCES attempts(attempt_id, task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_approvals_pending ON approvals(state, created_at DESC)
  WHERE state IN ('pending', 'approved_pending_delivery', 'rejected_pending_delivery');
CREATE INDEX IF NOT EXISTS idx_approvals_attempt ON approvals(attempt_id, state);

CREATE TABLE IF NOT EXISTS events (
  seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id            TEXT NOT NULL UNIQUE,
  stream_id           TEXT NOT NULL,
  project_id          TEXT,
  task_id             TEXT,
  attempt_id          TEXT,
  actor_kind          TEXT NOT NULL CHECK (actor_kind IN (%s)),
  actor_id            TEXT NOT NULL,
  kind                TEXT NOT NULL,
  visibility          TEXT NOT NULL CHECK (visibility IN (%s)),
  payload_json        TEXT NOT NULL,
  created_at          INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_stream_seq ON events(stream_id, seq);
CREATE INDEX IF NOT EXISTS idx_events_project_vis_seq ON events(project_id, visibility, seq);
CREATE INDEX IF NOT EXISTS idx_events_vis_seq ON events(visibility, seq);
CREATE INDEX IF NOT EXISTS idx_events_raw_gc ON events(visibility, created_at) WHERE visibility = 'raw_acp';

CREATE TABLE IF NOT EXISTS stream_cursors (
  consumer_id         TEXT NOT NULL,
  stream_id           TEXT NOT NULL,
  last_seq            INTEGER NOT NULL DEFAULT 0,
  updated_at          INTEGER NOT NULL,
  PRIMARY KEY (consumer_id, stream_id)
);

CREATE TABLE IF NOT EXISTS inbox_items (
  inbox_id            TEXT PRIMARY KEY,
  project_id          TEXT NOT NULL REFERENCES projects(project_id),
  task_id             TEXT,
  category            TEXT NOT NULL CHECK (category IN (%s)),
  ref_id              TEXT NOT NULL,
  title               TEXT NOT NULL,
  summary             TEXT NOT NULL,
  priority            INTEGER NOT NULL DEFAULT 2,
  state               TEXT NOT NULL CHECK (state IN (%s)),
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  resolved_at         INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_inbox_open ON inbox_items(state, priority ASC, updated_at DESC) WHERE state = 'open';

CREATE TABLE IF NOT EXISTS pa_state (
  pa_id               TEXT PRIMARY KEY DEFAULT 'default',
  active_wb_session_id TEXT,
  last_read_event_seq INTEGER NOT NULL DEFAULT 0,
  focus_pointers_json TEXT NOT NULL DEFAULT '[]',
  updated_at          INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS delivery_blobs (
  sha256              TEXT PRIMARY KEY,
  size_bytes          INTEGER NOT NULL,
  storage_path        TEXT NOT NULL,
  ref_count           INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL
);
`,
		protocol.SQLInClause(protocol.AllTransportModes),
		protocol.SQLInClause(protocol.AllMachineStatuses),
		protocol.SQLInClause(protocol.AllAdapters),
		protocol.SQLInClause(protocol.AllTaskStates),
		protocol.SQLInClause(protocol.AllTaskCreatedBy),
		protocol.SQLInClause(protocol.AllSessionAdapters),
		protocol.SQLInClause(protocol.AllSessionRuntimeStates),
		protocol.SQLInClause(protocol.AllAdapters),
		protocol.SQLInClause(protocol.AllTriggerKinds),
		protocol.SQLInClause(protocol.AllDeployPhases),
		protocol.SQLInClause(protocol.AllAttemptStates),
		protocol.SQLInClause(protocol.AllResumeStates),
		protocol.SQLInClause(protocol.AllCompletionSources),
		protocol.SQLInClause(protocol.ActiveAttemptStates),
		protocol.SQLInClause(protocol.ActiveAttemptStates),
		protocol.SQLInClause(protocol.ActiveDeployAttemptStates),
		protocol.SQLInClause(protocol.ActiveAttemptStates),
		protocol.SQLInClause(protocol.AllPendingInputKinds),
		protocol.SQLInClause(protocol.AllPendingInputStates),
		protocol.SQLInClause(protocol.AllDeliveryKinds),
		protocol.SQLInClause(protocol.AllRyugiSyncStates),
		protocol.SQLInClause(protocol.AllAnnotationSources),
		protocol.SQLInClause(protocol.AllAnnotationStatuses),
		protocol.SQLInClause(protocol.AllApprovalKinds),
		protocol.SQLInClause(protocol.AllApprovalRequestedBy),
		protocol.SQLInClause(protocol.AllApprovalStates),
		protocol.SQLInClause(protocol.AllApprovalDecidedBy),
		protocol.SQLInClause(protocol.AllActorKinds),
		protocol.SQLInClause(protocol.AllEventVisibilities),
		protocol.SQLInClause(protocol.AllInboxCategories),
		protocol.SQLInClause(protocol.AllInboxStates),
	)
}

// BuildMigrationV2DDL constructs the Phase 1 additive schema tables and indexes
// using internal/protocol enum sets as the single source of truth.
func BuildMigrationV2DDL() string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS pa_threads (
  pa_thread_id  TEXT PRIMARY KEY,
  title         TEXT NOT NULL,
  wb_session_id TEXT,
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN (%s)),
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_pa_threads_updated ON pa_threads(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS pa_messages (
  message_id     TEXT PRIMARY KEY,
  pa_thread_id   TEXT NOT NULL REFERENCES pa_threads(pa_thread_id),
  role           TEXT NOT NULL CHECK (role IN (%s)),
  content        TEXT NOT NULL,
  delta_from_seq INTEGER NOT NULL DEFAULT 0,
  delta_to_seq   INTEGER NOT NULL DEFAULT 0,
  actions_json   TEXT NOT NULL DEFAULT '[]',
  created_at     INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_pa_messages_thread ON pa_messages(pa_thread_id, created_at ASC);

CREATE TABLE IF NOT EXISTS task_sources (
  source_id   TEXT PRIMARY KEY,
  task_id     TEXT NOT NULL,
  project_id  TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN (%s)),
  label       TEXT NOT NULL,
  uri_or_ref  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_task_sources_task ON task_sources(task_id, created_at ASC);

CREATE TABLE IF NOT EXISTS board_messages (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id     TEXT NOT NULL UNIQUE,
  task_id        TEXT NOT NULL,
  project_id     TEXT NOT NULL,
  author_name    TEXT NOT NULL,
  target_mention TEXT,
  body           TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'posted' CHECK (status IN (%s)),
  external_key   TEXT UNIQUE,
  created_at     INTEGER NOT NULL,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_board_messages_task_seq ON board_messages(task_id, seq ASC);

CREATE TABLE IF NOT EXISTS board_members (
  task_id           TEXT NOT NULL,
  project_id        TEXT NOT NULL,
  member_name       TEXT NOT NULL,
  machine_id        TEXT,
  adapter           TEXT,
  state             TEXT NOT NULL DEFAULT 'working' CHECK (state IN (%s)),
  last_consumed_seq INTEGER NOT NULL DEFAULT 0,
  current_step      TEXT,
  worker_task_id    TEXT REFERENCES tasks(task_id),
  updated_at        INTEGER NOT NULL,
  PRIMARY KEY (task_id, member_name),
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_board_members_task ON board_members(task_id, updated_at DESC);
`,
		protocol.SQLInClause(protocol.AllPAThreadStatuses),
		protocol.SQLInClause(protocol.AllPAMessageRoles),
		protocol.SQLInClause(protocol.AllTaskSourceKinds),
		protocol.SQLInClause(protocol.AllBoardMessageStatuses),
		protocol.SQLInClause(protocol.AllBoardMemberStates),
	)
}

// BuildMigrationV3DDL constructs the Phase 1 review-hardening additive tables.
func BuildMigrationV3DDL() string {
	return `
CREATE TABLE IF NOT EXISTS pa_action_receipts (
  idempotency_key TEXT PRIMARY KEY,
  action          TEXT NOT NULL,
  result_json     TEXT NOT NULL,
  created_at      INTEGER NOT NULL
);
`
}
