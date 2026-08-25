package core

import (
	"context"
	"fmt"
	"sync"
)

// UnattributedSource is the bucket for records that reach admission without an
// attested identity. It exists so such records are still accounted for rather
// than sharing an empty-string key with each other invisibly; in standalone
// mode authentication means it should stay empty, and a non-zero count there
// is worth investigating.
const UnattributedSource = "<unattributed>"

// FairShareConfig configures per-source admission (ADR-0011).
type FairShareConfig struct {
	// Reservations is the guaranteed floor of buffer capacity per source, by
	// attested identity. A source at its floor can still use spare capacity;
	// it just loses that spare first when the buffer comes under pressure.
	Reservations map[string]int

	// UnlistedPool is capacity held back for sources with no explicit
	// reservation, shared among all of them.
	//
	// It is collective, not per-source, and that is a deliberate correction
	// rather than a shortcut: a per-source default cannot be guaranteed,
	// because the number of unlisted sources is unknowable at startup, so
	// granting each of them a floor admits more records than the buffer holds.
	// A collective pool keeps the bound exact — reserved + unlisted + spare is
	// always the capacity — at the cost of unlisted sources competing with
	// each other. Listing the sources that matter is the way to get a real
	// floor, which is the honest incentive.
	UnlistedPool int

	// Metrics receives quota rejections. Nil discards.
	Metrics Metrics
}

// FairShareBuffer decorates a BufferStore with per-source admission, so a
// noisy source degrades itself rather than its neighbours (ADR-0011). That
// property is what makes one shared instance viable at all.
//
// # Identity
//
// Admission and accounting key on rec.Resource.ServiceName. By the time a
// record reaches admission — step 8 of the stage order — that field has been
// overwritten from the authenticated principal at step 3 (ADR-0008), so a
// source cannot escape its own quota by renaming itself. Using it here is the
// reason that overwrite exists.
//
// # Quota state
//
// In-process only; it does not survive a restart, and replicas do not share
// it. Distributed quota is out of scope for the MVP, the same line moat draws
// between memory and Redis stores.
type FairShareBuffer struct {
	inner         BufferStore
	reservations  map[string]int
	unlistedPool  int
	spareCapacity int
	metrics       Metrics

	mu           sync.Mutex
	usage        map[string]*sourceUsage
	unlistedUsed int // unlisted pool currently in use
	spare        int // spare capacity currently in use
}

// sourceUsage counts a source's buffered records by the pool that paid for
// each one.
//
// Tracked explicitly rather than inferred on release: inference has to replay
// the admission rule backwards, and any disagreement between the two drifts
// the accounting until a source is throttled for records it no longer has
// buffered — a bug that would only show up under sustained mixed load.
type sourceUsage struct {
	reserved int
	unlisted int
	spare    int
}

func (u *sourceUsage) total() int { return u.reserved + u.unlisted + u.spare }

var _ BufferStore = (*FairShareBuffer)(nil)

// capacityReporter is implemented by buffers that can state their bound.
// Validating reservations against capacity requires knowing it, and a store
// that cannot say what its capacity is cannot have its reservations checked.
type capacityReporter interface {
	Capacity() int
}

// NewFairShareBuffer wraps inner with per-source admission.
//
// Reservations are validated eagerly (NFR4): reservations summing above the
// buffer's capacity is a configuration error that must fail at startup rather
// than silently under-deliver at runtime, where it looks like random loss.
func NewFairShareBuffer(inner BufferStore, cfg FairShareConfig) (*FairShareBuffer, error) {
	if inner == nil {
		return nil, fmt.Errorf("fair-share buffer needs an inner store")
	}
	reporter, ok := inner.(capacityReporter)
	if !ok {
		return nil, fmt.Errorf("inner store %T does not report Capacity(), so reservations cannot be validated", inner)
	}
	capacity := reporter.Capacity()

	var reserved int
	for source, n := range cfg.Reservations {
		if n < 0 {
			return nil, fmt.Errorf("reservation for %q is %d, want a non-negative number", source, n)
		}
		reserved += n
	}
	if reserved > capacity {
		return nil, fmt.Errorf("reservations sum to %d, above the buffer capacity of %d: "+
			"every source cannot be guaranteed more than the buffer holds", reserved, capacity)
	}
	if cfg.UnlistedPool < 0 {
		return nil, fmt.Errorf("unlisted pool is %d, want a non-negative number", cfg.UnlistedPool)
	}
	if reserved+cfg.UnlistedPool > capacity {
		return nil, fmt.Errorf("reservations (%d) plus the unlisted pool (%d) sum to more than "+
			"the buffer capacity of %d", reserved, cfg.UnlistedPool, capacity)
	}

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NopMetrics{}
	}

	reservations := make(map[string]int, len(cfg.Reservations))
	for k, v := range cfg.Reservations {
		reservations[k] = v
	}

	return &FairShareBuffer{
		inner:         inner,
		reservations:  reservations,
		unlistedPool:  cfg.UnlistedPool,
		spareCapacity: capacity - reserved - cfg.UnlistedPool,
		metrics:       metrics,
		usage:         make(map[string]*sourceUsage),
	}, nil
}

func sourceOf(rec LogRecord) string {
	if rec.Resource.ServiceName == "" {
		return UnattributedSource
	}
	return rec.Resource.ServiceName
}

// reservationFor returns a source's explicit floor, and whether it has one.
// Unlisted sources have no individual floor; they share UnlistedPool.
func (f *FairShareBuffer) reservationFor(source string) (int, bool) {
	n, listed := f.reservations[source]
	return n, listed
}

// Enqueue implements BufferStore, admitting the record only if its source's
// share allows it.
//
// It returns ErrSourceQuotaExhausted — never ErrBufferFull — when the source
// is over its share while the buffer still has room. Collapsing the two would
// have operators resizing the buffer to fix a throttling problem, which does
// nothing.
func (f *FairShareBuffer) Enqueue(ctx context.Context, rec LogRecord) error {
	source := sourceOf(rec)

	if err := f.reserve(source); err != nil {
		f.metrics.RecordsDropped(source, DropSourceQuota, 1)
		return err
	}

	if err := f.inner.Enqueue(ctx, rec); err != nil {
		// The inner store refused, so the slot was never really taken. Not
		// releasing it here would leak the source's quota one failed enqueue
		// at a time until it could never send again.
		f.release(source)
		return err
	}
	return nil
}

// reserve accounts for one admitted record, reporting which pool paid for it.
//
// The pools are disjoint and sum to the buffer's capacity, so admission can
// never hand out more slots than the inner store has room for. That invariant
// is the whole reason the pools are separate counters rather than one number.
func (f *FairShareBuffer) reserve(source string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	usage := f.usage[source]
	if usage == nil {
		usage = &sourceUsage{}
		f.usage[source] = usage
	}

	floor, listed := f.reservationFor(source)
	switch {
	case listed && usage.reserved < floor:
		// Inside its own floor: always admitted. Explicit reservations sum to
		// no more than capacity, so the slot is necessarily there.
		usage.reserved++
	case !listed && f.unlistedUsed < f.unlistedPool:
		usage.unlisted++
		f.unlistedUsed++
	case f.spare < f.spareCapacity:
		usage.spare++
		f.spare++
	default:
		if usage.total() == 0 {
			delete(f.usage, source)
		}
		return fmt.Errorf("%w: %q is at its share", ErrSourceQuotaExhausted, source)
	}
	return nil
}

// release returns one of the source's slots.
func (f *FairShareBuffer) release(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseLocked(source)
}

// releaseLocked frees the source's least-protected slot first: spare, then the
// shared unlisted pool, then its own reservation. A source therefore keeps its
// guaranteed floor for as long as it holds anything at all, which is what the
// floor is for. Callers hold f.mu.
func (f *FairShareBuffer) releaseLocked(source string) {
	usage := f.usage[source]
	if usage == nil {
		return
	}
	switch {
	case usage.spare > 0:
		usage.spare--
		f.spare--
	case usage.unlisted > 0:
		usage.unlisted--
		f.unlistedUsed--
	case usage.reserved > 0:
		usage.reserved--
	default:
		return
	}
	if usage.total() == 0 {
		// Deleted rather than left at zero, so the map tracks live sources
		// instead of every source ever seen.
		delete(f.usage, source)
	}
}

// DequeueBatch implements BufferStore, returning each record's slot to its
// source's share.
func (f *FairShareBuffer) DequeueBatch(ctx context.Context) ([]LogRecord, error) {
	batch, err := f.inner.DequeueBatch(ctx)
	if len(batch) == 0 {
		return batch, err
	}

	f.mu.Lock()
	for _, rec := range batch {
		f.releaseLocked(sourceOf(rec))
	}
	f.mu.Unlock()

	return batch, err
}

// Depth implements BufferStore.
func (f *FairShareBuffer) Depth() int { return f.inner.Depth() }

// Close implements BufferStore.
func (f *FairShareBuffer) Close() error { return f.inner.Close() }

// Usage reports how many of a source's records are currently buffered.
func (f *FairShareBuffer) Usage(source string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if usage := f.usage[source]; usage != nil {
		return usage.total()
	}
	return 0
}

// SpareInUse reports how much unreserved capacity is currently taken.
func (f *FairShareBuffer) SpareInUse() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spare
}

// UnlistedInUse reports how much of the shared unlisted pool is taken.
func (f *FairShareBuffer) UnlistedInUse() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unlistedUsed
}
