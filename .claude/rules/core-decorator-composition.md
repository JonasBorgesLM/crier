---
paths:
  - 'core/fanout.go'
  - 'core/retry.go'
  - 'core/circuitbreaker.go'
  - 'core/fanout_test.go'
  - 'core/retry_test.go'
  - 'core/circuitbreaker_test.go'
description: 'Decorator composition order for FanOut/Retry/CircuitBreaker (ADR-0013)'
---

# Decorator composition: retry inside fan-out

`FanOut(Retry(CB(a)), Retry(CB(b)))` — never the other order. Wrapping the
whole fan-out in one shared `Retry` re-sends *every* destination's batch when
*any one* fails, so a healthy exporter gets hammered by retries caused by an
unrelated broken one. This is ADR-0013, decided from audit finding A-1. It is
not a style preference and does not get re-litigated per change.

Each decorator wraps exactly one exporter. A new decorator (a rate limiter,
say) goes at the same layer — inside the fan-out, wrapping one destination —
never around the aggregate.

When touching `fanout_test.go`: the regression that actually matters is a test
that asserts overall delivery succeeded without also asserting that the
*healthy* destination's call count stayed independent of the broken one's
retry count. That independence is the entire point of A-1 — a test that only
checks "some destination got the batch" cannot catch its recurrence.
