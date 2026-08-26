package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// spyBuffer records everything that reaches step 8, so stage ordering can be
// asserted by what did and did not arrive.
type spyBuffer struct {
	mu       sync.Mutex
	admitted []LogRecord
	failWith error
}

func (s *spyBuffer) Enqueue(_ context.Context, rec LogRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.admitted = append(s.admitted, rec)
	return nil
}

func (s *spyBuffer) DequeueBatch(context.Context) ([]LogRecord, error) { return nil, ErrBufferClosed }

func (s *spyBuffer) Depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.admitted)
}

func (s *spyBuffer) Close() error { return nil }

func (s *spyBuffer) records() []LogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogRecord(nil), s.admitted...)
}

func newPipeline(t *testing.T, cfg PipelineConfig) (*Pipeline, *spyBuffer) {
	t.Helper()
	spy := &spyBuffer{}
	cfg.Buffer = spy
	p, err := NewPipeline(cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, spy
}

// The acceptance criterion for #12: ordering is observable, because a record
// the filter discards never reaches the buffer at all.
func TestFilteredRecordsNeverReachTheBuffer(t *testing.T) {
	var m CountingMetrics
	p, spy := newPipeline(t, PipelineConfig{
		Filter:  &Filter{MinSeverity: SeverityError},
		Metrics: &m,
	})
	ctx := context.Background()

	for _, sev := range []Severity{SeverityDebug, SeverityInfo, SeverityWarn} {
		if err := p.Admit(ctx, LogRecord{Severity: sev, Body: "noise"}, "task-api"); err != nil {
			t.Fatalf("Admit: %v", err)
		}
	}
	if err := p.Admit(ctx, LogRecord{Severity: SeverityError, Body: "real"}, "task-api"); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	got := spy.records()
	if len(got) != 1 {
		t.Fatalf("buffer holds %d records, want 1 — filtering is running after admission", len(got))
	}
	if got[0].Body != "real" {
		t.Errorf("buffer holds %q, want the ERROR record", got[0].Body)
	}
	// Bounded memory must only ever be spent on records that will be exported.
	if n := m.Snapshot().Filtered["task-api"]; n != 3 {
		t.Errorf("Filtered = %d, want 3", n)
	}
}

// Redaction must run before the buffer, or a durable buffer would hold
// unmasked values on disk (ADR-0006).
func TestRedactionHappensBeforeAdmission(t *testing.T) {
	r, err := NewRedactor(RedactionConfig{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	p, spy := newPipeline(t, PipelineConfig{Redactor: r})

	err = p.Admit(context.Background(), LogRecord{
		Body:       "auth failed token=s3cr3t-value-here",
		Attributes: map[string]any{"password": "hunter2"},
	}, "task-api")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	got := spy.records()[0]
	if strings.Contains(got.Body, "s3cr3t-value-here") {
		t.Errorf("unmasked body reached the buffer: %q", got.Body)
	}
	if got.Attributes["password"] != RedactionMark {
		t.Errorf("unmasked attribute reached the buffer: %v", got.Attributes["password"])
	}
}

// Fail-closed all the way through: a record that cannot be redacted must not
// be buffered, whatever else happens.
func TestRedactionFailureStopsTheRecordReachingTheBuffer(t *testing.T) {
	var m CountingMetrics
	r, err := NewRedactor(RedactionConfig{Metrics: &m})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	r.failHook = func() error { return errors.New("engine down") }

	p, spy := newPipeline(t, PipelineConfig{Redactor: r, Metrics: &m})

	err = p.Admit(context.Background(), LogRecord{Body: "token=s3cr3t-value-here"}, "task-api")
	if !errors.Is(err, ErrRedactionFailed) {
		t.Fatalf("Admit = %v, want ErrRedactionFailed", err)
	}
	if n := len(spy.records()); n != 0 {
		t.Errorf("buffer holds %d records after a redaction failure, want 0", n)
	}
	if n := m.Snapshot().DroppedBy(DropRedactionFailed); n != 1 {
		t.Errorf("DroppedBy(redaction_failed) = %d, want 1", n)
	}
}

// Limits must run before redaction, so redaction never scans a value that was
// about to be discarded anyway, and before admission so oversized records
// never occupy the buffer.
func TestLimitsRunBeforeAdmission(t *testing.T) {
	p, spy := newPipeline(t, PipelineConfig{
		Limits: Limits{MaxBodyBytes: 32, MaxValueBytes: 16},
	})

	err := p.Admit(context.Background(), LogRecord{
		Body:       strings.Repeat("x", 10_000),
		Attributes: map[string]any{"blob": strings.Repeat("y", 10_000)},
	}, "task-api")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	got := spy.records()[0]
	if len(got.Body) > 32 {
		t.Errorf("buffered body is %d bytes, want at most 32", len(got.Body))
	}
	if v := got.Attributes["blob"].(string); len(v) > 16 {
		t.Errorf("buffered attribute is %d bytes, want at most 16", len(v))
	}
}

// ADR-0008, finding D-2: the identity used for quotas, sampling, and
// attribution comes from the authenticated principal, never the payload.
func TestClientAssertedIdentityIsOverwrittenAndCounted(t *testing.T) {
	var m CountingMetrics
	p, spy := newPipeline(t, PipelineConfig{Metrics: &m})

	err := p.Admit(context.Background(), LogRecord{
		Body: "audit event",
		Resource: Resource{
			ServiceName: "gateway-auth", // the claim
			Attributes:  map[string]any{"region": "eu-west-1"},
		},
	}, "task-api") // the authenticated principal
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	got := spy.records()[0]
	if got.Resource.ServiceName != "task-api" {
		t.Errorf("ServiceName = %q, want the authenticated principal", got.Resource.ServiceName)
	}
	// Descriptive attributes outside the identity fields survive.
	if got.Resource.Attributes["region"] != "eu-west-1" {
		t.Errorf("descriptive resource attribute was lost: %v", got.Resource.Attributes)
	}
	if n := m.Snapshot().IdentityDiscrepancies["task-api"]; n != 1 {
		t.Errorf("IdentityDiscrepancies = %d, want 1 — a forged claim must be visible", n)
	}
}

func TestMatchingIdentityIsNotCountedAsADiscrepancy(t *testing.T) {
	var m CountingMetrics
	p, _ := newPipeline(t, PipelineConfig{Metrics: &m})

	err := p.Admit(context.Background(),
		LogRecord{Resource: Resource{ServiceName: "task-api"}}, "task-api")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if n := len(m.Snapshot().IdentityDiscrepancies); n != 0 {
		t.Errorf("a matching claim was counted as a discrepancy")
	}
}

func TestEmptySourceLeavesTheRecordAlone(t *testing.T) {
	// Embedded-library use: the host application is the trust boundary, and
	// there is no principal to attest.
	p, spy := newPipeline(t, PipelineConfig{})

	err := p.Admit(context.Background(),
		LogRecord{Resource: Resource{ServiceName: "host-app"}}, "")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if got := spy.records()[0].Resource.ServiceName; got != "host-app" {
		t.Errorf("ServiceName = %q, want it preserved when there is no principal", got)
	}
}

func TestObservedTimestampIsStampedBeforeAdmission(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	p, spy := newPipeline(t, PipelineConfig{
		Normalizer: &Normalizer{Now: fixedClock(observed)},
	})

	if err := p.Admit(context.Background(), LogRecord{Body: "x"}, "task-api"); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if got := spy.records()[0].ObservedTimestamp; !got.Equal(observed) {
		t.Errorf("ObservedTimestamp = %v, want %v", got, observed)
	}
}

// Every stage in order, on one record.
func TestFullStageOrder(t *testing.T) {
	observed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var m CountingMetrics
	r, err := NewRedactor(RedactionConfig{Metrics: &m})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	p, spy := newPipeline(t, PipelineConfig{
		Normalizer:  &Normalizer{Now: fixedClock(observed), Metrics: &m},
		Limits:      Limits{MaxValueBytes: 64, Metrics: &m},
		Cardinality: &CardinalityGuard{MaxDistinctValues: 2, Metrics: &m},
		Redactor:    r,
		Filter:      &Filter{MinSeverity: SeverityInfo, Metrics: &m},
		Metrics:     &m,
	})
	ctx := context.Background()

	for i := range 5 {
		err := p.Admit(ctx, LogRecord{
			Severity: SeverityWarn,
			Body:     "request failed, token=s3cr3t-value-here",
			Attributes: map[string]any{
				"request.id": string(rune('a'+i)) + strings.Repeat("u", 100),
				"password":   "hunter2",
			},
			Resource: Resource{ServiceName: "pretending"},
		}, "task-api")
		if err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	// Below the threshold: filtered, never buffered.
	if err := p.Admit(ctx, LogRecord{Severity: SeverityDebug}, "task-api"); err != nil {
		t.Fatalf("Admit debug: %v", err)
	}

	got := spy.records()
	if len(got) != 5 {
		t.Fatalf("buffer holds %d records, want 5", len(got))
	}
	last := got[4]

	if last.Resource.ServiceName != "task-api" {
		t.Errorf("identity: %q, want task-api", last.Resource.ServiceName)
	}
	if !last.ObservedTimestamp.Equal(observed) {
		t.Errorf("ObservedTimestamp = %v, want %v", last.ObservedTimestamp, observed)
	}
	if strings.Contains(last.Body, "s3cr3t-value-here") {
		t.Errorf("body was not redacted: %q", last.Body)
	}
	if last.Attributes["password"] != RedactionMark {
		t.Errorf("attribute was not redacted: %v", last.Attributes["password"])
	}
	if last.Attributes["request.id"] != DefaultCardinalityMark {
		t.Errorf("request.id = %v, want the cardinality marker", last.Attributes["request.id"])
	}

	snap := m.Snapshot()
	if snap.Ingested["task-api"] != 6 {
		t.Errorf("Ingested = %d, want 6", snap.Ingested["task-api"])
	}
	if snap.Filtered["task-api"] != 1 {
		t.Errorf("Filtered = %d, want 1", snap.Filtered["task-api"])
	}
	if snap.TotalDropped() != 0 {
		t.Errorf("TotalDropped() = %d, want 0", snap.TotalDropped())
	}
	// Five records claimed an identity; the DEBUG one asserted none, and an
	// absent claim is not a discrepancy.
	if snap.IdentityDiscrepancies["task-api"] != 5 {
		t.Errorf("IdentityDiscrepancies = %d, want 5", snap.IdentityDiscrepancies["task-api"])
	}
}

// One throttled record must not cost the rest of the request.
func TestAdmitBatchContinuesPastAFailure(t *testing.T) {
	spy := &spyBuffer{failWith: ErrSourceQuotaExhausted}
	p, err := NewPipeline(PipelineConfig{Buffer: spy})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	batch := make([]LogRecord, 5)
	admitted, firstErr := p.AdmitBatch(context.Background(), batch, "task-api")

	if admitted != 0 {
		t.Errorf("admitted = %d, want 0", admitted)
	}
	if !errors.Is(firstErr, ErrSourceQuotaExhausted) {
		t.Errorf("firstErr = %v, want ErrSourceQuotaExhausted", firstErr)
	}

	spy.failWith = nil
	admitted, firstErr = p.AdmitBatch(context.Background(), batch, "task-api")
	if admitted != 5 || firstErr != nil {
		t.Errorf("admitted %d with err %v, want 5 and nil", admitted, firstErr)
	}
}

func TestAdmitBatchStopsOnCancelledContext(t *testing.T) {
	p, spy := newPipeline(t, PipelineConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	admitted, err := p.AdmitBatch(ctx, make([]LogRecord, 100), "task-api")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if admitted > 1 {
		t.Errorf("admitted %d records after cancellation, want at most 1", admitted)
	}
	if n := len(spy.records()); n > 1 {
		t.Errorf("buffered %d records after cancellation", n)
	}
}

func TestNewPipelineValidatesEagerly(t *testing.T) {
	if _, err := NewPipeline(PipelineConfig{}); err == nil {
		t.Error("a pipeline with no buffer was accepted")
	}
	_, err := NewPipeline(PipelineConfig{
		Buffer: &spyBuffer{},
		Filter: &Filter{SampleRate: 5},
	})
	if err == nil {
		t.Fatal("an invalid filter was accepted")
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("error %q does not say which stage was misconfigured", err)
	}
}

// End to end against the real buffer, not the spy.
func TestPipelineWithFairShareAndRealBuffer(t *testing.T) {
	var m CountingMetrics
	inner := newBuffer(t, MemoryBufferConfig{
		Capacity: 20, BatchSize: 20, BatchWindow: 10 * time.Millisecond, Metrics: &m,
	})
	fair, err := NewFairShareBuffer(inner, FairShareConfig{
		Reservations: map[string]int{"quiet": 5}, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewFairShareBuffer: %v", err)
	}
	p, err := NewPipeline(PipelineConfig{Buffer: fair, Metrics: &m})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	ctx := context.Background()

	for range 100 {
		_ = p.Admit(ctx, LogRecord{Body: "flood"}, "noisy")
	}
	for i := range 5 {
		if err := p.Admit(ctx, LogRecord{Body: "important"}, "quiet"); err != nil {
			t.Fatalf("quiet source rejected at %d: %v", i, err)
		}
	}

	if got := m.Snapshot().DroppedBy(DropSourceQuota); got == 0 {
		t.Error("the flooding source was never throttled")
	}
	if got := fair.Usage("quiet"); got != 5 {
		t.Errorf("quiet usage = %d, want its full reservation of 5", got)
	}
}
