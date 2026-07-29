package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/summiteight/wren/internal/client"
)

// clearScreen is the ANSI "home cursor + clear display" sequence used between
// --watch frames — the same trick `watch`/`kubectl get -w` rely on, no TUI
// library needed for something this simple.
const clearScreen = "\033[H\033[2J"

// fleetOptions are the flags newRunListCmd and newFleetCmd share — the same
// view over the same data, just different scope defaults (own runs vs
// everything visible).
type fleetOptions struct {
	Scope    string
	Project  string
	Phase    string
	Output   string // "" (table, default) or "json"
	Watch    bool
	Interval time.Duration
}

// newFleetCmd is `wren fleet`: every run visible to the caller, across every
// project, at a glance — the orchestration view. It's the same table
// (renderRunsTable) and the same fetch/filter/watch machinery (runFleetView)
// as `wren run list`, just defaulting to --scope all instead of mine, since
// the whole point of "fleet" is seeing everything, not just your own runs.
func newFleetCmd() *cobra.Command {
	var opts fleetOptions
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Show every run across every project, at a glance",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			return runFleetView(cmd, c, opts)
		},
	}
	addFleetFlags(cmd.Flags(), &opts, "all")
	return cmd
}

// addFleetFlags registers the flags newRunListCmd and newFleetCmd share.
// defaultScope differs between them ("mine" vs "all") — everything else is
// identical, so both commands render through the exact same view.
func addFleetFlags(f *pflag.FlagSet, opts *fleetOptions, defaultScope string) {
	f.StringVar(&opts.Scope, "scope", defaultScope, "which runs to show: mine|team|all")
	f.StringVar(&opts.Project, "project", "", "show only runs for this project")
	f.StringVar(&opts.Phase, "phase", "", "show only runs in this phase (e.g. Running, Failed)")
	f.StringVarP(&opts.Output, "output", "o", "", "output format: table (default) or json")
	f.BoolVarP(&opts.Watch, "watch", "w", false, "keep polling and redraw the table until Ctrl-C")
	f.DurationVar(&opts.Interval, "interval", 3*time.Second, "poll interval for --watch")
}

// runFleetView fetches runs per opts, applies the client-side --phase filter,
// and renders them — once, or on a loop if opts.Watch is set. --project and
// --scope are server-side (client.ListRuns/coreapi.ListRuns); --phase stays
// client-side, per the brief, since it's not worth a new store query
// dimension yet.
func runFleetView(cmd *cobra.Command, c *client.Client, opts fleetOptions) error {
	if opts.Output != "" && opts.Output != "json" {
		return fmt.Errorf("--output must be \"json\" (or omitted for the default table), got %q", opts.Output)
	}
	render := func(ctx context.Context) error {
		runs, err := c.ListRuns(ctx, opts.Scope, opts.Project)
		if err != nil {
			return err
		}
		runs = filterByPhase(runs, opts.Phase)
		sortRunsNewestFirst(runs)
		if opts.Output == "json" {
			return emit(cmd, runs)
		}
		return renderRunsTable(cmd.OutOrStdout(), runs)
	}

	if !opts.Watch {
		return render(context.Background())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for {
		if opts.Output != "json" {
			fmt.Fprint(cmd.OutOrStdout(), clearScreen)
			fmt.Fprintf(cmd.OutOrStdout(), "every %s — Ctrl-C to exit\n\n", opts.Interval)
		}
		if err := render(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
}

// filterByPhase keeps only runs whose Phase matches (case-sensitive, matching
// the phase strings the API already returns — Pending/Provisioning/Running/
// Succeeded/Failed/Canceled/...). Empty phase means no filtering.
func filterByPhase(runs []client.Run, phase string) []client.Run {
	if phase == "" {
		return runs
	}
	out := runs[:0:0] // fresh backing array — don't mutate the caller's slice in place
	for _, r := range runs {
		if r.Phase == phase {
			out = append(out, r)
		}
	}
	return out
}

func sortRunsNewestFirst(runs []client.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
}

// renderRunsTable prints the fleet view: ID, PROJECT, PHASE, HARNESS,
// RESTARTS, AGE, PR — the same tabwriter shape newProjectListCmd already
// uses, so the CLI's table output stays visually consistent across commands.
func renderRunsTable(w io.Writer, runs []client.Run) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROJECT\tPHASE\tHARNESS\tRESTARTS\tAGE\tPR")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, dash(r.Project), dash(r.Phase), dash(r.Harness),
			r.RestartCount, formatAge(r.CreatedAt), dash(r.PRURL))
	}
	if len(runs) == 0 {
		fmt.Fprintln(tw, "(no runs)\t\t\t\t\t\t")
	}
	return tw.Flush()
}

// formatAge renders a coarse, human relative age (kubectl-style: "5s", "3m",
// "2h", "4d") — precise enough for a fleet glance, never a raw duration or an
// absolute timestamp that needs a timezone to read.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
