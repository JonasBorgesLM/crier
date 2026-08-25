package core

import (
	"sync"
	"time"
)

// DefaultMetricLabelCap bounds how many distinct label values CountingMetrics
// will track for any one metric before folding the rest into OverflowLabel.
const DefaultMetricLabelCap = 512

// OverflowLabel replaces a label value once a metric has reached its label
// cap. Its presence in a snapshot means the real value was discarded, not that
// a source is literally named this.
const OverflowLabel = "<other>"

// DropKey identifies one (source, reason) drop bucket.
type DropKey struct {
	Source string
	Reason DropReason
}

// LatencyStat summarises observed durations without retaining every sample.
type LatencyStat struct {
	Count int64
	Total time.Duration
	Max   time.Duration
}

// Mean returns the average observed duration, or zero if nothing was observed.
func (s LatencyStat) Mean() time.Duration {
	if s.Count == 0 {
		return 0
	}
	return s.Total / time.Duration(s.Count)
}

// Snapshot is a point-in-time copy of CountingMetrics. Maps are copies, so a
// caller may read them while the pipeline keeps running.
type Snapshot struct {
	Ingested              map[string]int64
	Dropped               map[DropKey]int64
	Filtered              map[string]int64
	Exported              map[string]int64
	Retries               map[string]int64
	OpenCircuits          map[string]bool
	ExportLatency         map[string]LatencyStat
	BufferDepth           int
	AttributesTruncated   map[string]int64
	CardinalityCapped     map[string]int64
	IdentityDiscrepancies map[string]int64
	ClockSkew             map[string]LatencyStat
	DeprecatedWireVersion map[string]int64
}

// TotalDropped sums every drop bucket.
func (s Snapshot) TotalDropped() int64 {
	var n int64
	for _, v := range s.Dropped {
		n += v
	}
	return n
}

// DroppedBy returns the count for one reason across all sources.
func (s Snapshot) DroppedBy(reason DropReason) int64 {
	var n int64
	for k, v := range s.Dropped {
		if k.Reason == reason {
			n += v
		}
	}
	return n
}

// CountingMetrics is an in-memory Metrics implementation, intended for tests
// and as the source for the standalone binary's metrics endpoint.
//
// Label values are bounded (LabelCap): a metrics implementation that grows a
// map key per distinct client-supplied string is the same unbounded-cardinality
// leak the pipeline's own guard exists to prevent (ADR-0010), and
// IdentityDiscrepancy in particular takes a value straight from an untrusted
// caller. Past the cap, values collapse into OverflowLabel.
//
// Safe for concurrent use.
type CountingMetrics struct {
	// LabelCap bounds distinct label values per metric. Zero means
	// DefaultMetricLabelCap. Set before first use.
	LabelCap int

	mu       sync.Mutex
	ingested map[string]int64
	dropped  map[DropKey]int64
	// droppedSources mirrors the distinct sources present in dropped, so the
	// label cap can be applied without walking every bucket per record.
	droppedSources        map[string]struct{}
	filtered              map[string]int64
	exported              map[string]int64
	retries               map[string]int64
	openCircuits          map[string]bool
	exportLatency         map[string]LatencyStat
	bufferDepth           int
	attributesTruncated   map[string]int64
	cardinalityCapped     map[string]int64
	identityDiscrepancies map[string]int64
	clockSkew             map[string]LatencyStat
	deprecatedWireVersion map[string]int64
}

var _ Metrics = (*CountingMetrics)(nil)

func (m *CountingMetrics) cap() int {
	if m.LabelCap > 0 {
		return m.LabelCap
	}
	return DefaultMetricLabelCap
}

// label returns the key to record under, folding into OverflowLabel once the
// map is at capacity and the value has not been seen before.
func label[V any](m *CountingMetrics, dst map[string]V, v string) string {
	if _, seen := dst[v]; seen {
		return v
	}
	if len(dst) >= m.cap() {
		return OverflowLabel
	}
	return v
}

// init allocates the maps. Every caller already holds m.mu.
func (m *CountingMetrics) init() {
	if m.ingested != nil {
		return
	}
	m.ingested = make(map[string]int64)
	m.dropped = make(map[DropKey]int64)
	m.droppedSources = make(map[string]struct{})
	m.filtered = make(map[string]int64)
	m.exported = make(map[string]int64)
	m.retries = make(map[string]int64)
	m.openCircuits = make(map[string]bool)
	m.exportLatency = make(map[string]LatencyStat)
	m.attributesTruncated = make(map[string]int64)
	m.cardinalityCapped = make(map[string]int64)
	m.identityDiscrepancies = make(map[string]int64)
	m.clockSkew = make(map[string]LatencyStat)
	m.deprecatedWireVersion = make(map[string]int64)
}

func observe(dst map[string]LatencyStat, k string, d time.Duration) {
	s := dst[k]
	s.Count++
	s.Total += d
	if d > s.Max {
		s.Max = d
	}
	dst[k] = s
}

// RecordsIngested implements Metrics.
func (m *CountingMetrics) RecordsIngested(source string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.ingested[label(m, m.ingested, source)] += int64(n)
}

// RecordsDropped implements Metrics.
func (m *CountingMetrics) RecordsDropped(source string, reason DropReason, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	// Bound on source alone: the reason set is closed, so it cannot grow.
	src := label(m, m.droppedSources, source)
	m.droppedSources[src] = struct{}{}
	m.dropped[DropKey{Source: src, Reason: reason}] += int64(n)
}

// RecordsFiltered implements Metrics.
func (m *CountingMetrics) RecordsFiltered(source string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.filtered[label(m, m.filtered, source)] += int64(n)
}

// RecordsExported implements Metrics.
func (m *CountingMetrics) RecordsExported(exporter string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.exported[label(m, m.exported, exporter)] += int64(n)
}

// ExportLatency implements Metrics.
func (m *CountingMetrics) ExportLatency(exporter string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	observe(m.exportLatency, label(m, m.exportLatency, exporter), d)
}

// ExportRetried implements Metrics.
func (m *CountingMetrics) ExportRetried(exporter string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.retries[label(m, m.retries, exporter)]++
}

// CircuitStateChanged implements Metrics.
func (m *CountingMetrics) CircuitStateChanged(exporter string, open bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.openCircuits[label(m, m.openCircuits, exporter)] = open
}

// BufferDepth implements Metrics.
func (m *CountingMetrics) BufferDepth(depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bufferDepth = depth
}

// AttributeTruncated implements Metrics.
func (m *CountingMetrics) AttributeTruncated(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.attributesTruncated[label(m, m.attributesTruncated, key)]++
}

// CardinalityCapped implements Metrics.
func (m *CountingMetrics) CardinalityCapped(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.cardinalityCapped[label(m, m.cardinalityCapped, key)]++
}

// IdentityDiscrepancy implements Metrics.
func (m *CountingMetrics) IdentityDiscrepancy(_, actual string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Keyed by the authenticated principal, never by the claim: the claim is
	// attacker-controlled and would let one caller fill this map by itself.
	m.init()
	m.identityDiscrepancies[label(m, m.identityDiscrepancies, actual)]++
}

// ClockSkew implements Metrics.
func (m *CountingMetrics) ClockSkew(source string, deviation time.Duration) {
	if deviation < 0 {
		deviation = -deviation
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	observe(m.clockSkew, label(m, m.clockSkew, source), deviation)
}

// DeprecatedWireVersion implements Metrics.
func (m *CountingMetrics) DeprecatedWireVersion(version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.deprecatedWireVersion[label(m, m.deprecatedWireVersion, version)]++
}

// Snapshot returns a copy of the current counters.
func (m *CountingMetrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		Ingested:              cloneMap(m.ingested),
		Dropped:               cloneMap(m.dropped),
		Filtered:              cloneMap(m.filtered),
		Exported:              cloneMap(m.exported),
		Retries:               cloneMap(m.retries),
		OpenCircuits:          cloneMap(m.openCircuits),
		ExportLatency:         cloneMap(m.exportLatency),
		BufferDepth:           m.bufferDepth,
		AttributesTruncated:   cloneMap(m.attributesTruncated),
		CardinalityCapped:     cloneMap(m.cardinalityCapped),
		IdentityDiscrepancies: cloneMap(m.identityDiscrepancies),
		ClockSkew:             cloneMap(m.clockSkew),
		DeprecatedWireVersion: cloneMap(m.deprecatedWireVersion),
	}
}

func cloneMap[K comparable, V any](src map[K]V) map[K]V {
	if src == nil {
		return nil
	}
	dst := make(map[K]V, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
