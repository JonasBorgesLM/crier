// Package otlp exports crier log batches over OTLP/HTTP.
//
// It lives in its own module (ADR-0003) so that consumers embedding core as a
// library do not inherit the OTLP dependency graph.
//
// Transport is HTTP with binary protobuf, not gRPC (ADR-0017): one protocol
// stack is easier to secure and proxy than two, and the canonical proto module
// carries gRPC service stubs whose own dependencies would raise this module's
// Go floor above the one NFR2 fixes.
//
// Credentials are held as masked secrets, never plain strings (NFR4, IR2).
//
// # Composition
//
// This exporter is the innermost layer. Compose it as ADR-0013 requires —
// retry and circuit breaking per destination, inside the fan-out:
//
//	exporter, err := otlp.New(otlp.Config{Endpoint: "https://collector:4318"})
//	breaker, err := core.NewCircuitBreaker(core.CircuitBreakerConfig{Name: "primary", Exporter: exporter})
//	retry, err := core.NewRetry(core.RetryConfig{Name: "primary", Exporter: breaker})
//	fanOut, err := core.NewFanOut(core.FanOutConfig{
//	    Destinations: []core.Destination{{Name: "primary", Exporter: retry}},
//	})
package otlp
