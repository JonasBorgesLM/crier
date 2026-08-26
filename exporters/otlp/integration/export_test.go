//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JonasBorgesLM/crier/core"
	"github.com/JonasBorgesLM/crier/exporters/otlp"
)

// marker returns a string unique to one test run, so a record found in the
// collector's log is unambiguously the one this test sent.
func marker(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("crier-it-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
}

func records(body string, n int) []core.LogRecord {
	batch := make([]core.LogRecord, n)
	now := time.Now()
	for i := range batch {
		batch[i] = core.LogRecord{
			Timestamp:         now.Add(-time.Second),
			ObservedTimestamp: now,
			Severity:          core.SeverityError,
			SeverityText:      "ERROR",
			Body:              fmt.Sprintf("%s #%d", body, i),
			Attributes:        map[string]any{"attempt": i, "region": "eu-west-1"},
			Resource:          core.Resource{ServiceName: "task-api", ServiceVersion: "1.4.0"},
			TraceID:           "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:            "00f067aa0ba902b7",
		}
	}
	return batch
}

// The payload crier considers well-formed has to be one a real collector
// parses. A handwritten test server agrees with whatever we send it; this is
// the only thing that can disagree.
func TestExportIsAcceptedByARealCollector(t *testing.T) {
	for _, compression := range []otlp.Compression{otlp.CompressionGzip, otlp.CompressionNone} {
		t.Run(string(compression), func(t *testing.T) {
			c := startCollector(t)
			body := marker(t)

			exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint, Compression: compression})
			if err != nil {
				t.Fatalf("otlp.New: %v", err)
			}
			t.Cleanup(func() { _ = exporter.Shutdown(context.Background()) })

			if err := exporter.Export(context.Background(), records(body, 3)); err != nil {
				t.Fatalf("Export: %v", err)
			}

			c.waitForLog(body + " #2")
			// The mapping is only right if the fields arrive as themselves:
			// the resource identity, the severity, and the trace correlation.
			logs := c.logs()
			for _, want := range []string{
				"task-api",
				"1.4.0",
				"ERROR",
				"4bf92f3577b34da6a3ce929d0e0e4736",
				"eu-west-1",
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("the collector never received %q", want)
				}
			}
		})
	}
}

// A record whose secret was masked by the pipeline must reach the backend
// masked. This is the assertion the whole project exists for, and it is the
// one that can only be made end to end (ADR-0014).
//
// Read the scope of that claim carefully. ADR-0014 declares body redaction
// best-effort and pattern-based: what this test proves is that two values
// chosen to match the default rules — an AWS key ID in free text, and an
// attribute under a key that reads as sensitive — survive the whole path
// masked. It is evidence that redaction is wired in and is not bypassed
// between the pipeline and the wire. It is not evidence of coverage, and a
// green run here says nothing about a credential shaped like something the
// patterns do not match.
//
// Coverage of the rules themselves belongs to the unit suite in core, where
// a case can be added for the cost of a table row. The reason attributes are
// preferred over interpolated message text is exactly this asymmetry: an
// attribute is redacted by its key, which is reliable, while free text is
// redacted by shape, which is a heuristic.
func TestRedactedRecordsReachTheCollectorMasked(t *testing.T) {
	c := startCollector(t)
	body := marker(t)
	const secret = "AKIAIOSFODNN7EXAMPLE"

	redactor, err := core.NewRedactor(core.RedactionConfig{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	buffer, err := core.NewMemoryBuffer(core.MemoryBufferConfig{
		Capacity: 64, BatchSize: 2, BatchWindow: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	pipeline, err := core.NewPipeline(core.PipelineConfig{Buffer: buffer, Redactor: redactor})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}
	fanOut, err := core.NewFanOut(core.FanOutConfig{
		Destinations: []core.Destination{{Name: "primary", Exporter: exporter}},
	})
	if err != nil {
		t.Fatalf("NewFanOut: %v", err)
	}
	dispatcher, err := core.NewDispatcher(core.DispatcherConfig{Buffer: buffer, Exporter: fanOut, Workers: 2})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dispatcher.Start(context.Background())

	rec := core.LogRecord{
		Severity: core.SeverityError,
		Body:     fmt.Sprintf("%s deploy failed with %s", body, secret),
		Attributes: map[string]any{
			"api_key": "hunter2-should-never-leave",
			"region":  "eu-west-1",
		},
	}
	if err := pipeline.Admit(context.Background(), rec, "task-api"); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	c.waitForLog(body)
	logs := c.logs()
	// Absence for these two values only — see the note on the test. Widening
	// this list is not the same as widening what redaction catches.
	for _, leak := range []string{secret, "hunter2-should-never-leave"} {
		if strings.Contains(logs, leak) {
			t.Errorf("%q reached the backend unmasked", leak)
		}
	}
	if !strings.Contains(logs, core.RedactionMark) {
		t.Error("the record arrived without the redaction marker; nothing was masked at all")
	}
	// The line still has to say what happened, or redaction has traded one
	// problem for another.
	if !strings.Contains(logs, "deploy failed with") {
		t.Error("redaction removed the context as well as the credential")
	}
}

// The classification table is only worth anything if a real collector's own
// status codes land on the right side of it (ADR-0017).
func TestRealCollectorRejectionsAreClassifiedPermanent(t *testing.T) {
	c := startCollector(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		// A logs payload posted to the metrics route: the collector parses it
		// as metrics, fails, and answers 400.
		{"logs payload on the metrics route", "/v1/metrics"},
		// A route the collector does not serve at all.
		{"unknown route", "/v1/nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint, Path: tc.path})
			if err != nil {
				t.Fatalf("otlp.New: %v", err)
			}
			t.Cleanup(func() { _ = exporter.Shutdown(context.Background()) })

			err = exporter.Export(context.Background(), records(marker(t), 1))
			if err == nil {
				t.Fatal("Export succeeded, want the collector's rejection")
			}
			if !core.IsPermanent(err) {
				t.Errorf("IsPermanent = false for %v; retrying this spends the budget on a batch "+
					"the collector will never accept", err)
			}
		})
	}
}

// Nothing reached the backend, so trying again is exactly right — and the
// retry decorator must actually do it.
func TestCollectorUnavailableIsRetryable(t *testing.T) {
	c := startCollector(t)
	c.stop()

	var m core.CountingMetrics
	exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}
	retry, err := core.NewRetry(core.RetryConfig{
		Name: "primary", Exporter: exporter, MaxAttempts: 3,
		InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}

	err = retry.Export(context.Background(), records(marker(t), 1))
	if err == nil {
		t.Fatal("Export succeeded against a stopped collector")
	}
	if core.IsPermanent(err) {
		t.Errorf("IsPermanent = true for %v, want retryable", err)
	}
	if got := m.Snapshot().Retries["primary"]; got != 2 {
		t.Errorf("Retries = %d, want 2 — the budget was not spent", got)
	}
}

// The end state ADR-0015 describes: a destination that stays down stops being
// dialled, readiness reflects it, and the records that are lost are counted.
func TestCircuitOpensAndThePipelineReportsDegraded(t *testing.T) {
	c := startCollector(t)
	c.stop()

	var m core.CountingMetrics
	exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}
	breaker, err := core.NewCircuitBreaker(core.CircuitBreakerConfig{
		Name: "primary", Exporter: exporter, FailureThreshold: 2, Cooldown: time.Hour, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	retry, err := core.NewRetry(core.RetryConfig{
		Name: "primary", Exporter: breaker, MaxAttempts: 2,
		InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewRetry: %v", err)
	}
	fanOut, err := core.NewFanOut(core.FanOutConfig{
		Destinations: []core.Destination{{Name: "primary", Exporter: retry}},
		Timeout:      10 * time.Second,
		Metrics:      &m,
	})
	if err != nil {
		t.Fatalf("NewFanOut: %v", err)
	}
	buffer, err := core.NewMemoryBuffer(core.MemoryBufferConfig{
		Capacity: 64, BatchSize: 1, BatchWindow: 50 * time.Millisecond, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	dispatcher, err := core.NewDispatcher(core.DispatcherConfig{
		Buffer: buffer, Exporter: fanOut, Workers: 1, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if dispatcher.Degraded() {
		t.Fatal("Degraded() = true before anything was exported")
	}

	dispatcher.Start(context.Background())
	for _, rec := range records(marker(t), 6) {
		if err := buffer.Enqueue(context.Background(), rec); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !dispatcher.Degraded() {
		t.Error("Degraded() = false with the only destination's circuit open")
	}
	if got := dispatcher.OpenCircuits(); len(got) != 1 || got[0] != "primary" {
		t.Errorf("OpenCircuits() = %v, want [primary]", got)
	}

	snap := m.Snapshot()
	if !snap.OpenCircuits["primary"] {
		t.Error("the circuit metric never reported the destination opening")
	}
	if !snap.Degraded {
		t.Error("the degraded metric was never reported")
	}
	// Every record is accounted for. Loss is permitted; unaccounted loss is
	// not (ADR-0015).
	if got := snap.DroppedBy(core.DropBackendUnavailable); got != 6 {
		t.Errorf("DroppedBy(backend_unavailable) = %d, want all 6 records counted", got)
	}
	if got := snap.Exported["primary"]; got != 0 {
		t.Errorf("Exported[primary] = %d, want 0", got)
	}
	// And once the circuit is open, the destination stops being dialled: the
	// later batches must not each have spent the full retry budget.
	if got := snap.Retries["primary"]; got > 2 {
		t.Errorf("Retries = %d, want at most 2 — an open circuit must not be retried", got)
	}
}

// A destination that recovers is used again without anyone restarting crier.
func TestCircuitClosesAfterTheCollectorComesBack(t *testing.T) {
	c := startRestartableCollector(t)
	body := marker(t)

	var m core.CountingMetrics
	exporter, err := otlp.New(otlp.Config{Endpoint: c.endpoint, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}
	breaker, err := core.NewCircuitBreaker(core.CircuitBreakerConfig{
		Name: "primary", Exporter: exporter, FailureThreshold: 1,
		Cooldown: 100 * time.Millisecond, Metrics: &m,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}

	// Down: the circuit opens.
	c.stop()
	if err := breaker.Export(context.Background(), records(body, 1)); err == nil {
		t.Fatal("Export succeeded against a stopped collector")
	}
	if !breaker.Open() {
		t.Fatalf("State() = %v after a failure with a threshold of 1, want open", breaker.State())
	}
	if err := breaker.Export(context.Background(), records(body, 1)); !errors.Is(err, core.ErrCircuitOpen) {
		t.Errorf("error = %v, want ErrCircuitOpen", err)
	}

	// Back up: the cooldown expires, the probe succeeds, and it closes.
	c.start()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := breaker.Export(context.Background(), records(body, 1)); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if breaker.Open() {
		t.Errorf("State() = %v after the collector came back, want closed", breaker.State())
	}
	c.waitForLog(body)
}
