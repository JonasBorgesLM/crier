// Package integration holds end-to-end tests that run crier's export layer
// against a real OpenTelemetry Collector in a container (NFR6).
//
// It is a separate module, following the precedent set in moat: testcontainers
// is a large dependency, and a test-only dependency is still a dependency —
// left in the exporter's own module it would appear in the go.sum of every
// consumer that imports it.
//
// Everything here is behind the `integration` build tag, so the default
// `go test ./...` stays fast and needs no infrastructure. Run this suite with:
//
//	go test -tags=integration -race ./...
//
// The unit suite proves the mapping and the classification table against a
// handwritten server, which is exactly the shape of test that agrees with
// whatever the implementation happens to do. This one exists because a real
// collector disagrees: it parses the protobuf for itself, it decides its own
// status codes, and it is the only thing that can show a payload crier
// considers well-formed being rejected at the far end.
package integration
