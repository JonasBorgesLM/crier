// Package otlp exports crier log batches over OTLP.
//
// It lives in its own module (ADR-0003) so that consumers embedding core as a
// library do not inherit the OTLP SDK's dependency graph.
//
// Credentials are held as masked secrets, never plain strings (NFR4, IR2).
//
// Implementation is tracked in milestone M2; this package is currently a
// placeholder that reserves the module path and its CI entry.
package otlp
