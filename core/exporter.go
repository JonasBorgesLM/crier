package core

import (
	"context"
	"errors"
)

// ErrPermanent marks a failure that retrying cannot fix: a malformed payload,
// a rejected credential, a 4xx other than 429. A retry decorator that sees it
// must give up immediately rather than spend its budget — and delay every
// batch queued behind it — on a batch the backend will never accept
// (ADR-0013).
//
// Exporters signal it by wrapping: fmt.Errorf("%w: %v", ErrPermanent, err).
var ErrPermanent = errors.New("permanent export failure")

// IsPermanent reports whether err is a permanent failure.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

// Exporter sends a batch of records to one destination.
//
// Implementations must be safe for concurrent use: fan-out dispatches to every
// exporter at once (ADR-0013).
//
// Export returns nil only when the destination has accepted the whole batch.
// A partial success must be reported as an error, because the caller's only
// recovery is to re-send the batch — which is within the at-least-once
// contract (ADR-0009).
type Exporter interface {
	// Export delivers batch, honouring ctx for cancellation and deadlines.
	// Returning an error wrapping ErrPermanent tells the retry decorator not
	// to try again.
	Export(ctx context.Context, batch []LogRecord) error

	// Shutdown releases the exporter's resources. It is called once, after
	// the pipeline has drained, and must not block past ctx's deadline
	// (ADR-0015).
	Shutdown(ctx context.Context) error
}

// Decorators wrap a single Exporter and are themselves Exporters. The
// composition order is a correctness property, not a style choice:
//
//	FanOut(
//	    Retry(CircuitBreaker(exporterA)),
//	    Retry(CircuitBreaker(exporterB)),
//	)
//
// Retry belongs *inside* fan-out, per exporter. Composed the other way round —
// Retry(FanOut(a, b)) — a failure in b re-sends the whole batch, so a healthy a
// receives it once per attempt because an unrelated destination is broken. That
// is duplicate amplification, audit finding A-1, and ADR-0013 exists to forbid
// it. FanOut therefore performs no retry of its own; it dispatches once per
// exporter and joins the results.
