package finalize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gh "github.com/google/go-github/v66/github"
)

// httpResp builds a minimal *http.Response for error fixtures. Request is set
// because some Error() implementations dereference it when the test prints.
func httpResp(code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Request:    &http.Request{Method: "POST", URL: &url.URL{Scheme: "https", Host: "api.github.com"}},
	}
}

// TestClassifyErrorMatrix is the WS-11 retry contract: transport-class
// finalize failures (network, HTTP 429/5xx, EOF/timeout) must come back
// wrapped in ErrRetryable; permanent classes (401/403 auth, 422 validation,
// non-fast-forward) must pass through untouched so the run fails
// deterministically.
func TestClassifyErrorMatrix(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		// transient: network / transport
		{"dial connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"dns timeout", &net.DNSError{IsTimeout: true}, true},
		{"url error wrapping net error", &url.Error{Op: "Post", URL: "https://api.github.com", Err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}}, true},
		{"io.EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped EOF", fmt.Errorf("push wren/r-1: %w", io.ErrUnexpectedEOF), true},
		// transient: HTTP status classes
		{"github 502", &gh.ErrorResponse{Response: httpResp(http.StatusBadGateway)}, true},
		{"github 429", &gh.ErrorResponse{Response: httpResp(http.StatusTooManyRequests)}, true},
		{"github 500", &gh.ErrorResponse{Response: httpResp(http.StatusInternalServerError)}, true},
		{"github rate limit", &gh.RateLimitError{Response: httpResp(http.StatusForbidden), Rate: gh.Rate{Reset: gh.Timestamp{Time: time.Now()}}}, true},
		{"github abuse limit", &gh.AbuseRateLimitError{Response: httpResp(http.StatusForbidden)}, true},
		{"github 202 accepted", &gh.AcceptedError{}, true},
		{"go-git http 503", plumbing.NewUnexpectedError(&githttp.Err{Response: httpResp(http.StatusServiceUnavailable)}), true},
		// permanent: auth / validation / human conflict
		{"github 401", &gh.ErrorResponse{Response: httpResp(http.StatusUnauthorized)}, false},
		{"github 403", &gh.ErrorResponse{Response: httpResp(http.StatusForbidden)}, false},
		{"github 422 validation", &gh.ErrorResponse{Response: httpResp(http.StatusUnprocessableEntity)}, false},
		{"github 404", &gh.ErrorResponse{Response: httpResp(http.StatusNotFound)}, false},
		{"git non-fast-forward (human pushed)", git.ErrNonFastForwardUpdate, false},
		{"git auth required", transport.ErrAuthenticationRequired, false},
		{"git authorization failed", transport.ErrAuthorizationFailed, false},
		{"git repo not found", transport.ErrRepositoryNotFound, false},
		{"go-git wrapped auth", plumbing.NewPermanentError(fmt.Errorf("%w: bad credentials", transport.ErrAuthenticationRequired)), false},
		{"plain error", errors.New("commit: malformed tree"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if tc.retryable {
				if !errors.Is(got, ErrRetryable) {
					t.Errorf("classify(%v) = %v, want ErrRetryable", tc.err, got)
				}
				if !errors.Is(got, tc.err) {
					t.Errorf("classify(%v) lost the cause: %v", tc.err, got)
				}
			} else if errors.Is(got, ErrRetryable) {
				t.Errorf("classify(%v) = %v, must stay deterministic", tc.err, got)
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	if err := classify(nil); err != nil {
		t.Errorf("classify(nil) = %v", err)
	}
}
