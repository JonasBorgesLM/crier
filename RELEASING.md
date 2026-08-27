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

The sequence below has been rehearsed end to end against a local clone,
finishing with an outside consumer running `go get` and executing the result.
See [Rehearsing it without publishing](#rehearsing-it-without-publishing) —
which is also how the `go get` failure in step 2 was found, after this document
had already been written the other way.

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

## Which branch

**Tags are cut on `main`, and every commit in this procedure is made there.**

`develop` is where work lands; `main` is what has been released, and it is what
GitHub shows a visitor. Those being the same thing is deliberate — see
[`CONTRIBUTING.md`](CONTRIBUTING.md#develop-and-main).

So a release starts by bringing `main` up to date:

```bash
git checkout main && git pull --ff-only
git merge --no-ff develop -m "Merge develop into main for the vX.Y.Z release"
git push origin main
```

and finishes by merging it back, which is step 4 below.

**Push the commit before pushing the tag.** A tag pushed on its own carries its
commit but moves no branch, leaving a release that points at a commit on no
branch — recoverable, but confusing to everyone who looks later.

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
go mod edit \
  -dropreplace=github.com/JonasBorgesLM/crier/core \
  -require=github.com/JonasBorgesLM/crier/core@v0.1.0
go mod tidy && go build ./... && go test ./...
```

**Both edits in `go mod edit`, and not `go get`.** `go get` has to resolve the
current module graph before it can apply an upgrade, and the current graph
still contains the version that does not exist — so it fails on the very thing
you are trying to remove:

```
go: github.com/JonasBorgesLM/crier/core@v0.0.0-00010101000000-000000000000:
    invalid version: unknown revision 000000000000
```

`go mod edit` is a text edit that resolves nothing, which is exactly what is
needed here. `go mod tidy` afterwards does the resolving, against a graph that
is now sound.

Commit that, and only then tag:

```bash
git tag -s exporters/otlp/v0.1.0 -m "exporters/otlp v0.1.0"
git push origin exporters/otlp/v0.1.0
```

**Confirm each module is resolvable before tagging the one that depends on
it.** The release workflow builds the tagged module, so it fetches the
dependency from the proxy — if `core` is not indexed yet, the dependent
module's own release fails:

```bash
GOPROXY=https://proxy.golang.org go list -m github.com/JonasBorgesLM/crier/core@v0.1.0
```

`gorelease` cannot run until this step is done — it fails to resolve the same
fake version a consumer would, which is how the problem was found in the first
place.

### 3. `cmd/crierd` third

It depends on all three. Drop every `replace`, require the published versions,
verify, commit, tag — the same `go mod edit` shape as step 2, with three
`-dropreplace` and three `-require` flags.

**`exporters/otlp/integration` keeps its `replace` directives.** It is
test-only, it is never published, and no tag pattern matches it. "Drop every
replace" is about the modules being released; touching this one only breaks
local development.

### 4. Merge back, and restore local resolution

```bash
git checkout develop
git merge --no-ff main -m "Merge main back after the vX.Y.Z release"
```

Without this, `develop` and `main` differ in every dependent `go.mod` — the
exact file the release changed — and the divergence grows with each release.

**The dropped replaces have to be replaced by something.** A `replace` is what
made a local change to `core` visible to `exporters/otlp` without publishing
`core` first; once it is gone, a `go.work` at the repository root takes over,
giving local resolution without appearing in any module's published `go.mod`:

```bash
go work init ./core ./exporters/otlp ./exporters/otlp/integration ./receivers/http ./cmd/crierd
```

This is part of the release, not a follow-up: between dropping the replaces and
adding `go.work`, no local change to `core` is visible anywhere else in the
tree.

**CI must then set `GOWORK=off`.** A committed `go.work` resolves every module
to the local directory, so CI would build against the working tree and never
check that the published versions actually resolve — a green build proving the
opposite of what it is there to prove. That is the failure class CLAUDE.md's
"a check must fail when it cannot run" is about, arriving through a
convenience. Either commit `go.work` and set `GOWORK=off` in the workflows, or
leave it untracked in `.gitignore` and let each developer create their own.

## Rehearsing it without publishing

The whole sequence can be run against a local clone, which is how the `go get`
problem in step 2 was found. Nothing leaves the machine and no tag reaches
GitHub.

```bash
SIM=$(mktemp -d)
git clone --bare . "$SIM/crier.git"
git -C "$SIM/crier.git" tag -a core/v0.1.0 develop -m "simulation"

cat > "$SIM/gitconfig" <<EOF
[url "$SIM/crier.git"]
	insteadOf = https://github.com/JonasBorgesLM/crier
EOF

export GIT_CONFIG_GLOBAL="$SIM/gitconfig"
export GOPRIVATE='github.com/JonasBorgesLM/*'
export GOFLAGS=-mod=mod

git worktree add --detach "$SIM/wt" develop
# now run the steps above inside $SIM/wt, tagging in $SIM/crier.git as you go
```

Go resolves the modules from that clone exactly as it would from GitHub, so a
step that fails here fails in production too.

**Finish with an outside consumer**, because that is the failure A-9 is about
and the only check that actually reproduces it:

```bash
mkdir "$SIM/consumer" && cd "$SIM/consumer"
go mod init example.com/consumer
go get github.com/JonasBorgesLM/crier/receivers/http@v0.1.0
# write a main.go that imports it, then:
go run .
```

**What the rehearsal covers:** that each `go.mod` is coherent after the edits,
that every module builds and tests at each stage, that the tags resolve, and
that an outside consumer can `go get` and run the result.

**What it does not:** the real proxy and its indexing delay, checksum database
verification, the release workflow itself, the binaries it cross-compiles, and
the GitHub release it creates. Those only happen on a real tag.

## Points of no return

Know which of these you cannot take back before starting, rather than while
recovering.

| Action | Reversible? | Cost if wrong |
| --- | --- | --- |
| **Pushing a tag** | **No.** | A tag is immutable once anyone — or the proxy — has fetched it. Deleting and re-pushing the same version leaves whoever already fetched it with different bytes under the same name, which is the thing versioning exists to prevent. Ship `v0.1.1`. |
| **The proxy indexing a module version** | **No.** | `proxy.golang.org` caches permanently and the checksum database is append-only. Even after a tag is deleted from GitHub, that version stays resolvable forever. A published mistake cannot be unpublished, only superseded. |
| **A module path in the first published version** | **No, in practice.** | The import path is now in other people's code. Renaming a module means a new path and a v2-style migration. |
| **A wire-format field name after the first `receivers/http` tag** | **No** (ADR-0021). | Requires a new path version served alongside the old one for a migration window. Check the trigger before tagging. |
| **The GitHub release object** | Yes. | Delete and recreate it; the tag underneath is what is permanent. |
| **Release notes and attached binaries** | Yes. | Re-upload. Nothing depends on their content. |
| **A `go.mod` edit that is wrong** | Yes, *before* the tag. | Free until tagged. Afterwards it is a new patch version. |
| **Enabling private vulnerability reporting** | Yes. | A setting toggle. |

The asymmetry is the whole point: everything before `git push origin <tag>` is
free to get wrong, and nothing after it is. Rehearse first.

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
