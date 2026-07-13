package finalize

import (
	"fmt"
	"strings"

	"github.com/summiteight/wren/internal/runspec"
)

// Rubric renders the PR body from the project's rubric template. M0 ships a
// default structured template (spec §5.7); per-project rubrics land with the
// Projects service config work.
func Rubric(spec runspec.RunSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", strings.TrimSpace(spec.Prompt))
	b.WriteString("## What Wren did\n\n")
	b.WriteString("An autonomous change produced by the Wren Software Factory. Review the diff and CI before merging.\n\n")
	b.WriteString("## Test plan\n\n")
	b.WriteString("- [ ] CI passes\n- [ ] Manually verified the change\n\n")
	b.WriteString("## Risk / rollback\n\n")
	b.WriteString("Low-risk, self-contained change. Roll back by reverting this PR.\n\n")
	b.WriteString("---\n")
	fmt.Fprintf(&b, "🐦 Opened by **Wren** · run `%s` · harness `%s`\n", spec.RunID, spec.Harness)
	return b.String()
}
