# hebi8-Orbit 文档索引

本目录归档 `hebi8-Orbit` 的全部架构设计、通信契约、UI/UX 规范与阶段性历史记录。所有文档均遵守公开仓库脱敏规范（见根目录 [AGENTS.md](../AGENTS.md)）。

## 当前有效架构文档（`architecture/`）

| 文档 | 内容摘要 |
|---|---|
| [overview-and-stack.md](architecture/overview-and-stack.md) | 系统总览、网络拓扑、四界单真源（4A）、纯 Go (`CGO_ENABLED=0`) + `mygo` 桌面壳技术栈、内置子会话 MCP/CLI 与仓库结构 |
| [data-model-and-contracts.md](architecture/data-model-and-contracts.md) | SQLite WAL Schema（三类生命周期拆表、复合外键、部分唯一索引）、防 GC 与配套备份、YAML 注册表、REST/WS 与统一有序 Outbox 契约 |
| [acp-runner-and-cross-machine.md](architecture/acp-runner-and-cross-machine.md) | 薄 ACP Runner 状态机、5 条 ACP Spike 硬边界、三适配器隔离层、零污染跨机 Git Checkpoint、Ryugi Sidecar 桥接、独占资源闸门与正向 SSH 部署协议 |
| [onboarding-and-machine-setup.md](architecture/onboarding-and-machine-setup.md) | 零门槛本机起跑、一次性配对码与动态注册、每机 Agent 自动探测/装桥/热配置、CLI (`h8o`) 与 `hebi8-Orbit.app` 收口 |
| [peer-mesh-and-v3-spec.md](architecture/peer-mesh-and-v3-spec.md) | v3（Route X）去中心对等网（签名账本 + 确定性值班节点）、3 层 Project 模型（`Project → Part → Location`）、普通文件夹原位执行与对应 UI/交互规范（与 `ux/` 互为补充） |

## 当前有效 UX 规格（`ux/`）

| 文档 | 内容摘要 |
|---|---|
| [ui-ux-and-pa-interface.md](ux/ui-ux-and-pa-interface.md) | 全局信息架构、5 个一级主页面交互模型、移动端 3-Tab 视图，以及全局 PA 旁路控制口协议与 8 个白名单接口 |
| [ux-design-spec-v2.md](ux/ux-design-spec-v2.md) | v2 视觉与交互定稿：低刺激单槽 `StatusMark`、对话内嵌附件 / 全局 `Artifacts` / 项目级 `Source Review`（Checkpoint Diff）三类边界、GitHub PR 风格行内评与纯图标 Composer |
| [subsession-ux-v4-spec.md](ux/subsession-ux-v4-spec.md) | **v4 全量 UX 与一等子会话设计定稿**（收口 `orbit-ux-v4` 全部 60 条审阅结论：总览/侧栏/已归档、全站统一 Composer 与 Lody 单一微标、新建线程、可折叠 RightCard/IDE 铅笔编辑/全宽普通文件夹/Source Review/单块过程折叠、单层子会话与方案 A 展收及条件穿透、`↗/↵` 极简通信轨、Hebi8 无子会话与 `HEAD` 置顶 Git-Tree、项目设置与 Codex 风格目录选择器） |

> 注：v3 阶段新增的 3 层项目设置、普通文件夹紧凑变更条、Lody 风格双栏输入框与彩色角色图标规范统一收口在 [peer-mesh-and-v3-spec.md](architecture/peer-mesh-and-v3-spec.md#五逐页-ui--交互修正规范融合全部-17-条-ryugi-批注与-lody-输入框习惯)。

## 历史归档（`archive/`）

以下文档记录了早期阶段规划与实机降噪重审上下文，保留作决策溯源参考，具体行为以「当前有效」文档与现行代码为准：

| 文档 | 说明 |
|---|---|
| [roadmap-and-mvp-verification.md](archive/roadmap-and-mvp-verification.md) | V1 初期 Phase 0–3 分阶段路线图与黄金端到端/R1–R5 回归验收基准 |
| [ux-subtraction-review.md](archive/ux-subtraction-review.md) | V1.1 实机降噪减法评审记录（低刺激单槽 `StatusMark` 的确立过程） |
| [handoff-2026-11-25-ux-audit.md](archive/handoff-2026-11-25-ux-audit.md) | 2026-11-25 阶段性 UX 审计交接记录 |

## 本地 Ryugi 画板与 PoC 双向对齐说明

- 可交互 UX 设计画板保存在仓库本地 `.ryugi/artifacts/artboard/`（如 `.ryugi/artifacts/artboard/orbit-ux-v4/`），由 `.gitignore` 忽略，**不进入 Git 公开提交**。
- 预览与评审使用独立外部 Ryugi 实例，不触碰 `h8o-hub` 自身托管的 Ryugi Sidecar 进程。
- UI 结构变更遵循**增量演进与双向同步**：不在既有画板上推倒重写；改动 `web/src/` 交互/组件后同步更新活跃 Ryugi 画板，画板拍板的新交互落地 `web/src/` 时保持组件与像素同构（详见 [AGENTS.md](../AGENTS.md)）。
