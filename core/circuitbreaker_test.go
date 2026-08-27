package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is the breaker's Now, so a cooldown can be asserted without a
// test sleeping through it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustBreaker(t *testing.T, cfg CircuitBreakerConfig) *CircuitBreaker {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "primary"
	}
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	return cb
}

func TestNewCircuitBreakerRejectsConfigurationThatCannotBeRight(t *testing.T) {
	ok := &fakeExporter{}

	for _, tc := range []struct {
		name string
		cfg  CircuitBreakerConfig
		want string
	}{
		{"no exporter", CircuitBreakerConfig{Name: "primary"}, "needs an exporter"},
		{"no name", CircuitBreakerConfig{Exporter: ok}, "needs a Name"},
		{"negative threshold", CircuitBreakerConfig{Name: "p", Exporter: ok, FailureThreshold: -1}, "want at least 1"},
		{"negative successes", CircuitBreakerConfig{Name: "p", Exporter: ok, HalfOpenSuccesses: -1}, "want at least 1"},
		{"negative cooldown", CircuitBreakerConfig{Name: "p", Exporter: ok, Cooldown: -time.Second}, "want a positive duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCircuitBreaker(tc.cfg)
			if err == nil {
				t.Fatalf("NewCircuitBreaker(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCircuitOpensAfterConsecutiveFailuresAndFailsFast(t *testing.T) {
	var m CountingMetrics
	failing := errors.New("connection refused")
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return failing }}
	cb := mustBreaker(t, CircuitBreakerConfig{Exporter: e, FailureThreshold: 3, Metrics: &m})

	for i := range 3 {
		if err := cb.Export(context.Background(), testBatch(1)); !errors.Is(err, failing) {
			t.Fatalf("attempt %d: error = %v, want the destination's failure", i+1, err)
		}
	}
	if got := cb.State(); got != CircuitOpen {
		t.Fatalf("State() = %v after 3 failures with a threshold of 3, want open", got)
	}

	// Now it must refuse without touching the network at all.
	err := cb.Export(context.Background(), testBatch(1))
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("error = %v, want ErrCircuitOpen", err)
	}
	if got := e.calls.Load(); got != 3 {
		t.Errorf("exporter called %d times, want 3 — the open circuit must not dial", got)
	}
	if !strings.Contains(err.Error(), "primary") {
		t.Errorf("error = %q, want it to name the destination", err)
	}
	if open, ok := m.Snapshot().OpenCircuits["primary"]; !ok || !open {
		t.Errorf("OpenCircuits[primary] = %v (present %v), want true", open, ok)
	}
}

// The threshold counts *consecutive* failures. A destination that fails
// occasionally and recovers is not the same as one that is down, and treating
// them alike takes a working destination out of service.
func TestCircuitSuccessResetsTheFailureRun(t *testing.T) {
	var fail bool
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		if fail {
			return errors.New("flaky")
		}
		return nil
	}}
	cb := mustBreaker(t, CircuitBreakerConfig{Exporter: e, FailureThreshold: 3})

	for range 3 {
		fail = true
		_ = cb.Export(context.Background(), testBatch(1))
		_ = cb.Export(context.Background(), testBatch(1))
		fail = false
		if err := cb.Export(context.Background(), testBatch(1)); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	if got := cb.State(); got != CircuitClosed {
		t.Errorf("State() = %v after runs of 2 failures under a threshold of 3, want closed", got)
	}
}

func TestCircuitProbesAfterCooldownAndClosesOnRecovery(t *testing.T) {
	var m CountingMetrics
	clock := newFakeClock()
	var down bool
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		if down {
			return errors.New("down")
		}
		return nil
	}}
	cb := mustBreaker(t, CircuitBreakerConfig{
		Exporter: e, FailureThreshold: 2, Cooldown: 30 * time.Second,
		HalfOpenSuccesses: 2, Metrics: &m, Now: clock.Now,
	})

	down = true
	for range 2 {
		_ = cb.Export(context.Background(), testBatch(1))
	}
	if got := cb.State(); got != CircuitOpen {
		t.Fatalf("State() = %v, want open", got)
	}

	// Still inside the cooldown: no probe.
	clock.Advance(29 * time.Second)
	if err := cb.Export(context.Background(), testBatch(1)); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("error = %v inside the cooldown, want ErrCircuitOpen", err)
	}

	clock.Advance(2 * time.Second)
	down = false
	if err := cb.Export(context.Background(), testBatch(1)); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// One success is not enough here: HalfOpenSuccesses is 2.
	if got := cb.State(); got != CircuitHalfOpen {
		t.Errorf("State() = %v after 1 of 2 required probes, want half-open", got)
	}
	if err := cb.Export(context.Background(), testBatch(1)); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if got := cb.State(); got != CircuitClosed {
		t.Errorf("State() = %v after both probes succeeded, want closed", got)
	}
	if open := m.Snapshot().OpenCircuits["primary"]; open {
		t.Error("OpenCircuits[primary] = true, want the recovery reported")
	}
}

func TestCircuitReopensWhenTheProbeFails(t *testing.T) {
	clock := newFakeClock()
	e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("still down") }}
	cb := mustBreaker(t, CircuitBreakerConfig{
		Exporter: e, FailureThreshold: 1, Cooldown: 10 * time.Second, Now: clock.Now,
	})

	_ = cb.Export(context.Background(), testBatch(1))
	clock.Advance(11 * time.Second)

	// The probe goes through and fails.
	if err := cb.Export(context.Background(), testBatch(1)); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("the probe was refused; after the cooldown one call must be admitted")
	}
	if got := cb.State(); got != CircuitOpen {
		t.Fatalf("State() = %v after a failed probe, want open", got)
	}
	// A failed probe buys a fresh cooldown rather than probing on every batch.
	clock.Advance(9 * time.Second)
	if err := cb.Export(context.Background(), testBatch(1)); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("error = %v, want a full cooldown after the failed probe", err)
	}
	if got := e.calls.Load(); got != 2 {
		t.Errorf("exporter called %d times, want 2 (the opener and the probe)", got)
	}
}

// A recovering destination gets one probe, not the whole backlog. Sending the
// burst is how a backend that just came back gets knocked over again.
func TestCircuitAdmitsOneProbeAtATime(t *testing.T) {
	clock := newFakeClock()
	release := make(chan struct{})
	var calls atomic.Int64
	e := &fakeExporter{export: func(ctx context.Context, _ []LogRecord) error {
		if calls.Add(1) == 1 {
			return errors.New("down") // opens the circuit
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cb := mustBreaker(t, CircuitBreakerConfig{
		Exporter: e, FailureThreshold: 1, Cooldown: time.Second, Now: clock.Now,
	})

	if err := cb.Export(context.Background(), testBatch(1)); err == nil {
		t.Fatal("the opening call succeeded, want it to fail")
	}
	if got := cb.State(); got != CircuitOpen {
		t.Fatalf("State() = %v, want open", got)
	}
	clock.Advance(2 * time.Second)

	// One call is admitted as the probe and blocks; the rest must fail fast.
	probe := make(chan error, 1)
	go func() { probe <- cb.Export(context.Background(), testBatch(1)) }()

	deadline := time.Now().Add(5 * time.Second)
	for cb.State() != CircuitHalfOpen && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := cb.State(); got != CircuitHalfOpen {
		close(release)
		t.Fatalf("State() = %v, want half-open with a probe in flight", got)
	}

	for i := range 5 {
		if err := cb.Export(context.Background(), testBatch(1)); !errors.Is(err, ErrCircuitOpen) {
			t.Errorf("concurrent call %d: error = %v, want ErrCircuitOpen while a probe is in flight", i, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the destination saw %d calls, want 2 — the opener and one probe", got)
	}

	close(release)
	if err := <-probe; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := cb.State(); got != CircuitClosed {
		t.Errorf("State() = %v after the probe succeeded, want closed", got)
	}
}

// What counts as a health signal, and what does not (ADR-0016).
func TestCircuitFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		exportFn func(ctx context.Context, batch []LogRecord) error
		wantOpen bool
	}{
		{
			// A stale credential makes a destination as unusable as one
			// refusing connections, and readiness has to see it (ADR-0015).
			name:     "permanent failure counts",
			ctx:      func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			exportFn: func(context.Context, []LogRecord) error { return fmt.Errorf("%w: 401", ErrPermanent) },
			wantOpen: true,
		},
		{
			// The per-destination deadline exists to turn a hang into a
			// failure a breaker can count. If it did not count, a destination
			// that never answers would look healthy forever.
			name: "deadline counts",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Millisecond)
			},
			exportFn: func(ctx context.Context, _ []LogRecord) error { <-ctx.Done(); return ctx.Err() },
			wantOpen: true,
		},
		{
			// Our own shutdown. Counting it would open every circuit on the
			// way out of the door and report a healthy fleet as degraded.
			name: "cancellation does not count",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			exportFn: func(ctx context.Context, _ []LogRecord) error { return ctx.Err() },
			wantOpen: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeExporter{export: tc.exportFn}
			cb := mustBreaker(t, CircuitBreakerConfig{Exporter: e, FailureThreshold: 2})

			for range 2 {
				ctx, cancel := tc.ctx()
				_ = cb.Export(ctx, testBatch(1))
				cancel()
			}

			if got := cb.Open(); got != tc.wantOpen {
				t.Errorf("Open() = %v after 2 such failures, want %v (state %v)", got, tc.wantOpen, cb.State())
			}
		})
	}
}

// A cancelled probe must release the probe slot, or the breaker is stuck
// half-open refusing everything while the destination is fine.
func TestCircuitCancelledProbeReleasesTheSlot(t *testing.T) {
	clock := newFakeClock()
	var cancelled bool
	e := &fakeExporter{export: func(ctx context.Context, _ []LogRecord) error {
		if cancelled {
			return ctx.Err()
		}
		return errors.New("down")
	}}
	cb := mustBreaker(t, CircuitBreakerConfig{
		Exporter: e, FailureThreshold: 1, Cooldown: time.Second, Now: clock.Now,
	})

	_ = cb.Export(context.Background(), testBatch(1))
	clock.Advance(2 * time.Second)

	cancelled = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = cb.Export(ctx, testBatch(1))

	// The slot is free, so the next call probes rather than being refused.
	cancelled = false
	e.export = func(context.Context, []LogRecord) error { return nil }
	if err := cb.Export(context.Background(), testBatch(1)); err != nil {
		t.Fatalf("the probe slot was not released: %v", err)
	}
	if got := cb.State(); got != CircuitClosed {
		t.Errorf("State() = %v, want closed", got)
	}
}

func TestCircuitStateString(t *testing.T) {
	for _, tc := range []struct {
		state CircuitState
		want  string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half-open"},
		{CircuitState(9), "CircuitState(9)"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("CircuitState(%d).String() = %q, want %q", int(tc.state), got, tc.want)
		}
	}
}

func TestCircuitBreakerShutdownReachesTheWrappedExporter(t *testing.T) {
	e := &fakeExporter{}
	cb := mustBreaker(t, CircuitBreakerConfig{Exporter: e})
	if err := cb.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := e.shutdown.Load(); got != 1 {
		t.Errorf("wrapped Shutdown called %d times, want 1", got)
	}
}
