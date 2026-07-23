package finalize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gh "github.com/google/go-github/v66/github"
)

// ErrRetryable marks a transient finalize failure — the push or the PR call
// failed for transport-class reasons (network, HTTP 429/5xx, EOF/timeout) that
// a fresh pod may get past. podruntime maps it onto its own ErrRetryable, which
// cmd/wren-runtime turns into runspec.ExitRetryable so the operator spends a
// restart instead of killing a run that has budget to spare (WS-11).
var ErrRetryable = errors.New("retryable finalize error")

// classify wraps err in ErrRetryable when it is transport-class. Permanent
// classes — 401/403 auth, 422 validation, a non-fast-forward from a *human*
// push to the run branch — pass through unwrapped so the failure stays
// deterministic: retrying them just re-spends the agent's tokens.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if transient(err) {
		return fmt.Errorf("%w: %w", ErrRetryable, err)
	}
	return err
}

// transient reports whether err is a transport-class failure worth a retry.
func transient(err error) bool {
	// go-git's plumbing wrappers have no Unwrap — peel them manually so the
	// inner cause classifies.
	var pe *plumbing.PermanentError
	if errors.As(err, &pe) {
		return transient(pe.Err)
	}
	var ue *plumbing.UnexpectedError
	if errors.As(err, &ue) {
		return transient(ue.Err)
	}
	// Git-transport permanents: auth/authorization/not-found, and a
	// non-fast-forward (someone else pushed to the run branch).
	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrRepositoryNotFound) ||
		errors.Is(err, git.ErrNonFastForwardUpdate) {
		return false
	}
	// HTTP status classes, in go-git's and go-github's flavors.
	var gitErr *githttp.Err
	if errors.As(err, &gitErr) {
		return retryableStatus(gitErr.StatusCode())
	}
	var ghResp *gh.ErrorResponse
	if errors.As(err, &ghResp) && ghResp.Response != nil {
		return retryableStatus(ghResp.Response.StatusCode)
	}
	// GitHub rate limits and 202 Accepted are transient by definition.
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return true
	}
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) {
		return true
	}
	// Network/transport: refused, reset, timeout, a dropped connection
	// mid-stream. (*url.Error and most dial errors satisfy net.Error.)
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded)
}

// retryableStatus classifies an HTTP status: 429 and 5xx are transient; other
// 4xx (401/403 auth, 422 validation, ...) are permanent.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}
