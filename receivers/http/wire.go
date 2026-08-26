package httpreceiver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

// WireVersion identifies an ingestion wire format. It is versioned in the
// path and independently of any module's semver (ADR-0012).
type WireVersion string

// Versions this receiver serves.
const (
	// V1 is the current format, served at /v1/logs.
	V1 WireVersion = "v1"
)

// Path returns the endpoint a version is served at.
func (v WireVersion) Path() string { return "/" + string(v) + "/logs" }

// MaxRecordsPerRequest bounds how many records one request may carry.
//
// This is a transport limit — step 1 of the canonical stage order (ADR-0010) —
// and it is separate from the buffer's capacity: it bounds the work one
// request can ask for before any of it is admitted.
const MaxRecordsPerRequest = 10_000

// logsRequest is the v1 payload.
//
// Fields are pointers or strings with no `omitempty` significance on decode:
// what matters is that an absent field is distinguishable from a zero one
// where the difference is real, which is why SeverityNumber is a pointer.
type logsRequest struct {
	Records []wireRecord `json:"records"`
}

type wireRecord struct {
	// Timestamp is RFC 3339, and is what the source claims. It is untrusted:
	// carried through, never used for a decision (ADR-0009).
	Timestamp string `json:"timestamp"`
	// SeverityNumber is the OTel severity. A pointer, so "absent" and "0" are
	// different — 0 is SEVERITY_NUMBER_UNSPECIFIED, which a client may mean.
	SeverityNumber *int           `json:"severityNumber"`
	SeverityText   string         `json:"severityText"`
	Body           string         `json:"body"`
	Attributes     map[string]any `json:"attributes"`
	TraceID        string         `json:"traceId"`
	SpanID         string         `json:"spanId"`
	// Resource is descriptive. Its identity fields are overwritten from the
	// authenticated principal (ADR-0008); sending them is not an error, it is
	// simply not authoritative.
	Resource *wireResource `json:"resource"`
}

type wireResource struct {
	ServiceName    string         `json:"serviceName"`
	ServiceVersion string         `json:"serviceVersion"`
	Attributes     map[string]any `json:"attributes"`
}

// BadRequestError is a payload this receiver refuses, with a message a client
// can act on.
//
// It names the offending field wherever the decoder gives one. ADR-0012's own
// consequences say strict parsing that cannot say *what* was wrong trades
// silent bugs for loud confusion, which is not an improvement.
type BadRequestError struct {
	// Field is the offending field, empty when the failure is not about one.
	Field string
	// Reason is safe to return to the caller.
	Reason string
}

// Error implements error.
func (e *BadRequestError) Error() string {
	if e.Field == "" {
		return "invalid request: " + e.Reason
	}
	return fmt.Sprintf("invalid request: field %q: %s", e.Field, e.Reason)
}

func badRequest(field, format string, args ...any) *BadRequestError {
	return &BadRequestError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// decodeV1 parses a v1 request body into records ready for the pipeline.
//
// Unknown fields are rejected rather than ignored (ADR-0012). A service that
// misspells "severityText" and receives 202 looks healthy while emitting
// unusable records; the mistake should surface at integration time, when
// someone is looking.
//
// One limit of that strictness is worth stating rather than discovering:
// encoding/json matches field names without regard to case, so "servicename"
// is accepted as "serviceName". It is left that way. The failure ADR-0012
// targets is a field that silently does nothing — a misspelling that maps to
// the intended field loses no data, and rejecting it would mean decoding
// twice to enforce a spelling the format never promised to police.
func decodeV1(body io.Reader, observed time.Time) ([]core.LogRecord, error) {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	var req logsRequest
	if err := dec.Decode(&req); err != nil {
		return nil, decodeError(err)
	}
	// A second JSON value after the first is not a valid request, and
	// ignoring it would accept half of what was sent.
	if dec.More() {
		return nil, badRequest("", "unexpected data after the JSON object")
	}

	if len(req.Records) > MaxRecordsPerRequest {
		return nil, badRequest("records", "%d records exceeds the limit of %d per request",
			len(req.Records), MaxRecordsPerRequest)
	}

	out := make([]core.LogRecord, 0, len(req.Records))
	for i := range req.Records {
		rec, err := req.Records[i].toLogRecord(observed)
		if err != nil {
			var bad *BadRequestError
			if errors.As(err, &bad) {
				bad.Field = fmt.Sprintf("records[%d].%s", i, bad.Field)
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// toLogRecord maps one wire record onto the internal model (ADR-0004).
func (w *wireRecord) toLogRecord(observed time.Time) (core.LogRecord, error) {
	rec := core.LogRecord{
		// Assigned here and never accepted from the caller: this is the
		// authoritative timestamp (ADR-0009).
		ObservedTimestamp: observed,
		SeverityText:      w.SeverityText,
		Body:              w.Body,
		Attributes:        w.Attributes,
		TraceID:           w.TraceID,
		SpanID:            w.SpanID,
	}

	if w.Timestamp != "" {
		claimed, err := time.Parse(time.RFC3339Nano, w.Timestamp)
		if err != nil {
			return core.LogRecord{}, badRequest("timestamp", "not an RFC 3339 timestamp")
		}
		rec.Timestamp = claimed
	}

	if w.SeverityNumber != nil {
		severity := core.Severity(*w.SeverityNumber)
		// Rejected now rather than tolerated now and tightened later:
		// tightening validation is a breaking change under ADR-0012, so the
		// strict reading has to be the one v1 ships with.
		if !severity.Valid() {
			return core.LogRecord{}, badRequest("severityNumber",
				"%d is outside the OpenTelemetry severity range 0-24", *w.SeverityNumber)
		}
		rec.Severity = severity
	}

	if w.Resource != nil {
		rec.Resource = core.Resource{
			ServiceName:    w.Resource.ServiceName,
			ServiceVersion: w.Resource.ServiceVersion,
			Attributes:     w.Resource.Attributes,
		}
	}
	return rec, nil
}

// decodeError turns a JSON failure into one that names the field.
func decodeError(err error) error {
	var unmarshalType *json.UnmarshalTypeError
	if errors.As(err, &unmarshalType) {
		field := unmarshalType.Field
		if field == "" {
			field = unmarshalType.Struct
		}
		return badRequest(field, "expected %s, got %s", unmarshalType.Type, unmarshalType.Value)
	}

	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return badRequest("", "malformed JSON at byte %d", syntax.Offset)
	}

	// A body that stops mid-value does not reach the syntax scanner; it comes
	// back as an unexpected EOF. Reported apart from a syntax error because
	// the causes differ: this one is usually a truncated upload rather than a
	// client that builds bad JSON.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badRequest("", "malformed JSON: the body ends mid-value")
	}

	// The unknown-field case has no typed error in encoding/json — only a
	// message, which does carry the field name. Lifting it out is worth the
	// string handling: the field name is the entire value of the diagnostic.
	if field, ok := unknownField(err); ok {
		return badRequest(field, "unknown field; adding fields is a wire-format change (ADR-0012)")
	}

	if errors.Is(err, io.EOF) {
		return badRequest("", "empty body")
	}
	return badRequest("", "could not be parsed as JSON")
}

// unknownFieldPrefix is what encoding/json emits for a disallowed field.
const unknownFieldPrefix = `json: unknown field `

// unknownField extracts the field name from the decoder's message.
func unknownField(err error) (string, bool) {
	msg := err.Error()
	rest, found := strings.CutPrefix(msg, unknownFieldPrefix)
	if !found {
		return "", false
	}
	return strings.Trim(rest, `"`), true
}
