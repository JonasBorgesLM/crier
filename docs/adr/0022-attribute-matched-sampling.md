# ADR-0022: Attribute-matched sampling narrows, never widens

## Status
Accepted — extends ADR-0010

## Context
Step 7 of the canonical stage order (ADR-0010) decides what reaches the buffer
from two inputs: the record's severity, and the attested source identity that
selects a `PerSource` override. Nothing else about the record is visible to it.

That gap has a real deployment behind it rather than a hypothetical. `task-api`
embeds crier and logs a line per request; its readiness and liveness probes are
requests, arriving as often as the orchestrator polls. Every one of those lines
carries the same `ServiceName`, so `PerSource` cannot tell them apart from the
checkout request beside them — the only selector available covers the whole
source, and turning it down turns down everything the service does (issue #71).
Any embedded application with a high-volume, low-information endpoint has the
same shape.

The obvious extension — select on an attribute — runs straight into a sentence
the code had already written down, in the very field it would extend:

> `PerSource` overrides the global settings by attested source identity
> (ADR-0008) — never by anything the client asserts, or a noisy source could
> exempt itself from its own sampling.

Attributes are precisely "something the client asserts". Adding attribute
matching without answering that sentence would reverse ADR-0008's reasoning by
accident, in the stage where a noisy source has the most to gain from it.

## Decision

**An attribute rule may lower what a source keeps. It may never raise it.**

That single constraint is what reconciles the extension with the sentence
above, and it is worth being precise about why. The hazard that sentence names
is an override that *widens*: a source able to raise its own sample rate has
exempted itself from a limit the operator set. A rule that can only narrow
cannot be abused that way, because evading it — by not setting the attribute,
or by setting it to something else — returns the record to the settings the
source's identity already established, and no further. Identity sets the
ceiling; attributes only sit below it.

Everything upstream is unchanged and still keyed to attestation: the per-source
floor and shared pool (ADR-0011, ADR-0019), the input limits (ADR-0010), and
identity attestation itself (ADR-0008). Nothing a caller puts in a payload
takes it above the ceiling those already give it, or takes anything from
another source.

An attribute rule is therefore a **volume control the source cooperates with**,
not an isolation control. Cooperation is the ordinary case: a health-check
flood is noise from a service that gains nothing by sending it.

### The mechanism

**A rule is an attribute key plus an exact value or a value prefix**, in an
ordered list. The first rule whose key is present on the record and whose value
matches decides; no rule matching leaves the source's own settings in force.

No regular expressions and no expression language. Redaction already paid for
that lesson and left the receipt: `SensitiveKeySubstrings` is a substring list
rather than a regex set because a substring scan is "roughly an order of
magnitude cheaper than a regex per attribute per record", and the benchmark
"made that difference the dominant cost of the whole pipeline". Filtering runs
on the same hot path, on records it is about to discard, where spending more
than the record is worth is exactly backwards.

**String-valued attributes only.** A rule against an attribute holding a number
or a bool does not match, rather than matching some rendering of it. Choosing
that rendering would put a number format inside a selector's behaviour, and
ADR-0021 is this project's record of what an unstated edge in a contract costs
later.

**Not the body.** Two reasons, either sufficient alone. Cost: scanning the body
is the most expensive thing this pipeline does (ADR-0014), and doing it to
decide whether to throw the record away inverts the reason filtering runs
before the buffer at all. Stability: message text is the least stable part of a
log line, so a body-matched rule stops matching the day somebody rewords a log
message — and a sampling rule that silently stops sampling is a cost regression
with nothing to alert on. Structured attributes are the stable surface, which
is the same reason ADR-0014 makes them redaction's reliable path and the body
best-effort.

**The sample floor still applies, and a rule cannot lower it.** `SampleFloor`
(default `SeverityError`) exempts severe records from sampling, and an
attribute rule does not override it: a health endpoint that has started failing
is exactly the case its own sampling rule must not suppress.

**No new stage.** This extends step 7 rather than adding a step, and the
position earns its keep twice. A record an attribute rule discards still never
reaches the buffer, which is the whole reason filtering a probe flood beats
admitting it and dropping it later. And because redaction is step 6, the
attributes this stage sees are already masked — a rule keyed on a sensitive
attribute matches `[REDACTED]`, not the secret, so rules cannot be written to
select on secret values.

**Counting is unchanged.** A record an attribute rule removes is filtered, not
dropped: `RecordsFiltered`, never `RecordsDropped`. It was never meant to leave.

## Consequences
- **An uncooperative source can opt out of its own attribute rules** and get
  the volume its identity already allowed. This belongs in the documentation
  where the rules are configured rather than left to be inferred, because the
  mistake it invites is an operator reading a cost control as an enforcement
  control. Enforcement is per-source admission — a different mechanism, in a
  different stage, keyed to something the caller cannot set.
- **The metric does not say which criterion filtered a record.**
  `RecordsFiltered` is labelled by source; severity, sampling and an attribute
  rule are indistinguishable in it. The per-destination filter added in #45 has
  the same property for the same reason: changing the `Metrics` interface is a
  separate decision, and not one this needs.
- **Configuration grows, from a base smaller than the library's.** `crierd`'s
  `FilterConfig` exposes `MinSeverity` and `SampleRate` and not even
  `PerSource`; both that and the rule list have to reach the config file before
  any of this is usable from the daemon.
- **Rules are ordered, and the order is the operator's to read.** First match
  wins, with no scoring and no implicit specificity. A precedence scheme would
  be shorter to write in a config file and impossible to predict from one.
- **Narrow-only means enumerating what to drop, not what to keep**, which is
  more typing for an operator who wants one loud path kept under a global
  sample. It fails in the right direction: a record matching no rule is kept. A
  keep-list loses the record nobody thought to name, which is the one worth
  having at 3am.
- **Rules are not scoped to a source.** A rule matching `path` applies to every
  source that sets `path`. A source needing different treatment gets it through
  `PerSource`, which sets the ceiling the rules narrow from. If a real
  multi-tenant case needs per-source rules, that is a later ADR with a case
  behind it rather than a field added now on the guess that someone will want
  one.
- **The matched-rule set is bounded by the configuration file**, not by client
  input, so this adds no unbounded label vector of the kind ADR-0010's
  cardinality guard exists for.
