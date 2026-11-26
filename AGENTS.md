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

## Core UI & Visual Invariants

- **Low-stimulus status language**: Use semantic tokens in `web/src/index.css`. Every list/sidebar row has at most one trailing `StatusMark` slot (`failed > approval > running > ready > resting`; resting is pure blank whitespace). Never render loud colored dots, status pills, or literal status text labels.
- **Isomorphic, equal-height Composer**: The main session and child-session drawer MUST reuse the exact same `Composer` component, background/border token hierarchy, placeholder rules, and control layout. In split view, both bottom input docks MUST be strictly equal height with horizontally aligned top divider lines.
- **Sub-session v4 invariants** ([subsession-ux-v4-spec.md](docs/ux/subsession-ux-v4-spec.md)):
  - **Sidebar Plan A**: Light expand chevron (`›`/`⌄`) in the parent row's existing left gutter (`0px` squeeze on parent title) + `1px` hairline minimal indented child rows with trailing `StatusMark` only.
  - **Split drawer header**: Single `40px` bar with a custom-drawn dropdown switcher (never native `<select>`).
  - **Full child-session page (`↗`)**: Integrate `parent / child` directly into `PageHeader`'s first-row breadcrumb; never stack an extra `.parent-crumb-bar`.
