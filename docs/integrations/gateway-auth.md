# gateway-auth: audit events, and identity at the edge

**IR3 and IR7.** `gateway-auth` emits audit events — failed logins, rate-limit
hits, rejected tokens — and crier ingests them. It is also the reverse proxy
crier can run behind.

This is the integration that makes ADR-0008 concrete: `gateway-auth` solves an
authentication problem crier genuinely has, rather than two services merely
sharing a compose file.

This is a design, not shipped code — `gateway-auth` is not a dependency here.

## Two relationships, not one

### 1. As a source of audit events (IR3)

`gateway-auth` posts to `/v1/logs` like any other client, authenticating with
its own credential:

```
Authorization: Bearer gateway-auth:<credential>
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

Deployed behind `gateway-auth`, crier does not authenticate the caller itself:
the gateway already did, and asserts the principal in a header.

```go
auth, err := httpreceiver.NewTrustedProxy(httpreceiver.TrustedProxyConfig{
    TrustedCIDRs:   []string{"10.4.0.0/16"},   // the gateway's addresses
    IdentityHeader: "X-Authenticated-Source",
})
```

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
D-2). A client asserting `service.name: gateway-auth` has that field
overwritten and the discrepancy counted — the record is still accepted, just
not on the caller's terms.

That matters most for an audit-event source. If a caller could label its own
records as coming from `gateway-auth`, anyone able to reach the ingestion
endpoint could forge audit history, which is a worse outcome than losing it.
`TestClientAssertedIdentityIsOverwrittenAndCounted` is the test.
