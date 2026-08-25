# ADR-0006: Redaction strategy for sensitive fields

## Status
Accepted

## Context
Logs frequently end up carrying sensitive data (tokens, secrets, PII)
accidentally logged by upstream applications. Exporting that data unmodified
to third-party observability backends turns an operational tool into a data
leak vector. Reinventing a masking primitive from scratch would also ignore
work already done and audited in `moat`.

## Decision
Redaction is a configurable processing stage between normalization and
buffering (see FR7). It operates on `LogRecord.Attributes` using a set of
field-name and/or regex rules, masking matched values before they ever reach
the buffer — so masked data never touches disk (if a durable buffer is added
later) or memory longer than necessary. The masking approach follows the
same philosophy validated in `moat`'s `secret` package (values are masked in
place, not merely marked, and equality/formatting never expose the
unmasked value).

## Consequences
- Redaction happens before buffering, so a buffer inspection or a future WAL
  file never contains unmasked sensitive values.
- Rule configuration (field names/patterns to mask) is part of the service's
  startup config and validated eagerly, consistent with NFR4.
- This creates an explicit, documented dependency of `crier` on the
  design lessons from `moat`, reinforcing the two projects as a coherent
  toolkit rather than isolated repos.

## Amendment (ADR-0014)
Two gaps in this ADR were found in audit and corrected by ADR-0014:
redaction scope was limited to `Attributes` and did not cover `Body` (where
accidentally logged secrets most commonly appear), and the failure policy was
undefined, defaulting implicitly to fail-open. ADR-0014 extends scope to the
body and mandates fail-closed behavior.
