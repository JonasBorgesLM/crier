# Architecture Decision Records

Every structural decision in `crier` is recorded here (NFR7). An ADR is never
edited to reflect a later change of mind: it is amended in place with a section
naming the ADR that superseded part of it, so the reasoning that was current at
the time remains readable.

## Index

| ADR | Title | Status | Amended by |
| --- | --- | --- | --- |
| [0001](0001-async-http-receiver.md) | Asynchronous HTTP receiver | Accepted | — |
| [0002](0002-buffer-and-backpressure-policy.md) | Buffer implementation and backpressure policy | Accepted | ADR-0011, ADR-0015 |
| [0003](0003-multi-module-structure.md) | Multi-module repository structure | Accepted | — |
| [0004](0004-otel-schema-and-trace-correlation.md) | OpenTelemetry-aligned schema and trace correlation | Accepted | ADR-0008, ADR-0009 |
| [0005](0005-self-observability.md) | Self-observability | Accepted | NFR11, ADR-0010, ADR-0011, ADR-0012 |
| [0006](0006-redaction-strategy.md) | Redaction strategy for sensitive fields | Accepted | ADR-0014 |
| [0007](0007-ecosystem-integration.md) | Integration with the existing project ecosystem | Accepted | — |
| [0008](0008-ingestion-auth-and-trust-boundary.md) | Ingestion authentication and source identity trust boundary | Accepted | — |
| [0009](0009-delivery-semantics-and-timestamps.md) | Delivery semantics and timestamp authority | Accepted | — |
| [0010](0010-pipeline-stage-order-and-input-limits.md) | Pipeline stage ordering and input limits | Accepted | — |
| [0011](0011-per-source-fair-share.md) | Per-source fair-share buffer admission | Accepted | — |
| [0012](0012-wire-format-versioning.md) | Ingestion wire format versioning | Accepted | — |
| [0013](0013-retry-and-fanout-layering.md) | Retry, circuit breaking, and fan-out layering | Accepted — corrects audit finding A-1 | — |
| [0014](0014-redaction-scope-and-failure-policy.md) | Redaction scope and failure policy | Accepted — closes audit findings A-2, A-3 | — |
| [0015](0015-degradation-and-drain-accounting.md) | Degradation under sustained export failure, and drain accounting | Accepted — closes audit findings A-4, A-5 | — |
| [0016](0016-export-worker-topology.md) | Export worker topology and failure isolation | Accepted — resolves ADR-0013's open question | — |
| [0017](0017-otlp-transport-and-encoding.md) | OTLP transport, encoding, and failure classification | Accepted | ADR-0018 |
| [0018](0018-otlp-slim-module.md) | The OTLP proto module is the slim variant | Accepted — enforced in CI | — |

ADR-0008 through ADR-0012 came out of the first review pass; ADR-0013 through
ADR-0015 out of the second. Both passes are recorded in
[`../audit-log.md`](../audit-log.md). ADR-0016 and ADR-0017 came out of
implementing the export layer — the first resolving a question ADR-0013 left
open on purpose, the second deciding how a batch actually leaves the process.
ADR-0018 came out of reviewing that second one: a decision whose reasoning is
invisible at the import site needs a guard, not a sentence.

## Open questions

Decisions deferred deliberately, each owned by an issue on the board. They are
listed here rather than left implicit, because an undecided question that looks
decided is the one that gets implemented by accident.

None outstanding. The one that stood here — export worker topology, left open
by ADR-0013 — was decided in
[ADR-0016](0016-export-worker-topology.md) before the export layer was written.

## Conventions

- Filename: `NNNN-kebab-case-title.md`, numbered sequentially, never reused.
- Sections: `Status`, `Context`, `Decision`, `Consequences`; amendments append
  an `## Amendment (ADR-XXXX)` section.
- `Status` is one of `Proposed`, `Accepted`, `Superseded by ADR-XXXX`, and may
  carry a short qualifier explaining what the ADR resolves.
- A decision that reverses an earlier one supersedes it; it does not rewrite it.
