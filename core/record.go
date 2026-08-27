package core

import (
	"maps"
	"time"
)

// Resource identifies the origin of a log record, using OpenTelemetry
// semantic-convention attribute names (ADR-0004).
//
// The identity fields are not client-asserted. In standalone mode the receiver
// overwrites them from the authenticated principal and counts the discrepancy
// (ADR-0008); descriptive attributes supplied by the client are preserved.
type Resource struct {
	// ServiceName is semconv "service.name". Authoritative, server-derived.
	ServiceName string
	// ServiceVersion is semconv "service.version".
	ServiceVersion string
	// Attributes carries any further resource-level attributes. These are
	// descriptive, not identifying, and survive identity attestation.
	Attributes map[string]any
}

// Clone returns a deep copy of r, safe to mutate independently.
func (r Resource) Clone() Resource {
	out := r
	if r.Attributes != nil {
		out.Attributes = maps.Clone(r.Attributes)
	}
	return out
}

// LogRecord is crier's internal representation of a single log entry, aligned
// with the OpenTelemetry Logs data model (ADR-0004).
type LogRecord struct {
	// Timestamp is when the source claims the event happened. It is
	// source-asserted and therefore untrusted: it may be zero, wildly skewed,
	// or deliberately falsified. It is carried through and exported, but it is
	// never used for any decision crier makes (ADR-0009).
	Timestamp time.Time

	// ObservedTimestamp is when crier observed the record. It is assigned by
	// the pipeline, is always set, and is the authoritative time for ordering,
	// retention, and export (ADR-0009).
	ObservedTimestamp time.Time

	// Severity is the OTel severity number. Filtering and sampling compare
	// against it before the record reaches the buffer (ADR-0010).
	Severity Severity

	// SeverityText is the source's own label for the severity, preserved
	// verbatim because it is often more specific than the numeric mapping.
	SeverityText string

	// Body is the log message. Secrets leak here far more often than into
	// Attributes, so redaction covers it (ADR-0014, finding A-2).
	Body string

	// Attributes are the record's structured fields. Bounded in count, key
	// length, and value length, with a cardinality guard over values
	// (ADR-0010).
	Attributes map[string]any

	// Resource identifies the emitting service.
	Resource Resource

	// TraceID and SpanID correlate this record with a trace when the source
	// has one. Both are optional and never required for a record to be valid
	// (ADR-0004).
	TraceID string
	SpanID  string
}

// Clone returns a deep copy of rec. Pipeline stages that mutate a record must
// clone it first when the original may be shared — most notably fan-out, where
// several exporters observe the same batch (ADR-0013).
func (rec LogRecord) Clone() LogRecord {
	out := rec
	if rec.Attributes != nil {
		out.Attributes = maps.Clone(rec.Attributes)
	}
	out.Resource = rec.Resource.Clone()
	return out
}

// EffectiveTime returns the timestamp to use for any decision or export.
//
// It is ObservedTimestamp, always. The method exists so that call sites read
// as a deliberate choice rather than as an arbitrary pick between two
// timestamp fields (ADR-0009).
func (rec LogRecord) EffectiveTime() time.Time { return rec.ObservedTimestamp }
