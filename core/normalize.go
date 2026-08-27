package core

import "time"

// DefaultSkewThreshold is how far a source's asserted Timestamp may sit from
// the ObservedTimestamp before the deviation is reported. Ordinary scheduling
// and network delay produce small differences on every record; reporting those
// would bury the signal that actually matters — a source whose clock is wrong.
const DefaultSkewThreshold = time.Minute

// Normalizer is step 4 of the canonical stage order (ADR-0010): it stamps the
// authoritative timestamp and reports what the source claimed.
//
// It never rejects a record. A missing or absurd Timestamp is a broken source,
// not a hostile one, and dropping its logs would destroy exactly the evidence
// needed to notice the breakage (ADR-0009).
//
// The zero value is usable: it uses time.Now, DefaultSkewThreshold, and
// discards metrics.
type Normalizer struct {
	// Now supplies the observation time. Nil means time.Now. Tests override it;
	// production should not.
	Now func() time.Time

	// SkewThreshold is the deviation past which skew is reported. Zero means
	// DefaultSkewThreshold. Negative disables reporting.
	SkewThreshold time.Duration

	// Metrics receives skew and missing-timestamp observations. Nil discards.
	Metrics Metrics
}

func (n *Normalizer) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Normalizer) metrics() Metrics {
	if n.Metrics != nil {
		return n.Metrics
	}
	return NopMetrics{}
}

func (n *Normalizer) skewThreshold() time.Duration {
	if n.SkewThreshold == 0 {
		return DefaultSkewThreshold
	}
	return n.SkewThreshold
}

// Normalize assigns rec.ObservedTimestamp and reports on rec.Timestamp.
//
// source is the attested principal (ADR-0008), used only to label the
// observations — never taken from the record, which is why it is a parameter
// rather than read from rec.Resource.
//
// ObservedTimestamp is assigned once. A record that already carries one has
// passed through a pipeline stage before, and re-stamping it would move the
// authoritative time forward every hop.
func (n *Normalizer) Normalize(rec *LogRecord, source string) {
	if rec.ObservedTimestamp.IsZero() {
		rec.ObservedTimestamp = n.now()
	}

	if rec.Timestamp.IsZero() {
		n.metrics().TimestampMissing(source)
		return
	}

	threshold := n.skewThreshold()
	if threshold < 0 {
		return
	}

	deviation := rec.Timestamp.Sub(rec.ObservedTimestamp)
	if deviation < 0 {
		deviation = -deviation
	}
	if deviation >= threshold {
		// Reported, not corrected. Rewriting the source's claim would erase
		// the only evidence that its clock is wrong, and ObservedTimestamp
		// already protects every decision the pipeline makes.
		n.metrics().ClockSkew(source, rec.Timestamp.Sub(rec.ObservedTimestamp))
	}
}

// NormalizeBatch applies Normalize to every record, sharing one observation
// time across the batch so records that arrived together are stamped together.
func (n *Normalizer) NormalizeBatch(batch []LogRecord, source string) {
	if len(batch) == 0 {
		return
	}
	observed := n.now()
	for i := range batch {
		if batch[i].ObservedTimestamp.IsZero() {
			batch[i].ObservedTimestamp = observed
		}
	}
	// Reuse the per-record path for the reporting half, with Now pinned so the
	// two halves cannot disagree about when the batch was observed.
	pinned := &Normalizer{
		Now:           func() time.Time { return observed },
		SkewThreshold: n.SkewThreshold,
		Metrics:       n.Metrics,
	}
	for i := range batch {
		pinned.Normalize(&batch[i], source)
	}
}
