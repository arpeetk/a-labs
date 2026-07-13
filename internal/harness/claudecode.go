package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/summiteight/wren/internal/runspec"
)

// ClaudeCode runs a task with Claude Code: it drives the bundled `claude` CLI in
// headless mode inside the cloned workspace, letting the agent read/edit files
// and run tools autonomously, and streams its stream-json events onto the Wren
// event bus. Model calls go through the egress-proxy (ANTHROPIC_BASE_URL); the
// real API key is injected there, never held by the runner (spec §5.4/§5.6).
type ClaudeCode struct{}

// Name implements Harness.
func (ClaudeCode) Name() string { return "claude-code" }

// Run implements Harness.
func (ClaudeCode) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		em.Errorf("claude CLI not found on PATH (use a claude-code harness image)")
		return Result{}, errors.New("claude-code harness: `claude` CLI not installed in the image")
	}

	args := []string{
		"--print", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions", // safe: the pod IS the sandbox
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = spec.WorkspacePath
	cmd.Env = claudeEnv()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	in, out, isErr := streamClaude(stdout, em)
	waitErr := cmd.Wait()

	branch := spec.BranchPrefix + "/" + spec.RunID
	if spec.BranchPrefix == "" {
		branch = "wren/" + spec.RunID
	}
	if waitErr != nil {
		em.Errorf("claude exited with error: " + waitErr.Error())
		return Result{Branch: branch, InputTokens: in, OutputTokens: out}, waitErr
	}
	if isErr {
		return Result{Branch: branch, InputTokens: in, OutputTokens: out}, errors.New("claude reported an error result")
	}
	return Result{Branch: branch, InputTokens: in, OutputTokens: out}, nil
}

// claudeEnv provides the subprocess environment. It ensures ANTHROPIC_API_KEY is
// set to a placeholder so the CLI starts even though the real key is injected by
// the egress-proxy (which overwrites the x-api-key header on the way out).
func claudeEnv() []string {
	env := os.Environ()
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		env = append(env, "ANTHROPIC_API_KEY=injected-by-egress-proxy")
	}
	// Keep the agent's own config/telemetry from leaking across runs.
	env = append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	return env
}

// claudeStreamEvent is the subset of the claude stream-json schema we consume.
type claudeStreamEvent struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// streamClaude parses newline-delimited claude events, re-emitting them as Wren
// events and returning the final token usage and whether the run errored.
func streamClaude(r io.Reader, em *Emitter) (inTokens, outTokens int64, isErr bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // agent messages can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev claudeStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // tolerate non-JSON / partial lines
		}
		switch ev.Type {
		case "assistant":
			if ev.Message != nil {
				for _, c := range ev.Message.Content {
					switch c.Type {
					case "text":
						if c.Text != "" {
							em.Message(c.Text)
						}
					case "tool_use":
						em.ToolCall(c.Name)
					}
				}
			}
			if ev.Usage != nil {
				inTokens, outTokens = ev.Usage.InputTokens, ev.Usage.OutputTokens
			}
		case "result":
			if ev.Usage != nil {
				inTokens, outTokens = ev.Usage.InputTokens, ev.Usage.OutputTokens
			}
			if ev.Result != "" {
				em.Message(ev.Result)
			}
			isErr = ev.IsError
		}
	}
	return inTokens, outTokens, isErr
}
