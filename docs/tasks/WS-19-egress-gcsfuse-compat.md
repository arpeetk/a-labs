# WS-19: make the GCS FUSE mount work under default egress enforcement

**Branch:** `ws19-egress-gcsfuse-compat` · **Worktree:** `../wren-ws19` · **Size:** M
**State:** READY. **Security-relevant if the fallback path is needed** — treat
any change to `internal/podruntime/lockdown.go` with WS-1's level of care.

*Context: WS-18 (#30) proved the GCS FUSE checkpoint mount works live on real
GKE, but only with `--egress-enforcement=off`. Under the default `iptables`
mode, the mount hangs: the GKE-injected `gke-gcsfuse-sidecar` talks directly to
`storage.googleapis.com` + the metadata server, and the lockdown's uid-based
allowlist (`internal/podruntime/lockdown.go`) only accepts the egress-proxy's
own uid — everything else, including DNS, gets REJECTed. This is a known gap
logged in `docs/tasks/STATUS.md`'s WS-18 entry. Investigate first, don't
assume an answer — the owner and I already reasoned through this once (see
below) but reasoning isn't verification.*

## What's already decided (from the design discussion — don't re-litigate)

- **A pure destination-based allow is unsafe here and is explicitly rejected
  as an approach.** The harness shares the pod's network namespace with every
  sidecar. A lockdown rule that allows a destination (e.g.
  `storage.googleapis.com`) regardless of *which uid* is asking would let the
  untrusted harness reach that same destination too — defeating the point of
  the lockdown. Any allowlist entry added to `lockdown.go` must be scoped by
  **both** uid and destination, never destination alone.
- **The preferred fix routes GCS FUSE traffic through the existing
  egress-proxy, not around it.** `lockdown.go`'s rule #1 already accepts *any*
  uid's traffic to the proxy's own port on loopback (`-o lo -p tcp --dport
  $EgressPort -j ACCEPT`) — that's the exact mechanism the harness itself
  already uses to reach the proxy. If the GCS FUSE sidecar can be pointed at
  that same proxy (many Google client tools respect `HTTPS_PROXY`), the fix is
  just adding `storage.googleapis.com` (and whatever the metadata-server
  exchange needs) to the proxy's *existing* destination allowlist
  (`internal/egress/proxy.go`'s `Allowed()`/`Routes` — the same mechanism
  already used three times for github.com/api.anthropic.com/api.openai.com).
  This changes zero security-critical lockdown code.
- **The fallback, only if the above doesn't work:** a narrow, explicitly-uid-
  AND-destination-scoped exemption in `lockdown.go` for the GCS FUSE sidecar's
  specific uid. This is the security-sensitive path — treat it accordingly.

## Investigation (do this first — it decides which path you're on)

1. **What uid does GKE's `gke-gcsfuse-sidecar` actually run as?** Deploy a run
   with `--checkpoint-gcs-mount` enabled (as WS-18's hand-off/PR #30
   describes) against a real GKE cluster, `kubectl exec` or `describe` the
   pod, and check. Don't assume 0/root — verify.
2. **Does the sidecar (or the underlying `gcsfuse` binary it wraps) honor
   `HTTPS_PROXY`/`HTTP_PROXY`, or expose a `mountOptions`/`--proxy`-style CSI
   volume attribute?** Check GKE's Cloud Storage FUSE CSI driver docs for
   supported `volumeAttributes` beyond `bucketName` (already used in
   `pod.go`) — there may be a documented `mountOptions` attribute that passes
   flags through to `gcsfuse`, or the sidecar container's own env may just be
   standard-Go-HTTP-client-respects-proxy-env-vars (verify, don't assume
   either way).
3. **What does the metadata-server credential exchange (169.254.169.254)
   actually look like on the wire from inside this pod?** GKE Workload
   Identity sometimes intercepts this at the node level rather than routing
   it like normal pod-egress traffic — if so it may not even be subject to
   the same OUTPUT chain rules the same way regular internet-bound traffic
   is. Observe it directly (e.g. watch iptables counters, or check whether
   the self-check fails specifically on the metadata call vs. the
   storage.googleapis.com call) rather than assuming.

## Two possible outcomes — implement whichever the investigation supports

**A. Proxy-routing works:** wire the CSI volume's `mountOptions` (or
whatever mechanism step 2 found) to point the sidecar's traffic at
`127.0.0.1:$EgressPort`; add `storage.googleapis.com` (and the metadata
server if it turns out to need explicit allowlisting rather than being
node-intercepted) to `internal/egress/proxy.go`'s allowlist, following the
existing pattern exactly. No changes to `lockdown.go`. Live-verify: the
WS-18 mount self-check passes under **default** `--egress-enforcement=iptables`
(not `off`) on a real GKE cluster.

**B. Proxy-routing doesn't work (document exactly why):** add a narrowly
scoped lockdown rule — uid-match on the GCS FUSE sidecar's specific
(verified, not guessed) uid, **destination-scoped to the specific IP
ranges/CIDRs the investigation identifies** for `storage.googleapis.com` and
the metadata server, not a blanket "this uid may reach anything" rule (that
would just recreate the proxy-uid's own unlimited-reach property for a
second identity, which the design discussion explicitly wants to avoid).
Pin the invariant in code with a comment explaining exactly why this specific
exemption exists and why it's safe (code standards rule #1); add a test
(mirroring `lockdown_test.go`'s existing pattern) proving the harness's own
uid still cannot reach those same destinations even though this new rule
exists.

## Scope guards

**OUT:** the real periodic checkpointer (still deferred, WS-8); any change to
the harness/checkpointer trust-tier boundary from WS-18 (mount stays
checkpointer-only); enabling `--checkpoint-gcs-mount` by default anywhere;
any change to `--egress-enforcement=off`'s behavior.
**Hot files:** `internal/podruntime/lockdown.go` (only if outcome B),
`internal/podruntime/lockdown_test.go`, `internal/egress/proxy.go` (only if
outcome A), `internal/egress/proxy_test.go`, `internal/controller/pod.go`
(CSI volume attributes, only if outcome A needs new ones), `docs/tasks/STATUS.md`
(update the WS-18 entry's "known gap" note once this closes it).

## Definition of done

- [ ] The investigation's three questions above are answered with evidence
      (not assumption) in the hand-off, regardless of which path gets taken.
- [ ] **Live proof on real GKE:** the WS-18 mount self-check (checkpointer
      startup Put/Get/List round-trip, independently verified via
      `gcloud storage cat` like WS-18's own proof) passes under the
      **default** `--egress-enforcement=iptables` — the whole point of this
      workstream is that `off` is no longer required.
- [ ] If outcome B: a test proves the untrusted harness's uid still cannot
      reach the newly-allowed destinations despite the new rule.
- [ ] `make test vet` + lint green; `make e2e` unaffected (both enforcement
      modes stay green — this must not regress WS-1's existing coverage).
- [ ] Real GCP infra (cluster/bucket) created for the live proof is torn down
      after, same discipline as every prior GCP-touching workstream.
- [ ] `docs/tasks/STATUS.md`'s WS-18 entry updated: the "known gap" note is
      either removed (outcome A/B both close it) or, if genuinely
      unresolvable, re-stated with the concrete reason why.
