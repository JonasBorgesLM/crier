package core

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestSeverityThresholdFiltersBelowTheLine(t *testing.T) {
	var m CountingMetrics
	f := &Filter{MinSeverity: SeverityWarn, Metrics: &m}

	for _, tc := range []struct {
		severity Severity
		wantKeep bool
	}{
		{SeverityDebug, false},
		{SeverityInfo, false},
		{SeverityWarn, true},
		{SeverityError, true},
		{SeverityFatal, true},
	} {
		rec := LogRecord{Severity: tc.severity}
		if got := f.Keep(&rec, "task-api"); got != tc.wantKeep {
			t.Errorf("Keep(%s) = %v, want %v", tc.severity, got, tc.wantKeep)
		}
	}

	// Filtered is not dropped: a correctly configured pipeline must not look
	// lossy in its metrics.
	snap := m.Snapshot()
	if got := snap.Filtered["task-api"]; got != 2 {
		t.Errorf("Filtered = %d, want 2", got)
	}
	if got := snap.TotalDropped(); got != 0 {
		t.Errorf("TotalDropped() = %d, want 0 — filtering is not loss", got)
	}
}

// The rare, important records are exactly the ones a uniform sampler is most
// likely to discard, and they are why anyone is reading the logs.
func TestSamplingNeverDiscardsErrors(t *testing.T) {
	f := &Filter{
		SampleRate: SampleNothing,
		Rand:       func() float64 { return 0.99 },
	}

	for _, sev := range []Severity{SeverityError, SeverityFatal} {
		rec := LogRecord{Severity: sev}
		if !f.Keep(&rec, "task-api") {
			t.Errorf("%s was sampled away", sev)
		}
	}
	for _, sev := range []Severity{SeverityDebug, SeverityInfo, SeverityWarn} {
		rec := LogRecord{Severity: sev}
		if f.Keep(&rec, "task-api") {
			t.Errorf("%s survived SampleNothing", sev)
		}
	}
}

func TestSampleFloorIsConfigurable(t *testing.T) {
	f := &Filter{
		SampleRate:  SampleNothing,
		SampleFloor: SeverityInfo,
	}

	if rec := (LogRecord{Severity: SeverityInfo}); !f.Keep(&rec, "task-api") {
		t.Error("INFO was sampled away despite the floor being INFO")
	}
	if rec := (LogRecord{Severity: SeverityDebug}); f.Keep(&rec, "task-api") {
		t.Error("DEBUG survived SampleNothing below the floor")
	}
}

func TestSampleRateIsHonoured(t *testing.T) {
	var draw float64
	f := &Filter{
		SampleRate: 0.25,
		Rand:       func() float64 { return draw },
	}

	for _, tc := range []struct {
		draw     float64
		wantKeep bool
	}{
		{0.0, true},
		{0.24, true},
		{0.25, false},
		{0.9, false},
	} {
		draw = tc.draw
		rec := LogRecord{Severity: SeverityInfo}
		if got := f.Keep(&rec, "task-api"); got != tc.wantKeep {
			t.Errorf("draw %v: Keep = %v, want %v", tc.draw, got, tc.wantKeep)
		}
	}
}

// A zero SampleRate means "unset", so there has to be a way to say "none"
// that is not an approximation like 0.0000001.
func TestZeroSampleRateKeepsEverything(t *testing.T) {
	f := &Filter{Rand: func() float64 { return 0.99 }}

	rec := LogRecord{Severity: SeverityDebug}
	if !f.Keep(&rec, "task-api") {
		t.Error("an unset SampleRate discarded a record")
	}
}

func TestPerSourceOverrides(t *testing.T) {
	var m CountingMetrics
	f := &Filter{
		MinSeverity: SeverityInfo,
		Metrics:     &m,
		PerSource: map[string]SourceFilter{
			"noisy":  {MinSeverity: ptr(SeverityError)},
			"quiet":  {MinSeverity: ptr(SeverityDebug)},
			"partly": {SampleRate: ptr(SampleNothing)},
		},
	}

	warn := LogRecord{Severity: SeverityWarn}
	if f.Keep(&warn, "noisy") {
		t.Error("noisy: WARN survived a per-source ERROR threshold")
	}
	if !f.Keep(&warn, "quiet") {
		t.Error("quiet: WARN was filtered despite a per-source DEBUG threshold")
	}
	if !f.Keep(&warn, "unlisted") {
		t.Error("unlisted: WARN was filtered despite the global INFO threshold")
	}

	// A nil field inherits rather than resetting to the zero value.
	debug := LogRecord{Severity: SeverityDebug}
	if f.Keep(&debug, "partly") {
		t.Error("partly: DEBUG survived, so the inherited INFO threshold was lost")
	}
}

// ADR-0022: an attribute rule may only narrow what a source's own settings
// already keep. These tests are the "never widen" property, checked from
// every direction it could fail.
func TestAttributeRuleNarrowsSeverity(t *testing.T) {
	f := &Filter{
		MinSeverity: SeverityInfo,
		Rules: []AttributeRule{
			{Key: "path", Value: "/health", MinSeverity: ptr(SeverityError)},
		},
	}

	probe := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": "/health"}}
	if f.Keep(&probe, "task-api") {
		t.Error("a health-check WARN survived a rule raising the threshold to ERROR")
	}

	business := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": "/v1/tasks"}}
	if !f.Keep(&business, "task-api") {
		t.Error("a non-matching WARN was filtered; the rule must not apply beyond its match")
	}
}

func TestAttributeRuleCannotWidenSeverity(t *testing.T) {
	f := &Filter{
		MinSeverity: SeverityError,
		Rules: []AttributeRule{
			// A rule trying to lower the bar below what identity requires.
			{Key: "path", Value: "/health", MinSeverity: ptr(SeverityDebug)},
		},
	}
	rec := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": "/health"}}
	if f.Keep(&rec, "task-api") {
		t.Error("a rule widened MinSeverity below the source's own ERROR floor")
	}
}

func TestAttributeRuleNarrowsSampleRate(t *testing.T) {
	f := &Filter{
		Rules: []AttributeRule{
			{Key: "path", Value: "/health", SampleRate: ptr(SampleNothing)},
		},
	}
	rec := LogRecord{Severity: SeverityInfo, Attributes: map[string]any{"path": "/health"}}
	for range 50 {
		if f.Keep(&rec, "task-api") {
			t.Fatal("a record matching a SampleNothing rule was kept")
		}
	}
}

func TestAttributeRuleCannotWidenSampleRate(t *testing.T) {
	f := &Filter{
		SampleRate: SampleNothing, // source keeps nothing eligible for sampling
		Rules: []AttributeRule{
			// A rule trying to keep everything for this attribute.
			{Key: "path", Value: "/health", SampleRate: ptr(1.0)},
		},
	}
	rec := LogRecord{Severity: SeverityInfo, Attributes: map[string]any{"path": "/health"}}
	for range 50 {
		if f.Keep(&rec, "task-api") {
			t.Fatal("a rule widened SampleRate above the source's own SampleNothing")
		}
	}
}

func TestAttributeRuleSampleFloorStillApplies(t *testing.T) {
	f := &Filter{
		Rules: []AttributeRule{
			{Key: "path", Value: "/health", SampleRate: ptr(SampleNothing)},
		},
	}
	// A health endpoint that has started failing: exactly the record its own
	// rule must not be able to suppress.
	rec := LogRecord{Severity: SeverityError, Attributes: map[string]any{"path": "/health"}}
	if !f.Keep(&rec, "task-api") {
		t.Error("an ERROR record was discarded by a matching rule; the sample floor must exempt it")
	}
}

func TestAttributeRuleMatchesValuePrefix(t *testing.T) {
	f := &Filter{Rules: []AttributeRule{
		{Key: "path", ValuePrefix: "/health", MinSeverity: ptr(SeverityError)},
	}}
	for _, path := range []string{"/health", "/health/ready", "/healthcheck"} {
		rec := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": path}}
		if f.Keep(&rec, "task-api") {
			t.Errorf("path %q matching the prefix survived the rule", path)
		}
	}
	rec := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": "/v1/tasks"}}
	if !f.Keep(&rec, "task-api") {
		t.Error("a path not matching the prefix was filtered")
	}
}

func TestAttributeRuleOnlyMatchesStringAttributes(t *testing.T) {
	f := &Filter{Rules: []AttributeRule{
		{Key: "status", Value: "200", MinSeverity: ptr(SeverityError)},
	}}
	// The value is the number 200, not the string "200" — a rule keyed on
	// string equality must not match by comparing a stringified rendering.
	rec := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"status": 200}}
	if !f.Keep(&rec, "task-api") {
		t.Error("a non-string attribute value matched a Value rule")
	}
}

func TestAttributeRuleNeverConsultsBody(t *testing.T) {
	f := &Filter{Rules: []AttributeRule{
		{Key: "path", Value: "/health", MinSeverity: ptr(SeverityError)},
	}}
	// The text "/health" appears in Body, not in an attribute. A rule that
	// somehow consulted Body would filter this; one that only reads
	// Attributes, as designed, must not.
	rec := LogRecord{Severity: SeverityWarn, Body: "GET /health 200 in 1ms"}
	if !f.Keep(&rec, "task-api") {
		t.Error("a rule matched against Body text; rules must only ever consult Attributes")
	}
}

func TestAttributeRuleFirstMatchWins(t *testing.T) {
	f := &Filter{Rules: []AttributeRule{
		{Key: "path", Value: "/health", MinSeverity: ptr(SeverityDebug)}, // would keep everything
		{Key: "path", Value: "/health", MinSeverity: ptr(SeverityError)}, // would filter this WARN
	}}
	rec := LogRecord{Severity: SeverityWarn, Attributes: map[string]any{"path": "/health"}}
	if !f.Keep(&rec, "task-api") {
		t.Error("the second rule applied; the first matching rule must win")
	}
}

func TestAttributeRuleWithNoMatchLeavesSourceSettingsInForce(t *testing.T) {
	f := &Filter{
		MinSeverity: SeverityInfo,
		PerSource:   map[string]SourceFilter{"noisy": {MinSeverity: ptr(SeverityError)}},
		Rules:       []AttributeRule{{Key: "path", Value: "/health", MinSeverity: ptr(SeverityFatal)}},
	}
	// No "path" attribute at all: the rule cannot match, so PerSource's own
	// ERROR threshold for "noisy" must be exactly what applies.
	rec := LogRecord{Severity: SeverityWarn}
	if f.Keep(&rec, "noisy") {
		t.Error("noisy: WARN survived despite the per-source ERROR threshold and no rule match")
	}
}

func TestAttributeRuleValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule AttributeRule
		want string
	}{
		{"no key", AttributeRule{Value: "x", MinSeverity: ptr(SeverityError)}, "no Key"},
		{
			"neither selector",
			AttributeRule{Key: "path", MinSeverity: ptr(SeverityError)},
			"neither Value nor ValuePrefix",
		},
		{
			"both selectors",
			AttributeRule{Key: "path", Value: "/health", ValuePrefix: "/health", MinSeverity: ptr(SeverityError)},
			"both Value and ValuePrefix",
		},
		{
			"neither narrowing field",
			AttributeRule{Key: "path", Value: "/health"},
			"a rule that changes nothing",
		},
		{
			"severity out of range",
			AttributeRule{Key: "path", Value: "/health", MinSeverity: ptr(Severity(99))},
			"MinSeverity",
		},
		{
			"rate out of range",
			AttributeRule{Key: "path", Value: "/health", SampleRate: ptr(2.0)},
			"SampleRate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Filter{Rules: []AttributeRule{tc.rule}}
			err := f.Validate()
			if err == nil {
				t.Fatal("Validate accepted an impossible rule")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestKeepBatchCompactsInPlace(t *testing.T) {
	f := &Filter{MinSeverity: SeverityWarn}

	batch := []LogRecord{
		{Severity: SeverityError, Body: "a"},
		{Severity: SeverityDebug, Body: "b"},
		{Severity: SeverityWarn, Body: "c"},
		{Severity: SeverityInfo, Body: "d"},
	}
	kept := f.KeepBatch(batch, "task-api")

	if len(kept) != 2 {
		t.Fatalf("kept %d records, want 2", len(kept))
	}
	if kept[0].Body != "a" || kept[1].Body != "c" {
		t.Errorf("kept %q and %q, want a and c", kept[0].Body, kept[1].Body)
	}
	// The tail must be cleared, or a filtered batch pins the attribute maps of
	// records it discarded for as long as the array lives.
	tail := batch[len(kept):cap(batch)]
	for i, rec := range tail {
		if rec.Body != "" || rec.Attributes != nil {
			t.Errorf("tail[%d] still holds a record: %+v", i, rec)
		}
	}
}

func TestKeepBatchOfNothing(t *testing.T) {
	f := &Filter{}
	if got := f.KeepBatch(nil, "task-api"); len(got) != 0 {
		t.Errorf("got %d records from a nil batch", len(got))
	}
}

func TestZeroFilterKeepsEverything(t *testing.T) {
	var f Filter
	for _, sev := range []Severity{SeverityUnspecified, SeverityTrace, SeverityFatal} {
		rec := LogRecord{Severity: sev}
		if !f.Keep(&rec, "task-api") {
			t.Errorf("the zero Filter discarded %s", sev)
		}
	}
}

// Silently discarding telemetry because of a config typo is the failure mode
// eager validation exists to prevent (NFR4).
func TestValidateRejectsImpossibleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter Filter
		want   string
	}{
		{"rate above one", Filter{SampleRate: 1.5}, "SampleRate"},
		{"negative rate", Filter{SampleRate: -0.5}, "SampleRate"},
		{"severity out of range", Filter{MinSeverity: 99}, "MinSeverity"},
		{
			"per-source rate",
			Filter{PerSource: map[string]SourceFilter{"x": {SampleRate: ptr(2.0)}}},
			`PerSource["x"].SampleRate`,
		},
		{
			"per-source severity",
			Filter{PerSource: map[string]SourceFilter{"x": {MinSeverity: ptr(Severity(-4))}}},
			`PerSource["x"].MinSeverity`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Validate()
			if err == nil {
				t.Fatal("Validate accepted an impossible configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending field %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsUsableConfiguration(t *testing.T) {
	f := Filter{
		MinSeverity: SeverityInfo,
		SampleRate:  0.1,
		PerSource: map[string]SourceFilter{
			"a": {SampleRate: ptr(SampleNothing)},
			"b": {MinSeverity: ptr(SeverityError)},
		},
	}
	if err := f.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestSamplingIsRoughlyUniform(t *testing.T) {
	f := &Filter{SampleRate: 0.3}

	const n = 20_000
	var kept int
	for range n {
		rec := LogRecord{Severity: SeverityInfo}
		if f.Keep(&rec, "task-api") {
			kept++
		}
	}

	ratio := float64(kept) / n
	if ratio < 0.27 || ratio > 0.33 {
		t.Errorf("kept %.3f of records, want ~0.30", ratio)
	}
}
