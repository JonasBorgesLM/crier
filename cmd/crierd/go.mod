module github.com/JonasBorgesLM/crier/cmd/crierd

go 1.24

require (
	github.com/JonasBorgesLM/crier/core v0.0.0
	github.com/JonasBorgesLM/crier/exporters/otlp v0.0.0
)

replace (
	github.com/JonasBorgesLM/crier/core => ../../core
	github.com/JonasBorgesLM/crier/exporters/otlp => ../../exporters/otlp
)
