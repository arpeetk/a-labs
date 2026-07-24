package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/summiteight/wren/internal/runspec"
)

// This file is the shared headless-agent runner behind the claude-code, codex,
// and opencode adapters (spec §5.4): each adapter supplies argv, environment,
// and a line parser for its CLI's JSONL stdout; the subprocess lifecycle and
// the contract's error/exit semantics live here exactly once.

// cliEvent is the normalized subset of one agent-CLI stdout line that the
// adapters map onto the Wren event stream. A single line can carry several
// events (e.g. a claude assistant message with text + tool_use), so parsers
// return a slice.
type cliEvent struct {
	text      string // assistant/final message → EventMessage
	tool      string // tool invocation label → EventToolCall
	inTokens  int64
	outTokens int64
	hasUsage  bool
	isErr     bool // the agent reported a terminal error result
}

// agentCLI describes one headless agent invocation.
type agentCLI struct {
	adapter string // adapter name, for events/errors (e.g. "claude-code")
	bin     string // binary to LookPath (e.g. "claude")
	args    []string
	env     []string
	// parseLine maps one stdout line to its events; an empty result skips the
	// line. Parsers must be tolerant: CLIs print warnings and partial lines
	// around their JSON stream.
	parseLine func(line []byte) []cliEvent
}

// runAgentCLI launches the CLI with cwd = workspace, streams its stdout events
// onto the bus, and maps the process outcome to the harness contract: a
// non-zero exit or an agent-reported error is a deterministic failure (the
// operator must NOT retry it — that would just re-spend the agent's tokens;
// spec §5.4 exit-code semantics). A clean exit reports the branch + final
// usage; the authoritative pr_ready is emitted later by the finalize step.
func runAgentCLI(ctx context.Context, spec runspec.RunSpec, em *Emitter, cli agentCLI) (Result, error) {
	bin, err := exec.LookPath(cli.bin)
	if err != nil {
		em.Errorf(cli.bin + " CLI not found on PATH (use a " + cli.adapter + " harness image)")
		return Result{}, fmt.Errorf("%s harness: `%s` CLI not installed in the image", cli.adapter, cli.bin)
	}

	cmd := exec.CommandContext(ctx, bin, cli.args...)
	cmd.Dir = spec.WorkspacePath
	cmd.Env = cli.env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	in, out, isErr := streamCLI(stdout, em, cli.parseLine)
	waitErr := cmd.Wait()

	branch := branchFor(spec)
	if waitErr != nil {
		em.Errorf(cli.bin + " exited with error: " + waitErr.Error())
		return Result{Branch: branch, InputTokens: in, OutputTokens: out}, waitErr
	}
	if isErr {
		return Result{Branch: branch, InputTokens: in, OutputTokens: out},
			errors.New(cli.bin + " reported an error result")
	}
	return Result{Branch: branch, InputTokens: in, OutputTokens: out}, nil
}

// streamCLI parses newline-delimited agent events, re-emitting them as Wren
// events and returning the final token usage (last wins — v0.1 records
// terminal values only, matching the operator's scrape in §5.4) and whether
// the run errored. token_usage is emitted as an event (not just returned)
// because the operator reads run results from the harness's log stream — a
// count that never becomes an event never reaches status.
func streamCLI(r io.Reader, em *Emitter, parse func([]byte) []cliEvent) (inTokens, outTokens int64, isErr bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // agent messages can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		for _, ev := range parse(line) {
			if ev.text != "" {
				em.Message(ev.text)
			}
			if ev.tool != "" {
				em.ToolCall(ev.tool)
			}
			if ev.hasUsage {
				inTokens, outTokens = ev.inTokens, ev.outTokens
				em.Usage(inTokens, outTokens)
			}
			if ev.isErr {
				isErr = true
			}
		}
	}
	return inTokens, outTokens, isErr
}

// branchFor computes the run's PR branch (shared by every adapter).
func branchFor(spec runspec.RunSpec) string {
	if spec.BranchPrefix == "" {
		return "wren/" + spec.RunID
	}
	return spec.BranchPrefix + "/" + spec.RunID
}

// ensureEnv returns env with key=placeholder appended when the variable is
// unset, so the agent CLI starts in API-key mode. The placeholder is never a
// real credential: the egress-proxy is the sole credential authority and
// overwrites the auth header on the way out (spec §5.6).
func ensureEnv(env []string, key, placeholder string) []string {
	if os.Getenv(key) == "" {
		env = append(env, key+"="+placeholder)
	}
	return env
}
