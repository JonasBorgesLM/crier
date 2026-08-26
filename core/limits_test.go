package core

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestApplyTruncatesOversizedValuesAndKeepsTheRecord(t *testing.T) {
	var m CountingMetrics
	l := Limits{MaxValueBytes: 20, Metrics: &m}

	rec := LogRecord{
		Body:       "short body",
		Attributes: map[string]any{"query": strings.Repeat("x", 500), "user": "alice"},
	}
	l.Apply(&rec)

	// Losing one oversized field beats losing the event.
	if rec.Body != "short body" {
		t.Errorf("Body = %q, want it untouched", rec.Body)
	}
	if rec.Attributes["user"] != "alice" {
		t.Errorf("user = %v, want alice", rec.Attributes["user"])
	}

	got, ok := rec.Attributes["query"].(string)
	if !ok {
		t.Fatalf("query = %T, want string", rec.Attributes["query"])
	}
	if len(got) > 20 {
		t.Errorf("query is %d bytes, want at most 20", len(got))
	}
	if !strings.HasSuffix(got, DefaultTruncationMark) {
		t.Errorf("query = %q, want the truncation marker — altered telemetry must not look like source data", got)
	}
	if m.Snapshot().AttributesTruncated["query"] != 1 {
		t.Errorf("AttributesTruncated[query] = %d, want 1", m.Snapshot().AttributesTruncated["query"])
	}
}

// A mangled trailing rune turns a log line into an encoding error downstream.
func TestTruncationCutsOnRuneBoundaries(t *testing.T) {
	l := Limits{MaxValueBytes: 24, TruncationMark: "!"}
	rec := LogRecord{Attributes: map[string]any{"msg": strings.Repeat("héllo→", 20)}}
	l.Apply(&rec)

	got := rec.Attributes["msg"].(string)
	if !utf8.ValidString(got) {
		t.Errorf("truncated value is not valid UTF-8: %q", got)
	}
	if len(got) > 24 {
		t.Errorf("value is %d bytes, want at most 24", len(got))
	}
}

func TestTruncationMarkerAloneWhenNoRoomForContent(t *testing.T) {
	l := Limits{MaxValueBytes: 2, TruncationMark: "…[truncated]"}
	rec := LogRecord{Attributes: map[string]any{"k": "a much longer value"}}
	l.Apply(&rec)

	// A cap this small is a misconfiguration. Emitting nothing would hide it.
	if got := rec.Attributes["k"]; got != "…[truncated]" {
		t.Errorf("value = %q, want the bare marker", got)
	}
}

// Truncating keys makes distinct fields collide into one, corrupting data
// instead of bounding it.
func TestOverlongKeyDropsItsAttribute(t *testing.T) {
	var m CountingMetrics
	l := Limits{MaxKeyBytes: 8, Metrics: &m}

	rec := LogRecord{Attributes: map[string]any{
		"short":                    "kept",
		strings.Repeat("k", 200):   "dropped",
		strings.Repeat("k", 200-1): "also dropped",
	}}
	l.Apply(&rec)

	if len(rec.Attributes) != 1 {
		t.Fatalf("got %d attributes, want 1: %v", len(rec.Attributes), rec.Attributes)
	}
	if rec.Attributes["short"] != "kept" {
		t.Errorf("short = %v, want kept", rec.Attributes["short"])
	}
	var dropped int64
	for _, v := range m.Snapshot().AttributesDropped {
		dropped += v
	}
	if dropped != 2 {
		t.Errorf("AttributesDropped total = %d, want 2", dropped)
	}
}

// Go randomises map iteration: "drop whatever comes last" would give two
// identical records different surviving fields.
func TestAttributeCountCapIsDeterministic(t *testing.T) {
	build := func() *LogRecord {
		return &LogRecord{Attributes: map[string]any{
			"alpha": 1, "bravo": 2, "charlie": 3, "delta": 4, "echo": 5,
		}}
	}
	l := Limits{MaxAttributes: 3}

	first := build()
	l.Apply(first)

	for range 20 {
		next := build()
		l.Apply(next)
		if len(next.Attributes) != 3 {
			t.Fatalf("got %d attributes, want 3", len(next.Attributes))
		}
		for k := range first.Attributes {
			if _, ok := next.Attributes[k]; !ok {
				t.Fatalf("survivors differ between runs: %v vs %v", first.Attributes, next.Attributes)
			}
		}
	}
}

// Walking an attacker-supplied structure to measure it is the exhaustion
// vector this stage exists to close.
func TestUnboundableValueTypesAreReplaced(t *testing.T) {
	var m CountingMetrics
	l := Limits{Metrics: &m}

	rec := LogRecord{Attributes: map[string]any{
		"nested": map[string]any{"a": map[string]any{"b": "deep"}},
		"list":   []any{1, 2, 3},
		"str":    "fine",
		"num":    42,
		"flt":    1.5,
		"bool":   true,
		"dur":    time.Second,
		"nil":    nil,
	}}
	l.Apply(&rec)

	for _, k := range []string{"nested", "list"} {
		if rec.Attributes[k] != DefaultUnsupportedMark {
			t.Errorf("%s = %v, want the unsupported marker", k, rec.Attributes[k])
		}
	}
	for k, want := range map[string]any{
		"str": "fine", "num": 42, "flt": 1.5, "bool": true, "dur": time.Second, "nil": nil,
	} {
		if rec.Attributes[k] != want {
			t.Errorf("%s = %v, want %v untouched", k, rec.Attributes[k], want)
		}
	}
	if got := m.Snapshot().AttributesDropped["nested"]; got != 1 {
		t.Errorf("AttributesDropped[nested] = %d, want 1", got)
	}
}

// Reslicing keeps the whole backing array alive, which defeats the cap.
func TestByteSliceTruncationCopies(t *testing.T) {
	large := make([]byte, 1024)
	rec := LogRecord{Attributes: map[string]any{"blob": large}}
	Limits{MaxValueBytes: 16}.Apply(&rec)

	got := rec.Attributes["blob"].([]byte)
	if len(got) != 16 {
		t.Fatalf("len = %d, want 16", len(got))
	}
	if cap(got) > 16 {
		t.Errorf("cap = %d, want at most 16 — the large allocation is still held", cap(got))
	}
}

func TestBodyIsCappedAndCounted(t *testing.T) {
	var m CountingMetrics
	l := Limits{MaxBodyBytes: 32, Metrics: &m}

	rec := LogRecord{Body: strings.Repeat("y", 1000)}
	l.Apply(&rec)

	if len(rec.Body) > 32 {
		t.Errorf("Body is %d bytes, want at most 32", len(rec.Body))
	}
	if got := m.Snapshot().AttributesTruncated["body"]; got != 1 {
		t.Errorf("body truncation count = %d, want 1", got)
	}
}

func TestResourceAttributesAreLimitedToo(t *testing.T) {
	rec := LogRecord{Resource: Resource{Attributes: map[string]any{
		"region": strings.Repeat("z", 500),
	}}}
	Limits{MaxValueBytes: 20}.Apply(&rec)

	if got := len(rec.Resource.Attributes["region"].(string)); got > 20 {
		t.Errorf("resource attribute is %d bytes, want at most 20", got)
	}
}

func TestNegativeLimitDisablesThatLimitOnly(t *testing.T) {
	long := strings.Repeat("q", 5000)
	rec := LogRecord{Body: long, Attributes: map[string]any{"k": long}}
	Limits{MaxValueBytes: -1, MaxBodyBytes: 100}.Apply(&rec)

	if rec.Attributes["k"] != long {
		t.Errorf("value was altered despite MaxValueBytes < 0")
	}
	if len(rec.Body) > 100 {
		t.Errorf("Body is %d bytes, want at most 100 — an unrelated limit was disabled", len(rec.Body))
	}
}

func TestZeroLimitsAppliesDefaults(t *testing.T) {
	rec := LogRecord{Body: strings.Repeat("b", DefaultMaxBodyBytes*2)}
	Limits{}.Apply(&rec)

	if len(rec.Body) > DefaultMaxBodyBytes {
		t.Errorf("Body is %d bytes, want at most %d", len(rec.Body), DefaultMaxBodyBytes)
	}
}

func TestApplyOnEmptyRecordDoesNothing(t *testing.T) {
	rec := LogRecord{}
	Limits{}.Apply(&rec)
	if rec.Attributes != nil || rec.Body != "" {
		t.Errorf("empty record was altered: %+v", rec)
	}
}
