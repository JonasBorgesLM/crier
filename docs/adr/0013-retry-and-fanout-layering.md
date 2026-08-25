# ADR-0013: Retry, circuit breaking, and fan-out layering

## Status
Accepted — resolves an open question and corrects a defect found in audit (A-1)

## Context
The draft design had `FanOut` implementing `Exporter`, with the intention of
wrapping an `Exporter` in retry logic. Composed in that order — retry *outside*
fan-out — the design produces duplicate amplification:

Given exporters A and B, if A succeeds and B fails, the joined error causes the
retry wrapper to re-send the entire batch. A receives the batch again despite
having already accepted it. With a five-attempt backoff and a persistently
failing B, a perfectly healthy A receives the same batch five times.

ADR-0009 accepts at-least-once delivery, so occasional duplicates are within
contract. Systematic amplification driven by an unrelated exporter's failure is
not the same thing: it multiplies cost and noise on a healthy destination
because a different destination is broken.

## Decision
Retry and circuit breaking are applied **per exporter, inside the fan-out**,
never outside it:

    FanOut( Retry(CircuitBreaker(ExporterA)), Retry(CircuitBreaker(ExporterB)) )

Each exporter retries only its own failed batch. `FanOut` performs no retry of
its own; it dispatches once per exporter and joins the results. A batch is
considered handled when every exporter has either succeeded or exhausted its
own retry policy.

Retry and circuit breaking are decorators that implement `Exporter`, following
the same composition pattern as `moat`'s `Chain` — composition is explicit and
visible at construction, not hidden inside the pipeline.

Retry policy: bounded attempts with exponential backoff and jitter. Only
failures classified as retryable are retried; an exporter signals a permanent
failure (malformed payload, authentication rejected, 4xx other than 429) with a
sentinel error that the retry decorator honors by giving up immediately. Retrying
a permanently rejected batch wastes the retry budget and delays every subsequent
batch behind it.

The circuit breaker is per exporter so an unhealthy destination stops consuming
worker time quickly, leaving workers available for healthy destinations.

## Consequences
- `FanOut` no longer needs a retry-aware error type; it joins per-exporter
  results after each has already exhausted its own policy.
- Duplicates still occur (a batch that succeeded but whose acknowledgement was
  lost will be retried) — this remains within ADR-0009's contract. What is
  eliminated is amplification caused by a *sibling* exporter's failure.
- Because retry lives below fan-out, a slow exporter now holds its worker for
  the duration of its backoff. Export workers must therefore be per exporter,
  or the fan-out must dispatch concurrently — otherwise one slow destination
  serializes the others, reintroducing the coupling this ADR removes.
- The decorator layering must be documented in the README's composition
  example, since constructing it in the wrong order silently reintroduces A-1.
