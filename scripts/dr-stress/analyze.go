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
	OpsPerSec         float64                  `json:"ops_per_sec"`
	LatencyByOp       map[string]*LatencyStats `json:"latency_by_op"`
	ErrorHistogram    map[string]int64         `json:"error_histogram"`
	ThroughputBuckets []ThroughputBucket       `json:"throughput_buckets,omitempty"`
	LagSlope          *LagSlopeResult          `json:"lag_slope,omitempty"`
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
	latencies, errors, buckets, firstTS := parseEventsFile(eventsFile)

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

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(analysis)
	} else {
		printAnalysisTable(analysis)
	}
}

func parseEventsFile(path string) (latencies map[string][]float64, errors map[string]int64, buckets []ThroughputBucket, firstTS time.Time) {
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
		if ev.Err != "" {
			key := fmt.Sprintf("%s:%d:%s", ev.Op, ev.Code, ev.Err)
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

func printAnalysisTable(a AnalysisResult) {
	fmt.Printf("\n=== Analysis: %s ===\n", a.RunID)
	fmt.Printf("Result: %s\n", a.ResultFile)
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

	// Sentinel.
	if len(a.Sentinel) > 0 {
		fmt.Printf("\nSentinel convergence:\n")
		for k, v := range a.Sentinel {
			fmt.Printf("  %s: %.1fs\n", k, v)
		}
	}

	fmt.Println()
}
