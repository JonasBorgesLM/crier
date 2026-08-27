package core

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCountingMetricsSeparatesDropReasons(t *testing.T) {
	var m CountingMetrics

	m.RecordsDropped("task-api", DropBufferFull, 3)
	m.RecordsDropped("task-api", DropSourceQuota, 5)
	m.RecordsDropped("gateway-auth", DropBufferFull, 2)
	m.RecordsDropped("gateway-auth", DropBackendUnavailable, 7)

	snap := m.Snapshot()

	// The point of the reason dimension: an operator must be able to tell
	// "size the buffer up" apart from "one source is misbehaving" apart from
	// "the destination is down".
	if got, want := snap.DroppedBy(DropBufferFull), int64(5); got != want {
		t.Errorf("DroppedBy(buffer_full) = %d, want %d", got, want)
	}
	if got, want := snap.DroppedBy(DropSourceQuota), int64(5); got != want {
		t.Errorf("DroppedBy(source_quota) = %d, want %d", got, want)
	}
	if got, want := snap.DroppedBy(DropBackendUnavailable), int64(7); got != want {
		t.Errorf("DroppedBy(backend_unavailable) = %d, want %d", got, want)
	}
	if got, want := snap.TotalDropped(), int64(17); got != want {
		t.Errorf("TotalDropped() = %d, want %d", got, want)
	}
	if got, want := snap.Dropped[DropKey{"task-api", DropSourceQuota}], int64(5); got != want {
		t.Errorf("per-source bucket = %d, want %d", got, want)
	}
}

// A metrics implementation that grows a map key per distinct client-supplied
// string is the leak the pipeline's cardinality guard exists to prevent.
func TestCountingMetricsBoundsLabelCardinality(t *testing.T) {
	m := &CountingMetrics{LabelCap: 4}

	for i := range 100 {
		m.RecordsIngested(fmt.Sprintf("source-%d", i), 1)
	}

	snap := m.Snapshot()
	if len(snap.Ingested) > 5 { // 4 real labels + the overflow bucket
		t.Fatalf("Ingested holds %d labels, want at most 5", len(snap.Ingested))
	}
	if snap.Ingested[OverflowLabel] != 96 {
		t.Errorf("overflow bucket = %d, want 96", snap.Ingested[OverflowLabel])
	}

	var total int64
	for _, v := range snap.Ingested {
		total += v
	}
	if total != 100 {
		t.Errorf("total ingested = %d, want 100 — capping must not lose counts", total)
	}
}

// IdentityDiscrepancy takes a claimed identity straight from an untrusted
// caller. Keying on it would let one client fill the map by itself.
func TestIdentityDiscrepancyKeysOnAuthenticatedPrincipal(t *testing.T) {
	m := &CountingMetrics{LabelCap: 8}

	for i := range 50 {
		m.IdentityDiscrepancy(fmt.Sprintf("forged-%d", i), "task-api")
	}

	snap := m.Snapshot()
	if len(snap.IdentityDiscrepancies) != 1 {
		t.Fatalf("got %d keys, want 1: %v", len(snap.IdentityDiscrepancies), snap.IdentityDiscrepancies)
	}
	if got := snap.IdentityDiscrepancies["task-api"]; got != 50 {
		t.Errorf("count for the real principal = %d, want 50", got)
	}
}

func TestClockSkewIsRecordedAsMagnitude(t *testing.T) {
	var m CountingMetrics

	// A source ahead of us and a source behind us are equally broken.
	m.ClockSkew("task-api", 2*time.Second)
	m.ClockSkew("task-api", -4*time.Second)

	stat := m.Snapshot().ClockSkew["task-api"]
	if stat.Count != 2 {
		t.Fatalf("Count = %d, want 2", stat.Count)
	}
	if stat.Max != 4*time.Second {
		t.Errorf("Max = %v, want 4s", stat.Max)
	}
	if stat.Mean() != 3*time.Second {
		t.Errorf("Mean() = %v, want 3s", stat.Mean())
	}
}

func TestLatencyStatMeanOfNothingIsZero(t *testing.T) {
	if got := (LatencyStat{}).Mean(); got != 0 {
		t.Errorf("Mean() = %v, want 0", got)
	}
}

func TestSnapshotIsAnIndependentCopy(t *testing.T) {
	var m CountingMetrics
	m.RecordsIngested("task-api", 1)

	snap := m.Snapshot()
	m.RecordsIngested("task-api", 41)

	if got := snap.Ingested["task-api"]; got != 1 {
		t.Errorf("snapshot changed under the caller: got %d, want 1", got)
	}
}

func TestCountingMetricsIsConcurrencySafe(t *testing.T) {
	var m CountingMetrics
	const goroutines, perGoroutine = 8, 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				m.RecordsIngested("task-api", 1)
				m.RecordsDropped("task-api", DropBufferFull, 1)
				m.ExportLatency("otlp", time.Millisecond)
				m.BufferDepth(g)
				_ = m.Snapshot()
			}
		}()
	}
	wg.Wait()

	snap := m.Snapshot()
	if got, want := snap.Ingested["task-api"], int64(goroutines*perGoroutine); got != want {
		t.Errorf("Ingested = %d, want %d", got, want)
	}
	if got, want := snap.TotalDropped(), int64(goroutines*perGoroutine); got != want {
		t.Errorf("TotalDropped() = %d, want %d", got, want)
	}
}

// Embedding NopMetrics must let an implementation override one method
// without writing the other twelve — otherwise an embedding application has to
// choose between wiring up every metric and wiring up none.
func TestNopMetricsMakesPartialImplementationsPossible(t *testing.T) {
	spy := &depthOnlyMetrics{}
	var m Metrics = spy

	// Methods it did not implement must be callable and inert.
	m.RecordsIngested("task-api", 1)
	m.RecordsDropped("task-api", DropBufferFull, 1)
	m.ClockSkew("task-api", time.Second)
	m.DeprecatedWireVersion("v0")

	m.BufferDepth(42)

	if spy.depth != 42 {
		t.Errorf("BufferDepth = %d, want 42 — the override did not take effect", spy.depth)
	}
	if spy.calls != 1 {
		t.Errorf("overridden method called %d times, want 1", spy.calls)
	}
}

type depthOnlyMetrics struct {
	NopMetrics
	depth int
	calls int
}

func (m *depthOnlyMetrics) BufferDepth(d int) {
	m.depth = d
	m.calls++
}
