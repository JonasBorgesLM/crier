# Contributing to crier

Thanks for taking a look. This document is short on ceremony and specific about
the few things that are genuinely non-negotiable.

## Before you start

Work is tracked on the [project board](https://github.com/users/JonasBorgesLM/projects/3),
across [milestones M0–M6](https://github.com/JonasBorgesLM/crier/milestones).
Prefer claiming an existing issue: each one cites the ADR that decided it, so
it is actionable without reading the whole decision record set. The ordering
across milestones is deliberate — the engine before the receiver, the receiver
before the daemon.

For anything structural, open an issue first. A pull request that resolves a
question listed as open in [`docs/adr/README.md`](docs/adr/README.md) needs an
ADR, not just an implementation.

## Development

This is a multi-module repository (ADR-0003) with **no root `go.mod`**. A green
build in one module says nothing about the others, so run everything per module:

```bash
for m in core exporters/otlp cmd/crierd; do
  (cd "$m" && go build ./... && go vet ./... && go test -race ./...)
done
```

Lint with [golangci-lint](https://golangci-lint.run) v2 (config in `.golangci.yml`):

```bash
(cd core && golangci-lint run ./...)
```

Requirements:

- **Go 1.24** minimum (NFR2). Don't raise it for convenience.
- **`core` must have zero third-party runtime dependencies** (NFR1). CI fails
  the build if one appears. Exporters may depend on whatever their SDK needs —
  that is why they are separate modules.

## Decisions you cannot quietly change

These are recorded, and most were found the expensive way — see
[`docs/audit-log.md`](docs/audit-log.md). Changing one requires a new ADR that
supersedes the old, not an edit:

1. Retry and circuit breaking compose **inside** fan-out, per exporter
   (ADR-0013). The other order causes duplicate amplification.
2. Redaction is **fail-closed** and covers `Body` (ADR-0014).
3. Source identity is **server-derived**, never taken from the request body
   (ADR-0008).
4. `ObservedTimestamp` is authoritative; `Timestamp` is untrusted (ADR-0009).
5. The pipeline stage order in ADR-0010 is a contract.
6. Every drop increments exactly one counter with a distinguishable reason.
   Silent loss is a defect (ADR-0015).

## Pull requests

- Branch from `develop`: `feature/<short-slug>`, `fix/<short-slug>`,
  `docs/<short-slug>`. Urgent fixes branch `hotfix/vX.Y.Z` off `main`.

### `develop` and `main`

**`develop` is where work lands. `main` is what has been released.**

- Every pull request targets `develop`. Nothing targets `main` directly.
- At each release, `develop` merges into `main`, and the tags are cut there.
  `main` is therefore always a released state, and it is also what GitHub shows
  a visitor — those being the same thing is the point.
- The release's own commits — the `go.mod` edits that swap a local `replace`
  for a published version, see [`RELEASING.md`](RELEASING.md) — are made on
  `main` during the release, and `main` is merged back into `develop` when it
  finishes.

That last step is not bookkeeping. Without it the two branches diverge in every
dependent `go.mod`, which is the file the release exists to change — and `main`
spent M0 through M6 holding only an initial commit precisely because nobody had
written down when it should have been updated.

**Once the replaces are dropped, local development needs a `go.work`.** A
`replace` is what makes a change to `core` visible to `exporters/otlp` without
publishing `core` first; when the release removes it, `go.work` is what takes
over. Adding it is part of the release, not a follow-up.
- Tagging a release has an order that is not optional — a dependent module
  tagged before `core` publishes something no consumer can resolve. See
  [`RELEASING.md`](RELEASING.md) before your first tag.
- **[Conventional Commits](https://www.conventionalcommits.org)** (NFR9),
  scoped to the module or area:

  ```
  feat(core): add per-source fair-share admission
  fix(receiver): reject forged identity header when proxy is untrusted
  docs(adr): record export worker topology decision
  ci: run govulncheck per module
  ```

  Breaking changes use `!` and a `BREAKING CHANGE:` footer.
- Explain **why** in the body, not just what — the diff already says what.
  Reference the requirement or ADR (`Refs ADR-0013`, `Closes #12`).
- Keep commits coherent. A commit that mixes a rename with a behaviour change
  is one nobody can review or revert cleanly.
- All code, comments, and documentation in **English** (NFR8), whatever
  language the discussion happened in.

### Stacked pull requests

Milestones stack: while `feature/m1-core-engine` was still open, the M2 branch
was based on it rather than on `develop`, so its PR showed the M2 diff alone.
That is worth keeping — but the merge has an order, and getting it wrong costs
a recovery.

**Retarget the dependent PR to `develop` before deleting the base branch.**

```bash
gh pr merge 42 --merge                 # merge the lower PR, keep the branch
gh pr edit 43 --base develop           # retarget the dependent PR first
git push origin --delete feature/m1-core-engine
```

Do not merge with `--delete-branch` while another PR is based on that branch,
and do not count on the dependent PR being retargeted for you. When this was
tried, deleting the base branch **closed** the dependent PR, and the state was
then stuck in both directions:

```
$ gh pr edit 43 --base develop
Cannot change the base branch of a closed pull request.
$ gh pr reopen 43
Could not open the pull request.        # its base branch no longer exists
```

**Recovery, if someone lands there.** Restore the branch at the commit the PR
was based on, then unwind in the order the API allows — reopen while the base
exists, retarget, and only then delete:

```bash
git push origin <sha>:refs/heads/feature/m1-core-engine   # restore the base
gh pr reopen 43
gh pr edit 43 --base develop                              # now it is allowed
git push origin --delete feature/m1-core-engine           # safe: base is develop
```

The PR survives this with its body, review history and checks intact, so there
is no reason to open a replacement.

## Tests

- Table-driven, with `t.Run` subtests.
- Assert the **specific** behaviour and counter, not merely that no error came
  back. "No silent drops" is only real if a test proves the counter moved.
- Public packages carry testable examples (`ExampleXxx`).
- Integration tests use real dependencies via testcontainers (a real OTel
  Collector, not a fake), matching the approach validated in `moat`.

## Security

Please do not open a public issue for a vulnerability. Use GitHub's private
vulnerability reporting on this repository.

crier's ingestion endpoint is treated as an attack surface, and the threat
model is documented in the [README](README.md#threat-model). A change that
weakens one of the mitigations there needs to say so explicitly in the PR
description.
