package httpreceiver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JonasBorgesLM/crier/core"
)

func (h *harness) chained(t *testing.T, cfg ChainConfig) http.Handler {
	t.Helper()
	handler, err := h.receiver.Handler(cfg)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return handler
}

// receiverWithAuth builds a minimal Receiver for tests that need a specific
// Authenticator — the shared harness always uses static credentials, which
// cannot exercise a check that keys off the concrete Authenticator type.
func receiverWithAuth(t *testing.T, auth Authenticator) *Receiver {
	t.Helper()
	buffer, err := core.NewMemoryBuffer(core.MemoryBufferConfig{Capacity: 64, BatchSize: 64})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	pipeline, err := core.NewPipeline(core.PipelineConfig{Buffer: buffer})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	receiver, err := New(Config{Pipeline: pipeline, Auth: auth})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return receiver
}

func chainRequest(t *testing.T, body, contentType string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, V1.Path(), strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer task-api:"+testSecret)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.RemoteAddr = "10.1.2.3:4444"
	return r
}

func TestChainAcceptsAWellFormedRequest(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	handler := h.chained(t, ChainConfig{})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chainRequest(t, recordsBody(2), ContentTypeJSON))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body)
	}
	if got := decodeResponse(t, w).Accepted; got != 2 {
		t.Errorf("accepted = %d, want 2", got)
	}
}

// moat's own ordering lesson, and the reason it is worth restating: the
// mistake is invisible in the happy path, where every response already looks
// right. Only an error response shows whether the headers are outermost.
func TestSecurityHeadersReachErrorResponsesToo(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	handler := h.chained(t, ChainConfig{})

	for _, tc := range []struct {
		name       string
		request    *http.Request
		wantStatus int
	}{
		{"accepted", chainRequest(t, recordsBody(1), ContentTypeJSON), http.StatusAccepted},
		{"rejected content type", chainRequest(t, recordsBody(1), "text/plain"), http.StatusUnsupportedMediaType},
		{"rejected payload", chainRequest(t, `{"records":[{"nope":1}]}`, ContentTypeJSON), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, tc.request)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body, tc.wantStatus)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q on a %d response — the headers are not outermost",
					got, w.Code)
			}
		})
	}
}

// A guard that runs after the body is read has already allowed the work it
// exists to prevent (ADR-0010, step 1).
func TestOversizedBodyIsRejectedBeforeTheHandlerParsesIt(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	handler := h.chained(t, ChainConfig{MaxBodyBytes: 256})

	oversized := `{"records":[{"body":"` + strings.Repeat("x", 1024) + `"}]}`
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chainRequest(t, oversized, ContentTypeJSON))

	if w.Code == http.StatusAccepted {
		t.Fatalf("an oversized body was accepted: %s", w.Body)
	}
	if got := h.buffer.Depth(); got != 0 {
		t.Errorf("buffer depth = %d, want 0 — nothing may be admitted from a body over the limit", got)
	}
}

func TestWrongContentTypeIsRejected(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	handler := h.chained(t, ChainConfig{})

	for _, contentType := range []string{"", "text/plain", "application/xml"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, chainRequest(t, recordsBody(1), contentType))

		if w.Code == http.StatusAccepted {
			t.Errorf("Content-Type %q was accepted", contentType)
		}
	}
}

func TestRateLimitRejectsABurstAboveTheConfiguredCeiling(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	handler := h.chained(t, ChainConfig{RateLimitBurst: 3, RateLimitPerSecond: 1})

	var limited int
	for range 10 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, chainRequest(t, recordsBody(1), ContentTypeJSON))
		if w.Code == http.StatusTooManyRequests {
			limited++
		}
	}

	if limited == 0 {
		t.Error("no request was rate limited; the limiter is not in the chain")
	}
}

// Omitting a control should be something someone typed, not something a zero
// value did — moat's reasoning about presets that quietly drop a protection.
func TestRateLimitingCanBeDisabledOnlyExplicitly(t *testing.T) {
	h := newHarness(t, harnessConfig{capacity: 1000})
	handler := h.chained(t, ChainConfig{DisableRateLimit: true})

	for range 50 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, chainRequest(t, recordsBody(1), ContentTypeJSON))
		if w.Code == http.StatusTooManyRequests {
			t.Fatal("a request was rate limited despite DisableRateLimit")
		}
	}
}

func TestChainRejectsConfigurationThatCannotBeRight(t *testing.T) {
	h := newHarness(t, harnessConfig{})

	for _, tc := range []struct {
		name string
		cfg  ChainConfig
	}{
		{"negative body limit", ChainConfig{MaxBodyBytes: -1}},
		{"negative burst", ChainConfig{RateLimitBurst: -1}},
		{"negative rate", ChainConfig{RateLimitPerSecond: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.receiver.Handler(tc.cfg); err == nil {
				t.Error("configuration accepted, want an error")
			}
		})
	}
}

// Behind a proxy every request arrives from the proxy, so limiting by peer
// address puts every tenant under one key.
func TestRateLimitKeyCanComeFromTheTrustedProxy(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}
	h := newHarness(t, harnessConfig{})

	handler := h.chained(t, ChainConfig{
		RateLimitBurst: 2, RateLimitPerSecond: 1,
		RateLimitKey: proxy.KeyFunc(),
	})

	w := httptest.NewRecorder()
	r := chainRequest(t, recordsBody(1), ContentTypeJSON)
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(w, r)

	// The point is that the key function is wired in and usable, not what it
	// resolves to — moat owns that, and tests it.
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body)
	}
}

// Issue #74: behind a trusted proxy, every request arrives from that same
// proxy, so limiting by peer address — RateLimitKey's default — puts every
// tenant behind the gateway in one shared bucket. Handler refuses that
// pairing rather than building it silently; DisableRateLimit and the proxy's
// own KeyFunc are both accepted ways to say the pairing was considered.
//
// Unlike TestRateLimitKeyCanComeFromTheTrustedProxy above, this receiver's
// Auth really is the *TrustedProxy — the check keys off the concrete
// Authenticator type, which the shared harness's static credentials cannot
// exercise.
func TestHandlerValidatesTrustedProxyRateLimitPairing(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}
	receiver := receiverWithAuth(t, proxy)

	for _, tc := range []struct {
		name    string
		cfg     ChainConfig
		wantErr bool
	}{
		{"no RateLimitKey, rate limiting on", ChainConfig{}, true},
		{"no RateLimitKey, rate limiting disabled", ChainConfig{DisableRateLimit: true}, false},
		{"RateLimitKey from the same proxy", ChainConfig{RateLimitKey: proxy.KeyFunc()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := receiver.Handler(tc.cfg)
			if tc.wantErr && err == nil {
				t.Error("Handler accepted the configuration, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Handler refused the configuration: %v", err)
			}
		})
	}
}

// A non-trusted-proxy Authenticator has no such hazard: the peer really is
// the caller, so the default rate-limit key (peer address) is a reasonable
// choice, and Handler must not refuse it.
func TestHandlerAcceptsNoRateLimitKeyForNonTrustedProxyAuth(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	if _, err := h.receiver.Handler(ChainConfig{}); err != nil {
		t.Fatalf("Handler refused static-credential auth with no RateLimitKey: %v", err)
	}
}
