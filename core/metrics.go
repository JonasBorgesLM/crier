package core

import "time"

// DropReason says why a record did not survive to export. Every discard path
// in the pipeline names one.
//
// The distinctions are not cosmetic. An operator seeing sustained
// DropBufferFull sizes the buffer up; seeing DropSourceQuota looks at one
// misbehaving source; seeing DropBackendUnavailable looks at the destination
// and leaves crier alone (ADR-0011, ADR-0015). Collapsing them into a single
// "dropped" counter would leave all three looking identical.
type DropReason string

// Reasons a record can be dropped. This list is exhaustive by intent: adding a
// discard path means adding a reason here, which is the point.
const (
	// DropInvalid — the record failed validation and never entered the pipeline.
	DropInvalid DropReason = "invalid"
	// DropRedactionFailed — redaction errored, so the record was discarded
	// rather than exported unmasked (ADR-0014, fail-closed).
	DropRedactionFailed DropReason = "redaction_failed"
	// DropSourceQuota — this source's fair share was exhausted while the
	// buffer as a whole still had room (ADR-0011).
	DropSourceQuota DropReason = "source_quota"
	// DropBufferFull — the buffer was full under DropPolicyReject (ADR-0002).
	DropBufferFull DropReason = "buffer_full"
	// DropOldest — evicted to make room under DropPolicyDropOldest (ADR-0002).
	DropOldest DropReason = "drop_oldest"
	// DropBackendUnavailable — every exporter's circuit was open, so the
	// pipeline shed load rather than buffering without bound (ADR-0015).
	DropBackendUnavailable DropReason = "backend_unavailable"
	// DropShutdownTimeout — still unexported when the drain deadline expired.
	// Loss at shutdown is permitted; unaccounted loss is not (ADR-0015).
	DropShutdownTimeout DropReason = "shutdown_timeout"
)

// Metrics is crier's self-observability seam (ADR-0005). It is an interface so
// that core carries no metrics-backend dependency (NFR1): the standalone
// binary can back it with Prometheus, an embedding application with whatever
// it already uses.
//
// The methods are explicit rather than a generic Counter(name string) for one
// reason: "every drop path increments exactly one counter" is a property that
// can be reviewed when the events are named in the type, and cannot be when
// they are strings passed at call sites.
//
// Implementations must be safe for concurrent use and must not block — every
// method sits on the hot path. Embed NopMetrics to implement only the subset
// you care about.
type Metrics interface {
	// RecordsIngested counts records accepted for processing, per source.
	RecordsIngested(source string, n int)

	// RecordsDropped counts records discarded, by source and reason. Source
	// may be empty where the record was rejected before identity was attested.
	RecordsDropped(source string, reason DropReason, n int)

	// RecordsFiltered counts records removed by the severity threshold or
	// sampler (ADR-0010, step 7). Filtered is not dropped: the record was
	// never meant to be exported, so it is not a loss.
	RecordsFiltered(source string, n int)

	// RecordsExported counts records a destination has accepted.
	RecordsExported(exporter string, n int)

	// ExportLatency observes how long one batch took, per exporter.
	ExportLatency(exporter string, d time.Duration)

	// ExportRetried counts retry attempts, per exporter (ADR-0013).
	ExportRetried(exporter string)

	// CircuitStateChanged reports an exporter's breaker opening or closing.
	// All-open is the degraded state surfaced through readiness (ADR-0015).
	CircuitStateChanged(exporter string, open bool)

	// BufferDepth reports current occupancy. A gauge, not a counter.
	BufferDepth(depth int)

	// AttributeTruncated counts values shortened to fit the length cap. The
	// record survives with a marker; losing one oversized field beats losing
	// the event (ADR-0010).
	AttributeTruncated(key string)

	// CardinalityCapped counts values replaced because the key exceeded its
	// distinct-value threshold (ADR-0010).
	CardinalityCapped(key string)

	// IdentityDiscrepancy counts records whose client-asserted identity did
	// not match the authenticated principal. The record is still accepted,
	// attributed to the real principal (ADR-0008, finding D-2). A rising
	// count is either a misconfigured client or someone probing.
	IdentityDiscrepancy(claimed, actual string)

	// ClockSkew observes how far a source's asserted Timestamp sat from the
	// ObservedTimestamp. Skew never rejects a record (ADR-0009); it is
	// reported so a source with a broken clock is discoverable.
	ClockSkew(source string, deviation time.Duration)

	// DeprecatedWireVersion counts requests on a wire version scheduled for
	// removal, so the migration is driven by data (ADR-0012).
	DeprecatedWireVersion(version string)
}

// NopMetrics implements Metrics as no-ops. Embed it to implement a subset:
//
//	type bufferOnly struct {
//	    core.NopMetrics
//	    depth atomic.Int64
//	}
//	func (m *bufferOnly) BufferDepth(d int) { m.depth.Store(int64(d)) }
//
// It is also the right default for an embedding application that has not
// wired metrics up yet — the pipeline must never require them to run.
type NopMetrics struct{}

var _ Metrics = NopMetrics{}

// RecordsIngested implements [Metrics] and counts nothing.
func (NopMetrics) RecordsIngested(string, int) {}

// RecordsDropped implements [Metrics] and counts nothing.
func (NopMetrics) RecordsDropped(string, DropReason, int) {}

// RecordsFiltered implements [Metrics] and counts nothing.
func (NopMetrics) RecordsFiltered(string, int) {}

// RecordsExported implements [Metrics] and counts nothing.
func (NopMetrics) RecordsExported(string, int) {}

// ExportLatency implements [Metrics] and observes nothing.
func (NopMetrics) ExportLatency(string, time.Duration) {}

// ExportRetried implements [Metrics] and counts nothing.
func (NopMetrics) ExportRetried(string) {}

// CircuitStateChanged implements [Metrics] and records nothing.
func (NopMetrics) CircuitStateChanged(string, bool) {}

// BufferDepth implements [Metrics] and records nothing.
func (NopMetrics) BufferDepth(int) {}

// AttributeTruncated implements [Metrics] and counts nothing.
func (NopMetrics) AttributeTruncated(string) {}

// CardinalityCapped implements [Metrics] and counts nothing.
func (NopMetrics) CardinalityCapped(string) {}

// IdentityDiscrepancy implements [Metrics] and counts nothing.
func (NopMetrics) IdentityDiscrepancy(string, string) {}

// ClockSkew implements [Metrics] and observes nothing.
func (NopMetrics) ClockSkew(string, time.Duration) {}

// DeprecatedWireVersion implements [Metrics] and counts nothing.
func (NopMetrics) DeprecatedWireVersion(string) {}
