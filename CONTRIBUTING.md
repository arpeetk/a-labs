# Contributing to Wren

Thanks for your interest in Wren — a CLI + Kubernetes control plane that runs
parallel, durable, sandboxed coding agents in the cloud.

This is the quick-start for contributors. The deep guide is
[`AGENTS.md`](AGENTS.md) — read it before making non-trivial changes; it covers
the repository layout, component flow, conventions, and the security posture in
full. (Yes, it's written for coding agents as much as humans — that's on brand.)

By participating you agree to our [Code of Conduct](CODE_OF_CONDUCT.md).

---

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | **1.26+** | see the PATH gotcha below |
| Docker | any recent | runtime image + kind |
| kind | v0.32+ | local Kubernetes for e2e |
| kubectl | 1.27+ | talk to the cluster |
| gh | any | GitHub auth for live-PR testing (optional) |

> **PATH gotcha:** a stale Go 1.17 may live at `/usr/local/go/bin/go` and shadow
> a usable Homebrew Go 1.26 at `/opt/homebrew/bin/go`. If `go version` is wrong,
> prefix commands with `export PATH="/opt/homebrew/bin:$PATH"` (or fix your
> profile). This only affects local dev — CI pins Go via `setup-go`.

**Module path** is `github.com/summiteight/wren`. The repo is hosted elsewhere;
the module path intentionally differs and is not `go get`-able externally.

---

## Build, test, lint

```sh
make build            # ./bin/wren (CLI); see the Makefile for the other binaries
make test             # go test ./...
make vet              # go vet ./...
make fmt              # gofmt -w .
make cover            # go test -cover ./...
```

Before opening a PR, make sure the same checks CI runs are green locally:

```sh
gofmt -l .            # must print nothing
go build ./...
go vet ./...
go test -race ./...
golangci-lint run     # see .golangci.yml (govet, staticcheck, errcheck,
                      # ineffassign, misspell)
```

`golangci-lint` v2 is required (config is v2 schema). Install it via
`brew install golangci-lint` or from the
[releases](https://github.com/golangci/golangci-lint/releases).

If you change `api/v1alpha1`, regenerate:

```sh
make generate         # zz_generated.deepcopy.go
make manifests        # config/crd/bases + config/rbac
```

### End-to-end

`make e2e` is the **keyless end-to-end gate** — the objective merge check that
every change must pass. It stands up a kind cluster, deploys the control plane,
runs a mock harness with **zero credentials and no repo**, asserts the run
reaches `Succeeded`, and tears down. It needs Docker + kind and runs in
< 10 min. See [`AGENTS.md`](AGENTS.md) §7 for the full local loop.

```sh
make e2e                 # full run + teardown
E2E_KEEP=1 make e2e      # keep the cluster up for debugging
```

---

## Conventions (the short version — see AGENTS.md §6 for depth)

- **Interface + real impl + fake.** External dependencies (Kubernetes, GitHub,
  the store) sit behind a small interface with a real implementation and an
  in-memory fake for tests. Business logic depends on the interface.
- **Hermetic tests.** Prefer no-network tests: controller-runtime `fake` client,
  `httptest`, local bare git repos. Inject `now`/`idgen`/clients via seams.
- **Errors.** Wrap with `%w` and context; export sentinel errors and map them at
  the transport boundary.
- **Coverage.** Ship tests in the *same change* as new code — do not defer. Keep
  coverage high on logic packages. Only `cmd/*` wiring and real-network glue are
  intentionally uncovered; call out any new uncovered spot.
- **Comments** explain *why*, not *what*, and reference the spec section a piece
  implements (e.g. "spec §5.7").
- **Security posture (do not regress).** The agent runner is untrusted: pods are
  hardened (non-root, read-only rootfs, dropped caps, seccomp, no SA token). See
  [`SECURITY.md`](SECURITY.md) and AGENTS.md §6.

---

## Pull requests

1. **Branch** off `main`. Keep PRs focused; one logical change per PR.
2. **Sign your commits (DCO).** Every commit must carry a
   `Signed-off-by: Your Name <you@example.com>` trailer — add it with
   `git commit -s`. This certifies the
   [Developer Certificate of Origin](https://developercertificate.org/). PRs
   without sign-off will be blocked by the DCO check.
3. **Definition of done:** `gofmt` clean, `go vet` clean, `go test ./...` green,
   `golangci-lint run` clean, new code covered, and the spec's living
   "Implementation status" block updated if behavior/scope changed.
4. **Describe the change** using the PR template: *what*, *why*, and *how you
   validated it*. Link the issue if there is one.
5. CI (build/test/vet/lint/govulncheck), the kind **e2e** gate, and CodeQL must
   pass before merge.

For anything larger than a bug fix, please open an issue first so we can agree on
the approach — this repo has a strong architectural spine and we'd rather align
early than ask you to rework.

---

## Reporting bugs & requesting features

Use the [issue templates](.github/ISSUE_TEMPLATE/). **Do not** file security
issues as public issues — follow [`SECURITY.md`](SECURITY.md) for private
disclosure.
