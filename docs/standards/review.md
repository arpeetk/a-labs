# Review standards

How we review in this repo — the orchestrator gate every branch passes before
merge. This process exists because it works: three rounds of it on PR #16
found a live bypass of the security boundary that two earlier passes
("verifies clean") had missed. Read with [testing.md](testing.md) and
[code.md](code.md).

## The rules

1. **Verify against the code, never the docs, the diff summary, or the
   hand-off.** Every claim in a review — including "this finding is real" and
   "this concern is refuted" — carries `file:line` evidence read from the
   branch. A review that clears an area must have read the area. (PR #16
   round 1: the uid wiring was called "consistent" while `runnerUID` sat
   declared-and-unused.)

2. **Be adversarial; your job is to find the hole.** Take the PR's own
   headline claim ("the runner cannot bypass the proxy") and try to break it
   from every angle: hostile inputs, hostile images, missing binaries,
   reorderings, admission rejection, IPv6, DNS, lateral movement. Then record
   what you tried.

3. **Record refuted candidates, not just findings.** A "checked and cleared"
   section (hypothesis → why it doesn't hold, with evidence) is what turns
   "no findings" from "didn't look" into "looked and it holds." It also stops
   the next reviewer from re-chasing the same ghosts.

4. **Severity is about the claim, not the diff size.** A one-line missing
   `RunAsUser` was the highest-severity finding of the sprint because it
   broke the boundary the PR shipped. Rate by blast radius against what the
   code *promises*, with the failure scenario written out.

5. **Split must-fix from follow-up, explicitly.** Reviews end with a verdict
   (approve / approve-with-comments / request-changes) and two lists:
   merge-blocking vs. ticketed. Follow-ups go into `docs/tasks/STATUS.md`'s
   ledger in the same motion — a follow-up that isn't written down is lost.

6. **Cross-review findings get folded in, not re-litigated.** When two
   reviews cover the same ground (rounds 1–3 on PR #16), reconcile them into
   one fix list: confirm what's real, refute what's wrong (with evidence —
   the `Proxy-Authorization`-through-ReverseProxy claim), and land every
   confirmed fix in one round-2 commit before re-review.

7. **The author fixes; the reviewer re-verifies.** Findings bounce back to
   the worker branch; the reviewer checks the fixes against the original
   evidence, runs the gate on the result, and only then merges. A fix you
   didn't re-verify is a finding you reopened.

8. **Hand-offs are part of the review surface.** "NOT verified" and
   "questions" sections are read line by line and triaged into STATUS.md —
   they are how the PVC-vs-clean-`Failed` semantic gap and the `run list`
   N+1 tradeoff got captured instead of lost.
