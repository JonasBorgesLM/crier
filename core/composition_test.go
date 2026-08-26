package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// buildChain composes one destination the way ADR-0013 requires:
// Retry(CircuitBreaker(exporter)), ready to be handed to a fan-out.
func buildChain(t *testing.T, name string, e Exporter, m Metrics) Exporter {
	t.Helper()
	cb, err := NewCircuitBreaker(CircuitBreakerConfig{Name: name, Exporter: e, Metrics: m})
	if err != nil {
		t.Fatalf("NewCircuitBreaker(%s): %v", name, err)
	}
	r, err := NewRetry(RetryConfig{
		Name: name, Exporter: cb, MaxAttempts: 4,
		InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRetry(%s): %v", name, err)
	}
	return r
}

// The acceptance criterion from issue #16, and the regression test for audit
// finding A-1: with retry inside the fan-out, a healthy destination receives
// the batch exactly once no matter how hard a sibling is failing.
func TestRetryInsideFanOutDoesNotAmplifyToHealthyDestinations(t *testing.T) {
	var m CountingMetrics
	healthy := &fakeExporter{}
	broken := &fakeExporter{export: func(context.Context, []LogRecord) error {
		return errors.New("connection refused")
	}}

	f := mustFanOut(t, FanOutConfig{
		Destinations: []Destination{
			{Name: "healthy", Exporter: buildChain(t, "healthy", healthy, &m)},
			{Name: "broken", Exporter: buildChain(t, "broken", broken, &m)},
		},
		Metrics: &m,
	})

	err := f.Export(context.Background(), testBatch(5))

	var fe *FanOutError
	if !errors.As(err, &fe) {
		t.Fatalf("Export returned %T (%v), want *FanOutError", err, err)
	}
	if fe.AllFailed() {
		t.Fatal("AllFailed() = true, but the healthy destination accepted the batch")
	}

	if got := healthy.calls.Load(); got != 1 {
		t.Errorf("the healthy destination received the batch %d times, want exactly 1 "+
			"— retry outside the fan-out is audit finding A-1", got)
	}
	if got := broken.calls.Load(); got != 4 {
		t.Errorf("the broken destination was tried %d times, want 4 (its own retry budget)", got)
	}

	snap := m.Snapshot()
	if got := snap.Retries["healthy"]; got != 0 {
		t.Errorf("Retries[healthy] = %d, want 0 — it never failed", got)
	}
	if got := snap.Retries["broken"]; got != 3 {
		t.Errorf("Retries[broken] = %d, want 3", got)
	}
	if got := snap.Exported["healthy"]; got != 5 {
		t.Errorf("Exported[healthy] = %d, want 5", got)
	}
}

// The same scenario composed the wrong way round, kept as executable evidence
// of what ADR-0013 forbids. Retry(FanOut(a, b)) re-sends the whole batch when
// any one destination fails, so a healthy destination is hammered because an
// unrelated one is broken.
//
// It asserts the amplification rather than guarding against it, because the
// wrong composition is still expressible — both are Exporters — and the reason
// it is wrong is easier to keep true in a test than in a comment.
func TestRetryOutsideFanOutAmplifiesToHealthyDestinations(t *testing.T) {
	healthy := &fakeExporter{}
	broken := &fakeExporter{export: func(context.Context, []LogRecord) error {
		return errors.New("connection refused")
	}}

	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{
		{Name: "healthy", Exporter: healthy},
		{Name: "broken", Exporter: broken},
	}})
	wrong, err := NewRetry(RetryConfig{
		Name: "whole-fan-out", Exporter: f, MaxAttempts: 4,
		InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}

	_ = wrong.Export(context.Background(), testBatch(5))

	if got := healthy.calls.Load(); got != 4 {
		t.Errorf("the healthy destination received the batch %d times, want 4 — "+
			"if this changed, the amplification A-1 describes no longer reproduces "+
			"and the reasoning in ADR-0013 needs revisiting", got)
	}
}

// A destination that stays down stops being dialled: retry gives up, the
// breaker opens, and subsequent batches fail fast without spending the retry
// budget on a fail-fast (which would turn 1 call into 4 stalls per batch).
func TestBrokenDestinationStopsConsumingDispatchWork(t *testing.T) {
	var m CountingMetrics
	healthy := &fakeExporter{}
	broken := &fakeExporter{export: func(context.Context, []LogRecord) error {
		return errors.New("connection refused")
	}}

	cb, err := NewCircuitBreaker(CircuitBreakerConfig{
		Name: "broken", Exporter: broken, FailureThreshold: 4, Cooldown: time.Hour, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	retry, err := NewRetry(RetryConfig{
		Name: "broken", Exporter: cb, MaxAttempts: 4,
		InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}
	f := mustFanOut(t, FanOutConfig{
		Destinations: []Destination{
			{Name: "healthy", Exporter: healthy},
			{Name: "broken", Exporter: retry},
		},
		Metrics: &m,
	})

	// The first batch spends the whole retry budget and opens the circuit.
	_ = f.Export(context.Background(), testBatch(1))
	if got := broken.calls.Load(); got != 4 {
		t.Fatalf("the broken destination was dialled %d times on the first batch, want 4", got)
	}
	if got := cb.State(); got != CircuitOpen {
		t.Fatalf("State() = %v after the retry budget was spent, want open", got)
	}

	// Every batch after it is refused without touching the network, and
	// without the retry decorator looping on the refusal.
	for range 10 {
		err := f.Export(context.Background(), testBatch(1))
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("error = %v, want ErrCircuitOpen", err)
		}
	}
	if got := broken.calls.Load(); got != 4 {
		t.Errorf("the broken destination was dialled %d times in total, want 4 — "+
			"an open circuit must not dial", got)
	}
	if got := m.Snapshot().Retries["broken"]; got != 3 {
		t.Errorf("Retries[broken] = %d, want 3 — retrying a fail-fast turns it into a stall", got)
	}
	if got := healthy.calls.Load(); got != 11 {
		t.Errorf("the healthy destination received %d batches, want all 11", got)
	}
}

// ExampleFanOut shows the composition ADR-0013 requires: retry and circuit
// breaking per destination, inside the fan-out.
func ExampleFanOut() {
	// One chain per destination. Innermost is the exporter, then its circuit
	// breaker, then its retry — so each destination retries only its own
	// batch.
	build := func(name string, e Exporter) Destination {
		breaker, err := NewCircuitBreaker(CircuitBreakerConfig{Name: name, Exporter: e})
		if err != nil {
			panic(err)
		}
		retry, err := NewRetry(RetryConfig{Name: name, Exporter: breaker})
		if err != nil {
			panic(err)
		}
		return Destination{Name: name, Exporter: retry}
	}

	fanOut, err := NewFanOut(FanOutConfig{
		Destinations: []Destination{
			build("primary", noopExporter{}),
			build("archive", noopExporter{}),
		},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		panic(err)
	}

	batch := []LogRecord{{Body: "hello", Severity: SeverityInfo}}
	if err := fanOut.Export(context.Background(), batch); err != nil {
		var fe *FanOutError
		if errors.As(err, &fe) && fe.Partial() {
			// At least one destination has it. Re-sending to satisfy the
			// other would duplicate at the healthy one (ADR-0013).
			fmt.Println("partial:", err)
		}
	}
	fmt.Println("dispatched to", fanOut.Names())

	// Output:
	// dispatched to [primary archive]
}

// noopExporter accepts everything, so the example has no side effects.
type noopExporter struct{}

func (noopExporter) Export(context.Context, []LogRecord) error { return nil }
func (noopExporter) Shutdown(context.Context) error            { return nil }
