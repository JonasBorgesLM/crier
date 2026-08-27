# ADR-0014: Redaction scope and failure policy

## Status
Accepted — extends ADR-0006 after audit findings A-2 and A-3

## Context
ADR-0006 established redaction as a pipeline stage operating on
`LogRecord.Attributes`. Two problems were found in audit.

**A-2: `Body` was not covered.** In practice, accidentally logged secrets most
often appear inside the message text, not in a structured attribute:

    log.Printf("auth failed for token=%s", tok)

A redaction stage that masks only attributes therefore misses the most common
leak path while appearing to provide protection. A security control that
creates false confidence is worse than an absent one, because it stops anyone
from looking further. This matters especially given IR3: `gateway-auth` is an
intended source, and authentication events are exactly where credentials and
tokens surface.

**A-3: failure behavior was undefined.** If a redaction rule fails to compile
or the stage errors at runtime, nothing said whether the record proceeds
unmasked (fail-open) or is dropped (fail-closed). The implicit default was
fail-open — silently exporting the very data the stage exists to protect.

## Decision

**Scope.** Redaction applies to record attributes, resource attributes, and the
`Body`. Body redaction is pattern-based (configurable regex rules with sensible
defaults for common credential shapes: bearer tokens, `key=value` pairs with
sensitive-looking keys, JWT-shaped strings). Matched spans are replaced with a
marker, not deleted, so the surrounding message stays readable and the fact
that redaction occurred is visible to whoever reads the log.

Body redaction is acknowledged as best-effort: pattern matching over free text
cannot be complete. This limitation is documented explicitly rather than
implied, and the README recommends structured attributes over interpolated
message text precisely because attribute-level redaction is reliable in a way
body-level redaction cannot be.

**Failure policy is fail-closed.** Rules are compiled and validated eagerly at
startup (NFR4); a rule that fails to compile prevents startup. If the redaction
stage fails at runtime for a record, that record is dropped and counted, never
exported unmasked. This follows the precedent already set in `moat`, where the
Redis-backed rate limiter fails closed when Redis is unavailable: a security
control that degrades into permitting the thing it guards against is not a
control.

Fail-closed is not configurable into fail-open. An operator who wants
unredacted export disables redaction explicitly, which is an auditable
configuration choice rather than a silent degradation.

## Consequences
- Body redaction adds regex evaluation per record on the hot path, over a field
  that is typically the largest. This is the most expensive stage in the
  pipeline and must be measured in the benchmark required by ADR-0010.
- Dropping records on redaction failure means a systematically failing rule
  causes total telemetry loss for affected records. The drop counter is
  therefore a required alerting signal, not merely a diagnostic one.
- Refusing to start on an invalid rule turns a config typo into a deployment
  failure. This is intended: the alternative is a service that runs while
  silently leaking.
