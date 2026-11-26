# 从零引导、多机配对与 Agent 配置闭环设计规范（hebi8-Orbit）

> **定稿日期**：2026-11-25  
> **配套参考**：Orbit v2 UX 规范（[ux-design-spec-v2.md](../ux/ux-design-spec-v2.md)）  
> **目标**：打通「零门槛进入（本机一键起跑 / 加入已有网络） → 多机一次性配对动态注册 → 每机 Agent 自动探测/体检/安装桥接/配置 → 选项目与自然进入主工作台」完整闭环，同时保持对既有静态 YAML 配置与 SQLite 数据的 100% 无损向后兼容。

---

## 一、借鉴原则与克制裁剪

### 保留并落地到 Orbit 的成熟设计

| 设计模式 | Orbit 落点 |
|---|---|
| 无账号体系、本机优先；持有凭据即成员 | Desktop 首次启动默认「就在这台机器用」零输入一键起跑；多机用一次性配对码换长期凭据 |
| 配对弹窗：一条可复制命令 + 倒计时 + 实时等待「已连接」后衔接配置 | `PairDialog` + `/ws/client` 推送 `pairing.updated` / `machine.state_changed` |
| 每台机器上报自身 Agent 运行时与版本，支持按白名单固定版本远程安装桥接组件 | `AgentProbe`（`ready / needs_login / missing_bridge / not_installed / error / disabled / checking / installing`）+ `runner.install_bridge` |
| 能力位门控：旧版节点不发新控制帧，避免悬空等待 | `hello.capabilities` / `welcome.capabilities` 声明 `agents.v1` / `fs.v1` |
| 低刺激状态表达：静息态不打标、只标异常，延迟用安静墨色 | 严格沿用 Orbit v2 单槽 `StatusMark` 与 `MachinesPopover` 视觉语言 |
| 配置改完热生效、不重启进程 | SQLite 动态表 + `runner.config` WS 帧热应用（惰性启停驱动，存量任务不受影响） |
| 失败可重试、凭据与原文不进共享状态 | `AgentProbe` 只带脱敏摘要与 `hintCode`，绝不透传 token 或授权 URL |

### 明确砍掉（保持 Orbit 轻量纯 Go 单二进制定位）

- E2EE / TLS-PSK 密钥交接（传输加密由本机 loopback 与 Tailscale Serve 负责；后续对等网阶段见 [peer-mesh-and-v3-spec.md](./peer-mesh-and-v3-spec.md) 的 mTLS 设计）
- CRDT 多副本同步
- Standby 自动故障转移与 Hub 热迁移
- 多租户 / 组织 / 计费
- 成员间 OAuth / GitHub Token / env 凭据搬运表、自更新通道
- 成员直连终端 / 文件传输通道
- 引导动画、音效、Tour 导览、冗余引导仪式步骤

---

## 二、核心现状约束与兼容性红线

1. **Loopback 不可直接当信任依据**：`orbit-hub` 强绑定 loopback（`ValidateListenAddr`），Tailscale Serve 反代进来的远程请求 `RemoteAddr` 同样是 `127.0.0.1`。认证逻辑必须以显式凭据（Bearer client token / `Tailscale-User-Login` / `X-Orbit-Dev-Token`）为准，不靠 `RemoteAddr` 跳过校验。
2. **YAML 与 DB 和平共存，Hub / UI 绝不回写 YAML**：
   - `machines.yaml` 与 `projects.yaml` 继续完整兼容；
   - 新增 SQLite 动态表存放：配对码、动态注册机器、机器多凭据（支持为 `machines.yaml` 中已有但尚缺 `tokenHash` 的节点补发凭据）、客户端 Token、每机 Agent 稀疏覆盖配置、UI 添加的动态项目、全局 `hub_settings`；
   - 运行时有效注册表 `effective = YAML ∪ SQLite DB`，同 ID 时 YAML 显式字段优先且在 UI 显示锁定（尝试覆盖返回 `409 ERR_FILE_MANAGED`）；非 YAML 托管字段（`enabled`、`displayAlias`、`defaultModel`、动态凭据、实时探针结果等）可自由读写。
3. **无损迁移**：Schema 迁移仅做 `CREATE TABLE IF NOT EXISTS` 与 `ALTER TABLE ADD COLUMN`，升级前自动 `VACUUM INTO` 热备份 `orbit.sqlite`。若迁移时已有任务、非 `_scratch` 项目或 `≥2` 台静态机器，自动写入 `onboarding_done (reason="migrated")`，老环境升级后绝不拦截进引导页。
4. **ACP 桥接固定版本白名单**：
   - Claude 桥接：`@agentclientprotocol/claude-agent-acp@0.112.0`
   - Codex 桥接：`@agentclientprotocol/codex-acp@2.12.0`
   - 装进 Orbit 自管目录 `<dataDir>/acp-adapters`（`npm install --prefix`），不做全局 `npm -g` 污染，一律直调 `exec` 不经 shell。

---

## 三、四阶段端到端闭环架构

### 阶段一：零门槛入口与网络初始化

- **Desktop 首启（`desktop/setup.html`）两张卡**：
  1. **就在这台机器用（默认推荐）**：
     - 附着优先：先探 `127.0.0.1:<port>/api/health` 与单实例文件锁 `hub.lock`，若本机已有 Orbit Hub 运行（如用户级 systemd 服务）则直接附着不另起进程；
     - 否则自动执行幂等初始化（生成 `hub.yaml` + 本机 `local_ws` 机器与 `runner.token`）并托管拉起本机 `orbit-hub` + `orbit-runner`（`Setpgid` 进程组 + `--parent-pid` 看门狗 + `flock` 防双开）；
     - Desktop 用 `devToken` 调用 `POST /api/auth/handoff` 换取 30 秒一次性 `code`，加载 `http://127.0.0.1:<port>/#/auth/<code>`；SPA 换取 `ock_` 客户端 Token 存入 `localStorage` 并 `replaceState` 抹掉 hash 中的 `code`，全程免填 URL 与 Token。
  2. **连到已有的 Orbit**：
     - 单个输入框智能识别三种输入：`orbit join https://<hub> K7QM-2XPA` 整行、`https://<hub>/#/join/K7QM-2XPA` 链接、或纯 Hub 地址；
     - 勾选「也让这台机器执行任务」（默认勾选）：调用 `POST /api/pairing/claim` 一次同时签发 `clientToken`（写进 `client.yaml`）与 `runnerToken`（写进 `runner.yaml` + `runner.token`），随即拉起本机 `orbit-runner` 并等待上线后自动通过 handoff 进入主工作台；
     - 若只填纯 Hub 地址（无配对码）：沿用原 Tailscale Serve 直连行为。
- **平滑升级为多机共享**：在设置页「网络与机器」点击「开启多机共享」，探测本机 `tailscale` CLI 与 `tailscale serve status --json`（不覆盖已有非 Orbit 443 映射），确认后执行 `tailscale serve --bg` 并将新 Origin / 当前 Tailscale Login 写入 DB `hub_settings`（与 `hub.yaml` 取并集生效，不改 YAML）。

### 阶段二：多机配对与动态注册协议

- **配对 API**：
  - `POST /api/pairing/codes`（human 写权限）：`{ scope: "runner"|"client"|"both", bindMachineId?, ttlSec? }` → `{ id, code: "XXXX-XXXX", expiresAt, joinCommand, joinLink }`（明文 code 仅回显一次，库里仅存 SHA-256；40 bit Crockford Base32，最多 5 个并发有效码）。
  - `GET /api/pairing/codes/{id}`：查询 `pending | claimed | expired | revoked` 及已配对 `machineId` / 在线态。
  - `DELETE /api/pairing/codes/{id}`：吊销配对码。
  - `POST /api/pairing/claim`（免常规登录，以配对码本身为凭据）：拒绝任何带 `Origin` 的浏览器请求，要求 `X-Orbit-Request: 1`；若带 `Tailscale-User-Login` 且白名单非空则必须命中白名单；全局限速（10 次/分 → `429`，1 小时累计失败 50 次熔断 15 分钟）；原子 `UPDATE ... WHERE code_hash=? AND state='pending' AND expires_at>?` 防重放；支持 `bindMachineId` 为既有 YAML 机器补凭据，否则在 `registry_machines` 动态注册并返回 `ort_` / `ock_` token。
  - `POST /api/auth/handoff`（需有效认证）→ `{ code, expiresAt(30s) }`；`POST /api/auth/handoff/claim` → `{ clientToken }`（允许浏览器同源调用以换取 `ock_` token）。
- **认证优先级（`authenticate()`）**：
  1. `Authorization: Bearer ock_...`（查询 `client_tokens` 哈希且未吊销；**无效直接 401，不向下回落**）；
  2. `Tailscale-User-Login`（现有逻辑不变，浏览器写请求仍要求精确 `Origin`）；
  3. `X-Orbit-Dev-Token` / `DevOpen`（loopback 限定）。
  - `ort_` 仅用于 `/ws/runner` 与 `/api/blobs`，且 `verifyRunnerAuth` 同时校验 YAML `tokenHash` 与 DB `machine_credentials`（任一匹配即通过）。

### 阶段三：每机 Agent 自动探测、体检与热配置

- **Runner 探测管线**：
  - 启动时抓取登录 shell `PATH`（`$SHELL -lic 'printf %s "$PATH"'`，3s 超时，解决 LaunchAgent/systemd 缺 fnm/nvm/homebrew 路径问题）；
  - 按优先级查找桥接二进制：`CLI flag → 本地 machines.yaml → Hub 下发配置 → <dataDir>/acp-adapters/node_modules/.bin → PATH`；
  - 判定状态：`disabled` / `not_installed` / `missing_bridge`（主 CLI `claude`/`codex` 存在但缺 ACP 桥接） / `error`（ACP `initialize` 失败） / `ready`（`initialize` 成功）；`needs_login` 由主动测试或真实 attempt 鉴权报错触发；
  - `runner.hello` 与 `runner.agents_report` 上报完整 `[]AgentProbe`；Hub 下发 `runner.config` 热更新驱动配置，下发 `runner.rpc`（`probe | test_agent | install_bridge | list_dirs | check_repo`）接收 `runner.rpc_result`。
- **管理 REST API**：
  - `GET /api/machines`、`PATCH /api/machines/{id}`、`DELETE /api/machines/{id}`
  - `POST /api/machines/{id}/agents/probe`、`POST /api/machines/{id}/agents/{adapter}/test`、`POST /api/machines/{id}/agents/{adapter}/install`、`PATCH /api/machines/{id}/agents/{adapter}`
  - `GET /api/machines/{id}/dirs?path=...`（限定 `$HOME` 子树、防 symlink 越界、最多 500 目录项，标出 `isGitRepo`）
  - `GET/PATCH /api/network/settings`、`POST /api/network/tailscale-enable`
  - `POST /api/projects`（经在线机器 `check_repo` 校验后动态注册 `source='ui'` 项目，自动建 bare mirror）、`DELETE /api/projects/{id}`（仅限 `source='ui'`）
  - `POST /api/onboarding/complete`

### 阶段四：顺滑衔接「开始使用」

- `BootstrapResponse` 包含 `setup: { needsOnboarding, readyAgents, onlineMachines, currentMachineId, workspaceName, tailscaleShare }`。
- Web 引导（`#/onboarding`）最多两步（仅在 `needsOnboarding` 为真且落地 `#/` 时触发）：
  1. **这台机器上的 Agent**：展示本机（及已加入节点）的 Antigravity / Claude Code / Codex 状态，支持一键「安装」「测试」「重新检查」与「添加其他机器」配对弹窗；
  2. **加一个项目（可选）**：选在线机器 → 浏览/粘贴目录 → 校验 Git 仓库一键添加，或点「先自由对话」直接进入。
- 新建线程页（`#/new`）升级：
  - 支持直观选择目标机器（仅在多机或自由对话/项目多锚点时显出）与该机就绪的 Agent（未就绪项置灰并在 `title` 提示原因）；
  - 智能预选顺序：项目锚定机器 → 本项目上次选择 → 当前客户端所属机器 → 第一台有就绪 Agent 的在线机器；
  - 若全网无就绪 Agent，顶部给出一行低刺激提示「还没有可用的 Agent · 去设置」。
- 工作台设置页（`#/settings`）：由左下角 `MachinesPopover` 底部「网络与节点设置」直达，统一管理空间名称、多机共享状态、机器列表、「+ 添加机器」配对弹窗、已声明但未配对机器的「连接这台机器」预绑定入口、每机 Agent 检测/安装/测试/展开配置。

---

## 四、CLI 与桌面端收口

- **CLI 子命令**（与 Web 共用同一套配对与配置 API）：
  - `orbit up [--no-runner] [--managed|--service]`：本机幂等初始化并启动 Hub + Runner
  - `orbit pair [--runner|--client|--both] [--machine <id>] [--no-wait]`：生成配对码并实时等待节点上线
  - `orbit join <hub> <code>` / `orbit join '<整行或链接>' [--no-runner] [--name <id>] [--service]`：零手动配置入网
  - `orbit discover`：手动扫描 Tailscale / 局域网可用 Orbit 实例
  - `orbit agents [list|check|test|install|enable|disable] ...`
  - `orbit machines [ls|rename|rm]`
  - `orbit projects add [dir] [--machine <id>] [--name <name>]`
  - `orbit service [install|uninstall|status] [hub|runner]`（支持 Linux systemd user 与 macOS LaunchAgent，自动识别已有 `orbit-hub.service`/`orbit-runner.service`）
  - `orbit init`：向后保留所有旧 flag，交互菜单收敛为 `[1] 本机起跑` / `[2] 加入已有网络` / `[3] 添加项目`
- **Desktop (`desktop/`)**：
  - `scripts/package-mac-app.sh` 同时打包 `orbit-hub`、`orbit-runner`、`orbit` 到 `Orbit.app/Contents/Resources/bin/`，令 `Orbit.app` 开箱即用无外置依赖；
  - `desktop/setup.html` 采用干净两卡（就在这台机器用 / 连到已有的 Orbit），全流程人话文案，无内部术语泄漏。
