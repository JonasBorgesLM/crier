# ADR-0012: Ingestion wire format versioning

## Status
Accepted

## Context
The ingestion endpoint is a published contract consumed by services the
`crier` maintainer does not control (and, in the portfolio's case, by
`task-api` and `gateway-auth`, which are versioned independently). Changing
the accepted JSON shape without a versioning strategy breaks every client
simultaneously and gives no migration path.

The multi-module structure (ADR-0003) already forces version discipline on
the Go API surface. The wire format needs the same treatment, and it is a
*separate* contract: the Go API and the HTTP payload can and will evolve at
different rates.

## Decision
The ingestion endpoint is versioned in the path: `/v1/logs`. The wire format
carries its own version, independent of any module's semver.

Compatibility policy within a major version:

- Adding optional fields is backwards-compatible and allowed.
- Unknown fields in a request are rejected rather than silently ignored.
  Silent tolerance hides client bugs — a service that misspells `severity`
  and gets `202` will look healthy while emitting unusable records. Strict
  rejection surfaces the mistake at integration time.
- Removing a field, changing its type, or tightening validation is a breaking
  change and requires a new path version.
- Two adjacent major versions may be served concurrently during a migration
  window, with the older one reporting its deprecation through a documented
  response header and a metric counting its use, so the migration can be
  driven by data rather than by guesswork.

## Consequences
- Strict unknown-field rejection will surface as integration friction for
  first-time users; the error response must name the offending field, or the
  policy trades silent bugs for loud confusion, which is not an improvement.
- Serving two versions concurrently means the normalization layer must map
  both onto the same internal `LogRecord`. This is the intended shape anyway
  — `LogRecord` is already the common target for all receivers (ADR-0004) —
  so the cost is bounded to the parsing layer.
- The deprecation metric gives a concrete, defensible criterion for removing
  an old version, which is the kind of operational reasoning worth making
  visible in the README.

## Amendment (ADR-0021)
Two things this ADR leaves implicit are settled in
[ADR-0021](0021-wire-contract-edges.md):

- **"Unknown fields are rejected" has an exception.** `encoding/json` matches
  field names without regard to case, so `servicename` is accepted as
  `serviceName`. Accepted deliberately: the failure this policy exists to
  prevent is a field that silently does nothing, and a misspelling that still
  reaches its intended field loses no data.
- **The response body is inside the contract**, under these same rules, except
  for the text of its `reason` field, which is diagnostic prose.
