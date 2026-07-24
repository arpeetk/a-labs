package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// execRunner is the real Runner: os/exec against the local machine. docker,
// kind, and gh have no typed client, so they stay exec'd (per the WS-13 brief;
// the cluster itself is driven through the typed client in kube.go).
type execRunner struct {
	out io.Writer // command progress (docker build output) streams here
}

func (r *execRunner) LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (r *execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	// docker/kind write progress to stderr; fold both into the install output.
	cmd.Stdout = r.out
	cmd.Stderr = r.out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// Output captures stdout without streaming it anywhere — used for
// `gh auth token`, whose value must never touch the logs.
func (r *execRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return out.String(), nil
}
