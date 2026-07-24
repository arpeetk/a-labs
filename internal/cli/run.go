package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/summiteight/wren/internal/client"
)

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Create and manage agent runs",
	}
	cmd.AddCommand(
		newRunCreateCmd(),
		newRunListCmd(),
		newRunGetCmd(),
		newRunLogsCmd(),
		newRunStopCmd(),
		newRunRmCmd(),
	)
	return cmd
}

func newRunCreateCmd() *cobra.Command {
	var opts client.RunCreateOptions
	var taskFile string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Submit a task to a new agent run",
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskFile != "" {
				b, err := os.ReadFile(taskFile)
				if err != nil {
					return err
				}
				opts.Task = string(b)
			}
			if opts.Project == "" {
				return fmt.Errorf("--project is required")
			}
			if opts.Task == "" {
				return fmt.Errorf("--task or --file is required")
			}
			// gvisor/kata are wired end-to-end in the operator but no v1 cluster
			// provisions those RuntimeClasses — reject them here with a clear M4
			// pointer instead of letting the pod fail admission downstream.
			if opts.Runtime != "" && opts.Runtime != "runc" {
				return fmt.Errorf("--runtime %q is not available yet: only runc works today; gvisor/kata sandboxes land in M4 (technical-spec §5.6)", opts.Runtime)
			}
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			run, err := c.CreateRun(context.Background(), opts)
			if err != nil {
				return err
			}
			return emit(cmd, run)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Project, "project", "", "project (registered repo) to run against")
	f.StringVar(&opts.Task, "task", "", "task prompt")
	f.StringVar(&taskFile, "file", "", "read the task prompt from a file")
	// These default to empty so the project's configured defaults apply; a flag
	// override only takes effect when explicitly set.
	f.StringVar(&opts.Harness, "harness", "", "agent harness override: mock|claude-code|codex|opencode|byo (default: project's)")
	f.BoolVar(&opts.Interactive, "interactive", false, "attach and allow steering after submit")
	f.StringVar(&opts.BaseRef, "base", "", "base git ref (default: repo default branch)")
	f.StringVar(&opts.CPU, "cpu", "", "CPU request override (e.g. 2)")
	f.StringVar(&opts.Memory, "mem", "", "memory request override (e.g. 4Gi)")
	f.StringVar(&opts.Runtime, "runtime", "", "sandbox runtime override (only runc works today; gvisor/kata land in M4)")
	return cmd
}

func newRunStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Stop a run: cancel it (no auto-resume) and delete its pod",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			if err := c.StopRun(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %s stopping (will reach Canceled)\n", args[0])
			return nil
		},
	}
}

func newRunRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <run-id>",
		Short: "Delete a run and its cluster resources (pod, workspace)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			if err := c.DeleteRun(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %s deleted\n", args[0])
			return nil
		},
	}
}

func newRunListCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(context.Background(), scope)
			if err != nil {
				return err
			}
			return emit(cmd, runs)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "mine", "which runs to show: mine|team|all")
	return cmd
}

func newRunLogsCmd() *cobra.Command {
	var opts client.LogsOptions
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Stream logs from a run's pod",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			return c.StreamLogs(context.Background(), args[0], opts, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&opts.Follow, "follow", "f", false, "follow the log stream (tail -f)")
	f.StringVar(&opts.Container, "container", "", "container to tail: harness (default)|agent-gateway|egress-proxy|checkpointer|hydrate")
	return cmd
}

func newRunGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's state, PR, and usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			run, err := c.GetRun(context.Background(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, run)
		},
	}
}
