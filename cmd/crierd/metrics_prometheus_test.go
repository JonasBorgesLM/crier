package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JonasBorgesLM/crier/core"
)

func TestWritePrometheusMetrics(t *testing.T) {
	tests := []struct {
		name string
		snap core.Snapshot
		want []string
	}{
		{
			name: "counter with one label",
			snap: core.Snapshot{Ingested: map[string]int64{"task-api": 3}},
			want: []string{
				`# HELP crier_records_ingested_total Records accepted, per source.`,
				`# TYPE crier_records_ingested_total counter`,
				`crier_records_ingested_total{source="task-api"} 3`,
			},
		},
		{
			name: "dropped counter carries both source and reason labels",
			snap: core.Snapshot{Dropped: map[core.DropKey]int64{
				{Source: "task-api", Reason: core.DropBufferFull}: 5,
			}},
			want: []string{
				`crier_records_dropped_total{source="task-api",reason="buffer_full"} 5`,
			},
		},
		{
			name: "multiple label values are sorted for deterministic output",
			snap: core.Snapshot{Ingested: map[string]int64{"zeta": 1, "alpha": 2}},
			want: []string{
				"crier_records_ingested_total{source=\"alpha\"} 2\ncrier_records_ingested_total{source=\"zeta\"} 1\n",
			},
		},
		{
			name: "scalar gauge has no labels",
			snap: core.Snapshot{Degraded: true},
			want: []string{
				`# TYPE crier_degraded gauge`,
				"crier_degraded 1\n",
			},
		},
		{
			name: "circuit state renders as 0/1, not true/false",
			snap: core.Snapshot{OpenCircuits: map[string]bool{"primary": true, "secondary": false}},
			want: []string{
				`crier_circuit_open{exporter="primary"} 1`,
				`crier_circuit_open{exporter="secondary"} 0`,
			},
		},
		{
			name: "buffer depth is a scalar int gauge",
			snap: core.Snapshot{BufferDepth: 42},
			want: []string{
				`# TYPE crier_buffer_depth gauge`,
				"crier_buffer_depth 42\n",
			},
		},
		{
			name: "latency stat renders sum, count, and max as separate series",
			snap: core.Snapshot{ExportLatency: map[string]core.LatencyStat{
				"primary": {Count: 4, Total: 2 * time.Second, Max: 900 * time.Millisecond},
			}},
			want: []string{
				`# TYPE crier_export_latency_seconds_sum counter`,
				`crier_export_latency_seconds_sum{exporter="primary"} 2`,
				`# TYPE crier_export_latency_seconds_count counter`,
				`crier_export_latency_seconds_count{exporter="primary"} 4`,
				`# TYPE crier_export_latency_seconds_max gauge`,
				`crier_export_latency_seconds_max{exporter="primary"} 0.9`,
			},
		},
		{
			name: "the overflow label is rendered like any other value, not specially handled",
			snap: core.Snapshot{Ingested: map[string]int64{core.OverflowLabel: 7}},
			want: []string{
				`crier_records_ingested_total{source="<other>"} 7`,
			},
		},
		{
			name: "a label value needing escaping is escaped, not rejected",
			snap: core.Snapshot{Ingested: map[string]int64{`weird"source\with` + "\nbreak": 1}},
			want: []string{
				`crier_records_ingested_total{source="weird\"source\\with\nbreak"} 1`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			writePrometheusMetrics(&buf, tt.snap)
			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\nfull output:\n%s", want, out)
				}
			}
		})
	}
}

// TestWritePrometheusMetricsEmitsEveryMetricFamily guards against silently
// dropping a counter when Snapshot grows a field: every exported field name
// must correspond to at least one "# TYPE <name>" line, so a future Snapshot
// field with no renderer here is a build-time reminder away from being
// forgotten, not a silent gap discovered later the way A-6 itself was.
func TestWritePrometheusMetricsEmitsEveryMetricFamily(t *testing.T) {
	snap := core.Snapshot{
		Ingested:              map[string]int64{"s": 1},
		Dropped:               map[core.DropKey]int64{{Source: "s", Reason: core.DropInvalid}: 1},
		Filtered:              map[string]int64{"s": 1},
		Exported:              map[string]int64{"e": 1},
		Retries:               map[string]int64{"e": 1},
		OpenCircuits:          map[string]bool{"e": true},
		Degraded:              true,
		DegradedTransitions:   1,
		ExportLatency:         map[string]core.LatencyStat{"e": {Count: 1}},
		BufferDepth:           1,
		AttributesTruncated:   map[string]int64{"k": 1},
		AttributesDropped:     map[string]int64{"k": 1},
		CardinalityCapped:     map[string]int64{"k": 1},
		IdentityDiscrepancies: map[string]int64{"s": 1},
		TimestampMissing:      map[string]int64{"s": 1},
		ClockSkew:             map[string]core.LatencyStat{"s": {Count: 1}},
		DeprecatedWireVersion: map[string]int64{"v1": 1},
	}

	var buf strings.Builder
	writePrometheusMetrics(&buf, snap)
	out := buf.String()

	wantFamilies := []string{
		"crier_records_ingested_total",
		"crier_records_dropped_total",
		"crier_records_filtered_total",
		"crier_records_exported_total",
		"crier_export_retries_total",
		"crier_circuit_open",
		"crier_degraded",
		"crier_degraded_transitions_total",
		"crier_export_latency_seconds_sum",
		"crier_export_latency_seconds_count",
		"crier_export_latency_seconds_max",
		"crier_buffer_depth",
		"crier_attributes_truncated_total",
		"crier_attributes_dropped_total",
		"crier_cardinality_capped_total",
		"crier_identity_discrepancies_total",
		"crier_timestamp_missing_total",
		"crier_clock_skew_seconds_sum",
		"crier_clock_skew_seconds_count",
		"crier_clock_skew_seconds_max",
		"crier_deprecated_wire_version_total",
	}
	for _, name := range wantFamilies {
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("no metric family emitted for %s\nfull output:\n%s", name, out)
		}
	}
}

func TestEscapeLabelValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain value is untouched", "task-api", "task-api"},
		{"backslash is escaped", `a\b`, `a\\b`},
		{"quote is escaped", `a"b`, `a\"b`},
		{"newline is escaped", "a\nb", `a\nb`},
		{"backslash escaped before newline/quote so it does not double-escape them", "\\\n\"", `\\\n\"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeLabelValue(tt.input); got != tt.want {
				t.Errorf("escapeLabelValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
