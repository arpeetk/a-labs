package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/summiteight/wren/internal/client"
)

// newProjectCreateCmd registers a repo (or a repo-less keyless project) against
// the control plane — the real POST /v1/projects (WS-13; was a placeholder).
func newProjectCreateCmd() *cobra.Command {
	var p client.Project
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Register a GitHub repo as a project",
		Long: "Register a project the control plane can run against.\n\n" +
			"A project with no --repo is keyless: runs skip the clone and the PR\n" +
			"(pair it with --harness mock for a zero-credential smoke test).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Name = args[0]
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			out, err := c.CreateProject(context.Background(), p)
			if err != nil {
				return err
			}
			return emit(cmd, out)
		},
	}
	f := cmd.Flags()
	f.StringVar(&p.Repo, "repo", "", "GitHub repo as owner/repo (empty = keyless project)")
	f.StringVar(&p.DefaultHarness, "harness", "", "default harness: claude-code|mock (default: control plane's)")
	f.StringVar(&p.HarnessImage, "harness-image", "", "image the harness runs in (e.g. the install's pushed runtime image)")
	f.StringVar(&p.DefaultModel, "model", "", "default model (e.g. claude-opus-4-8, mock)")
	f.StringVar(&p.CPU, "cpu", "", "CPU request (e.g. 2)")
	f.StringVar(&p.Memory, "memory", "", "memory request (e.g. 4Gi)")
	f.StringVar(&p.Disk, "disk", "", "workspace disk size (e.g. 10Gi)")
	f.StringVar(&p.Namespace, "namespace", "", "run namespace override (point this at install's --run-namespace for credentialed runs)")
	return cmd
}

// newProjectListCmd prints registered projects as a table (GET /v1/projects).
func newProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			projects, err := c.ListProjects(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tREPO\tHARNESS\tIMAGE\tCPU\tMEM\tDISK")
			for _, p := range projects {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					p.Name, dash(p.Repo), dash(p.DefaultHarness), dash(p.HarnessImage),
					dash(p.CPU), dash(p.Memory), dash(p.Disk))
			}
			return tw.Flush()
		},
	}
}

// dash renders an empty cell as "-" so table columns stay scannable.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
