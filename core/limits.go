package core

import (
	"cmp"
	"slices"
	"time"
	"unicode/utf8"
)

// Default input limits (ADR-0010, FR12). Chosen to be generous for real
// applications and still bounded: the point is that no single record can cost
// unbounded memory, not that legitimate logs get clipped.
const (
	DefaultMaxAttributes  = 128
	DefaultMaxKeyBytes    = 256
	DefaultMaxValueBytes  = 8 * 1024
	DefaultMaxBodyBytes   = 64 * 1024
	DefaultTruncationMark = "…[truncated]"
	// DefaultUnsupportedMark replaces a value whose type cannot be bounded
	// cheaply. See Limits.Apply.
	DefaultUnsupportedMark = "…[unsupported value type]"
)

// Limits caps the size of a single record (ADR-0010, step 5).
//
// They apply to embedded-library use as well as to the HTTP receiver. Embedded
// use is not exempt: a bug in the host application produces the same unbounded
// attribute map as a malicious client, and the process it takes down is the
// host's own.
//
// The zero value applies the defaults above. Set a field negative to disable
// that one limit — an explicit, greppable choice rather than a zero that could
// mean either "unset" or "none".
type Limits struct {
	// MaxAttributes caps entries per record, counting record and resource
	// attributes separately.
	MaxAttributes int
	// MaxKeyBytes caps an attribute key. An over-long key drops its attribute
	// rather than being shortened: truncating keys makes distinct fields
	// collide into one, which corrupts data instead of bounding it.
	MaxKeyBytes int
	// MaxValueBytes caps a string or []byte attribute value. Over-long values
	// are truncated with TruncationMark — losing one oversized field is better
	// than losing the event.
	MaxValueBytes int
	// MaxBodyBytes caps the log message.
	MaxBodyBytes int
	// TruncationMark is appended to anything shortened. Empty means
	// DefaultTruncationMark. It must be present: silently altered telemetry
	// that looks like source data is worse than obviously altered telemetry.
	TruncationMark string
	// UnsupportedMark replaces values of an unboundable type. Empty means
	// DefaultUnsupportedMark.
	UnsupportedMark string
	// Metrics receives every alteration. Nil discards, but nothing is ever
	// altered silently in the sense that matters — the marker is in the data.
	Metrics Metrics
}

func limitOr(v, def int) int {
	switch {
	case v < 0:
		return -1 // disabled
	case v == 0:
		return def
	default:
		return v
	}
}

func (l Limits) metrics() Metrics {
	if l.Metrics != nil {
		return l.Metrics
	}
	return NopMetrics{}
}

func (l Limits) truncationMark() string {
	return cmp.Or(l.TruncationMark, DefaultTruncationMark)
}

func (l Limits) unsupportedMark() string {
	return cmp.Or(l.UnsupportedMark, DefaultUnsupportedMark)
}

// Apply enforces the limits on rec in place.
//
// It never rejects the record. Every limit here degrades the record rather
// than discarding it, because an event that arrives with one field clipped
// still carries the information someone will be looking for at 3am; an event
// that never arrives does not.
//
// Attribute values must be strings, []byte, or scalars (bool, the integer and
// float kinds, time.Duration, time.Time). Anything else — a nested map, a
// slice, a struct — is replaced with UnsupportedMark and counted, because its
// size cannot be bounded without walking it, and walking an attacker-supplied
// structure on the hot path is the exhaustion vector this stage exists to
// close. Callers that need structure should flatten it into dotted keys, which
// is what the OTel semantic conventions do anyway.
func (l Limits) Apply(rec *LogRecord) {
	if maxBody := limitOr(l.MaxBodyBytes, DefaultMaxBodyBytes); maxBody >= 0 {
		if truncated, did := truncateString(rec.Body, maxBody, l.truncationMark()); did {
			rec.Body = truncated
			l.metrics().AttributeTruncated("body")
		}
	}

	l.applyToAttributes(rec.Attributes)
	l.applyToAttributes(rec.Resource.Attributes)
}

func (l Limits) applyToAttributes(attrs map[string]any) {
	if len(attrs) == 0 {
		return
	}

	maxKey := limitOr(l.MaxKeyBytes, DefaultMaxKeyBytes)
	maxValue := limitOr(l.MaxValueBytes, DefaultMaxValueBytes)
	maxAttrs := limitOr(l.MaxAttributes, DefaultMaxAttributes)

	for k, v := range attrs {
		if maxKey >= 0 && len(k) > maxKey {
			delete(attrs, k)
			l.metrics().AttributeDropped(k[:min(len(k), 64)])
			continue
		}
		if bounded, changed, mark := l.boundValue(v, maxValue); changed {
			attrs[k] = bounded
			switch mark {
			case markTruncated:
				l.metrics().AttributeTruncated(k)
			case markUnsupported:
				l.metrics().AttributeDropped(k)
			case markNone:
				// Unreachable: changed is false when nothing was marked.
			}
		}
	}

	if maxAttrs < 0 || len(attrs) <= maxAttrs {
		return
	}

	// Over the count cap. Which entries survive has to be deterministic:
	// Go randomises map iteration, so dropping "whatever comes last" would
	// give two identical records different fields, and make the loss
	// impossible to reason about in a report.
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys[maxAttrs:] {
		delete(attrs, k)
		l.metrics().AttributeDropped(k)
	}
}

type valueMark int

const (
	markNone valueMark = iota
	markTruncated
	markUnsupported
)

// boundValue returns the value to store, whether it changed, and why.
func (l Limits) boundValue(v any, maxValue int) (bounded any, changed bool, mark valueMark) {
	switch typed := v.(type) {
	case string:
		if maxValue < 0 {
			return v, false, markNone
		}
		if out, did := truncateString(typed, maxValue, l.truncationMark()); did {
			return out, true, markTruncated
		}
		return v, false, markNone

	case []byte:
		if maxValue < 0 || len(typed) <= maxValue {
			return v, false, markNone
		}
		// Copied, not resliced: keeping a short slice over a large backing
		// array holds the whole allocation alive, which defeats the cap.
		out := make([]byte, maxValue)
		copy(out, typed)
		return out, true, markTruncated

	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		time.Duration, time.Time:
		return v, false, markNone

	default:
		return l.unsupportedMark(), true, markUnsupported
	}
}

// truncateString shortens s to fit maxBytes including the marker, reporting
// whether it did. It cuts on a rune boundary so the result stays valid UTF-8 —
// a mangled trailing rune turns a log line into a JSON encoding error further
// down the pipeline.
func truncateString(s string, maxBytes int, mark string) (string, bool) {
	if maxBytes < 0 || len(s) <= maxBytes {
		return s, false
	}
	keep := maxBytes - len(mark)
	if keep <= 0 {
		// No room for content alongside the marker: the marker alone is the
		// honest answer, even though it exceeds the cap. A limit small enough
		// for this is a misconfiguration, and silently emitting nothing would
		// hide it.
		return mark, true
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + mark, true
}
