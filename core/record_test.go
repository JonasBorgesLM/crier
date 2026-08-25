package core

import (
	"testing"
	"time"
)

func TestLogRecordCloneIsDeep(t *testing.T) {
	orig := LogRecord{
		Body:       "original",
		Attributes: map[string]any{"user": "alice"},
		Resource: Resource{
			ServiceName: "task-api",
			Attributes:  map[string]any{"region": "eu-west-1"},
		},
	}

	clone := orig.Clone()
	clone.Body = "mutated"
	clone.Attributes["user"] = "mallory"
	clone.Resource.Attributes["region"] = "elsewhere"
	clone.Resource.ServiceName = "gateway-auth"

	if clone.Body != "mutated" {
		t.Errorf("Body: clone did not take the new value: %q", clone.Body)
	}
	if orig.Body != "original" {
		t.Errorf("Body: original mutated through clone: %q", orig.Body)
	}
	if got := clone.Attributes["user"]; got != "mallory" {
		t.Errorf("Attributes: clone did not take the new value: %v", got)
	}
	if got := orig.Attributes["user"]; got != "alice" {
		t.Errorf("Attributes: original mutated through clone: %v", got)
	}
	if got := orig.Resource.Attributes["region"]; got != "eu-west-1" {
		t.Errorf("Resource.Attributes: original mutated through clone: %v", got)
	}
	if orig.Resource.ServiceName != "task-api" {
		t.Errorf("Resource.ServiceName: original mutated: %q", orig.Resource.ServiceName)
	}
}

func TestCloneHandlesNilMaps(t *testing.T) {
	clone := LogRecord{Body: "no maps"}.Clone()

	if clone.Attributes != nil {
		t.Errorf("Attributes: want nil, got %v", clone.Attributes)
	}
	if clone.Resource.Attributes != nil {
		t.Errorf("Resource.Attributes: want nil, got %v", clone.Resource.Attributes)
	}
}

// A source-asserted Timestamp is never authoritative, however absurd it is
// (ADR-0009). The record is still accepted; only the effective time differs.
func TestEffectiveTimeIgnoresSourceTimestamp(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		claimed time.Time
	}{
		{"zero", time.Time{}},
		{"far future", observed.AddDate(500, 0, 0)},
		{"far past", observed.AddDate(-500, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := LogRecord{Timestamp: tc.claimed, ObservedTimestamp: observed}
			if got := rec.EffectiveTime(); !got.Equal(observed) {
				t.Errorf("EffectiveTime() = %v, want %v", got, observed)
			}
		})
	}
}
