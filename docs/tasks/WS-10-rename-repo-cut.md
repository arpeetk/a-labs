# WS-10: Rename + public repo cut

**Branch:** n/a (executes across repos) · **Size:** M · **State:** DRAFT
**Blocked on (all human):** final name · GitHub org created · license decision
(Apache-2.0 recommended). **Strictly last. Human drives; agent assists.**
*The rename script can and should be written early (it's testable any time —
"rename to `wrenx`" in a scratch worktree proves it); the cut itself is final.*

## Steps (settled — implementation-plan §WS-10)

1. **`hack/rename.sh <newname> <neworg> <newdomain>`** — mechanical, one
   commit: Go module path + imports; CRD group `wren.dev` → `<newdomain>`
   (+ `make manifests generate`); labels (`wren.dev/run`, `wren.dev/component`,
   `wren.dev/pool`); branch prefix `wren/`; binary names `wren*`; image names;
   env prefixes `WREN_*`; Helm chart name; every doc. Then `make test e2e`
   green. Write the script now; run it at cut time.
2. **Fresh repo** in the new org: copy the tree at HEAD (no history — the
   `a-labs` archive stays private); new `.gitignore` (Go/IDE/OS only — drop
   the Python/Expo remnants); LICENSE swap + NOTICE; curated initial commit.
3. **Gates before flipping public:** `gitleaks` on the final tree; full CI
   green in the new repo (secrets/settings configured: branch protection,
   Pages, GHCR permissions, tap repo); `wren quickstart` stranger-test against
   the public artifacts; SECURITY.md contact live.
4. Flip public in lockstep with the `oss-plan.md` Phase-7 launch checklist.

## Checklist for the human (before cut day)

- [ ] Name decided (checked: GitHub org free, domain purchasable, no
      collision with wren.io / WrenAI / trademark quick-search)
- [ ] Org + tap repo + Pages set up; GHCR enabled
- [ ] License call made (Apache-2.0 vs MIT)
- [ ] External security read of the egress path done (or consciously waived)
- [ ] Launch post drafted (the credential-boundary story)

## Definition of done

- [ ] Rename rehearsal: script run against a scratch name in a throwaway
      worktree, `make test e2e` green — BEFORE cut day.
- [ ] Cut executed; CI green publicly; quickstart verified from the public
      artifacts by someone who isn't the author.
