package core

import (
	"context"
	"fmt"
)

// PipelineConfig assembles the processing stages. Build a Pipeline with
// NewPipeline, which validates the configuration eagerly (NFR4).
type PipelineConfig struct {
	// Buffer is where admitted records land. Required.
	Buffer BufferStore

	// Normalizer stamps ObservedTimestamp (step 4). Nil uses a default one.
	Normalizer *Normalizer

	// Limits caps record size (step 5).
	Limits Limits

	// Cardinality guards attribute value cardinality (step 5). Nil disables
	// the guard; the size limits still apply.
	//
	// It runs after the size limits, per ADR-0010's ordering, which has a
	// consequence worth knowing: truncation collapses long values that shared
	// a prefix into one, so the guard sees the truncated form and reports
	// lower cardinality than the source actually emitted. That is the right
	// trade — the backend also only ever sees the truncated form, and it is
	// the backend's series count the guard exists to protect.
	Cardinality *CardinalityGuard

	// Redactor masks sensitive data (step 6).
	//
	// Nil disables redaction entirely, which is the explicit, auditable
	// choice ADR-0014 requires an operator to make. It is not the same as a
	// redactor that fails open — there is no such thing.
	Redactor *Redactor

	// Filter applies the severity threshold and sampler (step 7). Nil keeps
	// everything.
	Filter *Filter

	// Metrics receives pipeline counters, and is wired into any stage above
	// that does not carry its own.
	//
	// Filling in a stage's nil Metrics means mutating a struct the caller
	// supplied, which is worth stating plainly. The alternative is worse: an
	// operator sets Metrics once on the pipeline, every stage keeps its own
	// nil, and the counters that matter most silently stay at zero. A project
	// whose first rule is "no silent drops" cannot ship that default.
	Metrics Metrics
}

// Pipeline applies the canonical stage order to every record, whatever
// receiver it arrived through (ADR-0010).
//
//  4. normalize -> 5. record limits -> 6. redact -> 7. filter/sample -> 8. admit
//
// Steps 1 to 3 — transport limits, parsing, and identity attestation — belong
// to the receiver, because they are about the request rather than the record.
// The attested identity arrives here as Admit's source argument.
//
// The order is a contract, not an implementation detail. Everything cheap and
// everything reductive happens before the buffer, so bounded memory is only
// ever spent on records that will actually be exported. Only step 8 touches
// the BufferStore.
//
// Safe for concurrent use.
type Pipeline struct {
	buffer      BufferStore
	normalizer  *Normalizer
	limits      Limits
	cardinality *CardinalityGuard
	redactor    *Redactor
	filter      *Filter
	metrics     Metrics
}

// NewPipeline validates cfg and assembles the stages.
func NewPipeline(cfg PipelineConfig) (*Pipeline, error) {
	if cfg.Buffer == nil {
		return nil, fmt.Errorf("pipeline needs a buffer")
	}
	if cfg.Filter != nil {
		if err := cfg.Filter.Validate(); err != nil {
			return nil, fmt.Errorf("filter: %w", err)
		}
	}

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NopMetrics{}
	}
	normalizer := cfg.Normalizer
	if normalizer == nil {
		normalizer = &Normalizer{Metrics: metrics}
	} else if normalizer.Metrics == nil {
		normalizer.Metrics = metrics
	}
	limits := cfg.Limits
	if limits.Metrics == nil {
		limits.Metrics = metrics
	}
	if cfg.Cardinality != nil && cfg.Cardinality.Metrics == nil {
		cfg.Cardinality.Metrics = metrics
	}
	if cfg.Filter != nil && cfg.Filter.Metrics == nil {
		cfg.Filter.Metrics = metrics
	}

	return &Pipeline{
		buffer:      cfg.Buffer,
		normalizer:  normalizer,
		limits:      limits,
		cardinality: cfg.Cardinality,
		redactor:    cfg.Redactor,
		filter:      cfg.Filter,
		metrics:     metrics,
	}, nil
}

// Admit runs rec through every stage and, if it survives, enqueues it.
//
// source is the attested principal (ADR-0008). It overwrites the record's
// identity fields before any stage that keys on identity, so a client cannot
// influence its own quota, its own sampling, or how its records are attributed.
// Pass an empty source only where there is genuinely no authenticated
// principal — embedded-library use, where the host application is the
// boundary.
//
// A filtered record returns nil. Filtering is not failure: the record was
// handled exactly as configured, and a receiver that answers 202 for admitted
// records should answer 202 here too (ADR-0009 — 202 means admitted, not
// delivered, and a record deliberately discarded by policy was still accepted).
//
// Errors are the ones a caller must act on: ErrRedactionFailed (dropped
// fail-closed), ErrSourceQuotaExhausted (this source is throttled),
// ErrBufferFull (capacity pressure), ErrBufferClosed (shutting down).
func (p *Pipeline) Admit(ctx context.Context, rec LogRecord, source string) error {
	// Step 3, enforced here as well as at the receiver. Defence in depth is
	// cheap for a field assignment, and it means the fair-share and filter
	// stages below can trust the identity without knowing which receiver the
	// record came from.
	if source != "" {
		if claimed := rec.Resource.ServiceName; claimed != "" && claimed != source {
			p.metrics.IdentityDiscrepancy(claimed, source)
		}
		rec.Resource.ServiceName = source
	}

	p.metrics.RecordsIngested(source, 1)

	// Step 4: normalize.
	p.normalizer.Normalize(&rec, source)

	// Step 5: record limits, then the cardinality guard. Size first, so the
	// guard never inspects a value the limits were about to shorten anyway.
	p.limits.Apply(&rec)
	if p.cardinality != nil {
		p.cardinality.Apply(&rec)
	}

	// Step 6: redact. Fail-closed — a record that cannot be masked is dropped,
	// never exported (ADR-0014).
	if p.redactor != nil {
		if err := p.redactor.Redact(&rec, source); err != nil {
			return err
		}
	}

	// Step 7: filter and sample, before the buffer, so a record that will
	// never be exported never costs buffer memory.
	if p.filter != nil && !p.filter.Keep(&rec, source) {
		return nil
	}

	// Step 8: admit.
	return p.buffer.Enqueue(ctx, rec)
}

// AdmitBatch runs every record through the pipeline, returning how many were
// enqueued and the first error encountered.
//
// It does not stop at the first failure. A batch is a transport detail: one
// record hitting a source quota says nothing about the next, and abandoning
// the rest would turn one throttled record into a whole request lost.
func (p *Pipeline) AdmitBatch(ctx context.Context, batch []LogRecord, source string) (admitted int, firstErr error) {
	for i := range batch {
		switch err := p.Admit(ctx, batch[i], source); {
		case err == nil:
			admitted++
		case firstErr == nil:
			firstErr = err
		}
		if ctx.Err() != nil {
			// Shutdown or a cancelled request: continuing would spend work on
			// records nobody is waiting for.
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			break
		}
	}
	return admitted, firstErr
}

// Buffer exposes the underlying store, so a consumer can dequeue and a
// shutdown can drain (ADR-0015).
func (p *Pipeline) Buffer() BufferStore { return p.buffer }
