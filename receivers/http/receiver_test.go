package httpreceiver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JonasBorgesLM/moat/secret"

	"github.com/JonasBorgesLM/crier/core"
)

// harness is a receiver wired to a real pipeline and buffer, so a test
// exercises the path a request actually takes rather than a stub of it.
type harness struct {
	t        *testing.T
	receiver *Receiver
	buffer   core.BufferStore
	metrics  *core.CountingMetrics
}

type harnessConfig struct {
	capacity     int
	reservations map[string]int
	unlisted     int
	deprecated   map[WireVersion]string
	filter       *core.Filter
}

func newHarness(t *testing.T, hc harnessConfig) *harness {
	t.Helper()
	if hc.capacity == 0 {
		hc.capacity = 64
	}

	var metrics core.CountingMetrics
	memory, err := core.NewMemoryBuffer(core.MemoryBufferConfig{
		Capacity: hc.capacity, BatchSize: hc.capacity, Metrics: &metrics,
	})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}

	var buffer core.BufferStore = memory
	if hc.reservations != nil {
		fair, fairErr := core.NewFairShareBuffer(memory, core.FairShareConfig{
			Reservations: hc.reservations, UnlistedPool: hc.unlisted, Metrics: &metrics,
		})
		if fairErr != nil {
			t.Fatalf("NewFairShareBuffer: %v", fairErr)
		}
		buffer = fair
	}

	pipeline, err := core.NewPipeline(core.PipelineConfig{
		Buffer: buffer, Filter: hc.filter, Metrics: &metrics,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	receiver, err := New(Config{
		Pipeline:   pipeline,
		Auth:       testCredentials(t),
		Deprecated: hc.deprecated,
		Metrics:    &metrics,
		Now:        func() time.Time { return observedAt },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, receiver: receiver, buffer: buffer, metrics: &metrics}
}

// post sends an authenticated request unless authorization is overridden.
func (h *harness) post(body string, authorization ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	auth := "Bearer task-api:" + testSecret
	if len(authorization) > 0 {
		auth = authorization[0]
	}

	r := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, V1.Path(), strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.receiver.Handler().ServeHTTP(w, r)
	return w
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) ingestResponse {
	t.Helper()
	var got ingestResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the response %q: %v", w.Body.String(), err)
	}
	return got
}

func recordsBody(n int) string {
	records := make([]string, n)
	for i := range records {
		records[i] = fmt.Sprintf(`{"body":"record %d","severityNumber":9}`, i)
	}
	return `{"records":[` + strings.Join(records, ",") + `]}`
}

func TestNewValidatesEagerly(t *testing.T) {
	pipeline, err := core.NewPipeline(core.PipelineConfig{Buffer: mustBuffer(t)})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no pipeline", Config{Auth: testCredentials(t)}, "needs a pipeline"},
		{
			// An ingestion endpoint with no authentication accepts forged
			// telemetry from any peer that can reach it.
			name: "no authenticator",
			cfg:  Config{Pipeline: pipeline},
			want: "needs an Authenticator",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("configuration accepted, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func mustBuffer(t *testing.T) core.BufferStore {
	t.Helper()
	b, err := core.NewMemoryBuffer(core.MemoryBufferConfig{Capacity: 8, BatchSize: 8})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	return b
}

// A 202 means admitted to the buffer, not stored by any backend (ADR-0009).
func TestAcceptedRecordsAnswer202(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	w := h.post(recordsBody(3))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body)
	}
	if got := decodeResponse(t, w).Accepted; got != 3 {
		t.Errorf("accepted = %d, want 3", got)
	}
	if got := h.buffer.Depth(); got != 3 {
		t.Errorf("buffer depth = %d, want 3", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestUnauthenticatedRequestGetsA401AndEchoesNothing(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	w := h.post(recordsBody(1), "Bearer task-api:wrong-secret-value")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if strings.Contains(w.Body.String(), "wrong-secret-value") {
		t.Errorf("the response echoed the presented credential: %s", w.Body)
	}
	if got := h.buffer.Depth(); got != 0 {
		t.Errorf("buffer depth = %d, want 0 — nothing may be admitted before authentication", got)
	}
}

// The acceptance criterion for #21, through the handler this time.
func TestMisspelledFieldGetsA400NamingIt(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	w := h.post(`{"records":[{"body":"hi","severtyText":"ERROR"}]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := decodeResponse(t, w).Reason; !strings.Contains(got, "severtyText") {
		t.Errorf("reason = %q, want it to name the offending field", got)
	}
}

// The acceptance criterion for #24: a caller claiming someone else's identity
// has its records attributed to whoever actually authenticated (ADR-0008,
// finding D-2).
func TestClientAssertedIdentityIsOverwrittenAndCounted(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	w := h.post(`{"records":[{"body":"forged","resource":{"serviceName":"gateway-auth","attributes":{"region":"eu-west-1"}}}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202 — the record is accepted, just not on the caller's terms", w.Code, w.Body)
	}

	batch, err := h.buffer.DequeueBatch(t.Context())
	if err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("got %d records, want 1", len(batch))
	}

	if got := batch[0].Resource.ServiceName; got != "task-api" {
		t.Errorf("ServiceName = %q, want the authenticated principal %q", got, "task-api")
	}
	// Descriptive attributes outside the identity fields survive.
	if got := batch[0].Resource.Attributes["region"]; got != "eu-west-1" {
		t.Errorf("descriptive resource attributes were lost: %v", batch[0].Resource.Attributes)
	}
	// A rising count here is either a misconfigured client or someone probing.
	if got := h.metrics.Snapshot().IdentityDiscrepancies["task-api"]; got != 1 {
		t.Errorf("IdentityDiscrepancies = %d, want 1", got)
	}
}

// Capacity pressure and a source's own quota call for different responses, so
// they must not both surface as "the server is busy" (ADR-0011).
func TestQuotaRejectionIsDistinguishableFromCapacityPressure(t *testing.T) {
	t.Run("buffer full is 503", func(t *testing.T) {
		h := newHarness(t, harnessConfig{capacity: 2})

		if w := h.post(recordsBody(2)); w.Code != http.StatusAccepted {
			t.Fatalf("filling the buffer: status = %d", w.Code)
		}
		w := h.post(recordsBody(1))

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d (%s), want 503", w.Code, w.Body)
		}
		if got := decodeResponse(t, w).Reason; got != "buffer full" {
			t.Errorf("reason = %q", got)
		}
	})

	t.Run("source quota is 429", func(t *testing.T) {
		h := newHarness(t, harnessConfig{
			capacity:     10,
			reservations: map[string]int{"task-api": 2, "gateway-auth": 2},
			unlisted:     0,
		})

		// Burn this source's floor and every spare slot.
		for range 10 {
			h.post(recordsBody(1))
		}
		w := h.post(recordsBody(1))

		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d (%s), want 429 — a quota rejection is not capacity pressure", w.Code, w.Body)
		}
		if got := decodeResponse(t, w).Reason; got != "source quota exhausted" {
			t.Errorf("reason = %q", got)
		}
	})
}

// Answering with the failure status invites the caller to re-send everything,
// duplicating the part already accepted — the amplification ADR-0013 forbids
// on the export side, arriving from the other direction.
func TestPartiallyAdmittedBatchReportsCountsRatherThanFailing(t *testing.T) {
	h := newHarness(t, harnessConfig{capacity: 4})

	w := h.post(recordsBody(6))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body)
	}
	got := decodeResponse(t, w)
	if got.Accepted != 4 || got.Rejected != 2 {
		t.Errorf("accepted/rejected = %d/%d, want 4/2", got.Accepted, got.Rejected)
	}
	if got.Reason != "buffer full" {
		t.Errorf("reason = %q, want the rejected records explained", got.Reason)
	}
}

// A filtered record was handled exactly as configured. Reporting that as
// failure would make a correct configuration look broken (ADR-0010).
func TestFilteredRecordsAreStillA202(t *testing.T) {
	h := newHarness(t, harnessConfig{filter: &core.Filter{MinSeverity: core.SeverityError}})

	w := h.post(recordsBody(3)) // severityNumber 9 — below the threshold

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body)
	}
	// Filtered records are accepted: the server handled them exactly as
	// configured, and reporting that as a rejection would invite the caller
	// to re-send records the policy exists to discard.
	got := decodeResponse(t, w)
	if got.Accepted != 3 {
		t.Errorf("accepted = %d, want 3 — filtering is not a rejection", got.Accepted)
	}
	if got.Rejected != 0 {
		t.Errorf("rejected = %d, want 0", got.Rejected)
	}
	if got := h.metrics.Snapshot().Filtered["task-api"]; got != 3 {
		t.Errorf("Filtered = %d, want 3", got)
	}
	if got := h.metrics.Snapshot().TotalDropped(); got != 0 {
		t.Errorf("TotalDropped = %d, want 0 — filtering is not loss", got)
	}
}

// The mechanism ADR-0012 reserves for a migration window, exercised rather
// than asserted: a removal decision should rest on a counter, not a guess.
func TestDeprecatedVersionAdvertisesItselfAndIsCounted(t *testing.T) {
	h := newHarness(t, harnessConfig{
		deprecated: map[WireVersion]string{V1: "Wed, 31 Dec 2031 23:59:59 GMT"},
	})

	w := h.post(recordsBody(1))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want the deprecated version still served", w.Code)
	}
	if got := w.Header().Get("Deprecation"); got != "true" {
		t.Errorf("Deprecation header = %q, want \"true\"", got)
	}
	if got := w.Header().Get("Sunset"); got == "" {
		t.Error("no Sunset header; a deprecation without a date is not a migration plan")
	}
	if got := h.metrics.Snapshot().DeprecatedWireVersion["v1"]; got != 1 {
		t.Errorf("DeprecatedWireVersion = %d, want 1", got)
	}
}

func TestCurrentVersionCarriesNoDeprecationHeader(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	w := h.post(recordsBody(1))

	if got := w.Header().Get("Deprecation"); got != "" {
		t.Errorf("Deprecation header = %q on a current version", got)
	}
	if got := h.metrics.Snapshot().DeprecatedWireVersion["v1"]; got != 0 {
		t.Errorf("DeprecatedWireVersion = %d, want 0", got)
	}
}

func TestOnlyPostIsRouted(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequestWithContext(t.Context(), method, V1.Path(), strings.NewReader(recordsBody(1)))
		r.Header.Set("Authorization", "Bearer task-api:"+testSecret)
		w := httptest.NewRecorder()
		h.receiver.Handler().ServeHTTP(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, w.Code)
		}
	}
}

func TestUnknownPathIsNotServed(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2/logs", strings.NewReader(recordsBody(1)))
	r.Header.Set("Authorization", "Bearer task-api:"+testSecret)
	w := httptest.NewRecorder()
	h.receiver.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — an unserved version must not fall through to v1", w.Code)
	}
}

func TestReceiverStringNamesWhatItServes(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	if got := h.receiver.String(); !strings.Contains(got, V1.Path()) {
		t.Errorf("String() = %q, want it to name the served path", got)
	}
}

// A config dump must not become a credential dump.
func TestReceiverConfigDoesNotRenderCredentials(t *testing.T) {
	cfg := Config{Auth: testCredentials(t)}
	for _, out := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg)} {
		if strings.Contains(out, testSecret) {
			t.Errorf("the credential leaked into %q", out)
		}
	}
	_ = secret.Value{}
}
