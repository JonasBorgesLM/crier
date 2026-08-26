package otlp

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

// classify turns an HTTP status into the answer ADR-0013's retry decorator
// needs: retry this, or never retry this.
//
// The table is in ADR-0017 and the split is deliberate rather than mechanical.
// Reporting a 400 as retryable spends the whole budget on a batch no backend
// will ever accept; reporting a 503 as permanent throws telemetry away during
// a routine collector restart.
func classify(status int, retryAfter time.Duration, body string) error {
	err := statusError(status, body)

	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return withRetryAfter(err, retryAfter)
	case http.StatusNotImplemented:
		// The one 5xx that is not "try later": this endpoint does not do OTLP
		// logs, and it will not start doing them on the third attempt.
		return fmt.Errorf("%w: %w", core.ErrPermanent, err)
	}

	switch {
	case status >= 500:
		return withRetryAfter(err, retryAfter)
	case status >= 400:
		// Any other 4xx is an unknown client-side rejection. The conservative
		// reading is that it is our fault and repeating it will not help.
		return fmt.Errorf("%w: %w", core.ErrPermanent, err)
	default:
		// A 3xx the client did not follow, or a 1xx. Neither is a documented
		// OTLP answer; treat it as transient rather than discarding data on a
		// response nobody has thought about.
		return err
	}
}

// statusError renders the response for a human, including whatever the
// destination said about why.
func statusError(status int, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("otlp: %s", http.StatusText(status))
	}
	const maxBody = 512
	if len(body) > maxBody {
		body = body[:maxBody] + "…"
	}
	// The body is the destination's, not ours, and it lands in logs and
	// metrics labels. Newlines in it would forge log lines.
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\n", " "), "\r", " ")
	return fmt.Errorf("otlp: %d %s: %s", status, http.StatusText(status), body)
}

// throttleError carries the delay a destination asked for, so the retry
// decorator waits at least that long (ADR-0017).
type throttleError struct {
	err   error
	after time.Duration
}

// Error implements error.
func (e *throttleError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying failure, so the status is still reachable.
func (e *throttleError) Unwrap() error { return e.err }

// RetryAfter implements core.RetryHint.
func (e *throttleError) RetryAfter() time.Duration { return e.after }

// A compile-time check that the retry decorator will see the delay, not a
// discarded error.
//
//nolint:errcheck // core.RetryHint embeds error; this is an interface assertion
var _ core.RetryHint = (*throttleError)(nil)

// withRetryAfter attaches a delay to a retryable failure, if one was given.
func withRetryAfter(err error, after time.Duration) error {
	if after <= 0 {
		return err
	}
	return &throttleError{err: err, after: after}
}

// parseRetryAfter reads the Retry-After header in either of its two forms: a
// number of seconds, or an HTTP date.
//
// A malformed value returns zero rather than an error. The header is advisory;
// failing an export because a destination formatted its advice badly would
// lose data over a detail that changes nothing.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}

	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
