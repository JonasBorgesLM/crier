package core

import (
	"fmt"
	"math/rand/v2"
)

// Filter is step 7 of the canonical stage order (ADR-0010, FR8): the severity
// threshold and sampler, applied before the buffer so a record that will never
// be exported never costs buffer memory.
//
// Filtering is not dropping. A filtered record was never meant to leave, so it
// is counted through RecordsFiltered rather than RecordsDropped — folding the
// two together would make a correctly configured pipeline look lossy.
//
// Per-exporter filtering remains available after dequeue as an additional
// narrowing, never as the only filter.
//
// The zero value keeps everything. Safe for concurrent use.
type Filter struct {
	// MinSeverity drops records below this level. Zero (SeverityUnspecified)
	// keeps everything, which is also what a record with no severity gets.
	MinSeverity Severity

	// SampleRate is the fraction of eligible records kept, in [0,1]. Zero
	// means 1 — no sampling. Use SampleNothing to drop all eligible records.
	SampleRate float64

	// SampleFloor is the severity at or above which sampling never applies.
	// Zero means SeverityError.
	//
	// Sampling away errors defeats the purpose: the rare, important records
	// are exactly the ones a uniform sampler is most likely to discard, and
	// they are the reason anyone is looking at the logs.
	SampleFloor Severity

	// PerSource overrides the global settings by attested source identity
	// (ADR-0008) — never by anything the client asserts, or a noisy source
	// could exempt itself from its own sampling.
	PerSource map[string]SourceFilter

	// Rand returns a value in [0,1). Nil means math/rand/v2. Tests override it.
	Rand func() float64

	// Metrics receives filtered counts. Nil discards.
	Metrics Metrics
}

// SampleNothing is the SampleRate that discards every eligible record. It
// exists because a zero SampleRate means "unset", and a config that cannot
// express "none" pushes operators into approximating it with 0.0000001.
const SampleNothing = -1.0

// SourceFilter overrides Filter's thresholds for one source. A nil field means
// "inherit"; this is why they are pointers rather than plain values, where a
// zero would be indistinguishable from an unset override.
type SourceFilter struct {
	MinSeverity *Severity
	SampleRate  *float64
}

// Validate reports configuration that cannot be right, so it fails at startup
// rather than silently discarding telemetry in production (NFR4).
func (f *Filter) Validate() error {
	if err := validateRate("SampleRate", f.SampleRate); err != nil {
		return err
	}
	if f.MinSeverity != 0 && !f.MinSeverity.Valid() {
		return fmt.Errorf("MinSeverity %d is outside the OTel severity range", f.MinSeverity)
	}
	for source, override := range f.PerSource {
		if override.SampleRate != nil {
			if err := validateRate(fmt.Sprintf("PerSource[%q].SampleRate", source), *override.SampleRate); err != nil {
				return err
			}
		}
		if override.MinSeverity != nil && *override.MinSeverity != 0 && !override.MinSeverity.Valid() {
			return fmt.Errorf("PerSource[%q].MinSeverity %d is outside the OTel severity range",
				source, *override.MinSeverity)
		}
	}
	return nil
}

func validateRate(name string, rate float64) error {
	if rate == SampleNothing || rate == 0 {
		return nil
	}
	if rate < 0 || rate > 1 {
		return fmt.Errorf("%s is %v, want a fraction in [0,1] (or SampleNothing)", name, rate)
	}
	return nil
}

func (f *Filter) metrics() Metrics {
	if f.Metrics != nil {
		return f.Metrics
	}
	return NopMetrics{}
}

func (f *Filter) random() float64 {
	if f.Rand != nil {
		return f.Rand()
	}
	return rand.Float64() //nolint:gosec // sampling, not security
}

func (f *Filter) sampleFloor() Severity {
	if f.SampleFloor == 0 {
		return SeverityError
	}
	return f.SampleFloor
}

// settings resolves the effective thresholds for a source.
func (f *Filter) settings(source string) (minSeverity Severity, sampleRate float64) {
	minSeverity, sampleRate = f.MinSeverity, f.SampleRate
	if override, ok := f.PerSource[source]; ok {
		if override.MinSeverity != nil {
			minSeverity = *override.MinSeverity
		}
		if override.SampleRate != nil {
			sampleRate = *override.SampleRate
		}
	}
	return minSeverity, sampleRate
}

// Keep reports whether rec should continue to the buffer, counting it as
// filtered if not.
//
// source is the attested principal, used to resolve per-source overrides and
// to label the count.
func (f *Filter) Keep(rec *LogRecord, source string) bool {
	minSeverity, sampleRate := f.settings(source)

	if rec.Severity < minSeverity {
		f.metrics().RecordsFiltered(source, 1)
		return false
	}

	if rec.Severity >= f.sampleFloor() {
		return true
	}

	switch {
	case sampleRate == SampleNothing:
		f.metrics().RecordsFiltered(source, 1)
		return false
	case sampleRate == 0 || sampleRate >= 1:
		return true
	case f.random() < sampleRate:
		return true
	default:
		f.metrics().RecordsFiltered(source, 1)
		return false
	}
}

// KeepBatch filters batch in place and returns the surviving prefix.
//
// It reuses batch's backing array, so the caller must not keep the original
// slice header expecting the filtered records to still be there.
func (f *Filter) KeepBatch(batch []LogRecord, source string) []LogRecord {
	kept := batch[:0]
	for i := range batch {
		if f.Keep(&batch[i], source) {
			kept = append(kept, batch[i])
		}
	}
	// Release references to the records that dropped out, so a filtered batch
	// does not pin their attribute maps for as long as the array lives.
	clear(batch[len(kept):])
	return kept
}
