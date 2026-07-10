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
		placeholder("run", "logs", "Stream logs from a run", "M0"),
		placeholder("run", "stop", "Stop a run", "M0"),
		placeholder("run", "resume", "Resume a Failed or Interrupted run", "M0"),
		placeholder("run", "rm", "Delete a run", "M0"),
		placeholder("run", "attach", "Attach to and steer a running agent", "M2"),
		placeholder("run", "steer", "Send a steering message to a run", "M2"),
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
	f.StringVar(&opts.Harness, "harness", "claude-code", "agent harness: claude-code|codex|byo")
	f.BoolVar(&opts.Interactive, "interactive", false, "attach and allow steering after submit")
	f.StringVar(&opts.BaseRef, "base", "", "base git ref (default: repo default branch)")
	f.StringVar(&opts.CPU, "cpu", "", "CPU request (e.g. 2)")
	f.StringVar(&opts.Memory, "mem", "", "memory request (e.g. 4Gi)")
	f.StringVar(&opts.Runtime, "runtime", "runc", "sandbox runtime: runc|gvisor|kata")
	return cmd
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
