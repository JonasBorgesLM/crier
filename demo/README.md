# The demo

```bash
docker compose -f demo/compose.yaml up --build
```

Then open **http://localhost:3000**. It lands on a dashboard, no login, no data
source to configure. Give it a few seconds — the first records arrive once
`crierd` reports ready.

```
checkout-service ──▶ crierd ──▶ OpenTelemetry Collector ──▶ Loki ──▶ Grafana
   (example app)      (crier)         (otlp/http)
```

To stop it, and remove everything it created:

```bash
docker compose -f demo/compose.yaml down -v
```

## What to look at

**The `ERROR` record about a failed receipt upload.** The example service sends
it containing an AWS access key in the message text and a credential under an
`api_key` attribute. Both arrive as `[REDACTED]`. That is the thing crier does
that a log shipper generally does not, and it is invisible unless someone
builds the case for it — so the demo builds it.

Redaction is fail-closed: a record crier cannot mask is dropped and counted,
never exported unmasked (ADR-0014). Body redaction is pattern-based and
best-effort, which is why the demo shows both a secret in free text and one
under a structured key — the second is the reliable path.

**The record claiming to come from `billing-service`.** It arrives attributed
to `checkout-service`. Identity comes from the authenticated principal and
never from the request body, and the discrepancy is counted (ADR-0008).

## Sending a record yourself

`crierd` is on `localhost:4318`, so the wire format is one command away:

```bash
curl -i localhost:4318/v1/logs \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer checkout-service:demo-ingest-token' \
  -d '{"records":[{"severityNumber":9,"body":"hello from my terminal"}]}'
```

`202` means the record was admitted to the buffer. It does not mean any backend
has stored it — acceptance is not delivery (ADR-0009).

Misspell a field and see what strict parsing gets you:

```bash
curl -i localhost:4318/v1/logs \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer checkout-service:demo-ingest-token' \
  -d '{"records":[{"body":"hi","severtyText":"ERROR"}]}'
```

The `400` names the field. A service that misspells `severityText` and receives
`202` looks healthy while emitting records with no severity (ADR-0012).

## Health

```bash
curl localhost:9464/healthz   # liveness
curl localhost:9464/readyz    # readiness
```

Stop the collector — `docker compose -f demo/compose.yaml stop collector` — and
watch `/readyz` start failing once the circuit opens. That is the degraded
state, not a crash: the instance is alive, holding what it buffered, and
probing behind the breaker. Start the collector again and it recovers on its
own.

In this demo the admin port is published so you can try that. In a real
deployment it binds to loopback, because the readiness reason names which
destinations are refusing calls.

## Notes on the setup

Every image is pinned. A demo is the part of a repository that rots on its own:
an image that moves, a collector release that renames a config key, and the
first thing a reviewer tries is the first thing that fails.

The ingest credential is in `compose.yaml` as an environment variable, not in
`crier/config.json`. That is how crier expects credentials to be supplied — a
file is committed by accident far more often than an environment is — and this
particular token is a demo value meant to be public.
