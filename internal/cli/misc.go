package cli

import "github.com/spf13/cobra"

// newProjectCmd manages registered repositories and their defaults.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Register and configure projects (repos)"}
	cmd.AddCommand(
		newProjectCreateCmd(),
		newProjectListCmd(),
		placeholder("project", "get", "Show a project's config", "M0"),
		placeholder("project", "config", "Edit project defaults, rubric, and egress", "M1"),
	)
	return cmd
}

// newMCPCmd manages MCP server integrations per project.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp", Short: "Configure MCP servers for a project"}
	cmd.AddCommand(
		placeholder("mcp", "add", "Attach an MCP server to a project", "M1"),
		placeholder("mcp", "list", "List a project's MCP servers", "M1"),
		placeholder("mcp", "test", "Probe an MCP server's connectivity", "M1"),
	)
	return cmd
}

// newFleetCmd shows the cross-run dashboard.
func newFleetCmd() *cobra.Command {
	return placeholder("", "fleet", "Show all runs, states, and live cost", "M1")
}

// newUsageCmd reports token, cost, and compute usage.
func newUsageCmd() *cobra.Command {
	return placeholder("", "usage", "Report token, cost, CPU, and memory usage", "M1")
}
