# ADR-0008: Ingestion authentication and source identity trust boundary

## Status
Accepted

## Context
The original design treated the ingestion endpoint as an implicitly trusted
internal pipe. It is not. Two distinct problems were identified during review:

1. **No authentication.** Anyone able to reach the port can inject records.
   This enables log forgery (planting misleading audit trails, or flooding to
   bury real events), and cost abuse against the downstream backend, which is
   typically billed by ingested volume.
2. **Client-asserted identity.** `Resource.ServiceName` and the rest of the
   resource block arrive in the request body. Nothing prevents a caller from
   claiming to be `gateway-auth` and fabricating authentication audit events —
   the highest-value records in the whole pipeline.

These are independent: authenticating the caller does not by itself stop that
caller from lying about *which* service it is.

## Decision

**Authentication.** In standalone mode, the ingestion endpoint requires
authentication. The MVP ships a shared-credential scheme where each configured
source has an identifier and a credential; the credential is stored and
compared using `moat`'s `secret.Value` (constant-time comparison, masked in
memory and in any formatted output). mTLS is documented as the recommended
production alternative and left as a phase-2 implementation. Embedded-library
mode has no receiver and therefore no authentication concern — the host
application already owns that boundary.

**Source identity is server-attested.** The resource identity used for
attribution, per-source quotas (ADR-0011), and metrics is derived from the
authenticated principal, not from the request body. Client-supplied resource
fields that conflict with the attested identity are overwritten, not merged,
and the discrepancy is counted as a metric. Client-supplied resource
attributes outside the attested identity fields (e.g. `host.name`) are kept,
since they are descriptive rather than authoritative.

**Deployment behind `gateway-auth` (IR7).** `crierd` may run behind
`gateway-auth` acting as a reverse proxy, with the gateway performing
OAuth2/OIDC and asserting the authenticated identity to the upstream. In this
mode `crier` derives source identity from the gateway's assertion.

This mode is **opt-in and requires explicit trusted-proxy configuration.**
The failure mode is exactly the one already found and fixed in `moat`'s
`realip` package (finding M-2): if a service trusts an identity header
without knowing it sits behind a proxy that always overwrites that header,
any direct client can forge it. Therefore:

- Trusting gateway-asserted identity is never the default.
- The operator must configure which peer(s) are trusted to make the
  assertion; a configuration that would trust every peer must fail loudly
  rather than silently accept, mirroring `moat`'s `ErrDefaultRouteTrusted`
  and `InsecureTrustEveryPeer()` treatment.
- When the trusted-proxy check fails, the request is rejected — the header
  is never treated as "probably fine".

## Consequences
- The MVP gains a real authentication path, and with it a genuine reason for
  `crier` to depend on `moat` beyond convenience middleware.
- The `gateway-auth` integration becomes architecturally motivated rather
  than decorative: the gateway solves an authentication problem `crier`
  actually has, which is a far stronger portfolio narrative than two
  unrelated services sharing a docker-compose file.
- Overwriting client-asserted identity means a misconfigured client will see
  its records attributed differently than it expects; the discrepancy metric
  exists specifically so this is diagnosable rather than mysterious.
- Adds configuration surface (credentials, trusted proxies) that must be
  validated eagerly at startup per NFR4.
