# usher: audit events, and identity at the edge

**IR3 and IR7.** [`usher`](https://github.com/JonasBorgesLM/usher), the OAuth
2.1/OIDC authorization server and gateway, emits audit events — failed
logins, rate-limit hits, rejected tokens — and crier ingests them. It is also
the reverse proxy crier can run behind. The requirements and ADRs here that
predate it call it `gateway-auth`, its placeholder name.

This is the integration that makes ADR-0008 concrete: usher solves an
authentication problem crier genuinely has, rather than two services merely
sharing a compose file.

usher is not a dependency here. The emitting side ships in usher itself
(`internal/audit`, usher's RI-03 and ADR-0017), and the trusted-proxy side
is configuration of this repository's receiver.

## Two relationships, not one

### 1. As a source of audit events (IR3)

usher posts to `/v1/logs` like any other client, authenticating with its own
credential. The source name is whatever its `USHER_CRIER_SERVICE_NAME` is set
to:

```
Authorization: Bearer usher:<credential>
```

Audit events are the case that justifies several of crier's defaults:

- **They are the records you least want sampled.** A failed login at 03:00 is
  the rare event, and a uniform sampler discards rare events first. crier's
  sampler never applies at or above its floor, which defaults to `ERROR`
  (FR8) — `TestSamplingNeverDiscardsErrors` is what keeps that true.
- **They carry credential-shaped data by nature.** A rejected token event is
  about a token. Redaction covering `Body` as well as attributes exists for
  exactly this, and it fails closed: a record that cannot be masked is dropped
  and counted, never exported unmasked (ADR-0014).
- **Losing them silently is worse than losing them.** Every discard path
  increments exactly one counter, with the reason distinguishable — capacity
  pressure, per-source quota, backend unavailable (ADR-0005, ADR-0015), which
  `TestEveryDropReasonIsAccountedFor` enforces rather than documents.

### 2. As the reverse proxy in front of crier (IR7)

Deployed behind usher, crier does not authenticate the caller itself: the
gateway already did, and asserts the principal in a header. usher's identity
contract (its RI-04) injects `X-Auth-Subject`, `X-Auth-Client` and
`X-Auth-Scope` after stripping any the client sent. The calling client is
the source, so `X-Auth-Client` is the header to name:

```go
auth, err := httpreceiver.NewTrustedProxy(httpreceiver.TrustedProxyConfig{
    TrustedCIDRs:   []string{"10.4.0.0/16"},   // the gateway's addresses
    IdentityHeader: "X-Auth-Client",
})
```

usher's RI-04 states the same condition from its side: a consumer may rely
on these headers only when it restricts the peer allowed to set them to the
gateway's addresses. That is what `TrustedCIDRs` is here.

**This mode is never the default and cannot become one by omission.** A header
is identity only if the peer that set it is one crier was told to believe; from
anyone else it is an assertion by a stranger. A configuration whose trusted set
covers the default route is refused at startup with an error naming the opt-out
— the same treatment `moat` gives it in `realip`, where this exact failure was
found and fixed (finding M-2).

The trust decision is delegated to `moat`'s `realip` rather than re-derived
here, for that reason: the code that already got this wrong once is the code
that has the tests for it.

## Why identity is server-derived either way

Whichever mode is in use, the identity attached to a record comes from the
authenticated principal and never from the request body (ADR-0008, finding
D-2). A client asserting `service.name: usher` has that field
overwritten and the discrepancy counted — the record is still accepted, just
not on the caller's terms.

That matters most for an audit-event source. If a caller could label its own
records as coming from usher, anyone able to reach the ingestion
endpoint could forge audit history, which is a worse outcome than losing it.
`TestClientAssertedIdentityIsOverwrittenAndCounted` is the test.
