# Audit Log

A record of the review passes performed against `crier`'s design and, later,
its implementation. Each pass lists what was found, what closed it, and — the
part most often omitted — **who performed the review**.

> **Provenance.** Every pass recorded below — including the implementation
> audit in Pass 3, which M6 asked to be performed in a clean session — was
> performed by the same author who wrote the design and the code. **No pass has
> been independent.** That is a real limitation of this record and is stated
> here rather than left for a reader to infer. What an audit by its own author
> can find, and what it structurally cannot, is discussed in Pass 3.

## Pass 1 — Design review (pre-implementation)

**Scope.** The initial requirement set and ADR-0001 through ADR-0007, read
against the threat model in `REQUIREMENTS.md`.
**Reviewer.** Design author.
**Outcome.** `REQUIREMENTS.md` revised to v0.2; ADR-0008 through ADR-0012
written to close the gaps.

| ID | Finding | Closed by |
| --- | --- | --- |
| D-1 | The ingestion endpoint was treated as a trusted internal pipe. Nothing authenticated the caller, so log forgery was unmitigated. | ADR-0008, FR11 |
| D-2 | Source identity was taken from client-asserted resource fields. Any caller could attribute its records to another service, defeating attribution, quotas, and audit trails. | ADR-0008, FR11 |
| D-3 | No input limits. Oversized bodies, unbounded attribute maps, and high-cardinality attribute values were all accepted, each a memory-exhaustion path. | ADR-0010, FR12 |
| D-4 | Delivery semantics were never stated. `202 Accepted` implied a delivery guarantee the system does not provide, and timestamp authority was undefined. | ADR-0009, NFR10 |
| D-5 | Pipeline stage ordering was implicit. Nothing prevented filtering or redaction from running *after* buffering, which would let discarded records consume buffer memory and unmasked records reach it. | ADR-0010 |
| D-6 | The buffer was first-come-first-served across sources, so one noisy source could starve every other. | ADR-0011, FR13 |
| D-7 | The wire format was unversioned, leaving no compatible migration path. | ADR-0012, FR14 |

## Pass 2 — Architecture audit (pre-implementation)

**Scope.** The full ADR set (0001–0012) re-read for internal contradictions and
for failure modes left undescribed.
**Reviewer.** Design author.
**Outcome.** ADR-0013 through ADR-0015.

| ID | Finding | Severity | Closed by |
| --- | --- | --- | --- |
| A-1 | **Duplicate amplification.** Retry composed *outside* fan-out re-sends the whole batch when any single exporter fails, so a healthy destination receives the same batch once per attempt because an unrelated destination is broken. | High | ADR-0013 |
| A-2 | **Redaction missed `Body`.** Scope covered `Attributes` only, while accidentally logged secrets most often appear in the message text — a control that creates false confidence while missing the common leak path. | High | ADR-0014 |
| A-3 | **Redaction failure was implicitly fail-open.** Nothing defined what happens when a rule fails, so the default was to export unmasked the very data the stage exists to protect. | High | ADR-0014 |
| A-4 | **No defined end state under sustained export failure.** Circuit breaking only protects against short outages; with a bounded buffer and an hour-long backend outage the system had no described behavior for its most likely serious failure. | Medium | ADR-0015 |
| A-5 | **Silent loss at shutdown.** Records still unexported when the drain timeout expires were discarded with no accounting — the undocumented data loss ADR-0002 forbids on the ingestion path, reappearing at shutdown. | Medium | ADR-0015 |

## Pass 3 — Implementation audit (post-M5, pre-release)

**Scope.** The implemented system read against every accepted ADR and every
requirement in `REQUIREMENTS.md`; the dependency graph of all five modules; and
the declared threat model exercised against a running receiver.
**Reviewer.** The implementation author. **Independence: not satisfied.**
**Outcome.** Four findings, one fixed here, three tracked.

### On the independence this pass does not have

M6 asked for an audit "in a clean session". This was performed in the same
session that wrote the code, by the same author. Restating the outcome is
allowed; dressing it up is not, so:

- **What this pass could still find.** Mechanical checks do not care who runs
  them — the dependency graph, the vulnerability scan, the tag triggers, the
  requirement-by-requirement sweep, and the probes against a running receiver
  all produce evidence independent of the reviewer's beliefs. A-6 through A-9
  came from that half.
- **What it structurally cannot.** An author cannot audit their own model of
  the problem. Every threat in the model is one this author thought of; a
  threat nobody here considered is invisible to a review by the same person, no
  matter how carefully it is conducted. The redaction pattern set is the
  sharpest example: it catches the shapes its author knew to look for, and
  `TestBodyRedactionIsBestEffort` documents that rather than resolving it.
- **What would close it.** A reviewer who did not write this reading
  `REQUIREMENTS.md` and the ADR index cold, and a second pair of eyes on the
  threat model in particular. Neither has happened.

### Findings

| ID | Finding | Severity | Status |
| --- | --- | --- | --- |
| A-6 | **Metrics are collected and never exposed.** NFR5 requires the service to expose internal metrics. `crierd` builds a `CountingMetrics` and wires it through every stage, but the admin listener serves only `/healthz` and `/readyz`. Every counter the pipeline maintains is unreadable from outside the process, which makes the whole self-observability requirement decorative in the standalone binary. | High | Open — [#50](https://github.com/JonasBorgesLM/crier/issues/50) |
| A-7 | **NFR3's text predates ADR-0020.** It lists `core`, `exporters/<name>` and `cmd/crierd` as the module kinds. There are four; `receivers/<name>` was added by ADR-0020 and the requirement was never updated. The ADR is authoritative and the requirement contradicts it. | Low | Open — [#51](https://github.com/JonasBorgesLM/crier/issues/51) |
| A-8 | **`receivers/http` could not be released.** The release workflow's tag trigger listed `core`, `exporters/*` and `cmd/crierd`. Pushing `receivers/http/v0.1.0` would have done nothing, silently. | Medium | Fixed in this pass |
| A-9 | **Every dependent module is unpublishable as it stands.** `exporters/otlp`, `receivers/http` and `cmd/crierd` require `crier/core` at a version that does not exist, masked by a local `replace`. A consumer ignores a dependency's `replace` directives, so `go get` on any of them fails to resolve. This is not a bug in any module; it is a release *order* nobody had written down. | High | Documented — [`RELEASING.md`](../RELEASING.md) |

### Requirement sweep

All thirty-two requirements were read against the implementation. Thirty-one
are met. The exception is **NFR5**, which is met for health endpoints and unmet
for metrics exposure (A-6).

Two requirements are met in a way worth stating precisely rather than ticking:

- **IR6** asks that using `security-scanner` against the receiver be
  *documented*, not necessarily implemented. It was run down and the honest
  result is that its confirmers are `xss-reflected` and `sqli-boolean` — classes
  that do not exist in a JSON ingestion endpoint with no HTML rendering and no
  SQL. Pointing it at `crierd` would produce a clean report proving nothing,
  which is the least useful kind of security artifact. The threat model is
  exercised instead by [`docs/security/probe-threats.sh`](security/probe-threats.sh),
  twelve probes, all passing.
- **NFR9** asks for a changelog generated from the real API diff. The release
  workflow does that with `gorelease`. Note that `gorelease` cannot currently
  run for the dependent modules for the reason in A-9 — which is how A-9 was
  found.

### Dependency graph

Scanned per module, because a clean `core` says nothing about the others:

| Module | Modules in graph | Third-party vulnerabilities |
| --- | --- | --- |
| `core` | 1 | 0 |
| `receivers/http` | 3 | 0 |
| `exporters/otlp` | 7 | 0 |
| `cmd/crierd` | 9 | 0 |
| `exporters/otlp/integration` | 99 | 0 |

`core`'s graph contains only itself: NFR1 holds, and CI enforces it rather than
trusting review.

The 99-module graph belongs to the test-only integration module, which is never
published and which no consumer can import. That quarantine is deliberate and
is why testcontainers does not appear in anyone's `go.sum` — but it is worth
naming, because a reader who greps the repository for its dependency count will
find that number and it is not the shipped one.

**In the graph but off the build path.** Every finding reported by
`govulncheck` at module scope in every module was in the Go standard library of
the scanning toolchain, not in a required module. On a toolchain one patch
behind, eight such findings appear and all are fixed in the next patch; CI
scans on a supported toolchain, where none appear. No third-party module in any
graph carries a known vulnerability, called or otherwise.

### Cross-project precedent

| ID | Origin | Applied here |
| --- | --- | --- |
| M-2 | `moat`, `realip` package: a service trusting an identity header without knowing it sits behind a proxy that always overwrites it lets any direct client forge that header. | ADR-0008 makes trusted-proxy mode strictly opt-in and requires a configuration that would trust every peer to fail loudly, mirroring `moat`'s `ErrDefaultRouteTrusted`. |

## Outstanding

Items known to be unresolved. Kept here so they are not rediscovered as
surprises during implementation.

- **Independent audit pass.** No pass has been independent, Pass 3 included.
  Repeating that outcome is acceptable; misrepresenting it is not, so it is
  repeated. See Pass 3 for what that costs specifically.
- ~~**Export worker topology undecided.**~~ Decided in ADR-0016 before the
  export layer was built.
- ~~**Draft interfaces predate ADR-0009 and ADR-0013.**~~ Both landed carrying
  `ObservedTimestamp`, and `FanOut` documents retry as composed inside it.
- **Metrics are not exposed** (A-6). Tracked in
  [#50](https://github.com/JonasBorgesLM/crier/issues/50).
- **The release order is documented, not enforced** (A-9). Nothing stops
  someone tagging a dependent module before `core`; the result is a published
  module a consumer cannot resolve. See [`RELEASING.md`](../RELEASING.md).
