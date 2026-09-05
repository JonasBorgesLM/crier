package core

import (
	"fmt"
	"math/rand/v2"
	"strings"
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

	// Rules additionally narrow what a record's own source settings would
	// otherwise keep, matched against one LogRecord attribute (ADR-0022) —
	// something the client does assert, which is exactly why a rule may only
	// narrow. Evaluated in order; the first whose Key is present as a string
	// attribute and whose value matches applies. No match leaves the
	// source's settings, from PerSource, in force.
	Rules []AttributeRule

	// Rand returns a value in [0,1). Nil means math/rand/v2. Tests override it.
	Rand func() float64

	// Metrics receives filtered counts. Nil discards.
	Metrics Metrics
}

// AttributeRule matches one LogRecord attribute and narrows the sampling
// settings a source's own identity already established (ADR-0022).
//
// It cannot widen them: MinSeverity here only ever raises the effective
// threshold, and SampleRate only ever lowers the effective keep fraction,
// regardless of what value is configured on either. That constraint — not
// the matching itself — is what keeps an attribute rule, which matches
// something client-asserted, from becoming a way for a noisy source to
// exempt itself from a limit its identity already set. Evading a rule (by
// not setting the attribute, or setting a different value) returns a record
// to the source's own settings, and no further.
//
// Only string-valued attributes match, and never Body: attributes are the
// stable, structured surface (the same reason ADR-0014 makes them
// redaction's reliable path), and a record's own Body has already passed
// through redaction (step 6) by the time this stage sees it, so a rule
// keyed on a sensitive attribute name matches [REDACTED], never a secret.
type AttributeRule struct {
	// Key is the attribute name to match. Required.
	Key string

	// Value matches this exact string. Exactly one of Value or ValuePrefix
	// must be set; Validate rejects both set or neither set.
	Value string

	// ValuePrefix matches any value with this prefix.
	ValuePrefix string

	// MinSeverity, if set, raises the effective threshold for a matched
	// record — never lowers it below what the source's own settings already
	// require. Nil makes no change.
	MinSeverity *Severity

	// SampleRate, if set, narrows the effective keep fraction for a matched
	// record — never raises it above what the source's own settings already
	// allow. Nil makes no change. The sample floor is unaffected by any
	// rule: a record at or above it is never subject to sampling, matched or
	// not, so a health endpoint that starts erroring is not silenced by its
	// own rule.
	SampleRate *float64
}

// matches reports whether rec carries the attribute this rule selects on.
func (r AttributeRule) matches(rec *LogRecord) bool {
	v, ok := rec.Attributes[r.Key]
	if !ok {
		return false
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	if r.ValuePrefix != "" {
		return strings.HasPrefix(s, r.ValuePrefix)
	}
	return s == r.Value
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
	for i, rule := range f.Rules {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

// validate reports a rule that cannot be right: no selector, both selectors,
// or a narrowing field outside its valid range.
func (r AttributeRule) validate() error {
	if r.Key == "" {
		return fmt.Errorf("no Key; a rule with nothing to match against would apply to every record")
	}
	if r.Value == "" && r.ValuePrefix == "" {
		return fmt.Errorf("key %q: neither Value nor ValuePrefix is set; a rule needs exactly one", r.Key)
	}
	if r.Value != "" && r.ValuePrefix != "" {
		return fmt.Errorf("key %q: both Value and ValuePrefix are set; a rule needs exactly one", r.Key)
	}
	if r.MinSeverity == nil && r.SampleRate == nil {
		return fmt.Errorf("key %q: neither MinSeverity nor SampleRate is set; a rule that changes nothing is likely a mistake", r.Key)
	}
	if r.MinSeverity != nil && *r.MinSeverity != 0 && !r.MinSeverity.Valid() {
		return fmt.Errorf("key %q: MinSeverity %d is outside the OTel severity range", r.Key, *r.MinSeverity)
	}
	if r.SampleRate != nil {
		if err := validateRate(fmt.Sprintf("key %q: SampleRate", r.Key), *r.SampleRate); err != nil {
			return err
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

// settings resolves the effective thresholds for a source, then narrows them
// by the first matching rule (ADR-0022) — identity sets the ceiling, and a
// rule only ever moves the result further from "keep", never back toward it.
func (f *Filter) settings(rec *LogRecord, source string) (minSeverity Severity, sampleRate float64) {
	minSeverity, sampleRate = f.MinSeverity, f.SampleRate
	if override, ok := f.PerSource[source]; ok {
		if override.MinSeverity != nil {
			minSeverity = *override.MinSeverity
		}
		if override.SampleRate != nil {
			sampleRate = *override.SampleRate
		}
	}

	for _, rule := range f.Rules {
		if !rule.matches(rec) {
			continue
		}
		if rule.MinSeverity != nil && *rule.MinSeverity > minSeverity {
			minSeverity = *rule.MinSeverity
		}
		if rule.SampleRate != nil {
			sampleRate = narrowerSampleRate(sampleRate, *rule.SampleRate)
		}
		break
	}
	return minSeverity, sampleRate
}

// narrowerSampleRate combines two SampleRate values into whichever keeps
// fewer records, in the encoding SampleRate itself uses — where zero means
// "keep everything" and SampleNothing means "keep nothing", so a plain
// numeric minimum would get both wrong.
func narrowerSampleRate(a, b float64) float64 {
	na, nb := normalizeSampleRate(a), normalizeSampleRate(b)
	n := na
	if nb < n {
		n = nb
	}
	if n <= 0 {
		return SampleNothing
	}
	return n
}

// normalizeSampleRate rewrites a SampleRate as a plain keep-fraction in
// [0,1], so it can be compared with ordinary arithmetic.
func normalizeSampleRate(rate float64) float64 {
	switch rate {
	case SampleNothing:
		return 0
	case 0:
		return 1
	default:
		return rate
	}
}

// Keep reports whether rec should continue to the buffer, counting it as
// filtered if not.
//
// source is the attested principal, used to resolve per-source overrides and
// to label the count.
func (f *Filter) Keep(rec *LogRecord, source string) bool {
	minSeverity, sampleRate := f.settings(rec, source)

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
