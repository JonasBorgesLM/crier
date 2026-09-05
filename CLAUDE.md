# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What crier is

A Go log-control service and library: it ingests application logs, normalizes
them to the OpenTelemetry Logs data model, and exports them to observability
backends through a pluggable `Exporter`. The same engine runs embedded in a
host application or as the standalone `crierd` daemon.

Requirements live in [`REQUIREMENTS.md`](REQUIREMENTS.md) and are referenced by
ID (FR1, NFR4, IR7…). Decisions live in [`docs/adr/`](docs/adr/README.md).

## Repository layout

This is a **multi-module** repository (ADR-0003). There is no root `go.mod`.

| Path | Module | Notes |
| --- | --- | --- |
| `core/` | engine | **Zero third-party runtime deps (NFR1)** — CI enforces this |
| `exporters/otlp/` | OTLP exporter | one module per exporter |
| `receivers/http/` | async HTTP receiver | its own module (ADR-0020, amends ADR-0003) |
| `cmd/crierd/` | standalone daemon | thin shell over `core` |

A green build at one module says nothing about the others. Always run commands
per module:

```bash
for m in core exporters/otlp receivers/http cmd/crierd; do (cd "$m" && go build ./... && go vet ./... && go test -race ./...); done
```

Lint (config in `.golangci.yml`, golangci-lint v2):

```bash
(cd core && golangci-lint run ./...)
```

## Definition of Done

A change is not finished when it compiles. It is finished when all of these
hold, for every module it touches:

1. `go build ./...`, `go vet ./...`, and `go test -race ./...` all succeed in
   each affected module (the per-module loop above) — a green build in one
   module says nothing about another.
2. `golangci-lint run ./...` passes in `core`.
3. Any new guard, invariant, or counter has been seen to fail before it was
   seen to pass. An assertion nobody has watched go red is not verified, it is
   hoped — see "A check must fail when it cannot run" below.
4. The non-negotiable invariants below still hold. A change that trades one of
   them for convenience is not done; it is a new entry for
   `docs/audit-log.md`.

Fix the underlying issue rather than working around the check. Skipping a
subtest, silencing a vet warning, or reaching for `--no-verify` do not make a
task done — they make the next audit longer.

## Non-negotiable invariants

These are decided, not open. Each is a defect if violated, and most were found
the expensive way — see [`docs/audit-log.md`](docs/audit-log.md).

1. **Retry goes *inside* fan-out, never outside** (ADR-0013, finding A-1).
   `FanOut(Retry(CB(a)), Retry(CB(b)))`. The other order re-sends the whole
   batch when any one exporter fails, so a healthy destination is hammered
   because an unrelated one is broken.
2. **Redaction is fail-closed and covers `Body`** (ADR-0014, findings A-2/A-3).
   A record whose redaction fails is dropped and counted — never exported
   unmasked. Not configurable into fail-open.
3. **Source identity is server-derived, never client-asserted** (ADR-0008,
   finding D-2). Identity comes from the authenticated principal; client-supplied
   identity fields are overwritten and the discrepancy counted.
4. **`ObservedTimestamp` is authoritative** (ADR-0009). `Timestamp` is
   source-asserted and untrusted; use `LogRecord.EffectiveTime()`.
5. **Pipeline stage order is a contract** (ADR-0010):
   transport limits → parse/validate → attest identity → normalize →
   record limits → redact → filter/sample → admit. Only the last step touches
   the buffer, so filtered records never consume buffer memory.
6. **No silent drops.** Every discard path increments exactly one counter, with
   the reason distinguishable — capacity pressure vs. backend unavailable vs.
   per-source quota (ADR-0005, ADR-0011, ADR-0015). Loss at shutdown is
   permitted; *unaccounted* loss is not (ADR-0015, finding A-5).
7. **Trusted-proxy mode is opt-in and fails loudly** (ADR-0008, precedent M-2
   from `moat`). A config that would trust every peer must refuse to start.
8. **`crierd` never ingests its own operational logs** (NFR11) — that is a
   feedback loop, not observability.

## Conventions

- **English** for all code, comments, and documentation (NFR8) — regardless of
  the language the request was written in.
- **Conventional Commits** (NFR9). Scope is the module or area:
  `feat(core): add per-source admission`, `fix(receiver): …`, `docs(adr): …`,
  `ci: …`, `build(deps): …`.
- **One subject per commit, staged explicitly.** Do not use `git add -A` when
  the working tree holds work on more than one subject — stage the files for
  each subject and commit them separately.

  This has gone wrong twice here, the same way both times: `git add -A` swept
  an ADR, a test and a workflow change into one commit whose message described
  only the ADR. Both were caught and split afterwards, which is the expensive
  version of getting it right. A commit whose message does not describe
  everything in it is one nobody can review or revert cleanly, and the message
  is written from what the author was thinking about, not from what was staged.

  When several subjects have already been committed together, `git reset
  --soft HEAD~1` followed by explicit staging is the fix, not an amended
  message that lists them.
- **Every structural decision gets an ADR** (NFR7). Do not silently resolve a
  question listed as open in `docs/adr/README.md` — write the ADR first.
- **Go 1.24** is the floor (NFR2). Do not raise it for convenience.
- **Tests are table-driven**, use `t.Run` subtests, and assert the specific
  behaviour and counter — not just "no error".
- **Public packages carry testable examples** (`ExampleXxx`), matching `moat`.
- **A check must fail when it cannot run.** Six checks here have passed without
  verifying anything, so this is a rule rather than an aspiration:
  - A negative assertion — `!= 202`, "no output", "no match" — is satisfied by
    a tool that never ran. Assert the expected answer instead.
  - Resolve a tool's output into a variable first, so its exit status is
    checked separately from its content. Piped straight into `grep`, a crashed
    command and a clean result are the same thing.
  - A suite that skips itself exits 0. Where the dependency is guaranteed — CI
    — its absence must fail, not skip.
  - Verify a new guard by breaking what it guards. If you have not seen it go
    red, you do not know it is a check.
- Secrets and credentials are held as masked values, never plain strings
  (NFR4, IR2).

## Scope limits

- One issue, fix, or refactor per change. Finding a second thing to fix while
  you're already in the file is not a reason to fix it now — file it on the
  board (see "Working on issues" below) and keep going on the one you were
  given.
- Don't fold a cosmetic change (formatting, renaming, reordering) into a
  functional one. This is the same discipline "One subject per commit" already
  requires at staging time, applied earlier — before the second subject gets
  written, not just before it gets `git add`ed.

## Working on issues

The board is https://github.com/users/JonasBorgesLM/projects/3 — 40 issues
across milestones M0–M6, each citing the ADR that decided it. Prefer taking an
existing issue over inventing work; the ordering across milestones is
deliberate (the engine before the receiver, the receiver before the daemon).

Work lands on `develop`; `main` is what has been released and is merged from
`develop` at each release, never committed to directly. The rules, and why the
merge back after a release is not optional, are in
[`CONTRIBUTING.md`](CONTRIBUTING.md#develop-and-main).

Milestone branches stack — an open milestone's PR is often based on the
previous milestone's branch, not on `develop`. Merging them has an order:
retarget the dependent PR to `develop` **before** deleting the base branch, or
the dependent PR is closed and cannot be reopened or retargeted until the
branch is restored. The sequence and the recovery are in
[`CONTRIBUTING.md`](CONTRIBUTING.md#stacked-pull-requests).

The board was populated once from a throwaway bootstrap script that is not
kept in the repository. Create further issues by hand, matching the existing
convention: a milestone, a `track:` label, whatever `kind:` labels apply, and a
body that cites the ADR the work comes from.

## Ecosystem

crier is one of several related projects and reuses them deliberately rather
than reimplementing: `moat` (middleware, `secret.Value` masking),
`gateway-auth` (audit-event source, reverse-proxy identity), `task-api`
(embedded-library consumer), `security-scanner` (receiver hardening).
