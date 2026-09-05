# ADR-0023: Dashboards ship as a template, not as a provisioner

## Status
Accepted

## Context
Issue #72 proposes an opt-in package or subcommand — `crier/provisioning/signoz`,
or a `criercli` — that creates a SigNoz dashboard for a consumer from the
`ServiceName` crier is already configured with, idempotently, at startup.

The investigation behind it was real work and produced findings worth keeping.
The endpoint is `POST /api/v2/dashboards`; v1 is deprecated and answers
`dashboard_deprecated`. It requires `Authorization: Bearer <token>` and
`schemaVersion: "v6"` in the body. And the finding that decides this ADR: **a
panel could not be built from the public documentation.** Validation
contradicted itself — `unknown field "name" in query spec` against `"name is
required"`, for the same field. What worked was building the dashboard in the
real UI and reading the JSON back out of `GET /api/v2/dashboards/{id}`.

The question is not whether this would be useful to a consumer. It would. The
question is what crier takes on by shipping it.

## Decision
Crier ships the **artifact and the procedure**: a schema-valid dashboard
template carrying the placeholders a consumer substitutes, and the request that
posts it. It does not ship a provisioner — no module, no fifth module kind
alongside ADR-0003's and ADR-0020's, no SigNoz credential in crier's
configuration, and no call to a backend's control plane from `crierd`'s startup
path.

**The contract is not a contract.** Every external interface crier speaks is
either a specification — OTLP over HTTP (ADR-0017), the OpenTelemetry Logs data
model (ADR-0004) — or a module with a version, pinned down to the variant
(ADR-0018). A dashboard schema whose valid shape is discoverable only by
capturing it from a running instance is neither, and the issue proves it: the
public documentation was tried first and produced oscillating validation
errors. Code written against that is correct on the day it was captured and
silently wrong afterwards, with no version to pin and no diff to read.

**The credential is a different kind of credential.** Ingestion credentials and
exporter credentials are on the record's path, scoped to a system crier already
talks to. A dashboard token is a control-plane credential for the observability
stack. NFR4 and IR2 would hold it masked, and masking is not the problem —
scope is. It widens what a leaked crier configuration costs, permanently, to
buy a convenience exercised once per deployment.

**It fails in the wrong direction.** A stale template is a five-minute
re-capture by whoever notices. A shipped provisioner that breaks is a broken
release in a module somebody pinned, and it breaks at startup, in a deployment,
for a feature no record's delivery depends on. `crierd` refuses to start on a
bad configuration deliberately (NFR4); adding a way for it to fail against a
third system, over a dashboard, spends that strictness on the wrong thing.

**Vendor neutrality is a stated goal, and its exceptions are principled.**
`REQUIREMENTS.md`'s goals are explicit about making logging exportable "without
committing to a specific observability vendor". The OTLP exporter is a specific
choice and not a violation, because OTLP is a protocol several backends speak
and ADR-0017 chose a transport rather than a vendor. `provisioning/signoz`
would be the first component here whose reason to exist is one vendor's private
API.

There is precedent for this exact move, one milestone back. IR6 originally
named `security-scanner` as what would exercise the threat model; the M6 audit
ran it down, found its confirmers were classes a JSON ingestion endpoint does
not have, and rewrote the requirement to name the artifact that actually does
the job — [`../security/probe-threats.sh`](../security/probe-threats.sh) —
rather than the tool that looked like it would. Name the artifact; do not take
the dependency.

### What the template has to carry
- The placeholders it is substituted with (`{{.ServiceName}}`, `{{.PanelID}}`)
  and the request that posts it, including the `schemaVersion` and the header
  the API rejects it without.
- **The SigNoz version it was captured from, and the date.** A template
  captured from an undocumented API and dated is honest; the same file undated
  is a claim about the present that ages into a lie.
- **Whether anything exercises it.** If nothing does, it says so. A template no
  CI touches is documentation, and presenting it as verified would be the same
  fail-open shape the M6 sweep found six times (V-1 through V-6): a check whose
  failure is indistinguishable from its success.

### Verified on 2026-09-05, against SigNoz v0.139.0

Written before the template existed; this section records what happened when it
was built, because two claims in the Context above turned out to be wrong, and
both were wrong in the direction this ADR predicts.

**The header is `SIGNOZ-API-KEY`, not `Authorization: Bearer`.** The bearer
form — recorded in #72 and repeated in the Context above — returns `401
unauthenticated` for an API key. It is the session-JWT form.

**The captured template was already stale.** `tags` as an array of strings,
exactly as captured from a working instance, is rejected on v0.139.0: `value of
type 'string' was received for field 'tags', try sending 'tagtypes.PostableTag'
instead?`. Nothing had changed on this side; the version underneath moved. That
is the whole argument of this ADR arriving as a fact rather than a prediction,
and it happened between one capture and the next on the same machine.

The rest of the schema was learned the way this ADR prescribes — from the
instance, not the documentation — helped by an API that names the Go type it
wanted. Sending a deliberately invalid `plugin.kind` made the server enumerate
every valid panel kind. The shapes, and the two-part verification (`201` on
POST, then all ten panels executed against `/api/v5/query_range` returning real
rows) are written down in
[`../observability/signoz/README.md`](../observability/signoz/README.md).

Host and server panels were dropped rather than shipped empty: the instance
carries zero metric names, because crier ships logs and nothing else was
feeding it metrics.

### What would reverse this
Two things, either sufficient, neither of which has happened:

- **SigNoz publishes a documented, versioned dashboard API.** The contract
  objection is the load-bearing one; without it, vendor neutrality alone would
  not be enough to refuse.
- **A second backend needs the same thing.** An abstraction with two
  implementations has something to be right about. One implementation guessed
  at from a private API is a shape, not an abstraction.

## Consequences
- **The finding is preserved, and stops depending on where it currently
  lives.** As of this ADR the template exists in a scratch directory outside
  the repository, and the issue comment meant to carry it holds an unexpanded
  `$(cat ...)` where the JSON should be — so the artifact this whole proposal
  rests on is currently one cleanup away from being gone. Committing it is the
  work #72 keeps, and the reason #72 stays open rather than closing with this
  decision.
- **A consumer does one manual step per deployment.** That is the cost, and it
  is paid once by the operator who already holds the token, rather than
  forever by every crier deployment that has to hold one.
- **The template goes stale silently.** Nothing here notices a schema change;
  the next person to post it finds out. The cheap repair is a SigNoz variant of
  the `demo/` stack, where posting the template would be exercised the way the
  demo already exercises redaction and identity attestation — which would also
  turn "whether anything exercises it" from a disclaimer into a green check.
- **This does not decide dashboards for crier's own metrics.** `/metrics`
  exists now (NFR5, #50), and whether crier ships a dashboard describing itself
  is a separate question with a different answer available to it, because that
  one describes an interface crier controls.
