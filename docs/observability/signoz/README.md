# SigNoz dashboard for a crier consumer

A dashboard describing the logs one service ships through crier: volume,
severity, which endpoints produce it, and who is generating activity.

This is a **template and a procedure, not a provisioner** — crier does not
create dashboards for you, and holds no SigNoz credential.
[ADR-0023](../../adr/0023-dashboards-ship-as-a-template.md) records why, and
what would reverse that.

## Provisioning it

One placeholder, `{{.ServiceName}}`, substituted with the `ServiceName` crier
is configured with:

```bash
SIGNOZ=http://localhost:8080          # your SigNoz
SERVICE=task-api                      # the service whose logs these are

sed "s/{{\.ServiceName}}/$SERVICE/g" dashboard.json |
  curl -sS -X POST "$SIGNOZ/api/v2/dashboards" \
    -H "SIGNOZ-API-KEY: $(cat ~/.signoz-pat)" \
    -H 'Content-Type: application/json' \
    --data-binary @-
```

`201` means created. The token is a SigNoz API key, minted in the UI; keep it
in a file rather than an environment variable your shell history will remember.

**The header is `SIGNOZ-API-KEY`, not `Authorization: Bearer`.** An API key
sent as a bearer token is rejected with `401 unauthenticated` — the bearer form
is for a session JWT. Issue #72's original notes said `Authorization: Bearer`,
which is where this was corrected.

## What is in it

Ten panels, in four rows:

| Row | Panels |
| --- | --- |
| Counters | records ingested, distinct `request_id`, errors, health-check volume |
| Over time | volume by severity, volume by endpoint |
| Ranking | top endpoints by volume (table), distinct users over time |
| Detail | WARN by endpoint, p95 of HTTP status |

The endpoint panels are pointed: a service whose health probes dominate its own
log volume is the case
[ADR-0022](../../adr/0022-attribute-matched-sampling.md) exists for, and this
dashboard is where you would see it before writing the rule.

## Verified, and how

**Captured and verified against SigNoz `v0.139.0` on 2026-09-05**, self-hosted
via the Foundry Compose deployment, holding real `task-api` logs.

Verification was two separate claims, because passing the first proves nothing
about the second:

1. **The schema is accepted.** `POST /api/v2/dashboards` returned `201`.
2. **Every panel returns data.** Each panel's query was extracted and executed
   against `POST /api/v5/query_range` over the real log data: **10 of 10
   returned values**, not empty series.

The second check matters because a dashboard can be perfectly schema-valid and
render ten empty charts. A field name that does not exist fails loudly here —
verified deliberately, by querying a nonexistent field and watching it error
rather than return an empty result — so a query that returns rows is evidence
the field names are right.

Every shape in this file was confirmed empirically rather than taken from
documentation, which is the method ADR-0023 requires and the reason the
original capture exists at all: SigNoz's dashboard validation is stricter than
its public docs, and the docs' shape does not post.

Specifically confirmed on v0.139.0:

- `tags` is `[{"key": ..., "value": ...}]`. **An array of strings is rejected**
  — the shape captured from an earlier SigNoz version, which is how stale this
  contract can get between versions.
- Panel kinds are enumerated by the server itself: `signoz/BarChartPanel`,
  `signoz/HistogramPanel`, `signoz/ListPanel`, `signoz/NumberPanel`,
  `signoz/PieChartPanel`, `signoz/TablePanel`, `signoz/TimeSeriesPanel`.
- `signoz/NumberPanel` and `signoz/TablePanel` take `plugin.spec: {}`; the
  `TimeSeriesPanel` spec fields (`fillSpans`, and so on) are rejected on them.
- `groupBy` is `[{"name": "<field>"}]`.
- A composite query accepts several named `builder_query` entries (`A`, `B`, …).
- Filter expressions support `=`, `AND`, `OR`, and reference resource fields
  (`service.name`), log fields (`severity_text`), and attributes (`path`) by
  bare name.
- Aggregations used here: `count()`, `count_distinct(<field>)`, `p95(<field>)`.
- Ordering for a top-N table is `order: [{"key": {"name": "count()"},
  "direction": "desc"}]` with `limit`. Ordering by `__result_0` is rejected.

## What is deliberately not in it

**No host or server metrics.** Those are the `metrics` signal, and crier ships
logs — it has no host metrics to send. The instance this was verified against
had **zero** metric names, so any infrastructure panel here would have rendered
empty while looking authoritative, which is the failure this repository spends
most of its effort refusing.

The query shape for metrics *is* confirmed valid (`signal: "metrics"` with
`aggregations: [{"metricName": ..., "spaceAggregation": ..., "timeAggregation":
...}]` posts successfully), so adding server panels is not a research problem —
it needs a metrics pipeline pointed at the same SigNoz, such as an OpenTelemetry
Collector with the `hostmetrics` receiver, or the k8s-infra agent. Once metrics
exist, extend this file and re-run the two checks above.

## When this goes stale

Nothing in CI exercises this file — stated plainly, because a template no test
touches is documentation, and calling it verified would be the same fail-open
shape `docs/audit-log.md`'s V-1…V-6 sweep found six times.

It was true for v0.139.0 on the date above. The `tags` change proves the
contract moves between versions. If a POST starts failing, re-capture rather
than guess: build the panel in the SigNoz UI and read it back with
`GET /api/v2/dashboards/{id}`, which is how every shape here was learned.
