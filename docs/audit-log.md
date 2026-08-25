# Audit Log

A record of the review passes performed against `crier`'s design and, later,
its implementation. Each pass lists what was found, what closed it, and — the
part most often omitted — **who performed the review**.

> **Provenance.** Both passes recorded below were performed by the design
> author, not by an independent reviewer. That is a real limitation of this
> record and is stated here rather than left for a reader to infer. An
> independent pass is tracked as an open item under
> [M6 — Audit & release](https://github.com/JonasBorgesLM/crier/milestones).

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

### Cross-project precedent

| ID | Origin | Applied here |
| --- | --- | --- |
| M-2 | `moat`, `realip` package: a service trusting an identity header without knowing it sits behind a proxy that always overwrites it lets any direct client forge that header. | ADR-0008 makes trusted-proxy mode strictly opt-in and requires a configuration that would trust every peer to fail loudly, mirroring `moat`'s `ErrDefaultRouteTrusted`. |

## Outstanding

Items known to be unresolved. Kept here so they are not rediscovered as
surprises during implementation.

- **Independent audit pass.** Neither pass above was independent. Tracked in
  M6. Repeating that outcome is acceptable; misrepresenting it is not.
- **Export worker topology undecided.** ADR-0013 states the requirement (a slow
  exporter must not serialize its siblings) but not the mechanism. Must be
  recorded as an ADR before the export layer is built. Tracked in M2.
- **Draft interfaces predate ADR-0009 and ADR-0013.** The drafted `core/record.go`
  and `core/exporter.go` referenced by ADR-0004's amendment are not yet in this
  repository. When they land they must already carry `ObservedTimestamp`
  (ADR-0009), and `FanOut` must not document retry as wrapping it (ADR-0013,
  finding A-1). Tracked in M0.
