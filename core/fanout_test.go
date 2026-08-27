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

// fakeExporter is a scriptable Exporter for the export-layer tests.
type fakeExporter struct {
	mu       sync.Mutex
	batches  [][]LogRecord
	calls    atomic.Int64
	shutdown atomic.Int64

	// export runs on every call. Nil succeeds immediately.
	export func(ctx context.Context, batch []LogRecord) error
	// shutdownErr is returned by Shutdown.
	shutdownErr error
}

func (f *fakeExporter) Export(ctx context.Context, batch []LogRecord) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.batches = append(f.batches, batch)
	f.mu.Unlock()
	if f.export == nil {
		return nil
	}
	return f.export(ctx, batch)
}

func (f *fakeExporter) Shutdown(context.Context) error {
	f.shutdown.Add(1)
	return f.shutdownErr
}

func (f *fakeExporter) lastBatch() []LogRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	return f.batches[len(f.batches)-1]
}

// mutatingExporter declares that it writes to the batch it is given.
type mutatingExporter struct{ fakeExporter }

func (m *mutatingExporter) MutatesBatch() bool { return true }

var _ BatchMutator = (*mutatingExporter)(nil)

func testBatch(n int) []LogRecord {
	batch := make([]LogRecord, n)
	for i := range batch {
		batch[i] = LogRecord{
			Body:              fmt.Sprintf("record %d", i),
			Severity:          SeverityInfo,
			ObservedTimestamp: time.Unix(1700000000, 0),
			Attributes:        map[string]any{"i": i},
		}
	}
	return batch
}

func mustFanOut(t *testing.T, cfg FanOutConfig) *FanOut {
	t.Helper()
	f, err := NewFanOut(cfg)
	if err != nil {
		t.Fatalf("NewFanOut: %v", err)
	}
	return f
}

func TestNewFanOutRejectsConfigurationThatCannotBeRight(t *testing.T) {
	ok := &fakeExporter{}

	for _, tc := range []struct {
		name string
		cfg  FanOutConfig
		want string
	}{
		{
			// No destinations means every batch is accepted and discarded,
			// with a clean bill of health. A silent drop is a defect here.
			name: "no destinations",
			cfg:  FanOutConfig{},
			want: "at least one destination",
		},
		{
			name: "unnamed destination",
			cfg:  FanOutConfig{Destinations: []Destination{{Exporter: ok}}},
			want: "has no name",
		},
		{
			name: "nil exporter",
			cfg:  FanOutConfig{Destinations: []Destination{{Name: "primary"}}},
			want: "has no exporter",
		},
		{
			// Two destinations sharing a name report as one in every counter,
			// which hides the failure an operator is looking for.
			name: "duplicate names",
			cfg: FanOutConfig{Destinations: []Destination{
				{Name: "otlp", Exporter: ok},
				{Name: "otlp", Exporter: &fakeExporter{}},
			}},
			want: "declared twice",
		},
		{
			name: "negative timeout",
			cfg: FanOutConfig{
				Destinations: []Destination{{Name: "primary", Exporter: ok}},
				Timeout:      -time.Second,
			},
			want: "want a positive duration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFanOut(tc.cfg)
			if err == nil {
				t.Fatalf("NewFanOut(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The acceptance criterion from issue #15: a fast destination must not wait
// behind a slow one. Asserted by observing the fast exporter complete while
// the slow one is still blocked, rather than by timing, so the test cannot
// pass by accident on a loaded machine.
func TestFanOutDispatchesConcurrently(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slowFirst bool
	}{
		{"slow declared first", true},
		{"slow declared second", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastCalled := make(chan struct{})
			release := make(chan struct{})

			fast := &fakeExporter{export: func(context.Context, []LogRecord) error {
				close(fastCalled)
				return nil
			}}
			slow := &fakeExporter{export: func(ctx context.Context, _ []LogRecord) error {
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}

			dests := []Destination{{Name: "fast", Exporter: fast}, {Name: "slow", Exporter: slow}}
			if tc.slowFirst {
				dests[0], dests[1] = dests[1], dests[0]
			}
			f := mustFanOut(t, FanOutConfig{Destinations: dests, Timeout: 10 * time.Second})

			done := make(chan error, 1)
			go func() { done <- f.Export(context.Background(), testBatch(3)) }()

			select {
			case <-fastCalled:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the fast destination was still waiting on the slow one")
			}

			close(release)
			if err := <-done; err != nil {
				t.Errorf("Export: %v", err)
			}
		})
	}
}

func TestFanOutDeliversToEveryDestinationAndCounts(t *testing.T) {
	var m CountingMetrics
	a, b := &fakeExporter{}, &fakeExporter{}
	f := mustFanOut(t, FanOutConfig{
		Destinations: []Destination{{Name: "a", Exporter: a}, {Name: "b", Exporter: b}},
		Metrics:      &m,
	})

	if err := f.Export(context.Background(), testBatch(4)); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if got := a.calls.Load(); got != 1 {
		t.Errorf("a received %d batches, want 1", got)
	}
	if got := b.calls.Load(); got != 1 {
		t.Errorf("b received %d batches, want 1", got)
	}

	snap := m.Snapshot()
	for _, name := range []string{"a", "b"} {
		if got := snap.Exported[name]; got != 4 {
			t.Errorf("Exported[%q] = %d, want 4", name, got)
		}
		if got := snap.ExportLatency[name].Count; got != 1 {
			t.Errorf("ExportLatency[%q].Count = %d, want 1", name, got)
		}
	}
}

func TestFanOutEmptyBatchDispatchesNothing(t *testing.T) {
	a := &fakeExporter{}
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{{Name: "a", Exporter: a}}})

	if err := f.Export(context.Background(), nil); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := a.calls.Load(); got != 0 {
		t.Errorf("exporter was called %d times for an empty batch, want 0", got)
	}
}

// A batch that reached one destination is not lost, and must not be counted or
// treated as if it were (ADR-0009, ADR-0013).
func TestFanOutDistinguishesPartialFailureFromTotal(t *testing.T) {
	failing := &fakeExporter{export: func(context.Context, []LogRecord) error {
		return fmt.Errorf("%w: 400 malformed", ErrPermanent)
	}}
	healthy := &fakeExporter{}

	t.Run("partial", func(t *testing.T) {
		var m CountingMetrics
		f := mustFanOut(t, FanOutConfig{
			Destinations: []Destination{{Name: "healthy", Exporter: healthy}, {Name: "failing", Exporter: failing}},
			Metrics:      &m,
		})

		err := f.Export(context.Background(), testBatch(2))

		var fe *FanOutError
		if !errors.As(err, &fe) {
			t.Fatalf("Export returned %T (%v), want *FanOutError", err, err)
		}
		if fe.AllFailed() {
			t.Error("AllFailed() = true, but the healthy destination accepted the batch")
		}
		if !fe.Partial() {
			t.Error("Partial() = false, want true")
		}
		if _, ok := fe.Failures["failing"]; !ok {
			t.Errorf("Failures = %v, want it to name the failing destination", fe.Failures)
		}
		// Sentinels must survive the join, or the dispatcher above cannot tell
		// a permanent rejection from a transient one.
		if !IsPermanent(err) {
			t.Error("IsPermanent(err) = false, want the sentinel to reach through FanOutError")
		}
		// The healthy destination is still credited.
		if got := m.Snapshot().Exported["healthy"]; got != 2 {
			t.Errorf("Exported[healthy] = %d, want 2", got)
		}
		if got := m.Snapshot().Exported["failing"]; got != 0 {
			t.Errorf("Exported[failing] = %d, want 0 — it rejected the batch", got)
		}
	})

	t.Run("total", func(t *testing.T) {
		other := &fakeExporter{export: func(context.Context, []LogRecord) error {
			return errors.New("connection refused")
		}}
		f := mustFanOut(t, FanOutConfig{
			Destinations: []Destination{{Name: "failing", Exporter: failing}, {Name: "other", Exporter: other}},
		})

		err := f.Export(context.Background(), testBatch(2))

		var fe *FanOutError
		if !errors.As(err, &fe) {
			t.Fatalf("Export returned %T (%v), want *FanOutError", err, err)
		}
		if !fe.AllFailed() {
			t.Errorf("AllFailed() = false for %d failures of %d destinations", len(fe.Failures), fe.Dispatched)
		}
		// The message names both, so an operator does not have to guess which.
		for _, want := range []string{"failing", "other", "2 of 2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err, want)
			}
		}
	})
}

// Without a deadline, a destination that accepts the connection and then never
// answers holds a dispatch worker forever, and no breaker helps: a call that
// never returns never reports a failure to count (ADR-0016).
func TestFanOutBoundsEachDestinationWithATimeout(t *testing.T) {
	hung := &fakeExporter{export: func(ctx context.Context, _ []LogRecord) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	quick := &fakeExporter{}

	var m CountingMetrics
	f := mustFanOut(t, FanOutConfig{
		Destinations: []Destination{{Name: "hung", Exporter: hung}, {Name: "quick", Exporter: quick}},
		Timeout:      50 * time.Millisecond,
		Metrics:      &m,
	})

	err := f.Export(context.Background(), testBatch(1))

	var fe *FanOutError
	if !errors.As(err, &fe) {
		t.Fatalf("Export returned %T (%v), want *FanOutError", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if fe.AllFailed() {
		t.Error("AllFailed() = true, but the quick destination accepted the batch")
	}
	// A slow failure is still worth observing: it is the reason the worker was
	// unavailable.
	if got := m.Snapshot().ExportLatency["hung"].Count; got != 1 {
		t.Errorf("ExportLatency[hung].Count = %d, want the timed-out call observed", got)
	}
}

// The default is a shared, read-only batch. An exporter that declares
// otherwise gets its own copy, so it cannot corrupt what a sibling is reading
// concurrently (ADR-0016).
func TestFanOutClonesOnlyForDeclaredMutators(t *testing.T) {
	mutator := &mutatingExporter{fakeExporter: fakeExporter{
		export: func(_ context.Context, batch []LogRecord) error {
			batch[0].Body = "rewritten"
			batch[0].Attributes["injected"] = true
			return nil
		},
	}}
	reader := &fakeExporter{}

	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{
		{Name: "mutator", Exporter: mutator},
		{Name: "reader", Exporter: reader},
	}})

	batch := testBatch(2)
	if err := f.Export(context.Background(), batch); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if got := batch[0].Body; got != "record 0" {
		t.Errorf("caller's batch was modified: Body = %q, want %q", got, "record 0")
	}
	if _, injected := batch[0].Attributes["injected"]; injected {
		t.Error("caller's attributes were modified through a mutating exporter")
	}
	if got := reader.lastBatch()[0].Body; got != "record 0" {
		t.Errorf("the reading destination saw %q, want the unmodified %q", got, "record 0")
	}

	// A non-mutating destination shares the caller's slice — that is what
	// makes concurrent dispatch free, and it is worth pinning down.
	if &reader.lastBatch()[0] != &batch[0] {
		t.Error("a non-mutating destination was given a copy; the shared batch is the point")
	}
}

func TestFanOutShutdownReachesEveryDestination(t *testing.T) {
	a := &fakeExporter{shutdownErr: errors.New("flush failed")}
	b := &fakeExporter{}
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{{Name: "a", Exporter: a}, {Name: "b", Exporter: b}}})

	err := f.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "a: flush failed") {
		t.Errorf("Shutdown error = %v, want it to name the failing destination", err)
	}
	// b is shut down even though a failed: one destination's flush error must
	// not leak the others' resources.
	if got := b.shutdown.Load(); got != 1 {
		t.Errorf("b.Shutdown called %d times, want 1", got)
	}
}

func TestFanOutNamesAreReportedInOrder(t *testing.T) {
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{
		{Name: "primary", Exporter: &fakeExporter{}},
		{Name: "archive", Exporter: &fakeExporter{}},
	}})

	if got, want := f.Destinations(), 2; got != want {
		t.Errorf("Destinations() = %d, want %d", got, want)
	}
	if got := f.Names(); got[0] != "primary" || got[1] != "archive" {
		t.Errorf("Names() = %v, want [primary archive]", got)
	}
}
