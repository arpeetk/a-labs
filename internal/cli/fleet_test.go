package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/client"
)

func TestFilterByPhase(t *testing.T) {
	runs := []client.Run{
		{ID: "r-1", Phase: "Running"},
		{ID: "r-2", Phase: "Succeeded"},
		{ID: "r-3", Phase: "Running"},
	}
	got := filterByPhase(runs, "Running")
	if len(got) != 2 || got[0].ID != "r-1" || got[1].ID != "r-3" {
		t.Fatalf("filterByPhase(Running) = %+v", got)
	}
	// Empty phase is "no filter" -- the exact same slice, not an empty result.
	if got := filterByPhase(runs, ""); len(got) != 3 {
		t.Fatalf("filterByPhase(\"\") = %+v, want all 3 unfiltered", got)
	}
	// A phase matching nothing returns an empty (not nil-panicking) slice.
	if got := filterByPhase(runs, "NoSuchPhase"); len(got) != 0 {
		t.Fatalf("filterByPhase(NoSuchPhase) = %+v, want empty", got)
	}
	// filterByPhase must not mutate the caller's backing array -- proven by
	// filtering a copy and checking the original slice is untouched.
	original := append([]client.Run{}, runs...)
	_ = filterByPhase(runs, "Running")
	for i := range runs {
		if runs[i] != original[i] {
			t.Fatalf("filterByPhase mutated its input at index %d: %+v vs original %+v", i, runs[i], original[i])
		}
	}
}

func TestSortRunsNewestFirst(t *testing.T) {
	now := time.Now()
	runs := []client.Run{
		{ID: "old", CreatedAt: now.Add(-time.Hour)},
		{ID: "newest", CreatedAt: now},
		{ID: "middle", CreatedAt: now.Add(-time.Minute)},
	}
	sortRunsNewestFirst(runs)
	want := []string{"newest", "middle", "old"}
	for i, id := range want {
		if runs[i].ID != id {
			t.Fatalf("sorted[%d] = %q, want %q (full: %+v)", i, runs[i].ID, id, runs)
		}
	}
}

func TestRenderRunsTable(t *testing.T) {
	now := time.Now()
	runs := []client.Run{
		{ID: "r-1", Project: "payments-api", Phase: "Succeeded", Harness: "claude-code",
			RestartCount: 1, CreatedAt: now.Add(-90 * time.Minute), PRURL: "https://github.com/acme/payments-api/pull/12"},
		{ID: "r-2", Project: "keyless-demo", Phase: "Running", Harness: "mock",
			CreatedAt: now.Add(-5 * time.Second)},
	}
	var buf bytes.Buffer
	if err := renderRunsTable(&buf, runs); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("expected header + 2 rows, got %d lines:\n%s", len(lines), out)
	}
	header := lines[0]
	for _, col := range []string{"ID", "PROJECT", "PHASE", "HARNESS", "RESTARTS", "AGE", "PR"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing column %q: %q", col, header)
		}
	}
	if !strings.Contains(lines[1], "r-1") || !strings.Contains(lines[1], "payments-api") ||
		!strings.Contains(lines[1], "pull/12") {
		t.Errorf("row 1 = %q", lines[1])
	}
	if !strings.Contains(lines[2], "r-2") || !strings.Contains(lines[2], "mock") {
		t.Errorf("row 2 = %q", lines[2])
	}
	// A run with no PR gets the dash placeholder, not an empty/blank cell that
	// would misalign the table.
	if !strings.Contains(lines[2], "-") {
		t.Errorf("row 2 should show a dash for the empty PR column: %q", lines[2])
	}
}

func TestRenderRunsTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRunsTable(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no runs") {
		t.Errorf("empty fleet should say so, got:\n%s", buf.String())
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "-"},
		{"seconds", now.Add(-30 * time.Second), "30s"},
		{"minutes", now.Add(-5 * time.Minute), "5m"},
		{"hours", now.Add(-3 * time.Hour), "3h"},
		{"days", now.Add(-50 * time.Hour), "2d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAge(tc.t); got != tc.want {
				t.Errorf("formatAge(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}
