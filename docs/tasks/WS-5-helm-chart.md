# WS-5: Helm chart

**Branch:** `ws5-helm-chart` · **Worktree:** `../wren-ws5` · **Size:** M · **State:** DRAFT
**Blocked on:** WS-1 merged (manifests settle). *Orchestrator: confirm final
flag/secret names from WS-1/WS-2 before dispatch; if WS-2 hasn't merged,
template only the PAT path and leave App values commented with a TODO.*

## Design (settled — implementation-plan §WS-5)

- `charts/wren/`: CRDs in `crds/` (not templated); templates for operator +
  apiserver Deployments, apiserver Service, SAs, RBAC, optional NetworkPolicy
  (from `config/netpol/`).
- `values.yaml`: image repos/tags (one global registry prefix), operator flags
  (`egressEnforcement`, runtime image), apiserver flags (`store.type`,
  `store.dsnSecret`), credential secret names (`github.pat.secretName`,
  `github.app.secretName`, `anthropic.secretName`), namespace options.
- Source of truth remains `config/` kustomize for contributors; the chart is
  generated-by-hand from it — add a doc note about keeping them in sync, and a
  `make chart-lint` target (`helm lint` + `helm template | kubectl apply
  --dry-run=client`).
- CI: extend the e2e workflow (or a new job) to install via the chart on kind
  and run the WS-0 assertions against it.
- Rewrite `SETUP.md` install section around `helm install`.

**OUT:** publishing to GHCR (WS-6 owns release automation); bundled Postgres
subchart (external DSN only for now — note as follow-up); multi-namespace
tenancy templating beyond what `config/` already does.

## Definition of done (finalize at dispatch)

- [ ] `helm lint` clean; `helm template` output diff-reviewed against
      `kubectl kustomize config/default` (same objects modulo names/labels).
- [ ] kind install via chart passes the WS-0 e2e assertions (document the
      command sequence; add it as `make e2e-helm` if cheap).
- [ ] Values documented in the chart README (helm-docs or hand-written table).
