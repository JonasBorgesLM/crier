package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectingExporter keeps what it was sent, so an embedded test can assert
// what reached the far end rather than what was called.
type collectingExporter struct {
	mu      sync.Mutex
	records []LogRecord
	err     error
}

func (c *collectingExporter) Export(_ context.Context, batch []LogRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.records = append(c.records, batch...)
	return nil
}

func (c *collectingExporter) Shutdown(context.Context) error { return nil }

func (c *collectingExporter) exported() []LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LogRecord(nil), c.records...)
}

func newEmbedded(t *testing.T, opts Options) (*Crier, *collectingExporter) {
	t.Helper()
	e := &collectingExporter{}
	if opts.ServiceName == "" {
		opts.ServiceName = "task-api"
	}
	if opts.Exporters == nil {
		opts.Exporters = map[string]Exporter{"primary": e}
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 1
	}
	if opts.BatchWindow == 0 {
		opts.BatchWindow = 5 * time.Millisecond
	}

	crier, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = crier.Shutdown(ctx)
	})
	return crier, e
}

func TestNewValidatesEagerly(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{
			// There is no authenticated principal in embedded mode, so this
			// is the only place identity can come from.
			name: "no service name",
			opts: Options{Exporters: map[string]Exporter{"primary": &collectingExporter{}}},
			want: "ServiceName is required",
		},
		{
			// Accepts records and discards them, reporting success.
			name: "no exporters",
			opts: Options{ServiceName: "task-api"},
			want: "at least one exporter",
		},
		{
			name: "unusable buffer configuration",
			opts: Options{
				ServiceName: "task-api",
				Exporters:   map[string]Exporter{"primary": &collectingExporter{}},
				Capacity:    2, BatchSize: 64,
			},
			want: "batch size",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil {
				t.Fatal("options accepted, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestEmbeddedRecordsReachTheExporter(t *testing.T) {
	crier, exporter := newEmbedded(t, Options{ServiceVersion: "1.4.0"})

	if err := crier.Log(t.Context(), LogRecord{Body: "hello", Severity: SeverityInfo}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	waitFor(t, "the record to be exported", func() bool { return len(exporter.exported()) == 1 })

	rec := exporter.exported()[0]
	if rec.Body != "hello" {
		t.Errorf("Body = %q", rec.Body)
	}
	// Identity is asserted once, at construction, rather than per record.
	if rec.Resource.ServiceName != "task-api" {
		t.Errorf("ServiceName = %q, want the configured one", rec.Resource.ServiceName)
	}
	if rec.Resource.ServiceVersion != "1.4.0" {
		t.Errorf("ServiceVersion = %q", rec.Resource.ServiceVersion)
	}
	// Assigned by the pipeline, never by the caller (ADR-0009).
	if rec.ObservedTimestamp.IsZero() {
		t.Error("ObservedTimestamp was not stamped")
	}
}

// A bug in the host application produces the same unbounded attribute map as a
// malicious client, and the buffer cannot tell them apart (ADR-0010).
func TestInputLimitsApplyToTheEmbeddedPathToo(t *testing.T) {
	var m CountingMetrics
	crier, exporter := newEmbedded(t, Options{
		Limits:  Limits{MaxAttributes: 2, MaxValueBytes: 8},
		Metrics: &m,
	})

	err := crier.Log(t.Context(), LogRecord{
		Body: "runaway",
		Attributes: map[string]any{
			"a": strings.Repeat("x", 512),
			"b": "short",
			"c": "dropped",
			"d": "dropped",
		},
	})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	waitFor(t, "the record to be exported", func() bool { return len(exporter.exported()) == 1 })

	rec := exporter.exported()[0]
	if got := len(rec.Attributes); got > 2 {
		t.Errorf("the record carried %d attributes past a cap of 2", got)
	}
	for key, value := range rec.Attributes {
		if s, ok := value.(string); ok && len(s) > 64 {
			t.Errorf("attribute %q survived at %d bytes past a cap of 8", key, len(s))
		}
	}
	snap := m.Snapshot()
	if len(snap.AttributesDropped) == 0 && len(snap.AttributesTruncated) == 0 {
		t.Error("nothing was counted; a limit that trims silently is a limit nobody can see")
	}
}

// The composition is the reason to have a façade at all: built the other way
// round, one failing destination re-sends the batch to every healthy one.
func TestEmbeddedComposesRetryInsideFanOut(t *testing.T) {
	var m CountingMetrics
	healthy := &collectingExporter{}
	broken := &collectingExporter{err: errors.New("connection refused")}

	crier, _ := newEmbedded(t, Options{
		Exporters:     map[string]Exporter{"healthy": healthy, "broken": broken},
		RetryAttempts: 3,
		Metrics:       &m,
	})

	if err := crier.Log(t.Context(), LogRecord{Body: "one record"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	waitFor(t, "the healthy destination to receive it", func() bool { return len(healthy.exported()) > 0 })
	waitFor(t, "the broken destination to spend its budget", func() bool {
		return m.Snapshot().Retries["broken"] >= 2
	})

	if got := len(healthy.exported()); got != 1 {
		t.Errorf("the healthy destination received %d copies, want exactly 1 — retry outside the fan-out is audit finding A-1", got)
	}
	if got := m.Snapshot().Retries["healthy"]; got != 0 {
		t.Errorf("Retries[healthy] = %d, want 0 — it never failed", got)
	}
}

func TestEmbeddedShutdownReturnsASummary(t *testing.T) {
	crier, exporter := newEmbedded(t, Options{})

	for i := range 5 {
		if err := crier.Log(t.Context(), LogRecord{Body: string(rune('a' + i))}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}

	summary, err := crier.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !summary.Clean() {
		t.Errorf("Clean() = false, %d lost: %s", summary.Lost, summary)
	}
	if got := len(exporter.exported()); got != 5 {
		t.Errorf("exported %d records, want all 5 drained", got)
	}
}

func TestEmbeddedHealthIsAvailableToTheHost(t *testing.T) {
	crier, _ := newEmbedded(t, Options{})

	if ready, reason := crier.Health().Ready(); !ready {
		t.Errorf("Ready() = false (%s) on a healthy engine", reason)
	}
	if !crier.Health().Live() {
		t.Error("Live() = false")
	}
	if got := crier.Depth(); got < 0 {
		t.Errorf("Depth() = %d", got)
	}
}

func TestEmbeddedFilteredRecordIsNotAnError(t *testing.T) {
	var m CountingMetrics
	crier, _ := newEmbedded(t, Options{
		Filter:  &Filter{MinSeverity: SeverityError},
		Metrics: &m,
	})

	if err := crier.Log(t.Context(), LogRecord{Body: "chatter", Severity: SeverityDebug}); err != nil {
		t.Errorf("Log returned %v for a filtered record; it was handled exactly as configured", err)
	}
	if got := m.Snapshot().Filtered["task-api"]; got != 1 {
		t.Errorf("Filtered = %d, want 1", got)
	}
	if got := m.Snapshot().TotalDropped(); got != 0 {
		t.Errorf("TotalDropped = %d, want 0 — filtering is not loss", got)
	}
}

// LogBatch's own contract: every record admitted, the count returned, and the
// service version stamped on each.
//
// Whether admission continues past a failing record is the pipeline's
// behaviour and is tested there, deterministically
// (TestAdmitBatchContinuesPastAFailure). Reproducing it here meant filling a
// buffer that a running dispatcher is concurrently draining, which is a race
// dressed as a test: it passed most of the time and failed when the exporter
// was quick.
func TestEmbeddedLogBatchAdmitsEveryRecord(t *testing.T) {
	crier, exporter := newEmbedded(t, Options{ServiceVersion: "2.0.0"})

	batch := make([]LogRecord, 6)
	for i := range batch {
		batch[i] = LogRecord{Body: fmt.Sprintf("record %d", i), Severity: SeverityInfo}
	}

	accepted, err := crier.LogBatch(t.Context(), batch)
	if err != nil {
		t.Fatalf("LogBatch: %v", err)
	}
	if accepted != len(batch) {
		t.Fatalf("accepted = %d, want all %d", accepted, len(batch))
	}

	waitFor(t, "every record to be exported", func() bool { return len(exporter.exported()) == len(batch) })
	for _, rec := range exporter.exported() {
		if rec.Resource.ServiceName != "task-api" {
			t.Errorf("ServiceName = %q", rec.Resource.ServiceName)
		}
		if rec.Resource.ServiceVersion != "2.0.0" {
			t.Errorf("ServiceVersion = %q, want it stamped on every record", rec.Resource.ServiceVersion)
		}
	}
}
