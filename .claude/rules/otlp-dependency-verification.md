---
paths:
  - 'exporters/otlp/**/*.go'
  - 'exporters/otlp/go.mod'
description: 'Verify third-party API against the pinned version before changing the OTLP exporter'
---

# Verify against the pinned version, not training data

`exporters/otlp` is the one module allowed third-party dependencies — `core`
is zero-dep by NFR1. Before writing or changing code against
`go.opentelemetry.io/proto/slim/otlp` or `google.golang.org/protobuf`:

1. Read the version actually pinned in `exporters/otlp/go.mod` — not whatever
   version training data assumes is current.
2. Check the real shape of the type or function you're about to use (`go doc`,
   or the source under `$(go env GOMODCACHE)`), rather than pattern-matching
   from memory or from the canonical (non-slim) module's docs.
3. **Never reach for `go.opentelemetry.io/proto/otlp`** (the canonical,
   non-slim module) for a symbol, an import suggestion, or a doc reference.
   It exposes the same type names as `slim/otlp` but also carries the gRPC
   service stubs, which pull in `google.golang.org/grpc` — a dependency that
   declares `go 1.25.0` and would raise this module's floor above the Go 1.24
   NFR2 fixes (ADR-0018). CI enforces this at the module-graph level
   (`no-grpc-in-libraries`), but the point is to never trigger the guard, not
   to rely on it catching you.
4. If a change touches which HTTP status codes count as retryable vs.
   permanent, that's the classification ADR-0017 made deliberately (a 400 is
   never worth retrying; a 503 during a collector restart is) — changing it
   is a decision for that ADR to be amended, not a side effect of a library
   bump.
5. A dependency bump is `build(deps): ...` per the commit convention, and per
   ADR-0018 the CI guard must still pass afterward — check that it ran green,
   don't assume the bump alone is enough (Dependabot PR #59 passed
   `test (exporters/otlp)` while failing `test (cmd/crierd)` on exactly this
   kind of change).
