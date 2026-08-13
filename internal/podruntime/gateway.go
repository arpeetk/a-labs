package podruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/summiteight/wren/internal/harness"
)

var gatewayPollInterval = 100 * time.Millisecond

// RunGateway tails the append-only harness event file and forwards every JSONL
// item to the control plane through the trusted egress proxy. A restart replays
// from line one; attempt/sequence is a stable idempotency key, so the durable
// journal accepts each event exactly once without a fragile local cursor.
func RunGateway(ctx context.Context, out io.Writer) error {
	em := harness.NewEmitter(out)
	path := strings.TrimSpace(os.Getenv("WREN_EVENT_FILE"))
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("WREN_GATEWAY_URL")), "/")
	runID := strings.TrimSpace(os.Getenv("WREN_RUN_ID"))
	attempt := strings.TrimSpace(os.Getenv("WREN_ATTEMPT"))
	if path == "" || base == "" || runID == "" {
		return RunSidecar(ctx, out, RoleGateway)
	}
	if attempt == "" {
		attempt = "0"
	}

	f, err := openGatewayEventStream(ctx, path)
	if err != nil || f == nil {
		return err
	}
	defer f.Close()
	em.Message("agent-gateway: forwarding durable event stream")

	reader := bufio.NewReader(f)
	var fragment []byte
	var sequence int64
	for {
		part, readErr := reader.ReadBytes('\n')
		fragment = append(fragment, part...)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			sequence++
			if err := deliverGatewayLine(ctx, em, base, runID, attempt, sequence, fragment); err != nil {
				return err
			}
			fragment = nil
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read harness event stream: %w", readErr)
		}
		if errors.Is(readErr, io.EOF) && !waitGateway(ctx, gatewayPollInterval) {
			return nil
		}
	}
}

func openGatewayEventStream(ctx context.Context, path string) (*os.File, error) {
	for {
		f, err := os.Open(path)
		switch {
		case err == nil:
			return f, nil
		case os.IsNotExist(err):
			if !waitGateway(ctx, gatewayPollInterval) {
				return nil, nil
			}
		default:
			return nil, fmt.Errorf("open harness event stream: %w", err)
		}
	}
}

func deliverGatewayLine(ctx context.Context, em *harness.Emitter, base, runID, attempt string, sequence int64, data []byte) error {
	line := bytes.TrimSpace(data)
	var event harness.Event
	if err := json.Unmarshal(line, &event); err != nil {
		em.Errorf(fmt.Sprintf("agent-gateway: skip malformed event line %d: %v", sequence, err))
		return nil
	}
	err := forwardGatewayEvent(ctx, base, runID, attempt, sequence, event, line, func(err error) {
		em.Errorf(fmt.Sprintf("agent-gateway: event %d delivery retry: %v", sequence, err))
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func forwardGatewayEvent(ctx context.Context, base, runID, attempt string, sequence int64, event harness.Event, payload json.RawMessage, report func(error)) error {
	body, err := json.Marshal(map[string]any{
		"sourceId": fmt.Sprintf("attempt-%s/%d", attempt, sequence),
		"type":     string(event.Type),
		"payload":  payload,
		"at":       event.Time,
	})
	if err != nil {
		return fmt.Errorf("encode gateway event: %w", err)
	}
	delay := gatewayPollInterval
	retries := 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/internal/runs/"+url.PathEscape(runID)+"/events", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build gateway request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
				return nil
			}
			err = fmt.Errorf("control plane returned %s", resp.Status)
		}
		retries++
		if report != nil && (retries == 1 || retries%10 == 0) {
			report(err)
		}
		if !waitGateway(ctx, delay) {
			return context.Canceled
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}

func waitGateway(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
