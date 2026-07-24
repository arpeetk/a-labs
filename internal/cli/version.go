package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Build metadata, injected via -ldflags at build time (see the Makefile).
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wren CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "wren %s (commit %s, built %s)\n", Version, Commit, Date)
			return err
		},
	}
}
