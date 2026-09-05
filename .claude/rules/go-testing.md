---
paths:
  - '**/*_test.go'
description: 'Go test conventions for crier: table-driven, counter assertions, ExampleXxx'
---

# Go testing conventions

- **Table-driven.** A `[]struct{ name string; ... }` slice run through
  `t.Run(tt.name, ...)`. A case with more than one input that isn't
  table-driven needs a reason, not a habit.
- **Assert the specific behaviour and counter, not just "no error".** Where
  the code under test touches a counter — admission, redaction, degradation,
  per-source quota — assert *that counter's* value. `err == nil` alone proves
  nothing about which reason was recorded; a check that can't tell "dropped
  for capacity" from "dropped for backend unavailable" hasn't verified
  ADR-0005 / ADR-0011 / ADR-0015 ("No silent drops" in the root CLAUDE.md).
- **Public packages carry a testable `ExampleXxx`**, matching `moat`'s
  convention. It is both documentation and a compile-checked usage guarantee.
- **A suite must fail when it cannot run**, not skip and exit 0. Where the
  dependency is guaranteed to exist — a real collector in CI for
  `exporters/otlp/integration`, for instance — its absence is a failure, not a
  skip. See "A check must fail when it cannot run" in the root CLAUDE.md.
- **Verify a new guard by breaking what it guards.** Before trusting a new
  assertion, comment out or bypass the code path it checks and confirm the
  test actually goes red. A guard nobody has seen fail is not known to work.
