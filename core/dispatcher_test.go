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

func mustMemoryBuffer(t *testing.T, cfg MemoryBufferConfig) *MemoryBuffer {
	t.Helper()
	b, err := NewMemoryBuffer(cfg)
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	return b
}

func mustDispatcher(t *testing.T, cfg DispatcherConfig) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

// enqueue puts n records from source into the buffer.
func enqueue(t *testing.T, b BufferStore, source string, n int) {
	t.Helper()
	for i := range n {
		rec := LogRecord{
			Body:              fmt.Sprintf("%s %d", source, i),
			ObservedTimestamp: time.Unix(1700000000, 0),
			Resource:          Resource{ServiceName: source},
		}
		if err := b.Enqueue(context.Background(), rec); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
}

// waitFor polls until cond holds, failing the test if it never does. Used
// instead of a fixed sleep so the assertion is about the condition rather than
// about how fast the machine is.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewDispatcherRejectsConfigurationThatCannotBeRight(t *testing.T) {
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2})

	for _, tc := range []struct {
		name string
		cfg  DispatcherConfig
		want string
	}{
		{"no buffer", DispatcherConfig{Exporter: &fakeExporter{}}, "needs a buffer"},
		{"no exporter", DispatcherConfig{Buffer: buf}, "needs an exporter"},
		{"negative workers", DispatcherConfig{Buffer: buf, Exporter: &fakeExporter{}, Workers: -1}, "want at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDispatcher(tc.cfg)
			if err == nil {
				t.Fatalf("NewDispatcher(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDispatcherDrainsTheBufferToTheExporter(t *testing.T) {
	var m CountingMetrics
	var exported atomic.Int64
	e := &fakeExporter{export: func(_ context.Context, batch []LogRecord) error {
		exported.Add(int64(len(batch)))
		return nil
	}}
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 64, BatchSize: 4, BatchWindow: 10 * time.Millisecond})
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{{Name: "primary", Exporter: e}}, Metrics: &m})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: f, Workers: 2, Metrics: &m})

	d.Start(context.Background())
	enqueue(t, buf, "task-api", 10)

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := exported.Load(); got != 10 {
		t.Errorf("exported %d records, want all 10", got)
	}
	snap := m.Snapshot()
	if got := snap.Exported["primary"]; got != 10 {
		t.Errorf("Exported[primary] = %d, want 10", got)
	}
	if got := snap.TotalDropped(); got != 0 {
		t.Errorf("TotalDropped() = %d, want 0", got)
	}
	if got := e.shutdown.Load(); got != 1 {
		t.Errorf("exporter Shutdown called %d times, want 1", got)
	}
}

// Whether a batch was lost depends on whether it reached anywhere, which only
// the dispatcher can tell — and it is the difference between a counter an
// operator must act on and one they must not.
func TestDispatcherCountsOnlyBatchesThatReachedNowhere(t *testing.T) {
	for _, tc := range []struct {
		name        string
		healthyErr  error
		wantDropped int64
	}{
		{"partial failure loses nothing", nil, 0},
		{"total failure loses the batch", errors.New("connection refused"), 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m CountingMetrics
			healthy := &fakeExporter{export: func(context.Context, []LogRecord) error { return tc.healthyErr }}
			broken := &fakeExporter{export: func(context.Context, []LogRecord) error {
				return errors.New("connection refused")
			}}
			buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 64, BatchSize: 3, BatchWindow: 10 * time.Millisecond})
			f := mustFanOut(t, FanOutConfig{
				Destinations: []Destination{{Name: "healthy", Exporter: healthy}, {Name: "broken", Exporter: broken}},
				Metrics:      &m,
			})
			d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: f, Workers: 1, Metrics: &m})

			d.Start(context.Background())
			enqueue(t, buf, "task-api", 4)
			enqueue(t, buf, "gateway-auth", 2)
			if err := d.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}

			snap := m.Snapshot()
			if got := snap.DroppedBy(DropBackendUnavailable); got != tc.wantDropped {
				t.Errorf("DroppedBy(backend_unavailable) = %d, want %d", got, tc.wantDropped)
			}
			if tc.wantDropped == 0 {
				return
			}
			// Attributed per source: the operator's question is which of
			// their services lost telemetry.
			if got := snap.Dropped[DropKey{Source: "task-api", Reason: DropBackendUnavailable}]; got != 4 {
				t.Errorf("dropped[task-api] = %d, want 4", got)
			}
			if got := snap.Dropped[DropKey{Source: "gateway-auth", Reason: DropBackendUnavailable}]; got != 2 {
				t.Errorf("dropped[gateway-auth] = %d, want 2", got)
			}
		})
	}
}

// The degraded state is what takes the instance out of service (ADR-0015), so
// it must be true only when nothing can be exported at all.
func TestDispatcherDegradedStateFollowsEveryCircuit(t *testing.T) {
	newBroken := func(name string, cb **CircuitBreaker) Destination {
		e := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("down") }}
		breaker, err := NewCircuitBreaker(CircuitBreakerConfig{
			Name: name, Exporter: e, FailureThreshold: 1, Cooldown: time.Hour,
		})
		if err != nil {
			t.Fatalf("NewCircuitBreaker: %v", err)
		}
		*cb = breaker
		retry, err := NewRetry(RetryConfig{Name: name, Exporter: breaker, MaxAttempts: 1})
		if err != nil {
			t.Fatalf("NewRetry: %v", err)
		}
		return Destination{Name: name, Exporter: retry}
	}

	var a, b *CircuitBreaker
	var m CountingMetrics
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{newBroken("a", &a), newBroken("b", &b)}})
	// Circuits are discovered through the Retry wrapper, so readiness works
	// without the same list being kept in step in two places.
	d := mustDispatcher(t, DispatcherConfig{
		Buffer:   mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2}),
		Exporter: f, Metrics: &m,
	})

	if d.Degraded() {
		t.Fatal("Degraded() = true before anything failed")
	}

	// One destination down is a destination to look at, not an outage.
	_ = a.Export(context.Background(), testBatch(1))
	if !a.Open() {
		t.Fatal("a's circuit did not open")
	}
	if d.Degraded() {
		t.Error("Degraded() = true with one of two destinations still usable")
	}
	if got := d.OpenCircuits(); len(got) != 1 || got[0] != "a" {
		t.Errorf("OpenCircuits() = %v, want [a]", got)
	}

	_ = b.Export(context.Background(), testBatch(1))
	if !d.Degraded() {
		t.Error("Degraded() = false with every destination refusing calls")
	}

	snap := m.Snapshot()
	if !snap.Degraded {
		t.Error("the degraded metric was not reported")
	}
	// Reported on transitions only: not-degraded, then degraded.
	if got := snap.DegradedTransitions; got != 2 {
		t.Errorf("DegradedTransitions = %d, want 2 — the metric is an event, not a gauge re-set per call", got)
	}
	// Polling repeatedly must not re-report.
	for range 5 {
		_ = d.Degraded()
	}
	if got := m.Snapshot().DegradedTransitions; got != 2 {
		t.Errorf("DegradedTransitions = %d after repeated polling, want 2", got)
	}
}

// An unguarded destination has never said it is unusable. Assuming it is would
// take a healthy instance out of service.
func TestDispatcherIsNeverDegradedWithoutBreakersOnEveryDestination(t *testing.T) {
	broken := &fakeExporter{export: func(context.Context, []LogRecord) error { return errors.New("down") }}
	cb, err := NewCircuitBreaker(CircuitBreakerConfig{
		Name: "guarded", Exporter: broken, FailureThreshold: 1, Cooldown: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{
		{Name: "guarded", Exporter: cb},
		{Name: "bare", Exporter: &fakeExporter{}},
	}})
	d := mustDispatcher(t, DispatcherConfig{
		Buffer:   mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2}),
		Exporter: f,
	})

	_ = cb.Export(context.Background(), testBatch(1))
	if !cb.Open() {
		t.Fatal("the guarded circuit did not open")
	}
	if d.Degraded() {
		t.Error("Degraded() = true, but one destination has no breaker and may still be accepting records")
	}
}

// Workers bound how many batches are in flight. More would not buy capacity —
// a batch in flight has already left the buffer — but it would park more of
// the buffer on the same dead socket.
func TestDispatcherBoundsBatchesInFlight(t *testing.T) {
	release := make(chan struct{})
	var inFlight, peak atomic.Int64
	var mu sync.Mutex
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		n := inFlight.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		<-release
		inFlight.Add(-1)
		return nil
	}}
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 64, BatchSize: 2, BatchWindow: 5 * time.Millisecond})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: e, Workers: 2})

	d.Start(context.Background())
	enqueue(t, buf, "task-api", 20)

	waitFor(t, "both workers to be busy", func() bool { return inFlight.Load() == 2 })
	time.Sleep(20 * time.Millisecond) // give a third worker the chance to appear
	if got := peak.Load(); got > 2 {
		t.Errorf("%d batches were in flight at once, want at most the 2 configured workers", got)
	}

	close(release)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// Loss at shutdown is permitted; unaccounted loss is not (ADR-0015).
func TestDispatcherShutdownTimeoutCountsWhatItLost(t *testing.T) {
	var m CountingMetrics
	block := make(chan struct{})
	defer close(block)
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		<-block
		return nil
	}}
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 64, BatchSize: 2, BatchWindow: 5 * time.Millisecond})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: e, Workers: 1, Metrics: &m})

	d.Start(context.Background())
	enqueue(t, buf, "task-api", 10)
	waitFor(t, "the first batch to be in flight", func() bool { return e.calls.Load() > 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := d.Shutdown(ctx)

	if err == nil {
		t.Fatal("Shutdown succeeded, want the expired drain reported")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "still buffered") {
		t.Errorf("error = %q, want it to say how much was left", err)
	}
	if got := m.Snapshot().DroppedBy(DropShutdownTimeout); got == 0 {
		t.Error("records were lost at shutdown without being counted")
	}
}

// Nothing was draining the buffer, so nothing was lost by a drain that never
// ran. Counting it would be a guess about a dispatcher that never started.
func TestDispatcherShutdownWithoutStartCountsNothing(t *testing.T) {
	var m CountingMetrics
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: &fakeExporter{}, Metrics: &m})

	enqueue(t, buf, "task-api", 3)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := m.Snapshot().TotalDropped(); got != 0 {
		t.Errorf("TotalDropped() = %d, want 0", got)
	}
}

// A store failing in a way its contract does not describe must surface, not
// spin a core in a loop nobody is watching.
func TestDispatcherSurfacesAnUnexpectedStoreFailure(t *testing.T) {
	buf := &brokenBuffer{err: errors.New("disk on fire")}
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: &fakeExporter{}, Workers: 1})

	d.Start(context.Background())
	err := d.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("Shutdown error = %v, want the store failure surfaced", err)
	}
	if got := buf.dequeues.Load(); got > 2 {
		t.Errorf("DequeueBatch was called %d times, want the worker to stop rather than spin", got)
	}
}

// brokenBuffer is a BufferStore whose DequeueBatch fails in a way the contract
// does not describe.
type brokenBuffer struct {
	err      error
	dequeues atomic.Int64
}

func (b *brokenBuffer) Enqueue(context.Context, LogRecord) error { return nil }
func (b *brokenBuffer) DequeueBatch(context.Context) ([]LogRecord, error) {
	b.dequeues.Add(1)
	return nil, b.err
}
func (b *brokenBuffer) Depth() int   { return 0 }
func (b *brokenBuffer) Close() error { return nil }
