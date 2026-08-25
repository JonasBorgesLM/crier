package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func recFrom(source, body string) LogRecord {
	return LogRecord{Body: body, Resource: Resource{ServiceName: source}}
}

func newFairShare(t *testing.T, capacity int, cfg FairShareConfig) *FairShareBuffer {
	t.Helper()
	inner := newBuffer(t, MemoryBufferConfig{
		Capacity: capacity, BatchSize: capacity,
		// Short, so a test that dequeues fewer records than a full batch is
		// not waiting out the production default.
		BatchWindow: 10 * time.Millisecond,
		Metrics:     cfg.Metrics,
	})
	f, err := NewFairShareBuffer(inner, cfg)
	if err != nil {
		t.Fatalf("NewFairShareBuffer: %v", err)
	}
	return f
}

// The property the whole ADR exists for: a flooding source must degrade
// itself, not its neighbours.
func TestQuietSourceStillAdmittedWhileNoisySourceFloods(t *testing.T) {
	var m CountingMetrics
	f := newFairShare(t, 100, FairShareConfig{
		Reservations: map[string]int{"noisy": 10, "quiet": 10},
		Metrics:      &m,
	})
	ctx := context.Background()

	// The noisy source burns its floor and then every spare slot.
	var rejected int
	for i := range 500 {
		if err := f.Enqueue(ctx, recFrom("noisy", fmt.Sprint(i))); err != nil {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("the flooding source was never throttled")
	}

	// The quiet source's floor is untouched by any of that.
	for i := range 10 {
		if err := f.Enqueue(ctx, recFrom("quiet", fmt.Sprint(i))); err != nil {
			t.Fatalf("quiet source rejected at record %d: %v", i, err)
		}
	}

	if got := f.Usage("quiet"); got != 10 {
		t.Errorf("quiet usage = %d, want 10", got)
	}
	if got := m.Snapshot().Dropped[DropKey{"noisy", DropSourceQuota}]; got == 0 {
		t.Error("noisy source's rejections were not counted against it")
	}
	if got := m.Snapshot().Dropped[DropKey{"quiet", DropSourceQuota}]; got != 0 {
		t.Errorf("quiet source was charged %d rejections it never had", got)
	}
}

// Operators who cannot tell throttling from capacity pressure resize the
// buffer to fix a quota problem, which does nothing.
func TestQuotaRejectionIsDistinguishableFromBufferFull(t *testing.T) {
	var m CountingMetrics
	f := newFairShare(t, 20, FairShareConfig{
		Reservations: map[string]int{"a": 5, "b": 5},
		Metrics:      &m,
	})
	ctx := context.Background()

	// Fill a's floor plus all 10 spare slots. The buffer still has b's 5.
	for i := range 15 {
		if err := f.Enqueue(ctx, recFrom("a", fmt.Sprint(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	err := f.Enqueue(ctx, recFrom("a", "over"))
	if !errors.Is(err, ErrSourceQuotaExhausted) {
		t.Fatalf("Enqueue = %v, want ErrSourceQuotaExhausted", err)
	}
	if errors.Is(err, ErrBufferFull) {
		t.Error("a quota rejection also reports as ErrBufferFull, so the two are indistinguishable")
	}
	// The error names the source, so a log line is actionable on its own.
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("error %q does not name the throttled source", err)
	}

	snap := m.Snapshot()
	if got := snap.DroppedBy(DropSourceQuota); got != 1 {
		t.Errorf("DroppedBy(source_quota) = %d, want 1", got)
	}
	if got := snap.DroppedBy(DropBufferFull); got != 0 {
		t.Errorf("DroppedBy(buffer_full) = %d, want 0 — the buffer was not full", got)
	}

	// And b's floor really was held back for it.
	if err := f.Enqueue(ctx, recFrom("b", "reserved")); err != nil {
		t.Errorf("b was refused its own reservation: %v", err)
	}
}

// A source at its floor may still use spare — it just loses it first.
func TestSourceAtItsFloorStillUsesSpare(t *testing.T) {
	f := newFairShare(t, 10, FairShareConfig{Reservations: map[string]int{"a": 2, "b": 2}})
	ctx := context.Background()

	for i := range 8 { // 2 reserved + all 6 spare
		if err := f.Enqueue(ctx, recFrom("a", fmt.Sprint(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if got := f.SpareInUse(); got != 6 {
		t.Errorf("SpareInUse() = %d, want 6", got)
	}
	if err := f.Enqueue(ctx, recFrom("a", "over")); !errors.Is(err, ErrSourceQuotaExhausted) {
		t.Errorf("Enqueue = %v, want ErrSourceQuotaExhausted once spare is gone", err)
	}
}

func TestDequeueReturnsSlotsToTheirSource(t *testing.T) {
	f := newFairShare(t, 10, FairShareConfig{Reservations: map[string]int{"a": 2}, UnlistedPool: 2})
	ctx := context.Background()

	// 2 reserved plus the 6 spare slots left after the unlisted pool.
	for i := range 8 {
		if err := f.Enqueue(ctx, recFrom("a", fmt.Sprint(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if err := f.Enqueue(ctx, recFrom("a", "over")); !errors.Is(err, ErrSourceQuotaExhausted) {
		t.Fatalf("Enqueue = %v, want ErrSourceQuotaExhausted", err)
	}

	if _, err := f.DequeueBatch(ctx); err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}

	if got := f.Usage("a"); got != 0 {
		t.Errorf("Usage(a) = %d, want 0 after drain", got)
	}
	if got := f.SpareInUse(); got != 0 {
		t.Errorf("SpareInUse() = %d, want 0 after drain", got)
	}
	if err := f.Enqueue(ctx, recFrom("a", "again")); err != nil {
		t.Errorf("source could not send after its records drained: %v", err)
	}
}

// Not releasing on a failed inner Enqueue leaks the source's quota one failure
// at a time until it can never send again.
func TestFailedInnerEnqueueReleasesTheSlot(t *testing.T) {
	inner := newBuffer(t, MemoryBufferConfig{Capacity: 2, BatchSize: 2})
	f, err := NewFairShareBuffer(inner, FairShareConfig{UnlistedPool: 1})
	if err != nil {
		t.Fatalf("NewFairShareBuffer: %v", err)
	}
	ctx := context.Background()

	if err := inner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for range 10 {
		if err := f.Enqueue(ctx, recFrom("a", "x")); !errors.Is(err, ErrBufferClosed) {
			t.Fatalf("Enqueue = %v, want ErrBufferClosed", err)
		}
	}
	if got := f.Usage("a"); got != 0 {
		t.Errorf("Usage(a) = %d, want 0 — refused records must not consume quota", got)
	}
}

// A source cannot escape its quota by renaming itself: the field admission
// reads is the one ADR-0008 overwrites server-side.
func TestQuotaKeysOnTheRecordsAttestedIdentity(t *testing.T) {
	f := newFairShare(t, 10, FairShareConfig{Reservations: map[string]int{"greedy": 1}, UnlistedPool: 1})
	ctx := context.Background()

	// Everything the caller can influence is the body; the identity field is
	// what admission reads.
	for i := range 20 {
		_ = f.Enqueue(ctx, recFrom("greedy", fmt.Sprintf("claiming to be someone-else-%d", i)))
	}
	if got := f.Usage("greedy"); got > 10 {
		t.Errorf("Usage(greedy) = %d, above the whole buffer", got)
	}
	if got := f.Usage("someone-else-0"); got != 0 {
		t.Errorf("records were attributed to a name from the body: %d", got)
	}
}

func TestUnattributedRecordsGetTheirOwnBucket(t *testing.T) {
	var m CountingMetrics
	f := newFairShare(t, 4, FairShareConfig{UnlistedPool: 1, Metrics: &m})
	ctx := context.Background()

	// 1 from the shared pool plus 3 spare fills the buffer; the rest are
	// quota rejections, not capacity ones.
	for range 10 {
		_ = f.Enqueue(ctx, LogRecord{Body: "no identity"})
	}

	if got := f.Usage(UnattributedSource); got == 0 {
		t.Error("records with no identity were not accounted for at all")
	}
	if got := m.Snapshot().Dropped[DropKey{UnattributedSource, DropSourceQuota}]; got == 0 {
		t.Error("unattributed rejections were not counted under their own label")
	}
}

// Reservations above capacity silently under-deliver at runtime, where it
// looks like random loss (NFR4).
func TestNewFairShareBufferValidatesEagerly(t *testing.T) {
	inner := newBuffer(t, MemoryBufferConfig{Capacity: 10, BatchSize: 10})

	for _, tc := range []struct {
		name string
		cfg  FairShareConfig
		want string
	}{
		{
			"reservations exceed capacity",
			FairShareConfig{Reservations: map[string]int{"a": 6, "b": 6}},
			"above the buffer capacity",
		},
		{
			"negative reservation",
			FairShareConfig{Reservations: map[string]int{"a": -1}},
			"non-negative",
		},
		{
			"unlisted pool overruns what is left",
			FairShareConfig{Reservations: map[string]int{"a": 9}, UnlistedPool: 5},
			"sum to more than",
		},
		{"negative unlisted pool", FairShareConfig{UnlistedPool: -1}, "non-negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewFairShareBuffer(inner, tc.cfg)
			if err == nil {
				t.Fatal("NewFairShareBuffer accepted an impossible configuration")
			}
			if f != nil {
				t.Error("a buffer was returned alongside the error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.want)
			}
		})
	}
}

func TestNewFairShareBufferRejectsAnUnmeasurableStore(t *testing.T) {
	if _, err := NewFairShareBuffer(nil, FairShareConfig{}); err == nil {
		t.Error("a nil inner store was accepted")
	}
	if _, err := NewFairShareBuffer(unmeasurableStore{}, FairShareConfig{}); err == nil {
		t.Error("a store that cannot report capacity was accepted, so reservations went unvalidated")
	}
}

type unmeasurableStore struct{ BufferStore }

func TestFairShareIsConcurrencySafe(t *testing.T) {
	f := newFairShare(t, 200, FairShareConfig{
		Reservations: map[string]int{"a": 20, "b": 20}, UnlistedPool: 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(5)
	for _, source := range []string{"a", "b", "c", "d"} {
		go func() {
			defer wg.Done()
			for i := range 400 {
				_ = f.Enqueue(ctx, recFrom(source, fmt.Sprint(i)))
			}
		}()
	}
	go func() {
		defer wg.Done()
		for range 40 {
			if _, err := f.DequeueBatch(ctx); err != nil {
				return // context expired or the buffer closed; stop draining
			}
		}
	}()
	wg.Wait()

	if got := f.SpareInUse(); got < 0 {
		t.Errorf("SpareInUse() = %d, went negative — the accounting drifted", got)
	}
	for _, source := range []string{"a", "b", "c", "d"} {
		if got := f.Usage(source); got < 0 {
			t.Errorf("Usage(%s) = %d, went negative", source, got)
		}
	}
}

// Regression: the pools must be disjoint and sum to exactly the capacity.
//
// An earlier version granted every unlisted source its own default floor on
// top of the spare pool, which admitted more records than the buffer held.
// The symptom was not a crash — it was the inner store rejecting with
// ErrBufferFull while the quota accounting still believed there was room, so
// capacity pressure was reported as though the buffer were fine.
func TestAdmissionNeverExceedsCapacity(t *testing.T) {
	const capacity = 16

	for _, cfg := range []FairShareConfig{
		{},
		{UnlistedPool: capacity},
		{UnlistedPool: 4},
		{Reservations: map[string]int{"a": 4, "b": 4}},
		{Reservations: map[string]int{"a": 4, "b": 4}, UnlistedPool: 8},
		{Reservations: map[string]int{"a": capacity}},
	} {
		t.Run(fmt.Sprintf("%v/pool=%d", cfg.Reservations, cfg.UnlistedPool), func(t *testing.T) {
			inner := newBuffer(t, MemoryBufferConfig{
				Capacity: capacity, BatchSize: capacity, BatchWindow: 10 * time.Millisecond,
			})
			f, err := NewFairShareBuffer(inner, cfg)
			if err != nil {
				t.Fatalf("NewFairShareBuffer: %v", err)
			}
			ctx := context.Background()

			var admitted int
			for _, source := range []string{"a", "b", "unlisted-1", "unlisted-2", ""} {
				for range capacity * 2 {
					err := f.Enqueue(ctx, recFrom(source, "x"))
					switch {
					case err == nil:
						admitted++
					case errors.Is(err, ErrSourceQuotaExhausted):
					case errors.Is(err, ErrBufferFull):
						// Admission handed out a slot the buffer did not have.
						t.Fatalf("inner buffer reported full while quota accounting still admitted: "+
							"%d admitted, capacity %d", admitted, capacity)
					default:
						t.Fatalf("Enqueue: %v", err)
					}
				}
			}

			if admitted > capacity {
				t.Errorf("admitted %d records into a buffer of %d", admitted, capacity)
			}
			if got := inner.Depth(); got != admitted {
				t.Errorf("buffer holds %d but admission counted %d", got, admitted)
			}
		})
	}
}

// Releasing must free the least-protected slot first, so a source keeps its
// guaranteed floor for as long as it holds anything at all.
func TestReleaseFreesSpareBeforeReservation(t *testing.T) {
	f := newFairShare(t, 10, FairShareConfig{Reservations: map[string]int{"a": 2, "b": 2}})
	ctx := context.Background()

	for range 5 { // 2 reserved + 3 spare
		if err := f.Enqueue(ctx, recFrom("a", "x")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if got := f.SpareInUse(); got != 3 {
		t.Fatalf("SpareInUse() = %d, want 3", got)
	}

	f.release("a")
	if got := f.SpareInUse(); got != 2 {
		t.Errorf("SpareInUse() = %d, want 2 — the reservation was freed before the spare", got)
	}
	if got := f.Usage("a"); got != 4 {
		t.Errorf("Usage(a) = %d, want 4", got)
	}
}
