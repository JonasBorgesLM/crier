// Package httpreceiver is crier's HTTP ingestion endpoint.
//
// It lives in its own module (ADR-0020) because it depends on moat for the
// request-level guards required by IR1, and core must stay free of
// third-party dependencies (NFR1). An embedded consumer imports core and gets
// no HTTP server at all — in embedded mode the host application owns the
// trust boundary, so there is no receiver to own it (FR11).
//
// # What a 202 means
//
// A record that is accepted returns 202: it was admitted to the buffer. It
// says nothing about any backend having stored it. Delivery is at-least-once
// and acceptance is not delivery (ADR-0009); a caller that reads 202 as
// "stored" will be wrong during every export outage.
//
// # Trust boundary
//
// Identity is derived from the authenticated principal and never from the
// request body (ADR-0008). A client that asserts its own service.name has
// that field overwritten and the discrepancy counted — the record is still
// accepted, attributed to whoever actually authenticated.
package httpreceiver
