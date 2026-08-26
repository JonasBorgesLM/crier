package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewHealthNeedsADispatcher(t *testing.T) {
	_, err := NewHealth(nil)
	if err == nil {
		t.Fatal("health accepted a nil dispatcher; readiness would then reflect nothing")
	}
	if !strings.Contains(err.Error(), "export health") {
		t.Errorf("error = %q, want it to say what readiness is for", err)
	}
}

// Readiness reflects export health; liveness does not. Liveness that fails on
// a backend outage gets the instance killed and restarted into the same
// outage, losing whatever was buffered (ADR-0015).
func TestReadinessReflectsExportHealthAndLivenessDoesNot(t *testing.T) {
	broken := &fakeExporter{export: func(context.Context, []LogRecord) error {
		return errors.New("connection refused")
	}}
	breaker, err := NewCircuitBreaker(CircuitBreakerConfig{
		Name: "primary", Exporter: broken, FailureThreshold: 1, Cooldown: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{{Name: "primary", Exporter: breaker}}})
	d := mustDispatcher(t, DispatcherConfig{
		Buffer:   mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2}),
		Exporter: f,
	})
	health, err := NewHealth(d)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}

	if ready, reason := health.Ready(); !ready {
		t.Fatalf("Ready() = false (%s) before anything failed", reason)
	}

	_ = breaker.Export(context.Background(), testBatch(1))
	if !d.Degraded() {
		t.Fatal("the dispatcher is not degraded with its only circuit open")
	}

	ready, reason := health.Ready()
	if ready {
		t.Error("Ready() = true with every destination refusing calls")
	}
	// An operator seeing not-ready during a backend outage could read it as a
	// crash loop, so the reason has to say otherwise.
	for _, want := range []string{"degraded", "primary", "alive"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to mention %q", reason, want)
		}
	}
	if !health.Live() {
		t.Error("Live() = false during a backend outage; that restarts the instance into the same outage")
	}
}

func TestDrainingIsNotReady(t *testing.T) {
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 8, BatchSize: 2, BatchWindow: 5 * time.Millisecond})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: &fakeExporter{}, Workers: 1})
	health, err := NewHealth(d)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	d.Start(context.Background())

	if ready, reason := health.Ready(); !ready {
		t.Fatalf("Ready() = false (%s) while running normally", reason)
	}

	if _, err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	ready, reason := health.Ready()
	if ready {
		t.Error("Ready() = true while draining; the buffer is closed and nothing further is accepted")
	}
	if !strings.Contains(reason, "draining") {
		t.Errorf("reason = %q, want it to say the instance is draining", reason)
	}
	if !health.Live() {
		t.Error("Live() = false while draining; the process is still doing the drain")
	}
}

// Loss at shutdown is permitted; silent loss is not. The summary is what makes
// it not silent, so its contents are the thing to assert.
func TestDrainSummaryReportsWhatWasLostAndWhereItWasGoing(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	e := &fakeExporter{export: func(context.Context, []LogRecord) error {
		<-block
		return nil
	}}
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{
		{Name: "primary", Exporter: e},
		{Name: "archive", Exporter: &fakeExporter{}},
	}})
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 32, BatchSize: 1, BatchWindow: 5 * time.Millisecond})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: f, Workers: 1})

	d.Start(context.Background())
	enqueue(t, buf, "task-api", 8)
	waitFor(t, "the first batch to be in flight", func() bool { return e.calls.Load() > 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	summary, err := d.Shutdown(ctx)

	if err == nil {
		t.Fatal("Shutdown succeeded, want the expired drain reported")
	}
	if summary.Clean() {
		t.Fatal("Clean() = true, but the drain did not finish")
	}
	if summary.Lost == 0 {
		t.Error("Lost = 0 on an incomplete drain")
	}
	if summary.Duration == 0 {
		t.Error("Duration = 0")
	}

	line := summary.String()
	for _, want := range []string{"drain incomplete", "record(s) lost", "primary", "archive"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q is missing %q — an operator needs to know whether to look at crier or the backend", line, want)
		}
	}
}

func TestCleanDrainSaysSo(t *testing.T) {
	buf := mustMemoryBuffer(t, MemoryBufferConfig{Capacity: 32, BatchSize: 2, BatchWindow: 5 * time.Millisecond})
	f := mustFanOut(t, FanOutConfig{Destinations: []Destination{{Name: "primary", Exporter: &fakeExporter{}}}})
	d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: f, Workers: 2})

	d.Start(context.Background())
	enqueue(t, buf, "task-api", 6)

	summary, err := d.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !summary.Clean() {
		t.Errorf("Clean() = false with %d lost", summary.Lost)
	}
	if got := summary.String(); !strings.Contains(got, "no records lost") {
		t.Errorf("summary = %q", got)
	}
}
