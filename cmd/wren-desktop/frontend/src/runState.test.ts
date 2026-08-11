import { describe, expect, it } from "vitest"
import { canFollowRunLogs, logPlaceholder } from "./runState"

describe("run log lifecycle", () => {
  it.each(["Running", "Pausing", "Finalizing"])("follows logs while %s", phase => {
    expect(canFollowRunLogs(phase)).toBe(true)
  })

  it.each(["Pending", "Provisioning", "Paused", "Interrupted", "Succeeded", "Failed", "Canceled"])(
    "does not issue a live follow request while %s",
    phase => expect(canFollowRunLogs(phase)).toBe(false),
  )

  it("explains the expected podless paused state", () => {
    expect(logPlaceholder("Paused")).toContain("Resume")
    expect(logPlaceholder("Paused")).not.toContain("Unable")
  })
})
