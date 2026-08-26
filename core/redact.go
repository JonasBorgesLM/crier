package core

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RedactionMark replaces every masked span and value. It is a fixed string so
// that the presence of redaction is greppable, and so the marker itself can
// never leak a hint about what it replaced (no length, no prefix, no hash).
const RedactionMark = "[REDACTED]"

// ErrRedactionFailed reports that a record could not be redacted. The record
// is dropped, never exported (ADR-0014).
var ErrRedactionFailed = errors.New("redaction failed")

// SensitiveKeySubstrings are the attribute-key fragments masked by default.
//
// Substrings rather than regexes, matched case-insensitively, because the
// field is called "authorization" in one service and "auth_header" in the
// next — and because a substring scan is roughly an order of magnitude
// cheaper than a regex per attribute per record. The benchmark made that
// difference the dominant cost of the whole pipeline.
//
// Separators are enumerated instead of matched with a character class for the
// same reason.
var SensitiveKeySubstrings = []string{
	"pass", // password, passwd, passphrase
	"secret",
	"token",
	"apikey", "api_key", "api-key", "api.key",
	"authoriz",  // authorization
	"authentic", // authentication
	"credential",
	"privatekey", "private_key", "private-key", "private.key",
	"sessionid", "session_id", "session-id", "session.id",
	"cookie",
	"signature",
}

// defaultBodyTriggers are the literals that must be present for any default
// body pattern to match. Checking them first turns the common case — a log
// line with no credential in it — from five regex passes into one scan for
// literals, which is where most of a real workload lives.
//
// It is derived from BodyPatterns by hand and only applies to that set: a
// custom pattern list disables the prefilter entirely rather than risk a
// trigger list that silently stops a rule from ever running. A performance
// optimisation must not be able to turn redaction off.
var defaultBodyTriggers = []string{
	"bearer", "basic", "digest", // authorization schemes
	"eyj",                                                  // JWT header prefix
	"pass", "secret", "token", "api", "auth", "credential", // key=value shapes
	"akia", "asia", // AWS access key IDs
	"-----begin", // PEM blocks
}

// BodyPatterns are the value shapes masked inside free text by default.
//
// Each captures the credential itself in group 1 where possible, so the
// surrounding context ("Authorization: Bearer ") survives and the line stays
// readable — a redacted log that no longer says what happened has traded one
// problem for another.
var BodyPatterns = []string{
	// Bearer / Basic / Digest authorization values.
	`(?i)\b(?:bearer|basic|digest)\s+([A-Za-z0-9._~+/=-]{8,})`,
	// JWT: three base64url segments. Distinctive enough to match on shape.
	`\b(eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,})\b`,
	// key=value / key: value where the key looks sensitive. The value runs to
	// whitespace, quote, comma, or semicolon.
	`(?i)\b(?:pass(?:word|wd|phrase)?|secret|token|api[_.-]?key|auth|credential)` +
		`\s*[=:]\s*"?([^"\s,;]{3,})"?`,
	// AWS access key IDs, which have a fixed and recognisable shape.
	`\b((?:AKIA|ASIA)[0-9A-Z]{16})\b`,
	// PEM private key blocks.
	`(?s)(-----BEGIN[A-Z ]*PRIVATE KEY-----.*?-----END[A-Z ]*PRIVATE KEY-----)`,
}

// RedactionConfig describes what to mask. Compile it into a Redactor before
// use — a config is not usable directly, so an invalid rule cannot reach the
// hot path.
type RedactionConfig struct {
	// KeySubstrings are case-insensitive fragments matched against attribute
	// keys; a match masks the whole value. Nil means SensitiveKeySubstrings.
	// Empty and non-nil means no substring rules.
	KeySubstrings []string

	// KeyPatterns are regexes matched against attribute keys, for what a
	// substring cannot express. They are checked after KeySubstrings and cost
	// considerably more per attribute, so prefer a substring where one will do.
	KeyPatterns []string
	// BodyPatterns are regexes matched against Body and against string
	// attribute values. Capture group 1, when present, is what gets masked;
	// otherwise the whole match is. Nil means BodyPatterns.
	BodyPatterns []string
	// SkipBody turns off body scanning. It does not turn off key rules.
	//
	// This is the honest lever for the cost described in ADR-0014: body
	// redaction is the most expensive stage in the pipeline. It is not a
	// fail-open switch — attribute redaction still applies, and a record that
	// fails redaction is still dropped.
	SkipBody bool
	// Metrics receives drop counts on failure. Nil discards.
	Metrics Metrics
}

// Redactor masks sensitive data in a record (ADR-0006, ADR-0014). Build one
// with NewRedactor; the zero value is not usable.
//
// Safe for concurrent use: compiled patterns are immutable and regexp.Regexp
// is safe for concurrent use by design.
type Redactor struct {
	// keyMatcher is every key pattern in one alternation. Matching each rule
	// separately meant one regex evaluation per rule per attribute — with the
	// default eleven rules and a typical record, over a hundred evaluations to
	// decide nothing was sensitive. The benchmark put that at the majority of
	// the pipeline's cost.
	keySubstrings []string
	keyMatcher    *regexp.Regexp
	bodyRules     []*regexp.Regexp
	bodyTriggers  *triggerScanner
	skipBody      bool
	metrics       Metrics

	// failHook is a test seam. Unexported, so it is not API surface and
	// cannot be reached from outside this package: fail-closed behaviour is
	// only worth asserting if the failure can actually be provoked, and no
	// public knob may exist that turns redaction off by accident.
	failHook func() error
}

// NewRedactor compiles cfg.
//
// Compilation is eager and failure is fatal to startup (NFR4, ADR-0014): a
// config typo becomes a deployment failure rather than a service that runs
// while silently leaking. The error names the offending pattern, since a
// regex rejected without saying which one is a support ticket.
func NewRedactor(cfg RedactionConfig) (*Redactor, error) {
	keySubstrings := cfg.KeySubstrings
	if keySubstrings == nil {
		keySubstrings = SensitiveKeySubstrings
	}
	usingDefaultBody := cfg.BodyPatterns == nil
	bodyPatterns := cfg.BodyPatterns
	if usingDefaultBody {
		bodyPatterns = BodyPatterns
	}

	r := &Redactor{skipBody: cfg.SkipBody, metrics: cfg.Metrics}
	r.keySubstrings = make([]string, len(keySubstrings))
	for i, sub := range keySubstrings {
		r.keySubstrings[i] = strings.ToLower(sub)
	}
	if r.metrics == nil {
		r.metrics = NopMetrics{}
	}

	// Compiled individually first, so a bad pattern is reported with its own
	// index rather than as a syntax error inside an alternation nobody wrote.
	if _, err := compilePatterns("key", cfg.KeyPatterns); err != nil {
		return nil, err
	}
	var err error
	if r.keyMatcher, err = combinePatterns(cfg.KeyPatterns); err != nil {
		return nil, fmt.Errorf("combining key patterns: %w", err)
	}
	if r.bodyRules, err = compilePatterns("body", bodyPatterns); err != nil {
		return nil, err
	}
	if usingDefaultBody && len(r.bodyRules) > 0 {
		r.bodyTriggers = newTriggerScanner(defaultBodyTriggers)
	}
	return r, nil
}

// combinePatterns folds patterns into a single alternation.
//
// Each is wrapped in a non-capturing group so an inline flag such as (?i)
// stays scoped to the pattern that declared it. Only MatchString is called on
// the result, so capture groups inside the inputs do not matter.
func combinePatterns(patterns []string) (*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = "(?:" + p + ")"
	}
	return regexp.Compile(strings.Join(parts, "|"))
}

func compilePatterns(kind string, patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s pattern %d (%q): %w", kind, i, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Redact masks rec in place, covering record attributes, resource attributes,
// and Body (ADR-0014, findings A-2 and A-3).
//
// It returns an error wrapping ErrRedactionFailed if the record could not be
// fully processed. A caller receiving that error must drop the record. There
// is no fail-open mode and there will not be one: a security control that
// degrades into permitting the thing it guards against is not a control. An
// operator who wants unredacted export disables redaction explicitly, which is
// an auditable choice rather than a silent degradation.
//
// # Best-effort on Body
//
// Pattern matching over free text cannot be complete. A secret with no
// recognisable shape, interpolated into a message, will survive. This is why
// structured attributes are preferred: key-based redaction is reliable in a way
// body scanning cannot be.
func (r *Redactor) Redact(rec *LogRecord, source string) error {
	if r == nil {
		return fmt.Errorf("%w: redactor is nil", ErrRedactionFailed)
	}

	// A panic in a regex evaluation, or in anything this stage grows later,
	// must not become an unredacted export. Recovering here converts it into
	// the drop that ADR-0014 requires.
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%w: panic during redaction: %v", ErrRedactionFailed, p)
			}
		}()
		if r.failHook != nil {
			if hookErr := r.failHook(); hookErr != nil {
				err = fmt.Errorf("%w: %w", ErrRedactionFailed, hookErr)
				return
			}
		}
		r.redactAttributes(rec.Attributes)
		r.redactAttributes(rec.Resource.Attributes)
		if !r.skipBody {
			rec.Body = r.RedactString(rec.Body)
		}
	}()

	if err != nil {
		r.metrics.RecordsDropped(source, DropRedactionFailed, 1)
		return err
	}
	return nil
}

func (r *Redactor) redactAttributes(attrs map[string]any) {
	for k, v := range attrs {
		if r.keyMatches(k) {
			// Masked in place, not marked: the unmasked value must not remain
			// reachable anywhere in the record (ADR-0006, moat's secret.Value).
			attrs[k] = RedactionMark
			continue
		}
		if s, ok := v.(string); ok && !r.skipBody {
			// A sensitive value can sit under an innocuous key, so attribute
			// values get the same pattern scan the body does.
			if masked := r.RedactString(s); masked != s {
				attrs[k] = masked
			}
		}
	}
}

func (r *Redactor) keyMatches(key string) bool {
	if len(r.keySubstrings) > 0 {
		lowered := strings.ToLower(key)
		for _, sub := range r.keySubstrings {
			if strings.Contains(lowered, sub) {
				return true
			}
		}
	}
	return r.keyMatcher != nil && r.keyMatcher.MatchString(key)
}

// RedactString masks every configured pattern in s, replacing the captured
// credential — not the whole match — where a rule captures one.
//
// Exported so a receiver can mask text it handles outside a LogRecord, and so
// the behaviour is directly testable.
func (r *Redactor) RedactString(s string) string {
	if s == "" {
		return s
	}
	// Nothing a default rule could match is present, so the expensive passes
	// would all come back empty. Skipped only for the built-in pattern set,
	// where the triggers are known to cover every rule.
	if r.bodyTriggers != nil && !r.bodyTriggers.matches(s) {
		return s
	}
	for _, re := range r.bodyRules {
		s = replaceCaptured(re, s)
	}
	return s
}

// replaceCaptured masks capture group 1 of every match, falling back to the
// whole match when a rule has no capture group. Spans are replaced rather than
// deleted so the surrounding message stays readable and the redaction is
// visible to whoever reads the log.
func replaceCaptured(re *regexp.Regexp, s string) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if len(m) >= 4 && m[2] >= 0 {
			start, end = m[2], m[3] // capture group 1
		}
		if start < last {
			continue // overlapping match already covered
		}
		b.WriteString(s[last:start])
		b.WriteString(RedactionMark)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
