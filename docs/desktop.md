# Wren desktop

The Wren desktop app is the developer-facing control surface for the same
control plane used by the CLI. It is a Wails v2 application: Go owns the native
window and reuses `internal/client`; React renders a focused fleet/run/project
experience. The desktop never talks to Kubernetes and never receives runner or
provider credentials.

## Product shape

- **Fleet:** live-polling view across projects and users, with project/phase/scope filters.
- **Run workspace:** lifecycle state, restarts, namespace, PR, readable live
  per-container logs with bounded snapshots, verified checkpoint
  identity/integrity, the durable execution journal, recovery conditions, safe
  pause/resume, stop, and destructive delete. Filters, containers, projects, and
  harnesses use visible segmented choices rather than native dropdowns.
- **Composer:** submit a feature or bug-fix task against a registered project,
  optionally overriding harness and base branch.
- **Projects:** register GitHub repositories and inspect their orchestration defaults.
- **Contexts:** use the same owner-only control-plane configuration as the CLI;
  tokens are retained in Go and never serialized into the webview.

The app calls the real HTTP/JSON API; it does not fake unavailable server
features locally. Planned product surface is tracked in the
[roadmap](roadmap.md).

## Development

Prerequisites are Go, Node/NPM, the platform WebView toolchain, and Wails v2.

```sh
export PATH="/opt/homebrew/bin:$PATH"
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
cd cmd/wren-desktop
wails dev
```

Build a native application with `make build-desktop`. The complete frontend
and native verification workflow is in [verification.md](verification.md).
