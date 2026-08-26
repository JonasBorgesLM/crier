package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DefaultExportWorkers is how many batches a Dispatcher keeps in flight when
// none is configured.
//
// It is small on purpose. Workers bound concurrency, not memory: a batch in
// flight has already left the buffer, so more workers do not buy more capacity
// — they buy tolerance for destinations that are slow but working. Four is
// enough to keep a healthy destination busy while another rides out a backoff,
// and low enough that a total outage does not park a large slice of the buffer
// in goroutines that are all waiting on the same dead socket.
const DefaultExportWorkers = 4

// CircuitReporter is a destination that knows whether it is currently
// refusing calls. *CircuitBreaker implements it.
//
// The Dispatcher uses it for one thing: when every destination is refusing,
// the pipeline is degraded, which readiness must reflect (ADR-0015).
type CircuitReporter interface {
	// Name is the destination's metrics label.
	Name() string
	// Open reports whether calls are being refused right now.
	Open() bool
}

// DispatcherConfig configures a Dispatcher. Build one with NewDispatcher,
// which validates eagerly (NFR4).
type DispatcherConfig struct {
	// Buffer is drained by the workers. Required.
	Buffer BufferStore

	// Exporter receives each batch — normally the *FanOut, with retry and
	// circuit breaking already composed inside it (ADR-0013). Required.
	Exporter Exporter

	// Workers is how many batches may be in flight at once. Zero means
	// DefaultExportWorkers.
	Workers int

	// Circuits are the breakers whose collective state decides whether the
	// pipeline is degraded. Nil discovers them by walking Exporter, which is
	// what makes readiness work without a second place to keep the same list
	// in step.
	//
	// Set it explicitly only for an exporter chain the walk cannot see
	// through — a custom decorator, or a breaker of your own.
	Circuits []CircuitReporter

	// Metrics receives export and drop counters. Nil discards.
	Metrics Metrics
}

// Dispatcher drains the buffer and exports, on a bounded pool of workers
// (ADR-0016).
//
// It is the only component that knows whether a batch reached anywhere at all,
// so it owns the two accounting duties on the export side: counting records
// that reached no destination (ADR-0015), and reporting the degraded state
// that readiness reflects.
type Dispatcher struct {
	buffer   BufferStore
	exporter Exporter
	workers  int
	metrics  Metrics

	circuits []CircuitReporter
	// guarded is true when every destination has a breaker. Without that, one
	// unguarded destination is always assumed usable and the pipeline is
	// never reported degraded — which is correct, and worth being able to
	// state rather than infer.
	guarded bool

	startOnce sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup

	mu            sync.Mutex
	started       bool
	workerErr     error
	degraded      bool
	degradedKnown bool
}

// NewDispatcher validates cfg and returns the dispatcher. It does not start
// any workers; call Start.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Buffer == nil {
		return nil, errors.New("dispatcher needs a buffer")
	}
	if cfg.Exporter == nil {
		return nil, errors.New("dispatcher needs an exporter")
	}
	if cfg.Workers < 0 {
		return nil, fmt.Errorf("negative Workers %d, want at least 1", cfg.Workers)
	}

	d := &Dispatcher{
		buffer:   cfg.Buffer,
		exporter: cfg.Exporter,
		workers:  cfg.Workers,
		metrics:  cfg.Metrics,
		circuits: cfg.Circuits,
	}
	if d.workers == 0 {
		d.workers = DefaultExportWorkers
	}
	if d.metrics == nil {
		d.metrics = NopMetrics{}
	}

	if d.circuits == nil {
		d.circuits, d.guarded = discoverCircuits(cfg.Exporter)
	} else {
		d.guarded = len(d.circuits) > 0
	}
	return d, nil
}

// discoverCircuits walks an exporter chain and collects one breaker per
// destination, reporting whether every destination had one.
//
// An operator who composes the chain correctly should not also have to hand
// the same breakers to the dispatcher; forgetting to would leave readiness
// permanently reporting health, which is the silent failure this project
// exists to avoid.
func discoverCircuits(e Exporter) (circuits []CircuitReporter, everyDestination bool) {
	switch t := e.(type) {
	case *FanOut:
		everyDestination = true
		for _, dest := range t.destinations {
			found, ok := discoverCircuits(dest.Exporter)
			if !ok {
				everyDestination = false
			}
			circuits = append(circuits, found...)
		}
		return circuits, everyDestination && len(t.destinations) > 0

	case *CircuitBreaker:
		return []CircuitReporter{t}, true

	case *Retry:
		return discoverCircuits(t.exporter)

	case CircuitReporter:
		return []CircuitReporter{t}, true

	default:
		return nil, false
	}
}

// Start launches the workers. It is safe to call once; later calls are no-ops.
//
// ctx governs the workers' lifetime for cancellation, but it is not how you
// stop a Dispatcher: cancelling it abandons whatever is in the buffer.
// Shutdown drains first (ADR-0015).
func (d *Dispatcher) Start(ctx context.Context) {
	d.startOnce.Do(func() {
		d.mu.Lock()
		d.started = true
		d.mu.Unlock()

		d.wg.Add(d.workers)
		for range d.workers {
			go func() {
				defer d.wg.Done()
				d.work(ctx)
			}()
		}
	})
}

// work is one worker's loop: dequeue, export, account.
func (d *Dispatcher) work(ctx context.Context) {
	for {
		batch, err := d.buffer.DequeueBatch(ctx)
		switch {
		case errors.Is(err, ErrBufferClosed):
			// Drained. Nothing further will arrive.
			return
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			// The store failed in a way its contract does not describe.
			// Looping on it would spin a core and hide the fault; stopping
			// surfaces it through Shutdown.
			d.recordWorkerErr(err)
			return
		}

		if len(batch) > 0 {
			d.dispatch(ctx, batch)
		}
	}
}

// dispatch exports one batch and accounts for what happened to it.
func (d *Dispatcher) dispatch(ctx context.Context, batch []LogRecord) {
	err := d.exporter.Export(ctx, batch)
	if err == nil {
		return
	}

	// A batch that reached at least one destination is not lost. Delivery is
	// at-least-once (ADR-0009), and re-sending it to satisfy the destination
	// that failed would duplicate it at the one that did not — the
	// amplification ADR-0013 forbids.
	var fe *FanOutError
	if errors.As(err, &fe) && fe.Partial() {
		return
	}

	// It reached nowhere. Those records are gone, and a discard that is not
	// counted is the one defect this pipeline does not ship.
	d.countLost(batch, DropBackendUnavailable)
}

// countLost attributes a lost batch to the sources that produced it.
//
// Per source rather than per batch, because a batch is a transport detail: the
// operator's question is which of their services lost telemetry, and a single
// number cannot answer it.
func (d *Dispatcher) countLost(batch []LogRecord, reason DropReason) {
	bySource := make(map[string]int, 4)
	for i := range batch {
		bySource[batch[i].Resource.ServiceName]++
	}
	for source, n := range bySource {
		d.metrics.RecordsDropped(source, reason, n)
	}
}

func (d *Dispatcher) recordWorkerErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workerErr == nil {
		d.workerErr = err
	}
}

// Degraded reports whether every destination is currently refusing calls —
// the state ADR-0015 requires readiness to reflect, so an orchestrator takes
// the instance out of service instead of feeding it records it cannot export.
//
// It is false when any destination might still accept a batch, and false when
// no destination is guarded by a breaker at all: an unguarded destination has
// never told anyone it is unusable, and guessing that it is would take a
// healthy instance out of service.
//
// Calling it is how the degraded metric stays current, so a readiness probe
// polling it is doing double duty.
func (d *Dispatcher) Degraded() bool {
	degraded := d.guarded && len(d.circuits) > 0
	if degraded {
		for _, c := range d.circuits {
			if !c.Open() {
				degraded = false
				break
			}
		}
	}

	d.mu.Lock()
	changed := !d.degradedKnown || d.degraded != degraded
	d.degraded, d.degradedKnown = degraded, true
	d.mu.Unlock()

	if changed {
		d.metrics.ExportDegraded(degraded)
	}
	return degraded
}

// OpenCircuits names the destinations currently refusing calls, so an operator
// reading a readiness failure is told which one to look at.
func (d *Dispatcher) OpenCircuits() []string {
	var open []string
	for _, c := range d.circuits {
		if c.Open() {
			open = append(open, c.Name())
		}
	}
	return open
}

// Shutdown stops admission, drains what is buffered, and releases the
// exporters (FR10, ADR-0015).
//
// ctx bounds the drain. Records still unexported when it expires are counted
// as DropShutdownTimeout and lost: bounding shutdown is a hard requirement in
// any orchestrated environment, so loss at shutdown is permitted — silent loss
// is not.
//
// The context passed to Start must stay alive for the duration, or the workers
// stop before they have drained.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	var errs []error
	d.closeOnce.Do(func() {
		if err := d.buffer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the buffer: %w", err))
		}
	})

	if err := d.waitForDrain(ctx); err != nil {
		errs = append(errs, err)
	}

	if err := d.exporter.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutting down exporters: %w", err))
	}

	d.mu.Lock()
	workerErr := d.workerErr
	d.mu.Unlock()
	if workerErr != nil {
		errs = append(errs, fmt.Errorf("export worker stopped early: %w", workerErr))
	}

	return errors.Join(errs...)
}

// waitForDrain waits for the workers to finish, or counts what they did not
// reach when ctx expires first.
func (d *Dispatcher) waitForDrain(ctx context.Context) error {
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if !started {
		// Nothing is draining the buffer, so whatever is in it stays there.
		// Counting it as lost would be a guess about a dispatcher that was
		// never running.
		return nil
	}

	drained := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		lost := d.buffer.Depth()
		if lost > 0 {
			// Attributed to no source: grouping them would mean dequeuing the
			// very records we have just run out of time to handle. The count
			// is what ADR-0015 requires — the per-exporter summary belongs to
			// the shutdown path that owns the final report.
			d.metrics.RecordsDropped("", DropShutdownTimeout, lost)
		}
		return fmt.Errorf("drain timed out with %d records still buffered: %w", lost, ctx.Err())
	}
}
