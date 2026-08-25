# ADR-0009: Delivery semantics and timestamp authority

## Status
Accepted

## Context
Two contracts were implied by earlier decisions but never written down, which
is how consumers end up depending on guarantees the system does not provide.

**Delivery.** ADR-0001 returns `202 Accepted` before export, and FR6 adds
retry. Together these mean: acceptance is not delivery, and retry can produce
duplicates. Neither was stated.

**Timestamps.** `LogRecord` carried a single `Timestamp` supplied by the
caller. A client with a skewed clock — or a malicious one — can therefore
place records anywhere on the timeline, including the future or before an
incident window, which undermines the pipeline as an audit trail.

## Decision

**Delivery semantics are at-least-once, with no cross-batch ordering
guarantee.** Specifically:

- `202 Accepted` means the record was admitted to the buffer, nothing more.
  Records may still be dropped afterwards under the configured `DropPolicy`
  (ADR-0002) or lost on an ungraceful process termination while the in-memory
  buffer is the active `BufferStore`.
- Retries may deliver the same batch more than once. Exporters and downstream
  backends must tolerate duplicates; `crier` does not deduplicate.
- Records within a single batch preserve ingestion order. No ordering is
  guaranteed between batches, between sources, or across exporters, because
  export workers run concurrently and retry independently.

This contract is documented in the README and in the public API doc comments,
not only in this ADR — an undocumented guarantee is one consumers will assume.

**Timestamp authority is split**, following the OTel Logs data model:

- `Timestamp` — when the event occurred according to the source. Optional and
  untrusted; carried through as-is for correlation.
- `ObservedTimestamp` — when the pipeline received the record. Always set by
  `crier`, never accepted from the caller. This is the authoritative field
  for retention, ordering, and any audit reasoning.

When `Timestamp` is absent, `ObservedTimestamp` is used in its place at export
time. When `Timestamp` deviates from `ObservedTimestamp` beyond a configurable
threshold, the record is still accepted (dropping telemetry because a clock is
wrong is worse than the skew) but the deviation is counted as a metric so the
misconfiguration is visible.

## Consequences
- `LogRecord` gains an `ObservedTimestamp` field; the existing draft in
  `core/record.go` must be updated accordingly before implementation begins.
- Choosing at-least-once over at-most-once means duplicate records are a
  normal operating condition, which must be called out prominently — a
  reviewer seeing duplicates without this documented would read it as a bug.
- Exactly-once is explicitly not attempted: it would require deduplication
  state and coordination well beyond the scope and value of this project.
