module github.com/JonasBorgesLM/crier/exporters/otlp

go 1.24.0

replace github.com/JonasBorgesLM/crier/core => ../../core

require (
	github.com/JonasBorgesLM/crier/core v0.0.0-00010101000000-000000000000
	github.com/JonasBorgesLM/moat v0.2.0
	go.opentelemetry.io/proto/slim/otlp v1.10.0
	google.golang.org/protobuf v1.36.11
)
