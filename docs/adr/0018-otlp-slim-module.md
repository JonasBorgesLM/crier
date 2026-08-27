# ADR-0018: The OTLP proto module is the slim variant

## Status
Accepted — refines ADR-0017, and is enforced in CI

## Context
ADR-0017 chose OTLP/HTTP with binary protobuf and named
`go.opentelemetry.io/proto/slim/otlp` as the source of the generated types. It
recorded the reason in a sentence, alongside a dozen other decisions about
transport and failure classification. That is not enough for a decision whose
whole risk is that it looks arbitrary from the outside.

The canonical module, `go.opentelemetry.io/proto/otlp`, keeps the gRPC service
stubs in the *same package* as the request message. `ExportLogsServiceRequest`
and the gRPC client live in `collector/logs/v1` together, so importing the
message imports the client, and the module graph gains
`google.golang.org/grpc` — which declares `go 1.25.0`.

Under NFR2 a library module declares the lowest viable floor, because the `go`
directive is a compatibility promise to whoever imports it. `crier`'s is Go
1.24. A dependency declaring 1.25 raises `exporters/otlp` above that floor and
breaks the promise for every consumer still on 1.24 — to gain a gRPC client
this exporter never calls, since it speaks HTTP.

The slim variant is published by the same project from the same protobuf
definitions, and differs in exactly one way: the service stubs are left out. It
is the canonical types without the transport we are not using.

## Decision
`exporters/otlp` imports `go.opentelemetry.io/proto/slim/otlp`. Neither `core`
nor any exporter module may depend on `google.golang.org/grpc`.

A future gRPC exporter is not foreclosed. It would be its own module under
`exporters/`, where a gRPC dependency is the point rather than an accident, and
where its own floor is its own business.

## Consequences
- **The reason is invisible at the import site.** `slim/otlp` and `otlp` expose
  the same type names; code that used one compiles against the other after
  changing a single import line. Someone reaching for a symbol the slim
  variant does not carry — or an IDE completing the more obvious module path —
  reverts this decision without ever seeing a reason not to. The failure then
  surfaces as an unexplained toolchain error in a consumer's build, at which
  point nobody is looking at this file.

  So the decision is enforced rather than documented: CI fails when
  `google.golang.org/grpc` appears in the module graph of `core` or
  `exporters/otlp` (`.github/workflows/ci.yml`, job `no-grpc-in-libraries`). A
  comment does not survive a refactor. The guard does, and it names this ADR in
  its failure message so the next person meets the reasoning at the moment they
  need it.
- The guard is deliberately about the *module graph* rather than the import
  list. A `require` line is enough to constrain version selection and drag the
  floor up, whether or not any package is imported yet, so the graph is where
  the damage actually begins.
- If upstream ever merges the slim variant back into the canonical module, or
  splits the service stubs into their own package, this ADR should be revisited
  rather than worked around — the guard would then be protecting against
  something that no longer exists.
- The slim module tracks the canonical one release for release, so this costs
  nothing in currency. It does mean two module paths to keep straight when
  reading upstream release notes.
