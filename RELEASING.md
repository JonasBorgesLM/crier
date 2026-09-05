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

**This one bit for real, not hypothetically.** `core.Destination` gained a
`Filter` field (#45) while `go.work` was untracked and only in `.gitignore`,
per the choice above. Every local `go build`/`go test` in `cmd/crierd` across
several commits stayed green, because `go.work` resolved `core` to the
sibling directory regardless of what `cmd/crierd/go.mod` required — masking
that its `require` line still pointed at `core v0.1.0`, which has no `Filter`
field. CI has no `go.work` (never committed) and resolves the real published
module, so pushing to `develop` failed `lint (cmd/crierd)`, `test
(cmd/crierd)`, and `govulncheck (cmd/crierd)` all at once, on a change that
had looked fine locally through several commits. Fixed by cutting
`core/v0.2.0` and pointing `cmd/crierd` at it (`go mod edit
-require=...@v0.2.0 && go mod tidy`), verified with `GOWORK=off` specifically
so the check means what it claims. The lesson: a green local build across a
`core` API change proves nothing about a dependent module unless it was
checked with `GOWORK=off`, or by pushing and watching CI.

## Rehearsing it without publishing

The whole sequence can be run against a local clone, which is how the `go get`
problem in step 2 was found. Nothing leaves the machine and no tag reaches
GitHub.

```bash
SIM=$(mktemp -d)
git clone --bare . "$SIM/crier.git"
git -C "$SIM/crier.git" tag -a core/v0.1.0 main -m "simulation"

cat > "$SIM/gitconfig" <<EOF
[url "$SIM/crier.git"]
	insteadOf = https://github.com/JonasBorgesLM/crier
EOF

export GIT_CONFIG_GLOBAL="$SIM/gitconfig"
export GOPRIVATE='github.com/JonasBorgesLM/*'
export GOFLAGS=-mod=mod
export GOMODCACHE="$SIM/gomodcache"

git worktree add --detach "$SIM/wt" main
# now run the steps above inside $SIM/wt, tagging in $SIM/crier.git as you go
```

Go resolves the modules from that clone exactly as it would from GitHub, so a
step that fails here fails in production too.

**`GOMODCACHE` is what makes that sentence true.** It is not hygiene: without
it the sentence is false. The shared module cache outlives the rehearsal, so a
later run resolves versions an earlier one published and finds them whether or
not this run tagged anything. A run of this procedure reported `cmd/crierd OK`
with `exporters/otlp/v0.1.0` and `receivers/http/v0.1.0` never created — the
tagging order A-9 is about, which is the one thing the rehearsal exists to
prove, was exactly what the warm cache hid. Pointed at `$SIM`, the cache is
born with the rehearsal and dies with it, and step 3 fails as it should:

```
reading .../exporters/otlp/go.mod at revision exporters/otlp/v0.1.0:
    unknown revision exporters/otlp/v0.1.0
```

**And it protects the machine from the rehearsal, which is the worse
direction.** `insteadOf` makes Go record the *real* URL for what it fetched, and
`GOPRIVATE` switches the checksum database off, so without `GOMODCACHE` the
rehearsal writes simulation bits into the shared cache under the real module
path and version — unverified. They sit there until a genuine `go mod tidy`
finds them and dies:

```
github.com/JonasBorgesLM/crier/exporters/otlp@v0.1.0: verifying module:
    checksum mismatch
SECURITY ERROR
```

That happened during this release, from a rehearsal run before the isolation
existed. The published bits were fine; the local cache was not. If it happens,
the fix is to evict those entries — `chmod -R u+w` the module's directories
under `$(go env GOMODCACHE)` and delete them — not to trust the message.

Two more details: the outside-consumer step below must run in the same shell so
it inherits the cold cache, and `rm -rf "$SIM"` will not remove the cache
afterwards, because module cache entries are read-only. Run
`GOMODCACHE="$SIM/gomodcache" go clean -modcache` first.

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
  from memory — against an explicit base version (this module's own previous
  tag, or `none` for a first release), and fails the release rather than
  publishing if `gorelease` cannot produce one (#53);
- cross-compiles binaries for `cmd/` modules only;
- checks the tag signature against `.github/allowed_signers`, gating the
  release on it (#54).

## On signed tags

Tags are signed. Verify locally before pushing:

```bash
git verify-tag core/v0.1.0
```

**CI enforces this, not just reports it (#54).** `.github/allowed_signers`
publishes the release SSH public key, and `release.yml` points
`gpg.ssh.allowedSignersFile` at it before calling `git verify-tag` — an
unsigned tag, or one signed by a key not in that file, fails the release
before it publishes. A key rotation or a second person tagging releases means
adding a line to `.github/allowed_signers` first, in its own commit, before
the first tag signed with the new key.

## After the release

- Confirm the workflow published what you expected, including the binaries.
- A release existing at all now means `gorelease` exited zero against an
  explicit base — a failure fails the workflow before publishing, rather than
  a manual check catching it afterward (#53). A first release's notes
  legitimately have no `## compatible changes` section, only a
  suggested-version summary; that is `-base=none` working as documented, not
  `gorelease` having failed silently.
