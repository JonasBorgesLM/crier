package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Retry defaults. Four attempts over roughly a second of backoff is enough to
// ride out a collector restart or a load-balancer reconvergence, and short
// enough that a batch does not sit on a dispatch worker while a destination is
// genuinely down — that case belongs to the circuit breaker, not to retry.
const (
	// DefaultRetryAttempts is the total number of attempts, first included.
	DefaultRetryAttempts = 4
	// DefaultInitialBackoff is the base of the exponential backoff.
	DefaultInitialBackoff = 100 * time.Millisecond
	// DefaultMaxBackoff caps one backoff interval.
	DefaultMaxBackoff = 5 * time.Second
)

// RetryHint is implemented by an error that knows how long the caller should
// wait before trying again — an HTTP 429 or 503 carrying Retry-After
// (ADR-0017).
//
// Retry waits for the longer of its own backoff and the hint. Ignoring a
// destination that has just told us how long it needs is how a rate limit
// turns into an outage: the sender keeps arriving early, keeps being refused,
// and spends its whole budget doing it.
//
// The hint is bounded by the export deadline, not obeyed unconditionally
// (ADR-0016). A destination asking for an hour gets the deadline, and the
// batch fails rather than parking a dispatch worker for an hour.
type RetryHint interface {
	error

	// RetryAfter is how long the destination asked us to wait.
	RetryAfter() time.Duration
}

// RetryConfig configures a Retry. Build one with NewRetry, which validates
// eagerly (NFR4).
type RetryConfig struct {
	// Name labels this destination's retry counter. Required, and should
	// match the fan-out Destination name, or the retry count and the export
	// count for one destination land under different labels.
	Name string

	// Exporter is what gets retried — the circuit breaker, which wraps the
	// real exporter (ADR-0013).
	Exporter Exporter

	// MaxAttempts bounds attempts, the first included. Zero means
	// DefaultRetryAttempts. One disables retrying without removing the
	// decorator, which is the honest way to turn it off.
	MaxAttempts int

	// InitialBackoff is the first interval; it doubles per attempt. Zero means
	// DefaultInitialBackoff.
	InitialBackoff time.Duration

	// MaxBackoff caps one interval. Zero means DefaultMaxBackoff.
	MaxBackoff time.Duration

	// Metrics receives retry counts. Nil discards.
	Metrics Metrics

	// Rand returns a value in [0,1), used for jitter. Nil means math/rand/v2.
	Rand func() float64
}

// Retry re-sends a failed batch to one destination, with bounded attempts and
// exponential backoff (ADR-0013, FR6).
//
// It is composed *inside* the fan-out — FanOut(Retry(CircuitBreaker(e))) — so
// it only ever re-sends its own destination's batch. Wrapped the other way
// round, one destination's failure re-sends the batch to every healthy
// destination as well, once per attempt. That is duplicate amplification,
// audit finding A-1.
//
// Safe for concurrent use.
type Retry struct {
	name           string
	exporter       Exporter
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	metrics        Metrics
	random         func() float64

	// sleep is a test seam, so the backoff schedule can be asserted without
	// the tests waiting for it. Nil sleeps for real.
	sleep func(ctx context.Context, d time.Duration) error
}

var (
	_ Exporter     = (*Retry)(nil)
	_ BatchMutator = (*Retry)(nil)
)

// NewRetry validates cfg and returns the decorator.
func NewRetry(cfg RetryConfig) (*Retry, error) {
	if cfg.Exporter == nil {
		return nil, errors.New("retry needs an exporter to wrap")
	}
	if cfg.Name == "" {
		return nil, errors.New("retry needs a Name; it labels the retry counter")
	}
	if cfg.MaxAttempts < 0 {
		return nil, fmt.Errorf("MaxAttempts is %d, want at least 1", cfg.MaxAttempts)
	}
	if cfg.InitialBackoff < 0 || cfg.MaxBackoff < 0 {
		return nil, fmt.Errorf("backoff is negative: initial %v, max %v", cfg.InitialBackoff, cfg.MaxBackoff)
	}

	r := &Retry{
		name:           cfg.Name,
		exporter:       cfg.Exporter,
		maxAttempts:    cfg.MaxAttempts,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		metrics:        cfg.Metrics,
		random:         cfg.Rand,
	}
	if r.maxAttempts == 0 {
		r.maxAttempts = DefaultRetryAttempts
	}
	if r.initialBackoff == 0 {
		r.initialBackoff = DefaultInitialBackoff
	}
	if r.maxBackoff == 0 {
		r.maxBackoff = DefaultMaxBackoff
	}
	if r.initialBackoff > r.maxBackoff {
		return nil, fmt.Errorf("InitialBackoff %v is longer than MaxBackoff %v", r.initialBackoff, r.maxBackoff)
	}
	if r.metrics == nil {
		r.metrics = NopMetrics{}
	}
	if r.random == nil {
		// Jitter spreads a retry herd; it is not a security decision.
		r.random = rand.Float64
	}
	return r, nil
}

// Export sends batch, retrying only failures that retrying could fix.
//
// It gives up immediately on:
//
//   - a permanent failure (ErrPermanent) — a malformed payload or a rejected
//     credential is not going to be accepted on the third attempt, and the
//     budget spent on it delays every batch queued behind this one;
//   - an open circuit (ErrCircuitOpen) — the breaker below has already
//     decided this destination is unhealthy, and asking it again in a loop
//     turns a fail-fast into a stall that spends the whole export deadline;
//   - a cancelled or expired context — nobody is waiting for the answer.
func (r *Retry) Export(ctx context.Context, batch []LogRecord) error {
	for attempt := 1; ; attempt++ {
		err := r.exporter.Export(ctx, batch)
		switch {
		case err == nil:
			return nil
		case IsPermanent(err), errors.Is(err, ErrCircuitOpen), ctx.Err() != nil:
			return err
		case attempt >= r.maxAttempts:
			return fmt.Errorf("gave up after %d attempts: %w", attempt, err)
		}

		if waitErr := r.wait(ctx, attempt, err); waitErr != nil {
			// Both errors matter: the export failure says why we were
			// retrying, the wait error says why we stopped.
			return fmt.Errorf("retry abandoned during backoff after %d attempts: %w",
				attempt, errors.Join(err, waitErr))
		}
		r.metrics.ExportRetried(r.name)
	}
}

// backoff returns the interval to wait before the attempt after this one.
//
// It is exponential with full jitter — a uniform draw from [0, ceiling) rather
// than the ceiling itself. Jittering the whole interval rather than adding a
// small perturbation is what actually spreads a thundering herd: after a
// collector restart, every sender is failing on the same schedule, and equal
// backoffs reconverge them onto the same retry instant.
func (r *Retry) backoff(attempt int) time.Duration {
	ceiling := r.initialBackoff
	for range attempt - 1 {
		if ceiling >= r.maxBackoff/2 {
			ceiling = r.maxBackoff
			break
		}
		ceiling *= 2
	}
	return time.Duration(r.random() * float64(ceiling))
}

// wait sleeps for the backoff interval, or returns early if ctx is done.
//
// cause is the failure being retried, consulted for a Retry-After hint.
func (r *Retry) wait(ctx context.Context, attempt int, cause error) error {
	d := r.backoff(attempt)
	if hint := retryAfter(cause); hint > d {
		d = hint
	}
	if r.sleep != nil {
		return r.sleep(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryAfter returns the delay a failure asked for, or zero if it named none.
func retryAfter(err error) time.Duration {
	var hint RetryHint
	if errors.As(err, &hint) {
		return hint.RetryAfter()
	}
	return 0
}

// MutatesBatch forwards the wrapped exporter's declaration, so a leaf that
// writes to its batch is not hidden behind the decorator chain (ADR-0016).
func (r *Retry) MutatesBatch() bool { return mutatesBatch(r.exporter) }

// Shutdown releases the wrapped exporter.
func (r *Retry) Shutdown(ctx context.Context) error { return r.exporter.Shutdown(ctx) }
