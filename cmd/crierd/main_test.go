package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", l.Addr())
	}
	return addr.Port
}

func workingConfig() Config {
	return Config{
		Listen:       "127.0.0.1:0",
		AdminListen:  "127.0.0.1:0",
		DrainTimeout: Duration(5 * time.Second),
		Auth:         AuthConfig{Credentials: map[string]string{"task-api": "ingest-token"}},
		Exporters: []ExporterConfig{
			{Name: "primary", Endpoint: "https://collector.example.com:4318"},
		},
	}
}

// The invariant is reservations + UnlistedPool <= capacity, not "reservations
// <= capacity". Over-committing does not crash — quota accounting believes
// there is room while the store rejects with ErrBufferFull, so capacity
// pressure reads as a healthy buffer (ADR-0019).
func TestFairShareOverCommitmentIsRefusedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capacity     int
		reservations map[string]int
		pool         int
		wantErr      bool
	}{
		{
			// The exact shape from #27: reservations alone fit, the pool
			// pushes the total over.
			name:     "reservations fit but the pool pushes it over",
			capacity: 100, reservations: map[string]int{"a": 50, "b": 40}, pool: 20,
			wantErr: true,
		},
		{
			name:     "reservations alone exceed capacity",
			capacity: 100, reservations: map[string]int{"a": 80, "b": 40}, pool: 0,
			wantErr: true,
		},
		{
			name:     "exactly at capacity",
			capacity: 100, reservations: map[string]int{"a": 50, "b": 30}, pool: 20,
			wantErr: false,
		},
		{
			name:     "room to spare",
			capacity: 100, reservations: map[string]int{"a": 10}, pool: 10,
			wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workingConfig()
			cfg.Buffer.Capacity = tc.capacity
			cfg.Buffer.Reservations = tc.reservations
			cfg.Buffer.UnlistedPool = tc.pool

			_, err := build(cfg, discardLogger())

			if tc.wantErr && err == nil {
				total := tc.pool
				for _, n := range tc.reservations {
					total += n
				}
				t.Fatalf("a configuration committing %d of %d started; over-commitment fails silently, "+
					"which is why it has to fail loudly here", total, tc.capacity)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a valid configuration was refused: %v", err)
			}
		})
	}
}

// Everything that can be wrong is wrong before a record is accepted (NFR4).
func TestBuildRefusesConfigurationThatCannotBeRight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "no exporters",
			mutate: func(c *Config) { c.Exporters = nil },
			want:   "no exporters configured",
		},
		{
			name:   "no ingestion credentials",
			mutate: func(c *Config) { c.Auth.Credentials = nil },
			want:   "reject every request",
		},
		{
			// ADR-0014: a rule that does not compile must not be discovered
			// after the first unmasked record has been exported.
			name:   "redaction pattern that will not compile",
			mutate: func(c *Config) { c.Redaction.BodyPatterns = []string{"(unclosed"} },
			want:   "redaction",
		},
		{
			// ADR-0008 and moat's M-2.
			name: "trusted proxy covering the default route",
			mutate: func(c *Config) {
				c.Auth.TrustedProxy = &TrustedProxyConfig{TrustedCIDRs: []string{"0.0.0.0/0"}}
			},
			want: "trust every peer",
		},
		{
			name: "credential over plaintext",
			mutate: func(c *Config) {
				c.Exporters[0].Endpoint = "http://collector.example.com:4318"
				c.Exporters[0].Credential = "a-token"
			},
			want: "refusing to send a credential",
		},
		{
			name: "duplicate exporter names",
			mutate: func(c *Config) {
				c.Exporters = append(c.Exporters, ExporterConfig{Name: "primary", Endpoint: "https://other:4318"})
			},
			want: "configured twice",
		},
		{
			name:   "unknown drop policy",
			mutate: func(c *Config) { c.Buffer.Policy = "discard-everything" },
			want:   "drop policy",
		},
		{
			// A destination's own Filter is validated the same as every other
			// per-destination setting: at startup, not on the first batch
			// that would have hit it (issue #45).
			name:   "invalid destination filter",
			mutate: func(c *Config) { c.Exporters[0].Filter = FilterConfig{SampleRate: 2} },
			want:   "filter",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workingConfig()
			tc.mutate(&cfg)

			_, err := build(cfg, discardLogger())
			if err == nil {
				t.Fatal("the configuration started, want a refusal at startup")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// recordingExporter is a minimal core.Exporter double: buildFanOut's own
// logic (composing Retry/CircuitBreaker, attaching a Filter) is what these
// tests exercise, not an exporter's delivery behaviour, which core.FanOut's
// own tests already cover.
type recordingExporter struct {
	mu      sync.Mutex
	batches [][]core.LogRecord
}

func (r *recordingExporter) Export(_ context.Context, batch []core.LogRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, batch)
	return nil
}

func (r *recordingExporter) Shutdown(context.Context) error { return nil }

func (r *recordingExporter) lastBatch() []core.LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.batches) == 0 {
		return nil
	}
	return r.batches[len(r.batches)-1]
}

// Issue #45: an exporter's own FilterConfig must reach core.Destination.Filter,
// so that destination alone narrows what it receives.
func TestBuildFanOutAttachesPerDestinationFilter(t *testing.T) {
	picky := &recordingExporter{}
	open := &recordingExporter{}
	configs := []ExporterConfig{
		{Name: "picky", Filter: FilterConfig{MinSeverity: int(core.SeverityWarn)}},
		{Name: "open"},
	}

	fanOut, err := buildFanOut(configs, map[string]core.Exporter{"picky": picky, "open": open}, nil)
	if err != nil {
		t.Fatalf("buildFanOut: %v", err)
	}

	batch := []core.LogRecord{
		{Body: "info", Severity: core.SeverityInfo},
		{Body: "warn", Severity: core.SeverityWarn},
	}
	if err := fanOut.Export(context.Background(), batch); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if got := len(picky.lastBatch()); got != 1 {
		t.Errorf("picky destination received %d records, want 1 (its FilterConfig.MinSeverity narrows to Warn and above)", got)
	}
	if got := len(open.lastBatch()); got != 2 {
		t.Errorf("open destination (no Filter configured) received %d records, want 2 (unnarrowed)", got)
	}
}

// Redaction off is a choice someone types, not one a zero value makes
// (ADR-0014).
func TestRedactionCanBeDisabledOnlyExplicitly(t *testing.T) {
	cfg := workingConfig()
	if _, err := build(cfg, discardLogger()); err != nil {
		t.Fatalf("the default configuration was refused: %v", err)
	}

	cfg.Redaction.Disabled = true
	if _, err := build(cfg, discardLogger()); err != nil {
		t.Fatalf("disabling redaction was refused: %v", err)
	}
	redactor, err := buildRedactor(RedactionConfig{Disabled: true}, nil)
	if err != nil {
		t.Fatalf("buildRedactor: %v", err)
	}
	if redactor != nil {
		t.Error("Disabled produced a redactor; off means no redactor, not a permissive one")
	}
}

// The daemon end to end: it starts, answers probes, accepts a record, and
// drains on signal.
func TestDaemonServesAndDrains(t *testing.T) {
	ingestPort, adminPort := freePort(t), freePort(t)

	cfg := workingConfig()
	cfg.Listen = fmt.Sprintf("127.0.0.1:%d", ingestPort)
	cfg.AdminListen = fmt.Sprintf("127.0.0.1:%d", adminPort)
	cfg.DrainTimeout = Duration(3 * time.Second)

	d, err := build(cfg, discardLogger())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- d.serve(ctx) }()

	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	waitFor(t, "the admin listener", func() bool {
		resp, probeErr := http.Get(adminURL + "/healthz") //nolint:noctx // a probe in a test
		if probeErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// Ready while running: the destination is unreachable but its circuit has
	// not opened yet, and readiness is about "can export", not "has exported".
	if status := get(t, adminURL+"/readyz"); status != http.StatusOK {
		t.Errorf("/readyz = %d while running, want 200", status)
	}

	// A record goes in through the real chain.
	body := strings.NewReader(`{"records":[{"body":"hello","severityNumber":9}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/logs", ingestPort), body)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer task-api:ingest-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting a record: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(resp.Body)
		t.Errorf("ingest = %d (%s), want 202", resp.StatusCode, payload)
	}

	// The counters the pipeline already maintains, readable from the admin
	// listener (audit finding A-6). Asserting the specific source and count,
	// not just that the endpoint answers.
	metricsResp, err := http.Get(adminURL + "/metrics") //nolint:noctx // a probe in a test
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBody, err := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	if ct := metricsResp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("/metrics Content-Type = %q, want text/plain", ct)
	}
	if want := `crier_records_ingested_total{source="task-api"} 1`; !strings.Contains(string(metricsBody), want) {
		t.Errorf("/metrics missing %q\nfull body:\n%s", want, metricsBody)
	}

	cancel()
	select {
	case err := <-served:
		// The export destination does not exist, so the drain reports what it
		// could not deliver rather than pretending it did.
		if err != nil && !strings.Contains(err.Error(), "drain") {
			t.Logf("serve returned: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not shut down")
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // a probe in a test
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestVersionFlagPrintsAndExits(t *testing.T) {
	var out strings.Builder
	if err := run(context.Background(), []string{"-version"}, noEnv, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "crierd") {
		t.Errorf("output = %q, want the version line", out.String())
	}
}
