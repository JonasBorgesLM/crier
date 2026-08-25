# ADR-0004: OpenTelemetry-aligned schema and trace correlation

## Status
Accepted

## Context
An internal `LogRecord` schema designed ad hoc would require a lossy mapping
step at export time and would prevent logs from being correlated with traces
— one of the main things that separates a generic "log shipper" from an
actual observability component.

## Decision
`LogRecord` is modeled directly on the OpenTelemetry Logs data model from the
start: `Timestamp`, `SeverityNumber`/`SeverityText`, `Body`, `Attributes`,
`Resource` (following OTel semantic conventions — `service.name`,
`service.version`, `deployment.environment`, etc.), and optional
`TraceID`/`SpanID` fields. Receivers populate `Resource` once per source and
attach it to every batch; `TraceID`/`SpanID` are accepted from the caller
when available (e.g. propagated from an active OTel context) but are never
required.

## Consequences
- The OTLP exporter (ADR planned separately as exporters ship) becomes a
  near-direct mapping rather than a translation layer, reducing surface for
  bugs and data loss.
- Any future exporter (Datadog, Loki, Elasticsearch) maps *from* this common
  schema, keeping `core` exporter-agnostic.
- Consumers that don't have tracing in place simply omit `TraceID`/`SpanID`
  — no forced adoption of tracing to use the log pipeline.

## Amendment (ADR-0008, ADR-0009)
Two aspects of the schema were under-specified here:

- **Resource identity is not client-asserted.** ADR-0008 establishes that the
  identity fields of `Resource` are overwritten from the authenticated
  principal. Descriptive resource attributes from the client are preserved;
  identity fields are not.
- **`ObservedTimestamp` was missing.** ADR-0009 adds it as the
  pipeline-assigned, authoritative timestamp, with the client-supplied
  `Timestamp` retained but untrusted. The draft in `core/record.go` predates
  this decision and must be updated before implementation.
