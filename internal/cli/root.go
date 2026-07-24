// Package cli implements the wren command tree.
package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/summiteight/wren/internal/client"
	"github.com/summiteight/wren/internal/config"
)

// NewRootCommand builds the root `wren` command with all subcommands attached.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "wren",
		Short: "Spin up massively parallel, durable, sandboxed coding agents in the cloud",
		Long: "Wren is the backbone of an internal Software Factory: submit a task, an\n" +
			"agent runs it in a hardened cloud sandbox, survives crashes, and opens a PR.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().String("context", "", "config context to use (defaults to the current context)")

	root.AddCommand(
		newVersionCmd(),
		newLoginCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newRunCmd(),
		newProjectCmd(),
		newMCPCmd(),
		newFleetCmd(),
		newUsageCmd(),
	)
	return root
}

// clientFromFlags resolves the active context and returns a control-plane client.
func clientFromFlags(cmd *cobra.Command) (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	name, _ := cmd.Flags().GetString("context")
	cctx, err := cfg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return client.New(cctx), nil
}

// emit writes v to stdout as indented JSON.
func emit(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// placeholder builds a subcommand that reports which milestone will implement it.
func placeholder(group, use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short + " (not implemented yet)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented(group, use, milestone)
		},
	}
}
