# ADR-0017: OTLP transport, encoding, and failure classification

## Status
Accepted

## Context
FR5 requires an OTLP exporter, and ADR-0004 aligned `LogRecord` with the
OpenTelemetry Logs data model so that exporting it is a mapping rather than a
translation. What was never decided is how the payload actually leaves the
process: gRPC or HTTP, which encoding, whose generated types, and — the part
that decides whether the retry and circuit-breaking layer works at all — which
responses mean "try again" and which mean "never".

That last question is not cosmetic. ADR-0013 has the retry decorator honour a
sentinel for permanent failures, but a sentinel is only as good as the
classification behind it: an exporter that reports a 400 as retryable spends
its whole budget on a batch no backend will ever accept, and one that reports a
503 as permanent throws away telemetry during a routine collector restart.

## Decision

**OTLP/HTTP with binary protobuf.** Not gRPC, for the MVP.

- It matches the posture the rest of the project already has: ADR-0001 makes
  the receiver HTTP, and one protocol stack is easier to secure, proxy, and
  debug than two.
- The dependency argument is decisive rather than aesthetic. The canonical
  `go.opentelemetry.io/proto/otlp` module carries the gRPC service stubs in the
  same package as the request message, so importing the request pulls in
  `google.golang.org/grpc` — which declares `go 1.25.0` and would raise this
  module's floor above the Go 1.24 that NFR2 fixes. Convenience is exactly the
  reason NFR2 says not to raise it.
- gRPC remains available later as a second exporter module. Nothing here
  forecloses it.

**Generated types from `go.opentelemetry.io/proto/slim/otlp`.** It is the
upstream-published variant of the same generated code with the service stubs
left out — the canonical types, without the transport we are not using. Writing
the wire encoding by hand was considered and rejected: this project reuses
rather than reimplements (ADR-0007), and a hand-rolled encoder's bugs surface as
silently malformed telemetry at the far end.

**Failure classification.** The exporter answers ADR-0013's question directly:

| Response | Classification | Why |
| --- | --- | --- |
| 200 / 202, no rejections | success | — |
| 200 with `partial_success.rejected_log_records` | permanent | The contract in `core.Exporter` is that partial success is an error; those records were refused on their merits, and re-sending them changes nothing |
| 400, 401, 403, 404, 405, 409, 413, 422, 501 | permanent | Malformed, unauthenticated, unauthorised, wrong route, too large. Retrying an identical payload gets an identical answer |
| 408, 429 | retryable | Explicitly "later" |
| 500, 502, 503, 504 | retryable | The destination is having a moment |
| any other 4xx | permanent | Unknown client-side rejection; the conservative reading is that it is our fault |
| any other 5xx | retryable | Unknown server-side failure; the conservative reading is that it is theirs |
| transport error, timeout | retryable | Nothing reached the backend |

**`Retry-After` is honoured.** A 429 or 503 that names a delay is answered by
waiting at least that long. Ignoring it and retrying on our own 100 ms backoff
is how a rate-limited sender turns a throttle into an outage.

Mechanically this needs the retry decorator to see a hint from an error it
otherwise treats opaquely, so `core` gains one small optional interface —
`RetryHint`, an error that can state its own delay — and `Retry` waits for the
longer of its backoff and the hint. The bound is the export deadline
(ADR-0016), not the hint: a destination asking for an hour gets the deadline,
and the batch fails rather than parking a worker for an hour.

**Credentials are `secret.Value` from `moat`** (ADR-0007, NFR4, IR2). The
exporter never holds a token as a `string`, so a config dump, a log line, or a
panic cannot print one.

**Compression is gzip by default.** Log batches are extremely compressible and
every collector accepts it, so the default that saves bandwidth is the right
one; it is configurable for a destination that does not.

## Consequences
- `exporters/otlp` depends on `slim/otlp` and `protobuf`, and on `moat` for
  `secret.Value`. `core` stays dependency-free (NFR1) because none of this is
  in it; that separation is what ADR-0003 bought.
- The classification table above is the thing to keep true. It is asserted
  per status code in unit tests and end-to-end against a real collector
  (NFR6), because a mistake in it is invisible until telemetry is already
  gone.
- Honouring `Retry-After` means one batch can hold a worker for the delay the
  destination asked for, up to the export deadline. That is the intended
  trade: the alternative spends the retry budget being rejected.
- A destination that answers 200 while rejecting records is reported as a
  permanent failure for the whole batch, which over-counts — the accepted
  records are counted as failed too. The alternative is splitting a batch by
  the indices a partial-success response does not give us. The count is
  conservative in the honest direction: it reports a problem that is real.
