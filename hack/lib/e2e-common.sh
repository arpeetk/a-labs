# shellcheck shell=bash
# hack/lib/e2e-common.sh — shared plumbing for hack/e2e.sh and hack/e2e-gke.sh.
#
# Not a standalone script: sourced by both e2e gates. Callers must set the
# following globals *before* sourcing (both scripts already define these as
# part of their own config block):
#   KCTX        kubectl context to talk to (kind-<cluster> / gke_<...>)
#   NS_SYSTEM   namespace the control plane is deployed into
# and must maintain these globals as the run progresses (used by the shared
# helpers below): PF_PID, RUN_ID, RUN_NS, WREN_CONFIG_DIR, STATUS, WREN, API.
#
# hack/ is dev/test tooling ONLY (code standards rule 8) — this lib has no
# product-surface use, it only exists to keep the two e2e gates from drifting
# (WS-16 A.1: they'd grown near-identical preamble/teardown/polling logic).

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

# k <kubectl args...> — talk to the cluster under test via $KCTX.
k() { kubectl --context "$KCTX" "$@"; }

# dump_diagnostics prints everything an operator needs to debug a failed run:
# control-plane logs, the AgentRun resource, and the agent pod's container logs.
# Reads NS_SYSTEM/RUN_ID/RUN_NS from the caller's globals.
dump_diagnostics() {
  warn "dumping diagnostics (control plane + run)"
  echo "----- operator logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-operator --tail=200 2>&1 || true
  echo "----- apiserver logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-apiserver --tail=200 2>&1 || true
  if [ -n "$RUN_ID" ] && [ -n "$RUN_NS" ]; then
    echo "----- AgentRun/$RUN_ID (yaml) -----"
    k -n "$RUN_NS" get agentrun "$RUN_ID" -o yaml 2>&1 || true
    echo "----- pods in $RUN_NS -----"
    k -n "$RUN_NS" get pods -o wide 2>&1 || true
    # The agent pod is labelled wren.dev/run=<run-id>. Dump every container.
    local pod
    pod="$(k -n "$RUN_NS" get pods -l "wren.dev/run=$RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [ -n "$pod" ]; then
      echo "----- pod/$pod describe -----"
      k -n "$RUN_NS" describe pod "$pod" 2>&1 || true
      for c in $(k -n "$RUN_NS" get pod "$pod" -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}{range .spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null); do
        echo "----- pod/$pod container=$c logs -----"
        k -n "$RUN_NS" logs "$pod" -c "$c" --tail=200 2>&1 || true
      done
    else
      warn "no agent pod found for run $RUN_ID (it may not have been scheduled)"
    fi
  fi
  echo "-------------------------"
}

# cleanup_common runs the teardown steps identical across both gates: stop the
# port-forward (before anything else, so a failed run never leaks it), dump
# diagnostics unless the run reached STATUS=ok, and scrub the throwaway CLI
# config dir. Callers still own their own trap/exit and cluster-vs-namespace
# teardown (kind delete cluster vs. kubectl delete namespace differ by gate).
cleanup_common() {
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  if [ "$STATUS" != "ok" ]; then
    dump_diagnostics
  fi
  [ -n "${WREN_CONFIG_DIR:-}" ] && rm -rf "$WREN_CONFIG_DIR" 2>/dev/null || true
}

# wait_for_apiserver_healthz <sleep-seconds> <max-attempts> — block until
# $API/healthz answers, or die. Also dies fast if the port-forward (PF_PID)
# has already exited. Reads/uses the caller's API and PF_PID globals.
wait_for_apiserver_healthz() {
  local interval="${1:-1}" attempts="${2:-30}" i
  for i in $(seq 1 "$attempts"); do
    if curl -fsS "${API}/healthz" >/dev/null 2>&1; then return 0; fi
    kill -0 "$PF_PID" 2>/dev/null || die "port-forward died before apiserver was reachable"
    sleep "$interval"
    [ "$i" = "$attempts" ] && die "apiserver /healthz never became reachable via port-forward"
  done
}

# poll_run_until_succeeded <sleep-seconds> — poll `wren run get $RUN_ID` until
# it reaches phase Succeeded (returns 0) or Failed (dies), or RUN_TIMEOUT
# elapses (dies). Reads WREN/RUN_ID/RUN_TIMEOUT globals; leaves the final
# phase in the caller's `phase` variable.
poll_run_until_succeeded() {
  local interval="${1:-3}" deadline get_out
  deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  phase=""
  last=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    get_out="$("$WREN" run get "$RUN_ID" 2>/dev/null || true)"
    phase="$(printf '%s' "$get_out" | sed -n 's/.*"phase"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    case "$phase" in
      Succeeded) log "run reached Succeeded"; break ;;
      Failed)    die "run entered Failed phase" ;;
      "")        : ;;
      *) if [ "$phase" != "$last" ]; then printf '    phase=%s\n' "$phase"; last="$phase"; fi ;;
    esac
    sleep "$interval"
  done
  [ "$phase" = "Succeeded" ] || die "run did not reach Succeeded within ${RUN_TIMEOUT}s (last phase='${phase:-<none>}')"
}
