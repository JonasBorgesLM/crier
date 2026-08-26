# crier — Requirements

> Version 0.2 — revised after an architecture review that identified gaps in
> ingestion trust, input limits, delivery semantics, pipeline stage ordering,
> and per-source isolation. See ADR-0008 through ADR-0012.

## Context

`crier` is a lightweight, easily-adoptable Go log-control microservice/library
that ingests application logs, normalizes them to the OpenTelemetry Logs data
model, and exports them to external observability backends through a pluggable
`Exporter` interface. It is designed to work both as an embedded library and as
a standalone sidecar process from the same core engine, and to integrate
naturally with the author's existing Go security/portfolio ecosystem (`moat`,
`gateway-auth`, `task-api`, `security-scanner`).

## Goals

- Make it trivial to add structured, exportable logging to any Go service
  without committing to a specific observability vendor.
- Demonstrate senior-level Go engineering: concurrency, pluggable interfaces,
  multi-module design, self-observability, graceful degradation, and a
  defensible trust model for an ingestion endpoint.
- Serve as the observability layer for the author's own portfolio projects,
  proving real reuse rather than isolated demos.

## Threat Model (summary)

The ingestion endpoint is an attack surface, not a trusted internal pipe.
Threats explicitly considered (detailed in ADR-0008 and ADR-0010):

- **Log forgery** — an unauthenticated caller injecting fabricated records to
  hide activity or to plant misleading audit trails.
- **Source spoofing** — a caller asserting `service.name` (or other resource
  identity) belonging to another service.
- **Volume/cost abuse** — flooding the endpoint to inflate the observability
  backend's bill or to evict legitimate records from the buffer.
- **Resource exhaustion** — oversized bodies, unbounded attribute maps, or
  high-cardinality attributes exhausting memory or degrading the backend.
- **Sensitive data leakage** — secrets/PII accidentally logged upstream being
  forwarded verbatim to a third-party backend (FR7, ADR-0006).

## Functional Requirements

- **FR1** Ingest structured log records via an asynchronous HTTP receiver
  (JSON body, validated, enqueued, `202 Accepted` returned immediately).
- **FR2** Normalize ingested records into an internal `LogRecord` aligned with
  the OpenTelemetry Logs data model: `Timestamp` (source-asserted),
  `ObservedTimestamp` (pipeline-assigned, authoritative), severity, body,
  attributes, resource, and optional `trace_id`/`span_id`.
- **FR3** Buffer records in memory by default, batching by size and/or time
  window, behind a `BufferStore` interface that allows a future durable
  (WAL-based) implementation without changing callers.
- **FR4** Apply an explicit, configurable backpressure policy when the buffer
  is full: block, reject (`503`), or drop-oldest. Default must be documented
  and safe (no silent, undocumented data loss).
- **FR5** Export batches through a pluggable `Exporter` interface; ship an
  OTLP exporter first. Support fan-out to more than one exporter concurrently.
- **FR6** Support per-exporter retry with circuit breaking, so a slow/down
  backend does not stall ingestion or other exporters.
- **FR7** Support field-level redaction/masking of sensitive log attributes,
  applied before buffering, following the masking approach already
  established in `moat`'s `secret` package.
- **FR8** Support severity-based filtering/sampling, applied before buffering
  (global/per-source), with optional additional filtering per exporter.
- **FR9** Expose the same core engine as both an embeddable Go library
  (`crier.New(...).Handler()`) and a standalone binary (`crierd`).
- **FR10** Perform a graceful shutdown on SIGTERM/SIGINT: stop accepting new
  records, drain the buffer, attempt final export within a bounded timeout.
- **FR11** Authenticate ingestion requests in standalone mode and derive
  source identity server-side rather than trusting client-asserted resource
  fields (ADR-0008).
- **FR12** Enforce configurable input limits: maximum request body size,
  maximum records per request, maximum attributes per record, maximum
  key/value lengths, and an attribute cardinality guard (ADR-0010).
- **FR13** Provide per-source fair-share admission to the shared buffer so a
  single noisy source cannot starve others (ADR-0011).
- **FR14** Version the ingestion wire format (`/v1/logs`) with a documented
  compatibility policy (ADR-0012).

## Non-Functional Requirements

- **NFR1** `core` module must have zero third-party runtime dependencies
  beyond the Go standard library.
- **NFR2** Two toolchain policies, not one, because the two kinds of module in
  this repository answer different questions:
  - **Library modules** (`core`, `exporters/<name>`) declare the *lowest
    viable* floor — currently Go 1.24. The `go` directive in a library is a
    compatibility promise: it says who may import this. Whoever imports it
    compiles the code with their own toolchain, so raising the floor strands
    consumers without patching anything for them. A dependency that forces the
    floor up is therefore a defect in the dependency choice, not a reason to
    move the floor (ADR-0018 is one such case, enforced in CI).
  - **Command modules** (`cmd/crierd`) build on the *most recent supported*
    toolchain. Nobody imports a binary, so there is no compatibility to
    preserve — and the toolchain that builds it is the standard library that
    ships inside it. Building a release on an unsupported toolchain
    distributes standard-library code that is known to be unpatched, which
    makes the building toolchain part of the attack surface rather than a
    detail of the build.

  The two are independent: the daemon moving to a newer toolchain says nothing
  about what a library may promise, and a library's floor never constrains what
  the daemon is built with. CI verifies both — library modules build and test
  at their declared floor, and the vulnerability scan runs on the supported
  toolchain (issue #44 tracks bringing the daemon's own build under the second
  policy).
- **NFR3** Multi-module repository: `core`, each `exporters/<name>`, and
  `cmd/crierd` are independently versioned modules.
- **NFR4** Configuration via environment variables and/or a config file,
  validated eagerly at startup (fail-fast on invalid config). Exporter
  credentials are held as masked secrets, never plain strings, following
  `moat`'s `secret.Value` approach.
- **NFR5** The service must expose its own operational health: readiness/
  liveness endpoints (standalone mode) and internal metrics (records
  ingested/dropped/exported/filtered, buffer depth, export latency, retry
  counts, per-source admission rejections).
- **NFR6** CI must run linting (`golangci-lint`), `go vet`, `govulncheck`,
  unit tests, and integration tests (OTLP exporter against a real collector
  via testcontainers).
- **NFR7** Every structural decision is recorded as an ADR under `docs/adr/`.
- **NFR8** Code comments, README, and all documentation in English.
- **NFR9** Conventional Commits, semantic versioning, changelog generated
  from the real API diff per release (not hand-written).
- **NFR10** Delivery semantics are at-least-once with no cross-batch ordering
  guarantee, stated explicitly in the public API contract (ADR-0009).
- **NFR11** `crierd`'s own operational logs must never be ingested by the
  instance producing them (ADR-0005 amendment).

## Ecosystem Integration Requirements

- **IR1** The standalone HTTP receiver's middleware chain is composed using
  `moat` (rate limiting on the ingestion endpoint, security headers, content-
  type validation, request size limits) — a real dependency, not just a
  documented possibility.
- **IR2** Redaction (FR7) and credential handling (NFR4) reuse `moat`'s
  `secret.Value` masking philosophy for consistency across the two projects.
- **IR3** Provide a documented example of `gateway-auth` emitting audit
  events (failed logins, rate-limit hits, rejected tokens) consumed by
  `crier`.
- **IR4** Provide a documented example of `task-api` using `crier` in
  embedded-library mode.
- **IR5** Provide an end-to-end `docker-compose.yml` demo: an example
  service -> `crierd` -> OTel Collector -> a viewable backend (e.g.
  Grafana or Jaeger), runnable with a single command.
- **IR6** Document (not necessarily implement in MVP) using `security-
  scanner` against `crierd`'s own HTTP receiver as a validation step,
  specifically exercising the threat model above.
- **IR7** Support deployment behind `gateway-auth` as a reverse proxy, where
  `crier` derives source identity from the gateway-asserted identity
  instead of client-asserted fields. This mode is strictly opt-in and
  requires explicit trusted-proxy configuration (ADR-0008).

## Out of Scope (MVP)

- Durable (WAL) buffer implementation (interface only, FR3).
- Non-HTTP receivers (stdin, file tail, unix socket) — phase 2.
- Exporters other than OTLP — phase 2+.
- security-scanner integration execution (IR6 is documentation-only for now).
- Multi-tenant quota persistence across restarts (FR13 is in-process only).

## Visibility / Success Signals

- README with architecture diagram, CI/coverage/govulncheck badges.
- Published benchmarks for the pipeline's hot path.
- One-command runnable demo (IR5) that a reviewer can execute without reading code.
- A documented threat model — rare in log-shipper projects and a strong
  differentiator for a security-focused portfolio.
- LICENSE and CONTRIBUTING present from the first tagged release.
