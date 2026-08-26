package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Options configures an embedded engine. Build one with New, which validates
// eagerly (NFR4).
//
// Everything has a working default except Exporters and ServiceName, because
// an engine with no destination and no identity is not a thing anyone meant to
// build.
type Options struct {
	// ServiceName identifies the host application. It becomes the records'
	// resource identity and the key for fair share, filtering and metrics.
	//
	// Required. In embedded mode there is no authenticated principal to derive
	// it from — the host application is the trust boundary (FR11) — so it is
	// asserted here once rather than per record.
	ServiceName string

	// ServiceVersion is optional, and worth setting: it is what makes a
	// deployment distinguishable in the backend.
	ServiceVersion string

	// Exporters are the destinations, by name. At least one is required.
	//
	// Each is wrapped in its own circuit breaker and its own retry, inside the
	// fan-out — the composition ADR-0013 requires. Assembling it here rather
	// than asking the host to is the point: built the other way round, one
	// failing destination re-sends the batch to every healthy one, and that
	// mistake is invisible until a backend goes down.
	Exporters map[string]Exporter

	// Capacity bounds the buffer. Zero means DefaultBufferCapacity.
	Capacity int
	// BatchSize and BatchWindow control how records are grouped for export.
	// Zero means the buffer's defaults.
	BatchSize   int
	BatchWindow time.Duration
	// DropPolicy is what happens to a record when the buffer is full. The
	// zero value rejects, which is the default a caller has to opt out of.
	DropPolicy DropPolicy

	// Limits caps record size (ADR-0010). The zero value applies defaults.
	//
	// They apply here exactly as they do to the HTTP receiver, and for the
	// same reason: a bug in the host application produces the same unbounded
	// attribute map as a malicious client, and the buffer cannot tell them
	// apart.
	Limits Limits

	// Cardinality guards attribute value cardinality. Nil disables the guard.
	Cardinality *CardinalityGuard

	// Redactor masks sensitive data. Nil disables redaction, which is the
	// explicit, auditable choice ADR-0014 requires an operator to make.
	Redactor *Redactor

	// Filter applies the severity threshold and sampler. Nil keeps everything.
	Filter *Filter

	// Workers bounds how many batches are exported at once. Zero means
	// DefaultExportWorkers.
	Workers int

	// ExportTimeout bounds one destination's export, retry backoff included.
	// Zero means DefaultExportTimeout.
	ExportTimeout time.Duration

	// RetryAttempts bounds attempts per destination, the first included. Zero
	// means DefaultRetryAttempts; one disables retrying without removing the
	// decorator.
	RetryAttempts int

	// FailureThreshold is how many consecutive failures open a destination's
	// circuit. Zero means DefaultFailureThreshold.
	FailureThreshold int

	// Cooldown is how long an open circuit waits before probing. Zero means
	// DefaultCooldown.
	Cooldown time.Duration

	// Metrics receives every counter. Nil discards, which is the right default
	// for a host that has not wired metrics up yet — the engine must never
	// require them to run.
	Metrics Metrics
}

// Crier is the engine embedded in a host application (FR9): the same pipeline,
// buffer and export layer the daemon runs, with no receiver.
//
// There is no receiver because in embedded mode there is nothing to receive
// from — the host calls Log directly, and owns the trust boundary itself.
//
// Safe for concurrent use.
type Crier struct {
	pipeline   *Pipeline
	dispatcher *Dispatcher
	health     *Health
	buffer     BufferStore
	source     string
	version    string
}

// New assembles the engine and starts exporting.
func New(opts Options) (*Crier, error) {
	if opts.ServiceName == "" {
		return nil, errors.New("ServiceName is required: it is the records' identity, and in embedded mode there is no principal to derive it from")
	}
	if len(opts.Exporters) == 0 {
		return nil, errors.New("at least one exporter is required; an engine with no destination accepts records and discards them")
	}

	metrics := opts.Metrics
	if metrics == nil {
		metrics = NopMetrics{}
	}

	buffer, err := NewMemoryBuffer(MemoryBufferConfig{
		Capacity:    opts.Capacity,
		BatchSize:   opts.BatchSize,
		BatchWindow: opts.BatchWindow,
		Policy:      opts.DropPolicy,
		Metrics:     metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}

	pipeline, err := NewPipeline(PipelineConfig{
		Buffer:      buffer,
		Limits:      opts.Limits,
		Cardinality: opts.Cardinality,
		Redactor:    opts.Redactor,
		Filter:      opts.Filter,
		Metrics:     metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}

	fanOut, err := buildFanOut(opts, metrics)
	if err != nil {
		return nil, err
	}

	dispatcher, err := NewDispatcher(DispatcherConfig{
		Buffer:   buffer,
		Exporter: fanOut,
		Workers:  opts.Workers,
		Metrics:  metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	health, err := NewHealth(dispatcher)
	if err != nil {
		return nil, err
	}

	dispatcher.Start(context.Background())

	return &Crier{
		pipeline:   pipeline,
		dispatcher: dispatcher,
		health:     health,
		buffer:     buffer,
		source:     opts.ServiceName,
		version:    opts.ServiceVersion,
	}, nil
}

// buildFanOut composes each destination as FanOut(Retry(CircuitBreaker(e))).
func buildFanOut(opts Options, metrics Metrics) (*FanOut, error) {
	// Sorted, so the destination order is the same on every run. It does not
	// affect behaviour — fan-out dispatches concurrently — but it makes logs
	// and config dumps comparable between restarts.
	names := make([]string, 0, len(opts.Exporters))
	for name := range opts.Exporters {
		names = append(names, name)
	}
	sort.Strings(names)

	destinations := make([]Destination, 0, len(names))
	for _, name := range names {
		breaker, err := NewCircuitBreaker(CircuitBreakerConfig{
			Name:             name,
			Exporter:         opts.Exporters[name],
			FailureThreshold: opts.FailureThreshold,
			Cooldown:         opts.Cooldown,
			Metrics:          metrics,
		})
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", name, err)
		}
		retry, err := NewRetry(RetryConfig{
			Name:        name,
			Exporter:    breaker,
			MaxAttempts: opts.RetryAttempts,
			Metrics:     metrics,
		})
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", name, err)
		}
		destinations = append(destinations, Destination{Name: name, Exporter: retry})
	}

	return NewFanOut(FanOutConfig{
		Destinations: destinations,
		Timeout:      opts.ExportTimeout,
		Metrics:      metrics,
	})
}

// Log runs one record through the pipeline and admits it.
//
// It returns when the record is buffered, not when it is exported: export
// happens on the dispatcher's own workers, so a host application's latency
// never depends on whether a backend is healthy (ADR-0001, ADR-0009).
//
// A filtered record returns nil. It was handled exactly as configured.
func (c *Crier) Log(ctx context.Context, rec LogRecord) error {
	if rec.Resource.ServiceVersion == "" {
		rec.Resource.ServiceVersion = c.version
	}
	return c.pipeline.Admit(ctx, rec, c.source)
}

// LogBatch admits several records, returning how many were accepted and the
// first error.
//
// It does not stop at the first failure: one record hitting a limit says
// nothing about the next.
func (c *Crier) LogBatch(ctx context.Context, batch []LogRecord) (accepted int, err error) {
	for i := range batch {
		if batch[i].Resource.ServiceVersion == "" {
			batch[i].Resource.ServiceVersion = c.version
		}
	}
	return c.pipeline.AdmitBatch(ctx, batch, c.source)
}

// Health reports liveness and readiness, for a host that exposes its own
// health endpoints and wants crier's state in them.
func (c *Crier) Health() *Health { return c.health }

// Depth reports how many records are buffered and not yet exported.
func (c *Crier) Depth() int { return c.buffer.Depth() }

// Shutdown stops accepting records, drains what is buffered within ctx, and
// releases the exporters.
//
// The summary is returned rather than logged, because this package has no
// logger and should not acquire one: what the host does with the number of
// records lost is the host's decision. Ignoring it is also a decision, and one
// the return value at least makes visible (ADR-0015).
func (c *Crier) Shutdown(ctx context.Context) (DrainSummary, error) {
	return c.dispatcher.Shutdown(ctx)
}
