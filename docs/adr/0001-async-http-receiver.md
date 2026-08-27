# ADR-0001: Asynchronous HTTP receiver

## Status
Accepted

## Context
The HTTP receiver is the primary ingestion path. A synchronous design (accept,
process inline, respond only after the record is fully handled) couples the
caller's request latency to the health of the internal pipeline and, by
extension, to the health of downstream observability backends.

## Decision
The HTTP receiver validates the incoming payload, enqueues it onto an internal
buffered channel, and responds `202 Accepted` immediately. A separate, fixed-size
worker pool consumes the channel and performs processing, batching, and export.

A fixed worker pool was chosen over an elastic one: it makes backpressure
behavior predictable (a full channel blocks or rejects deterministically,
per ADR-0002) and is easier to reason about and test than dynamically spawned
goroutines.

## Consequences
- Ingestion latency is decoupled from exporter latency/availability.
- The channel's capacity becomes the primary backpressure control point
  (see ADR-0002 for the policy when it is full).
- Callers only get confirmation that a record was accepted for processing,
  not that it was successfully exported — this must be documented clearly
  in the API contract so consumers don't assume delivery guarantees the
  system doesn't provide.
