package core

import (
	"sync"
	"time"
)

// Cardinality guard defaults (ADR-0010, FR12).
const (
	// DefaultMaxDistinctValues is how many distinct values one attribute key
	// may carry within a window before the key is capped.
	DefaultMaxDistinctValues = 1000
	// DefaultMaxTrackedKeys bounds how many keys the guard tracks at once.
	DefaultMaxTrackedKeys = 256
	// DefaultCardinalityWindow is how long observations stay relevant.
	DefaultCardinalityWindow = 10 * time.Minute
	// DefaultCardinalityMark replaces a value once its key is capped.
	DefaultCardinalityMark = "…[high cardinality]"
)

// CardinalityGuard replaces attribute values whose key has carried too many
// distinct values recently (ADR-0010).
//
// The problem it solves is not primarily an attack. Request IDs, user IDs, and
// timestamps used as attribute values explode the series count of essentially
// every observability backend, degrading it and inflating its bill, and they
// get there through ordinary carelessness far more often than through malice.
//
// # Its own state is bounded
//
// A guard that keeps a set of every value it has seen is the memory leak it
// exists to prevent. Three bounds apply:
//
//   - MaxTrackedKeys caps how many keys are tracked. Past it, new keys are not
//     tracked at all — they pass through unguarded rather than displacing a key
//     already known to be a problem.
//   - MaxDistinctValues caps the set per key. On reaching it the key is marked
//     capped and its value set is released: once a key is capped, knowing which
//     values it held buys nothing.
//   - Window ages observations out, in two generations. A key that was noisy an
//     hour ago and is quiet now recovers on its own.
//
// The zero value is usable and safe for concurrent use.
type CardinalityGuard struct {
	// MaxDistinctValues per key. Zero means DefaultMaxDistinctValues,
	// negative disables the guard.
	MaxDistinctValues int
	// MaxTrackedKeys bounds tracked keys. Zero means DefaultMaxTrackedKeys.
	MaxTrackedKeys int
	// Window is how long an observation counts for. Zero means
	// DefaultCardinalityWindow.
	Window time.Duration
	// Mark replaces a capped key's value. Zero means DefaultCardinalityMark.
	Mark string
	// Now supplies the clock. Nil means time.Now.
	Now func() time.Time
	// Metrics receives cap events. Nil discards.
	Metrics Metrics

	mu sync.Mutex
	// current and previous are the two generations of the rolling window.
	// Rotation drops previous wholesale, which is what keeps the memory bound
	// hard rather than best-effort.
	current, previous map[string]*keyState
	rotatedAt         time.Time
}

type keyState struct {
	values map[string]struct{}
	capped bool
}

func (g *CardinalityGuard) maxValues() int {
	if g.MaxDistinctValues == 0 {
		return DefaultMaxDistinctValues
	}
	return g.MaxDistinctValues
}

func (g *CardinalityGuard) maxKeys() int {
	if g.MaxTrackedKeys <= 0 {
		return DefaultMaxTrackedKeys
	}
	return g.MaxTrackedKeys
}

func (g *CardinalityGuard) window() time.Duration {
	if g.Window <= 0 {
		return DefaultCardinalityWindow
	}
	return g.Window
}

func (g *CardinalityGuard) mark() string {
	if g.Mark == "" {
		return DefaultCardinalityMark
	}
	return g.Mark
}

func (g *CardinalityGuard) metrics() Metrics {
	if g.Metrics != nil {
		return g.Metrics
	}
	return NopMetrics{}
}

func (g *CardinalityGuard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Apply guards the record's attributes and resource attributes in place.
func (g *CardinalityGuard) Apply(rec *LogRecord) {
	g.applyTo(rec.Attributes)
	g.applyTo(rec.Resource.Attributes)
}

func (g *CardinalityGuard) applyTo(attrs map[string]any) {
	if len(attrs) == 0 || g.maxValues() < 0 {
		return
	}
	for k, v := range attrs {
		s, ok := v.(string)
		if !ok {
			// Only strings drive cardinality in practice, and bounding
			// non-strings would mean stringifying every value on the hot path
			// to find out.
			continue
		}
		if g.Observe(k, s) {
			attrs[k] = g.mark()
		}
	}
}

// Observe records that key carried value and reports whether the key is now
// capped, meaning the caller should substitute a marker.
//
// It is exported so a receiver can guard values it handles outside a
// LogRecord, and so the behaviour is directly testable.
func (g *CardinalityGuard) Observe(key, value string) bool {
	maxValues := g.maxValues()
	if maxValues < 0 {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.rotateIfDue()

	state, tracked := g.current[key]
	if !tracked {
		// Carry a capped verdict across the generation boundary: a key that
		// was capped moments ago should not be re-learned from scratch.
		if prev, ok := g.previous[key]; ok && prev.capped {
			g.track(key, &keyState{capped: true})
			g.metrics().CardinalityCapped(key)
			return true
		}
		if len(g.current) >= g.maxKeys() {
			// At the key bound. New keys pass through unguarded rather than
			// evicting a key already known to be a problem — the alternative
			// lets an attacker launder a capped key back out by flooding new
			// ones.
			return false
		}
		state = &keyState{values: make(map[string]struct{})}
		g.track(key, state)
	}

	if state.capped {
		g.metrics().CardinalityCapped(key)
		return true
	}

	if _, seen := state.values[value]; seen {
		return false
	}
	state.values[value] = struct{}{}

	if len(state.values) >= maxValues {
		state.capped = true
		// Released: once capped, which values the key held buys nothing, and
		// holding them is the largest allocation the guard makes.
		state.values = nil
		g.metrics().CardinalityCapped(key)
		return true
	}
	return false
}

func (g *CardinalityGuard) track(key string, state *keyState) {
	if g.current == nil {
		g.current = make(map[string]*keyState)
	}
	g.current[key] = state
}

// rotateIfDue ages the window forward. Callers hold g.mu.
func (g *CardinalityGuard) rotateIfDue() {
	now := g.now()
	if g.rotatedAt.IsZero() {
		g.rotatedAt = now
		return
	}
	elapsed := now.Sub(g.rotatedAt)
	if elapsed < g.window() {
		return
	}
	// More than two windows of silence means both generations are stale;
	// dropping previous as well avoids resurrecting long-dead observations.
	if elapsed >= 2*g.window() {
		g.previous = nil
	} else {
		g.previous = g.current
	}
	g.current = nil
	g.rotatedAt = now
}

// TrackedKeys reports how many keys the guard currently holds state for.
// Exported for tests and for operators who want to see the guard's own bound
// being respected.
func (g *CardinalityGuard) TrackedKeys() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.current)
}
