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
