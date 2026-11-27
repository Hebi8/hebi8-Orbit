# hebi8-Orbit

Single-user, multi-machine agent project delivery and review workbench. Pure Go (`CGO_ENABLED=0`) core (`orbit-hub`, `orbit-runner`, `orbit`), embedded React SPA (`web/`), and thin pure-Go `egoist/mygo` desktop client (`desktop/`). Apache-2.0 public repository (`github.com/Hebi8/hebi8-Orbit`, default branch `main`).

Read [docs/README.md](docs/README.md) before changing architecture, protocol contracts, or UI/UX behavior.

## Code Map

- `cmd/orbit-hub/`: Central Hub binary (single-writer SQLite WAL, Git Smart HTTP mirror + `pre-receive`, Ryugi sidecar bridge, Hebi8/PA runtime, embedded `web/dist`).
- `cmd/orbit-runner/`: Thin ACP Runner (`agy`, `claude-agent-acp`, `codex-acp`), per-session serialization, SQLite Outbox, worktree/folder execution, Unix socket delegate proxy.
- `cmd/orbit/`: Terminal CLI (`up`, `pair`, `join`, `discover`, `tasks`, `child`, `mcp-serve`, `deliver`, `approvals`, `hebi8`/`pa`, `agents`, `machines`, `projects`, `service`, `init`, `doctor`, `open`).
- `cmd/gen-ts/`: Generates `web/src/generated/protocol.ts` from `internal/protocol`.
- `internal/`: `protocol/` (single source of truth for enums/DTOs/WS frames), `store/` (`ncruces/go-sqlite3` WAL), `hub/` (HTTP/WS/scheduler/child wakeup/hebi8-line/pairing/Ryugi), `runner/` (ACP driver/Swarm tracker/Outbox), `mcpserver/` (built-in 5-tool stdio MCP), `gitops/`, `discovery/`, `sshcfg/`.
- `web/`: React 19 + Vite 6 + TypeScript + Tailwind v4 SPA (`web/src/generated/protocol.ts` is generated — never edit by hand).
- `desktop/`: Separate Go module (`desktop/go.mod`) for `orbit-client` (`egoist/mygo` WebView shell, managed local Hub/Runner, SSH config IDE launcher, `setup.html`).
- `docs/`: All architecture specs, UX specs, and historical archives (indexed in [docs/README.md](docs/README.md)).

## Commands

```sh
make gen-types          # REQUIRED after editing internal/protocol; updates web/src/generated/protocol.ts
make test               # go vet + go test -race ./... (core + CLI)
make test-desktop       # go vet + go test -race ./... inside desktop/
pnpm --dir web build    # tsc -b && vite build (TypeScript check + SPA bundle)
make build              # gen-types + build-web + CGO_ENABLED=0 orbit-hub / orbit-runner / orbit
make build-desktop      # CGO_ENABLED=0 orbit-client
make check-privacy      # REQUIRED before commit; scans index blobs & untracked files
```

## Engineering & Privacy Rules

- **Pure Go, zero CGO**: All Go targets (including `ncruces/go-sqlite3` and `desktop/` `mygo`) build with `CGO_ENABLED=0`. Keep `orbit-hub` SQLite strictly single-writer WAL with `BEGIN IMMEDIATE` (`_txlock=immediate`) for multi-table writes.
- **Single protocol source**: Edit enums/DTOs only in `internal/protocol/` and run `make gen-types` immediately so `web/src/generated/protocol.ts` stays in sync.
- **Runtime config & data stay outside Git**: `~/.config/hebi8-orbit/` and `~/.local/share/hebi8-orbit/` never enter this repository; only sanitized `configs/*.example.yaml` templates belong in Git.
- **Public repo hygiene (Apache-2.0)**: Every commit MUST pass `make check-privacy`. Never commit real usernames, personal paths, Tailscale CGNAT IPs or real `*.ts.net` domains, personal hostnames, emails, or secrets. Use sanitized placeholders (`hub-node`, `mac-node`, `aux-node`, `deploy-vps`, `orbit.example.ts.net`, `user@example.com`, `/home/orbit`, RFC 5737 IPs) in all docs and tests.
- **Code discipline**: Follow Conventional Commits. Do not write defensive fallback branches, catch-alls, or compatibility layers for cases that cannot happen.

## Documentation Rules

- Archive all architecture designs, UX specifications, decision records, and handoffs exclusively under `docs/` using English ASCII filenames (Chinese prose is fine), and register every doc in [docs/README.md](docs/README.md). Never archive Orbit docs into external personal directories.
- Update the matching spec under `docs/` whenever changing protocol behavior, state machines, or UI/UX structure.

## Ryugi Artboards & PoC Bidirectional Alignment

- Interactive UX artboards live locally under `.ryugi/artifacts/artboard/` (kept in `.gitignore` — never commit heavy static artboard bundles or private demo fixtures to Git).
- Use an independent external Ryugi instance for design preview and review; never touch or interfere with Orbit's own managed Ryugi sidecar process.
- **Incremental edits only**: Never rewrite an existing artboard from scratch or drop previously confirmed features — always build incrementally on top of the latest baseline.
- **Bidirectional sync**: When changing `web/src/` UI layout or controls, update the active Ryugi artboard to match; when implementing newly approved artboard changes in `web/src/`, preserve pixel and component isomorphism so code and design never drift apart.

## Core UI & Visual Invariants (v4 Final)

All finalized rules live in [subsession-ux-v4-spec.md](docs/ux/subsession-ux-v4-spec.md). Do not regress these invariants:

- **Low-stimulus flat language**: Use semantic tokens in `web/src/index.css`. Every list/sidebar row has at most one trailing `StatusMark` slot (`failed > approval > running > ready > resting`; resting is pure blank whitespace). Never render loud colored dots, status pills, literal status text labels, or glowing/blurred shadows (Home urgent rows use `9px` radius + flat `2.5px` solid left arc only).
- **Navigation & Sidebar hierarchy**: Top primary nav has exactly 4 items (`总览` / `Hebi8` / `Artifacts` / `新建线程`); `已归档` lives at the bottom-left footer (replacing theme toggle; theme toggle stays in Settings & `⌘K`). Project groups use `12px` spacing + `1px` divider + folder icon; child session rows indent `8px`; row hover exposes a direct archive icon with in-place 2-step confirmation (no `···` submenu).
- **Unified `Composer` & single Lody indicator**:
  - Every desktop and mobile composer footer MUST keep the exact left-aligned order **`[🔗 引用] · [Agent] · [Model] · [Effort] · [权限图标]`** (permission is icon-only immediately after `Effort`: gray geometric icons for `Ask`/`Auto`/`Plan`, amber warning icon for `YOLO`/`Full`).
  - Left of the send button uses a **single `14px` ring + percentage badge** (New Thread shows Agent quota; active sessions/sub-sessions/Hebi8 show conversation context; clicking opens an upward popover showing context + `5h`/`7d` quotas and reset times).
  - Empty input keeps the send button truly `disabled`. Main session and split child-session drawer MUST reuse the exact same `Composer` component and token hierarchy at strictly equal height (`Δtop=0px / Δheight=0px`) with a continuous `1px` top divider across the drag sash.
- **Single-block process folding & permanent prose**: Across all views, collapse each turn's reasoning + tool calls into one `过程 · 思考与 N 步调用` block. Never hide or fold prior turns' final prose when a new turn starts.
- **Session, RightCard, Folder & Source Review**: Keep Session header free of `ssh <alias>`, code-diff buttons, and sub-session dropdowns; place code changes (`+N −M` / `Checkpoint`) inside the compact collapsible `RightCard`. IDE dropdown uses an inline pencil icon after `SSH · <name>` (no input shown by default). Plain-folder change strip is full-width (`100%`), transparent with neutral border/rail, and has no "收进 Artifacts" button. Source Review supports Unified/Split diff, revocable `Checkpoint` before send, full-width inline comments, and an `×` close button.
- **Sub-session v4 invariants**:
  - **Max depth = 1**: Child sessions MUST NOT spawn grandchild sessions; only sibling collaboration/communication is allowed.
  - **Sidebar Plan A**: Light expand chevron (`›`/`⌄`) in the parent row's existing left gutter (`0px` squeeze on parent title) + `1px` hairline minimal indented child rows with trailing `StatusMark` only. Opening a full child-session page auto-expands its parent branch; switching to another session MUST NEVER auto-collapse an expanded branch.
  - **Conditional status bleed-through**: Child status aggregates onto the parent row's trailing `StatusMark` ONLY when the parent branch is **collapsed**; when **expanded**, the parent row shows its own status only.
  - **Split drawer & full page**: No parent top-bar sub-session dropdown and no persistent `SubsessionStrip`. Split drawer uses a single `40px` bar with custom-drawn dropdown switcher (never native `<select>`) and `Maximize2` expand icon. Full child-session page integrates `parent / child` into `PageHeader`'s first-row breadcrumb (no `.parent-crumb-bar` and no redundant "分屏" button).
  - **Minimal cross-session communication rails & shared board**: Drop verbose labels ("同级会话通信" / "来自子会话" / "已回传摘要"); use unified `1.5px` rails — outbound (`↗`, neutral gray) and inbound (`↵`, muted accent; folded `child_result` expanding clean Markdown, never raw `<pre>`). Parent sessions with first-class child sessions expose the low-stimulus `消息板 →` entry inside `RightCard` to view/post on the shared board (`@child` / `@all`).
- **Hebi8 / PA invariants**: PA/Hebi8 does NOT attach child sessions (retains independent cross-directory session jump cards). Top-right keeps a single tree icon; "快速汇总" lives as an on-demand suggestion bar directly above the composer (auto-hides when unread top-level count is 0). Git-Tree places **`HEAD · 当前活跃段` at the top** with older nodes descending downward, merged nodes collapsed by default, no redundant pin/ID badges or footer hint copy, and `HEAD` turn count resets to 0 after manual fold.
- **Project Settings, Machines & Directory Picker**: No redundant default isolation policy in Project Settings (keep `Worktree` fixed label in New Thread only). Each Part and path supports independent edit/delete (deleting the last path keeps the Part empty). All directory browsing uses the Codex-style picker defaulting to `~` with a single editable breadcrumb path bar and consistent mouse/keyboard selection. Duty priority rows use a subtle left drag handle (`GripVertical`, no rank numbers or up/down buttons); all non-local machines (including offline, `yaml`, `ssh_outbound`, and `worker`) expand for management and support in-place 2-step removal.
