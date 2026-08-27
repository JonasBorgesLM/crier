# ADR-0002: Buffer implementation and backpressure policy

## Status
Accepted

## Context
Buffering decouples ingestion from export, but the buffer is finite. Two
questions need explicit answers: how records are held before export, and what
happens when capacity is exhausted. Leaving this implicit was identified as a
gap during project review — an unspecified backpressure behavior is a form of
silent, undocumented data loss risk.

## Decision
- Default buffer implementation is in-memory, batching records by size or by
  a time window (whichever triggers first).
- The buffer is defined behind a `BufferStore` interface so a future durable
  implementation (e.g. an append-only WAL on disk) can be added without
  changing callers. No durable implementation ships in the MVP.
- Backpressure policy is explicit and configurable, with three supported
  modes: `block` (sender waits), `reject` (receiver returns `503` once the
  buffer is full), and `drop-oldest` (oldest unbatched record is evicted to
  make room). Default is `reject`, since silent drops should never be the
  out-of-the-box behavior — a caller must opt into `drop-oldest` knowingly.

## Consequences
- The interface boundary (`BufferStore`) mirrors the `Store` pattern already
  used in `moat` (memory vs. Redis), keeping architectural vocabulary
  consistent across the author's projects.
- `reject` as the safe default means downstream services must handle `503`
  from the ingestion endpoint; this needs to be called out prominently in
  the README and client examples.
- Buffer depth becomes one of the core internal metrics (see ADR-0005).

## Amendment (ADR-0011)
This ADR's `DropPolicy` operates on the buffer as a whole and is therefore
first-come-first-served across sources. ADR-0011 adds a per-source fair-share
admission check in front of it, introducing a second rejection reason
("this source's share is exhausted") distinct from "the buffer is full".
Both must be separately observable.

## Amendment (ADR-0015)
This ADR did not describe the end state when an exporter is unavailable long
enough for the buffer to fill. ADR-0015 records that behavior explicitly: the
bound is never relaxed, the degraded state is surfaced through readiness and a
dedicated metric, and drops caused by a failing backend are counted separately
from drops caused by capacity pressure.
