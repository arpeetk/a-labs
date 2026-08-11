const liveLogPhases = new Set(["Running", "Pausing", "Finalizing"])

// Podless phases must not start a follow request: the API correctly returns
// 409 when no pod exists, but surfacing that expected lifecycle state as a
// streaming failure leaves a stale error after the run becomes live.
export function canFollowRunLogs(phase?: string) {
  return phase !== undefined && liveLogPhases.has(phase)
}

export function logPlaceholder(phase?: string) {
  if (phase === "Paused") return "Run is paused. Resume it to continue live output."
  if (phase === "Pending" || phase === "Provisioning") return "Waiting for the agent pod…"
  return "Waiting for container output…"
}
