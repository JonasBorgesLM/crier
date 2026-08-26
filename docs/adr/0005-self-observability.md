# ADR-0005: Self-observability

## Status
Accepted

## Context
A tool whose purpose is observability but which cannot itself be observed is
an operational blind spot: if the buffer is filling up or an exporter is
failing silently, operators need to see that without reading logs of the log
pipeline.

## Decision
The core pipeline exposes internal counters/gauges — records ingested,
records dropped (with reason: reject/drop-oldest), records exported per
exporter, buffer depth, export latency, and retry counts — through a metrics
interface decoupled from any specific backend (so it can itself be emitted
via OTLP metrics, or scraped as Prometheus in the standalone binary). The
standalone binary additionally exposes `/healthz` (liveness) and `/readyz`
(readiness, false while draining on shutdown).

## Consequences
- Metrics collection has a small, constant overhead on every pipeline stage;
  this is accepted as the cost of operability.
- The metrics interface, like `Exporter` and `BufferStore`, is an internal
  seam that keeps `core` free of a hard dependency on any specific metrics
  backend.
- Readiness reporting `false` during drain (ADR-0001's shutdown path) allows
  orchestrators (k8s, etc.) to stop routing traffic before the process exits.

## Amendment (NFR11)
Self-observability creates a recursion hazard: if `crierd`'s own
operational logs are ingested by the instance producing them, any error in the
export path generates logs that generate further export attempts. An instance
must never ingest its own operational output. Its self-telemetry is emitted
through the metrics interface described above, and its operational logs go to
stderr (or to a *different* collector), never back into its own receiver.

The metrics set is also extended by later decisions: records filtered
(ADR-0010), truncation and cardinality-cap events (ADR-0010), per-source
admission rejections (ADR-0011), identity-discrepancy counts (ADR-0008),
clock-skew deviations and absent-timestamp counts (ADR-0009), and deprecated
wire-version usage (ADR-0012).

The last two are separate counters, not one. An absent `Timestamp` is not a
clock that is wrong by the distance to the zero time, and folding it into the
skew statistic swamps the number an operator uses to find a source whose clock
is broken. ADR-0009 already treats the two cases apart — absent means
`ObservedTimestamp` is used at export, deviation beyond the threshold is what
gets counted — so this enumeration follows it rather than deciding anything
new.
