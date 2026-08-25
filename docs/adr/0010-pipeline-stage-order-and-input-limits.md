# ADR-0010: Pipeline stage ordering and input limits

## Status
Accepted

## Context
Two related gaps surfaced during review.

**Stage ordering was undefined.** FR8 described severity filtering as a
per-exporter concern. If filtering only happens at export time, the pipeline
buffers records it is guaranteed to discard — spending the scarcest resource
in the system (bounded memory) on data that will never leave. The same
argument applies to redaction: ADR-0006 already placed it before buffering so
unmasked values never reach a future durable buffer, but the full ordering was
never written as a single contract.

**Input was unbounded.** `Attributes` is a map with no cap on entry count,
key length, or value length, and no limit existed on request body size or
records per request. A single request with a very large body or a
multi-thousand-entry attribute map is a straightforward memory exhaustion
vector. Separately, unbounded *distinct* attribute values (request IDs, user
IDs, timestamps used as attribute values) cause cardinality explosion, which
degrades and inflates the cost of essentially every observability backend —
an availability and cost problem even when no attacker is involved.

## Decision

**Canonical stage order**, applied to every record regardless of receiver:

1. **Transport limits** — body size cap, enforced before parsing.
2. **Parse & validate** — schema validation, records-per-request cap.
3. **Attest identity** — resource identity overwritten from the authenticated
   principal (ADR-0008).
4. **Normalize** — map to `LogRecord`, assign `ObservedTimestamp` (ADR-0009).
5. **Enforce record limits** — attribute count, key/value lengths,
   cardinality guard.
6. **Redact** — mask sensitive attributes (ADR-0006).
7. **Filter/sample** — severity threshold and sampling.
8. **Admit** — per-source fair-share check (ADR-0011), then `Enqueue`.

Everything cheap and everything reductive happens before the buffer. Only
step 8 touches the `BufferStore`. Per-exporter filtering remains available
after dequeue, but as an *additional* narrowing, never as the only filter.

**Input limits (FR12)** are configurable with safe defaults, and every
rejection is counted by reason:

- maximum request body size (enforced via `moat`'s request-size middleware
  where available, rather than reimplemented);
- maximum records per request;
- maximum attributes per record;
- maximum attribute key and value lengths (values over the cap are truncated
  with a marker rather than dropping the record — losing one oversized field
  is better than losing the event);
- a cardinality guard that tracks distinct values per attribute key within a
  rolling window and, past a threshold, stops forwarding that key's value
  (replacing it with a marker) rather than dropping records.

Limits are enforced on both the standalone receiver and the embedded-library
entry point. Embedded use is not exempt: a bug in the host application can
produce the same unbounded attribute map as a malicious client.

## Consequences
- Buffer memory is spent only on records that will actually be exported,
  making buffer sizing predictable and `DropPolicy` (ADR-0002) meaningful.
- Truncation and cardinality-capping alter data before export. This must be
  visible: truncated values carry an explicit marker and both actions are
  counted as metrics (ADR-0005), so silently altered telemetry is never
  mistaken for source data.
- Steps 5-7 add per-record cost on the hot path. This is accepted, and the
  hot-path benchmark should measure the pipeline with all stages enabled —
  benchmarking only the bare path would publish a number nobody experiences,
  the same reasoning applied to the documented hot-path regression in `moat`.
- The cardinality guard needs bounded state of its own (a rolling
  per-key value set), which itself must be capped so the guard does not become
  the memory leak it exists to prevent.
