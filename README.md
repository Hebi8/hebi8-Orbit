# hebi8-Orbit

Single-user, multi-machine agent project delivery and review workbench built with a pure-Go (`CGO_ENABLED=0`) core, a lightweight React web UI, a terminal CLI, and a thin pure-Go desktop client (`egoist/mygo`).

## Components

- **Hub (`orbit-hub`)**: Single-writer SQLite WAL (`ncruces/go-sqlite3`), static YAML project/machine registries, Git Smart HTTP mirror with `pre-receive` immutability guard, Ryugi sidecar bridge, PA runtime, and embedded React SPA.
- **ACP Runner (`orbit-runner`)**: Thin pure-Go ACP runner supporting **`agy`**, **`claude` (`claude-agent-acp`)**, and **`codex` (`codex-acp`)** via standard ACP (`github.com/coder/acp-go-sdk`), with dynamic cross-adapter `configOptions` mapping, per-session serialization, persistent SQLite Outbox, and zero-pollution Git checkpoints.
- **CLI (`orbit`)**: Fast terminal interface for daily workflows — `init`, `status`, `tasks` (`list`/`new`/`show`/`followup`/`cancel`/`retry`), `approvals` (`list`/`allow`/`reject`), `pa`, `artifacts`, `brief`, `deliver`, `doctor`, `open`, `version`.
- **Desktop Client (`desktop/` -> `orbit-client`)**: Thin pure-Go (`CGO_ENABLED=0`) desktop shell using `github.com/egoist/mygo` (`WKWebView` / `WebKitGTK` / `WebView2`) with native IDE deep-link handoff (`cursor://`, `vscode://`, `zed://`, `antigravity://`), cross-origin external link routing (Ryugi canvases), global PA shortcut (`CmdOrCtrl+Shift+O`), and built-in Hub setup screen.

## Configuration Paths

Private configuration and runtime data live outside the repository:

- Config directory: `~/.config/hebi8-orbit/` (`hub.yaml`, `machines.yaml`, `projects.yaml`, `client.yaml`)
- Data directory: `~/.local/share/hebi8-orbit/` (`orbit.sqlite`, `git/`, `blobs/`, `deliveries/`, `backups/`)

Copy `configs/*.example.yaml` into `~/.config/hebi8-orbit/` and replace the placeholder values.

## Build & Verify

```bash
# Generate TypeScript protocol types from Go source
make gen-types

# Run Go vet + race tests (core + CLI + desktop client)
make test
make test-desktop

# Build frontend + hub/runner/cli binaries (CGO_ENABLED=0)
make build

# Cross-compile server, CLI, and desktop client (linux/amd64, darwin/arm64, windows/amd64)
make build-all

# Verify zero private information leaks
make check-privacy
```

## CLI Quick Start

```bash
# Guided first-time setup (connect to Hub, scaffold a new Hub, join as Runner, or register a Git repo)
orbit init
orbit init --connect https://orbit.example.ts.net
orbit init --hub
orbit init --runner --hub-url https://orbit.example.ts.net --machine mac-node
orbit init --project .

orbit doctor
orbit status
orbit tasks list
orbit tasks new --project demo-app --adapter claude --effort high --goal "Add unit tests for auth flow"
orbit tasks followup <taskId> --message "Also cover expired tokens"
orbit approvals list
orbit approvals allow <approvalId>
orbit pa "What is blocking today's release?"
orbit artifacts
orbit deliver --project demo-app --summary "Checkpoint"
orbit open
```

## Releases & CI

- `.github/workflows/ci.yml`: Privacy hygiene check, protocol typegen verification, web build, Go race tests, and cross-OS builds (core + CLI + `mygo` desktop client).
- `.github/workflows/release.yml`: Triggered on `v*` tags (or manual dispatch) to publish versioned CLI/Hub/Runner archives (`linux/amd64`, `linux/arm64`, `darwin/arm64`, `darwin/amd64`, `windows/amd64`), Desktop Client (`Orbit.app` + `.dmg` + `.zip` on macOS, `.tar.gz` on Linux, `.zip` on Windows), and `SHA256SUMS.txt` to GitHub Releases.

## Documentation

Architecture specifications, communication contracts, UI/UX guidelines, and historical design records live in [docs/README.md](docs/README.md). See [AGENTS.md](AGENTS.md) for engineering constraints, verification commands, and design-sync rules.

## License

Apache-2.0 — see [LICENSE](LICENSE).
