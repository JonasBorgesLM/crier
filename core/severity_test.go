package core

import "testing"

func TestSeverityString(t *testing.T) {
	for _, tc := range []struct {
		in   Severity
		want string
	}{
		{SeverityUnspecified, "UNSPECIFIED"},
		{SeverityTrace, "TRACE"},
		{SeverityDebug, "DEBUG"},
		{SeverityInfo, "INFO"},
		{SeverityInfo + 3, "INFO"}, // INFO2..INFO4 stay INFO
		{SeverityWarn, "WARN"},
		{SeverityError, "ERROR"},
		{SeverityFatal, "FATAL"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSeverityValid(t *testing.T) {
	for _, tc := range []struct {
		in   Severity
		want bool
	}{
		{SeverityUnspecified, true},
		{SeverityFatal, true},
		{24, true},
		{25, false},
		{-1, false},
	} {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("Severity(%d).Valid() = %v, want %v", tc.in, got, tc.want)
		}
	}
}
