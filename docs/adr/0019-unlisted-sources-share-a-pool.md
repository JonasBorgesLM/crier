# ADR-0019: Unlisted sources share a pool, not a per-source default

## Status
Accepted — supersedes part of ADR-0011

## Context
ADR-0011 says reservations are "configured per source with a default applied to
unlisted sources". That reads naturally and does not survive contact with the
arithmetic.

A reservation is a *guarantee*: capacity no other source can take. Guaranteeing
each unlisted source a floor of *d* requires holding back *d × n*, where *n* is
how many unlisted sources will appear. That number is unknowable at startup —
it is whoever authenticates, over the life of the process. Any fixed choice is
wrong in one of two directions: too small and the guarantee is not a guarantee,
too large and the buffer is mostly reserved for sources that never arrive.

Implementing it as written admits more records than the buffer holds, and the
symptom is not a crash. The inner store rejects with `ErrBufferFull` while
quota accounting still believes there is room, so capacity pressure surfaces as
though the buffer were fine — the one thing ADR-0011's own consequences say
must not happen, since it makes an operator resize a buffer that is not the
problem.

## Decision
Capacity for sources without an explicit reservation is a **single shared
pool**, `UnlistedPool`, not a per-source default.

The invariant is arithmetic and checked at startup (NFR4):

    sum(reservations) + UnlistedPool + spare == capacity

An unlisted source draws from the pool first-come-first-served, then from
spare, exactly as a listed source draws from its own floor and then from spare.
Everything else in ADR-0011 stands: identity is the attested principal,
rejections are counted per source, and the quota rejection stays
distinguishable from whole-buffer pressure in both the metric and the error.

## Consequences
- An unlisted source has no individual guarantee. It is not starved — the pool
  is reserved against listed sources taking everything — but one unlisted
  source can exhaust the pool at the expense of another. That is the honest
  version of what a per-source default could never have delivered, and the
  remedy is to give a source that needs a guarantee an explicit reservation.
- Configuration gains a number an operator must choose. A default of zero would
  mean unlisted sources compete only for spare, which is defensible but makes
  the first unlisted source's experience depend entirely on how much spare the
  listed reservations left over.
- `TestAdmissionNeverExceedsCapacity` covers the invariant across
  configurations, because this is the class of bug that hides rather than
  crashes.
