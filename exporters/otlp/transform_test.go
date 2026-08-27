package otlp

import (
	"fmt"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/slim/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/slim/otlp/logs/v1"

	"github.com/JonasBorgesLM/crier/core"
)

// One buffered batch can hold records from several sources, and OTLP groups by
// resource. Sending them as one resource would attribute every service's logs
// to whichever one happened to be first in the batch.
func TestBuildRequestGroupsByResource(t *testing.T) {
	batch := []core.LogRecord{
		{Body: "a1", Resource: core.Resource{ServiceName: "task-api", ServiceVersion: "1.0"}},
		{Body: "b1", Resource: core.Resource{ServiceName: "gateway-auth"}},
		{Body: "a2", Resource: core.Resource{ServiceName: "task-api", ServiceVersion: "1.0"}},
		{Body: "a3", Resource: core.Resource{ServiceName: "task-api", ServiceVersion: "2.0"}},
	}

	req := buildRequest(batch)

	if got := len(req.GetResourceLogs()); got != 3 {
		t.Fatalf("ResourceLogs = %d, want 3 (two task-api versions and gateway-auth)", got)
	}

	bodies := map[string][]string{}
	for _, rl := range req.GetResourceLogs() {
		var name, version string
		for _, kv := range rl.GetResource().GetAttributes() {
			switch kv.GetKey() {
			case "service.name":
				name = kv.GetValue().GetStringValue()
			case "service.version":
				version = kv.GetValue().GetStringValue()
			}
		}
		key := name + "@" + version
		for _, rec := range rl.GetScopeLogs()[0].GetLogRecords() {
			bodies[key] = append(bodies[key], rec.GetBody().GetStringValue())
		}
	}

	for key, want := range map[string][]string{
		"task-api@1.0":  {"a1", "a2"},
		"task-api@2.0":  {"a3"},
		"gateway-auth@": {"b1"},
	} {
		got := bodies[key]
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("records for %s = %v, want %v", key, got, want)
		}
	}
}

// Grouping is by the whole resource, descriptive attributes included: two
// records that differ in deployment.environment are not the same origin.
func TestBuildRequestGroupsOnResourceAttributesToo(t *testing.T) {
	batch := []core.LogRecord{
		{Body: "prod", Resource: core.Resource{
			ServiceName: "task-api",
			Attributes:  map[string]any{"deployment.environment": "production"},
		}},
		{Body: "staging", Resource: core.Resource{
			ServiceName: "task-api",
			Attributes:  map[string]any{"deployment.environment": "staging"},
		}},
	}

	if got := len(buildRequest(batch).GetResourceLogs()); got != 2 {
		t.Errorf("ResourceLogs = %d, want 2 — the environments are different origins", got)
	}
}

func TestResourceKeyIsOrderIndependent(t *testing.T) {
	a := core.Resource{ServiceName: "task-api", Attributes: map[string]any{"x": 1, "y": 2}}
	b := core.Resource{ServiceName: "task-api", Attributes: map[string]any{"y": 2, "x": 1}}

	if resourceKey(a) != resourceKey(b) {
		t.Errorf("resourceKey depends on map iteration order:\n%q\n%q", resourceKey(a), resourceKey(b))
	}
}

// Correlation is optional and never required for a record to be valid
// (ADR-0004). An id that is not a real id is dropped rather than sent as
// something a backend would index and nobody could join on.
func TestTraceCorrelationIdsAreValidatedNotGuessed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		traceID string
		spanID  string
		wantIDs bool
	}{
		{"valid", "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7", true},
		{"absent", "", "", false},
		{"too short", "4bf92f35", "00f067aa", false},
		{"not hex", "zzf92f3577b34da6a3ce929d0e0e4736", "zzf067aa0ba902b7", false},
		{"all zeroes is the OTel invalid id", "00000000000000000000000000000000", "0000000000000000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := logRecord(&core.LogRecord{TraceID: tc.traceID, SpanID: tc.spanID})
			if got := len(rec.GetTraceId()) > 0; got != tc.wantIDs {
				t.Errorf("traceId present = %v, want %v", got, tc.wantIDs)
			}
			if got := len(rec.GetSpanId()) > 0; got != tc.wantIDs {
				t.Errorf("spanId present = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

// OTLP reads a zero timestamp as "absent", and the epoch is an instant a
// source could in principle assert. The two must not be confused.
func TestTimestampsDistinguishAbsentFromZero(t *testing.T) {
	t.Run("absent source timestamp stays absent", func(t *testing.T) {
		rec := logRecord(&core.LogRecord{ObservedTimestamp: time.Unix(1700000000, 0)})
		if got := rec.GetTimeUnixNano(); got != 0 {
			t.Errorf("timeUnixNano = %d, want 0", got)
		}
		if got := rec.GetObservedTimeUnixNano(); got == 0 {
			t.Error("observedTimeUnixNano = 0, want the authoritative timestamp")
		}
	})

	t.Run("a source claiming a time before the epoch is not sent as garbage", func(t *testing.T) {
		rec := logRecord(&core.LogRecord{
			Timestamp:         time.Unix(-1000, 0),
			ObservedTimestamp: time.Unix(1700000000, 0),
		})
		if got := rec.GetTimeUnixNano(); got != 0 {
			t.Errorf("timeUnixNano = %d, want 0 — a negative instant cannot be an unsigned nanosecond count", got)
		}
	})

	// Skew is reported, never corrected (ADR-0009): a clock that is wrong by
	// two years is carried through so the backend can see it.
	t.Run("an absurd source timestamp is carried, not corrected", func(t *testing.T) {
		claimed := time.Unix(4102444800, 0) // 2100
		rec := logRecord(&core.LogRecord{Timestamp: claimed, ObservedTimestamp: time.Unix(1700000000, 0)})
		if got, want := rec.GetTimeUnixNano(), uint64(claimed.UnixNano()); got != want {
			t.Errorf("timeUnixNano = %d, want %d", got, want)
		}
	})
}

func TestAttributeValueMapping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		check func(*commonpb.AnyValue) bool
	}{
		{"string", "hello", func(v *commonpb.AnyValue) bool { return v.GetStringValue() == "hello" }},
		{"bool", true, func(v *commonpb.AnyValue) bool { return v.GetBoolValue() }},
		{"int", 42, func(v *commonpb.AnyValue) bool { return v.GetIntValue() == 42 }},
		{"int64", int64(-7), func(v *commonpb.AnyValue) bool { return v.GetIntValue() == -7 }},
		{"uint32", uint32(7), func(v *commonpb.AnyValue) bool { return v.GetIntValue() == 7 }},
		{"float64", 1.5, func(v *commonpb.AnyValue) bool { return v.GetDoubleValue() == 1.5 }},
		{"float32", float32(0.5), func(v *commonpb.AnyValue) bool { return v.GetDoubleValue() == 0.5 }},
		{"bytes", []byte{1, 2}, func(v *commonpb.AnyValue) bool { return len(v.GetBytesValue()) == 2 }},
		{"nil", nil, func(v *commonpb.AnyValue) bool { return v.GetValue() == nil }},
		{
			// The limits stage has already reduced values to scalars
			// (ADR-0010); anything else is rendered rather than dropped, so
			// the field survives even when its type does not.
			name:  "anything else renders rather than disappearing",
			value: struct{ A int }{3},
			check: func(v *commonpb.AnyValue) bool { return v.GetStringValue() == "{3}" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyValue(tc.value); !tc.check(got) {
				t.Errorf("anyValue(%#v) = %v, which is not what this type should map to", tc.value, got)
			}
		})
	}
}

func TestAttributesAreOrderedSoIdenticalRecordsEncodeIdentically(t *testing.T) {
	attrs := map[string]any{"z": 1, "a": 2, "m": 3}

	first := attributes(attrs)
	for range 20 {
		got := attributes(attrs)
		for i := range got {
			if got[i].GetKey() != first[i].GetKey() {
				t.Fatalf("attribute order is not stable: %v then %v", keys(first), keys(got))
			}
		}
	}
	if want := []string{"a", "m", "z"}; fmt.Sprint(keys(first)) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v", keys(first), want)
	}
}

func keys(kvs []*commonpb.KeyValue) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.GetKey()
	}
	return out
}

func TestSeverityMapsStraightThrough(t *testing.T) {
	// ADR-0004 aligned the model with OTel precisely so this needs no lookup
	// table to go wrong in.
	for _, sev := range []core.Severity{
		core.SeverityTrace, core.SeverityDebug, core.SeverityInfo,
		core.SeverityWarn, core.SeverityError, core.SeverityFatal,
	} {
		rec := logRecord(&core.LogRecord{Severity: sev})
		if got, want := rec.GetSeverityNumber(), logspb.SeverityNumber(sev); got != want {
			t.Errorf("severity %v mapped to %v, want %v", sev, got, want)
		}
	}
}

// OTLP has no unsigned integer type. A value that does not fit in a signed
// 64-bit integer must not wrap: -1 presented as a real reading is worse than
// an oddly formatted one.
func TestUnsignedAttributesDoNotWrapNegative(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      any
		wantInt    int64
		wantString string
	}{
		{name: "fits", value: uint64(42), wantInt: 42},
		{name: "uint fits", value: uint(7), wantInt: 7},
		{name: "overflows", value: uint64(1) << 63, wantString: "9223372036854775808"},
		{name: "max", value: ^uint64(0), wantString: "18446744073709551615"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := anyValue(tc.value)
			if tc.wantString != "" {
				if got.GetStringValue() != tc.wantString {
					t.Errorf("anyValue(%v) = %v, want the string %q", tc.value, got, tc.wantString)
				}
				return
			}
			if got.GetIntValue() != tc.wantInt {
				t.Errorf("anyValue(%v) = %v, want %d", tc.value, got, tc.wantInt)
			}
		})
	}
}

// A value outside the OTel range is not a severity, and truncating it into
// int32 would invent one.
func TestSeverityOutsideTheOTelRangeIsUnspecified(t *testing.T) {
	for _, sev := range []core.Severity{-1, 25, 1 << 40} {
		rec := logRecord(&core.LogRecord{Severity: sev})
		if got := rec.GetSeverityNumber(); got != logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
			t.Errorf("severity %d mapped to %v, want unspecified", sev, got)
		}
	}
}

// A version placeholder reaching the backend reads like a real version and
// identifies nothing.
func TestPlaceholderScopeVersionsAreDropped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v1.2.3", "v1.2.3"},
		{"(devel)", ""},
		{"v0.0.0-00010101000000-000000000000", ""},
		{"", ""},
	} {
		if got := releaseVersion(tc.in); got != tc.want {
			t.Errorf("releaseVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
