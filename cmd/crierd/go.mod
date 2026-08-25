module github.com/JonasBorgesLM/crier/cmd/crierd

go 1.24

replace (
	github.com/JonasBorgesLM/crier/core => ../../core
	github.com/JonasBorgesLM/crier/exporters/otlp => ../../exporters/otlp
)
