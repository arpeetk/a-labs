package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// newProjectCmd manages registered repositories and their defaults.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Register and configure projects (repos)"}
	cmd.AddCommand(
		newProjectCreateCmd(),
		newProjectListCmd(),
		newProjectGetCmd(),
	)
	return cmd
}

// newProjectGetCmd shows a single project's configuration (GET
// /v1/projects/{name}).
func newProjectGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a project's configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			p, err := c.GetProject(context.Background(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, p)
		},
	}
}
