package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultExportTimeout bounds one destination's Export call when FanOut is
// given no Timeout of its own.
const DefaultExportTimeout = 30 * time.Second

// Destination pairs an exporter with the label its counters carry.
//
// The name is required rather than derived from the exporter's type, because
// two destinations are frequently the same type — two OTLP collectors, one per
// region — and counters that both report as "otlp" hide exactly the failure an
// operator is looking for.
type Destination struct {
	// Name labels this destination's metrics. Required, and unique within a
	// fan-out.
	Name string

	// Exporter is the composed chain for this destination, retry and circuit
	// breaking included: FanOut(Retry(CircuitBreaker(e))), never the reverse
	// (ADR-0013).
	Exporter Exporter
}

// BatchMutator is implemented by an exporter that writes to the batch it is
// given. FanOut hands such an exporter its own clone and leaves every other
// exporter sharing the original.
//
// The default is therefore that a batch is read-only for the duration of
// Export, which is what makes concurrent dispatch safe without copying
// (ADR-0016). Cloning for everyone would put a deep copy of every batch on the
// hot path to defend against something almost no exporter does; cloning for
// nobody would make one badly behaved exporter corrupt its siblings' data in a
// way that shows up as a data race under load and nowhere else.
//
// A decorator must forward this from the exporter it wraps, or a leaf's
// declaration becomes invisible the moment it is composed.
type BatchMutator interface {
	Exporter

	// MutatesBatch reports whether Export modifies batch or its records.
	MutatesBatch() bool
}

// mutatesBatch reports whether e needs its own copy of the batch.
func mutatesBatch(e Exporter) bool {
	m, ok := e.(BatchMutator)
	return ok && m.MutatesBatch()
}

// cloneBatch deep-copies a batch, so a mutating exporter cannot reach its
// siblings' records.
func cloneBatch(batch []LogRecord) []LogRecord {
	out := make([]LogRecord, len(batch))
	for i := range batch {
		out[i] = batch[i].Clone()
	}
	return out
}

// FanOutConfig configures a FanOut. Build one with NewFanOut, which validates
// eagerly (NFR4).
type FanOutConfig struct {
	// Destinations receive every batch. At least one is required.
	Destinations []Destination

	// Timeout bounds each destination's Export call. Zero means
	// DefaultExportTimeout.
	//
	// It is a ceiling on the whole composed chain, retry backoff included,
	// because that is the point: a destination that accepts a connection and
	// then never answers otherwise holds a dispatch worker forever, and no
	// circuit breaker helps — a call that never returns never reports the
	// failure a breaker would have counted (ADR-0016). Set it above the retry
	// budget, or retries are cut off by it.
	Timeout time.Duration

	// Metrics receives per-destination export counters. Nil discards.
	Metrics Metrics
}

// FanOut sends every batch to every destination, concurrently, and joins the
// results (ADR-0013, ADR-0016).
//
// It performs no retry of its own. Retry and circuit breaking are per
// destination, composed inside it:
//
//	NewFanOut(FanOutConfig{Destinations: []Destination{
//	    {Name: "primary", Exporter: NewRetry(RetryConfig{Exporter: NewCircuitBreaker(...)})},
//	    {Name: "archive", Exporter: NewRetry(RetryConfig{Exporter: NewCircuitBreaker(...)})},
//	}})
//
// Composed the other way round — a retry wrapping the fan-out — a failure at
// one destination re-sends the whole batch, so a healthy destination receives
// it once per attempt because an unrelated one is broken. That is audit
// finding A-1 and ADR-0013 exists to forbid it.
//
// Safe for concurrent use.
type FanOut struct {
	destinations []Destination
	timeout      time.Duration
	metrics      Metrics
}

var _ Exporter = (*FanOut)(nil)

// NewFanOut validates cfg and returns the fan-out.
func NewFanOut(cfg FanOutConfig) (*FanOut, error) {
	if len(cfg.Destinations) == 0 {
		// A fan-out with no destinations accepts every batch and discards it,
		// reporting success. That is a silent drop with a clean bill of
		// health, which is the one thing this project does not ship.
		return nil, errors.New("fan-out needs at least one destination")
	}
	seen := make(map[string]struct{}, len(cfg.Destinations))
	for i, d := range cfg.Destinations {
		if d.Name == "" {
			return nil, fmt.Errorf("destination %d has no name", i)
		}
		if d.Exporter == nil {
			return nil, fmt.Errorf("destination %q has no exporter", d.Name)
		}
		if _, dup := seen[d.Name]; dup {
			return nil, fmt.Errorf("destination %q is declared twice; names label metrics and must be unique", d.Name)
		}
		seen[d.Name] = struct{}{}
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("negative Timeout %v, want a positive duration", cfg.Timeout)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultExportTimeout
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NopMetrics{}
	}

	return &FanOut{
		destinations: append([]Destination(nil), cfg.Destinations...),
		timeout:      timeout,
		metrics:      metrics,
	}, nil
}

// Destinations reports how many destinations the fan-out dispatches to.
func (f *FanOut) Destinations() int { return len(f.destinations) }

// Names returns the destination names, in configuration order.
func (f *FanOut) Names() []string {
	out := make([]string, len(f.destinations))
	for i, d := range f.destinations {
		out[i] = d.Name
	}
	return out
}

// Export dispatches batch to every destination at once and waits for all of
// them.
//
// It returns nil only when every destination accepted the batch. Otherwise it
// returns a *FanOutError naming the ones that did not, which the caller
// inspects to tell a partial failure — the batch reached somewhere, so nothing
// is lost — from a total one, where the records are gone and must be counted
// (ADR-0015).
func (f *FanOut) Export(ctx context.Context, batch []LogRecord) error {
	if len(batch) == 0 {
		return nil
	}

	failures := make([]error, len(f.destinations))

	if len(f.destinations) == 1 {
		// One destination needs no goroutine, and this is the common shape.
		failures[0] = f.export(ctx, f.destinations[0], batch)
	} else {
		var wg sync.WaitGroup
		wg.Add(len(f.destinations))
		for i, d := range f.destinations {
			go func() {
				defer wg.Done()
				failures[i] = f.export(ctx, d, batch)
			}()
		}
		wg.Wait()
	}

	var fe *FanOutError
	for i, err := range failures {
		if err == nil {
			continue
		}
		if fe == nil {
			fe = &FanOutError{Dispatched: len(f.destinations), Failures: make(map[string]error, len(failures))}
		}
		fe.Failures[f.destinations[i].Name] = err
	}
	if fe == nil {
		return nil
	}
	return fe
}

// export sends the batch to one destination under its own deadline.
func (f *FanOut) export(ctx context.Context, d Destination, batch []LogRecord) error {
	if mutatesBatch(d.Exporter) {
		batch = cloneBatch(batch)
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	start := time.Now()
	err := d.Exporter.Export(ctx, batch)
	// Observed on failure too: a destination that takes 30 seconds to reject a
	// batch is a problem whether or not it eventually accepts one.
	f.metrics.ExportLatency(d.Name, time.Since(start))

	if err != nil {
		return err
	}
	f.metrics.RecordsExported(d.Name, len(batch))
	return nil
}

// Shutdown releases every destination's resources, concurrently, and joins the
// errors. One destination that hangs must not consume another's share of the
// shutdown deadline.
func (f *FanOut) Shutdown(ctx context.Context) error {
	errs := make([]error, len(f.destinations))
	var wg sync.WaitGroup
	wg.Add(len(f.destinations))
	for i, d := range f.destinations {
		go func() {
			defer wg.Done()
			if err := d.Exporter.Shutdown(ctx); err != nil {
				errs[i] = fmt.Errorf("%s: %w", d.Name, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// FanOutError reports which destinations rejected a batch.
//
// It names them rather than joining anonymous errors because the distinction
// between "one of three destinations is down" and "all three are down" is the
// difference between a warning and the degraded state that takes the instance
// out of service (ADR-0015), and a joined error string cannot be asked which
// one it is.
type FanOutError struct {
	// Dispatched is how many destinations the batch went to.
	Dispatched int
	// Failures holds one error per destination that did not accept it.
	Failures map[string]error
}

// Error implements error.
func (e *FanOutError) Error() string {
	names := make([]string, 0, len(e.Failures))
	for name := range e.Failures {
		names = append(names, name)
	}
	// Sorted so the message is stable across runs; map order is not.
	sort.Strings(names)

	var b strings.Builder
	fmt.Fprintf(&b, "export failed at %d of %d destinations: ", len(e.Failures), e.Dispatched)
	for i, name := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %v", name, e.Failures[name])
	}
	return b.String()
}

// Unwrap exposes the per-destination errors, so errors.Is and errors.As reach
// through to sentinels such as ErrPermanent and ErrCircuitOpen.
func (e *FanOutError) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, err := range e.Failures {
		out = append(out, err)
	}
	return out
}

// AllFailed reports whether no destination accepted the batch — the batch is
// lost, and its records must be counted as dropped rather than exported.
func (e *FanOutError) AllFailed() bool { return len(e.Failures) == e.Dispatched }

// Partial reports whether at least one destination accepted the batch. Nothing
// is lost in that case: delivery is at-least-once (ADR-0009), and re-sending
// to satisfy the failed destination would duplicate at the healthy one — which
// is the amplification ADR-0013 forbids.
func (e *FanOutError) Partial() bool { return !e.AllFailed() }
