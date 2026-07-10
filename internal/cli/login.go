package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/summiteight/wren/internal/config"
)

func newLoginCmd() *cobra.Command {
	var server, org, user string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to a Wren control plane",
		Long: "Save a control-plane context. SSO/device-flow authentication lands in M1;\n" +
			"for now this records the server address (and, for M0, your identity) so\n" +
			"other commands can target it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return errors.New("--control-plane is required (e.g. wren.corp.internal:443)")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := org
			if name == "" {
				name = "default"
			}
			cfg.Upsert(config.Context{Name: name, Server: server, Org: org, User: user})
			cfg.CurrentContext = name
			if err := cfg.Save(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Saved context %q → %s\n", name, server)
			fmt.Fprintln(out, "Note: SSO/device-flow auth is not implemented yet (M1); identity is taken from --user (M0).")
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "control-plane", "", "control plane address (host:port)")
	cmd.Flags().StringVar(&org, "org", "", "organization / context name (default: \"default\")")
	cmd.Flags().StringVar(&user, "user", "", "your identity (M0 stand-in for SSO; e.g. you@corp.com)")
	return cmd
}
