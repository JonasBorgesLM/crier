package httpreceiver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

// Config configures a Receiver. Build one with New, which validates eagerly
// (NFR4).
type Config struct {
	// Pipeline receives every accepted record. Required.
	Pipeline *core.Pipeline

	// Auth establishes the source identity. Required: an ingestion endpoint
	// with no authentication accepts forged telemetry from anyone who can
	// reach it, which is the first threat in the model.
	Auth Authenticator

	// Deprecated marks wire versions scheduled for removal, mapping each to
	// the sunset date to advertise. A request on such a version is still
	// served, with a Deprecation header and a counted metric, so a migration
	// is driven by data rather than guesswork (ADR-0012).
	Deprecated map[WireVersion]string

	// Metrics receives receiver-level counters. Nil discards.
	Metrics core.Metrics

	// Now supplies ObservedTimestamp. Nil means time.Now.
	Now func() time.Time
}

// Receiver is crier's HTTP ingestion endpoint (FR1, ADR-0001).
//
// It validates, admits, and answers 202 immediately; export happens on the
// dispatcher's own workers. That is what keeps a caller's request latency
// independent of whether a backend is healthy.
//
// Safe for concurrent use.
type Receiver struct {
	pipeline   *core.Pipeline
	auth       Authenticator
	deprecated map[WireVersion]string
	metrics    core.Metrics
	now        func() time.Time
}

// New validates cfg and returns the receiver.
func New(cfg Config) (*Receiver, error) {
	if cfg.Pipeline == nil {
		return nil, errors.New("receiver needs a pipeline")
	}
	if cfg.Auth == nil {
		return nil, errors.New("receiver needs an Authenticator; an unauthenticated ingestion endpoint accepts forged telemetry from any peer that can reach it (ADR-0008)")
	}

	rc := &Receiver{
		pipeline:   cfg.Pipeline,
		auth:       cfg.Auth,
		deprecated: cfg.Deprecated,
		metrics:    cfg.Metrics,
		now:        cfg.Now,
	}
	if rc.metrics == nil {
		rc.metrics = core.NopMetrics{}
	}
	if rc.now == nil {
		rc.now = time.Now
	}
	return rc, nil
}

// Mux serves every version this receiver speaks, without the request-level
// guards.
//
// Handler wraps it in the recommended chain. This is what to compose a
// different chain around — but the guards it omits are real, so replacing them
// means providing equivalents, not dropping them.
func (rc *Receiver) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+V1.Path(), rc.logsHandler(V1))
	return mux
}

// ingestResponse is what a caller gets back.
//
// Both numbers are reported because a batch can be partly accepted, and a
// caller that re-sends the whole batch on a partial rejection duplicates the
// part that was already taken.
type ingestResponse struct {
	// Accepted is how many records the receiver took responsibility for.
	//
	// It is not how many were exported — acceptance is not delivery
	// (ADR-0009) — and it is not how many are sitting in the buffer either: a
	// record removed by the severity threshold or the sampler was handled
	// exactly as configured and is counted here too. Filtering is not
	// failure, so it cannot be reported as one (ADR-0010).
	//
	// The two are not broken apart because the pipeline does not separate
	// them per request, deliberately: how many records a server's own policy
	// discarded is an operator's question, answered by the RecordsFiltered
	// metric, not the caller's.
	Accepted int `json:"accepted"`
	// Rejected is how many the receiver refused.
	Rejected int `json:"rejected,omitempty"`
	// Reason explains the rejected ones, when there are any.
	Reason string `json:"reason,omitempty"`
}

// logsHandler serves one wire version.
//
// A 202 means the records were admitted to the buffer. It does not mean any
// backend has stored them: delivery is at-least-once and acceptance is not
// delivery (ADR-0009). A caller that reads 202 as "stored" will be wrong
// during every export outage.
func (rc *Receiver) logsHandler(version WireVersion) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sunset, deprecated := rc.deprecated[version]; deprecated {
			// RFC 8594's header, plus the counter that makes removal a
			// decision rather than a guess (ADR-0012).
			w.Header().Set("Deprecation", "true")
			if sunset != "" {
				w.Header().Set("Sunset", sunset)
			}
			rc.metrics.DeprecatedWireVersion(string(version))
		}

		source, err := rc.auth.Authenticate(r)
		if err != nil {
			// The error is not echoed: it is about a credential, and this
			// path is reachable by anyone who can open a connection.
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}

		records, err := decodeV1(r.Body, rc.now())
		if err != nil {
			var bad *BadRequestError
			if errors.As(err, &bad) {
				// Safe to return: it describes the caller's own payload, and
				// naming the field is the point (ADR-0012).
				writeError(w, http.StatusBadRequest, bad.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "invalid request")
			return
		}

		// source is the attested principal. Passing it here is what overwrites
		// any identity the client asserted in the body, and counts the
		// discrepancy (ADR-0008, finding D-2).
		accepted, admitErr := rc.pipeline.AdmitBatch(r.Context(), records, source)
		rc.respond(w, len(records), accepted, admitErr)
	})
}

// respond turns the pipeline's outcome into a status.
func (rc *Receiver) respond(w http.ResponseWriter, sent, accepted int, err error) {
	if err == nil {
		writeJSON(w, http.StatusAccepted, ingestResponse{Accepted: accepted})
		return
	}

	status, reason := statusFor(err)

	if accepted > 0 {
		// Part of the batch is in the buffer. Answering with the failure
		// status invites the caller to re-send all of it, duplicating the
		// part that was already accepted — the same amplification ADR-0013
		// forbids on the export side, arriving from the other direction.
		writeJSON(w, http.StatusAccepted, ingestResponse{
			Accepted: accepted,
			Rejected: sent - accepted,
			Reason:   reason,
		})
		return
	}
	writeJSON(w, status, ingestResponse{Rejected: sent, Reason: reason})
}

// statusFor maps a pipeline error to a response.
//
// Capacity pressure and a source's own quota get different statuses on
// purpose: they call for different responses from the caller, and ADR-0011
// requires the two to stay distinguishable rather than both surfacing as "the
// server is busy".
func statusFor(err error) (status int, reason string) {
	switch {
	case errors.Is(err, core.ErrSourceQuotaExhausted):
		// This source is over its share while the buffer still has room.
		// Backing off is the caller's move, and 429 is how that is said.
		return http.StatusTooManyRequests, "source quota exhausted"
	case errors.Is(err, core.ErrBufferFull):
		return http.StatusServiceUnavailable, "buffer full"
	case errors.Is(err, core.ErrBufferClosed):
		return http.StatusServiceUnavailable, "shutting down"
	case errors.Is(err, core.ErrRedactionFailed):
		// Fail-closed: the record was dropped rather than exported unmasked
		// (ADR-0014). It is the server's fault, and the caller cannot fix it
		// by re-sending.
		return http.StatusInternalServerError, "redaction failed"
	default:
		return http.StatusInternalServerError, "could not admit the records"
	}
}

func writeJSON(w http.ResponseWriter, status int, body ingestResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The response is two small integers and a fixed string; a write failure
	// here means the client hung up, which nothing in this handler can act on
	// — the records are already admitted either way.
	//
	//nolint:errcheck,gosec // the status line is already sent; there is no second answer to give
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, ingestResponse{Reason: reason})
}

// String implements fmt.Stringer.
func (rc *Receiver) String() string {
	return fmt.Sprintf("httpreceiver serving %s", V1.Path())
}
