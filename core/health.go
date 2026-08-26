package core

import (
	"fmt"
	"strings"
	"time"
)

// DrainSummary is what a bounded shutdown actually achieved (FR10, ADR-0015).
//
// Loss at shutdown is permitted — bounding shutdown time is a hard requirement
// in any orchestrated environment. Silent loss is not, which is why this type
// exists: the number has to be reportable, not merely counted.
type DrainSummary struct {
	// Lost is how many records were still buffered when the deadline expired.
	Lost int
	// Duration is how long the drain took.
	Duration time.Duration
	// Destinations are the exporters the records would have gone to.
	Destinations []string
	// OpenCircuits are the destinations that were refusing calls when the
	// drain ended — the likeliest explanation for anything lost.
	OpenCircuits []string
}

// Clean reports whether the buffer emptied before the deadline.
func (s DrainSummary) Clean() bool { return s.Lost == 0 }

// String is the final summary line ADR-0015 requires before exit.
//
// It names the count and the destinations rather than saying "some records
// were lost", because an operator reading it at 3am needs to know whether to
// look at crier or at the backend — and "to which exporters" is the half that
// answers that.
func (s DrainSummary) String() string {
	var b strings.Builder
	if s.Clean() {
		fmt.Fprintf(&b, "drain complete in %s, no records lost", s.Duration.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "drain incomplete after %s: %d record(s) lost, never exported to %s",
			s.Duration.Round(time.Millisecond), s.Lost, joinOr(s.Destinations, "no configured destination"))
	}
	if len(s.OpenCircuits) > 0 {
		fmt.Fprintf(&b, "; circuits open at exit: %s", strings.Join(s.OpenCircuits, ", "))
	}
	return b.String()
}

func joinOr(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	return strings.Join(values, ", ")
}

// Health answers the two questions an orchestrator asks (NFR5, ADR-0005).
//
// It is deliberately not an HTTP handler: what "ready" means is engine
// behaviour and belongs where it can be tested without a server, and serving
// it is the daemon's job.
//
// The zero value is not usable; build one with NewHealth.
type Health struct {
	dispatcher *Dispatcher
}

// NewHealth returns the health view of a running dispatcher.
func NewHealth(dispatcher *Dispatcher) (*Health, error) {
	if dispatcher == nil {
		return nil, fmt.Errorf("health needs a dispatcher; without one readiness cannot reflect export health, which is the only thing it is for (ADR-0015)")
	}
	return &Health{dispatcher: dispatcher}, nil
}

// Live reports whether the process should stay alive.
//
// It is true for as long as the process is running, including while degraded
// and while draining. Liveness that fails on a backend outage gets the
// instance killed and restarted into the same outage, losing whatever was
// buffered — which is a way of turning someone else's outage into data loss of
// our own.
func (h *Health) Live() bool { return true }

// Ready reports whether this instance should receive traffic, and why not when
// it should not.
//
// Not ready in two states, both from ADR-0015:
//
//   - draining, because the buffer is closed and nothing further is accepted;
//   - degraded, because every destination's circuit is open, so an accepted
//     record has nowhere to go and would be counted as lost on arrival.
//
// An operator seeing not-ready during a backend outage could read it as a
// crash loop, so the reason is returned rather than left to be inferred. The
// instance is still alive, still buffering what it already holds, and still
// probing the destinations behind their breakers.
func (h *Health) Ready() (ready bool, reason string) {
	if h.dispatcher.Draining() {
		return false, "draining: shutting down and no longer accepting records"
	}
	if h.dispatcher.Degraded() {
		return false, fmt.Sprintf(
			"degraded: every destination is refusing calls (%s); the instance is alive and will recover when one accepts again",
			joinOr(h.dispatcher.OpenCircuits(), "unknown"))
	}
	return true, "ready"
}
