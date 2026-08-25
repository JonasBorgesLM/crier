package core

import (
	"errors"
	"strings"
	"testing"
)

func mustRedactor(t *testing.T, cfg RedactionConfig) *Redactor {
	t.Helper()
	r, err := NewRedactor(cfg)
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	return r
}

// Finding A-2: secrets leak into the message text far more often than into a
// structured attribute, and a stage that only masks attributes provides false
// confidence while missing the common path.
func TestBodyRedactionMasksSecretsInMessageText(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	for _, tc := range []struct {
		name    string
		body    string
		secret  string
		context string
	}{
		{
			name:    "bearer token",
			body:    "upstream rejected: Authorization: Bearer aGVsbG8td29ybGQtc2VjcmV0",
			secret:  "aGVsbG8td29ybGQtc2VjcmV0",
			context: "upstream rejected",
		},
		{
			name:   "jwt",
			body:   "decoding eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk failed",
			secret: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		},
		{
			name:   "interpolated key=value",
			body:   `auth failed for token=s3cr3t-value-here user=alice`,
			secret: "s3cr3t-value-here",
		},
		{
			name:   "password with colon",
			body:   `connecting with password: hunter2hunter2`,
			secret: "hunter2hunter2",
		},
		{
			name:   "aws access key",
			body:   "using AKIAIOSFODNN7EXAMPLE for upload",
			secret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:   "pem private key",
			body:   "loaded -----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END RSA PRIVATE KEY----- ok",
			secret: "MIIBOgIBAAJBAK",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := LogRecord{Body: tc.body}
			if err := r.Redact(&rec, "task-api"); err != nil {
				t.Fatalf("Redact: %v", err)
			}
			if strings.Contains(rec.Body, tc.secret) {
				t.Errorf("secret survived redaction:\n  got:  %q\n  leak: %q", rec.Body, tc.secret)
			}
			if !strings.Contains(rec.Body, RedactionMark) {
				t.Errorf("no redaction marker in %q — the masking must be visible", rec.Body)
			}
			// Spans are replaced, not deleted: a redacted log that no longer
			// says what happened has traded one problem for another.
			if tc.context != "" && !strings.Contains(rec.Body, tc.context) {
				t.Errorf("surrounding context lost: %q", rec.Body)
			}
		})
	}
}

func TestSensitiveAttributeKeysAreMaskedWholesale(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	rec := LogRecord{Attributes: map[string]any{
		"password":      "hunter2",
		"api_key":       "abc123",
		"Authorization": "Bearer xyz",
		"session-id":    "s-1",
		"user":          "alice",
		"http.status":   200,
	}}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	for _, k := range []string{"password", "api_key", "Authorization", "session-id"} {
		if rec.Attributes[k] != RedactionMark {
			t.Errorf("%s = %v, want %s", k, rec.Attributes[k], RedactionMark)
		}
	}
	if rec.Attributes["user"] != "alice" {
		t.Errorf("user = %v, want alice", rec.Attributes["user"])
	}
	if rec.Attributes["http.status"] != 200 {
		t.Errorf("http.status = %v, want 200", rec.Attributes["http.status"])
	}
}

// A sensitive value can sit under an innocuous key.
func TestAttributeValuesGetThePatternScanToo(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	rec := LogRecord{Attributes: map[string]any{
		"note": "retry with Bearer aGVsbG8td29ybGQtc2VjcmV0",
	}}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if got := rec.Attributes["note"].(string); strings.Contains(got, "aGVsbG8td29ybGQtc2VjcmV0") {
		t.Errorf("secret survived under an innocuous key: %q", got)
	}
}

func TestResourceAttributesAreRedacted(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	rec := LogRecord{Resource: Resource{Attributes: map[string]any{"api_key": "leak"}}}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if rec.Resource.Attributes["api_key"] != RedactionMark {
		t.Errorf("resource attribute = %v, want redacted", rec.Resource.Attributes["api_key"])
	}
}

// Finding A-3: the failure policy must be fail-closed. A control that degrades
// into permitting what it guards against is not a control.
func TestRuntimeRedactionFailureDropsAndCounts(t *testing.T) {
	var m CountingMetrics
	r := mustRedactor(t, RedactionConfig{Metrics: &m})
	r.failHook = func() error { return errors.New("rule engine unavailable") }

	rec := LogRecord{
		Body:       "Authorization: Bearer aGVsbG8td29ybGQtc2VjcmV0",
		Attributes: map[string]any{"password": "hunter2"},
	}
	err := r.Redact(&rec, "task-api")

	if err == nil {
		t.Fatal("Redact returned nil after the stage failed — that is fail-open")
	}
	if !errors.Is(err, ErrRedactionFailed) {
		t.Errorf("error does not wrap ErrRedactionFailed: %v", err)
	}
	// The caller drops on this error, so the record itself is still unmasked.
	// What must never happen is the error being absent.
	if got := m.Snapshot().DroppedBy(DropRedactionFailed); got != 1 {
		t.Errorf("DroppedBy(redaction_failed) = %d, want 1 — the drop must be visible", got)
	}
	if got := m.Snapshot().TotalDropped(); got != 1 {
		t.Errorf("TotalDropped() = %d, want exactly 1", got)
	}
}

// A panic anywhere in the stage must become a drop, never an unredacted
// export slipping past on a recovered goroutine.
func TestPanicDuringRedactionBecomesADrop(t *testing.T) {
	var m CountingMetrics
	r := mustRedactor(t, RedactionConfig{Metrics: &m})
	r.failHook = func() error { panic("boom") }

	rec := LogRecord{Body: "token=s3cr3t-value-here"}
	err := r.Redact(&rec, "task-api")

	if err == nil {
		t.Fatal("a panic in redaction was swallowed and the record allowed through")
	}
	if !errors.Is(err, ErrRedactionFailed) {
		t.Errorf("error does not wrap ErrRedactionFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error does not carry the panic value: %v", err)
	}
	if got := m.Snapshot().DroppedBy(DropRedactionFailed); got != 1 {
		t.Errorf("DroppedBy(redaction_failed) = %d, want 1", got)
	}
}

func TestNilRedactorFailsClosed(t *testing.T) {
	var r *Redactor
	rec := LogRecord{Body: "Bearer aGVsbG8td29ybGQtc2VjcmV0"}

	err := r.Redact(&rec, "task-api")
	if err == nil {
		t.Fatal("a nil redactor let the record through unmasked")
	}
	if !errors.Is(err, ErrRedactionFailed) {
		t.Errorf("error does not wrap ErrRedactionFailed: %v", err)
	}
}

// A config typo must become a deployment failure, not a service that runs
// while silently leaking (NFR4, ADR-0014).
func TestInvalidPatternPreventsConstruction(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RedactionConfig
		want string
	}{
		{"bad key pattern", RedactionConfig{KeyPatterns: []string{"("}}, "key pattern 0"},
		{"bad body pattern", RedactionConfig{BodyPatterns: []string{"a(b"}}, "body pattern 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRedactor(tc.cfg)
			if err == nil {
				t.Fatal("NewRedactor accepted an invalid pattern")
			}
			if r != nil {
				t.Error("a redactor was returned alongside the error")
			}
			// A regex rejected without saying which one is a support ticket.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not identify the offending pattern", err)
			}
		})
	}
}

func TestSkipBodyStillRedactsKeys(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{SkipBody: true})

	rec := LogRecord{
		Body:       "token=s3cr3t-value-here",
		Attributes: map[string]any{"password": "hunter2"},
	}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if rec.Attributes["password"] != RedactionMark {
		t.Error("SkipBody turned off key redaction — it is a cost lever, not a fail-open switch")
	}
	if rec.Body != "token=s3cr3t-value-here" {
		t.Errorf("Body = %q, want it unscanned", rec.Body)
	}
}

func TestEmptyPatternSetsDisableTheirRules(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{
		KeyPatterns:  []string{},
		BodyPatterns: []string{},
	})

	rec := LogRecord{Body: "password: hunter2", Attributes: map[string]any{"token": "t"}}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if rec.Body != "password: hunter2" || rec.Attributes["token"] != "t" {
		t.Errorf("explicitly empty rule sets still redacted: %+v", rec)
	}
}

func TestMultipleSecretsInOneLineAreAllMasked(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	rec := LogRecord{Body: "token=aaaaaaaa then password=bbbbbbbb then api_key=cccccccc"}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	for _, leak := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if strings.Contains(rec.Body, leak) {
			t.Errorf("%q survived: %q", leak, rec.Body)
		}
	}
	if n := strings.Count(rec.Body, RedactionMark); n != 3 {
		t.Errorf("got %d markers, want 3: %q", n, rec.Body)
	}
}

func TestRedactionIsIdempotent(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	rec := LogRecord{Body: "Authorization: Bearer aGVsbG8td29ybGQtc2VjcmV0"}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	once := rec.Body
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if rec.Body != once {
		t.Errorf("second pass changed the record:\n  1: %q\n  2: %q", once, rec.Body)
	}
}

// Pattern matching over free text cannot be complete, and pretending
// otherwise is the false confidence finding A-2 warns about.
func TestBodyRedactionIsBestEffort(t *testing.T) {
	r := mustRedactor(t, RedactionConfig{})

	// A secret with no recognisable shape and no sensitive-looking key.
	rec := LogRecord{Body: "user alice used correct horse battery staple"}
	if err := r.Redact(&rec, "task-api"); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if strings.Contains(rec.Body, RedactionMark) {
		t.Skip("pattern set became broad enough to catch this; update the documented limitation")
	}
	// Documented, not fixed: this is why structured attributes are preferred.
}
