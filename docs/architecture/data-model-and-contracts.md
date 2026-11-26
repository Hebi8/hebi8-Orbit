# 数据模型与通信契约（hebi8-Orbit）

本章给出 **SQLite Schema（WAL 模式，由 `ncruces/go-sqlite3` 纯 Go 驱动）**、**静态/动态注册表 YAML Schema**，以及 **Client ↔ Hub / Hub ↔ Runner 通信契约**。核心约束：**三类生命周期严格拆表**（统一事件流 / 任务与执行尝试 / 不可变交付与批注审批），并在数据库层通过复合外键、三条部分唯一索引与 `BEGIN IMMEDIATE` 事务物理堵死并发双领取、断线重发重复、陈旧审批误放行。

> 注：本规范为 V1 基础契约；后续演进中新增的动态配对与每机 Agent 管理表见 [onboarding-and-machine-setup.md](./onboarding-and-machine-setup.md)，3 层 Project 模型（`Project → Part → Location`）与对等网签名账本设计见 [peer-mesh-and-v3-spec.md](./peer-mesh-and-v3-spec.md)，一等子会话扩展见 [subsession-ux-v4-spec.md](../ux/subsession-ux-v4-spec.md)。实际落地表结构以 `internal/store/schema.go` 与 `internal/protocol/enums.go` 为准。

---

## 1. SQLite 存储配置与并发事务纪律

### 1.1 连接初始化 PRAGMA（`ncruces/go-sqlite3` 单写连接池）

`orbit-hub` 维护单一写连接池（`MaxOpenConns(1)` + DSN 参数 `_txlock=immediate`）服务全部写事务，另配只读连接池（`mode=ro`）供大查询（历史 Diff / 事件翻页），杜绝多线程争抢写锁：

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;       -- WAL 下兼顾吞吐与掉电安全
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;
PRAGMA cache_size = -32000;        -- 32 MiB 页缓存
```

### 1.2 五条写事务铁律

1. **所有多表状态迁移必须走 `BEGIN IMMEDIATE`**（通过 `ncruces/go-sqlite3` DSN `_txlock=immediate` 与写库单连接封装保证）：防止 `DEFERRED` 事务在读转写阶段抛 `SQLITE_BUSY`。
2. **业务状态变更、事件追加、收件箱投影在同一事务提交**：例如 `approval.requested` 或 `delivery.published` 发生时，主表更新 + `events` 插入 + `inbox_items` 投影必须原子成功或一起回滚，绝不允许「任务已阻塞但收件箱无卡」。
3. **单一枚举真源 + 复合外键防跨任务串属**：所有状态集合由 Go `internal/protocol` 统一定义，通过 `make gen-types`（`go generate`）同步生成 `web/src/generated/protocol.ts` 与校验常量；`deliveries`、`annotations`、`approvals` 通过复合外键 `(task_id, project_id)` / `(attempt_id, task_id, project_id)` 与应用层反查推导，物理杜绝「交付填任务 U、Attempt 填任务 T」的跨归属污染。
4. **高频 `raw_acp` 流式增量批量落盘**：Runner 对连续的 `agent_message_chunk` / `agent_thought_chunk` 做 **200ms 滑动合并窗口**，Hub 对上报的一批帧在单事务内插入 `events`；`raw_acp` 可按保留期（如 30 天）压缩清理，`timeline` / `inbox` / `audit` 永久保留。
5. **物理防双发、防跳空、防并发互盖**：用部分唯一索引约束「同一 Task 同时最多 1 个活跃 Attempt」「同一 provider session 同时最多被 1 个活跃 Attempt 拥有」「同一部署环境 `target_env` 同时最多 1 个活跃部署 Attempt」；用每 Attempt 严格连续递增的 Outbox 序号防止跳空 ACK；用 `lease_token` 与独立历史通道隔离迟到旧 Attempt 回报。

---

## 2. 完整 SQLite Schema（按三类生命周期分层）

### 2.1 注册表运行态镜像（来源：YAML 配置 / 动态注册 + 实时心跳）

```sql
-- 逻辑项目（启动及热重载时从 projects.yaml 或动态项目表同步；作外键锚点与查询索引）
CREATE TABLE projects (
  project_id          TEXT PRIMARY KEY,           -- 如 'demo-app'、'mobile-client'
  display_name        TEXT NOT NULL,
  repo_identity       TEXT NOT NULL,              -- 规范化标识，如 'github.com/example-org/demo-app' 或 'local:demo-poc'
  default_branch      TEXT NOT NULL DEFAULT 'main',
  mirror_bare_path    TEXT NOT NULL,              -- hub-node 中心 bare mirror 路径
  config_json         TEXT NOT NULL,              -- 完整项目配置快照（能力锚点、ide 偏好、部署配置等）
  archived            INTEGER NOT NULL DEFAULT 0,
  updated_at          INTEGER NOT NULL            -- Unix 毫秒时间戳
);

-- 机器运行态（基础配置来自 machines.yaml / 动态配对，在线态与资源占用由心跳更新）
CREATE TABLE machines (
  machine_id          TEXT PRIMARY KEY,           -- 如 'hub-node' | 'mac-node' | 'aux-node' | 'deploy-vps'
  transport_mode      TEXT NOT NULL CHECK (transport_mode IN ('local_ws', 'remote_ws', 'ssh_outbound')),
  status              TEXT NOT NULL CHECK (status IN ('online', 'offline', 'degraded')),
  capabilities_json   TEXT NOT NULL,              -- ["primary-dev","linux-x64"] 等
  resources_json      TEXT NOT NULL DEFAULT '{}', -- 实时资源探针态：{ "chrome_logged_in": {"state":"free"|"busy"|"unavailable","reason":...} }
  runner_version      TEXT,
  last_seen_at        INTEGER,
  updated_at          INTEGER NOT NULL
);
```

### 2.2 生命周期 A：Tasks、Attempts 与 ACP Sessions

> **核心语义**：`读到 ≠ 领取 ≠ 完成`。任务可跨多次 `attempt`（失败重跑、批注回流触发新一轮、跨机接力、受审批部署、子会话结果唤醒 `child_result`、手工导入），每次 `attempt` 绑定固定输入基线与独立隔离 worktree，历史 `attempt` 永久保留供追溯。

```sql
-- 逻辑任务（归属某 Project；跨机/跨模型及一等子会话通过 parent_task_id 关联）
CREATE TABLE tasks (
  task_id             TEXT PRIMARY KEY,           -- ULID，如 'tsk_01J...'
  project_id          TEXT NOT NULL REFERENCES projects(project_id),
  parent_task_id      TEXT REFERENCES tasks(task_id),
  title               TEXT NOT NULL,
  goal                TEXT NOT NULL,
  required_capability TEXT NOT NULL,              -- 如 'primary-dev' | 'macos-env' | 'deploy-node'
  required_resources  TEXT NOT NULL DEFAULT '[]', -- JSON 数组，如 ["chrome_logged_in"] 或 ["xcode_sim"]
  preferred_adapter   TEXT NOT NULL DEFAULT 'agy' CHECK (preferred_adapter IN ('agy', 'claude', 'codex', 'ssh_deploy', 'manual')),
  desired_config_json TEXT NOT NULL DEFAULT '{}', -- {"model":"gemini-4-neon","thought_level":"high","mode":"auto"}
  state               TEXT NOT NULL CHECK (state IN (
                        'queued',
                        'waiting_resource',       -- 显式等待 mac-node 锁屏/浏览器/模拟器等物理条件
                        'running',
                        'waiting_approval',       -- 挂起等待 ACP 工具审批或生产部署审批
                        'in_review',              -- 已产出交付物，或需人工核对（如 Swarm 静默超时/崩溃窗口）
                        'done',
                        'failed',
                        'cancelled'
                      )),
  waiting_reason      TEXT,                       -- 人可读的等待或待核对原因
  active_attempt_id   TEXT,                       -- 冗余指针，加速列表页读取
  latest_delivery_id  TEXT,                       -- 最近一次已核验的交付物 ID
  created_by          TEXT NOT NULL CHECK (created_by IN ('human', 'pa', 'annotation', 'template', 'cli')),
  idempotency_key     TEXT UNIQUE,                -- 防 PA/客户端/重试重复建单
  version             INTEGER NOT NULL DEFAULT 1, -- 乐观锁
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  archived_at         INTEGER,
  UNIQUE (task_id, project_id)                    -- 供子表复合外键强约束归属一致
);

CREATE INDEX idx_tasks_project_state ON tasks(project_id, state, updated_at DESC);
CREATE INDEX idx_tasks_parent ON tasks(parent_task_id) WHERE parent_task_id IS NOT NULL;

-- 底层 ACP 会话元数据（映射外层 session_id <-> CLI provider_session_id；同一时刻最多被 1 个活跃 Attempt 拥有）
CREATE TABLE agent_sessions (
  wb_session_id       TEXT PRIMARY KEY,           -- ULID: 'ses_01J...'
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  machine_id          TEXT NOT NULL REFERENCES machines(machine_id),
  adapter             TEXT NOT NULL CHECK (adapter IN ('agy', 'claude', 'codex')),
  provider_session_id TEXT,                       -- agy/claude/codex 返回的原生 sessionId
  cwd                 TEXT NOT NULL,              -- 强绑定的隔离 worktree 绝对路径（换目录必须开新会话）
  desired_config_json TEXT NOT NULL,              -- 每次 session/resume 后强制重发的 configOptions
  system_prompt_hash  TEXT,                       -- 注入契约哈希，用于审计
  runtime_state       TEXT NOT NULL CHECK (runtime_state IN ('starting', 'live', 'closing', 'idle_closed', 'dead')),
  worker_pid          INTEGER,                    -- 仅用于观测/排查内存（--process-per-session）
  last_activity_at    INTEGER NOT NULL,
  created_at          INTEGER NOT NULL,
  closed_at           INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
  UNIQUE (wb_session_id, task_id, machine_id)
);

CREATE INDEX idx_agent_sessions_task ON agent_sessions(task_id, created_at DESC);
CREATE INDEX idx_agent_sessions_machine_live ON agent_sessions(machine_id, runtime_state);

-- 单次执行尝试（一个 Task 可有多轮 Attempt：初次执行、批注回流续跑、审批过期重跑、SSH 部署、子会话回传唤醒、手工导入）
CREATE TABLE attempts (
  attempt_id          TEXT PRIMARY KEY,           -- ULID: 'att_01J...'
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  attempt_no          INTEGER NOT NULL,
  machine_id          TEXT NOT NULL REFERENCES machines(machine_id),
  adapter             TEXT NOT NULL CHECK (adapter IN ('agy', 'claude', 'codex', 'ssh_deploy', 'manual')),
  wb_session_id       TEXT REFERENCES agent_sessions(wb_session_id),
  trigger_kind        TEXT NOT NULL CHECK (trigger_kind IN ('initial', 'followup', 'annotation_batch', 'retry', 'deploy', 'workspace_import', 'manual_import', 'child_result')),
  trigger_payload_json TEXT NOT NULL DEFAULT '{}',-- 触发本轮的提示词/批注 ID 列表/审批 ID
  worktree_path       TEXT,                       -- 执行机上的隔离 worktree 路径
  input_delivery_id   TEXT,                       -- 跨机接力或部署时的上游固定交付 ID
  base_sha            TEXT,                       -- 审阅基线 SHA（固定为本轮输入起点，不随 worktree 内 commit 漂移）
  output_delivery_id  TEXT,                       -- 本轮产出的交付 ID
  target_env          TEXT,                       -- 仅 ssh_deploy 使用，如 'deploy-vps:prod'，用于同环境串行互斥
  deploy_phase        TEXT CHECK (deploy_phase IN ('awaiting_approval', 'pushing', 'switching', 'verifying', 'done')),
  state               TEXT NOT NULL CHECK (state IN (
                        'leased',                 -- Hub 已派发租约，等待 Runner 确认
                        'preparing',              -- Runner 正在 fetch SHA / 建 worktree / 探针检查
                        'waiting_resource',       -- 显式等待 mac-node 物理资源，继续持租、就绪后原轮唤醒
                        'running',                -- ACP session/prompt 执行中或 SSH 部署执行中
                        'swarm_monitoring',       -- agy prompt 已返 end_turn，原生 Swarm 仍在后台推送 unsolicited updates
                        'waiting_approval',       -- 等待人类审批（ACP request_permission 或生产部署）
                        'succeeded',              -- 正常完成
                        'needs_verification',     -- 静默超时 / 审批回写崩溃窗口 / SSH 断连结果未知，保留现场待人工核对
                        'failed',
                        'expired',                -- 租约过期或审批发送前 Runner 重启导致挂起句柄死亡
                        'cancelled'
                      )),
  resume_state        TEXT CHECK (resume_state IN ('running', 'swarm_monitoring')), -- 挂起审批结束后回到进入前的状态
  completion_source   TEXT CHECK (completion_source IN ('explicit_deliver', 'prompt_end_turn', 'quiet_timeout', 'ssh_verified', 'manual_import')),
  lease_token         TEXT NOT NULL,              -- 随机高熵令牌，所有 Runner 回报必须校验匹配
  lease_expires_at    INTEGER NOT NULL,           -- 心跳续租截止时间
  last_acked_outbox   INTEGER NOT NULL DEFAULT 0, -- 严格连续已确认最大 outboxSeq（绝不跨缺口推进）
  error_code          TEXT,
  error_detail        TEXT,
  token_usage_json    TEXT,                       -- {"input":...,"output":...,"cachedRead":...,"thought":...}
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
  UNIQUE (task_id, attempt_no),
  UNIQUE (attempt_id, task_id, project_id)        -- 供 deliveries/approvals 复合外键校验归属一致
);

-- 数据库层物理防双领取：同一 Task 同时只能有 1 个活跃 Attempt（含 waiting_resource）
CREATE UNIQUE INDEX idx_attempts_one_active_per_task
  ON attempts(task_id)
  WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'swarm_monitoring', 'waiting_approval');

-- 数据库层物理防同一 provider session 被两个活跃 Attempt 并发复用导致串轮
CREATE UNIQUE INDEX idx_attempts_one_active_per_session
  ON attempts(wb_session_id)
  WHERE wb_session_id IS NOT NULL
    AND state IN ('leased', 'preparing', 'waiting_resource', 'running', 'swarm_monitoring', 'waiting_approval');

-- 数据库层物理防同一部署环境（如 deploy-vps:prod）并发执行两笔已批准部署互相覆盖
CREATE UNIQUE INDEX idx_attempts_one_running_deploy_per_env
  ON attempts(target_env)
  WHERE adapter = 'ssh_deploy' AND target_env IS NOT NULL
    AND state IN ('leased', 'preparing', 'running');

CREATE INDEX idx_attempts_machine_active
  ON attempts(machine_id, state)
  WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'swarm_monitoring', 'waiting_approval');

-- 批注/followup 排队表（当目标任务正处于 running/swarm_monitoring 且用户选择"等当前轮结束后续跑"时持久暂存）
CREATE TABLE task_pending_inputs (
  input_id            TEXT PRIMARY KEY,           -- ULID: 'inp_01J...'
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN ('followup', 'annotation_batch')),
  payload_json        TEXT NOT NULL,
  state               TEXT NOT NULL CHECK (state IN ('queued', 'dispatched', 'cancelled')),
  created_at          INTEGER NOT NULL,
  dispatched_attempt_id TEXT REFERENCES attempts(attempt_id),
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX idx_task_pending_inputs_open ON task_pending_inputs(task_id, created_at) WHERE state = 'queued';
```

### 2.3 生命周期 B：不可变交付物、Ryugi 绑定、批注与审批

> **核心语义**：交付物一旦发布即**不可变**，绑定确定 `head_sha`、`review base_sha` 与防 GC 的 `git_ref`。复合外键保证交付、批注、审批在物理上绝不会跨项目/跨任务串属。

```sql
-- 不可变交付物（跨机代码 checkpoint、设计稿快照、部署回执、手工导入快照）
CREATE TABLE deliveries (
  delivery_id         TEXT PRIMARY KEY,           -- ULID: 'dlv_01J...'
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  attempt_id          TEXT NOT NULL,
  source_machine_id   TEXT NOT NULL REFERENCES machines(machine_id),
  repo_identity       TEXT NOT NULL,
  base_sha            TEXT NOT NULL,              -- 固定为本轮审阅基线（attempts.base_sha），不随 worktree 内多次 commit 漂移
  head_sha            TEXT NOT NULL,              -- 不可变交付 commit SHA
  git_ref             TEXT NOT NULL UNIQUE,       -- 'refs/orbit/deliveries/<delivery_id>'，受防 GC 锁定
  kind                TEXT NOT NULL CHECK (kind IN ('code_diff', 'ryugi_artifact', 'mixed', 'deploy_receipt', 'workspace_snapshot')),
  summary             TEXT NOT NULL,
  manifest_json       TEXT NOT NULL,              -- {"filesChanged":12,"insertions":...,"blobs":[{"path":...,"sha256":...,"size":...}],"ryugiArtifacts":["artboard/v1"]}
  verified_machines_json TEXT NOT NULL DEFAULT '{}', -- {"mac-node":{"verifiedAt":...,"sha":"..."}}
  frozen_snapshot_dir TEXT,                       -- hub-node 冻结快照目录 '~/.local/share/hebi8-orbit/deliveries/<delivery_id>'
  ryugi_sync_state    TEXT NOT NULL DEFAULT 'none' CHECK (ryugi_sync_state IN ('none', 'pending', 'ready', 'failed')),
  late_arrival        INTEGER NOT NULL DEFAULT 0, -- 1 = 超租约迟到补交，仅入历史，不覆盖任务主指针
  pinned              INTEGER NOT NULL DEFAULT 1, -- 1 = 防 GC 与清理保护
  created_at          INTEGER NOT NULL,
  FOREIGN KEY (attempt_id, task_id, project_id) REFERENCES attempts(attempt_id, task_id, project_id),
  UNIQUE (delivery_id, task_id, project_id)       -- 供 annotations 复合外键强校验
);

CREATE INDEX idx_deliveries_task ON deliveries(task_id, created_at DESC);
CREATE INDEX idx_deliveries_project_sha ON deliveries(project_id, head_sha);

-- Ryugi 黑盒 Sidecar 注册绑定（解决多版本 .artifact-id 冲突与 receipt 游标断点续传）
CREATE TABLE ryugi_bindings (
  binding_id          TEXT PRIMARY KEY,           -- ULID: 'ryg_01J...'
  delivery_id         TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  frozen_project_dir  TEXT NOT NULL,              -- 冻结快照根路径（源目录 0555 不可替换，.ryugi/artifacts/.state 0755 可写）
  artifact_locator    TEXT NOT NULL,              -- 如 'artboard/settings-compare' 或 'document/v1-spec'
  derived_artifact_id TEXT NOT NULL UNIQUE,       -- 由 (delivery_id, locator) 确定性派生的 21 位 [A-Za-z0-9] ID，杜绝跨版本冲突
  open_url            TEXT NOT NULL,              -- ryugi 返回的完整访问 URL（保留完整 path/query/key，仅按实测规则转换 origin）
  last_thread_receipt TEXT,                       -- 上次落库并 ack 的 threads receipt
  last_changes_receipt TEXT,                      -- 文档类 artifact 的 changes receipt
  last_state_hashes   TEXT NOT NULL DEFAULT '{}', -- asks/groups/rounds 无 receipt，仅同步最新已提交状态哈希集合
  active              INTEGER NOT NULL DEFAULT 1, -- 仅对 active=1 的近期交付定时轮询；冷历史按需点击重新打开/同步
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  FOREIGN KEY (delivery_id, task_id, project_id) REFERENCES deliveries(delivery_id, task_id, project_id),
  UNIQUE (delivery_id, artifact_locator)
);

-- 统一批注表（同时承载 Ryugi 画布批注与右侧 Diff 抽屉行评，复合外键强绑定 delivery/task/project）
CREATE TABLE annotations (
  annotation_id       TEXT PRIMARY KEY,           -- ULID: 'ann_01J...'
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  delivery_id         TEXT NOT NULL,
  source              TEXT NOT NULL CHECK (source IN ('ryugi', 'diff_drawer', 'mobile_inbox')),
  external_key        TEXT UNIQUE,                -- 幂等键：如 'ryugi:<binding_id>:<section>:<item_id>:<rev>'
  anchor_json         TEXT NOT NULL,              -- Diff: {path,side,startLine,endLine,contextBefore,contextAfter,baseSha,headSha}
                                                  -- Ryugi: {artifact,section,threadId,elementId,page,verdict}
  body                TEXT NOT NULL,
  status              TEXT NOT NULL CHECK (status IN ('open', 'queued', 'addressed', 'wontfix', 'stale')),
  triggered_attempt_id TEXT REFERENCES attempts(attempt_id),
  resolution_delivery_id TEXT REFERENCES deliveries(delivery_id),
  resolution_note     TEXT,                       -- 执行者回报的改动位置与说明
  created_at          INTEGER NOT NULL,
  resolved_at         INTEGER,
  FOREIGN KEY (delivery_id, task_id, project_id) REFERENCES deliveries(delivery_id, task_id, project_id)
);

CREATE INDEX idx_annotations_task_status ON annotations(task_id, status, created_at);
CREATE INDEX idx_annotations_delivery ON annotations(delivery_id, created_at);

-- 强绑定审批表（同时承载 ACP 工具权限请求与正向 SSH 生产部署审批；完整区分已批准待送达/已执行/崩溃窗口未知态）
-- 注：kind 中的 'hetz_deploy' 为现行协议枚举字面量（见 internal/protocol/enums.go），泛指受控正向 SSH 部署审批。
CREATE TABLE approvals (
  approval_id         TEXT PRIMARY KEY,           -- ULID: 'apr_01J...'，即 requestId
  project_id          TEXT NOT NULL,
  task_id             TEXT NOT NULL,
  attempt_id          TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN ('acp_tool_permission', 'hetz_deploy', 'external_action')),
  action_name         TEXT NOT NULL,              -- 如 'run_command' 或 'ssh_deploy'
  target_env          TEXT NOT NULL,              -- 如 'hub-node:worktree' | 'mac-node:chrome' | 'deploy-vps:prod'
  input_sha           TEXT NOT NULL,              -- 强绑定的交付/工作树 SHA
  payload_json        TEXT NOT NULL,              -- 完整 toolCall 或部署参数（含 expectedCurrentVersion）
  payload_sha256      TEXT NOT NULL,              -- canonical JSON 的 SHA-256，提交与消费双端核对
  acp_rpc_id          TEXT,                       -- 仅 acp_tool_permission 使用，如 'jetski-acp-front:3'
  runner_instance_id  TEXT,                       -- 发起该审批时的 Runner 进程实例 ID（用于重连区分网络闪断 vs 进程重启）
  options_json        TEXT NOT NULL,              -- [{"id":"allow","label":"允许一次","kind":"allow_once"}, ...]
  requested_by        TEXT NOT NULL CHECK (requested_by IN ('agent', 'pa', 'human')),
  state               TEXT NOT NULL CHECK (state IN (
                        'pending',                   -- 等待决策
                        'approved_pending_delivery', -- 人类已批准，待送达 Runner 或待 SshDeployExecutor 领取（断线自动重发）
                        'rejected_pending_delivery', -- 人类已拒绝，待送达 Runner 回写 ACP
                        'consumed_allowed',          -- Runner 已回写允许 / 部署已领取执行
                        'consumed_rejected',         -- Runner 已回写拒绝，或部署申请被拒
                        'uncertain',                 -- 决定发出后、回执到达前 Runner 崩溃（工具可能已执行，禁止标"安全重跑"）
                        'expired'                    -- 决定尚未发出前超时或 Runner 重启，确定未执行，可安全重跑
                      )),
  decision_id         TEXT UNIQUE,                -- 每次人类决定生成固定 ULID，用于 Runner 幂等去重
  selected_option_id  TEXT,
  decided_by          TEXT CHECK (decided_by IN ('human', 'policy')), -- SSH 部署审批硬校验必须 = 'human'
  decision_note       TEXT,
  expires_at          INTEGER NOT NULL,           -- 自动过期时间戳，防数天后误点旧卡片
  created_at          INTEGER NOT NULL,
  decided_at          INTEGER,
  consumed_at         INTEGER,
  FOREIGN KEY (attempt_id, task_id, project_id) REFERENCES attempts(attempt_id, task_id, project_id)
);

CREATE INDEX idx_approvals_pending ON approvals(state, created_at DESC)
  WHERE state IN ('pending', 'approved_pending_delivery', 'rejected_pending_delivery');
CREATE INDEX idx_approvals_attempt ON approvals(attempt_id, state);
```

### 2.4 生命周期 C：统一事件日志、游标、低噪音收件箱与 PA 指针

> **核心语义**：所有人工操作、Agent 输出、Ryugi 批注、审批决策、部署核验都写入同一张只追加 `events` 表。通过 `visibility` 字段将高频 ACP token 流（`raw_acp`）与业务关键节点（`timeline`/`inbox`/`audit`）隔离，让 UI 和 PA 天然不失忆且不被噪音淹没。

```sql
-- 统一只追加事件日志（seq 严格单调递增，作全局游标与断线补读偏移）
CREATE TABLE events (
  seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id            TEXT NOT NULL UNIQUE,       -- 客户端/Runner 生成的幂等去重键（ULID 或确定性复合键）
  stream_id           TEXT NOT NULL,              -- 'project:<id>' | 'task:<id>' | 'attempt:<id>' | 'global'
  project_id          TEXT,
  task_id             TEXT,
  attempt_id          TEXT,
  actor_kind          TEXT NOT NULL CHECK (actor_kind IN ('human', 'agent', 'runner', 'pa', 'ryugi', 'system')),
  actor_id            TEXT NOT NULL,
  kind                TEXT NOT NULL,              -- 规范枚举见 §2.5
  visibility          TEXT NOT NULL CHECK (visibility IN ('timeline', 'inbox', 'audit', 'raw_acp')),
  payload_json        TEXT NOT NULL,
  created_at          INTEGER NOT NULL
);

CREATE INDEX idx_events_stream_seq ON events(stream_id, seq);
CREATE INDEX idx_events_project_vis_seq ON events(project_id, visibility, seq);
CREATE INDEX idx_events_vis_seq ON events(visibility, seq);
CREATE INDEX idx_events_raw_gc ON events(visibility, created_at) WHERE visibility = 'raw_acp';

-- 消费者已读游标（PA、前端多设备、集成桥）
CREATE TABLE stream_cursors (
  consumer_id         TEXT NOT NULL,              -- 如 'pa:default'、'device:mobile'
  stream_id           TEXT NOT NULL,              -- 如 'global'、'project:demo-app'
  last_seq            INTEGER NOT NULL DEFAULT 0,
  updated_at          INTEGER NOT NULL,
  PRIMARY KEY (consumer_id, stream_id)
);

-- 严格准入的待拍板收件箱（同一事务随主实体变化同步维护，绝不放普通动态）
CREATE TABLE inbox_items (
  inbox_id            TEXT PRIMARY KEY,           -- 确定性 ID，如 'inb:apr:<approval_id>' 防重复挂卡
  project_id          TEXT NOT NULL REFERENCES projects(project_id),
  task_id             TEXT,
  category            TEXT NOT NULL CHECK (category IN (
                        'approval_pending',       -- 要你点头（ACP 权限 / 生产部署）
                        'delivery_review',        -- 产物待审（新交付就绪 / 新设计稿待选）
                        'task_blocked',           -- 失败卡住 / 等待 mac-node 物理资源 / 审批过期或崩溃窗口待核对
                        'memory_conflict'         -- 预留：跨项目记忆被事实推翻时的一键作废卡
                      )),
  ref_id              TEXT NOT NULL,              -- 指向 approval_id / delivery_id / attempt_id / memory_id
  title               TEXT NOT NULL,
  summary             TEXT NOT NULL,
  priority            INTEGER NOT NULL DEFAULT 2, -- 1=紧急审批 2=待审/阻塞 3=低优作废卡
  state               TEXT NOT NULL CHECK (state IN ('open', 'resolved', 'dismissed')),
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  resolved_at         INTEGER,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id)
);

CREATE INDEX idx_inbox_open ON inbox_items(state, priority ASC, updated_at DESC) WHERE state = 'open';

-- 全局 PA 注意力状态（遵守 4A：只存指针与游标，禁止存事实副本）
CREATE TABLE pa_state (
  pa_id               TEXT PRIMARY KEY DEFAULT 'default',
  active_wb_session_id TEXT,                      -- 当前复用的 PA ACP 会话 ID
  last_read_event_seq INTEGER NOT NULL DEFAULT 0, -- 上次已同步给 PA 的全局事件水位
  focus_pointers_json TEXT NOT NULL DEFAULT '[]', -- 最多 20 条：{kind, id, label, pinnedAt}
  updated_at          INTEGER NOT NULL
);

-- 必需大文件 blob 索引（content-addressed，不进 Git 仓库）
CREATE TABLE delivery_blobs (
  sha256              TEXT PRIMARY KEY,
  size_bytes          INTEGER NOT NULL,
  storage_path        TEXT NOT NULL,              -- '~/.local/share/hebi8-orbit/blobs/<aa>/<bb>/<sha256>'
  ref_count           INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL
);
```

### 2.5 核心事件类型与可见性分级表

| `kind` | `visibility` | 触发源 | 说明 |
|---|---|---|---|
| `task.created` / `task.state_changed` / `task.cancelled` / `task.child_spawned` / `task.child_updated` | `timeline` | Hub / PA / Human | 任务与子会话建单、状态流转与终止 |
| `attempt.started` / `attempt.state_changed` / `attempt.finished` | `timeline` | Hub / Runner | Attempt 起止、进入 `swarm_monitoring`、完成源 |
| `resource.waiting` / `resource.acquired` | `timeline` / `inbox` | Runner | 等待 `mac-node` 锁屏、登录态浏览器或模拟器占用 |
| `delivery.published` / `delivery.verified` | `timeline` / `inbox` | Runner / Hub | 新不可变交付生成、目标机核验 SHA 通过 |
| `annotation.created` / `annotation.addressed` | `timeline` | RyugiBridge / Human / Runner | 画布或 Diff 批注产生及后续轮次闭环回应 |
| `approval.requested` / `approval.decided` / `approval.expired` / `approval.consumed` | `inbox` / `audit` | Runner / Human / Hub | 高危审批全生命周期，永久留审计 |
| `deploy.started` / `deploy.verified` / `deploy.failed` | `timeline` / `audit` | SshDeployExecutor | `deploy-vps` 正向 SSH 部署与线上版本核对结果 |
| `acp.message_batch` / `acp.thought_batch` / `acp.tool_call` / `acp.unsolicited_update` / `acp.usage` | `raw_acp` | Runner | 原生 ACP 与 Swarm 明细；UI 默认折叠，PA 默认不读 |
| `pa.turn` / `pa.action_executed` / `hebi8.folded` | `audit` / `timeline` | PA / Hebi8 Runtime | PA 对话、代发控制命令与 Fold-Rollover 折叠记录 |

---

## 3. 防 GC 与配套一致性备份策略

单人自建最昂贵的故障不是中心停机 10 分钟，而是**半成功撕裂**（SQLite 有交付记录但 Git 对象被 GC 删了，或批注还在但快照目录没了）。因此三项状态绑定同一生命周期：

1. **Git 防 GC 锚定**：
   - 每次交付写入 `hub-node` 中心 bare mirror 仓库（`~/.local/share/hebi8-orbit/git/<project_id>.git`）的固定 ref：`refs/orbit/deliveries/<delivery_id>`。只要 ref 存在，`git gc` 永不回收其可达对象。
   - 当用户归档任务超过 90 天且该交付无 `open` 批注时，定时清理脚本才将 `deliveries.pinned = 0`、删除冻结快照目录（**同时保留 `.ryugi/artifacts/.state/` 与各 `<type>/<slug>/comments.json` 评论原文归档**，排除 `*.tmp`）并视磁盘水位决定是否清理旧 ref。
2. **三件套原子配套备份（定时同步到 `aux-node` 或外部备份库）**：
   - **第一步（暂停清理与排空同步锁）**：备份开始前调用 `POST /api/admin/gc-pause`：先阻止清理 `git_ref`/`blobs` 与新启动的 `RyugiBridge` 同步，**并等待所有在途同步完成 commit→ack 区间后才返回成功**，避免在线 SQLite 快照引用到随后被删的对象或出现「旧 SQLite + 已推进回执」撕裂。
   - **第二步（一致性热备）**：调用 `ncruces/go-sqlite3` 原生在线备份接口 `BackupInit` / `BackupStep`（或 `VACUUM INTO`）生成一致性 WAL 快照 `~/.local/share/hebi8-orbit/backups/orbit.sqlite`（绝不直接 `cp` 正在写的 `.sqlite` + `.sqlite-wal`）。
   - **第三步（整包增量同步）**：使用 `restic` 将以下五项作为一个快照整体备份到辅助备份节点，完成后释放维护锁：
     - `~/.local/share/hebi8-orbit/backups/orbit.sqlite`
     - `~/.local/share/hebi8-orbit/git/`（所有逻辑项目 bare mirror 及全部 `refs/orbit/deliveries/*`）
     - `~/.local/share/hebi8-orbit/blobs/`（大文件 content-addressed 存储）
     - `~/.local/share/hebi8-orbit/deliveries/*/.ryugi/artifacts/.state/` 与 `~/.local/share/hebi8-orbit/deliveries/*/.ryugi/artifacts/*/*/comments.json`（排除 `*.tmp` 临时文件）
     - `~/.config/hebi8-orbit/`（`hub.yaml`、`projects.yaml`、`machines.yaml` 及凭据配置，严禁进公开代码仓）
   - **第四步（可验证恢复演练）**：从空目录恢复出 SQLite、bare mirror 与 blobs，校验 `PRAGMA integrity_check` 及历史 `deliveries` 的 `git cat-file` 与 blob `sha256`；随后按 `refs/orbit/deliveries/<delivery_id>` 重新导出快照（派生一致的 21 位 `.artifact-id`）、**回填已备份的 `comments.json` 与 `.state/`**、向 Ryugi 重新注册并启动轮询，确认能打开旧链接并成功新增评论与读回。

---

## 4. 项目与机器能力注册表 YAML Schema

个人实际配置文件存放于 `~/.config/hebi8-orbit/`（不进公开代码仓；改完可 `POST /api/admin/reload-config` 热加载并校验 Go struct schema）。仓库内 `configs/*.example.yaml` 提供可直接复制的占位模板；以下为对应结构示意：

### 4.1 `machines.yaml` 样例

```yaml
schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node (Primary Hub & Dev)"
    transport: local_ws
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev, linux-x64, docker-local, ryugi-host, pa-host]
    adapters:
      agy:
        bin: "/home/orbit/.local/bin/agy"
        defaultArgs: ["acp", "--stdio", "--process-per-session"]
        maxLiveWorkers: 8          # 实测单 worker ~230 MB RSS，上限 ~1.8 GiB
        idleCloseSeconds: 1200     # 20 分钟无活跃转 session/close
      claude:
        bin: "claude-agent-acp"
        maxLiveWorkers: 4
        idleCloseSeconds: 900
      codex:
        bin: "codex-acp"
        maxLiveWorkers: 4
        idleCloseSeconds: 900
    exclusiveResources: {}

  mac-node:
    displayName: "mac-node (macOS / Xcode / 登录态浏览器)"
    transport: remote_ws
    tailscaleHost: "mac-node.example.ts.net"
    tokenHash: "sha256:replace-with-sha256-of-runner-token"
    capabilities: [macos-env, xcode, ios-sim, logged-in-browser]
    adapters:
      agy:
        bin: "/Users/orbit/.local/bin/agy"
        defaultArgs: ["acp", "--stdio", "--process-per-session"]
        maxLiveWorkers: 6
        idleCloseSeconds: 900
      claude:
        bin: "claude-agent-acp"
        maxLiveWorkers: 3
        idleCloseSeconds: 600
      codex:
        bin: "codex-acp"
        maxLiveWorkers: 3
        idleCloseSeconds: 600
    exclusiveResources:
      macos_gui:
        probe: "macos_screen_unlocked"
        description: "macOS 桌面已解锁"
      chrome_logged_in:
        probe: "opencli_chrome_ready"
        description: "已登录 Chrome 浏览器就绪"
      xcode_sim:
        probe: "xcode_sim_available"
        description: "Xcode iOS 模拟器独占槽位"

  aux-node:
    displayName: "aux-node (Auxiliary Linux)"
    transport: remote_ws
    tailscaleHost: "aux-node.example.ts.net"
    tokenHash: "sha256:replace-with-sha256-of-runner-token"
    capabilities: [aux-linux]
    adapters:
      agy:
        bin: "/home/orbit/.local/bin/agy"
        defaultArgs: ["acp", "--stdio", "--process-per-session"]
        maxLiveWorkers: 3
        idleCloseSeconds: 600
    exclusiveResources: {}

  deploy-vps:
    displayName: "deploy-vps (生产部署目标)"
    transport: ssh_outbound        # 遵守单向隔离，仅由 hub-node 正向 SSH 连接
    sshAlias: "deploy-vps"
    capabilities: [deploy-node]
    adapters: {}                   # 不跑 Agent CLI
    exclusiveResources: {}
```

### 4.2 `projects.yaml` 样例

```yaml
schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    defaultModelConfig:
      model: "gemini-4-neon"
      thought_level: "high"
      mode: "auto"
    ideScheme: "cursor"            # 一键跳 IDE 默认协议：vscode | cursor | zed | antigravity
    # 能力路由锚点（静态表，按任务 required_capability 隐式选机）
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "/home/orbit/dev/demo-app"
        worktreeRoot: "/home/orbit/.local/share/hebi8-orbit/worktrees/demo-app"
      macos-env:
        machine: "mac-node"
        repoPath: "/Users/orbit/code/demo-app"
        worktreeRoot: "/Users/orbit/.local/share/hebi8-orbit/worktrees/demo-app"
      deploy-node:
        machine: "deploy-vps"
        remoteDir: "/srv/demo-app"
        remoteMirrorDir: "/srv/orbit-mirrors/demo-app.git"
        deployStrategy:
          kind: "ssh_push_and_switch"
          targetEnv: "deploy-vps:prod"
          verifyRunningVersionCmd: "curl -sf http://127.0.0.1:8080/api/version | jq -r .sha"
          deployCmdTemplate: "/srv/demo-app/scripts/deploy-sha.sh --mirror /srv/orbit-mirrors/demo-app.git --sha {{headSha}} --expect {{expectedCurrentVersion}}"
          healthCheckUrl: "http://127.0.0.1:8080/api/health"
          requireHumanApproval: true
    # 可选：跨机大文件白名单（不塞进 Git，走 content-addressed blob）
    extraArtifactGlobs:
      - "assets/mockups/*.png"

  mobile-client:
    displayName: "mobile-client"
    repoIdentity: "github.com/example-org/mobile-client"
    defaultBranch: "main"
    defaultCapability: "macos-env"
    defaultAdapter: "agy"
    defaultModelConfig:
      model: "gemini-4-neon"
      thought_level: "high"
      mode: "auto"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "/home/orbit/dev/mobile-client"
        worktreeRoot: "/home/orbit/.local/share/hebi8-orbit/worktrees/mobile-client"
      macos-env:
        machine: "mac-node"
        repoPath: "/Users/orbit/code/mobile-client"
        worktreeRoot: "/Users/orbit/.local/share/hebi8-orbit/worktrees/mobile-client"
```

---

## 5. API 与跨机协议契约

### 5.1 Web / Mobile ↔ Hub（REST + WS）

**设计风格**：写操作全部走标准 REST JSON（带 `Idempotency-Key` 请求头），实时推送走单条 **WebSocket (`/ws/client`)**（断线自动携带 `sinceSeq` 补读遗漏事件，彻底消除多订阅源竞态）。

| 分类 | 端点 | 关键契约 |
|---|---|---|
| **总览与收件箱** | `GET /api/bootstrap` | **在单个只读事务内**返回 `projects`、`machines`、`openInboxItems`、`setup` 与同一快照时刻的 `snapshotSeq`，确保后续 WS 衔接无缺口 |
| **任务管理** | `POST /api/projects/:projectId/tasks` | 必填 `goal`、`requiredCapability?`、`adapter?`、`config?`；支持 `Idempotency-Key`；路由解析后统一做权限护栏（PA 禁入 `ssh_deploy`） |
| | `GET /api/projects/:projectId/tasks` | 任务列表（含当前活跃 `attempt` 状态、`waitingReason`、未决批注数） |
| | `GET /api/tasks/:taskId/timeline?beforeSeq=&includeRaw=0` | 默认仅返 `visibility != 'raw_acp'` 的折叠时间线；点开胶囊按 `attemptId` 懒拉 `raw_acp` |
| | `POST /api/tasks/:taskId/followup` | 追加指令或切能力继续；若当前 Attempt 仍在 `running`/`swarm_monitoring`，可选 `mode: 'queue' \| 'fork_new_session'`，绝不往活跃旧 session 硬塞 |
| | `POST /api/tasks/:taskId/cancel` | 向对应 Runner 发 `attempt.cancel`（转 ACP `session/cancel`） |
| | `POST /api/tasks/:taskId/retry` | 安全重跑（用于 `failed` / `expired` / `waiting_resource`；若上一轮为 `needs_verification` 需前端显式确认已核对现场） |
| **交付与审阅** | `GET /api/deliveries/:deliveryId` | 交付元数据、`ryugiBindings`（含实测转换后的完整 URL）、变更文件列表、IDE 跳转 URI |
| | `GET /api/deliveries/:deliveryId/diff?file=` | 从冻结 bare mirror 直接读取 `git diff base_sha..head_sha`（`base_sha` 固定为本轮审阅基线） |
| | `POST /api/deliveries/:deliveryId/annotations` | 由 `deliveryId` 反查推导 `project_id`/`task_id`（忽略或拒绝矛盾字段）；新增 Diff 行评（带锚点与上下文哈希） |
| | `POST /api/tasks/:taskId/dispatch-annotations` | 事务内原子将所选批注 `open -> queued` 并建新 Attempt（或若当前轮活跃则入 `task_pending_inputs` 排队）；Attempt 失败/取消后释放未解决批注回 `open` |
| **审批** | `POST /api/approvals/:approvalId/decide` | 必填 `{ optionId, expectedAttemptId, expectedPayloadSha256, note? }`；校验 `optionId` 属原 `options_json` 且仅人类身份可调用（PA 返回 `403`）；不符或已过期返回 **`409 Conflict`** |
| **全局 PA** | `POST /api/pa/chat` | 每 PA 严格串行处理回合：先锁定 `H = MAX(seq)`，仅读 `(last_read_event_seq, H]` 增量并成功后仅推进到 `H` |
| | `POST /api/pa/brief` | 无状态快捷今日摘要（零会话历史负担） |
| | `POST /api/pa/reset-session` | 手动换新 PA 会话（保留 `focus_pointers_json`） |
| **Git & Blob** | `ANY /git/:projectId.git/*` | 受 Runner Token 鉴权的 Git Smart HTTP（开启受控 `http.receivepack` + 裸仓库 `pre-receive` 钩子强校验仅允许创建本次获准 `refs/orbit/deliveries/<deliveryId>`，同 SHA 幂等、异 SHA 冲突） |
| | `PUT/GET /api/blobs/:sha256` | Content-addressed 大文件上传/下载，写入时强制核对流式 SHA-256 |
| **实时事件流** | `WSS /ws/client?sinceSeq=<snapshotSeq>` | 握手严格校验 `Origin`；回放 `(sinceSeq, replayHeadSeq]` 期间客户端先缓冲实时推送事件，回放结束后按 `seq` 去重有序接续 |

### 5.2 Hub ↔ 远程 Thin Runner（WS 长连接、统一有序 Outbox 与状态对账）

**连接方向**：所有局域网 / Tailnet Runner（`hub-node` 本地、`mac-node`、`aux-node`、`laptop-node`）均作为客户端向 `wss://orbit.example.ts.net/ws/runner` 发起长连接（指数退避重连 `1s -> 2s -> 5s -> max 15s`，带抖动）。

#### 握手与状态对账（`runner.hello` / `hub.welcome`）

断线重连后第一帧必须完成**活体对账**，区分「同一进程的 Tailscale 瞬断重连」与「Runner 进程重启」，既补发丢失的审批决定，也准确标出确定过期 vs 崩溃未知窗口：

```jsonc
// Runner -> Hub
{
  "type": "runner.hello",
  "protocolVersion": 1,
  "machineId": "mac-node",
  "runnerInstanceId": "run_01J...",   // 每次进程启动生成新 UUID；变化即代表进程重启过
  "runnerVersion": "0.2.0",
  "liveAttempts": [
    { "attemptId": "att_01J...", "leaseToken": "...", "state": "running" }
  ],
  "livePendingApprovalIds": ["apr_01J..."],      // 当前内存里仍挂着 ACP Promise 的权限请求
  "handledApprovalDecisions": [                  // 最近已本地回写 ACP 但可能未送达 Hub 的决定回执
    { "approvalId": "apr_01K...", "decisionId": "dec_01K...", "outcome": "consumed_allowed" }
  ],
  "resources": {
    "macos_gui": { "state": "free" },
    "chrome_logged_in": { "state": "free" },
    "xcode_sim": { "state": "free" }
  }
}

// Hub -> Runner
{
  "type": "hub.welcome",
  "serverTime": 1795460000000,
  "heartbeatIntervalMs": 10000,
  "leaseTtlMs": 45000,
  "reconcile": {
    "ackedOutboxByAttempt": { "att_01J...": 42 }, // 告知 Runner 严格从 outboxSeq = 43 顺序补发
    "abortAttemptIds": ["att_01OLD..."],          // Hub 侧已判过期/取消的 attempt，Runner 立刻停止新操作
    "redeliverApprovals": [                       // 仅面向仍持有句柄的同 runnerInstanceId 重发待送达决定
      { "approvalId": "apr_01J...", "decisionId": "dec_01J...", "attemptId": "att_01J...", "payloadSha256": "...", "optionId": "allow" }
    ]
  }
}
```

#### 核心双工帧定义

| 方向 | 帧 `type` | 关键字段与幂等语义 |
|---|---|---|
| Hub → Runner | `attempt.dispatch` | `{ attemptId, taskId, projectId, leaseToken, adapter, wbSessionId, providerSessionId?, expectedSessionCwd?, desiredConfig, systemPromptAppend?, inputDelivery?: { deliveryId, gitRef, headSha, blobs[] }, prompt, requiredResources[] }`。Runner 用 `attemptId` 去重，重复收到相同 `attemptId + leaseToken` 直接 ACK。 |
| Hub → Runner | `attempt.cancel` | `{ attemptId, leaseToken }` → 触发 ACP `session/cancel` 与子进程清理。 |
| Hub → Runner | `approval.decision` | `{ approvalId, decisionId, attemptId, payloadSha256, optionId, decidedBy }` → Runner 按 `decisionId` 幂等去重；核对内存挂起句柄的 `approvalId + payloadSha256` 一致后回写 ACP `session/request_permission` 响应，再通过统一 Outbox 上报 `approval_ack`。若句柄已不在，回报 `approval.stale`。 |
| Hub → Runner | `session.close` | `{ wbSessionId, providerSessionId }` → 进入该 session 串行队列并关闭空闲 worker。 |
| Hub → Runner | `runner.config` / `runner.rpc` | 热下发每机 Agent 覆盖配置或按需触发 `probe \| test_agent \| install_bridge \| list_dirs \| check_repo`（详见 [onboarding-and-machine-setup.md](./onboarding-and-machine-setup.md)）。 |
| Runner → Hub | `runner.heartbeat` | 每 10s 发送 `{ machineId, runnerInstanceId, liveAttemptLeaseTokens[], resources, workerRssMb }`；Hub 仅续延仍处于活跃态且未过期的 `attempts.lease_expires_at`。 |
| Runner → Hub | `attempt.outbox_batch` | **每 Attempt 唯一有序通道**（统一承载事件、状态变更、审批请求、审批回执、交付发布、消息板帧）：`{ attemptId, leaseToken, items: [{ outboxSeq, eventId, frameType, payload, ts }] }`。 |
| Hub → Runner | `hub.outbox_ack` | `{ attemptId, ackedOutboxSeq, acceptedMode: 'active' \| 'late_history' }`。Hub 严格按连续序号推进，确认后 Runner 才裁剪本地 Outbox。 |

#### 统一有序 Outbox 连续水位与迟到回报仲裁

1. **禁止跳空 ACK（消灭消息丢失漏洞）**：
   - 某个 Attempt 产生的所有状态变更、审批请求、审批回执、交付发布与 `raw_acp` 事件全部写入同一个本地 SQLite Outbox（`~/.local/share/hebi8-orbit/runner-outbox.sqlite`，由 `ncruces/go-sqlite3` 驱动），分配严格单调递增的 `outboxSeq = 1, 2, 3...` 与固定 `eventId`。
   - Hub 收到 `attempt.outbox_batch` 时，在一个 `BEGIN IMMEDIATE` 事务内逐条处理：
     - 若 `item.outboxSeq <= attempt.last_acked_outbox`：视为重复重发，直接幂等跳过；
     - 若 `item.outboxSeq === attempt.last_acked_outbox + 1`：按 `eventId` 去重、执行业务投影、并将 `last_acked_outbox` 推进 `+1`；
     - 若 `item.outboxSeq > attempt.last_acked_outbox + 1`（出现缺口）：**立刻停止推进水位**，向 Runner 返回当前的连续 `ackedOutboxSeq`，促使 Runner 从 `ackedOutboxSeq + 1` 重发，绝不允许跨缺口 ACK 导致中间审批或状态帧被本地裁掉。
2. **断网期间发布交付的可靠补交**：
   - 若断网时本地已执行 `orbit deliver`：Runner 先把固定的 `(deliveryId, baseSha, headSha, manifest, blobs)` 持久化到本地表，并在本机仓库保留 `refs/orbit/local/<deliveryId>`；
   - 网络恢复后，Runner 必须先按**原固定 `deliveryId` 与 `headSha`**完成 Git ref 推送与 blob 上传（绝不重新 `commit-tree` 换 SHA），收到 HTTP 200 后才解锁发送 Outbox 中对应的 `delivery_published` 帧。
3. **迟到旧 Attempt 独立历史写入路径**：
   - 假设 `mac-node` 断网超过 `leaseTtlMs`（如 90s > 45s），Hub 将 `att_1` 判为 `expired`，随后用户手动重跑启动了 `att_2`。
   - 当 `mac-node` 重连并补交 `att_1` 的 `outbox_batch`（含 `delivery_published` 或 `state=succeeded`）时：
     - Hub 校验发现 `att_1` 已非活跃态或 `tasks.active_attempt_id !== 'att_1'`，自动切入 **`late_history` 独立写入路径**；
     - 仅写入带 `late_arrival: true` 的 `events` 审计记录和 `deliveries(late_arrival = 1)` 只读历史档案（不丢失已算出的工作成果）；
     - **严禁触碰当前主状态**：不修改 `tasks.state` / `tasks.active_attempt_id` / `tasks.latest_delivery_id`，**不向 `inbox_items` 挂新的待审卡，绝不把 `att_2` 正在处理的 `annotations` 改标为 `addressed`**。
