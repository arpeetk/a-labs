# AGENTS.md — working in Wren

Read this file before changing the repository. Wren is a Kubernetes/GCP
software-factory control plane: the CLI submits a run, the API persists and
publishes it, the operator creates a hardened agent pod, and the pod restores a
workspace, runs a coding harness, checkpoints progress, and opens a pull
request.

The current architecture and contracts live in
[`docs/technical-spec.md`](docs/technical-spec.md). Documentation navigation is
in [`docs/README.md`](docs/README.md), and intentional gaps are listed once in
[`docs/roadmap.md`](docs/roadmap.md). Git history and pull requests—not planning
briefs in the tree—are the record of completed work.

## Toolchain

| Tool | Minimum/use |
|---|---|
| Go 1.26+ | all Go builds, tests, generation |
| Docker | runtime and harness images |
| kind 0.32+ | local Kubernetes end-to-end gates |
| kubectl 1.27+ | cluster inspection and product tests |
| gh | authenticated GitHub/release checks |

On the primary development machine, `/usr/local/go/bin/go` is stale. Put the
Homebrew toolchain first for every Go or make command:

```sh
export PATH="/opt/homebrew/bin:$PATH"
```

zsh does not split unquoted scalar variables. Do not store a whole command in a
string and invoke `$CMD`; use a function, an array, or write the command
directly.

The module path is `github.com/summiteight/wren`. The hosting repository name
may differ; imports intentionally use the module identity.

## Repository map

```text
api/v1alpha1/        AgentRun CRD API and generated deepcopy
cmd/wren/            CLI entrypoint
cmd/wren-apiserver/  HTTP control-plane entrypoint
cmd/wren-operator/   controller-runtime manager
cmd/wren-runtime/    multi-call in-pod runtime
cmd/wren-desktop/    Wails native app and React frontend

internal/apiserver/  transport handlers and error mapping
internal/coreapi/    project/run business rules and cluster reconciliation
internal/store/      memory and Postgres persistence, outbox, event journal
internal/launcher/   Kubernetes AgentRun/log bridge
internal/controller/ AgentRun lifecycle and hardened pod construction
internal/runspec/    versioned input contract mounted into a run pod
internal/harness/    mock, Claude Code, Codex, and OpenCode adapters
internal/podruntime/ hydrate, harness, gateway, checkpoint, proxy roles
internal/blob/       mounted checkpoint store and safe archive/restore
internal/egress/     allowlist proxy and credential injection
internal/gitwork/    go-git clone/commit/push
internal/github/     pull-request client; shared fakes are in githubtest
internal/finalize/   idempotent branch/push/PR workflow
internal/install/    install/uninstall orchestration and embedded manifests
internal/desktop/    native app backend

config/              kustomize source manifests and production overlays
build/               reproducible runtime/harness Dockerfiles
hack/                test gates only; never put product onboarding here
docs/                maintained architecture, operations, and standards
```

Primary flow:

```text
wren CLI/desktop → apiserver → store + launch outbox → AgentRun CR
  → operator → PVC + hardened pod
  → hydrate → harness → finalize
  ↘ gateway events / checkpoints / CR status → control plane
```

## Build and generate

```sh
make build             # CLI
make build-apiserver
make build-operator
make build-runtime
make build-desktop
make docker-runtime
```

After changing `api/v1alpha1`, run both generators and apply the regenerated CRD
to any live test cluster:

```sh
make generate
make manifests
```

After changing `config/`, refresh the embedded installer render:

```sh
make assets
make check-assets
```

Generated files are outputs, not alternate sources of truth. Do not hand-edit
`zz_generated.deepcopy.go`, generated CRDs/RBAC, Wails bindings, or frontend
build output.

## Engineering rules

Read the repository standards before implementation:

- [`docs/standards/code.md`](docs/standards/code.md)
- [`docs/standards/testing.md`](docs/standards/testing.md)
- [`docs/standards/review.md`](docs/standards/review.md)

The load-bearing rules are:

1. Keep external systems behind small interfaces. Business logic depends on
   `store.Store`, `launcher.Launcher`, and `github.Client`, not concrete clients.
2. Ship the real implementation and hermetic tests together. Use
   controller-runtime fake clients, `httptest`, and local bare Git repositories.
3. Wrap errors with `%w` and operation context. Map sentinel errors at transport
   boundaries; do not match human-readable strings.
4. Make retry behavior explicit. Deterministic failures terminate; transient
   infrastructure failures consume a bounded retry budget.
5. Preserve the security split: the harness is untrusted; proxy and checkpoint
   credentials must never enter its environment or mounts.
6. New CLI/UI surface must work end to end. Do not add placeholder commands,
   buttons, or configuration fields.
7. Comments explain invariants and threat boundaries, not project history.
   Prefer a stable spec section over an obsolete workstream identifier.
8. Keep tests and test doubles out of shipped packages when practical. Shared
   test utilities belong in an explicitly named test-support subpackage.
9. Preserve user changes in a dirty worktree and keep commits narrowly scoped.

## Security invariants

The harness pod path is hostile-input code. Do not weaken these invariants:

- non-root harness, read-only root filesystem, dropped capabilities, seccomp,
  no privilege escalation, and no service-account token;
- no GitHub/model/cloud secret in the harness container;
- default iptables egress confinement plus the startup bypass canary;
- checkpoint storage mounted only into trusted hydrate/checkpoint containers;
- archive extraction confined with `os.Root` and safe relative symlinks;
- no silent replacement of a lost uncheckpointed workspace;
- `--egress-enforcement=off` is an explicit, visible security downgrade.

Read [`SECURITY.md`](SECURITY.md) before changing pod construction, egress,
credentials, archive/restore, identity, or admission behavior.

## Verification

Use [`docs/verification.md`](docs/verification.md) to select the narrow gate,
then run the merge baseline before committing:

```sh
make test vet check-assets
```

For logic changes, also inspect coverage and use the race detector where
concurrency is involved:

```sh
make cover
go test -race ./...
```

`make e2e` is the keyless product gate on kind. It drives the real CLI → API →
operator → pod path and needs no provider credentials. Lifecycle, HA, and GKE
changes have dedicated gates listed in the verification guide. Use provisioned
GKE wrappers for disposable cloud tests; never adopt or delete an unrelated
cluster. Set `E2E_KEEP=1` only when retaining a failed environment is useful and
tear it down deliberately afterward.

Desktop changes require frontend tests/build, native packaging, and a manual or
computer-driven pass against the same live control plane. A browser-only test
does not validate Wails bindings.

## Definition of done

- behavior is implemented through the product path, not a dev-only script;
- focused tests cover success, failure, retry/idempotency, and security edges;
- `gofmt`, `go test ./...`, `go vet ./...`, and `make check-assets` pass;
- the relevant end-to-end gate passes for cross-component behavior;
- generated assets are refreshed where required;
- current docs describe what the code does, while future work appears only in
  `docs/roadmap.md`;
- no secrets, build output, temporary clusters, or obsolete planning artifacts
  are left behind.
