export type DisplayLogLine = {
  id: string
  time: string
  kind: string
  message: string
}

export function displayLogLines(output: string): DisplayLogLine[] {
  return output.split("\n").filter(Boolean).map((line, index) => {
    try {
      const value = JSON.parse(line) as Record<string, unknown>
      const kind = typeof value.type === "string" ? value.type : "output"
      const message = typeof value.message === "string"
        ? value.message
        : typeof value.error === "string"
          ? value.error
          : kind === "status" && typeof value.phase === "string"
            ? `Phase changed to ${value.phase}`
            : kind === "tool_call" && typeof value.tool === "string"
              ? value.tool
              : kind === "token_usage"
                ? `${Number(value.inputTokens || 0).toLocaleString()} input · ${Number(value.outputTokens || 0).toLocaleString()} output tokens`
                : kind === "pr_ready" && value.pr && typeof value.pr === "object" && "branch" in value.pr
                  ? `Branch ready: ${String(value.pr.branch)}`
            : JSON.stringify(value, null, 2)
      const timestamp = typeof value.time === "string" ? new Date(value.time) : undefined
      return {
        id: `${index}-${line}`,
        time: timestamp && !Number.isNaN(timestamp.getTime())
          ? timestamp.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
          : "",
        kind,
        message,
      }
    } catch {
      return { id: `${index}-${line}`, time: "", kind: "output", message: line }
    }
  })
}
