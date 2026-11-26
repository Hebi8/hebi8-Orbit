# [历史归档] V1 分阶段落地路线与验收标准（hebi8-Orbit）

> **归档说明**：本文档为 2026-11-24 项目建仓前制定的 V1 初期 Phase 0–3 落地路线图与 MVP 黄金端到端/R1–R5 回归验收基准，已基本完成落地。后续架构与 UX 演进请以 [overview-and-stack.md](../architecture/overview-and-stack.md)、[peer-mesh-and-v3-spec.md](../architecture/peer-mesh-and-v3-spec.md) 及 [subsession-ux-v4-spec.md](../ux/subsession-ux-v4-spec.md) 为准。

---

## 1. 分阶段落地路线图（约 2 周切分）

核心策略：**第 1 周先在 `hub-node` 用纯 Go 核心 + 轻量 React 前端跑通「不可变交付 + Ryugi 画布 + 轻量 Diff/跳 IDE + 原生 `agy` ACP 任务闭环 + `Brief Me`」**，第一周末即可投入日常项目使用；第 2 周叠加跨机 `mac-node`/`deploy-vps`、手机 PWA、全局 PA 控制口与 **`egoist/mygo` 极薄桌面壳**。

### Phase 0（Day 1–2）：纯 Go 地基 + 不可变交付与双审阅抽屉（纯本机闭环）

**目标**：尚不接入 ACP Runner 与跨机 Smart HTTP，即能用本机路径把任意 Git 仓库的改动（含未提交脏改动）无损打包成不可变交付，在桌面与手机上核验 Ryugi 三端口访问与轻量 Diff 行评。

- **交付物**：
  - 创建公开仓库 **`Hebi8/hebi8-Orbit`**，初始化 Go `1.27.3+` 模块、`web/`（React 19 + Vite + Tailwind + `react-diff-view` + `shiki`）与 `Makefile`（含 `check-privacy` 公开仓库隐私扫描）；
  - 落地 `internal/protocol` 单一真源（`go generate` 自动输出 `web/src/generated/protocol.ts` 与 SQLite 校验常量）+ `internal/store`（`ncruces/go-sqlite3` WAL + `_txlock=immediate` + 复合外键 + 三条部分唯一索引）；
  - 实现 `orbit deliver` 零污染打包（`mktemp -d` 临时索引 + `git read-tree` + 脏 submodule 检测 + 区分 `review_base_sha` 与 `commit_parent_sha`），Phase 0 直接推送至 `hub-node` 本地 bare mirror 路径，并在 `orbit-hub` 自动建一条归属明确的 `manual` Task/Attempt；
  - 部署 `hub-node` `tailscale serve` 三端口隔离（`:443` `orbit-hub` 无 Cookie 鉴权 + 精确 Origin/`X-Orbit-Request` 校验 / `:8443` Ryugi / `:8444` Artboard），实现不可替换冻结目录（源目录 `0555`、`.state` `0755`、21 位确定性 `.artifact-id`）与单飞 `RyugiBridge`（支持 `threads`/`changes` 落库后 `ack` 与 `asks`/`groups`/`rounds` 最新状态哈希）；
  - 落地桌面最小界面（编译后经 Go `embed.FS` 打入 `orbit-hub`）：项目列表 + 交付卡片 + 右侧双标签抽屉（Ryugi 预览卡/独立打开 + `react-diff-view` 行评 + `cursor://`/`vscode://` 一键跳 IDE）。
- **当期自洽验收**：
  1. 在 `hub-node` 脏工作区执行 `orbit deliver` → 原仓库 `git status` 零污染；连续两版交付共存时无 `.artifact-id` 冲突；若 Agent 已自行 commit 且工作树干净，网页 Diff 仍正确比对 `attempts.base_sha .. head_sha`（非空 Diff）；
  2. **手机三端口实测门禁**：用移动设备从 `:443` 打开 `:8443` Ryugi 页面 → 进入 `:8444` Artboard → 留评 → 刷新 → 在 `orbit-hub` 成功读回且幂等落库后 `ack`（据此决定是否开启 iframe 嵌入）。

### Phase 1（Day 3–6）：`hub-node` 纯 Go 薄 ACP Runner（`agy`）+ 任务时间线 + 收件箱 + `Brief Me`

**目标**：`hub-node` 单机跑通真实日常编码与批注回流闭环，并用无状态 **`Brief Me`** 提前验证统一事件日志的信息质量。

- **交付物**：
  - 实现 `cmd/orbit-runner` 与 `AgyAcpDriver`（基于 `github.com/coder/acp-go-sdk` `v0.20.0+`）；
  - 完整落地 5 条 Spike 硬边界与复审补强：
    1. 每 `provider_session_id` 单活跃 Attempt 独占拥有 + 按 `toolCallId` 合并的 `ActiveSwarmTracker` 观测 + 显式 `orbit deliver --final` / 静默超时转 `needs_verification`；
    2. 每 `wb_session_id` 单飞串行锁（`closing` 态防 `close` 撞 `dispatch`）+ `ensureSessionReady()` 强制核对 `cwd == WorktreePath` + `ResumeSession` 后单项 10s 超时校准 `SetSessionConfigOption`；
    3. `NewSessionRequest.Meta` 传入 `_meta.antigravity.systemPrompt.append` 注入极简交付契约；
    4. 审批完整状态机（`pending` / `approved_pending_delivery` / `rejected_pending_delivery` / `consumed_*` / `uncertain` / `expired`）+ 多并发审批恢复 `resume_state`；
    5. 统一每 Attempt 有序 SQLite Outbox（严格连续水位不跳空）+ 200ms `raw_acp` 合并落盘；
  - 落地中间主区折叠式任务时间线 + 3 类严格准入收件箱 + 批注原子 `open -> queued` 派发（失败/取消自动回滚）+ 顶栏无状态 **`Brief Me`** 按钮；
  - 浏览器 `/api/bootstrap` 单事务快照水位 + `/ws/client` 回放期实时事件缓冲去重。
- **当期自洽验收**：
  - 在 `hub-node` 用真实项目跑通「首轮任务（含原生 `agy swarm`）→ 在 Ryugi 与 Diff 各留 1 条批注 → 一键交回触发第二轮逐条回应 → 仍可回看第一版冻结画布与 Diff → 点 `Brief Me` 准确总结现状」，同时跑通下方自动化回归 R1–R3。

### Phase 2（Day 7–10）：跨机 `mac-node` 单二进制 Runner + 资源闸门 + `deploy-vps` 正向 SSH 部署（桌面验收）

**目标**：打通多台 Tailscale 机器的隐式跨机交付、断网续传、有状态资源等待与受审批保护的生产部署。

- **交付物**：
  - `orbit-hub` 暴露受 Runner Token 鉴权的 `/ws/runner`、受 `pre-receive` 强守门的 Git Smart HTTP（`/git/:projectId.git/*`）与 `/api/blobs/:sha256`；
  - 交叉编译 `darwin/arm64` 单二进制 `orbit-runner` + `orbit` 部署到 `mac-node` 并挂 `launchd` 常驻（`macos_screen_unlocked` / `opencli_chrome_ready` / `xcode_sim_available` 原子整组领锁 + `waiting_resource` 原轮持租等待 + 断网本地 Outbox 补交 + 迟到 Attempt 独立 `late_history` 通道）；
  - `orbit-hub` 落地 `SshDeployExecutor`（正向 SSH 推送对象至 `deploy-vps` 裸镜像 + 环境级互斥唯一索引 + 远端 `flock` 与运行中版本核对 + 双条件健康检查 + 崩溃恢复探针）。
- **当期自洽验收（桌面端闭环）**：
  - 完成 `hub-node -> mac-node` 跨机交付（含锁屏触发 `waiting_resource`、20s 短断网续跑无重无丢、90s 超租约后迟到回报仅入历史不覆盖新轮与批注）+ `deploy-vps` 正向审批部署（含篡改 SHA 返回 `409`、模拟切换期中断恢复核验运行版本与健康状态），通过自动化回归 R4。

### Phase 3（Day 11–14）：全局 PA 控制口 + `mygo` 桌面壳 + 手机 PWA + 第二适配器 + 备份演练

**目标**：上线全网随时可达的全局 PA 控制抽屉、`egoist/mygo` 极薄原生桌面壳、移动端 3-Tab PWA、按需第二 CLI 适配器与可验证的灾备恢复闭环。

- **交付物**：
  - 落地全局 PA Runtime（每 PA 串行锁 + 读前锁定上界 `H` 防跳过并发事件 + 4k 增量预算与折叠范围标记 + 8 个白名单控制接口 + 路由后置 `ssh_deploy` 硬拦截 + 手动换新会话）；
  - 落地 `desktop/` —— **`egoist/mygo`（WebView 模式）极薄桌面壳**：加载 `orbit-hub` 前端，提供全局系统快捷键呼出 PA 抽屉、系统托盘/菜单栏状态点、免弹窗原生直拉本机/SSH IDE；
  - 落地移动端 3-Tab PWA（收件箱首屏 / 项目 / PA，16px 输入框防 iOS 缩放）；
  - 接入第二/第三上游适配器（`claude-agent-acp` / `codex-acp`）及一次性「跨模型只读审阅」任务模板；
  - 配置每日维护锁 + `ncruces/go-sqlite3` `BackupInit`/`BackupStep` + `restic` 同步到 `aux-node`（覆盖 SQLite + bare mirrors + blobs + `.state/` + `~/.config/hebi8-orbit/`），跑通恢复脚本 R5。
- **当期自洽验收**：
  - 全量跑通下方 §2.1 完整黄金端到端场景（含 `mygo` 桌面壳、移动 PWA 与全局 PA）+ R5 从空目录恢复并重打开旧 Ryugi 链接完成留评读回。

### V1.1 延后清单

- 原生 iOS 客户端或局部 Native UI 评估；
- PA 上下文达到阈值时的自动轮换（后续演进为 Hebi8 Fold-Rollover 机制）；
- iOS Push / Web Push 弹窗推送；
- `memory_conflict` 记忆作废收件箱卡；
- 对多份历史 Ryugi 快照挂载常驻 `fs.watch`（采用活跃绑定单飞轮询 + 手动/回焦同步）。

---

## 2. MVP 黄金端到端验收场景与核心并发回归（R1–R5）

### 2.1 完整黄金端到端场景

1. **显式脏工作区导入 + 隐式跨机发起**：在项目页勾选导入 `hub-node` 脏工作区发起任务，验证原目录 `git status` 零污染；任务在 `hub-node` 隔离 worktree 产出交付 `dlv_1`（验证 `deliveries.base_sha` 固定为起步输入基线，即使 worktree 内有多次正常 commit，网页 Diff 也完整非空）。
2. **`mac-node` 资源诚实等待与 SHA 强核验**：任务转入 `macos-env`（需 `mac-node` + `macos_gui`）。先锁定 `mac-node` 屏幕，确认 Attempt 保持活跃态 `waiting_resource` 且页面直白显示等待原因；解锁后原轮自动唤醒，从 `hub-node` Smart HTTP 拉取并核验 `dlv_1` SHA 与大文件 blob 哈希，构建并产出含 `.ryugi/artifacts/` A/B 设计稿的 `dlv_2`。
3. **两档断网恢复（严格区分租约内 vs 超租约）**：
   - **3a（短断网续跑：断网 20s < `leaseTtlMs` 45s）**：在 `dlv_2` 已打包但未上报完成时断开 `mac-node` 网络 20s 后恢复，验证用原固定 `deliveryId + headSha` 幂等补传并通过连续 Outbox 水位送达，项目页**仅出现 1 条 `dlv_2`**；
   - **3b（超租约迟到隔离：断网 90s > `leaseTtlMs` 45s）**：另开测试任务，在 `att_1` 断网超租约后重跑 `att_2` 并开始处理批注，随后恢复 `att_1` 补交；验证 `att_1` 仅落入历史审计与 `late_arrival = 1` 档案，**不覆盖 `tasks.active_attempt_id` / `latest_delivery_id`，不挂新待审卡，绝不把 `att_2` 的批注标为 `addressed`**。
4. **双画布冻结与批注回流**：
   - 在移动 PWA 打开 `dlv_2`：进入 `:8443`/`:8444` Ryugi 给 B 方案留评，并在 Diff 侧栏留行评；
   - 点击打包发回后，事务原子将两条批注 `open -> queued` 并直接触发新一轮 Attempt（不绕 PA），产出 `dlv_3` 并逐条回应；
   - 切回历史 `dlv_2`，验证旧画布、旧 Diff 与旧批注锚点仍 100% 可打开且与 `dlv_3` 无 `.artifact-id` 冲突。
5. **PA 全网控制口零失忆与部署安全红线**：
   - 1–4 步全程绕过 PA 完成；在桌面壳按全局快捷键或在手机唤起全局 PA 问「刚才发生了什么，现在能部署吗？」——同时在 PA 思考期间人为插入一条新审批事件（`H + 1`）；
   - 验证 PA 准确说出 `dlv_1 -> dlv_3` 与批注闭环，且下一轮问 PA 时 **`H + 1` 事件被完整读到未被跳过**；
   - 让 PA 执行「直接部署到 `deploy-vps`」或尝试经 `create_task`/`followup` 绕路切到 `deploy-node`：验证路由后置拦截直接拒绝执行，PA 只能通过 `request_deploy_approval` 生成绑定 `dlv_3` 的待人类审批单；
   - 篡改 `input_sha` 提交审批返回 `409 Conflict`；点击合法审批后 `hub-node` 正向 SSH 将 `dlv_3` 推至 `deploy-vps` 镜像、加 `flock` 核验运行中版本并完成切换与双条件健康检查。

### 2.2 核心并发与边界自动化回归清单（R1–R5）

| # | 回归测试项 | 交错触发场景（必测最难竞态） | 通过标准 |
|---|---|---|---|
| R1 | **Swarm 尾帧撞新轮 + 独占所有权** | `agy` `att_1` 调用 `swarm add` 后返 `end_turn` 进入 `swarm_monitoring` → 尚未退出前用户发回批注或 followup | `att_1` 独占该 `provider_session_id`，所有 unsolicited 尾帧全归 `att_1`；新请求要么持久入 `task_pending_inputs` 排队，要么强制新开独立 session+worktree，绝不串轮；超 `swarm_idle_timeout` 转 `needs_verification` 且不复用旧 session |
| R2 | **`CloseSession` 撞 `dispatch` + `cwd` 与 config 校准** | 回收扫描触发 `CloseSession`（已入 `closing`）的瞬间收到同会话新 `dispatch`；另测换 worktree 派发与 `thought_level=low` 冷恢复 | 新 `dispatch` 排队等 `close` 完成后再 `ResumeSession`；换目录强制开新 session；`resume` 后重发并验证 `thought_level=low`（超时/被拒则 `failed` 并释放 `mac-node` 资源） |
| R3 | **审批双断点（瞬断重发 vs 崩溃未知窗口）** | ① Hub 批准落库后断 WS 10s，同进程 Runner 重连；② 模拟 Runner 回写 ACP 后未发 ACK 即 `kill -9` 重启；③ 决定未发前 `kill -9` 重启 | ① `hub.welcome.reconcile.redeliverApprovals` 补发，Runner 按 `decisionId` 幂等回写并保留批准/拒绝态；② 标 `uncertain` + `needs_verification`（禁标"安全重跑"）；③ 标 `expired` 可安全重跑 |
| R4 | **Outbox 缺口保护 + 迟到旧交付防污新批注** | ① 模拟跳过 `outboxSeq=41` 先送 `42`；② `att_1` 超租约过期后 `att_2` 接手批注，`att_1` 迟到补交交付 | ① Hub 拒绝推进水位，维持 `acked=40` 促使重发 `41`；② `att_1` 仅走 `late_history`，绝不挂收件箱卡或改动 `att_2` 的批注状态 |
| R5 | **三件套配套恢复 + 旧链接重开留评** | 备份期间验 GC 锁生效；从空目录恢复 SQLite + bare mirror + blobs + `.state/` + YAML | `PRAGMA integrity_check` 为 `ok`；按确定性 21 位 `.artifact-id` 重建旧快照并向 Ryugi 重新注册后，能打开旧交付链接、新增一条批注并在 Hub 成功读回 |
