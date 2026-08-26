package otlp

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JonasBorgesLM/moat/secret"
	collogs "go.opentelemetry.io/proto/slim/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/JonasBorgesLM/crier/core"
)

// capture is a fake collector that records what it was sent.
type capture struct {
	mu       chan struct{} // used as a 1-slot mutex, so the zero value is unusable by mistake
	requests []*http.Request
	bodies   [][]byte

	status     int
	respBody   []byte
	header     http.Header
	handleFunc func(w http.ResponseWriter, r *http.Request) bool
}

func newCapture() *capture {
	c := &capture{mu: make(chan struct{}, 1), status: http.StatusOK, header: http.Header{}}
	return c
}

func (c *capture) lock()   { c.mu <- struct{}{} }
func (c *capture) unlock() { <-c.mu }

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	c.lock()
	c.requests = append(c.requests, r)
	c.bodies = append(c.bodies, body)
	handle, status, respBody, header := c.handleFunc, c.status, c.respBody, c.header
	c.unlock()

	if handle != nil && handle(w, r) {
		return
	}
	for k, values := range header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (c *capture) count() int {
	c.lock()
	defer c.unlock()
	return len(c.requests)
}

func (c *capture) last() (req *http.Request, body []byte) {
	c.lock()
	defer c.unlock()
	if len(c.requests) == 0 {
		return nil, nil
	}
	return c.requests[len(c.requests)-1], c.bodies[len(c.bodies)-1]
}

// decode reads back the payload the exporter sent, undoing compression.
func (c *capture) decode(t *testing.T) *collogs.ExportLogsServiceRequest {
	t.Helper()
	req, body := c.last()
	if req == nil {
		t.Fatal("the collector received nothing")
	}
	if req.Header.Get("Content-Encoding") == "gzip" {
		r, err := gzip.NewReader(strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		body, err = io.ReadAll(r)
		if err != nil {
			t.Fatalf("gzip read: %v", err)
		}
	}
	var out collogs.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &out); err != nil {
		t.Fatalf("the collector could not parse the payload: %v", err)
	}
	return &out
}

func testRecords() []core.LogRecord {
	return []core.LogRecord{{
		Timestamp:         time.Unix(1700000000, 0),
		ObservedTimestamp: time.Unix(1700000001, 0),
		Severity:          core.SeverityError,
		SeverityText:      "ERROR",
		Body:              "database unreachable",
		Attributes:        map[string]any{"attempt": 3, "endpoint": "db:5432"},
		Resource:          core.Resource{ServiceName: "task-api", ServiceVersion: "1.4.0"},
		TraceID:           "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:            "00f067aa0ba902b7",
	}}
}

func mustExporter(t *testing.T, cfg Config) *Exporter {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestNewRejectsConfigurationThatCannotBeRight(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no endpoint", Config{}, "Endpoint is required"},
		{"bad scheme", Config{Endpoint: "ftp://collector:4318"}, "want http or https"},
		{"no host", Config{Endpoint: "https://"}, "names no host"},
		{"unknown compression", Config{Endpoint: "https://c:4318", Compression: "brotli"}, "want \"gzip\""},
		{"negative timeout", Config{Endpoint: "https://c:4318", Timeout: -time.Second}, "negative Timeout"},
		{
			// Discovering this from a packet capture is worse than being told
			// at startup. The escape hatch exists and has to be named.
			name: "credential over plaintext",
			cfg:  Config{Endpoint: "http://collector:4318", Credential: secret.New([]byte("t"))},
			want: "refusing to send a credential",
		},
		{
			// A credential put in Headers is a plain string, which is exactly
			// what NFR4 says a credential must never be.
			name: "credential smuggled through Headers",
			cfg: Config{
				Endpoint: "https://collector:4318",
				Headers:  map[string]string{"authorization": "Bearer hunter2"},
			},
			want: "use Credential",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("New(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewAllowsPlaintextDeliberately(t *testing.T) {
	e, err := New(Config{
		Endpoint:                "http://collector:4318",
		Credential:              secret.New([]byte("t")),
		AllowInsecureCredential: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := e.Endpoint(), "http://collector:4318/v1/logs"; got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
}

func TestEndpointPathHandling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		path     string
		want     string
	}{
		{"default path appended", "https://c:4318", "", "https://c:4318/v1/logs"},
		{"trailing slash", "https://c:4318/", "", "https://c:4318/v1/logs"},
		{"explicit path override", "https://c:4318", "/custom/logs", "https://c:4318/custom/logs"},
		{
			// A gateway that routes OTLP under a prefix. Appending the default
			// on top of it would post to a path nobody configured.
			name:     "endpoint that already names a path is left alone",
			endpoint: "https://gw.example.com/otlp/v1/logs",
			want:     "https://gw.example.com/otlp/v1/logs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := mustExporter(t, Config{Endpoint: tc.endpoint, Path: tc.path})
			if got := e.Endpoint(); got != tc.want {
				t.Errorf("Endpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExportSendsAWellFormedPayload(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c)
	defer srv.Close()

	e := mustExporter(t, Config{Endpoint: srv.URL, Headers: map[string]string{"X-Tenant": "acme"}})
	if err := e.Export(context.Background(), testRecords()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	req, _ := c.last()
	if got, want := req.Method, http.MethodPost; got != want {
		t.Errorf("method = %s, want %s", got, want)
	}
	if got, want := req.URL.Path, "/v1/logs"; got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
	if got, want := req.Header.Get("Content-Type"), "application/x-protobuf"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("Content-Encoding"), "gzip"; got != want {
		t.Errorf("Content-Encoding = %q, want %q — gzip is the default", got, want)
	}
	if got, want := req.Header.Get("X-Tenant"), "acme"; got != want {
		t.Errorf("X-Tenant = %q, want %q", got, want)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "crier-otlp/") {
		t.Errorf("User-Agent = %q, want it to identify crier", got)
	}

	payload := c.decode(t)
	if got := len(payload.GetResourceLogs()); got != 1 {
		t.Fatalf("ResourceLogs = %d, want 1", got)
	}
	rl := payload.GetResourceLogs()[0]

	attrs := map[string]string{}
	for _, kv := range rl.GetResource().GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if got, want := attrs["service.name"], "task-api"; got != want {
		t.Errorf("service.name = %q, want %q", got, want)
	}
	if got, want := attrs["service.version"], "1.4.0"; got != want {
		t.Errorf("service.version = %q, want %q", got, want)
	}

	sl := rl.GetScopeLogs()[0]
	if got, want := sl.GetScope().GetName(), scopeName; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}

	rec := sl.GetLogRecords()[0]
	if got, want := rec.GetBody().GetStringValue(), "database unreachable"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := rec.GetSeverityNumber(), int32(core.SeverityError); int32(got) != want {
		t.Errorf("severityNumber = %d, want %d", got, want)
	}
	// ObservedTimestamp is authoritative, Timestamp is what the source claimed
	// (ADR-0009). Both are carried; neither is invented.
	if got, want := rec.GetObservedTimeUnixNano(), uint64(time.Unix(1700000001, 0).UnixNano()); got != want {
		t.Errorf("observedTimeUnixNano = %d, want %d", got, want)
	}
	if got, want := rec.GetTimeUnixNano(), uint64(time.Unix(1700000000, 0).UnixNano()); got != want {
		t.Errorf("timeUnixNano = %d, want %d", got, want)
	}
	if got, want := fmt.Sprintf("%x", rec.GetTraceId()), "4bf92f3577b34da6a3ce929d0e0e4736"; got != want {
		t.Errorf("traceId = %s, want %s", got, want)
	}

	recAttrs := map[string]*string{}
	for _, kv := range rec.GetAttributes() {
		v := kv.GetValue().String()
		recAttrs[kv.GetKey()] = &v
	}
	if _, ok := recAttrs["attempt"]; !ok {
		t.Errorf("record attributes = %v, want the attempt attribute carried", recAttrs)
	}
}

func TestExportEmptyBatchSendsNothing(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c)
	defer srv.Close()

	e := mustExporter(t, Config{Endpoint: srv.URL})
	if err := e.Export(context.Background(), nil); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := c.count(); got != 0 {
		t.Errorf("the collector received %d requests for an empty batch, want 0", got)
	}
}

func TestExportCompressionIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		compression Compression
		wantHeader  string
	}{
		{CompressionGzip, "gzip"},
		{CompressionNone, ""},
	} {
		t.Run(string(tc.compression), func(t *testing.T) {
			c := newCapture()
			srv := httptest.NewServer(c)
			defer srv.Close()

			e := mustExporter(t, Config{Endpoint: srv.URL, Compression: tc.compression})
			if err := e.Export(context.Background(), testRecords()); err != nil {
				t.Fatalf("Export: %v", err)
			}
			req, _ := c.last()
			if got := req.Header.Get("Content-Encoding"); got != tc.wantHeader {
				t.Errorf("Content-Encoding = %q, want %q", got, tc.wantHeader)
			}
			// Either way the collector must be able to read it back.
			if got := len(c.decode(t).GetResourceLogs()); got != 1 {
				t.Errorf("ResourceLogs = %d, want 1", got)
			}
		})
	}
}

func TestCredentialIsSentButNeverPrintable(t *testing.T) {
	const token = "super-secret-token"
	c := newCapture()
	srv := httptest.NewTLSServer(c)
	defer srv.Close()

	cfg := Config{
		Endpoint:   srv.URL,
		Credential: secret.New([]byte(token)),
		HTTPClient: srv.Client(),
	}
	e := mustExporter(t, cfg)
	if err := e.Export(context.Background(), testRecords()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	req, _ := c.last()
	if got, want := req.Header.Get("Authorization"), "Bearer "+token; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}

	// The whole point of holding it masked: nothing that renders the config
	// or the exporter can print it (NFR4, IR2).
	for _, rendered := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg.Credential),
		cfg.Credential.String(),
		e.Endpoint(),
	} {
		if strings.Contains(rendered, token) {
			t.Errorf("the credential leaked into %q", rendered)
		}
	}
}

func TestCredentialSchemeIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		header string
		want   string
	}{
		{
			name:   "default bearer",
			cfg:    Config{},
			header: "Authorization",
			want:   "Bearer k",
		},
		{
			name:   "custom header, bare value",
			cfg:    Config{CredentialHeader: "X-Api-Key", CredentialScheme: "-"},
			header: "X-Api-Key",
			want:   "k",
		},
		{
			name:   "custom scheme",
			cfg:    Config{CredentialScheme: "Token"},
			header: "Authorization",
			want:   "Token k",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture()
			srv := httptest.NewTLSServer(c)
			defer srv.Close()

			cfg := tc.cfg
			cfg.Endpoint = srv.URL
			cfg.Credential = secret.New([]byte("k"))
			cfg.HTTPClient = srv.Client()

			e := mustExporter(t, cfg)
			if err := e.Export(context.Background(), testRecords()); err != nil {
				t.Fatalf("Export: %v", err)
			}
			req, _ := c.last()
			if got := req.Header.Get(tc.header); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// The classification table from ADR-0017. A mistake here is invisible until
// telemetry is already gone: a retryable 400 burns the budget on a batch no
// backend will accept, a permanent 503 discards data during a restart.
func TestStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status        int
		wantPermanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusConflict, true},
		{http.StatusRequestEntityTooLarge, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusTeapot, true}, // unknown 4xx: assume it is our fault
		{http.StatusNotImplemented, true},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusGatewayTimeout, false},
		{http.StatusInsufficientStorage, false}, // unknown 5xx: assume it is theirs
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			c := newCapture()
			c.status = tc.status
			c.respBody = []byte("upstream said no")
			srv := httptest.NewServer(c)
			defer srv.Close()

			e := mustExporter(t, Config{Endpoint: srv.URL})
			err := e.Export(context.Background(), testRecords())
			if err == nil {
				t.Fatalf("Export succeeded on %d, want an error", tc.status)
			}
			if got := core.IsPermanent(err); got != tc.wantPermanent {
				t.Errorf("IsPermanent(%v) = %v, want %v", err, got, tc.wantPermanent)
			}
			if !strings.Contains(err.Error(), "upstream said no") {
				t.Errorf("error = %q, want the destination's own explanation kept", err)
			}
		})
	}
}

// A destination that has just said how long it needs (ADR-0017).
func TestRetryAfterIsSurfacedToTheRetryDecorator(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"zero", "0", 0},
		{"garbage is ignored rather than fatal", "soon", 0},
		{"absent", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture()
			c.status = http.StatusTooManyRequests
			if tc.header != "" {
				c.header = http.Header{"Retry-After": []string{tc.header}}
			}
			srv := httptest.NewServer(c)
			defer srv.Close()

			e := mustExporter(t, Config{Endpoint: srv.URL})
			err := e.Export(context.Background(), testRecords())
			if err == nil {
				t.Fatal("Export succeeded on 429, want an error")
			}
			if core.IsPermanent(err) {
				t.Error("429 was classified permanent, want retryable")
			}

			var hint core.RetryHint
			if !errors.As(err, &hint) {
				if tc.want != 0 {
					t.Fatalf("error = %v, want it to carry a Retry-After hint", err)
				}
				return
			}
			if got := hint.RetryAfter(); got != tc.want {
				t.Errorf("RetryAfter() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryAfterAcceptsAnHTTPDate(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	h := http.Header{"Retry-After": []string{now.Add(90 * time.Second).Format(http.TimeFormat)}}

	if got, want := parseRetryAfter(h, now), 90*time.Second; got != want {
		t.Errorf("parseRetryAfter = %v, want %v", got, want)
	}
	// A date already in the past is not a request to wait.
	past := http.Header{"Retry-After": []string{now.Add(-time.Minute).Format(http.TimeFormat)}}
	if got := parseRetryAfter(past, now); got != 0 {
		t.Errorf("parseRetryAfter for a past date = %v, want 0", got)
	}
}

// A 200 that rejects records is not a success. Reporting it as one loses them
// silently, which is the one thing this pipeline does not do.
func TestPartialSuccessIsReportedAsPermanentFailure(t *testing.T) {
	resp := &collogs.ExportLogsServiceResponse{
		PartialSuccess: &collogs.ExportLogsPartialSuccess{
			RejectedLogRecords: 2,
			ErrorMessage:       "unsupported severity",
		},
	}
	body, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	c := newCapture()
	c.respBody = body
	srv := httptest.NewServer(c)
	defer srv.Close()

	e := mustExporter(t, Config{Endpoint: srv.URL})
	err = e.Export(context.Background(), testRecords())
	if err == nil {
		t.Fatal("Export succeeded despite rejected records")
	}
	if !core.IsPermanent(err) {
		t.Errorf("IsPermanent = false for %v, want true — a refused record is refused again", err)
	}
	for _, want := range []string{"rejected 2", "unsupported severity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestSuccessIsNotOverreportedAsPartialFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty body", nil},
		{"empty response message", mustMarshal(&collogs.ExportLogsServiceResponse{})},
		{
			name: "partial success with nothing rejected",
			body: mustMarshal(&collogs.ExportLogsServiceResponse{
				PartialSuccess: &collogs.ExportLogsPartialSuccess{},
			}),
		},
		// A 200 with a body we cannot parse does not un-accept the batch;
		// failing here would re-send records the collector already stored.
		{"unparsable body", []byte("<html>hello</html>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture()
			c.respBody = tc.body
			srv := httptest.NewServer(c)
			defer srv.Close()

			e := mustExporter(t, Config{Endpoint: srv.URL})
			if err := e.Export(context.Background(), testRecords()); err != nil {
				t.Errorf("Export: %v", err)
			}
		})
	}
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func TestTransportFailureIsRetryable(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c)
	srv.Close() // nothing is listening

	e := mustExporter(t, Config{Endpoint: srv.URL, Timeout: time.Second})
	err := e.Export(context.Background(), testRecords())
	if err == nil {
		t.Fatal("Export succeeded against a closed collector")
	}
	if core.IsPermanent(err) {
		t.Errorf("IsPermanent = true for %v, want retryable — nothing reached the backend", err)
	}
}

func TestExportAfterShutdownFailsPermanently(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c)
	defer srv.Close()

	e := mustExporter(t, Config{Endpoint: srv.URL})
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown is not idempotent: %v", err)
	}

	err := e.Export(context.Background(), testRecords())
	if !core.IsPermanent(err) {
		t.Errorf("error = %v, want a permanent failure — this exporter is not coming back", err)
	}
	if got := c.count(); got != 0 {
		t.Errorf("the collector received %d requests after shutdown, want 0", got)
	}
}

func TestExportHonoursContextCancellation(t *testing.T) {
	c := newCapture()
	c.handleFunc = func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	srv := httptest.NewServer(c)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	e := mustExporter(t, Config{Endpoint: srv.URL})
	err := e.Export(ctx, testRecords())
	if err == nil {
		t.Fatal("Export succeeded against a collector that never answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}
