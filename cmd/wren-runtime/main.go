// Command wren-runtime is the multi-call in-pod binary. Its first argument
// selects a role (harness | hydrate | egress-proxy | checkpointer |
// agent-gateway); with no argument it runs the harness role, so a harness image
// can use ["wren-runtime"] as its entrypoint (spec §5.4).
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/summiteight/wren/internal/podruntime"
	"github.com/summiteight/wren/internal/runspec"
)

func main() {
	role := podruntime.RoleHarness
	if len(os.Args) > 1 {
		role = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// RunSpec path comes from the env the operator sets (WREN_RUNSPEC), falling
	// back to the default mount path.
	specPath := os.Getenv("WREN_RUNSPEC")

	if err := podruntime.Dispatch(ctx, os.Stdout, role, specPath); err != nil {
		log.Printf("wren-runtime %s: %v", role, err)
		if errors.Is(err, podruntime.ErrRetryable) {
			os.Exit(runspec.ExitRetryable) // transient — operator may retry
		}
		os.Exit(runspec.ExitError) // deterministic — operator must not retry
	}
}
