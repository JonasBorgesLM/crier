package httpreceiver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

var observedAt = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func decode(t *testing.T, body string) ([]core.LogRecord, error) {
	t.Helper()
	return decodeV1(strings.NewReader(body), observedAt)
}

// The acceptance criterion for #21. A service that misspells a field and gets
// 202 looks healthy while emitting unusable records; strict rejection is only
// an improvement if the error says which field (ADR-0012).
func TestUnknownFieldIsRejectedAndNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "misspelled at record level",
			body: `{"records":[{"body":"hi","severtyText":"ERROR"}]}`,
			want: "severtyText",
		},
		{
			name: "unknown at top level",
			body: `{"recrods":[]}`,
			want: "recrods",
		},
		{
			name: "unknown nested in resource",
			body: `{"records":[{"body":"hi","resource":{"tenant":"acme"}}]}`,
			want: "tenant",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decode(t, tc.body)
			if err == nil {
				t.Fatal("the payload was accepted; an unknown field must be rejected")
			}

			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("error is %T (%v), want *BadRequestError", err, err)
			}
			if !strings.Contains(bad.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", bad, tc.want)
			}
		})
	}
}

// encoding/json matches field names without regard to case, so strictness has
// this hole in it. Pinned as a decision rather than left as a surprise: a
// misspelling that still maps to the intended field loses no data, which is
// not the failure ADR-0012 is about.
func TestFieldMatchingIgnoresCase(t *testing.T) {
	records, err := decode(t, `{"records":[{"BODY":"hi","resource":{"servicename":"task-api"}}]}`)
	if err != nil {
		t.Fatalf("decode: %v — if this now rejects, the documented limitation in decodeV1 is stale", err)
	}
	if got := records[0].Body; got != "hi" {
		t.Errorf("Body = %q, want the value to reach the intended field", got)
	}
	if got := records[0].Resource.ServiceName; got != "task-api" {
		t.Errorf("Resource.ServiceName = %q", got)
	}
}

// Adding an optional field stays backwards-compatible: an older client that
// omits it is still valid (ADR-0012).
func TestOmittedOptionalFieldsAreAccepted(t *testing.T) {
	records, err := decode(t, `{"records":[{"body":"the minimum a client can send"}]}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	rec := records[0]
	if rec.Body != "the minimum a client can send" {
		t.Errorf("body = %q", rec.Body)
	}
	// The authoritative timestamp is assigned here, never accepted from the
	// caller (ADR-0009).
	if !rec.ObservedTimestamp.Equal(observedAt) {
		t.Errorf("ObservedTimestamp = %v, want %v", rec.ObservedTimestamp, observedAt)
	}
	if !rec.Timestamp.IsZero() {
		t.Errorf("Timestamp = %v, want the zero value when the client sent none", rec.Timestamp)
	}
}

func TestFullRecordIsMappedOntoTheInternalModel(t *testing.T) {
	claimed := "2026-08-26T11:59:00Z"
	records, err := decode(t, `{"records":[{
		"timestamp":"`+claimed+`",
		"severityNumber":17,
		"severityText":"ERROR",
		"body":"database unreachable",
		"attributes":{"attempt":3,"endpoint":"db:5432"},
		"traceId":"4bf92f3577b34da6a3ce929d0e0e4736",
		"spanId":"00f067aa0ba902b7",
		"resource":{"serviceName":"task-api","serviceVersion":"1.4.0","attributes":{"region":"eu-west-1"}}
	}]}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec := records[0]
	wantClaimed, _ := time.Parse(time.RFC3339, claimed)
	for _, tc := range []struct {
		field string
		got   any
		want  any
	}{
		{"Severity", rec.Severity, core.SeverityError},
		{"SeverityText", rec.SeverityText, "ERROR"},
		{"Body", rec.Body, "database unreachable"},
		{"TraceID", rec.TraceID, "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"SpanID", rec.SpanID, "00f067aa0ba902b7"},
		{"Resource.ServiceName", rec.Resource.ServiceName, "task-api"},
		{"Resource.ServiceVersion", rec.Resource.ServiceVersion, "1.4.0"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}
	if !rec.Timestamp.Equal(wantClaimed) {
		t.Errorf("Timestamp = %v, want the claimed %v carried through", rec.Timestamp, wantClaimed)
	}
	if got := rec.Attributes["endpoint"]; got != "db:5432" {
		t.Errorf("attributes[endpoint] = %v", got)
	}
	if got := rec.Resource.Attributes["region"]; got != "eu-west-1" {
		t.Errorf("resource attributes[region] = %v", got)
	}
}

// Absent and zero are different: 0 is SEVERITY_NUMBER_UNSPECIFIED, which a
// client may genuinely mean.
func TestSeverityZeroIsNotTheSameAsAbsent(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"absent", `{"records":[{"body":"hi"}]}`},
		{"explicit zero", `{"records":[{"body":"hi","severityNumber":0}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, err := decode(t, tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := records[0].Severity; got != core.SeverityUnspecified {
				t.Errorf("Severity = %v, want unspecified", got)
			}
		})
	}
}

func TestInvalidValuesAreRejectedAndNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "unparsable timestamp",
			body: `{"records":[{"body":"hi","timestamp":"last tuesday"}]}`,
			want: "records[0].timestamp",
		},
		{
			// Tightening validation later is a breaking change under
			// ADR-0012, so the strict reading has to ship with v1.
			name: "severity outside the OTel range",
			body: `{"records":[{"body":"hi","severityNumber":99}]}`,
			want: "records[0].severityNumber",
		},
		{
			name: "wrong type",
			body: `{"records":[{"body":42}]}`,
			want: "body",
		},
		{
			name: "malformed JSON",
			body: `{"records":[`,
			want: "malformed JSON",
		},
		{
			name: "empty body",
			body: ``,
			want: "empty body",
		},
		{
			// Accepting the first object and ignoring the rest would accept
			// half of what was sent.
			name: "trailing data",
			body: `{"records":[]}{"records":[]}`,
			want: "unexpected data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decode(t, tc.body)
			if err == nil {
				t.Fatal("payload accepted, want a rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A transport limit (step 1 of ADR-0010), bounding the work one request can
// ask for before any of it is admitted.
func TestTooManyRecordsIsRejected(t *testing.T) {
	body := `{"records":[` + strings.Repeat(`{"body":"x"},`, MaxRecordsPerRequest) + `{"body":"one too many"}]}`

	_, err := decode(t, body)
	if err == nil {
		t.Fatal("a request over the record limit was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds the limit") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
}

func TestEmptyRecordListIsAccepted(t *testing.T) {
	// Nothing is lost by an empty request, and rejecting it would be
	// validation this version cannot tighten later without a new path
	// (ADR-0012).
	records, err := decode(t, `{"records":[]}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("got %d records, want 0", len(records))
	}
}

func TestWireVersionPath(t *testing.T) {
	if got, want := V1.Path(), "/v1/logs"; got != want {
		t.Errorf("V1.Path() = %q, want %q", got, want)
	}
}
