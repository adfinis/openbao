package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func analyzeMode(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	outputDir := fs.String("output-dir", "./dr-stress-results", "Results root directory")
	jsonOutput := fs.Bool("json", false, "Output analysis as JSON instead of table")
	fs.Parse(args)

	paths := fs.Args()
	resultFiles := resolveResultFiles(paths, *outputDir)
	if len(resultFiles) == 0 {
		log.Fatal("no result files found")
	}

	for _, rf := range resultFiles {
		analyzeRun(rf, *jsonOutput)
	}
}

func resolveResultFiles(paths []string, outputDir string) []string {
	var results []string

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			rf := filepath.Join(p, "result.json")
			if _, err := os.Stat(rf); err == nil {
				results = append(results, rf)
			}
		} else {
			results = append(results, p)
		}
	}

	if len(results) > 0 || len(paths) > 0 {
		return results
	}

	// Auto-discover from output dir.
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "drmixed-") {
			continue
		}
		rf := filepath.Join(outputDir, e.Name(), "result.json")
		if _, err := os.Stat(rf); err == nil {
			results = append(results, rf)
		}
	}
	sort.Strings(results)
	return results
}

// AnalysisResult holds the full analysis output for one run.
type AnalysisResult struct {
	RunID             string                   `json:"run_id"`
	ResultFile        string                   `json:"result_file"`
	TestContext       map[string]string        `json:"test_context,omitempty"`
	OpsPerSec         float64                  `json:"ops_per_sec"`
	LatencyByOp       map[string]*LatencyStats `json:"latency_by_op"`
	ErrorHistogram    map[string]int64         `json:"error_histogram"`
	ThroughputBuckets []ThroughputBucket       `json:"throughput_buckets,omitempty"`
	LagSlope          *LagSlopeResult          `json:"lag_slope,omitempty"`
	StatusDelta       *StatusDeltaResult       `json:"status_delta,omitempty"`
	StepdownWindows   []StepdownWindowSummary  `json:"stepdown_windows,omitempty"`
	Sentinel          map[string]float64       `json:"sentinel_converge_seconds"`
}

// LatencyStats holds percentile latency for one op type.
type LatencyStats struct {
	Count int64   `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Min   float64 `json:"min_ms"`
	Max   float64 `json:"max_ms"`
	Mean  float64 `json:"mean_ms"`
}

// ThroughputBucket is a 1-second ops count bucket.
type ThroughputBucket struct {
	SecondOffset int   `json:"second_offset"`
	Count        int64 `json:"count"`
}

// LagSlopeResult holds linear regression on lag_entries over time.
type LagSlopeResult struct {
	Role      string  `json:"role"`
	Slope     float64 `json:"slope_entries_per_second"`
	Intercept float64 `json:"intercept"`
	Points    int     `json:"data_points"`
}

// StatusDeltaResult summarizes monotonic DR counter movement between the
// first and last status samples in a run.
type StatusDeltaResult struct {
	DurationSeconds float64          `json:"duration_seconds"`
	Primary         *RoleStatusDelta `json:"primary,omitempty"`
	Secondary1      *RoleStatusDelta `json:"secondary1,omitempty"`
	Secondary2      *RoleStatusDelta `json:"secondary2,omitempty"`
}

// RoleStatusDelta captures high-signal status deltas for one node role.
type RoleStatusDelta struct {
	PrimaryIndexDelta                    int64   `json:"primary_index_delta,omitempty"`
	LastAppliedIndexDelta                int64   `json:"last_applied_index_delta,omitempty"`
	StreamTxnEntriesDelta                int64   `json:"stream_txn_entries_delta,omitempty"`
	StreamTxnPhysicalEntriesDelta        int64   `json:"stream_txn_physical_entries_delta,omitempty"`
	StreamTxnPhysicalEntriesPerSecond    float64 `json:"stream_txn_physical_entries_per_second,omitempty"`
	StreamTxnBatchesDelta                int64   `json:"stream_txn_batches_delta,omitempty"`
	StreamTxnCoalescedEntriesDelta       int64   `json:"stream_txn_coalesced_entries_delta,omitempty"`
	ReconcileCountDelta                  int64   `json:"reconcile_count_delta,omitempty"`
	ReconcileRetriesDelta                int64   `json:"reconcile_retries_delta,omitempty"`
	FlatAccumulatorFastPathDelta         int64   `json:"flat_accumulator_fast_path_delta,omitempty"`
	FlatAccumulatorIndexedRepairDelta    int64   `json:"flat_accumulator_indexed_repair_delta,omitempty"`
	FlatAccumulatorIndexedRangesDelta    int64   `json:"flat_accumulator_indexed_repair_ranges_delta,omitempty"`
	FlatAccumulatorProofMismatchDelta    int64   `json:"flat_accumulator_proof_mismatch_delta,omitempty"`
	FlatAccumulatorFullBucketFallbacks   int64   `json:"flat_accumulator_full_bucket_fallback_delta,omitempty"`
	FlatAccumulatorFullBucketRangesDelta int64   `json:"flat_accumulator_full_bucket_ranges_delta,omitempty"`
	LocalKIDIndexFallbackScansDelta      int64   `json:"local_kid_index_fallback_scans_delta,omitempty"`
	CheckpointStorageDriftDelta          int64   `json:"checkpoint_storage_drift_delta,omitempty"`
}

// StepdownWindowSummary summarizes workload behavior around one requested
// primary stepdown.
type StepdownWindowSummary struct {
	TS      time.Time        `json:"ts"`
	Windows []StepdownWindow `json:"windows"`
}

// StepdownWindow captures operation and error rates in a relative time window.
type StepdownWindow struct {
	Name            string           `json:"name"`
	FromSeconds     float64          `json:"from_seconds"`
	ToSeconds       float64          `json:"to_seconds"`
	ObservedSeconds float64          `json:"observed_seconds"`
	Operations      int64            `json:"operations"`
	Errors          int64            `json:"errors"`
	ErrorRate       float64          `json:"error_rate"`
	OpsPerSecond    float64          `json:"ops_per_second"`
	OpCounts        map[string]int64 `json:"op_counts,omitempty"`
	ErrorCounts     map[string]int64 `json:"error_counts,omitempty"`
}

func analyzeRun(resultFile string, jsonOutput bool) {
	runDir := filepath.Dir(resultFile)

	// Read result.json for summary data.
	resultData, err := os.ReadFile(resultFile)
	if err != nil {
		log.Printf("skip %s: %v", resultFile, err)
		return
	}
	var result map[string]interface{}
	json.Unmarshal(resultData, &result)

	runID := ""
	if v, ok := result["run_id"].(string); ok {
		runID = v
	}

	analysis := AnalysisResult{
		RunID:          runID,
		ResultFile:     resultFile,
		TestContext:    extractTestContext(result),
		LatencyByOp:    make(map[string]*LatencyStats),
		ErrorHistogram: make(map[string]int64),
		Sentinel:       make(map[string]float64),
	}

	// Extract ops_per_sec from result.
	if wl, ok := result["workload"].(map[string]interface{}); ok {
		if v, ok := wl["operations_per_second"].(float64); ok {
			analysis.OpsPerSec = v
		}
	}

	// Extract sentinel data.
	if repl, ok := result["replication"].(map[string]interface{}); ok {
		for _, role := range []string{"sentinel_secondary1", "sentinel_secondary2"} {
			if s, ok := repl[role].(map[string]interface{}); ok {
				if v, ok := s["converge_seconds"].(float64); ok {
					analysis.Sentinel[role] = v
				}
			}
		}
	}

	// Parse events.ndjson for latency percentiles and error histograms.
	eventsFile := filepath.Join(runDir, "events.ndjson")
	latencies, errors, buckets, firstTS, events := parseEventsFile(eventsFile)

	for op, lats := range latencies {
		analysis.LatencyByOp[op] = computeLatencyStats(lats)
	}
	analysis.ErrorHistogram = errors
	_ = firstTS

	if len(buckets) > 0 {
		analysis.ThroughputBuckets = buckets
	}

	// Parse status_timeline.ndjson for lag slope.
	timelineFile := filepath.Join(runDir, "status_timeline.ndjson")
	analysis.LagSlope = computeLagSlope(timelineFile)
	analysis.StatusDelta = computeStatusDeltas(timelineFile)
	analysis.StepdownWindows = computeStepdownWindows(events, firstTS)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(analysis)
	} else {
		printAnalysisTable(analysis)
	}
}

func extractTestContext(result map[string]interface{}) map[string]string {
	raw, ok := result["test_context"].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseEventsFile(path string) (latencies map[string][]float64, errors map[string]int64, buckets []ThroughputBucket, firstTS time.Time, events []Event) {
	latencies = make(map[string][]float64)
	errors = make(map[string]int64)

	f, err := os.Open(path)
	if err != nil {
		log.Printf("cannot open events: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var firstSet bool
	bucketMap := make(map[int]int64) // second offset -> count

	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		events = append(events, ev)

		if ev.Control {
			continue
		}

		if !firstSet {
			firstTS = ev.TS
			firstSet = true
		}

		// Latency in milliseconds.
		latMS := float64(ev.LatencyNS) / 1e6
		latencies[ev.Op] = append(latencies[ev.Op], latMS)

		// Throughput buckets.
		secOffset := int(ev.TS.Sub(firstTS).Seconds())
		bucketMap[secOffset]++

		// Error histogram.
		if eventIsError(ev) {
			key := fmt.Sprintf("%s:%d:%s", ev.Op, ev.Code, eventErrorClass(ev))
			errors[key]++
		}
	}

	// Convert bucket map to sorted slice.
	if len(bucketMap) > 0 {
		maxSec := 0
		for s := range bucketMap {
			if s > maxSec {
				maxSec = s
			}
		}
		buckets = make([]ThroughputBucket, 0, maxSec+1)
		for s := 0; s <= maxSec; s++ {
			buckets = append(buckets, ThroughputBucket{
				SecondOffset: s,
				Count:        bucketMap[s],
			})
		}
	}

	return
}

func eventIsError(ev Event) bool {
	return ev.Err != "" || (ev.Code != 0 && ev.Code != 200 && ev.Code != 204 && ev.Code != 404)
}

func eventErrorClass(ev Event) string {
	s := ev.Err
	switch {
	case s == "":
		return fmt.Sprintf("http_%d", ev.Code)
	case strings.Contains(s, "connection refused"):
		return "conn_refused"
	case strings.Contains(s, "connection reset"):
		return "conn_reset"
	case strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "EOF"):
		return "eof"
	case strings.Contains(s, "TLS"):
		return "tls_error"
	case strings.Contains(s, "context canceled"):
		return "ctx_canceled"
	case strings.Contains(s, "context deadline exceeded"):
		return "ctx_deadline"
	case ev.Code == 0 && (strings.Contains(s, "http://") || strings.Contains(s, "https://")):
		return "transport_error"
	default:
		return s
	}
}

func computeLatencyStats(values []float64) *LatencyStats {
	if len(values) == 0 {
		return &LatencyStats{}
	}

	sort.Float64s(values)
	n := len(values)

	var sum float64
	for _, v := range values {
		sum += v
	}

	return &LatencyStats{
		Count: int64(n),
		P50:   percentile(values, 50),
		P95:   percentile(values, 95),
		P99:   percentile(values, 99),
		Min:   values[0],
		Max:   values[n-1],
		Mean:  sum / float64(n),
	}
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := float64(p) / 100.0 * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

func computeLagSlope(timelinePath string) *LagSlopeResult {
	f, err := os.Open(timelinePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	type point struct {
		t float64 // seconds from first sample
		v float64 // lag_entries
	}

	var points []point
	var firstTS time.Time
	var firstSet bool

	for scanner.Scan() {
		var snap StatusSnapshot
		if err := json.Unmarshal(scanner.Bytes(), &snap); err != nil {
			continue
		}

		if !firstSet {
			firstTS = snap.TS
			firstSet = true
		}

		// Use secondary1 lag if available.
		if snap.Secondary1 != nil && snap.Secondary1.LagEntries >= 0 {
			points = append(points, point{
				t: snap.TS.Sub(firstTS).Seconds(),
				v: float64(snap.Secondary1.LagEntries),
			})
		}
	}

	if len(points) < 2 {
		return nil
	}

	// Simple linear regression: y = slope*x + intercept
	var sumX, sumY, sumXY, sumX2 float64
	n := float64(len(points))
	for _, p := range points {
		sumX += p.t
		sumY += p.v
		sumXY += p.t * p.v
		sumX2 += p.t * p.t
	}

	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return nil
	}

	slope := (n*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / n

	return &LagSlopeResult{
		Role:      "secondary1",
		Slope:     slope,
		Intercept: intercept,
		Points:    len(points),
	}
}

func computeStatusDeltas(timelinePath string) *StatusDeltaResult {
	f, err := os.Open(timelinePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var first, last StatusSnapshot
	var firstSet bool

	for scanner.Scan() {
		var snap StatusSnapshot
		if err := json.Unmarshal(scanner.Bytes(), &snap); err != nil {
			continue
		}
		if !firstSet {
			first = snap
			firstSet = true
		}
		last = snap
	}
	if !firstSet {
		return nil
	}

	durationSeconds := last.TS.Sub(first.TS).Seconds()
	if durationSeconds < 0 {
		durationSeconds = 0
	}

	return &StatusDeltaResult{
		DurationSeconds: durationSeconds,
		Primary:         roleStatusDelta(first.Primary, last.Primary, durationSeconds),
		Secondary1:      roleStatusDelta(first.Secondary1, last.Secondary1, durationSeconds),
		Secondary2:      roleStatusDelta(first.Secondary2, last.Secondary2, durationSeconds),
	}
}

func roleStatusDelta(first, last *DRStatusResponse, durationSeconds float64) *RoleStatusDelta {
	if first == nil || last == nil {
		return nil
	}
	d := &RoleStatusDelta{
		PrimaryIndexDelta:                    last.PrimaryIndex - first.PrimaryIndex,
		LastAppliedIndexDelta:                last.LastAppliedIndex - first.LastAppliedIndex,
		StreamTxnEntriesDelta:                last.StreamTxnEntriesTotal - first.StreamTxnEntriesTotal,
		StreamTxnPhysicalEntriesDelta:        last.StreamTxnPhysicalTotal - first.StreamTxnPhysicalTotal,
		StreamTxnBatchesDelta:                last.StreamTxnBatchesTotal - first.StreamTxnBatchesTotal,
		StreamTxnCoalescedEntriesDelta:       last.StreamTxnCoalescedTotal - first.StreamTxnCoalescedTotal,
		ReconcileCountDelta:                  last.ReconcileCount - first.ReconcileCount,
		ReconcileRetriesDelta:                last.ReconcileRetriesTotal - first.ReconcileRetriesTotal,
		FlatAccumulatorFastPathDelta:         last.FlatAccFastPathTotal - first.FlatAccFastPathTotal,
		FlatAccumulatorIndexedRepairDelta:    last.FlatAccIndexedRepairTotal - first.FlatAccIndexedRepairTotal,
		FlatAccumulatorIndexedRangesDelta:    last.FlatAccIndexedRepairRanges - first.FlatAccIndexedRepairRanges,
		FlatAccumulatorProofMismatchDelta:    last.FlatAccIndexedProofMismatches - first.FlatAccIndexedProofMismatches,
		FlatAccumulatorFullBucketFallbacks:   last.FlatAccIndexedFullBucketTotal - first.FlatAccIndexedFullBucketTotal,
		FlatAccumulatorFullBucketRangesDelta: last.FlatAccIndexedFullBucketRanges - first.FlatAccIndexedFullBucketRanges,
		LocalKIDIndexFallbackScansDelta:      last.LocalKIDIndexFallbackScans - first.LocalKIDIndexFallbackScans,
		CheckpointStorageDriftDelta:          last.CheckpointStorageDrift - first.CheckpointStorageDrift,
	}
	if durationSeconds > 0 {
		d.StreamTxnPhysicalEntriesPerSecond = float64(d.StreamTxnPhysicalEntriesDelta) / durationSeconds
	}
	return d
}

func computeStepdownWindows(events []Event, firstTS time.Time) []StepdownWindowSummary {
	if len(events) == 0 {
		return nil
	}

	var stepdowns []Event
	var lastTS time.Time
	for _, ev := range events {
		if lastTS.IsZero() || ev.TS.After(lastTS) {
			lastTS = ev.TS
		}
		if ev.Control && ev.Op == "stepdown" {
			stepdowns = append(stepdowns, ev)
		}
	}
	if len(stepdowns) == 0 {
		return nil
	}

	out := make([]StepdownWindowSummary, 0, len(stepdowns))
	for i, sd := range stepdowns {
		nextBoundary := lastTS
		if i+1 < len(stepdowns) && stepdowns[i+1].TS.Before(nextBoundary) {
			nextBoundary = stepdowns[i+1].TS
		}

		specs := []struct {
			name string
			from time.Duration
			to   time.Duration
		}{
			{name: "pre_30s_to_t", from: -30 * time.Second, to: 0},
			{name: "t_to_plus_10s", from: 0, to: 10 * time.Second},
			{name: "plus_10s_to_plus_60s", from: 10 * time.Second, to: 60 * time.Second},
			{name: "plus_60s_to_next_stepdown_or_end", from: 60 * time.Second, to: nextBoundary.Sub(sd.TS)},
		}

		summary := StepdownWindowSummary{TS: sd.TS}
		for _, spec := range specs {
			if spec.to <= spec.from {
				continue
			}
			from := sd.TS.Add(spec.from)
			to := sd.TS.Add(spec.to)
			if spec.name == "plus_60s_to_next_stepdown_or_end" {
				to = nextBoundary
			}
			if !firstTS.IsZero() && from.Before(firstTS) {
				from = firstTS
			}
			if to.After(nextBoundary) {
				to = nextBoundary
			}
			if !to.After(from) {
				continue
			}
			summary.Windows = append(summary.Windows, summarizeStepdownWindow(events, sd.TS, spec.name, from, to))
		}
		out = append(out, summary)
	}
	return out
}

func summarizeStepdownWindow(events []Event, stepdownTS time.Time, name string, from, to time.Time) StepdownWindow {
	w := StepdownWindow{
		Name:        name,
		FromSeconds: from.Sub(stepdownTS).Seconds(),
		ToSeconds:   to.Sub(stepdownTS).Seconds(),
		OpCounts:    make(map[string]int64),
		ErrorCounts: make(map[string]int64),
	}
	w.ObservedSeconds = to.Sub(from).Seconds()

	for _, ev := range events {
		if ev.Control || ev.TS.Before(from) || !ev.TS.Before(to) {
			continue
		}
		w.Operations++
		w.OpCounts[ev.Op]++
		if eventIsError(ev) {
			w.Errors++
			w.ErrorCounts[ev.Op]++
		}
	}
	if w.ObservedSeconds > 0 {
		w.OpsPerSecond = float64(w.Operations) / w.ObservedSeconds
	}
	if w.Operations > 0 {
		w.ErrorRate = float64(w.Errors) / float64(w.Operations)
	}
	if len(w.OpCounts) == 0 {
		w.OpCounts = nil
	}
	if len(w.ErrorCounts) == 0 {
		w.ErrorCounts = nil
	}
	return w
}

func printAnalysisTable(a AnalysisResult) {
	fmt.Printf("\n=== Analysis: %s ===\n", a.RunID)
	fmt.Printf("Result: %s\n", a.ResultFile)
	if len(a.TestContext) > 0 {
		fmt.Printf("Context: %s\n", formatStringMap(a.TestContext))
	}
	fmt.Printf("Throughput: %.2f ops/s\n\n", a.OpsPerSec)

	// Latency table.
	fmt.Printf("%-15s %8s %8s %8s %8s %8s %8s %8s\n",
		"Operation", "Count", "Min(ms)", "P50(ms)", "P95(ms)", "P99(ms)", "Max(ms)", "Mean(ms)")
	fmt.Printf("%-15s %8s %8s %8s %8s %8s %8s %8s\n",
		"---------------", "--------", "--------", "--------", "--------", "--------", "--------", "--------")

	ops := make([]string, 0, len(a.LatencyByOp))
	for op := range a.LatencyByOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)

	for _, op := range ops {
		s := a.LatencyByOp[op]
		fmt.Printf("%-15s %8d %8.2f %8.2f %8.2f %8.2f %8.2f %8.2f\n",
			op, s.Count, s.Min, s.P50, s.P95, s.P99, s.Max, s.Mean)
	}

	// Error histogram.
	if len(a.ErrorHistogram) > 0 {
		fmt.Printf("\nErrors:\n")
		errKeys := make([]string, 0, len(a.ErrorHistogram))
		for k := range a.ErrorHistogram {
			errKeys = append(errKeys, k)
		}
		sort.Strings(errKeys)
		for _, k := range errKeys {
			fmt.Printf("  %-60s %d\n", k, a.ErrorHistogram[k])
		}
	}

	// Lag slope.
	if a.LagSlope != nil {
		fmt.Printf("\nLag slope (%s): %.4f entries/s (intercept=%.1f, points=%d)\n",
			a.LagSlope.Role, a.LagSlope.Slope, a.LagSlope.Intercept, a.LagSlope.Points)
	}

	if a.StatusDelta != nil {
		printStatusDelta(a.StatusDelta)
	}

	if len(a.StepdownWindows) > 0 {
		fmt.Printf("\nStepdown windows:\n")
		for _, sd := range a.StepdownWindows {
			fmt.Printf("  %s\n", sd.TS.Format(time.RFC3339))
			for _, w := range sd.Windows {
				fmt.Printf("    %-32s ops=%d rate=%.1f/s errors=%d error_rate=%.2f%%\n",
					w.Name, w.Operations, w.OpsPerSecond, w.Errors, w.ErrorRate*100)
			}
		}
	}

	// Sentinel.
	if len(a.Sentinel) > 0 {
		fmt.Printf("\nSentinel convergence:\n")
		for k, v := range a.Sentinel {
			fmt.Printf("  %s: %.1fs\n", k, v)
		}
	}

	fmt.Println()
}

func printStatusDelta(d *StatusDeltaResult) {
	fmt.Printf("\nDR status deltas (%.0fs):\n", d.DurationSeconds)
	printRoleStatusDelta := func(name string, rd *RoleStatusDelta) {
		if rd == nil {
			return
		}
		fmt.Printf(
			"  %-10s applied=%d primary_index=%d physical=%d (%.1f/s) batches=%d reconcile=%d indexed_repair=%d full_bucket=%d proof_mismatch=%d kid_scan=%d drift=%d\n",
			name,
			rd.LastAppliedIndexDelta,
			rd.PrimaryIndexDelta,
			rd.StreamTxnPhysicalEntriesDelta,
			rd.StreamTxnPhysicalEntriesPerSecond,
			rd.StreamTxnBatchesDelta,
			rd.ReconcileCountDelta,
			rd.FlatAccumulatorIndexedRepairDelta,
			rd.FlatAccumulatorFullBucketFallbacks,
			rd.FlatAccumulatorProofMismatchDelta,
			rd.LocalKIDIndexFallbackScansDelta,
			rd.CheckpointStorageDriftDelta,
		)
	}
	printRoleStatusDelta("primary", d.Primary)
	printRoleStatusDelta("secondary1", d.Secondary1)
	printRoleStatusDelta("secondary2", d.Secondary2)
}

func formatStringMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, " ")
}
