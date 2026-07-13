// Command wren is the Wren developer-experience CLI: submit tasks to cloud
// agents, watch and steer them, and manage projects, MCP servers, and usage.
package main

import (
	"os"

	"github.com/summiteight/wren/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
