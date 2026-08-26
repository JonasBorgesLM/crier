// Command crierd is crier's standalone log-shipping daemon.
//
// It is a thin shell over core (FR9): it assembles the receiver, the pipeline
// and the exporters from configuration and owns process lifecycle. Anything
// worth testing on its own belongs in core, not here.
//
// # Its own logs
//
// crierd writes its operational logs to stderr and never ingests them (NFR11).
// A configuration that points an exporter at this instance's own receiver is
// refused at startup: every exported batch would produce operational logs,
// which would be ingested, exported, and produce more. That is a feedback
// loop, not observability.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JonasBorgesLM/crier/core"
	httpreceiver "github.com/JonasBorgesLM/crier/receivers/http"
)

// version is overwritten at build time via -ldflags.
var version = "dev"

func main() {
	os.Exit(exitCode())
}

// exitCode runs the daemon in a scope where deferred cleanup still happens.
// os.Exit inside main would skip the signal handler's own stop.
func exitCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "crierd: %v\n", err)
		return 1
	}
	return 0
}

// run is main with its inputs passed in, so the daemon can be started and
// stopped by a test rather than only by a process.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	flags := flag.NewFlagSet("crierd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to a JSON configuration file")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *showVersion {
		fmt.Fprintf(stderr, "crierd %s\n", version) //nolint:errcheck // nothing to do if stderr is gone
		return nil
	}

	cfg, err := LoadConfig(*configPath, getenv)
	if err != nil {
		return err
	}

	d, err := build(cfg, logger)
	if err != nil {
		// Everything that can be wrong is wrong now, before a single record
		// has been accepted (NFR4). A daemon that starts and then discovers
		// its redaction rules do not compile has already exported unmasked.
		return fmt.Errorf("configuration: %w", err)
	}

	return d.serve(ctx)
}

// daemon is the assembled process.
type daemon struct {
	dispatcher *core.Dispatcher
	health     *core.Health
	ingest     *http.Server
	admin      *http.Server
	drain      time.Duration
	logger     *slog.Logger
}

// build assembles everything from configuration, failing on anything wrong.
//
// Validation is construction: each component below already refuses input it
// cannot honour — the fair-share buffer refuses reservations plus pool over
// capacity, the redactor refuses a pattern that will not compile, the
// trusted-proxy authenticator refuses a set covering the default route. This
// function's job is to build them in the right order and let their errors out,
// not to restate their rules.
func build(cfg Config, logger *slog.Logger) (*daemon, error) {
	if err := checkSelfIngestion(cfg.Listen, cfg.Exporters); err != nil {
		return nil, err
	}

	policy, err := dropPolicy(cfg.Buffer.Policy)
	if err != nil {
		return nil, err
	}

	metrics := &core.CountingMetrics{}

	memory, err := core.NewMemoryBuffer(core.MemoryBufferConfig{
		Capacity:    cfg.Buffer.Capacity,
		BatchSize:   batchSize(cfg.Buffer),
		BatchWindow: time.Duration(cfg.Buffer.BatchWindow),
		Policy:      policy,
		Metrics:     metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}

	// The fair-share invariant is reservations + pool <= capacity, and it is
	// checked here by the constructor rather than restated. Over-committing
	// does not crash: quota accounting believes there is room while the store
	// rejects, so capacity pressure reads as a healthy buffer (ADR-0019).
	var buffer core.BufferStore = memory
	if len(cfg.Buffer.Reservations) > 0 || cfg.Buffer.UnlistedPool > 0 {
		fair, fairErr := core.NewFairShareBuffer(memory, core.FairShareConfig{
			Reservations: cfg.Buffer.Reservations,
			UnlistedPool: cfg.Buffer.UnlistedPool,
			Metrics:      metrics,
		})
		if fairErr != nil {
			return nil, fmt.Errorf("fair share: %w", fairErr)
		}
		buffer = fair
	}

	redactor, err := buildRedactor(cfg.Redaction, metrics)
	if err != nil {
		return nil, fmt.Errorf("redaction: %w", err)
	}

	pipeline, err := core.NewPipeline(core.PipelineConfig{
		Buffer: buffer,
		Limits: core.Limits{
			MaxAttributes: cfg.Limits.MaxAttributes,
			MaxKeyBytes:   cfg.Limits.MaxKeyBytes,
			MaxValueBytes: cfg.Limits.MaxValueBytes,
			MaxBodyBytes:  cfg.Limits.MaxBodyBytes,
		},
		Cardinality: buildCardinality(cfg.Limits),
		Redactor:    redactor,
		Filter:      buildFilter(cfg.Filter),
		Metrics:     metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}

	destinations, err := exporters(cfg.Exporters)
	if err != nil {
		return nil, err
	}
	fanOut, err := buildFanOut(destinations, metrics)
	if err != nil {
		return nil, err
	}

	dispatcher, err := core.NewDispatcher(core.DispatcherConfig{
		Buffer:   buffer,
		Exporter: fanOut,
		Workers:  cfg.Buffer.Workers,
		Metrics:  metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	health, err := core.NewHealth(dispatcher)
	if err != nil {
		return nil, err
	}

	auth, err := authenticator(cfg.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	receiver, err := httpreceiver.New(httpreceiver.Config{
		Pipeline: pipeline,
		Auth:     auth,
		Metrics:  metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("receiver: %w", err)
	}
	ingestHandler, err := receiver.Handler(httpreceiver.ChainConfig{})
	if err != nil {
		return nil, fmt.Errorf("receiver: %w", err)
	}

	d := &daemon{
		dispatcher: dispatcher,
		health:     health,
		drain:      time.Duration(cfg.DrainTimeout),
		logger:     logger,
	}
	d.ingest = &http.Server{
		Addr:              cfg.Listen,
		Handler:           ingestHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	d.admin = &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           d.adminMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return d, nil
}

// buildFanOut composes each destination as FanOut(Retry(CircuitBreaker(e))),
// which is the only correct order (ADR-0013).
func buildFanOut(destinations map[string]core.Exporter, metrics core.Metrics) (*core.FanOut, error) {
	built := make([]core.Destination, 0, len(destinations))
	for name, exporter := range destinations {
		breaker, err := core.NewCircuitBreaker(core.CircuitBreakerConfig{
			Name: name, Exporter: exporter, Metrics: metrics,
		})
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", name, err)
		}
		retry, err := core.NewRetry(core.RetryConfig{Name: name, Exporter: breaker, Metrics: metrics})
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", name, err)
		}
		built = append(built, core.Destination{Name: name, Exporter: retry})
	}
	return core.NewFanOut(core.FanOutConfig{Destinations: built, Metrics: metrics})
}

func buildRedactor(cfg RedactionConfig, metrics core.Metrics) (*core.Redactor, error) {
	if cfg.Disabled {
		// The explicit, auditable choice ADR-0014 requires. There is no
		// fail-open redactor — only no redactor, chosen deliberately.
		return nil, nil //nolint:nilnil // a nil redactor is the documented "off" state
	}
	return core.NewRedactor(core.RedactionConfig{
		KeySubstrings: cfg.KeySubstrings,
		KeyPatterns:   cfg.KeyPatterns,
		BodyPatterns:  cfg.BodyPatterns,
		SkipBody:      cfg.SkipBody,
		Metrics:       metrics,
	})
}

// batchSize resolves the batch size, keeping it inside the buffer.
//
// core refuses a batch larger than the buffer, and rightly: such a batch can
// only ever be flushed by timeout, which is a throughput collapse nobody
// configured. But the library default is 512, so a small buffer plus no batch
// size is a configuration that reads as reasonable and will not start. The
// daemon resolves that rather than making an operator discover the
// relationship between two numbers neither of which looks wrong alone.
//
// Only when unset: an explicit batch size is honoured, and refused by core if
// it does not fit, because that one someone chose.
func batchSize(cfg BufferConfig) int {
	if cfg.BatchSize != 0 {
		return cfg.BatchSize
	}
	if cfg.Capacity > 0 && cfg.Capacity < core.DefaultBatchSize {
		return cfg.Capacity
	}
	return 0 // core's default
}

func buildCardinality(cfg LimitsConfig) *core.CardinalityGuard {
	if cfg.MaxDistinctValues == 0 {
		return nil
	}
	return &core.CardinalityGuard{MaxDistinctValues: cfg.MaxDistinctValues}
}

func buildFilter(cfg FilterConfig) *core.Filter {
	if cfg.MinSeverity == 0 && cfg.SampleRate == 0 {
		return nil
	}
	return &core.Filter{
		MinSeverity: core.Severity(cfg.MinSeverity),
		SampleRate:  cfg.SampleRate,
	}
}

// adminMux serves liveness and readiness (NFR5, ADR-0015).
func (d *daemon) adminMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness stays true while degraded and while draining. Failing it
	// during a backend outage gets the instance killed and restarted into the
	// same outage, losing whatever was buffered.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.health.Live() {
			http.Error(w, "not alive", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "alive") //nolint:errcheck // the probe hung up; the status is already sent
	})

	// Readiness reflects export health, so an orchestrator stops sending
	// records this instance cannot export. Seeing it fail during a backend
	// outage is expected and is not a crash loop — which is why the reason is
	// in the body rather than left to be inferred from a status code.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		ready, reason := d.health.Ready()
		if !ready {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, reason) //nolint:errcheck // the probe hung up; the status is already sent
	})

	return mux
}

// serve runs until ctx is done, then shuts down within the drain timeout.
func (d *daemon) serve(ctx context.Context) error {
	d.dispatcher.Start(context.WithoutCancel(ctx))

	errs := make(chan error, 2)
	go serveHTTP(d.ingest, "ingest", errs)
	go serveHTTP(d.admin, "admin", errs)

	d.logger.Info("crierd started",
		"version", version, "ingest", d.ingest.Addr, "admin", d.admin.Addr)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		d.logger.Info("shutting down", "drainTimeout", d.drain)
	}
	// Deliberately not ctx: it is already cancelled, and the drain needs its
	// own deadline to finish the work ctx's cancellation is asking it to stop
	// starting.
	return d.shutdown() //nolint:contextcheck // the drain outlives the signal that triggered it
}

func serveHTTP(srv *http.Server, name string, errs chan<- error) {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("%s listener: %w", name, err)
	}
}

// shutdown stops accepting, drains, and reports what the drain achieved.
//
// The order matters. Ingestion closes first so nothing new arrives; the
// dispatcher drains what is already buffered; the admin listener closes last,
// so readiness keeps answering "draining" for as long as the drain runs rather
// than the probe hitting a closed port and being read as a crash.
func (d *daemon) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), d.drain)
	defer cancel()

	var errs []error
	if err := d.ingest.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stopping ingestion: %w", err))
	}

	summary, err := d.dispatcher.Shutdown(ctx)
	if err != nil {
		errs = append(errs, err)
	}

	// The final summary line ADR-0015 requires. Loss at shutdown is permitted;
	// silent loss is not, and this is the line that makes it not silent.
	if summary.Clean() {
		d.logger.Info("drain complete", "summary", summary.String())
	} else {
		d.logger.Error("records lost at shutdown",
			"lost", summary.Lost, "summary", summary.String())
	}

	if err := d.admin.Shutdown(context.Background()); err != nil {
		errs = append(errs, fmt.Errorf("stopping the admin listener: %w", err))
	}
	return errors.Join(errs...)
}
