package otlp_test

import (
	"fmt"

	"github.com/JonasBorgesLM/moat/secret"

	"github.com/JonasBorgesLM/crier/core"
	"github.com/JonasBorgesLM/crier/exporters/otlp"
)

// Example shows a destination composed the way ADR-0013 requires: the exporter
// innermost, then its circuit breaker, then its retry, then the fan-out.
//
// Composed the other way round — a retry around the fan-out — one broken
// destination re-sends the batch to every healthy one, once per attempt.
func Example() {
	exporter, err := otlp.New(otlp.Config{
		Endpoint: "https://collector.example.com:4318",
		// Held masked, so a config dump or a panic cannot print it.
		Credential: secret.New([]byte("ingest-token")),
	})
	if err != nil {
		panic(err)
	}

	breaker, err := core.NewCircuitBreaker(core.CircuitBreakerConfig{Name: "primary", Exporter: exporter})
	if err != nil {
		panic(err)
	}
	retry, err := core.NewRetry(core.RetryConfig{Name: "primary", Exporter: breaker})
	if err != nil {
		panic(err)
	}
	fanOut, err := core.NewFanOut(core.FanOutConfig{
		Destinations: []core.Destination{{Name: "primary", Exporter: retry}},
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("posting to", exporter.Endpoint())
	fmt.Println("destinations:", fanOut.Names())

	// Output:
	// posting to https://collector.example.com:4318/v1/logs
	// destinations: [primary]
}
