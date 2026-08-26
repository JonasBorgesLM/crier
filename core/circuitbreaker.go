package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCircuitOpen means the breaker rejected the batch without calling the
// destination, because recent calls failed and the cooldown has not elapsed.
//
// It is a distinct sentinel because the caller's response differs: a batch
// refused by an open circuit never touched the network, so it costs nothing to
// have tried, and retrying it in a loop only spends the export deadline
// (ADR-0013).
var ErrCircuitOpen = errors.New("circuit open")

// Circuit breaker defaults.
const (
	// DefaultFailureThreshold is how many consecutive failures open a circuit.
	DefaultFailureThreshold = 5
	// DefaultCooldown is how long a circuit stays open before a probe.
	DefaultCooldown = 30 * time.Second
	// DefaultHalfOpenSuccesses is how many probes must succeed to close it.
	DefaultHalfOpenSuccesses = 1
)

// CircuitState is a breaker's current state.
type CircuitState int

const (
	// CircuitClosed passes every call through. The healthy state.
	CircuitClosed CircuitState = iota
	// CircuitOpen rejects every call with ErrCircuitOpen.
	CircuitOpen
	// CircuitHalfOpen lets one probe through to find out whether the
	// destination has recovered.
	CircuitHalfOpen
)

// String implements fmt.Stringer.
func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return fmt.Sprintf("CircuitState(%d)", int(s))
	}
}

// CircuitBreakerConfig configures a CircuitBreaker. Build one with
// NewCircuitBreaker, which validates eagerly (NFR4).
type CircuitBreakerConfig struct {
	// Name labels this destination's circuit metric. Required, and should
	// match the fan-out Destination name.
	Name string

	// Exporter is the destination being guarded.
	Exporter Exporter

	// FailureThreshold is how many consecutive failures open the circuit.
	// Zero means DefaultFailureThreshold.
	FailureThreshold int

	// Cooldown is how long the circuit stays open before admitting a probe.
	// Zero means DefaultCooldown.
	Cooldown time.Duration

	// HalfOpenSuccesses is how many consecutive probe successes close the
	// circuit again. Zero means DefaultHalfOpenSuccesses.
	HalfOpenSuccesses int

	// Metrics receives circuit transitions. Nil discards.
	Metrics Metrics

	// Now supplies the current time. Nil means time.Now; tests override it so
	// the cooldown can be asserted without waiting for it.
	Now func() time.Time
}

// CircuitBreaker stops sending to a destination that keeps failing, so an
// unhealthy backend stops consuming dispatch workers that healthy ones need
// (FR6, ADR-0013).
//
// It is composed per destination, innermost — FanOut(Retry(CircuitBreaker(e))).
// One breaker shared across destinations would let one broken backend silence
// the others, which is the coupling this whole layering exists to remove.
//
// When every destination's breaker is open the pipeline is in the degraded
// state ADR-0015 surfaces through readiness; Open reports this breaker's part
// of that.
//
// Safe for concurrent use.
type CircuitBreaker struct {
	name              string
	exporter          Exporter
	failureThreshold  int
	cooldown          time.Duration
	halfOpenSuccesses int
	metrics           Metrics
	now               func() time.Time

	mu        sync.Mutex
	state     CircuitState
	failures  int
	successes int
	openedAt  time.Time
	// probing is true while a half-open probe is in flight, so a burst of
	// batches does not send the whole burst at a destination that has given
	// no sign of recovery yet.
	probing bool
}

var (
	_ Exporter     = (*CircuitBreaker)(nil)
	_ BatchMutator = (*CircuitBreaker)(nil)
)

// NewCircuitBreaker validates cfg and returns the decorator.
func NewCircuitBreaker(cfg CircuitBreakerConfig) (*CircuitBreaker, error) {
	if cfg.Exporter == nil {
		return nil, errors.New("circuit breaker needs an exporter to wrap")
	}
	if cfg.Name == "" {
		return nil, errors.New("circuit breaker needs a Name; it labels the circuit metric")
	}
	if cfg.FailureThreshold < 0 {
		return nil, fmt.Errorf("FailureThreshold is %d, want at least 1", cfg.FailureThreshold)
	}
	if cfg.HalfOpenSuccesses < 0 {
		return nil, fmt.Errorf("HalfOpenSuccesses is %d, want at least 1", cfg.HalfOpenSuccesses)
	}
	if cfg.Cooldown < 0 {
		return nil, fmt.Errorf("negative Cooldown %v, want a positive duration", cfg.Cooldown)
	}

	cb := &CircuitBreaker{
		name:              cfg.Name,
		exporter:          cfg.Exporter,
		failureThreshold:  cfg.FailureThreshold,
		cooldown:          cfg.Cooldown,
		halfOpenSuccesses: cfg.HalfOpenSuccesses,
		metrics:           cfg.Metrics,
		now:               cfg.Now,
	}
	if cb.failureThreshold == 0 {
		cb.failureThreshold = DefaultFailureThreshold
	}
	if cb.cooldown == 0 {
		cb.cooldown = DefaultCooldown
	}
	if cb.halfOpenSuccesses == 0 {
		cb.halfOpenSuccesses = DefaultHalfOpenSuccesses
	}
	if cb.metrics == nil {
		cb.metrics = NopMetrics{}
	}
	if cb.now == nil {
		cb.now = time.Now
	}
	return cb, nil
}

// Name returns the label this breaker's metrics carry.
func (cb *CircuitBreaker) Name() string { return cb.name }

// State reports the current state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Open reports whether the breaker is currently refusing calls. A half-open
// breaker is not open: it is admitting probes, so the destination is not yet
// written off.
func (cb *CircuitBreaker) Open() bool { return cb.State() == CircuitOpen }

// Export sends batch unless the circuit is open.
func (cb *CircuitBreaker) Export(ctx context.Context, batch []LogRecord) error {
	probe, err := cb.admit()
	if err != nil {
		return err
	}

	exportErr := cb.exporter.Export(ctx, batch)

	switch {
	case exportErr == nil:
		cb.succeeded(probe)
		return nil
	case errors.Is(ctx.Err(), context.Canceled):
		// Our own shutdown or a cancelled request aborted the call. That is
		// crier's state, not the destination's health, and counting it would
		// open every circuit on the way out of the door (ADR-0016).
		//
		// A deadline is different and is counted: FanOut's per-destination
		// timeout exists precisely to turn a destination that hangs into a
		// failure a breaker can see.
		cb.abandoned(probe)
	default:
		cb.failed(probe)
	}
	return exportErr
}

// admit decides whether this call may proceed, and reports whether it is the
// half-open probe.
func (cb *CircuitBreaker) admit() (probe bool, err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return false, nil

	case CircuitOpen:
		if cb.now().Sub(cb.openedAt) < cb.cooldown {
			return false, fmt.Errorf("%w: %s", ErrCircuitOpen, cb.name)
		}
		cb.state = CircuitHalfOpen
		cb.successes = 0
		cb.probing = true
		return true, nil

	case CircuitHalfOpen:
		if cb.probing {
			// A probe is already deciding this. Sending the rest of the burst
			// at a destination that has shown no sign of life is how a
			// recovering backend gets knocked over again.
			return false, fmt.Errorf("%w: %s (probing)", ErrCircuitOpen, cb.name)
		}
		cb.probing = true
		return true, nil

	default:
		return false, fmt.Errorf("circuit breaker %s is in unknown state %v", cb.name, cb.state)
	}
}

func (cb *CircuitBreaker) succeeded(probe bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if probe {
		cb.probing = false
		cb.successes++
		if cb.successes < cb.halfOpenSuccesses {
			return
		}
		cb.transition(CircuitClosed)
		return
	}
	cb.failures = 0
}

func (cb *CircuitBreaker) failed(probe bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if probe {
		// The destination is still unhealthy; serve another full cooldown
		// rather than probing again immediately.
		cb.probing = false
		cb.openedAt = cb.now()
		cb.transition(CircuitOpen)
		return
	}

	cb.failures++
	if cb.state == CircuitClosed && cb.failures >= cb.failureThreshold {
		cb.openedAt = cb.now()
		cb.transition(CircuitOpen)
	}
}

// abandoned releases a probe slot without counting the call either way.
func (cb *CircuitBreaker) abandoned(probe bool) {
	if !probe {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.probing = false
}

// transition moves to state and reports it. The caller holds cb.mu.
//
// The metric fires on transitions only. A gauge that is re-set on every batch
// says nothing an operator can alert on; a transition is an event.
func (cb *CircuitBreaker) transition(state CircuitState) {
	if cb.state == state {
		return
	}
	cb.state = state
	switch state {
	case CircuitClosed:
		cb.failures = 0
		cb.successes = 0
		cb.metrics.CircuitStateChanged(cb.name, false)
	case CircuitOpen:
		cb.failures = 0
		cb.successes = 0
		cb.metrics.CircuitStateChanged(cb.name, true)
	case CircuitHalfOpen:
		// Not reported: half-open is a step inside recovery, and an operator
		// watching transitions wants "it broke" and "it came back", not the
		// probe in between.
	}
}

// MutatesBatch forwards the wrapped exporter's declaration (ADR-0016).
func (cb *CircuitBreaker) MutatesBatch() bool { return mutatesBatch(cb.exporter) }

// Shutdown releases the wrapped exporter.
func (cb *CircuitBreaker) Shutdown(ctx context.Context) error { return cb.exporter.Shutdown(ctx) }
