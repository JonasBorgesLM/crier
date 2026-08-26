# ADR-0021: Where the v1 wire contract ends

## Status
Accepted — qualifies ADR-0012

## Context
ADR-0012 states the compatibility policy for the ingestion format: unknown
fields are rejected, optional fields may be added, removing or retyping a field
needs a new path version. Implementing v1 surfaced two questions it does not
answer, and both were briefly answered only by code.

**Field-name matching.** "Unknown fields are rejected" reads as absolute and is
not. `encoding/json` matches field names without regard to case, so
`severtyText` is rejected while `servicename` is quietly accepted as
`serviceName`. Nothing in the ADR admits the exception, so a reader takes the
rule at face value.

**The response.** The compatibility rules are written entirely about requests.
A caller parses the response too, and during M3 a response field was renamed
from `admitted` to `accepted`. Whether that was a breaking change was
unanswerable, because nothing said whether the response is inside the contract
— and "unanswerable" is the worst state for the next person to modify it.

## Decision

**Case-insensitive field matching is accepted, and stated.** A misspelling that
still reaches its intended field is not the failure ADR-0012 exists to prevent.
That failure is a field which silently does *nothing* — a service that misspells
`severityText`, gets 202, and emits records with no severity while looking
healthy. `servicename` loses no data; it arrives exactly where the sender meant
it to.

Closing the gap would mean decoding each object twice — once to see the keys as
written, once to bind them — to enforce a spelling the format never promised to
police. The cost is on every request; the benefit is a stricter error for a
client whose data already arrived intact.

**The response body is part of the versioned contract**, under the same rules
as the request:

- Adding a field is backwards-compatible.
- Renaming, removing, or retyping a field is breaking, and needs a new path
  version.
- The **text** of the `reason` field is explicitly outside the contract. It is
  diagnostic prose for a human reading a log; a client that branches on its
  wording is relying on something no version promises. Whether `reason` is
  present, and the status code beside it, are contractual.

Callers parse responses. Treating the body as decoration means a rename lands
in a patch release and breaks a client that was reading it as documented, which
is the same class of silent breakage strict field rejection exists to prevent —
arriving from the server's side.

`admitted` → `accepted` therefore happened inside the contract, and was
non-breaking for one reason only: it landed before anything was tagged.

## The trigger

That reason expires, so it is written here as a check to run rather than a fact
to remember. **Before renaming, removing, or retyping any field in the response
— including the `accepted` and `rejected` counts — run:**

```bash
git tag --list 'receivers/http/v*'
```

- **Nothing printed.** No release carries the field names. The change is free;
  make it now rather than after the tag exists.
- **Anything printed.** The names are frozen. The change is breaking, and it
  needs a new path version (`/v2/logs`) served alongside v1 for a migration
  window, with the older version reporting its deprecation through the header
  and the metric ADR-0012 reserves for exactly this.

The same check governs the request schema, and always did — this ADR only adds
the response to what it covers.

Adding a field is not on this list: it is backwards-compatible and needs no
check.

## Consequences
- The response schema needs the same review a request-schema change gets. In
  practice this makes the counts and their meanings hard to change casually,
  which is the intent.
- The freedom is time-limited and the deadline is a command, not a memory. A
  consequence written as an observation is one somebody reads after breaking
  something; written as a check with two branches, it is one somebody runs
  before.
- Keeping `reason`'s text out of the contract preserves the freedom to improve
  a diagnostic message, which is the part most likely to want improving and
  the part least useful to depend on.
- The case-insensitivity exception is now something a reader of ADR-0012 will
  find. It is also pinned by a test, so if a future decoder changes the
  behaviour, the exception is re-examined rather than silently reversed.
- A stricter matcher remains possible later — that direction only rejects
  payloads previously accepted, so it is a breaking change and would arrive
  with a new path version, which is exactly the process ADR-0012 sets up.
