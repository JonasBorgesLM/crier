package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// discardBuffer accepts everything at no cost, so a benchmark measures the
// pipeline rather than the buffer's ring arithmetic.
type discardBuffer struct{}

func (discardBuffer) Enqueue(context.Context, LogRecord) error { return nil }
func (discardBuffer) DequeueBatch(context.Context) ([]LogRecord, error) {
	return nil, ErrBufferClosed
}
func (discardBuffer) Depth() int    { return 0 }
func (discardBuffer) Close() error  { return nil }
func (discardBuffer) Capacity() int { return 1 << 20 }

// benchRecord is deliberately ordinary: a realistic message with a credential
// in it, a handful of attributes, one high-cardinality field. Benchmarking a
// record with no attributes and a short body would publish a number nobody
// experiences, the same objection ADR-0010 raises about benchmarking the bare
// path.
func benchRecord(i int) LogRecord {
	return LogRecord{
		Timestamp: time.Now(),
		Severity:  SeverityInfo,
		Body: "POST /v1/orders 401 in 12ms: upstream rejected credentials, " +
			"Authorization: Bearer aGVsbG8td29ybGQtc2VjcmV0LXZhbHVl",
		Attributes: map[string]any{
			"http.method":      "POST",
			"http.route":       "/v1/orders",
			"http.status_code": 401,
			"request.id":       fmt.Sprintf("req-%d", i),
			"user.id":          "u-4821",
			"duration_ms":      12.4,
		},
		Resource: Resource{
			ServiceName:    "claimed-by-client",
			ServiceVersion: "1.4.2",
			Attributes:     map[string]any{"deployment.environment": "production"},
		},
	}
}

func benchPipeline(b *testing.B, cfg PipelineConfig) *Pipeline {
	b.Helper()
	cfg.Buffer = discardBuffer{}
	p, err := NewPipeline(cfg)
	if err != nil {
		b.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func fullConfig(b *testing.B) PipelineConfig {
	b.Helper()
	r, err := NewRedactor(RedactionConfig{})
	if err != nil {
		b.Fatalf("NewRedactor: %v", err)
	}
	return PipelineConfig{
		Limits:      Limits{},
		Cardinality: &CardinalityGuard{},
		Redactor:    r,
		Filter:      &Filter{MinSeverity: SeverityDebug, SampleRate: 1},
		Metrics:     &CountingMetrics{},
	}
}

// The number to publish: every stage enabled, as the pipeline actually runs.
func BenchmarkPipelineAllStages(b *testing.B) {
	p := benchPipeline(b, fullConfig(b))
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := p.Admit(ctx, benchRecord(i), "task-api"); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}

// The common case in production: a log line with no credential in it. Both
// numbers matter — the matching case above is the worst case, and publishing
// only one of them would misrepresent the pipeline in one direction or the
// other.
func BenchmarkPipelineAllStagesCleanBody(b *testing.B) {
	p := benchPipeline(b, fullConfig(b))
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		rec := benchRecord(i)
		rec.Body = "POST /v1/orders 201 in 12ms: created order 4821"
		if err := p.Admit(ctx, rec, "task-api"); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}

// Body redaction is expected to dominate (ADR-0014). Measuring with it off
// makes the cost attributable rather than merely suspected.
func BenchmarkPipelineWithoutBodyRedaction(b *testing.B) {
	r, err := NewRedactor(RedactionConfig{SkipBody: true})
	if err != nil {
		b.Fatalf("NewRedactor: %v", err)
	}
	cfg := fullConfig(b)
	cfg.Redactor = r
	p := benchPipeline(b, cfg)
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := p.Admit(ctx, benchRecord(i), "task-api"); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}

func BenchmarkPipelineWithoutRedaction(b *testing.B) {
	cfg := fullConfig(b)
	cfg.Redactor = nil
	p := benchPipeline(b, cfg)
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := p.Admit(ctx, benchRecord(i), "task-api"); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}

// The bare path, for reference only. Publishing this alone would be the
// misleading number ADR-0010 warns about.
func BenchmarkPipelineBare(b *testing.B) {
	p := benchPipeline(b, PipelineConfig{})
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := p.Admit(ctx, benchRecord(i), "task-api"); err != nil {
			b.Fatalf("Admit: %v", err)
		}
	}
}

func BenchmarkRedactBodyOnly(b *testing.B) {
	r, err := NewRedactor(RedactionConfig{})
	if err != nil {
		b.Fatalf("NewRedactor: %v", err)
	}
	body := benchRecord(0).Body

	b.ReportAllocs()
	for b.Loop() {
		_ = r.RedactString(body)
	}
}

// A body with nothing to mask still pays for every pattern to be evaluated,
// and that is the common case in production.
func BenchmarkRedactBodyNoMatch(b *testing.B) {
	r, err := NewRedactor(RedactionConfig{})
	if err != nil {
		b.Fatalf("NewRedactor: %v", err)
	}
	body := "GET /v1/health 200 in 1ms: ok"

	b.ReportAllocs()
	for b.Loop() {
		_ = r.RedactString(body)
	}
}

func BenchmarkLimitsApply(b *testing.B) {
	l := Limits{Metrics: &CountingMetrics{}}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		rec := benchRecord(i)
		l.Apply(&rec)
	}
}

func BenchmarkCardinalityGuard(b *testing.B) {
	g := &CardinalityGuard{}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		rec := benchRecord(i)
		g.Apply(&rec)
	}
}

func BenchmarkFairShareAdmission(b *testing.B) {
	inner, err := NewMemoryBuffer(MemoryBufferConfig{Capacity: 1 << 16, BatchSize: 1024})
	if err != nil {
		b.Fatalf("NewMemoryBuffer: %v", err)
	}
	f, err := NewFairShareBuffer(inner, FairShareConfig{
		Reservations: map[string]int{"task-api": 1024}, UnlistedPool: 1024,
	})
	if err != nil {
		b.Fatalf("NewFairShareBuffer: %v", err)
	}
	ctx := context.Background()
	rec := recFrom("task-api", "x")

	b.ReportAllocs()
	for b.Loop() {
		if err := f.Enqueue(ctx, rec); err != nil {
			// Drain and carry on; measuring admission, not backpressure.
			if _, dErr := f.DequeueBatch(ctx); dErr != nil {
				b.Fatalf("DequeueBatch: %v", dErr)
			}
		}
	}
}

// Contended throughput, since a receiver runs many goroutines against one
// pipeline.
func BenchmarkPipelineAllStagesParallel(b *testing.B) {
	p := benchPipeline(b, fullConfig(b))
	ctx := context.Background()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			i++
			if err := p.Admit(ctx, benchRecord(i), "task-api"); err != nil {
				b.Fatalf("Admit: %v", err)
			}
		}
	})
}
