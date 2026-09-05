package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/JonasBorgesLM/crier/core"
)

// writePrometheusMetrics renders a Snapshot in the Prometheus text exposition
// format (NFR5; ADR-0005 anticipates "the standalone binary can back it with
// Prometheus"). Hand-rolled rather than importing a client library: the
// format itself is a handful of stable text lines, and the actual work here —
// turning this specific internal snapshot into metric families — is bespoke
// either way, so a library would add a dependency tree to cmd/crierd without
// removing any real risk. core.CountingMetrics.Snapshot already collects
// everything; this only renders it.
//
// Label-value bounding is not repeated here. CountingMetrics applies its own
// cap (OverflowLabel past DefaultMetricLabelCap distinct values) at
// collection time, so a Snapshot already carries the bounded label set — this
// function renders whatever it is given, and a scrape sees the overflow
// bucket as the literal label "<other>" rather than an unbounded number of
// real ones.
func writePrometheusMetrics(w io.Writer, snap core.Snapshot) {
	counterMap(w, "crier_records_ingested_total", "Records accepted, per source.", "source", snap.Ingested)
	dropCounter(w, snap.Dropped)
	counterMap(w, "crier_records_filtered_total", "Records filtered before export, per source.", "source", snap.Filtered)
	counterMap(w, "crier_records_exported_total", "Records exported, per destination.", "exporter", snap.Exported)
	counterMap(w, "crier_export_retries_total", "Export attempts retried, per destination.", "exporter", snap.Retries)

	gaugeBoolMap(w, "crier_circuit_open", "Whether a destination's circuit breaker is open (1) or closed (0).", "exporter", snap.OpenCircuits)
	gaugeBool(w, "crier_degraded", "Whether the export path is degraded (1) or healthy (0).", snap.Degraded)
	counterScalar(w, "crier_degraded_transitions_total", "Number of times the degraded state changed.", snap.DegradedTransitions)

	latencyMap(w, "crier_export_latency", "Export call latency, per destination.", "exporter", snap.ExportLatency)

	gaugeScalarInt(w, "crier_buffer_depth", "Records currently held in the buffer.", snap.BufferDepth)

	counterMap(w, "crier_attributes_truncated_total", "Attribute values truncated, per key.", "key", snap.AttributesTruncated)
	counterMap(w, "crier_attributes_dropped_total", "Attributes dropped, per key.", "key", snap.AttributesDropped)
	counterMap(w, "crier_cardinality_capped_total", "Distinct attribute values capped, per key.", "key", snap.CardinalityCapped)
	counterMap(w, "crier_identity_discrepancies_total", "Client-asserted identity overwritten, per authenticated source.", "source", snap.IdentityDiscrepancies)
	counterMap(w, "crier_timestamp_missing_total", "Records with no source timestamp, per source.", "source", snap.TimestampMissing)
	latencyMap(w, "crier_clock_skew", "Deviation between source and observed timestamp, per source.", "source", snap.ClockSkew)
	counterMap(w, "crier_deprecated_wire_version_total", "Records received on a deprecated wire version.", "version", snap.DeprecatedWireVersion)
}

func meta(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help) //nolint:errcheck // an unwritable ResponseWriter has nothing left to report to
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)  //nolint:errcheck // same as above
}

func sample(w io.Writer, name string, labels [][2]string, value string) {
	if len(labels) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, value) //nolint:errcheck // same as meta
		return
	}
	parts := make([]string, len(labels))
	for i, kv := range labels {
		parts[i] = kv[0] + `="` + escapeLabelValue(kv[1]) + `"`
	}
	fmt.Fprintf(w, "%s{%s} %s\n", name, strings.Join(parts, ","), value) //nolint:errcheck // same as meta
}

// escapeLabelValue escapes the three characters the exposition format
// requires escaped in a label value. Order matters: the backslash escape must
// run first, or it would double-escape the backslashes the other two
// replacements introduce.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
func formatInt(n int64) string     { return strconv.FormatInt(n, 10) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func counterMap(w io.Writer, name, help, label string, values map[string]int64) {
	meta(w, name, help, "counter")
	for _, k := range sortedKeys(values) {
		sample(w, name, [][2]string{{label, k}}, formatInt(values[k]))
	}
}

func counterScalar(w io.Writer, name, help string, v int64) {
	meta(w, name, help, "counter")
	sample(w, name, nil, formatInt(v))
}

func gaugeScalarInt(w io.Writer, name, help string, v int) {
	meta(w, name, help, "gauge")
	sample(w, name, nil, strconv.Itoa(v))
}

func gaugeBool(w io.Writer, name, help string, v bool) {
	meta(w, name, help, "gauge")
	sample(w, name, nil, boolValue(v))
}

func gaugeBoolMap(w io.Writer, name, help, label string, values map[string]bool) {
	meta(w, name, help, "gauge")
	for _, k := range sortedKeys(values) {
		sample(w, name, [][2]string{{label, k}}, boolValue(values[k]))
	}
}

func boolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// dropCounter has a two-part key (core.DropKey), so it cannot share
// counterMap's single-label-string shape.
func dropCounter(w io.Writer, values map[core.DropKey]int64) {
	const name = "crier_records_dropped_total"
	meta(w, name, "Records dropped, per source and reason.", "counter")
	keys := make([]core.DropKey, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Source != keys[j].Source {
			return keys[i].Source < keys[j].Source
		}
		return keys[i].Reason < keys[j].Reason
	})
	for _, k := range keys {
		sample(w, name, [][2]string{{"source", k.Source}, {"reason", string(k.Reason)}}, formatInt(values[k]))
	}
}

// latencyMap renders a LatencyStat map as three series — _sum and _count
// (both counters, matching how a Prometheus summary/histogram exposes them)
// plus _max, which is not a standard summary quantile but is what
// core.LatencyStat actually tracks instead of one, and is worth exposing
// rather than discarding.
func latencyMap(w io.Writer, base, help, label string, values map[string]core.LatencyStat) {
	sumName, countName, maxName := base+"_seconds_sum", base+"_seconds_count", base+"_seconds_max"

	meta(w, sumName, help+" Sum of observed durations, in seconds.", "counter")
	for _, k := range sortedKeys(values) {
		sample(w, sumName, [][2]string{{label, k}}, formatFloat(values[k].Total.Seconds()))
	}
	meta(w, countName, help+" Number of observations.", "counter")
	for _, k := range sortedKeys(values) {
		sample(w, countName, [][2]string{{label, k}}, formatInt(values[k].Count))
	}
	meta(w, maxName, help+" Largest observed duration, in seconds.", "gauge")
	for _, k := range sortedKeys(values) {
		sample(w, maxName, [][2]string{{label, k}}, formatFloat(values[k].Max.Seconds()))
	}
}
