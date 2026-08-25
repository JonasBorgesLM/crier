# ADR-0003: Multi-module repository structure

## Status
Accepted

## Context
Exporters pull in different, potentially heavy third-party SDKs (OTLP now,
Datadog/Loki/Elasticsearch later). A single-module layout would force any
consumer of `core` — including embedded-library users — to transitively
depend on every exporter's SDK, even unused ones.

## Decision
The repository is split into independently versioned Go modules:
- `core` — the engine (receiver contracts, pipeline, buffer, exporter
  interface). Zero third-party runtime dependencies.
- `exporters/<name>` — one module per exporter (starts with `exporters/otlp`).
- `cmd/crierd` — the standalone binary, its own module, depending on
  `core` and whichever exporters it ships with.

This mirrors the `core` / `redisstore` split already validated in `moat`.

## Consequences
- Embedding `crier` as a library only pulls in `core` plus the specific
  exporter(s) the consumer chooses.
- Each module has its own `go.mod`, versioning, and release tags, requiring
  the same release discipline already established for `moat` (tag naming,
  changelog per module, retraction process if a bad version is published).
- Slightly more repository ceremony (multiple `go.mod`/`CHANGELOG` files) in
  exchange for a materially lighter dependency footprint for `core` consumers.
