package finalize

import (
	"fmt"
	"strings"

	"github.com/summiteight/wren/internal/runspec"
)

// Rubric renders the default structured PR body. Per-project rubric editing is
// not yet exposed by the project API.
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
