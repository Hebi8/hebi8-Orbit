# [历史归档] 使用者视角 UI/UX 降噪重审记录（hebi8-Orbit V1.1）

> **归档说明**：本文档为 2026-11-25 阶段性实机降噪减法评审记录，确立了全站低刺激单槽 `StatusMark` 与「使用者视角去噪」北极星原则。最新界面定稿与演进规范请以 [ux-design-spec-v2.md](../ux/ux-design-spec-v2.md)、[peer-mesh-and-v3-spec.md](../architecture/peer-mesh-and-v3-spec.md#五逐页-ui--交互修正规范融合全部-17-条-ryugi-批注与-lody-输入框习惯) 及 [subsession-ux-v4-spec.md](../ux/subsession-ux-v4-spec.md) 为准。

---

## 北极星原则

1. **从使用者日常动线出发，而不是把系统数据模型翻译成文字**：先问「早起回来扫一眼总览 → 问 PA / 开新线程 → 进某个 Session 跟进 → 审阅产物并回流修改 → 扫一眼左下角本机状态」每一步真正要看什么，其他全部删掉或收起。
2. **用单通道克制视觉标记代替字面状态标签**：彻底根除「需要注意 / 运行中 / 已结束」满屏字样与左侧一整列彩色/灰色圆点；静息已结束状态用**留白**表达，绝不挂图标也不挂文字。
3. **不重复表达同一份信息**：Home 不重复列两次同一批会话、PA 页不把历史列表在顶栏和右侧卡画两遍、Session 不在正文和右卡重复列协作与当前步骤、审阅浮层不把 4 层工具栏和重复批注列表叠在一起。

---

## 1. 全站低刺激状态视觉语言

废弃早期字面状态标签方案，确立单通道五态 `SessionMark`：

```ts
export type SessionMark = 'failed' | 'needs-you' | 'running' | 'ready' | null;
// 优先级：failed > needs-you > running > ready > null
// - failed    : task.state === 'failed' || activeAttemptState ∈ {'failed', 'expired'} || inbox(task_blocked 且 attempt 失败/待核对)
// - needs-you : inbox(approval_pending | task_blocked | memory_conflict) || task.state === 'waiting_approval' || activeAttemptState === 'needs_verification'
// - running   : task.state ∈ {'running', 'queued', 'waiting_resource'} || activeAttemptState ∈ {'leased', 'preparing', 'waiting_resource', 'running', 'swarm_monitoring'}
// - ready     : inbox(delivery_review) || task.state === 'in_review'
// - null      : 其余一律留白（resting）
```

### 统一组件 `StatusMark.tsx`（固定行尾单槽）

| `SessionMark` | 视觉 | 颜色 | Tooltip / `aria-label`（绝不渲染到界面正文） |
|---|---|---|---|
| `failed` | `CircleAlert` 12px (`strokeWidth={2.25}`) | `var(--bad)` | `出错了` |
| `needs-you` | `Hand` 12px (`strokeWidth={1.8}`) | `var(--accent)` | `等你确认` |
| `running` | 3×3 静态极细点阵 SVG（低刺激无动画） | `var(--muted)` | `进行中` |
| `ready` | 6px 实心圆 | `var(--accent)` | `有新产出` |
| `null` | **不渲染**（行尾在列表页改显相对时间，在侧栏保持留白） | — | — |

### 各处落点
- **Sidebar 线程行**：删除左侧所有 `.sd` 圆点与 `.sd.done`；标题左对齐，行尾只放 `<StatusMark />`（子任务状态向上汇总到根线程，一行最多一标，静息留白）。
- **Sidebar 顶栏与项目组**：删掉「总览」右侧重复橙点；删掉项目名前无信息量的首字母方块 `.pl`（只留 caret + 项目名 + hover `+`）。
- **Home / Project / ⌘K / BriefView 列表行**：行尾有 `mark` 时只画 `<StatusMark />`，无 `mark` 时才显示相对时间（二者互斥，不并排抢视线）。
- **Session / ReviewOverlay 顶栏**：不挂任何状态文字或状态点。
- **MobileTabs**：总览 tab 的提示点只在存在 `failed` 或 `needs-you` 时亮起（`running` 和 `ready` 不打扰）。
- **验收门禁**：全站 `web/src/` 中「需要注意 / 已结束 / 运行中」字面渲染数为 0。

---

## 2. 总览页（Home）信息减法

- **删掉**：
  - `.hello` 整行（问候语、日期与右上角独立 `Brief Me` 按钮）；
  - 「需要注意」「会话」两个区块标题与 `活跃中 / 全部` 切换（全量检索由侧栏、项目页和 `⌘K` 承担）；
  - 卡片左侧彩色竖条、所有「进入会话 →」按钮（改成整行点击进入）、Home 上的「审阅完成」按钮（统一收进审阅浮层）；
  - Home 中断行的「我已核对现场 + 重试」控件（核对现场必须进入会话看工作区再决定，避免 Home 随手误勾）。
- **改为**：
  - 顶部直出 PA 快捷输入框（placeholder `问 PA…`），右侧内嵌 `Zap` 图标按钮（tooltip「今日摘要」，全站统一不再用英文 `Brief Me`）+ 发送按钮。
  - 下方为**单一去重列表**（`openInboxItems` 与 `activeTasks` 按 `taskId` 合并去重，过滤掉 `mark === null` 的静息会话）：
    - 上半部分：需要你看的会话（`failed` / `needs-you` / `ready`），按优先级与更新时间排；
    - 一条无标题细分隔线；
    - 下半部分：安静在跑的会话（`running`），仅占单行、无按钮。
  - 行内容极简：
    - 审批行：第二行显示人话版 `actionName` + `targetEnv`（如 `部署至生产节点 · deploy-vps:prod`），原始命令收进可点开的 `<details>`（`查看命令`），按钮仅留 `批准部署 / 拒绝`；
    - 新产出行：简短摘要 + 单主按钮（v2 进一步收口为进会话再决定是否打开审阅）；
    - 中断/失败行：一行简短原因，整行点击进入会话核对；
    - 全部静息空态：一行极淡文字 `都安静着`（无灰点）。

---

## 3. PA 页、Home PA 入口与 `⌘K` 边界

| 入口 | 唯一职责 | 减法改动 |
|---|---|---|
| **Home PA 输入框** | 随口问一句 / 点 ⚡ 看今日摘要，回复就地展开 +「在 PA 页继续」 | 去掉问候语和独立 Brief Me 按钮；`BriefView` 去 `h5` 分组，改为统一列表 + `StatusMark` +「新产出」 |
| **PA 专属页** | 长期对话与历史切换 | **整块删掉右侧浮卡 `aside.fcard`**（及顶栏浮卡开关与移动端 dock），历史对话只保留顶栏 crumb 下拉一个入口（底部放 `+ 新对话` 与淡色 `重置上下文并新开`）；顶栏右侧只留 ⚡（今日摘要）与 `⌘K`；消息时间戳改为 hover 显示；删掉 Composer `.hint` |
| **`⌘K` 快唤** | 秒跳线程/项目 + 随口问 PA | 空输入时**最近线程排在最前**（优先活跃/带标线程，补齐近期静息线程）；项目行去掉不准确的状态文案；线程行尾改 `<StatusMark />` 或时间；动作精简为 `今日摘要 / 新建线程 / 总览 / PA / 切换深浅色`；删掉底部快捷键提示栏 |

---

## 4. Session 工作台减法

- **顶栏**：删掉冗余的 worktree 名与 SHA chip、`stateLabel`、「协作」按钮和重复图标；右侧仅保留高频动作。
- **Turn & Segments**：
  - `思考过程 · N 字` → `思考`（默认折叠，不显字数；浮层 `compact` 模式下不渲染）；
  - `工作组 · N 步 · M 完成` → `过程`（始终默认折叠；运行中时标题行直接显示 `◌ 动词 目标`，展开才看步骤列表，**删掉下方重复的 `.it.running` 行**）；
  - 删掉正文内嵌的重复协作条；
  - 产出行简化为 `产出 +21 −11 [查看]`（仅 `>=2` 版本时显 `v2`，SHA 进 tooltip，去行首灰点）；
  - 成功结束轮不渲染 `done-line`（仅失败/停止/过期轮保留说明）；
  - `RetryNotice` 改人话（「上一轮中途断了」「工作区可能有没提交的改动，看一眼再继续。」「看过了」），`errorCode` 进 tooltip。
- **QueueBubbles**：删掉 `.qhead` 整行、`#n` 和来源「你」（非人类来源才标 `批注` / `PA`）；操作按钮改为 hover 显出（`立即发` / `编辑` / `撤回`，`↑↓` 仅在 `>=2` 条时出现）；气泡下留极淡「下一轮发出」。
- **Composer**：后续在 v2/v3 进一步收敛为纯图标发送键与紧凑底栏（见 [ux-design-spec-v2.md](../ux/ux-design-spec-v2.md)）。
- **RightCard**：小标题中文化（`改动 / 设计稿 / 协作 / 参考`）；「改动」压成单行 `2 个文件  +21 −11`；「参考」为空时不占独立区块，底部放极淡 `+ 参考`；无改动/设计稿/协作时默认不自动展开，留出纯净单栏阅读体验。

---

## 5. 固定分栏审阅浮层（`ReviewOverlay` + `CodeReviewPane` + `ArtifactsPane`）

- **左侧头部合并为单一 40px 工具栏**（彻底删掉第二层 tab 条）；
- 文件数 `<= 2` 或移动端默认收起文件树；文件树收起时，diff 顶部的 sticky `.dhead` 兼作文件切换下拉；删掉静态说教文案；
- 删掉文件树底部的重复「待发送行评」列表；
- **设计稿 Tab（`ArtifactsPane`）与 Ryugi CSP 技术取舍**：因 `ryugi-artifacts` 对 `:8443` 响应强制携带 `Content-Security-Policy: frame-ancestors 'none'`（且跨端口 Origin 隔离），强行嵌入 `<iframe>` 会被浏览器直接拦截；因此保留 `:8443` 独立窗口打开能力，同时删掉三栏挤压布局与元信息键值表，切入设计稿 tab 与窗口回焦时自动静默同步批注；
- **右侧会话栏**：顶栏删掉圆点和状态文字，分栏比例按 tab 独立记忆。

---

## 6. 新建线程页 / 项目页 / 左下角主机状态

- **新建线程页**：删掉重复标题；3 个固定 Tab 保持 `0px` 位移；
- **项目页**：删掉 `.pl` 首字母方块与默认分支为 `main/master` 时的冗余提示；删掉冗余分组标题，改为单列表；搜索框仅在 `threads.length > 10` 时显示；
- **左下角单主机底栏与弹窗**：
  - 底栏 36px、无 `border-t`、`padding: 4px 12px`，行控件 28px 与顶部导航行同材质；
  - 正常连接时**不画绿点**，仅主机/空间名 + 右侧 tertiary 色 `tabular-nums` 延迟（如 `12 ms`）；断连/重连才分别用 `--bad`「未连接」 / `--warn`「重连中」；
  - 弹窗删掉内部版本号（放 tooltip）；仅在资源被占时以人话提示（如「iOS 模拟器占用」「macOS 锁屏中」）。
