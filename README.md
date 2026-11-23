# hebi8-Orbit

Single-user, multi-machine agent project delivery and review workbench built with a pure-Go (`CGO_ENABLED=0`) core and a lightweight React web UI.

## Phase 0 Scope

- **Single Source of Truth Protocol (`internal/protocol`)**: Shared Go enums, SQLite `CHECK` constraints, YAML config schemas, and REST/WS DTOs with `go generate` TypeScript output (`web/src/generated/protocol.ts`).
- **SQLite WAL Store (`internal/store`)**: Pure-Go `ncruces/go-sqlite3` with single-writer `BEGIN IMMEDIATE`, read-only query pool, composite foreign keys, partial unique indexes, atomic state+event+inbox commits, and online `BackupInit`/`BackupStep` hot backups.
- **Zero-Pollution GitOps (`internal/gitops`)**: Temporary-index checkpoint packaging without modifying the repository index/working tree, dual baseline separation (`reviewBaseSha` vs `commitParentSha`), bare mirror `pre-receive` immutability guard, deterministic 21-char `.artifact-id` frozen snapshot export, and unified diff reader.
- **Hub & RyugiBridge (`internal/hub`, `cmd/orbit-hub`)**: Cookie-less Tailscale auth + exact `Origin` & `X-Orbit-Request` CSRF/CSWSH defense, REST + `/ws/client` gapless replay stream, single-flight `ryugi` CLI sidecar bridge with commit-before-ack receipt handling, content-addressed blob storage, and embedded SPA frontend.
- **CLI (`cmd/orbit`)**: `orbit deliver` and `orbit status` for local zero-pollution delivery and status inspection.
- **Web Workbench (`web/`)**: Low-noise 3-pane desktop workbench (Inbox + Projects + Machine strip / Task timeline & delivery cards / Resizable Ryugi + Shiki-highlighted `react-diff-view` inline annotation drawer + one-click IDE jump) plus mobile 3-tab layout.

## Configuration Paths

Private configuration and runtime data live outside the repository:

- Config directory: `~/.config/hebi8-orbit/` (`hub.yaml`, `machines.yaml`, `projects.yaml`)
- Data directory: `~/.local/share/hebi8-orbit/` (`orbit.sqlite`, `git/`, `blobs/`, `deliveries/`, `backups/`)

Copy `configs/*.example.yaml` into `~/.config/hebi8-orbit/` and replace the placeholder values.

## Build & Verify

```bash
# Generate TypeScript protocol types from Go source
make gen-types

# Run Go vet + race tests
make test

# Build frontend + single-binary hub & CLI (CGO_ENABLED=0)
make build

# Cross-compile linux/amd64 and darwin/arm64 (CGO_ENABLED=0)
make build-all

# Verify zero private information leaks
make check-privacy
```

## Local Development

```bash
./bin/orbit-hub --config-dir ~/.config/hebi8-orbit --dev-open
./bin/orbit deliver --project demo-app --repo /path/to/repo --summary "Phase 0 checkpoint"
./bin/orbit status
```
