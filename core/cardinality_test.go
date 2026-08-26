package core

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestGuardCapsAKeyAfterTooManyDistinctValues(t *testing.T) {
	var m CountingMetrics
	g := &CardinalityGuard{MaxDistinctValues: 5, Metrics: &m}

	for i := range 4 {
		if capped := g.Observe("request.id", fmt.Sprintf("id-%d", i)); capped {
			t.Fatalf("capped after %d distinct values, want 5", i+1)
		}
	}
	if !g.Observe("request.id", "id-4") {
		t.Fatal("not capped at the threshold")
	}
	if !g.Observe("request.id", "id-5") {
		t.Error("un-capped on the next value")
	}
	if got := m.Snapshot().CardinalityCapped["request.id"]; got < 2 {
		t.Errorf("CardinalityCapped = %d, want at least 2", got)
	}
}

func TestRepeatedValuesDoNotCountTwice(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 3}

	for range 100 {
		if g.Observe("http.method", "GET") {
			t.Fatal("a key with one repeated value was capped")
		}
	}
}

func TestGuardReplacesValuesInPlace(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 2}

	var last LogRecord
	for i := range 5 {
		last = LogRecord{Attributes: map[string]any{
			"trace":  fmt.Sprintf("t-%d", i),
			"method": "GET",
		}}
		g.Apply(&last)
	}

	if last.Attributes["trace"] != DefaultCardinalityMark {
		t.Errorf("trace = %v, want the cardinality marker", last.Attributes["trace"])
	}
	if last.Attributes["method"] != "GET" {
		t.Errorf("method = %v, want GET — a low-cardinality key was caught up in it", last.Attributes["method"])
	}
}

// The guard's own state is the thing most likely to become the leak it exists
// to prevent.
func TestGuardStateStaysBoundedUnderUniqueValues(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 10, MaxTrackedKeys: 8}

	for i := range 100_000 {
		g.Observe(fmt.Sprintf("key-%d", i%50), fmt.Sprintf("value-%d", i))
	}

	if got := g.TrackedKeys(); got > 8 {
		t.Errorf("TrackedKeys() = %d, want at most 8", got)
	}
}

// Once a key is capped its value set is released — it is the largest
// allocation the guard makes and it buys nothing after the verdict.
func TestCappedKeyReleasesItsValueSet(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 100}
	for i := range 100 {
		g.Observe("noisy", fmt.Sprintf("v-%d", i))
	}

	g.mu.Lock()
	state := g.current["noisy"]
	g.mu.Unlock()

	if !state.capped {
		t.Fatal("key is not capped")
	}
	if state.values != nil {
		t.Errorf("value set still holds %d entries after capping", len(state.values))
	}
}

// Evicting a tracked key to make room lets an attacker launder a capped key
// back out by flooding new ones.
func TestNewKeysPassThroughRatherThanEvicting(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 2, MaxTrackedKeys: 1}

	g.Observe("noisy", "a")
	if !g.Observe("noisy", "b") {
		t.Fatal("noisy was not capped")
	}

	for i := range 1000 {
		g.Observe(fmt.Sprintf("flood-%d", i), "x")
	}

	if !g.Observe("noisy", "c") {
		t.Error("the capped key was laundered back out by flooding new keys")
	}
}

// A key that was noisy an hour ago and is quiet now should recover.
func TestWindowRotationLetsAQuietKeyRecover(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	g := &CardinalityGuard{MaxDistinctValues: 3, Window: time.Minute, Now: clock}

	for i := range 3 {
		g.Observe("bursty", fmt.Sprintf("v-%d", i))
	}
	if !g.Observe("bursty", "v-again") {
		t.Fatal("key was not capped")
	}

	// One window on, the verdict is still carried forward.
	now = now.Add(90 * time.Second)
	if !g.Observe("bursty", "v-carry") {
		t.Error("the capped verdict was forgotten after a single rotation")
	}

	// Two quiet windows on, both generations are stale and the key recovers.
	now = now.Add(10 * time.Minute)
	if g.Observe("bursty", "v-new") {
		t.Error("key never recovers, so a one-off burst is permanent")
	}
}

func TestNonStringValuesAreLeftAlone(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 1}

	rec := LogRecord{Attributes: map[string]any{"count": 1, "ok": true}}
	for range 10 {
		g.Apply(&rec)
	}
	if rec.Attributes["count"] != 1 || rec.Attributes["ok"] != true {
		t.Errorf("non-string values were altered: %v", rec.Attributes)
	}
}

func TestNegativeMaxDistinctValuesDisablesTheGuard(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: -1}
	for i := range 1000 {
		if g.Observe("anything", fmt.Sprintf("v-%d", i)) {
			t.Fatal("guard capped a key while disabled")
		}
	}
	if got := g.TrackedKeys(); got != 0 {
		t.Errorf("TrackedKeys() = %d, want 0 — a disabled guard should hold no state", got)
	}
}

func TestGuardIsConcurrencySafe(t *testing.T) {
	g := &CardinalityGuard{MaxDistinctValues: 50, MaxTrackedKeys: 16}

	var wg sync.WaitGroup
	wg.Add(8)
	for w := range 8 {
		go func() {
			defer wg.Done()
			for i := range 500 {
				g.Observe(fmt.Sprintf("key-%d", i%20), fmt.Sprintf("w%d-v%d", w, i))
			}
		}()
	}
	wg.Wait()

	if got := g.TrackedKeys(); got > 16 {
		t.Errorf("TrackedKeys() = %d, want at most 16", got)
	}
}

// The acceptance criterion for #9: memory stays bounded under an endless
// stream of unique attribute values.
func TestMemoryStaysBoundedUnderUniqueValueFlood(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation measurement is noisy under -short")
	}
	g := &CardinalityGuard{MaxDistinctValues: 100, MaxTrackedKeys: 32}

	flood := func(n int) {
		for i := range n {
			rec := LogRecord{Attributes: map[string]any{
				fmt.Sprintf("key-%d", i%64): fmt.Sprintf("unique-%d", i),
			}}
			g.Apply(&rec)
		}
	}

	flood(50_000) // warm up past every cap
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	flood(200_000) // four times as much again
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 1<<20 {
		t.Errorf("heap grew by %d bytes across the second flood, want the guard's state to be bounded", growth)
	}
	if got := g.TrackedKeys(); got > 32 {
		t.Errorf("TrackedKeys() = %d, want at most 32", got)
	}
}

func FuzzCardinalityGuardNeverPanicsAndStaysBounded(f *testing.F) {
	f.Add("request.id", "abc-123")
	f.Add("", "")
	f.Add("k", "\x00\xff")

	g := &CardinalityGuard{MaxDistinctValues: 8, MaxTrackedKeys: 4}
	f.Fuzz(func(t *testing.T, key, value string) {
		g.Observe(key, value)
		if got := g.TrackedKeys(); got > 4 {
			t.Fatalf("TrackedKeys() = %d, want at most 4", got)
		}
	})
}
