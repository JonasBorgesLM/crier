# Releasing

Modules are versioned and tagged independently (ADR-0003, NFR3). The tag prefix
selects which module is released, and the workflow does the rest.

**The order is not optional.** Read the next section before tagging anything.

## Why order matters

`exporters/otlp`, `receivers/http` and `cmd/crierd` each require
`github.com/JonasBorgesLM/crier/core` at a version that does not exist, masked
by a local `replace` directive:

```
require github.com/JonasBorgesLM/crier/core v0.0.0
replace github.com/JonasBorgesLM/crier/core => ../../core
```

That works in this repository and nowhere else. **A consumer ignores a
dependency's `replace` directives**, so anyone running
`go get github.com/JonasBorgesLM/crier/receivers/http` resolves `core v0.0.0`,
which is not a thing, and the build fails. Tagging a dependent module before
`core` is published produces a release nobody can use.

This was finding A-9 of the M6 audit. It is documented rather than enforced —
nothing stops someone tagging in the wrong order — which is itself tracked as
an outstanding item in [`docs/audit-log.md`](docs/audit-log.md).

## Before the first tag of a module

Some decisions become expensive the moment a version exists.

**Wire format.** [ADR-0021](docs/adr/0021-wire-contract-edges.md) freezes the
request *and response* field names of `receivers/http` at its first tag. Run
its trigger:

```bash
git tag --list 'receivers/http/v*'
```

Nothing printed means the names are still free — change them now if they need
changing. Anything printed means a rename requires a new path version served
alongside the old one.

**Private vulnerability reporting.** Confirm it is on before publishing
anything, because the window where someone finds a hole and has nowhere private
to report it starts at the first release:

```bash
gh api repos/JonasBorgesLM/crier/private-vulnerability-reporting
# {"enabled":true}
```

It was found disabled during the `moat` release and again here, which is twice,
so it is a checklist item rather than an assumption.

## The order

### 1. `core` first

It depends on no other module in this repository, so it can go as it stands.

```bash
git tag -s core/v0.1.0 -m "core v0.1.0"
git push origin core/v0.1.0
```

### 2. Point the dependents at the published `core`

For each of `exporters/otlp`, `receivers/http`:

```bash
cd exporters/otlp
go mod edit -dropreplace=github.com/JonasBorgesLM/crier/core
go get github.com/JonasBorgesLM/crier/core@v0.1.0
go mod tidy && go build ./... && go test ./...
```

Commit that, and only then tag:

```bash
git tag -s exporters/otlp/v0.1.0 -m "exporters/otlp v0.1.0"
git push origin exporters/otlp/v0.1.0
```

`gorelease` cannot run until this step is done — it fails to resolve the same
fake version a consumer would, which is how the problem was found in the first
place.

### 3. `cmd/crierd` last

It depends on all three. Drop every `replace`, require the published versions,
verify, commit, tag.

**Keeping the replaces for local development** is a separate question. Removing
them means every local change to `core` needs a tagged `core` before the
dependents see it. A `go.work` file at the repository root gives local
resolution without shipping it in any module's `go.mod`, and is the usual answer
— it is not set up here yet.

## What the workflow does

Pushing a matching tag runs `.github/workflows/release.yml`, which:

- resolves the module and version from the tag, and the toolchain from NFR2's
  second policy — a command module builds on the supported toolchain, a library
  at its declared floor;
- runs build, vet, tests and `govulncheck` **before** the release exists,
  because a tag is immutable once anyone has fetched it;
- generates release notes from the real API diff with `gorelease` (NFR9), not
  from memory;
- cross-compiles binaries for `cmd/` modules only;
- checks the tag signature.

## On signed tags

Tags are signed. Verify locally before pushing:

```bash
git verify-tag core/v0.1.0
```

**CI's check is a warning, not a gate, and that is deliberate.** The signing key
here is an SSH key, and `git verify-tag` in CI has no allowed-signers file to
check it against — a hard failure would block every release on a key CI cannot
see. Enforcing it properly means publishing an allowed-signers file and
configuring `gpg.ssh.allowedSignersFile` in the workflow. Until that exists, the
signature is enforced by the person tagging and merely reported by CI, which is
worth knowing rather than assuming.

## After the release

- Confirm the workflow published what you expected, including the binaries.
- Confirm the release notes contain a real API diff rather than
  `(no API diff available)` — that string means `gorelease` failed and nobody
  noticed.
