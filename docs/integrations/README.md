# Ecosystem integrations

crier is one of several related projects and reuses them deliberately rather
than reimplementing (ADR-0007). Two of those relationships are consumer-facing
and documented here:

| Document | Relationship | Requirement |
| --- | --- | --- |
| [`task-api.md`](task-api.md) | `task-api` embeds crier as a library | IR4 |
| [`gateway-auth.md`](gateway-auth.md) | `gateway-auth` emits audit events crier ingests | IR3, IR7 |

A third is not a document but a dependency: crier's receiver takes its rate
limiting, security headers, content-type checks and body limits from
[`moat`](https://github.com/JonasBorgesLM/moat) (IR1), and holds every
credential as `moat`'s `secret.Value` (IR2). That one is visible in
`receivers/http/go.mod`, which is the point — it is a real dependency rather
than a documented possibility.

**These are integration designs, not shipped code.** Neither `task-api` nor
`gateway-auth` is a dependency of this repository, and neither should be: crier
is the thing being depended on. What is written down is the shape each
integration takes and which of crier's decisions it exercises, so the ecosystem
story is checkable rather than asserted.
