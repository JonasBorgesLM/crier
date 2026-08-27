package core

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// In-memory buffer defaults (ADR-0002).
const (
	DefaultBufferCapacity = 10_000
	DefaultBatchSize      = 512
	DefaultBatchWindow    = 5 * time.Second
)

// MemoryBufferConfig configures a MemoryBuffer.
type MemoryBufferConfig struct {
	// Capacity is the maximum number of records held. Zero means
	// DefaultBufferCapacity. The bound is never relaxed under pressure — that
	// is the whole point of having one (ADR-0015).
	Capacity int
	// BatchSize is how many records a full batch holds. Zero means
	// DefaultBatchSize.
	BatchSize int
	// BatchWindow is how long the oldest pending record waits for company
	// before the batch goes out short. Zero means DefaultBatchWindow.
	BatchWindow time.Duration
	// Policy is what happens on a full buffer. Defaults to DropPolicyReject.
	Policy DropPolicy
	// Metrics receives depth and drop counts. Nil discards.
	Metrics Metrics
}

// MemoryBuffer is the default BufferStore: a bounded ring buffer that batches
// by size or by time window, whichever comes first.
//
// Safe for concurrent use by many producers and many consumers.
type MemoryBuffer struct {
	capacity    int
	batchSize   int
	batchWindow time.Duration
	policy      DropPolicy
	metrics     Metrics

	mu      sync.Mutex
	ring    []LogRecord
	head    int
	count   int
	firstAt time.Time
	closed  bool

	// Broadcast channels. A waiter takes the current channel under the lock
	// and selects on it outside; a state change closes it and installs a new
	// one. This is sync.Cond with the one thing sync.Cond cannot do —
	// participate in a select alongside a timer and a context.
	notEmpty chan struct{}
	notFull  chan struct{}

	// now is the clock, overridable in tests.
	now func() time.Time
}

var _ BufferStore = (*MemoryBuffer)(nil)

// NewMemoryBuffer builds a buffer from cfg, validating it eagerly (NFR4).
func NewMemoryBuffer(cfg MemoryBufferConfig) (*MemoryBuffer, error) {
	capacity := cfg.Capacity
	if capacity == 0 {
		capacity = DefaultBufferCapacity
	}
	if capacity < 0 {
		return nil, fmt.Errorf("buffer capacity is %d, want a positive number", capacity)
	}
	batchSize := cfg.BatchSize
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize < 0 {
		return nil, fmt.Errorf("batch size is %d, want a positive number", batchSize)
	}
	if batchSize > capacity {
		// Not fatal on its own, but it means the size trigger can never fire
		// and every batch waits out the window. Better to say so at startup
		// than to have someone wonder why throughput collapsed.
		return nil, fmt.Errorf("batch size %d exceeds buffer capacity %d, so batches could only ever be flushed by timeout",
			batchSize, capacity)
	}
	window := cfg.BatchWindow
	if window == 0 {
		window = DefaultBatchWindow
	}
	if window < 0 {
		return nil, fmt.Errorf("batch window is %v, want a positive duration", window)
	}
	if !cfg.Policy.Valid() {
		return nil, fmt.Errorf("drop policy %v is not one of reject, block, drop-oldest", cfg.Policy)
	}

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NopMetrics{}
	}

	return &MemoryBuffer{
		capacity:    capacity,
		batchSize:   batchSize,
		batchWindow: window,
		policy:      cfg.Policy,
		metrics:     metrics,
		ring:        make([]LogRecord, capacity),
		notEmpty:    make(chan struct{}),
		notFull:     make(chan struct{}),
		now:         time.Now,
	}, nil
}

// signalNotEmpty wakes consumers waiting for records. Callers hold b.mu.
func (b *MemoryBuffer) signalNotEmpty() {
	close(b.notEmpty)
	b.notEmpty = make(chan struct{})
}

// signalNotFull wakes producers waiting for room. Callers hold b.mu.
func (b *MemoryBuffer) signalNotFull() {
	close(b.notFull)
	b.notFull = make(chan struct{})
}

// Enqueue implements BufferStore.
//
// source labels the drop counters. It is the attested principal, passed in
// rather than read from the record, because the record's own resource fields
// are not authoritative (ADR-0008).
func (b *MemoryBuffer) Enqueue(ctx context.Context, rec LogRecord) error {
	return b.EnqueueFrom(ctx, rec, rec.Resource.ServiceName)
}

// EnqueueFrom is Enqueue with an explicit source label for metrics.
func (b *MemoryBuffer) EnqueueFrom(ctx context.Context, rec LogRecord, source string) error {
	for {
		b.mu.Lock()

		if b.closed {
			b.mu.Unlock()
			return ErrBufferClosed
		}

		if b.count < b.capacity {
			b.push(rec)
			depth := b.count
			b.signalNotEmpty()
			b.mu.Unlock()
			b.metrics.BufferDepth(depth)
			return nil
		}

		switch b.policy {
		case DropPolicyReject:
			b.mu.Unlock()
			b.metrics.RecordsDropped(source, DropBufferFull, 1)
			return ErrBufferFull

		case DropPolicyDropOldest:
			evicted := b.ring[b.head]
			b.head = (b.head + 1) % b.capacity
			b.count--
			b.push(rec)
			depth := b.count
			b.signalNotEmpty()
			b.mu.Unlock()
			// Counted against the evicted record's own source, not the
			// arriving one: the loss belongs to whoever lost data.
			b.metrics.RecordsDropped(evicted.Resource.ServiceName, DropOldest, 1)
			b.metrics.BufferDepth(depth)
			return nil

		case DropPolicyBlock:
			wait := b.notFull
			b.mu.Unlock()
			select {
			case <-wait:
				continue // room may exist now; re-check under the lock
			case <-ctx.Done():
				b.metrics.RecordsDropped(source, DropBufferFull, 1)
				return ctx.Err()
			}

		default:
			b.mu.Unlock()
			return fmt.Errorf("unreachable: %w: policy %v", ErrBufferFull, b.policy)
		}
	}
}

// push appends rec. Callers hold b.mu and have checked for room.
func (b *MemoryBuffer) push(rec LogRecord) {
	if b.count == 0 {
		b.firstAt = b.now()
	}
	b.ring[(b.head+b.count)%b.capacity] = rec
	b.count++
}

// DequeueBatch implements BufferStore.
func (b *MemoryBuffer) DequeueBatch(ctx context.Context) ([]LogRecord, error) {
	for {
		b.mu.Lock()

		switch {
		case b.count >= b.batchSize:
			return b.takeLocked(b.batchSize), nil

		case b.count > 0 && b.closed:
			// Drain: on the way down, a partial batch beats waiting out a
			// window nobody is going to fill (ADR-0015).
			return b.takeLocked(b.count), nil

		case b.count > 0 && b.now().Sub(b.firstAt) >= b.batchWindow:
			return b.takeLocked(b.count), nil

		case b.count == 0 && b.closed:
			b.mu.Unlock()
			return nil, ErrBufferClosed
		}

		wait := b.notEmpty
		var timer *time.Timer
		var timeout <-chan time.Time
		if b.count > 0 {
			remaining := b.batchWindow - b.now().Sub(b.firstAt)
			timer = time.NewTimer(remaining)
			timeout = timer.C
		}
		b.mu.Unlock()

		select {
		case <-wait:
		case <-timeout:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, ctx.Err()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// takeLocked removes n records and returns them. Callers hold b.mu; it
// unlocks before returning.
func (b *MemoryBuffer) takeLocked(n int) []LogRecord {
	out := make([]LogRecord, n)
	for i := range n {
		idx := (b.head + i) % b.capacity
		out[i] = b.ring[idx]
		// Cleared so the ring stops pinning the record's attribute maps for
		// as long as the buffer lives.
		b.ring[idx] = LogRecord{}
	}
	b.head = (b.head + n) % b.capacity
	b.count -= n
	if b.count > 0 {
		b.firstAt = b.now()
	}
	depth := b.count
	b.signalNotFull()
	b.mu.Unlock()

	b.metrics.BufferDepth(depth)
	return out
}

// Depth implements BufferStore.
func (b *MemoryBuffer) Depth() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// Capacity reports the configured bound.
func (b *MemoryBuffer) Capacity() int { return b.capacity }

// Close implements BufferStore. It is idempotent and discards nothing: every
// record already inside stays available to DequeueBatch until drained.
func (b *MemoryBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	// Wake everyone: blocked producers so they see ErrBufferClosed, blocked
	// consumers so they drain instead of waiting out a window.
	b.signalNotEmpty()
	b.signalNotFull()
	return nil
}
