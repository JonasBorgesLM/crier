# ADR-0007: Integration with the existing project ecosystem

## Status
Accepted

## Context
`crier` is built alongside `moat` (security middleware library),
`gateway-auth` (OAuth2/OIDC or JWT gateway), `task-api`, and `security-scanner`.
Treating it as an isolated repository would forgo a real opportunity: proving
these components compose into an actual toolkit, not just a set of unrelated
portfolio pieces.

## Decision
- The standalone receiver's middleware chain (rate limiting, security
  headers, content-type validation) is composed using `moat` directly, as a
  real dependency — not reimplemented.
- Redaction reuses `moat`'s masking philosophy (ADR-0006).
- The repository ships a documented example of `gateway-auth` emitting audit
  events consumed by `crier`, and a documented example of `task-api`
  using `crier` in embedded-library mode.
- The repository ships an end-to-end `docker-compose.yml` demo (example
  service → `crierd` → OTel Collector → a viewable backend), runnable
  with one command.
- Using `security-scanner` against `crierd`'s own HTTP receiver is
  documented as a recommended validation step, without being a hard
  dependency of the MVP (IR6).

## Consequences
- `crier`'s standalone binary takes on a direct dependency on `moat`,
  meaning `moat` release quality directly affects `crier`'s stability —
  an acceptable and intended coupling given both are maintained together.
- The demo and documented integrations increase the effort of the MVP
  slightly but substantially raise the project's value as a portfolio
  artifact: a reviewer can see the components working together, not just
  read about them.
- Future exporters or receivers should keep this integration pattern in
  mind (e.g. a future file-tail receiver could reuse `moat`'s `validate`
  package for input sanitization).
