# 一等子会话视觉表达减法设计定稿与审阅记录（hebi8-Orbit v4）

> **记录日期**：2026-11-27  
> **配套本地 Ryugi 画板（`.gitignore` 忽略，独立外部 Ryugi 预览）**：`.ryugi/artifacts/artboard/orbit-ux-v4/canvas.json`  
> **前序基线（100% 完整保留，不得破坏）**：
> - [ux-design-spec-v2.md](./ux-design-spec-v2.md)
> - [peer-mesh-and-v3-spec.md](../architecture/peer-mesh-and-v3-spec.md#五逐页-ui--交互修正规范融合全部-17-条-ryugi-批注与-lody-输入框习惯)
> - [ux-subtraction-review.md](../archive/ux-subtraction-review.md)（历史确立依据）  
> **当前状态**：
> - 侧栏交互已拍板为 **方案 A**（父行左侧留白槽轻箭头 + 细竖线极简缩进子树，0px 挤占父标题）。
> - 主会话与子会话抽屉底部 `Composer` 已明确为**严格等高、顶线水平齐平、完全同构复用同一组件与配色层级**（关闭 Ryugi 审阅留存批注 `#1 / #2`）。
> - 正在执行 **Ryugi 画板（`orbit-ux-v4`）与当前 `web/src/` PoC 的双向对齐**，确保设计稿与实现零漂移。

---

## 一、已拍板决策与核心减法基线

### 1. 左侧栏方案拍板：**选定方案 A**

- **方案 A（悬停 / 轻箭头展开极简缩进子树）**：
  - 保持 248px（窄窗 220px）侧栏呼吸感，彻底废除 `2a32e64` 初版中挤压父标题的 `[↳ N]` 粗胶囊与彩色圆点。
  - 在父会话行原有左侧 22px 留白槽内放极淡 `› / ⌄` 轻箭头（悬停、展开或子会话活跃时显现），**0px 挤占父标题宽度**。
  - 父行行尾保持**唯一 14×14 `StatusMark`**（向上汇总子会话最高优先级状态：`failed > approval > running > ready > resting`）。
  - 展开后的子会话行仅保留 `1px` 细竖线 + `26px` 紧凑纯标题 + 行尾单槽 `StatusMark`（无彩色圆点、无 role 胶囊）；点击子行直接跳进父会话并同步拉开右侧分屏抽屉。

### 2. 统一低刺激状态语言（`StatusMark`）

- 彻底废除刺眼绿/橙/红圆点、彩色 `role` 胶囊、虚线 `内置` 徽标与字面状态词（「已完成 / 运行中 / 已结束」）。
- 统一单槽五态：`failed`（暗红警示标）、`approval`（克制琥珀手势微标）、`running`（低刺激点阵/轻呼吸标）、`ready`（6px 克制蓝点）、`resting`（纯留白，不画灰点、不写「已完成」）。
- 底层原生 subagent / swarm 成员仅在元信息末标注极淡纯文本「内置」，抽屉内给单行静默提示条。

### 3. 四处核心界面减法落点

1. **顶部入口克制化**：取消常驻厚横条 `SubsessionStrip`；在 `Session` / `Hebi8` 顶栏右侧（与 IDE / Diff 同层级）提供轻量计数下拉入口（活跃/待审批时带微标，全部静息时静音收纳；菜单内含 `+ 派发子会话`）。
2. **时间线派发与回传极简化**：
   - 派发节点采用与 `思考` / `过程` 同质感的 `1.5px` 左轨单行条目（`29px`：`StatusMark` + 标题 + 极淡元信息，整行点击即开分屏，去掉多行边框卡片与底排操作按钮）；整批子会话均已静息时自然收合为 `▸ N 个子会话 · 摘要`。
   - 异步回传（`child_result`）压成一行极淡连接条（`↳ 标题 · 已回传摘要 [+N −M]`），默认折叠不抢父回复，展开看干净格式化 Markdown + `分屏查看会话 ›`，**严禁裸吐 `<pre>` 系统唤醒词**。
3. **右侧无遮罩可拖拽分屏抽屉**：
   - 默认与父对话左右并排（可拖拽左边缘 `340–680px`），不随意变成全屏黑遮罩盖死父会话。
   - **单行 `40px` 顶栏**：左侧自绘轻标题下拉切换同级子会话（**严禁浏览器原生 `<select>`**），孙会话支持原位面包屑下钻；右侧仅留紧凑 `Diff` / `IDE` / `↗` / `×`。
   - 内含完整子会话对话流、内联审批放行卡片、真接管输入区（追问 / `⌘↵` 实时打断 / 停止 / 切配置），且与 `Source Review` 分栏审阅切换互不丢失状态。
4. **放大完整子会话页（`↗`）与 Hebi8 共存**：
   - `父会话 / 子会话` 直接融进现有 `PageHeader` 第一行单行面包屑，**取消额外叠层的 `.parent-crumb-bar`**，顶栏右侧提供 `分屏` 返回按钮。
   - `Hebi8` 支持跨项目子会话派发与 `Fold-Rollover` 折叠历史条和平共存。

---

## 二、Ryugi 审阅关键批注与主/子 Composer 强不变量

> 来源：`artboard/orbit-ux-v4` · Page `flows` · Board `sub-active-split`（两项批注已提升为永久工程不变量，画板与 `web/src/` 同步遵守）。

| # | 批注锚点 | 审阅反馈要点 | 根因与强制落地要求 |
|---|---|---|---|
| 1 | `flows / sub-active-split` · 右侧子会话抽屉底部 `.sub-composer-dock` | **子对话与主对话底部输入区视觉高度必须一致** | **根因**：初版设计稿中右侧抽屉 `.sub-composer-dock` 的上下 padding / 输入盒高度比左侧主对话 `.sess-composer-dock` 更矮，导致左右底栏顶部分隔线错位、视觉断层。<br>**不变量**：主对话与子会话抽屉底部输入区必须**严格等高且底栏顶线水平对齐**（共享同一 `Composer` 布局尺寸、内外边距与容器高度），左右并排时底部构成一条完整连续的基准线。 |
| 2 | `flows / sub-active-split` · 右侧子会话抽屉底部控制栏 `.sub-composer-left` | **子会话抽屉输入区视觉内容必须与主对话完全一致** | **根因**：初版设计稿中左侧主对话是 `var(--bg)` 底座 + `var(--panel)` 输入盒 + `Agent · Model · Effort · Permission` + 附件图标；右侧抽屉却画成了反色容器（`var(--panel)` 底座 + `var(--bg)` 输入盒）与另一套控件结构，导致主子会话割裂。<br>**不变量**：子会话抽屉与主对话**复用同一套 `Composer` 组件与视觉内容**（相同的背景/边框层级、相同的控制项结构与排版顺序、相同的 placeholder 规则与纯图标发送/停止/入队按钮），仅在抽屉较窄时做无折行溢出保护（如 `text-overflow: ellipsis`），绝不为主/子对话各造一套外观。 |

---

## 三、工程落点与 PoC / Ryugi 双向对齐清单

- **核心前端落点**（基于一等子会话底层基线 `2a32e64`）：
  - `web/src/session/SubsessionStrip.tsx`：移除常驻厚横条，改为顶栏紧凑下拉入口
  - `web/src/session/ChildSessionDrawer.tsx`：无遮罩可拖拽分屏 + 单行 `40px` 自绘标题切换顶栏（禁 `<select>`）+ 与主会话完全同构等高的 `Composer`
  - `web/src/session/Turn.tsx`：派发节点改单行左轨 + 全静息收合；`child_result` 改一行折叠干净 Markdown，去 `<pre>`
  - `web/src/components/Sidebar.tsx`：按**方案 A** 落地：去 `[↳ N]` 粗胶囊与彩色圆点，改左侧留白槽轻箭头 + 极简缩进子树 + 行尾唯一 `StatusMark`
  - `web/src/session/SessionPage.tsx`：放大完整子会话页将 `父 / 子` 融进 `PageHeader` 单行面包屑，移除 `.parent-crumb-bar`
- **不可破坏的基线**：
  - 保留 v2 / v3 全部既有体验（`Source Review` 多行/词段评 + `Checkpoint`、全局 `Artifacts`、3 层项目设置、普通文件夹紧凑变更条、路线 X 对等网设计）；
  - 保留一等子会话底层协议（`parent_task_id`、内置 5 工具 stdio MCP `orbit mcp-serve`、`orbit child` CLI、`child_result` 持久异步唤醒、跨机 steer）。
- **画板与代码双向同步**：任何上述组件或布局调整都须在 `.ryugi/artifacts/artboard/orbit-ux-v4/` 与 `web/src/` 之间双向保持同步，禁止推倒重写画板或丢弃既有功能。
