# ADR-0015: Degradation under sustained export failure, and drain accounting

## Status
Accepted — closes audit findings A-4 and A-5

## Context
**A-4.** FR6 claims circuit breaking prevents a failing backend from stalling
ingestion. With a bounded buffer this is only true for short outages. If a
destination is unavailable for an extended period, the buffer fills regardless
of any circuit breaker, and ingestion begins rejecting under ADR-0002's
`DropPolicy`. The end state was never described, so the system had no defined
behavior for its most likely serious failure: a backend down for an hour.

**A-5.** FR10's graceful shutdown drains the buffer within a bounded timeout.
Nothing specified what happens to records still unexported when that timeout
expires. They are discarded — silently. This is precisely the undocumented data
loss that ADR-0002 forbids on the ingestion path, reappearing at shutdown.

## Decision

**Sustained failure.** The behavior is stated explicitly rather than left to
emerge: with a bounded in-memory buffer and a destination that stays
unavailable, telemetry *will* be lost, and `crier` chooses to lose it
loudly rather than to grow without bound. Specifically:

- The buffer's bound is never relaxed to accommodate a failing exporter.
  Unbounded growth converts an observability outage into an outage of the host
  application, which is a strictly worse failure.
- Once every configured exporter's circuit is open, the pipeline enters a
  degraded state that is reported through readiness (`/readyz` returns not
  ready) and through a dedicated metric, so an orchestrator and an operator
  both see it without inspecting logs.
- Records dropped in this state are counted separately from records dropped
  under normal backpressure. "Dropped because the backend is down" and
  "dropped because we are over capacity" call for different responses and must
  not share a counter.
- A durable `BufferStore` (ADR-0002, out of scope for MVP) is the actual remedy
  for long outages. This ADR records that the MVP knowingly does not have one,
  so the gap is a documented limitation rather than a discovered surprise.

**Drain accounting.** At shutdown, records that remain unexported when the
drain timeout expires are counted and reported in a final summary line before
exit, stating how many records were lost and to which exporters. The drain
timeout is configurable. Loss at shutdown remains possible — bounding shutdown
time is a hard requirement in any orchestrated environment — but it is never
silent.

## Consequences
- `/readyz` now reflects export health, not just process liveness. In an
  orchestrated deployment this can remove the instance from service while all
  exporters are down, which is the desired behavior for a sidecar but must be
  documented, since an operator could otherwise read it as a crash loop.
- Separate drop counters slightly expand the metrics surface (ADR-0005) in
  exchange for making the two failure modes distinguishable at a glance.
- Explicitly documenting expected loss during long outages is uncomfortable but
  honest, and it is the kind of stated limitation that distinguishes a
  considered design from an optimistic one.
