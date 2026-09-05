module github.com/JonasBorgesLM/crier/cmd/crierd

go 1.26

require (
	github.com/JonasBorgesLM/crier/core v0.3.0
	github.com/JonasBorgesLM/crier/exporters/otlp v0.1.0
	github.com/JonasBorgesLM/crier/receivers/http v0.1.0
	github.com/JonasBorgesLM/moat v0.2.0
)

require (
	go.opentelemetry.io/proto/slim/otlp v1.10.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
