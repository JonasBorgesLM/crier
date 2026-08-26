package otlp

import (
	"encoding/hex"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	collogs "go.opentelemetry.io/proto/slim/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/slim/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/slim/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/slim/otlp/resource/v1"

	"github.com/JonasBorgesLM/crier/core"
)

// scopeName identifies crier as the producer of these records, per the OTel
// instrumentation-scope convention.
const scopeName = "github.com/JonasBorgesLM/crier"

// scopeVersion reports this module's version from the build info, so the
// scope does not carry a constant that drifts from reality the first time
// nobody remembers to bump it.
var scopeVersion = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/JonasBorgesLM/crier/exporters/otlp" {
			return dep.Version
		}
	}
	if info.Main.Path == "github.com/JonasBorgesLM/crier/exporters/otlp" {
		return info.Main.Version
	}
	return ""
})

// buildRequest maps a crier batch onto the OTLP logs payload.
//
// ADR-0004 aligned LogRecord with the OTel data model precisely so that this
// stays a mapping rather than a translation, and it is: every field has a
// counterpart, and nothing has to be invented or guessed.
func buildRequest(batch []core.LogRecord) *collogs.ExportLogsServiceRequest {
	// Records from several sources share one batch, and OTLP groups by
	// resource, so the batch is regrouped rather than sent as one resource
	// with mixed identities.
	order := make([]string, 0, 4)
	groups := make(map[string][]*logspb.LogRecord, 4)
	resources := make(map[string]core.Resource, 4)

	for i := range batch {
		key := resourceKey(batch[i].Resource)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
			resources[key] = batch[i].Resource
		}
		groups[key] = append(groups[key], logRecord(&batch[i]))
	}

	out := &collogs.ExportLogsServiceRequest{
		ResourceLogs: make([]*logspb.ResourceLogs, 0, len(order)),
	}
	for _, key := range order {
		out.ResourceLogs = append(out.ResourceLogs, &logspb.ResourceLogs{
			Resource: resource(resources[key]),
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: scopeName, Version: scopeVersion()},
				LogRecords: groups[key],
			}},
		})
	}
	return out
}

// resourceKey identifies a resource for grouping. Two records group together
// exactly when every resource field matches.
func resourceKey(r core.Resource) string {
	if len(r.Attributes) == 0 {
		// The common shape, and worth not paying for the sort.
		return r.ServiceName + "\x00" + r.ServiceVersion
	}

	keys := make([]string, 0, len(r.Attributes))
	for k := range r.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(r.ServiceName)
	b.WriteByte(0)
	b.WriteString(r.ServiceVersion)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", r.Attributes[k])
	}
	return b.String()
}

// resource maps crier's Resource onto the OTel one, using semantic-convention
// attribute names (ADR-0004).
func resource(r core.Resource) *resourcepb.Resource {
	attrs := make([]*commonpb.KeyValue, 0, len(r.Attributes)+2)
	if r.ServiceName != "" {
		attrs = append(attrs, keyValue("service.name", r.ServiceName))
	}
	if r.ServiceVersion != "" {
		attrs = append(attrs, keyValue("service.version", r.ServiceVersion))
	}
	attrs = append(attrs, attributes(r.Attributes)...)
	return &resourcepb.Resource{Attributes: attrs}
}

// logRecord maps one record.
func logRecord(rec *core.LogRecord) *logspb.LogRecord {
	out := &logspb.LogRecord{
		// ObservedTimeUnixNano is the authoritative one (ADR-0009).
		ObservedTimeUnixNano: unixNano(rec.EffectiveTime().UnixNano(), !rec.ObservedTimestamp.IsZero()),
		// TimeUnixNano is what the source claimed. It is carried through
		// untouched, including when it is absurd: the backend can see the
		// skew for itself, and silently correcting it would hide a broken
		// clock rather than expose it.
		TimeUnixNano:   unixNano(rec.Timestamp.UnixNano(), !rec.Timestamp.IsZero()),
		SeverityNumber: severityNumber(rec.Severity),
		SeverityText:   rec.SeverityText,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: rec.Body}},
		Attributes:     attributes(rec.Attributes),
	}

	// Correlation is optional and never required for a record to be valid
	// (ADR-0004). An unparsable id is dropped rather than sent as garbage a
	// backend would index and nobody could ever join on.
	if id, ok := decodeID(rec.TraceID, 16); ok {
		out.TraceId = id
	}
	if id, ok := decodeID(rec.SpanID, 8); ok {
		out.SpanId = id
	}
	return out
}

// severityNumber maps crier's severity onto OTLP's.
//
// ADR-0004 aligned the two, so this is the identity for every defined value
// and needs no lookup table to go wrong in. What it does need is the bound: a
// value outside the OTel range is not a severity, and silently truncating it
// into int32 would invent one.
func severityNumber(s core.Severity) logspb.SeverityNumber {
	if s < core.SeverityUnspecified || s > maxSeverity {
		return logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED
	}
	return logspb.SeverityNumber(s)
}

// maxSeverity is the top of the OTel severity range (FATAL4).
const maxSeverity = core.Severity(24)

// unixNano returns the timestamp, or zero when the field was never set. The
// distinction matters: OTLP reads 0 as "absent", and the epoch is a real
// instant a source could in principle assert.
func unixNano(nanos int64, set bool) uint64 {
	if !set || nanos < 0 {
		return 0
	}
	return uint64(nanos)
}

// decodeID parses a hex trace or span id of exactly size bytes.
func decodeID(s string, size int) ([]byte, bool) {
	if len(s) != size*2 {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, false
	}
	// An all-zero id is the OTel "invalid" value, and sending it is the same
	// as sending nothing while looking like a real correlation.
	for _, c := range b {
		if c != 0 {
			return b, true
		}
	}
	return nil, false
}

// attributes maps crier's attribute map onto OTLP key-values, in a stable
// order so that two identical records produce two identical payloads.
func attributes(attrs map[string]any) []*commonpb.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyValue(k, attrs[k]))
	}
	return out
}

func keyValue(k string, v any) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: anyValue(v)}
}

// anyValue maps a Go value onto an OTLP AnyValue.
//
// The pipeline's limits stage has already reduced attribute values to strings,
// byte slices, and scalars (ADR-0010) — walking an attacker-supplied nested
// structure on the hot path is the exhaustion vector that stage exists to
// close. This handles what survives that, and renders anything else as its
// string form rather than dropping the field.
func anyValue(v any) *commonpb.AnyValue {
	switch t := v.(type) {
	case nil:
		return &commonpb.AnyValue{}
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: t}}
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: t}}
	case int:
		return intValue(int64(t))
	case int8:
		return intValue(int64(t))
	case int16:
		return intValue(int64(t))
	case int32:
		return intValue(int64(t))
	case int64:
		return intValue(t)
	case uint:
		return unsignedValue(uint64(t))
	case uint8:
		return intValue(int64(t))
	case uint16:
		return intValue(int64(t))
	case uint32:
		return intValue(int64(t))
	case uint64:
		return unsignedValue(t)
	case float32:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: float64(t)}}
	case float64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: t}}
	case []byte:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: t}}
	default:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("%v", t)}}
	}
}

func intValue(i int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
}

// unsignedValue maps an unsigned integer, which OTLP has no type for.
//
// One that does not fit in a signed 64-bit integer becomes its decimal string
// rather than wrapping to a negative number. A field that reads oddly is
// recoverable; one that reads as -1 when it was 18446744073709551615 is a
// wrong answer presented as a right one.
func unsignedValue(u uint64) *commonpb.AnyValue {
	if u > math.MaxInt64 {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{
			StringValue: strconv.FormatUint(u, 10),
		}}
	}
	return intValue(int64(u))
}
