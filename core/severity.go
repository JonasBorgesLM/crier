package core

// Severity is the OpenTelemetry log severity number. The numeric values are
// fixed by the OTel Logs data model and are relied upon for range comparisons,
// so a threshold filter can be expressed as a single >= check.
type Severity int

// Severity levels, aligned with the OpenTelemetry Logs data model.
const (
	SeverityUnspecified Severity = 0
	SeverityTrace       Severity = 1
	SeverityDebug       Severity = 5
	SeverityInfo        Severity = 9
	SeverityWarn        Severity = 13
	SeverityError       Severity = 17
	SeverityFatal       Severity = 21
)

// String returns the short name of the severity range s falls into.
func (s Severity) String() string {
	switch {
	case s >= SeverityFatal:
		return "FATAL"
	case s >= SeverityError:
		return "ERROR"
	case s >= SeverityWarn:
		return "WARN"
	case s >= SeverityInfo:
		return "INFO"
	case s >= SeverityDebug:
		return "DEBUG"
	case s >= SeverityTrace:
		return "TRACE"
	default:
		return "UNSPECIFIED"
	}
}

// Valid reports whether s is within the range defined by the OTel data model.
func (s Severity) Valid() bool { return s >= SeverityUnspecified && s <= 24 }
