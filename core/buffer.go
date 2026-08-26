package core

import (
	"context"
	"errors"
	"fmt"
)

// Buffer errors. They are distinct because the receiver maps them to different
// responses and an operator reads them as different problems (ADR-0002,
// ADR-0011).
var (
	// ErrBufferFull means the buffer as a whole is at capacity. Under the
	// default policy the receiver answers 503.
	ErrBufferFull = errors.New("buffer full")
	// ErrBufferClosed means the buffer is shutting down and will accept
	// nothing further. Records already inside are still drained.
	ErrBufferClosed = errors.New("buffer closed")
	// ErrSourceQuotaExhausted means this source's fair share is spent while
	// the buffer still has room (ADR-0011). Distinct from ErrBufferFull on
	// purpose: the operator response is entirely different.
	ErrSourceQuotaExhausted = errors.New("source quota exhausted")
)

// DropPolicy is what happens to a record when the buffer is full (ADR-0002).
type DropPolicy int

const (
	// DropPolicyReject returns ErrBufferFull. The default, because a silent
	// drop must never be the out-of-the-box behaviour — a caller opts into
	// losing data knowingly.
	DropPolicyReject DropPolicy = iota
	// DropPolicyBlock makes the sender wait for room.
	DropPolicyBlock
	// DropPolicyDropOldest evicts the oldest pending record to make room.
	DropPolicyDropOldest
)

// String implements fmt.Stringer.
func (p DropPolicy) String() string {
	switch p {
	case DropPolicyReject:
		return "reject"
	case DropPolicyBlock:
		return "block"
	case DropPolicyDropOldest:
		return "drop-oldest"
	default:
		return fmt.Sprintf("DropPolicy(%d)", int(p))
	}
}

// Valid reports whether p is a defined policy.
func (p DropPolicy) Valid() bool {
	return p >= DropPolicyReject && p <= DropPolicyDropOldest
}

// BufferStore holds records between ingestion and export (FR3, ADR-0002).
//
// It is an interface so a durable, WAL-backed implementation can replace the
// in-memory one without changing callers. None ships in the MVP; the seam
// exists so that adding one later is not a rewrite. It mirrors the Store split
// already validated in moat.
//
// Implementations must be safe for concurrent use by many producers and many
// consumers.
type BufferStore interface {
	// Enqueue admits one record. It returns ErrBufferFull, ErrBufferClosed,
	// or ctx.Err(); under DropPolicyDropOldest a successful Enqueue may have
	// evicted an older record, which is counted rather than reported.
	Enqueue(ctx context.Context, rec LogRecord) error

	// DequeueBatch returns the next batch, blocking until the batch is full,
	// the batch window expires, or ctx is done.
	//
	// After Close it returns whatever remains, one batch at a time, and then
	// ErrBufferClosed. That is what makes a bounded drain possible: the
	// consumer keeps calling until it sees ErrBufferClosed or runs out of
	// time (ADR-0015).
	DequeueBatch(ctx context.Context) ([]LogRecord, error)

	// Depth reports how many records are currently held.
	Depth() int

	// Close stops admission. It is idempotent and never discards records that
	// are already inside — draining them is the consumer's job.
	Close() error
}
