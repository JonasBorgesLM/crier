# Benchmarks

Hot-path measurements for the `core` pipeline, taken with every stage enabled
— limits, cardinality guard, redaction, filtering, and admission — because
that is how the pipeline actually runs. ADR-0010 requires this explicitly:
benchmarking the bare path publishes a number nobody experiences.

Reproduce with:

```bash
cd core && go test -run '^$' -bench . -benchmem -benchtime 3s
```

## Results

```
goos: darwin
goarch: arm64
pkg: github.com/JonasBorgesLM/crier/core
cpu: Apple M1

BenchmarkPipelineAllStages-8              294274     12180 ns/op    1132 B/op    10 allocs/op
BenchmarkPipelineAllStagesCleanBody-8    1715794      2095 ns/op     728 B/op     8 allocs/op
BenchmarkPipelineWithoutBodyRedaction-8  1860075      1931 ns/op     728 B/op     8 allocs/op
BenchmarkPipelineWithoutRedaction-8      3774022       967 ns/op     728 B/op     8 allocs/op
BenchmarkPipelineBare-8                  7455931       487 ns/op     712 B/op     7 allocs/op
BenchmarkRedactBodyOnly-8                 350563     10111 ns/op     400 B/op     3 allocs/op
BenchmarkRedactBodyNoMatch-8            96753494      37.5 ns/op       0 B/op     0 allocs/op
BenchmarkLimitsApply-8                   8350000       432 ns/op     712 B/op     7 allocs/op
BenchmarkCardinalityGuard-8              5187520       698 ns/op     728 B/op     8 allocs/op
BenchmarkFairShareAdmission-8           35070855       104 ns/op     279 B/op     1 allocs/op
BenchmarkPipelineAllStagesParallel-8     1000000      3689 ns/op    1162 B/op    10 allocs/op
```

## Reading them

**Two numbers, not one.** A record whose message contains a credential costs
**12.2 µs**; one that does not costs **2.1 µs**. Real traffic is overwhelmingly
the second, but the first is what a burst of authentication failures looks
like, and publishing only the flattering number would misrepresent the
pipeline. Single-core throughput is therefore roughly 82k records/second in the
common case and 82k→8k under a body full of credentials; contended across 8
cores it measures 3.7 µs/record.

**Redaction is the cost, as ADR-0014 predicted.** Everything else in the
pipeline together is under 1 µs. Body scanning against a matching message is
10.1 µs of the 12.2 µs total. That is the price of a control that catches
secrets in free text, and it is the concrete reason the README recommends
structured attributes: attribute-level redaction is both reliable and cheap,
body scanning is neither.

**The cardinality guard and fair-share admission are effectively free** at
0.7 µs and 0.1 µs. Neither is a reason to disable a protection.

## What was fixed, and what it cost before

The first run of this benchmark measured **32.7 µs/op**, and two thirds of that
was not where it was assumed to be.

**Attribute-key matching: ~16 µs.** Eleven regexes were evaluated per attribute
per record — over a hundred regex evaluations to conclude that an ordinary
record held nothing sensitive. Replacing them with case-insensitive substring
matching (`SensitiveKeySubstrings`) took the no-body path from 17.2 µs to
1.9 µs. Regex key rules remain available for what a substring cannot express;
they simply are not the default any more.

**The body prefilter, first attempt: slower than the work it skipped.** Adding
a cheap "could any rule match at all?" check as a single case-insensitive regex
alternation made things *worse* — `RedactBodyNoMatch` went from 4.1 µs to
5.1 µs. Wrapping a literal in `(?i)` expands it into character classes, which
defeats the literal-prefix optimisation that makes RE2 fast on plain strings.
Replacing it with a hand-rolled byte scan over a 256-entry first-byte table
brought the same check to **37 ns**, a 135× improvement on the version that was
supposed to be the optimisation.

Both are recorded here rather than quietly fixed, following the precedent set
in `moat`, where a hot-path regression was published instead of hidden. The
first number was wrong in a way that only a benchmark with every stage enabled
would have shown.

## Still on the table

Body redaction runs five regex passes over a matching message. Combining them
into one alternation with per-pattern capture groups would make it a single
pass, at the cost of changing overlap semantics between rules. Not done yet:
the common case is already 37 ns, so this only improves the worst case, and it
is not worth a subtle change to what gets masked without a test corpus large
enough to prove the semantics held.
