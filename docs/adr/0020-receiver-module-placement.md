# ADR-0020: The HTTP receiver is its own module

## Status
Accepted — extends ADR-0003

## Context
ADR-0003 names three kinds of module: `core`, `exporters/<name>`, and
`cmd/crierd`. It puts "receiver contracts" in `core` and never says where a
receiver *implementation* goes, because at the time there was none.

There is now, and it cannot go in `core`. IR1 requires the ingestion endpoint
to take its rate limiting, security headers, content-type checks and body
limits from `moat` rather than reimplement them, and NFR1 requires `core` to
have zero third-party runtime dependencies. Those two are only compatible if
the receiver lives somewhere else.

The pressure to bend NFR1 "just for one import" is exactly why it is enforced
in CI rather than trusted to review. The rule is not the problem here; the
missing module is.

## Decision
HTTP ingestion lives in **`receivers/http`**, an independently versioned module
depending on `core` and on `moat`.

Plural `receivers/`, mirroring `exporters/`, because the same reason for one
applies to the other: a second receiver — gRPC, syslog, a file tailer — is a
different dependency graph that nobody should inherit for using the first.

`core` keeps what ADR-0003 gave it: the record model, the pipeline the receiver
feeds, and the contracts. Nothing about HTTP moves into it.

## Consequences
- An embedded consumer (`task-api`, IR4) imports `core` and gets no HTTP server,
  no `moat`, and no receiver at all. That is the point of the split: the
  embedded case has no receiver, because the host application owns that
  boundary (FR11).
- `cmd/crierd` gains a dependency on `receivers/http`. It stays the thin shell
  ADR-0003 describes — assembling receiver, pipeline and exporters from
  configuration.
- CI's module fan-out picks the new module up automatically; the NFR1 guard
  continues to watch `core` alone, which is the module the rule is about.
- Four modules now version independently. That is the cost ADR-0003 accepted
  in exchange for consumers not inheriting graphs they do not use, and this is
  the case it was accepted for.
