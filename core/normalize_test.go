package core

import (
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// The acceptance criterion for ADR-0009: a broken clock must not cost a source
// its logs, and the breakage must still be visible.
func TestNormalizeAcceptsAndReportsBadTimestamps(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		claimed     time.Time
		wantSkew    bool
		wantMissing bool
	}{
		{"missing", time.Time{}, false, true},
		{"far future", observed.AddDate(50, 0, 0), true, false},
		{"far past", observed.AddDate(-50, 0, 0), true, false},
		{"just over threshold", observed.Add(2 * time.Minute), true, false},
		{"within threshold", observed.Add(3 * time.Second), false, false},
		{"exact", observed, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m CountingMetrics
			n := &Normalizer{Now: fixedClock(observed), Metrics: &m}

			rec := LogRecord{Body: "kept", Timestamp: tc.claimed}
			n.Normalize(&rec, "task-api")

			// Accepted, always.
			if rec.Body != "kept" {
				t.Fatalf("record was altered: %q", rec.Body)
			}
			if !rec.ObservedTimestamp.Equal(observed) {
				t.Errorf("ObservedTimestamp = %v, want %v", rec.ObservedTimestamp, observed)
			}
			if !rec.EffectiveTime().Equal(observed) {
				t.Errorf("EffectiveTime() = %v, want %v", rec.EffectiveTime(), observed)
			}
			// The source's claim is preserved, not corrected: rewriting it
			// would erase the evidence that the clock is wrong.
			if !rec.Timestamp.Equal(tc.claimed) {
				t.Errorf("Timestamp was rewritten to %v, want %v", rec.Timestamp, tc.claimed)
			}

			snap := m.Snapshot()
			if got := snap.ClockSkew["task-api"].Count > 0; got != tc.wantSkew {
				t.Errorf("skew reported = %v, want %v", got, tc.wantSkew)
			}
			if got := snap.TimestampMissing["task-api"] > 0; got != tc.wantMissing {
				t.Errorf("missing reported = %v, want %v", got, tc.wantMissing)
			}
		})
	}
}

// A missing timestamp is not a clock that is wrong by ~2000 years. Folding it
// into the skew statistic would swamp every real measurement.
func TestMissingTimestampDoesNotPolluteSkewStatistic(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var m CountingMetrics
	n := &Normalizer{Now: fixedClock(observed), Metrics: &m}

	skewed := LogRecord{Timestamp: observed.Add(5 * time.Minute)}
	n.Normalize(&skewed, "task-api")
	for range 10 {
		missing := LogRecord{}
		n.Normalize(&missing, "task-api")
	}

	stat := m.Snapshot().ClockSkew["task-api"]
	if stat.Count != 1 {
		t.Fatalf("skew samples = %d, want 1", stat.Count)
	}
	if stat.Max != 5*time.Minute {
		t.Errorf("Max = %v, want 5m — a missing timestamp leaked into the statistic", stat.Max)
	}
	if got := m.Snapshot().TimestampMissing["task-api"]; got != 10 {
		t.Errorf("TimestampMissing = %d, want 10", got)
	}
}

// Re-stamping on every hop would move the authoritative time forward each time
// a record passed a stage.
func TestNormalizeDoesNotRestampAnObservedRecord(t *testing.T) {
	first := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	rec := LogRecord{ObservedTimestamp: first}
	n := &Normalizer{Now: fixedClock(later)}
	n.Normalize(&rec, "task-api")

	if !rec.ObservedTimestamp.Equal(first) {
		t.Errorf("ObservedTimestamp = %v, want the original %v", rec.ObservedTimestamp, first)
	}
}

func TestNormalizeBatchStampsRecordsTogether(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var m CountingMetrics
	n := &Normalizer{Now: fixedClock(observed), Metrics: &m}

	batch := []LogRecord{
		{Timestamp: observed.Add(time.Second)},
		{},
		{Timestamp: observed.Add(-time.Hour)},
	}
	n.NormalizeBatch(batch, "task-api")

	for i, rec := range batch {
		if !rec.ObservedTimestamp.Equal(observed) {
			t.Errorf("batch[%d].ObservedTimestamp = %v, want %v", i, rec.ObservedTimestamp, observed)
		}
	}

	snap := m.Snapshot()
	if got := snap.TimestampMissing["task-api"]; got != 1 {
		t.Errorf("TimestampMissing = %d, want 1", got)
	}
	if got := snap.ClockSkew["task-api"].Count; got != 1 {
		t.Errorf("skew samples = %d, want 1 (only the one-hour deviation)", got)
	}
}

func TestNormalizeBatchOfNothingDoesNothing(t *testing.T) {
	var m CountingMetrics
	n := &Normalizer{Metrics: &m}
	n.NormalizeBatch(nil, "task-api")

	if snap := m.Snapshot(); len(snap.TimestampMissing) != 0 || len(snap.ClockSkew) != 0 {
		t.Errorf("empty batch produced observations: %+v", snap)
	}
}

func TestZeroNormalizerIsUsable(t *testing.T) {
	var n Normalizer
	before := time.Now()
	rec := LogRecord{}
	n.Normalize(&rec, "task-api")

	if rec.ObservedTimestamp.Before(before) {
		t.Errorf("ObservedTimestamp = %v, want at or after %v", rec.ObservedTimestamp, before)
	}
}

func TestNegativeSkewThresholdDisablesReporting(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var m CountingMetrics
	n := &Normalizer{Now: fixedClock(observed), SkewThreshold: -1, Metrics: &m}

	rec := LogRecord{Timestamp: observed.AddDate(100, 0, 0)}
	n.Normalize(&rec, "task-api")

	if got := m.Snapshot().ClockSkew["task-api"].Count; got != 0 {
		t.Errorf("skew samples = %d, want 0", got)
	}
}
