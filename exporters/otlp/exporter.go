package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JonasBorgesLM/moat/secret"
	collogs "go.opentelemetry.io/proto/slim/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/JonasBorgesLM/crier/core"
)

// Defaults.
const (
	// DefaultPath is where OTLP/HTTP puts logs.
	DefaultPath = "/v1/logs"
	// DefaultTimeout bounds one HTTP request. The fan-out's per-destination
	// deadline bounds the whole chain above it (ADR-0016); this bounds a
	// single attempt so a retry can still happen inside that budget.
	DefaultTimeout = 10 * time.Second
	// DefaultCredentialHeader carries the credential when none is named.
	DefaultCredentialHeader = "Authorization"
	// DefaultCredentialScheme prefixes the credential value.
	DefaultCredentialScheme = "Bearer"
	// maxResponseBody bounds what is read back. A destination's error body is
	// for a human to read, not for us to store.
	maxResponseBody = 1 << 20
)

// Compression is how the payload is encoded on the wire.
type Compression string

// Supported compressions.
const (
	// CompressionGzip is the default: log batches compress extremely well and
	// every collector accepts it.
	CompressionGzip Compression = "gzip"
	// CompressionNone sends the protobuf as-is.
	CompressionNone Compression = "none"
)

// Config configures an Exporter. Build one with New, which validates eagerly
// (NFR4).
type Config struct {
	// Endpoint is the collector's base URL, for example
	// "https://collector.example.com:4318". Required.
	//
	// Path is appended unless the endpoint already names one.
	Endpoint string

	// Path overrides where logs are posted. Zero means DefaultPath.
	Path string

	// Headers are sent with every request — a tenant id, a routing header.
	// Never a credential: use Credential, so it cannot be printed.
	Headers map[string]string

	// Credential is the token sent with every request, held masked so that a
	// config dump, a log line, or a panic cannot print it (NFR4, IR2).
	Credential secret.Value

	// CredentialHeader carries the credential. Zero means
	// DefaultCredentialHeader.
	CredentialHeader string

	// CredentialScheme prefixes the credential value. Zero means
	// DefaultCredentialScheme; set it to "-" for a bare value, as an API-key
	// header expects.
	CredentialScheme string

	// AllowInsecureCredential permits sending a credential over plaintext
	// http://. It exists so that the decision is made deliberately and shows
	// up in review, rather than being discovered from a packet capture.
	AllowInsecureCredential bool

	// Compression is the payload encoding. Zero means CompressionGzip.
	Compression Compression

	// Timeout bounds one HTTP request. Zero means DefaultTimeout. Ignored
	// when HTTPClient is set, which carries its own.
	Timeout time.Duration

	// HTTPClient overrides the client. Nil builds one from Timeout.
	HTTPClient *http.Client

	// UserAgent overrides the User-Agent header.
	UserAgent string
}

// Exporter sends crier batches to one OTLP/HTTP collector (FR5, ADR-0017).
//
// It is the innermost layer of a destination's chain: wrap it in a circuit
// breaker, then a retry, then hand it to the fan-out (ADR-0013). It performs
// no retrying of its own — that would be a second, invisible retry budget
// underneath the one an operator configured.
//
// Safe for concurrent use.
type Exporter struct {
	endpoint    string
	headers     map[string]string
	credential  secret.Value
	credHeader  string
	credScheme  string
	compression Compression
	client      *http.Client
	userAgent   string

	closed atomic.Bool
	gzip   sync.Pool
}

var _ core.Exporter = (*Exporter)(nil)

// New validates cfg and returns the exporter.
func New(cfg Config) (*Exporter, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("otlp: Endpoint is required")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("otlp: parsing Endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("otlp: Endpoint scheme %q, want http or https", endpoint.Scheme)
	}
	if endpoint.Host == "" {
		return nil, fmt.Errorf("otlp: Endpoint %q names no host", cfg.Endpoint)
	}

	path := cfg.Path
	if path == "" && (endpoint.Path == "" || endpoint.Path == "/") {
		path = DefaultPath
	}
	if path != "" {
		endpoint = endpoint.JoinPath(path)
	}

	switch cfg.Compression {
	case "", CompressionGzip, CompressionNone:
	default:
		return nil, fmt.Errorf("otlp: Compression %q, want %q or %q", cfg.Compression, CompressionGzip, CompressionNone)
	}

	if !cfg.Credential.IsZero() && endpoint.Scheme != "https" && !cfg.AllowInsecureCredential {
		// Refusing loudly at startup beats discovering it in a packet
		// capture. The escape hatch exists, and naming it in config is the
		// point: the decision shows up in review.
		return nil, fmt.Errorf("otlp: refusing to send a credential to %s over plaintext http; "+
			"use https, or set AllowInsecureCredential to accept it deliberately", endpoint.Host)
	}
	for name := range cfg.Headers {
		if strings.EqualFold(name, credentialHeader(cfg)) {
			return nil, fmt.Errorf("otlp: Headers[%q] would carry a credential as a plain string; use Credential", name)
		}
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("otlp: negative Timeout %v", cfg.Timeout)
	}

	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = DefaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	compression := cfg.Compression
	if compression == "" {
		compression = CompressionGzip
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = "crier-otlp/" + versionOrDev()
	}

	e := &Exporter{
		endpoint:    endpoint.String(),
		headers:     maps(cfg.Headers),
		credential:  cfg.Credential,
		credHeader:  credentialHeader(cfg),
		credScheme:  credentialScheme(cfg),
		compression: compression,
		client:      client,
		userAgent:   userAgent,
	}
	e.gzip.New = func() any { return gzip.NewWriter(io.Discard) }
	return e, nil
}

func credentialHeader(cfg Config) string {
	if cfg.CredentialHeader != "" {
		return cfg.CredentialHeader
	}
	return DefaultCredentialHeader
}

func credentialScheme(cfg Config) string {
	switch cfg.CredentialScheme {
	case "":
		return DefaultCredentialScheme
	case "-":
		// A bare value, as an API-key header expects.
		return ""
	default:
		return cfg.CredentialScheme
	}
}

func versionOrDev() string {
	if v := scopeVersion(); v != "" {
		return v
	}
	return "dev"
}

// maps copies a header map so a caller cannot mutate the exporter's after the
// fact.
func maps(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Endpoint reports the URL batches are posted to. Useful in a config dump —
// and safe in one, because the credential is not in it.
func (e *Exporter) Endpoint() string { return e.endpoint }

// Export sends batch to the collector.
//
// It returns nil only when the collector accepted every record. A response
// that accepts some and rejects the rest is an error, per the core.Exporter
// contract: the caller's only recovery is to re-send, and reporting a partial
// rejection as success would lose those records silently.
func (e *Exporter) Export(ctx context.Context, batch []core.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	if e.closed.Load() {
		// Not retryable: this exporter is not coming back.
		return fmt.Errorf("%w: otlp: exporter is shut down", core.ErrPermanent)
	}

	payload, err := proto.Marshal(buildRequest(batch))
	if err != nil {
		// Our own encoding failed. Retrying an identical batch produces an
		// identical failure.
		return fmt.Errorf("%w: otlp: encoding the payload: %w", core.ErrPermanent, err)
	}

	body, encoding, err := e.compress(payload)
	if err != nil {
		return fmt.Errorf("%w: otlp: compressing the payload: %w", core.ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: otlp: building the request: %w", core.ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", e.userAgent)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	if !e.credential.IsZero() {
		req.Header.Set(e.credHeader, e.credentialValue())
	}

	//nolint:bodyclose // drainAndClose, deferred below, closes it
	resp, err := e.client.Do(req)
	if err != nil {
		// Nothing reached the backend, so trying again is exactly the right
		// response — unless it was our own context that ended it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("otlp: %w", err)
		}
		return fmt.Errorf("otlp: posting to %s: %w", e.endpoint, err)
	}
	defer drainAndClose(resp.Body)

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("otlp: reading the response from %s: %w", e.endpoint, err)
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		return partialSuccess(respBody, len(batch))
	}
	return classify(resp.StatusCode, parseRetryAfter(resp.Header, time.Now()), string(respBody))
}

// credentialValue renders the credential for its header. The plain value
// exists only for the length of this call.
func (e *Exporter) credentialValue() string {
	value := string(e.credential.Bytes())
	if e.credScheme == "" {
		return value
	}
	return e.credScheme + " " + value
}

// compress encodes the payload, returning the body and its Content-Encoding.
func (e *Exporter) compress(payload []byte) (body []byte, encoding string, err error) {
	if e.compression != CompressionGzip {
		return payload, "", nil
	}

	var buf bytes.Buffer
	buf.Grow(len(payload) / 2)

	w, ok := e.gzip.Get().(*gzip.Writer)
	if !ok {
		w = gzip.NewWriter(io.Discard)
	}
	w.Reset(&buf)
	if _, err := w.Write(payload); err != nil {
		e.gzip.Put(w)
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		e.gzip.Put(w)
		return nil, "", err
	}
	e.gzip.Put(w)

	return buf.Bytes(), "gzip", nil
}

// partialSuccess reads a 200 response and reports the records the collector
// refused.
func partialSuccess(body []byte, sent int) error {
	if len(body) == 0 {
		return nil
	}

	var resp collogs.ExportLogsServiceResponse
	if err := proto.Unmarshal(body, &resp); err != nil {
		// The collector said 200. A response we cannot parse does not
		// un-accept the batch, and failing here would re-send records that
		// are already stored.
		//
		//nolint:nilerr // an unreadable body does not retract a 200
		return nil
	}

	ps := resp.GetPartialSuccess()
	if ps == nil || ps.GetRejectedLogRecords() == 0 {
		return nil
	}

	// Permanent: records refused on their merits are refused again on the
	// next attempt. This over-counts — the accepted records in the same batch
	// are reported as failed too — because a partial-success response does
	// not say which ones they were (ADR-0017).
	msg := strings.TrimSpace(ps.GetErrorMessage())
	if msg == "" {
		msg = "no reason given"
	}
	return fmt.Errorf("%w: otlp: collector rejected %d of %d records: %s",
		core.ErrPermanent, ps.GetRejectedLogRecords(), sent, msg)
}

// drainAndClose finishes with a response body so its connection can be reused
// rather than torn down once per batch.
//
// Neither error is actionable: whatever the caller needed has already been
// read, and a failure here means the connection is gone — which is the state
// closing it was for.
func drainAndClose(body io.ReadCloser) {
	//nolint:errcheck,gosec // draining is best-effort; the response is already read
	io.Copy(io.Discard, io.LimitReader(body, maxResponseBody))
	//nolint:errcheck,gosec // a close failure means the connection is already gone
	body.Close()
}

// Shutdown releases the exporter's connections. It is idempotent, and any
// Export after it fails permanently rather than silently doing nothing.
func (e *Exporter) Shutdown(context.Context) error {
	if e.closed.Swap(true) {
		return nil
	}
	if transport, ok := e.client.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	} else {
		e.client.CloseIdleConnections()
	}
	return nil
}
