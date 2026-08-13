import { describe, expect, it } from "vitest"
import { displayLogLines } from "./logLines"

describe("displayLogLines", () => {
  it("turns harness JSONL into concise display rows", () => {
    const rows = displayLogLines([
      '{"type":"status","time":"2026-08-11T16:33:02Z","phase":"running"}',
      '{"type":"message","time":"2026-08-11T16:33:03Z","message":"clone complete"}',
    ].join("\n"))

    expect(rows.map(row => [row.kind, row.message])).toEqual([
      ["status", "Phase changed to running"],
      ["message", "clone complete"],
    ])
    expect(rows[0].time).not.toBe("")
  })

  it("preserves ordinary and malformed output verbatim", () => {
    expect(displayLogLines("plain text\n{broken").map(row => row.message)).toEqual(["plain text", "{broken"])
  })

  it("summarizes high-volume tool and usage records", () => {
    const rows = displayLogLines([
      '{"type":"tool_call","tool":"git status --short"}',
      '{"type":"token_usage","inputTokens":1200,"outputTokens":45}',
      '{"type":"pr_ready","pr":{"branch":"wren/run-1"}}',
    ].join("\n"))

    expect(rows.map(row => row.message)).toEqual([
      "git status --short",
      "1,200 input · 45 output tokens",
      "Branch ready: wren/run-1",
    ])
  })
})
