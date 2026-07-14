# WS-2: GitHub App per-run tokens

**Branch:** `ws2-github-app` · **Worktree:** `../wren-ws2` · **Size:** L · **State:** DRAFT
**Blocked on:** WS-1 merged (frees `pod.go`); a test GitHub App created (human).
**Security-critical: strong-model or human review mandatory before merge.**
*Orchestrator: before dispatch, re-verify the pod.go/credential-mount state
post-WS-1 and finalize the file list; split step 0 into its own tiny PR.*

## Design (settled — implementation-plan §WS-2)

0. **Interface-freeze PR first, alone:** `spec.credentials.githubTokenSecret`
   on `AgentRunSpec` (+deepcopy, `make manifests generate`). Merge before the
   rest.
1. **Env → volume:** proxy credentials move from env vars (`secretEnv` in
   `pod.go`) to Secret **volume mounts**; `internal/egress` `Authorizer`s read
   the file per-request (kubelet refreshes mounted Secrets ≈1 min) so 1-hour
   installation tokens can rotate. Applies to both GitHub and Anthropic creds.
2. **Minting:** apiserver loads App creds (App ID + PEM) from a Secret
   (`--github-app-secret`); on `CreateRun`, coreapi resolves the repo's
   installation, mints a token scoped to that repo with
   `contents:write, pull_requests:write, metadata:read`, writes Secret
   `<run>-github-token` in the run namespace **owner-ref'd to the AgentRun**.
   New launcher capability `EnsureSecret`.
3. **Refresh loop** in the apiserver: re-mint for non-terminal runs at ~45 min;
   worklist rebuilt from the store on restart.
4. **Fallback preserved:** operator-level PAT secret remains for quickstart;
   per-run secret wins when present.
5. `SETUP.md`: App creation walkthrough; PAT demoted to quickstart-only.

## Scope guards

**OUT:** token support for non-GitHub SCMs; MCP credential injection; changing
the finalize/PR flow; egress route changes beyond credential sourcing.
**Hot files you will own:** `api/v1alpha1/agentrun_types.go` (step 0 PR),
`internal/controller/pod.go`, `internal/egress/auth.go`,
`internal/{github,coreapi,launcher}/*`, `cmd/wren-apiserver/main.go`,
`config/apiserver/*`, `SETUP.md`.

## Definition of done (finalize at dispatch)

- [ ] Unit: minting/scoping against the existing fake App API; auth-from-file
      reload behavior; pod-builder secret-mount matrix (per-run vs PAT vs none).
- [ ] `make e2e` green (PAT path — defaults unchanged).
- [ ] Live validation on kind with the real test GitHub App: run → PR, runner
      env contains no token, token Secret GC'd with the run. (Human provides
      App creds; never commit them.)
- [ ] Token-refresh path exercised (shorten the refresh interval via flag).
- [ ] Spec §5.7 status + README GitHub-creds row updated.
