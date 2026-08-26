# task-api: crier as an embedded library

**IR4.** `task-api` already served as the real-world consumer for `moat`.
Reusing it keeps the ecosystem story concrete: the same application that proved
one library is the one that exercises this one.

This is a design, not shipped code — `task-api` is not a dependency here.

## What it looks like

`task-api` is a single Go service. It has no separate log-shipping sidecar, no
agent, and no reason to run one: it imports the engine and calls it.

```go
crier, err := core.New(core.Options{
    ServiceName:    "task-api",
    ServiceVersion: build.Version,
    Exporters:      map[string]core.Exporter{"otlp": otlpExporter},
    Redactor:       redactor,
    Limits:         core.Limits{MaxAttributes: 64},
})
if err != nil {
    return fmt.Errorf("log pipeline: %w", err)
}
defer func() {
    summary, _ := crier.Shutdown(shutdownCtx)
    log.Printf("crier: %s", summary)
}()
```

Records go in through `Log`, which returns once the record is buffered rather
than once it is exported. `task-api`'s request latency therefore does not
depend on whether the observability backend is healthy — which is the whole
reason to have a buffer between them (ADR-0001).

## What this integration actually exercises

**There is no receiver, and that is the point.** In embedded mode the host
application is the trust boundary (FR11): `task-api` decides what is a log
line, so there is nothing to authenticate and no wire format to parse. The
module split exists so this case costs nothing — `task-api` imports `core` and
gets no HTTP server, no `moat`, and no exporter it did not ask for (ADR-0020).

**Input limits still apply.** A bug in `task-api` that interpolates a growing
map into an attribute produces exactly the unbounded record a malicious client
would, and the buffer cannot tell them apart (ADR-0010). The limits are not a
receiver feature that embedding skips; they are pipeline stages, and
`TestInputLimitsApplyToTheEmbeddedPathToo` is what keeps that true.

**Composition is not `task-api`'s problem.** `core.New` builds
`FanOut(Retry(CircuitBreaker(e)))` for each destination. A host assembling that
by hand can get the order wrong and silently reintroduce duplicate
amplification (ADR-0013), and it would not find out until a backend went down.

**Shutdown accounting reaches the host.** `Shutdown` returns a summary rather
than logging it, because `core` has no logger and should not acquire one. What
`task-api` does with "42 records lost" is `task-api`'s decision — but it is a
decision, which is what the return value makes it (ADR-0015).

## What crier gets back from it

The embedded API's shape came from asking what a real consumer would need:
identity asserted once at construction rather than per record, a `Log` that
returns an error the caller can act on, and a filtered record not counting as a
failure. Each of those is a decision that looks arbitrary until an actual host
application has to live with it.
