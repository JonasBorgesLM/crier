package core

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The acceptance criterion for #13: every discard path increments exactly one
// counter, and no drop reason exists without a path that produces it.
//
// Asserting that path by path is what this project did first, and it is not
// the same claim. Three reasons were declared with no producer at all; two of
// them gained one a milestone later, and nothing noticed that the new
// producers were unverified. The invariant existed precisely to catch that,
// and the per-path tests could not, because a test that is never written
// cannot fail.
//
// So the set of reasons is derived from the source rather than typed out here:
// declaring a new DropReason fails this test until someone either covers it or
// records why it is not covered yet.

// dropScenarios drives one real discard path per reason. Add a reason to
// metrics.go and this map is what tells you it is unaccounted for.
func dropScenarios() map[DropReason]func(*testing.T, *dropRecorder) {
	return map[DropReason]func(*testing.T, *dropRecorder){
		DropRedactionFailed: func(t *testing.T, m *dropRecorder) {
			redactor := mustRedactor(t, RedactionConfig{Metrics: m})
			redactor.failHook = func() error { return errors.New("rule engine unavailable") }
			p, err := NewPipeline(PipelineConfig{
				Buffer: &spyBuffer{}, Redactor: redactor, Metrics: m,
			})
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			if err := p.Admit(context.Background(), LogRecord{Body: "token=hunter2"}, "task-api"); err == nil {
				t.Fatal("Admit succeeded despite redaction failing — that is fail-open")
			}
		},

		DropSourceQuota: func(t *testing.T, m *dropRecorder) {
			f := newFairShare(t, 4, FairShareConfig{
				Reservations: map[string]int{"noisy": 2, "quiet": 2},
				Metrics:      m,
			})
			ctx := context.Background()
			for i := range 2 {
				if err := f.Enqueue(ctx, recFrom("noisy", strconv.Itoa(i))); err != nil {
					t.Fatalf("Enqueue %d: %v", i, err)
				}
			}
			if err := f.Enqueue(ctx, recFrom("noisy", "over")); !errors.Is(err, ErrSourceQuotaExhausted) {
				t.Fatalf("error = %v, want ErrSourceQuotaExhausted", err)
			}
		},

		DropBufferFull: func(t *testing.T, m *dropRecorder) {
			b := newBuffer(t, MemoryBufferConfig{Capacity: 1, BatchSize: 1, Policy: DropPolicyReject, Metrics: m})
			ctx := context.Background()
			if err := b.Enqueue(ctx, recFrom("task-api", "first")); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := b.Enqueue(ctx, recFrom("task-api", "second")); !errors.Is(err, ErrBufferFull) {
				t.Fatalf("error = %v, want ErrBufferFull", err)
			}
		},

		DropOldest: func(t *testing.T, m *dropRecorder) {
			b := newBuffer(t, MemoryBufferConfig{Capacity: 1, BatchSize: 1, Policy: DropPolicyDropOldest, Metrics: m})
			ctx := context.Background()
			if err := b.Enqueue(ctx, recFrom("task-api", "first")); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := b.Enqueue(ctx, recFrom("task-api", "second")); err != nil {
				t.Fatalf("Enqueue evicting the oldest: %v", err)
			}
		},

		DropBackendUnavailable: func(t *testing.T, m *dropRecorder) {
			e := &fakeExporter{export: func(context.Context, []LogRecord) error {
				return errors.New("connection refused")
			}}
			buf := newBuffer(t, MemoryBufferConfig{
				Capacity: 16, BatchSize: 2, BatchWindow: 5 * time.Millisecond, Metrics: m,
			})
			d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: e, Workers: 1, Metrics: m})
			d.Start(context.Background())
			for i := range 2 {
				if err := buf.Enqueue(context.Background(), recFrom("task-api", strconv.Itoa(i))); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			}
			if err := d.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		},

		DropShutdownTimeout: func(t *testing.T, m *dropRecorder) {
			// The in-flight batch blocks and never returns during the test, so
			// it never reports a failure of its own. What is counted is what
			// stayed in the buffer when the drain deadline passed.
			block := make(chan struct{})
			t.Cleanup(func() { close(block) })
			e := &fakeExporter{export: func(context.Context, []LogRecord) error {
				<-block
				return nil
			}}
			buf := newBuffer(t, MemoryBufferConfig{
				Capacity: 16, BatchSize: 1, BatchWindow: 5 * time.Millisecond, Metrics: m,
			})
			d := mustDispatcher(t, DispatcherConfig{Buffer: buf, Exporter: e, Workers: 1, Metrics: m})
			d.Start(context.Background())
			for i := range 6 {
				if err := buf.Enqueue(context.Background(), recFrom("task-api", strconv.Itoa(i))); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			}
			waitFor(t, "the first batch to be in flight", func() bool { return e.calls.Load() > 0 })

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := d.Shutdown(ctx); err == nil {
				t.Fatal("Shutdown succeeded, want the expired drain reported")
			}
		},
	}
}

// notYetProduced are reasons declared ahead of the code that will produce
// them. Each needs a reason and an owner, because "we will get to it" is how
// DropBackendUnavailable sat unproduced for a milestone.
var notYetProduced = map[DropReason]string{
	DropInvalid: "produced by the receiver when a record fails validation (M3, #21/#22); " +
		"nothing in core parses a wire format yet",
}

// TestEveryDropReasonIsAccountedFor is the exhaustiveness half: the set comes
// from the source, so a new reason cannot be added without a decision here.
func TestEveryDropReasonIsAccountedFor(t *testing.T) {
	declared := declaredDropReasons(t)
	if len(declared) == 0 {
		t.Fatal("found no DropReason constants; the source scan is broken, not the code")
	}

	scenarios := dropScenarios()
	for _, reason := range declared {
		_, covered := scenarios[reason]
		why, pending := notYetProduced[reason]

		switch {
		case covered && pending:
			t.Errorf("%q is both exercised and listed as not yet produced; remove it from notYetProduced", reason)
		case covered, pending:
			if pending {
				t.Logf("%q has no producer yet: %s", reason, why)
			}
		default:
			t.Errorf("DropReason %q has no scenario in dropScenarios and is not listed in notYetProduced.\n"+
				"Every discard path must increment exactly one counter (ADR-0005, ADR-0015). Either add the "+
				"path that produces it and a scenario that proves the count, or record why it cannot exist yet.", reason)
		}
	}

	// The reverse direction: a scenario for a reason nobody declares any more
	// is dead weight that will quietly stop testing anything.
	declaredSet := make(map[DropReason]struct{}, len(declared))
	for _, r := range declared {
		declaredSet[r] = struct{}{}
	}
	for reason := range scenarios {
		if _, ok := declaredSet[reason]; !ok {
			t.Errorf("dropScenarios covers %q, which is no longer a declared DropReason", reason)
		}
	}
	for reason := range notYetProduced {
		if _, ok := declaredSet[reason]; !ok {
			t.Errorf("notYetProduced lists %q, which is no longer a declared DropReason", reason)
		}
	}
}

// TestEveryDropPathIncrementsExactlyOneCounter is the accounting half. Exactly
// one: a path that counts twice inflates loss and sends an operator looking
// for records that were never lost, which erodes the counter as fast as
// silence does.
func TestEveryDropPathIncrementsExactlyOneCounter(t *testing.T) {
	for reason, scenario := range dropScenarios() {
		t.Run(string(reason), func(t *testing.T) {
			var m dropRecorder
			scenario(t, &m)

			drops := m.dropped()
			if len(drops) != 1 {
				t.Fatalf("the path recorded %d drop counters, want exactly 1: %+v", len(drops), drops)
			}
			if drops[0].Reason != reason {
				t.Errorf("counted under %q, want %q", drops[0].Reason, reason)
			}
			if drops[0].N <= 0 {
				t.Errorf("counted %d records, want a positive number", drops[0].N)
			}
			// A discarded record is not a filtered one. Folding them together
			// makes a correct pipeline look lossy (ADR-0010).
			if got := m.filteredCalls(); got != 0 {
				t.Errorf("the path also counted %d records as filtered", got)
			}
		})
	}
}

// declaredDropReasons reads the DropReason constants out of the package source.
//
// Deriving them rather than listing them is the point: a list in a test is
// exactly as complete as whoever last edited it remembered to make it.
//
// It sees this package only. A reason produced by the receiver or an exporter
// module is covered by that module's own suite; what is enforced here is that
// core declares nothing it leaves unaccounted for.
func declaredDropReasons(t *testing.T) []DropReason {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var reasons []DropReason
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		reasons = append(reasons, dropReasonsIn(file)...)
	}
	return reasons
}

func dropReasonsIn(file *ast.File) []DropReason {
	var reasons []DropReason
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "DropReason" {
				continue
			}
			for _, expr := range value.Values {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				reasons = append(reasons, DropReason(unquoted))
			}
		}
	}
	return reasons
}

// dropRecorder records every drop and filter call, so "exactly one" is a
// statement about the whole path rather than about one counter read at the end.
type dropRecorder struct {
	NopMetrics

	mu       sync.Mutex
	drops    []droppedCall
	filtered int
}

type droppedCall struct {
	Source string
	Reason DropReason
	N      int
}

// RecordsDropped implements Metrics.
func (r *dropRecorder) RecordsDropped(source string, reason DropReason, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops = append(r.drops, droppedCall{Source: source, Reason: reason, N: n})
}

// RecordsFiltered implements Metrics.
func (r *dropRecorder) RecordsFiltered(_ string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filtered += n
}

func (r *dropRecorder) dropped() []droppedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]droppedCall(nil), r.drops...)
}

func (r *dropRecorder) filteredCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filtered
}
