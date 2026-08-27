// Package core is crier's engine: the log record model, the ingestion
// pipeline, the buffer, and the exporter contract.
//
// It has zero third-party runtime dependencies (NFR1) so that embedding crier
// as a library does not drag an exporter's SDK into the consumer's build.
// Exporters live in their own modules under exporters/.
//
// # Delivery semantics
//
// Delivery is at-least-once, with no ordering guarantee across batches
// (ADR-0009). A record accepted by the pipeline may be exported more than
// once; callers must tolerate duplicates. Acceptance is not delivery: the
// receiver's 202 response means a record was admitted to the buffer, not that
// any backend has stored it.
package core
