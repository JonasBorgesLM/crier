module github.com/JonasBorgesLM/crier/cmd/crierd

go 1.24.0

replace (
	github.com/JonasBorgesLM/crier/core => ../../core
	github.com/JonasBorgesLM/crier/exporters/otlp => ../../exporters/otlp
)

require (
	github.com/JonasBorgesLM/crier/core v0.1.0
	github.com/JonasBorgesLM/crier/exporters/otlp v0.0.0-00010101000000-000000000000
	github.com/JonasBorgesLM/crier/receivers/http v0.0.0
	github.com/JonasBorgesLM/moat v0.2.0
)

require (
	go.opentelemetry.io/proto/slim/otlp v1.10.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/JonasBorgesLM/crier/receivers/http => ../../receivers/http
