# crier

[![CI](https://github.com/JonasBorgesLM/crier/actions/workflows/ci.yml/badge.svg)](https://github.com/JonasBorgesLM/crier/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/JonasBorgesLM/crier/core.svg)](https://pkg.go.dev/github.com/JonasBorgesLM/crier/core)
[![Go Report Card](https://goreportcard.com/badge/github.com/JonasBorgesLM/crier/core)](https://goreportcard.com/report/github.com/JonasBorgesLM/crier/core)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A lightweight Go log-control service and library. It ingests application logs,
normalizes them to the OpenTelemetry Logs data model, redacts what should never
leave the process, and exports to observability backends through a pluggable
`Exporter` — embedded in your binary or as the standalone `crierd` daemon.

> **Status: pre-release.** The design is complete and recorded in
> [16 ADRs](docs/adr/README.md); implementation is in progress across
> [milestones M0–M6](https://github.com/JonasBorgesLM/crier/milestones).
> Sections below marked *(pending)* are reserved, not yet written.

## Why

Most log shippers treat their ingestion endpoint as a trusted internal pipe.
crier does not: it has a [documented threat model](#threat-model), server-derived
source identity, per-source fair-share admission, and fail-closed redaction —
and it states plainly what it does *not* guarantee.

## Delivery semantics — read this first

Stated up front because a reviewer who discovers it later reads it as a bug
(ADR-0009, NFR10):

- **At-least-once.** A record may be exported more than once. Consumers must
  tolerate duplicates.
- **No ordering guarantee across batches.** Within a batch, order is preserved.
- **`202 Accepted` means admitted, not delivered.** It confirms the record
  entered the buffer. Nothing about any backend having stored it.
- **`ObservedTimestamp` is authoritative.** The source-asserted `Timestamp` is
  carried through and exported, but never used for a decision — it may be
  absent, skewed, or falsified.
- **Loss is possible and always counted.** Buffer pressure and shutdown-drain
  timeouts can drop records; every drop increments a counter tagged with its
  reason. Silent loss is a defect (ADR-0015).

## Architecture

*(pending — diagram tracked in M5)*

The pipeline stage order is a contract, not an implementation detail
(ADR-0010). Only the final stage touches the buffer, so filtered and rejected
records never consume buffer memory:

```
transport limits → parse & validate → attest identity → normalize
    → record limits → redact → filter/sample → admit
```

## Composition

Retry and circuit breaking are **per exporter, inside the fan-out**:

```go
exporter := core.FanOut(
    core.Retry(core.CircuitBreaker(otlpExporter)),
    core.Retry(core.CircuitBreaker(otherExporter)),
)
```

Building it the other way round — `Retry(FanOut(a, b))` — is a real defect, not
a style preference. If `b` fails, the whole batch is re-sent and healthy `a`
receives it once per attempt, because an unrelated destination is broken. See
ADR-0013 and audit finding A-1.

## Install

*(pending — tracked in M4/M5)*

## Usage

### Embedded library

*(pending — tracked in M4)*

### Standalone daemon

*(pending — tracked in M4)*

## Threat model

The ingestion endpoint is an attack surface. Threats explicitly considered, and
where each is addressed:

| Threat | Mitigation |
| --- | --- |
| Log forgery by an unauthenticated caller | Per-source authentication, constant-time credential comparison (ADR-0008) |
| Source spoofing via client-asserted `service.name` | Identity derived server-side from the authenticated principal; client fields overwritten and the discrepancy counted (ADR-0008) |
| Volume/cost abuse | Per-source fair-share admission, so a noisy source degrades itself rather than its neighbours (ADR-0011) |
| Resource exhaustion — oversized bodies, unbounded attributes, high cardinality | Configurable input limits and a bounded cardinality guard (ADR-0010) |
| Sensitive data leaked to a third-party backend | Fail-closed redaction covering attributes *and* body (ADR-0014) |
| Forged identity headers behind a proxy | Trusted-proxy mode is opt-in; a config trusting every peer refuses to start (ADR-0008) |

Body redaction is **best-effort and pattern-based**. Structured attributes are
preferred precisely because they can be redacted reliably; a secret interpolated
into free-form text can only be matched heuristically.

## Benchmarks

Measured with every stage enabled — limits, cardinality guard, redaction,
filtering, admission — because that is how the pipeline runs. Apple M1, Go 1.26.

| Path | ns/op | allocs/op |
| --- | --- | --- |
| Full pipeline, message with no credential (the common case) | 2,095 | 8 |
| Full pipeline, message containing a credential (worst case) | 12,180 | 10 |
| Full pipeline, 8 cores contended | 3,689 | 10 |
| Body scan, nothing to mask | 37 | 0 |
| Cardinality guard | 698 | 8 |
| Per-source admission | 104 | 1 |

Body redaction is the cost — everything else together is under 1 µs. That is
the concrete reason structured attributes are preferred over interpolated
message text: attribute-level redaction is reliable *and* cheap.

Full results, including two performance defects this benchmark found and the
one still outstanding, are in [`docs/benchmarks.md`](docs/benchmarks.md).

## Documentation

- [Requirements](REQUIREMENTS.md) — functional, non-functional, ecosystem
- [Architecture Decision Records](docs/adr/README.md) — 16 decisions, with amendments
- [Audit log](docs/audit-log.md) — review passes, findings, and who performed them
- [Contributing](CONTRIBUTING.md)

## License

MIT — see [LICENSE](LICENSE).
