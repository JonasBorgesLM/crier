# ADR-0016: Export worker topology and failure isolation

## Status
Accepted — resolves the open question left by ADR-0013

## Context
ADR-0013 requires that a slow or unhealthy destination must not serialize its
siblings, but deliberately left the mechanism open. Two were on the table:

1. **Per-exporter worker pools.** Each exporter gets its own queue and its own
   workers. A slow destination fills only its own queue.
2. **Concurrent dispatch inside `FanOut`.** One shared queue; `FanOut`
   dispatches a batch to every exporter at once and waits for all of them.

Retry now lives *below* fan-out (ADR-0013), so an exporter holds whatever
worker is dispatching to it for the whole of its backoff. That makes this
question load-bearing rather than stylistic.

Option 1 isolates best, but it buys that isolation with a second buffering
layer per exporter. With three destinations, the memory actually held is three
times the configured buffer bound, in queues that ADR-0002's capacity
accounting and ADR-0011's fair share know nothing about, with their own
overflow paths and therefore their own drop reasons. ADR-0015 states that the
buffer's bound is never relaxed to accommodate a failing exporter; N private
queues relax it by a factor of N, in a place no operator is looking.

Option 2 keeps one bound and one set of drop reasons, but a single dispatch
loop means the slowest exporter sets the drain rate for everything: while
`FanOut` waits on a destination in backoff, no batch moves to any destination.

## Decision
**Concurrent dispatch inside `FanOut`, driven by a bounded pool of dispatch
workers over the single shared buffer.**

- `FanOut.Export` starts one goroutine per exporter, waits for all of them, and
  joins the errors. It performs no retry of its own (ADR-0013).
- A `Dispatcher` runs *W* workers, each looping: dequeue a batch, hand it to
  the fan-out, account for the result. *W* bounds how many batches are in
  flight; it does not multiply buffered memory, because a batch in flight has
  already left the buffer.
- The two levels are different things and are configured separately: `FanOut`'s
  concurrency is the exporter count and is not a tunable; `W` is.

**Per-exporter export timeout.** `FanOut` applies a bounded deadline to each
exporter's `Export` call. Without it a destination that accepts a connection
and then never answers holds a worker forever, and no breaker helps — a call
that never returns never reports a failure. The timeout is what converts a hang
into a failure the breaker can count.

**What trips a breaker.** Every error from a wrapped exporter counts as a
failure, *except* a context cancellation caused by our own shutdown — that is
crier's state, not the destination's health. Permanent failures (ADR-0013) count
too, and that is the deliberate part: a destination rejecting every batch on a
stale credential is, from crier's side, as unavailable as one refusing
connections, and it is exactly the case ADR-0015 needs readiness to surface.
The cost is understood — a run of malformed payloads opens a circuit and takes
healthy batches down with it — and accepted, because a crier that emits
payloads its own exporter cannot encode is broken in a way that should be loud.

## Consequences
- With every exporter slow at once, all *W* workers end up waiting on them, and
  throughput falls to *W* batches per slow-call duration. Bounding that is the
  per-exporter breaker's job (ADR-0013): once open it fails fast, and the
  worker returns to serving healthy destinations. Isolation under sustained
  slowness comes from the breaker and the timeout, not from separate queues.
- Because several exporters observe the same batch concurrently, the batch and
  its records are read-only during `Export`. An exporter that must mutate what
  it was given declares it, and `FanOut` hands that one a clone. Defaulting to
  a clone per exporter would put a deep copy of every batch on the hot path to
  protect against something almost no exporter does.
- One shared queue keeps a single answer to "how many records are buffered",
  which is what ADR-0011's fair share divides and what `BufferDepth` reports.
- A durable per-exporter store would change this calculus, since the memory
  objection disappears. That is ADR-0002's deferred `BufferStore`, and if it
  arrives this ADR should be revisited rather than assumed to still hold.
