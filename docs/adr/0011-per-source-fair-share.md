# ADR-0011: Per-source fair-share buffer admission

## Status
Accepted

## Context
In standalone/sidecar mode several services share one `crierd` instance
and therefore one buffer. The backpressure policy in ADR-0002 operates on the
buffer as a whole, which means it is first-come-first-served: a single service
in a crash loop, emitting DEBUG at high volume, can occupy the entire buffer
and cause every other service to receive `503` (or, under `drop-oldest`, to
have its records evicted).

This is an availability problem even with no attacker present — and the
records most likely to be starved are exactly the low-volume, high-value ones
(audit events from `gateway-auth`, for instance), because a noisy neighbour
drowns them out precisely when an incident is generating volume.

Rate limiting at the HTTP edge via `moat` mitigates request-rate abuse but
does not solve this: a source can stay within its request-rate limit while
still submitting far more *records* than others, and embedded-library use has
no HTTP edge at all.

## Decision
Buffer admission is subject to a per-source fair share. Each authenticated
source (ADR-0008) gets a guaranteed reservation — a floor of buffer capacity
that other sources cannot consume — while unreserved capacity remains freely
available first-come-first-served. A source at its reservation may still use
spare capacity; it simply loses that spare capacity first when the buffer
comes under pressure.

Sources are identified by the attested identity from ADR-0008, never by a
client-supplied field, so a source cannot escape its own quota by renaming
itself.

Reservations are configured per source with a default applied to unlisted
sources. Admission rejections are counted per source (NFR5), so "service X is
being throttled" is directly observable rather than inferred from missing
data.

Quota state is in-process only and does not survive a restart; distributed
quota across replicas is explicitly out of scope for the MVP, consistent with
how `moat` treats memory vs. Redis stores as separate concerns.

## Consequences
- A noisy source degrades primarily itself, which is the property that makes a
  shared instance viable at all.
- Adds a per-record admission check on the hot path (see ADR-0010's note on
  benchmarking with all stages enabled).
- Introduces a second, finer notion of "full" alongside ADR-0002's whole-buffer
  policy: a record can be rejected because *its source's* share is exhausted
  while the buffer still has room. The rejection reason must be distinguishable
  in both metrics and the error returned to the caller, otherwise operators
  will misdiagnose quota rejections as capacity problems and resize the buffer
  pointlessly.
- Reservation configuration must be validated eagerly (NFR4): reservations
  summing above total buffer capacity is a configuration error that should
  fail at startup, not silently under-deliver at runtime.

## Amendment (ADR-0019)
"Reservations are configured per source with a default applied to unlisted
sources" is superseded. A per-source default cannot be guaranteed, because the
number of unlisted sources is unknowable at startup, so granting each of them a
floor admits more records than the buffer holds — and does it silently, which
is the failure this ADR's own consequences warn about.

[ADR-0019](0019-unlisted-sources-share-a-pool.md) replaces it with a single
shared `UnlistedPool`. Everything else here stands.
