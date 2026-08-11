# Wren desktop

The Wren desktop app is the developer-facing control surface for the same
control plane used by the CLI. It is a Wails v2 application: Go owns the native
window and reuses `internal/client`; React renders a focused fleet/run/project
experience. The desktop never talks to Kubernetes and never receives runner or
provider credentials.

## Product shape

- **Fleet:** live-polling view across projects and users, with project/phase/scope filters.
- **Run workspace:** lifecycle state, restarts, namespace, PR, live per-container
  logs with bounded snapshots, verified checkpoint identity/integrity and
  recovery conditions, safe pause/resume, stop, and destructive delete.
- **Composer:** submit a feature or bug-fix task against a registered project,
  optionally overriding harness and base branch.
- **Projects:** register GitHub repositories and inspect their orchestration defaults.
- **Contexts:** use the same owner-only control-plane configuration as the CLI;
  tokens are retained in Go and never serialized into the webview.

This first vertical slice intentionally calls the real HTTP/JSON API. The next
desktop increments are interactive steering, durable conversation history,
review/diff artifacts, notifications, approval policy,
usage/budget views, and admin capacity/health controls. Those require matching
control-plane APIs; they should not be faked locally in the UI.

## Development

Prerequisites are Go, Node/NPM, the platform WebView toolchain, and Wails v2.

```sh
export PATH="/opt/homebrew/bin:$PATH"
go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0
cd cmd/wren-desktop
wails dev
```

Build a native application with `wails build` from `cmd/wren-desktop`. The
frontend can be checked independently with `npm install && npm run build`.
