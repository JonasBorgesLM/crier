package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newTestRetry builds a Retry whose backoff is recorded rather than slept, so
// the schedule can be asserted without the test waiting for it.
func newTestRetry(t *testing.T, cfg RetryConfig, slept *[]time.Duration) *Retry {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "primary"
	}
	if cfg.Rand == nil {
		// Draw the top of the jitter window, so the recorded interval is the
		// ceiling and the schedule is readable.
		cfg.Rand = func() float64 { return 1 }
	}
	r, err := NewRetry(cfg)
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}
	r.sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}
	return r
}

func TestNewRetryRejectsConfigurationThatCannotBeRight(t *testing.T) {
	ok := &fakeExporter{}

	for _, tc := range []struct {
		name string
		cfg  RetryConfig
		want string
	}{
		{"no exporter", RetryConfig{Name: "primary"}, "needs an exporter"},
		{"no name", RetryConfig{Exporter: ok}, "needs a Name"},
		{"negative attempts", RetryConfig{Name: "p", Exporter: ok, MaxAttempts: -1}, "want at least 1"},
		{"negative backoff", RetryConfig{Name: "p", Exporter: ok, InitialBackoff: -time.Second}, "backoff is negative"},
		{
			// Silently reordering them would give an operator a schedule they
			// did not ask for and cannot see.
			name: "initial above max",
			cfg:  RetryConfig{Name: "p", Exporter: ok, InitialBackoff: 10 * time.Second, MaxBackoff: time.Second},
			want: "longer than MaxBackoff",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRetry(tc.cfg)
			if err == nil {
				t.Fatalf("NewRetry(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRetryStopsForFailuresRetryingCannotFix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantCalls   int64
		wantRetries int64
	}{
		{
			// A rejected credential is not accepted on the third attempt, and
			// the budget spent on it delays every batch behind this one.
			name:      "permanent",
			err:       fmt.Errorf("%w: 401 unauthorized", ErrPermanent),
			wantCalls: 1,
		},
		{
			// The breaker below already decided. Looping on its fail-fast
			// turns it into a stall that spends the export deadline.
			name:      "circuit open",
			err:       fmt.Errorf("%w: primary", ErrCircuitOpen),
			wantCalls: 1,
		},
		{
			name:        "transient",
			err:         errors.New("connection reset"),
			wantCalls:   4,
			wantRetries: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m CountingMetrics
			var slept []time.Duration
			e := &fakeExporter{export: func(context.Context, []LogRecord) error { return tc.err }}
			r := newTestRetry(t, RetryConfig{Exporter: e, MaxAttempts: 4, Metrics: &m}, &slept)

			err := r.Export(context.Background(), testBatch(2))
			if err == nil {
				t.Fatal("Export succeeded, want the failure to surface")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.err)
			}
			if got := e.calls.Load(); got != tc.wantCalls {
				t.Errorf("exporter called %d times, want %d", got, tc.wantCalls)
			}
			if got := m.Snapshot().Retries["primary"]; got != tc.wantRetries {
				t.Errorf("Retries = %d, want %d", got, tc.wantRetries)
			}
			if got := int64(len(slept)); got != tc.wantRetries {
				t.Errorf("backed off %d times, want %d", got, tc.wantRetries)
			}
		})
	}
}

func TestRetrySucceedsOnceTheDestinationRecovers(t *testing.T) {
	var m CountingMetrics
	var slept []time.Duration
	var attempts int
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		attempts++
		if attempts < 3 {
			return errors.New("503 service unavailable")
		}
		return nil
	}}
	r := newTestRetry(t, RetryConfig{Exporter: e, MaxAttempts: 5, Metrics: &m}, &slept)

	if err := r.Export(context.Background(), testBatch(1)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if got := m.Snapshot().Retries["primary"]; got != 2 {
		t.Errorf("Retries = %d, want 2", got)
	}
}

func TestRetryExhaustionNamesTheAttempts(t *testing.T) {
	var slept []time.Duration
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("connection refused") }}
	r := newTestRetry(t, RetryConfig{Exporter: e, MaxAttempts: 3}, &slept)

	err := r.Export(context.Background(), testBatch(1))
	if err == nil {
		t.Fatal("Export succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("error = %q, want it to say how many attempts were spent", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want the underlying failure preserved", err)
	}
}

// MaxAttempts of 1 is how retrying is turned off — visibly, with the decorator
// still in place — rather than by removing it from the chain and quietly
// changing the composition ADR-0013 depends on.
func TestRetryMaxAttemptsOneNeverRetries(t *testing.T) {
	var slept []time.Duration
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("nope") }}
	r := newTestRetry(t, RetryConfig{Exporter: e, MaxAttempts: 1}, &slept)

	if err := r.Export(context.Background(), testBatch(1)); err == nil {
		t.Fatal("Export succeeded, want an error")
	}
	if got := e.calls.Load(); got != 1 {
		t.Errorf("exporter called %d times, want 1", got)
	}
	if len(slept) != 0 {
		t.Errorf("backed off %v, want no backoff at all", slept)
	}
}

func TestRetryBackoffIsExponentialAndCapped(t *testing.T) {
	var slept []time.Duration
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("down") }}
	r := newTestRetry(t, RetryConfig{
		Exporter:       e,
		MaxAttempts:    6,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     400 * time.Millisecond,
	}, &slept)

	_ = r.Export(context.Background(), testBatch(1))

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond, // capped
		400 * time.Millisecond,
	}
	if len(slept) != len(want) {
		t.Fatalf("backoffs = %v, want %d of them", slept, len(want))
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("backoff %d = %v, want %v", i+1, slept[i], want[i])
		}
	}
}

// Full jitter draws from the whole interval [0, ceiling). Adding a small
// perturbation instead leaves every sender retrying on nearly the same
// schedule, which is how a collector that just came back gets knocked over.
func TestRetryJitterDrawsFromTheWholeInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		draw float64
		want time.Duration
	}{
		{"bottom of the window", 0, 0},
		{"middle", 0.5, 50 * time.Millisecond},
		{"top", 1, 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var slept []time.Duration
			e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("down") }}
			r := newTestRetry(t, RetryConfig{
				Exporter:       e,
				MaxAttempts:    2,
				InitialBackoff: 100 * time.Millisecond,
				Rand:           func() float64 { return tc.draw },
			}, &slept)

			_ = r.Export(context.Background(), testBatch(1))

			if len(slept) != 1 || slept[0] != tc.want {
				t.Errorf("backoffs = %v, want [%v]", slept, tc.want)
			}
		})
	}
}

func TestRetryAbandonsBackoffWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exportErr := errors.New("connection reset")
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return exportErr }}
	r, err := NewRetry(RetryConfig{
		Name: "primary", Exporter: e, MaxAttempts: 5,
		InitialBackoff: time.Hour, MaxBackoff: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}
	r.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	err = r.Export(ctx, testBatch(1))
	if err == nil {
		t.Fatal("Export succeeded, want an error")
	}
	// Both halves are reachable: one says why we were retrying, the other why
	// we stopped.
	if !errors.Is(err, exportErr) {
		t.Errorf("error = %v, want it to wrap the export failure", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if got := e.calls.Load(); got != 1 {
		t.Errorf("exporter called %d times after cancellation, want 1", got)
	}
}

// A leaf that declares it writes to its batch must stay visible through the
// decorators, or fan-out hands it the shared batch and it corrupts what a
// sibling is reading (ADR-0016).
func TestDecoratorsForwardMutatesBatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		leaf Exporter
		want bool
	}{
		{"plain exporter", &fakeExporter{}, false},
		{"declared mutator", &mutatingExporter{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cb, err := NewCircuitBreaker(CircuitBreakerConfig{Name: "primary", Exporter: tc.leaf})
			if err != nil {
				t.Fatalf("NewCircuitBreaker: %v", err)
			}
			r, err := NewRetry(RetryConfig{Name: "primary", Exporter: cb})
			if err != nil {
				t.Fatalf("NewRetry: %v", err)
			}

			if got := cb.MutatesBatch(); got != tc.want {
				t.Errorf("CircuitBreaker.MutatesBatch() = %v, want %v", got, tc.want)
			}
			if got := r.MutatesBatch(); got != tc.want {
				t.Errorf("Retry.MutatesBatch() = %v, want %v", got, tc.want)
			}
			if got := mutatesBatch(r); got != tc.want {
				t.Errorf("mutatesBatch(chain) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryShutdownReachesTheWrappedExporter(t *testing.T) {
	e := &fakeExporter{}
	r, err := NewRetry(RetryConfig{Name: "primary", Exporter: e})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := e.shutdown.Load(); got != 1 {
		t.Errorf("wrapped Shutdown called %d times, want 1", got)
	}
}

// throttled is an error that names its own delay, as an HTTP 429 does.
type throttled struct{ after time.Duration }

func (t throttled) Error() string             { return fmt.Sprintf("429, retry after %v", t.after) }
func (t throttled) RetryAfter() time.Duration { return t.after }

var _ RetryHint = throttled{}

// A destination that has just said how long it needs is not helped by arriving
// early on our own schedule (ADR-0017).
func TestRetryHonoursTheDelayADestinationAsksFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "hint longer than the backoff wins",
			err:  throttled{after: 2 * time.Second},
			want: 2 * time.Second,
		},
		{
			// A hint shorter than the backoff is not an instruction to hurry.
			name: "backoff longer than the hint wins",
			err:  throttled{after: time.Millisecond},
			want: 100 * time.Millisecond,
		},
		{
			name: "no hint leaves the backoff alone",
			err:  errors.New("connection reset"),
			want: 100 * time.Millisecond,
		},
		{
			// Wrapped, which is how it will actually arrive.
			name: "hint reached through a wrapper",
			err:  fmt.Errorf("exporting to primary: %w", throttled{after: 3 * time.Second}),
			want: 3 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var slept []time.Duration
			e := &fakeExporter{export: func(context.Context, []LogRecord) error { return tc.err }}
			r := newTestRetry(t, RetryConfig{
				Exporter: e, MaxAttempts: 2, InitialBackoff: 100 * time.Millisecond,
			}, &slept)

			_ = r.Export(context.Background(), testBatch(1))

			if len(slept) != 1 || slept[0] != tc.want {
				t.Errorf("backoffs = %v, want [%v]", slept, tc.want)
			}
		})
	}
}
