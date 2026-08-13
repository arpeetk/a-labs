# Verification guide

Use the narrowest gate that proves the changed contract, then run the full
merge set before publishing. All Go commands require Homebrew Go first on PATH:

```sh
export PATH="/opt/homebrew/bin:$PATH"
```

## Fast feedback

| Command | Proves |
|---|---|
| `make test vet check-assets` | Go behavior, static checks, generated install-manifest parity |
| `go test -race ./...` | Go tests plus race detection |
| `make cover` | Package-level coverage for review |
| `golangci-lint run ./...` | Repository lint policy |
| `govulncheck ./...` | Reachable Go vulnerability scan |
| `shellcheck -x hack/*.sh hack/lib/*.sh` | E2E script correctness, including sourced helpers |
| `kubectl kustomize config/default` and `kubectl kustomize config/production-gcp` | Kubernetes overlays render successfully |
| `gitleaks git . --redact` | Tracked history contains no detected credentials |
| `npm test && npm run build && npm audit` in `cmd/wren-desktop/frontend` | Frontend behavior, types, production bundle, dependencies |
| `make build-desktop` | Native Wails packaging and bindings |

## Kubernetes end-to-end gates

| Command | Proves |
|---|---|
| `make e2e` | Keyless CLI → API → operator → hardened pod success path on kind |
| `E2E_BAD_IMAGE=1 make e2e` | Diagnostic failure path |
| `E2E_EGRESS_ENFORCEMENT=off make e2e` | Explicit weaker-egress configuration |
| `make e2e-install` | Product install, core CLI workflows, and uninstall |
| `make e2e-pause-resume` | Verified pause checkpoint, controller restart, exact restore, negative restore |
| `make e2e-control-plane-recovery` | Postgres HA migration, API replacement, outbox replay, event journal |

The scripts own unique default cluster names and delete them unless
`E2E_KEEP=1` is set. Never delete an unrelated existing cluster.

## Real GKE gates

`make e2e-gke` and `make e2e-gke-checkpoint` use an existing GKE Standard
cluster. The safer disposable wrappers are:

```sh
make e2e-gke-provisioned
make e2e-gke-checkpoint-provisioned
```

The provisioned runner refuses to adopt an existing cluster with the same
name, tries explicit capacity pools, and verifies cluster deletion. Run these
only for cloud-sensitive changes: pod admission, egress lockdown, Workload
Identity, GCS FUSE, checkpoint restore, image architecture, or scheduling.

## Native workflow

After `make build-desktop`, connect the app to the same freshly built API used
by the test cluster. Exercise context switching, project creation, fleet
filters, run creation, structured logs, recovery journal, pause/resume, stop,
and confirmed deletion. Unit or browser-only tests do not replace this pass for
changes to Wails bindings or lifecycle UI.
