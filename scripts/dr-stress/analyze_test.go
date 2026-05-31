package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseEventsFileSeparatesControlEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	base := time.Unix(1000, 0).UTC()
	events := []Event{
		{TS: base, Op: "put", Role: "primary", Code: 204, LatencyNS: int64(5 * time.Millisecond)},
		{TS: base.Add(time.Second), Op: "stepdown", Role: "primary", Code: 204, Control: true},
		{TS: base.Add(2 * time.Second), Op: "get_primary", Role: "primary", Code: 500, Err: "active_handoff"},
	}
	writeEvents(t, path, events)

	latencies, errors, buckets, firstTS, parsed := parseEventsFile(path)
	if !firstTS.Equal(base) {
		t.Fatalf("firstTS = %s, want %s", firstTS, base)
	}
	if len(parsed) != len(events) {
		t.Fatalf("parsed events = %d, want %d", len(parsed), len(events))
	}
	if got := len(latencies["stepdown"]); got != 0 {
		t.Fatalf("stepdown latencies = %d, want 0", got)
	}
	if got := len(latencies["put"]); got != 1 {
		t.Fatalf("put latencies = %d, want 1", got)
	}
	if got := len(errors); got != 1 {
		t.Fatalf("errors = %d, want 1", got)
	}
	if got := len(buckets); got != 3 {
		t.Fatalf("throughput buckets = %d, want 3", got)
	}
	if buckets[1].Count != 0 {
		t.Fatalf("control event counted in throughput bucket: %#v", buckets[1])
	}
}

func TestComputeStepdownWindows(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	events := []Event{
		{TS: base.Add(-20 * time.Second), Op: "put", Code: 204},
		{TS: base, Op: "stepdown", Role: "primary", Code: 204, Control: true},
		{TS: base.Add(2 * time.Second), Op: "put", Code: 500, Err: "active_handoff"},
		{TS: base.Add(20 * time.Second), Op: "get_primary", Code: 200},
		{TS: base.Add(70 * time.Second), Op: "status_s1", Code: 200},
	}

	got := computeStepdownWindows(events, events[0].TS)
	if len(got) != 1 {
		t.Fatalf("windows summaries = %d, want 1", len(got))
	}
	if len(got[0].Windows) != 4 {
		t.Fatalf("windows = %d, want 4", len(got[0].Windows))
	}
	if got[0].Windows[0].Operations != 1 {
		t.Fatalf("pre window ops = %d, want 1", got[0].Windows[0].Operations)
	}
	if got[0].Windows[1].Errors != 1 {
		t.Fatalf("handoff window errors = %d, want 1", got[0].Windows[1].Errors)
	}
	if got[0].Windows[3].Operations != 0 {
		t.Fatalf("post-60 window ops = %d, want 0 because last event is the boundary", got[0].Windows[3].Operations)
	}
}

func TestComputeStatusDeltas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status_timeline.ndjson")
	base := time.Unix(1000, 0).UTC()
	snapshots := []StatusSnapshot{
		{
			TS: base,
			Secondary1: &DRStatusResponse{
				PrimaryIndex:                  10,
				LastAppliedIndex:              10,
				StreamTxnPhysicalTotal:        20,
				StreamTxnBatchesTotal:         2,
				FlatAccIndexedRepairTotal:     1,
				FlatAccIndexedFullBucketTotal: 0,
				LocalKIDIndexFallbackScans:    0,
				FlatAccIndexedProofMismatches: 0,
				CheckpointStorageDrift:        0,
			},
		},
		{
			TS: base.Add(10 * time.Second),
			Secondary1: &DRStatusResponse{
				LastAppliedIndex:              15,
				StreamTxnPhysicalTotal:        45,
				StreamTxnBatchesTotal:         5,
				FlatAccIndexedRepairTotal:     2,
				FlatAccIndexedFullBucketTotal: 1,
				LocalKIDIndexFallbackScans:    1,
				FlatAccIndexedProofMismatches: 1,
				CheckpointStorageDrift:        3,
				PrimaryIndex:                  20,
			},
		},
	}
	writeSnapshots(t, path, snapshots)

	got := computeStatusDeltas(path)
	if got == nil || got.Secondary1 == nil {
		t.Fatal("missing secondary1 delta")
	}
	if got.Secondary1.StreamTxnPhysicalEntriesDelta != 25 {
		t.Fatalf("physical delta = %d, want 25", got.Secondary1.StreamTxnPhysicalEntriesDelta)
	}
	if got.Secondary1.StreamTxnPhysicalEntriesPerSecond != 2.5 {
		t.Fatalf("physical rate = %f, want 2.5", got.Secondary1.StreamTxnPhysicalEntriesPerSecond)
	}
	if got.Secondary1.FlatAccumulatorFullBucketFallbacks != 1 {
		t.Fatalf("full-bucket delta = %d, want 1", got.Secondary1.FlatAccumulatorFullBucketFallbacks)
	}
}

func writeEvents(t *testing.T, path string, events []Event) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
}

func writeSnapshots(t *testing.T, path string, snapshots []StatusSnapshot) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, snap := range snapshots {
		if err := enc.Encode(snap); err != nil {
			t.Fatal(err)
		}
	}
}
