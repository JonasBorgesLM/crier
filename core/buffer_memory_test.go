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

func newBuffer(t *testing.T, cfg MemoryBufferConfig) *MemoryBuffer {
	t.Helper()
	b, err := NewMemoryBuffer(cfg)
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func rec(body string) LogRecord {
	return LogRecord{Body: body, Resource: Resource{ServiceName: "task-api"}}
}

// Each policy gets a test that fills the buffer and asserts the specific
// behaviour and the specific counter.
func TestDropPolicyReject(t *testing.T) {
	var m CountingMetrics
	b := newBuffer(t, MemoryBufferConfig{Capacity: 2, BatchSize: 2, Policy: DropPolicyReject, Metrics: &m})
	ctx := context.Background()

	if err := b.Enqueue(ctx, rec("a")); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if err := b.Enqueue(ctx, rec("b")); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	err := b.Enqueue(ctx, rec("c"))
	if !errors.Is(err, ErrBufferFull) {
		t.Fatalf("Enqueue on a full buffer = %v, want ErrBufferFull", err)
	}
	if got := b.Depth(); got != 2 {
		t.Errorf("Depth() = %d, want 2 — reject must not evict", got)
	}
	if got := m.Snapshot().DroppedBy(DropBufferFull); got != 1 {
		t.Errorf("DroppedBy(buffer_full) = %d, want 1", got)
	}
}

func TestDropPolicyDropOldest(t *testing.T) {
	var m CountingMetrics
	b := newBuffer(t, MemoryBufferConfig{Capacity: 2, BatchSize: 2, Policy: DropPolicyDropOldest, Metrics: &m})
	ctx := context.Background()

	for _, body := range []string{"a", "b", "c"} {
		if err := b.Enqueue(ctx, rec(body)); err != nil {
			t.Fatalf("Enqueue(%s): %v", body, err)
		}
	}

	batch, err := b.DequeueBatch(ctx)
	if err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}
	if len(batch) != 2 || batch[0].Body != "b" || batch[1].Body != "c" {
		t.Errorf("batch = %v, want [b c] — the oldest should have been evicted", bodies(batch))
	}
	if got := m.Snapshot().DroppedBy(DropOldest); got != 1 {
		t.Errorf("DroppedBy(drop_oldest) = %d, want 1", got)
	}
	if got := m.Snapshot().DroppedBy(DropBufferFull); got != 0 {
		t.Errorf("DroppedBy(buffer_full) = %d, want 0 — the reasons must stay distinguishable", got)
	}
}

func TestDropPolicyBlockWaitsForRoom(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 1, BatchSize: 1, Policy: DropPolicyBlock})
	ctx := context.Background()

	if err := b.Enqueue(ctx, rec("a")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	admitted := make(chan error, 1)
	go func() { admitted <- b.Enqueue(ctx, rec("b")) }()

	select {
	case err := <-admitted:
		t.Fatalf("Enqueue returned %v on a full buffer instead of blocking", err)
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := b.DequeueBatch(ctx); err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}

	select {
	case err := <-admitted:
		if err != nil {
			t.Errorf("blocked Enqueue = %v, want nil once room appeared", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Enqueue never woke after room appeared")
	}
}

func TestDropPolicyBlockRespectsContext(t *testing.T) {
	var m CountingMetrics
	b := newBuffer(t, MemoryBufferConfig{Capacity: 1, BatchSize: 1, Policy: DropPolicyBlock, Metrics: &m})

	if err := b.Enqueue(context.Background(), rec("a")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// A blocking policy must still be interruptible, or a shutdown hangs on
	// whichever producer happened to be waiting.
	if err := b.Enqueue(ctx, rec("b")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Enqueue = %v, want DeadlineExceeded", err)
	}
	if got := m.Snapshot().DroppedBy(DropBufferFull); got != 1 {
		t.Errorf("DroppedBy(buffer_full) = %d, want 1 — an abandoned record is still a loss", got)
	}
}

func TestBatchIsReturnedWhenFull(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 10, BatchSize: 3, BatchWindow: time.Hour})
	ctx := context.Background()

	for _, body := range []string{"a", "b", "c", "d"} {
		if err := b.Enqueue(ctx, rec(body)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	// The window is an hour; only the size trigger can produce this.
	batch, err := b.DequeueBatch(ctx)
	if err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}
	if len(batch) != 3 {
		t.Errorf("batch has %d records, want 3", len(batch))
	}
	if got := b.Depth(); got != 1 {
		t.Errorf("Depth() = %d, want 1", got)
	}
}

func TestBatchIsReturnedWhenWindowExpires(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{
		Capacity: 100, BatchSize: 100, BatchWindow: 40 * time.Millisecond,
	})
	ctx := context.Background()

	if err := b.Enqueue(ctx, rec("lonely")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	start := time.Now()
	batch, err := b.DequeueBatch(ctx)
	if err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("batch has %d records, want 1", len(batch))
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned after %v, want it to wait out the window", elapsed)
	}
}

func TestDequeueBlocksOnAnEmptyBufferUntilContextDone(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 4, BatchSize: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := b.DequeueBatch(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DequeueBatch = %v, want DeadlineExceeded", err)
	}
}

// Close must not discard what is already inside — draining it is the whole
// point of a bounded shutdown (ADR-0015).
func TestCloseDrainsBeforeReportingClosed(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 100, BatchSize: 100, BatchWindow: time.Hour})
	ctx := context.Background()

	for _, body := range []string{"a", "b", "c"} {
		if err := b.Enqueue(ctx, rec(body)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A partial batch beats waiting out an hour-long window nobody will fill.
	batch, err := b.DequeueBatch(ctx)
	if err != nil {
		t.Fatalf("DequeueBatch after Close: %v", err)
	}
	if len(batch) != 3 {
		t.Errorf("drained %d records, want 3", len(batch))
	}

	if _, err := b.DequeueBatch(ctx); !errors.Is(err, ErrBufferClosed) {
		t.Errorf("DequeueBatch on a drained closed buffer = %v, want ErrBufferClosed", err)
	}
}

func TestEnqueueAfterCloseIsRejected(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 4, BatchSize: 2})
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Enqueue(context.Background(), rec("late")); !errors.Is(err, ErrBufferClosed) {
		t.Errorf("Enqueue after Close = %v, want ErrBufferClosed", err)
	}
}

func TestCloseIsIdempotentAndWakesBlockedProducers(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 1, BatchSize: 1, Policy: DropPolicyBlock})
	ctx := context.Background()

	if err := b.Enqueue(ctx, rec("a")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- b.Enqueue(ctx, rec("b")) }()
	time.Sleep(30 * time.Millisecond)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case err := <-blocked:
		if !errors.Is(err, ErrBufferClosed) {
			t.Errorf("blocked producer = %v, want ErrBufferClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close left a producer blocked forever")
	}
}

// The ring must not pin the attribute maps of records it has already handed
// out — that turns a bounded buffer into an unbounded retainer.
func TestDequeuedSlotsAreCleared(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{Capacity: 4, BatchSize: 2, BatchWindow: time.Millisecond})
	ctx := context.Background()

	for i := range 2 {
		r := rec(fmt.Sprintf("r%d", i))
		r.Attributes = map[string]any{"big": strings.Repeat("x", 1024)}
		if err := b.Enqueue(ctx, r); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if _, err := b.DequeueBatch(ctx); err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, held := range b.ring {
		if held.Attributes != nil || held.Body != "" {
			t.Errorf("ring[%d] still holds %+v", i, held)
		}
	}
}

func TestBufferIsConcurrencySafe(t *testing.T) {
	b := newBuffer(t, MemoryBufferConfig{
		Capacity: 256, BatchSize: 16, BatchWindow: 5 * time.Millisecond,
		Policy: DropPolicyDropOldest,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(6)
	for p := range 4 {
		go func() {
			defer wg.Done()
			for i := range 500 {
				_ = b.Enqueue(ctx, rec(fmt.Sprintf("p%d-%d", p, i)))
			}
		}()
	}
	consumed := make(chan int, 2)
	for range 2 {
		go func() {
			defer wg.Done()
			var n int
			for {
				batch, err := b.DequeueBatch(ctx)
				if err != nil {
					consumed <- n
					return
				}
				n += len(batch)
			}
		}()
	}
	wg.Wait()
	close(consumed)

	for n := range consumed {
		if n < 0 {
			t.Errorf("negative consumption: %d", n)
		}
	}
	if got := b.Depth(); got < 0 || got > b.Capacity() {
		t.Errorf("Depth() = %d, outside [0,%d]", got, b.Capacity())
	}
}

func TestNewMemoryBufferValidatesEagerly(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  MemoryBufferConfig
		want string
	}{
		{"negative capacity", MemoryBufferConfig{Capacity: -1}, "capacity"},
		{"negative batch size", MemoryBufferConfig{BatchSize: -1}, "batch size"},
		{"batch larger than buffer", MemoryBufferConfig{Capacity: 4, BatchSize: 8}, "exceeds buffer capacity"},
		{"negative window", MemoryBufferConfig{BatchWindow: -time.Second}, "batch window"},
		{"unknown policy", MemoryBufferConfig{Policy: DropPolicy(9)}, "drop policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewMemoryBuffer(tc.cfg)
			if err == nil {
				t.Fatal("NewMemoryBuffer accepted an unusable configuration")
			}
			if b != nil {
				t.Error("a buffer was returned alongside the error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the problem %q", err, tc.want)
			}
		})
	}
}

func TestDropPolicyString(t *testing.T) {
	for _, tc := range []struct {
		policy DropPolicy
		want   string
	}{
		{DropPolicyReject, "reject"},
		{DropPolicyBlock, "block"},
		{DropPolicyDropOldest, "drop-oldest"},
		{DropPolicy(7), "DropPolicy(7)"},
	} {
		if got := tc.policy.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func bodies(batch []LogRecord) []string {
	out := make([]string, len(batch))
	for i, r := range batch {
		out[i] = r.Body
	}
	return out
}
