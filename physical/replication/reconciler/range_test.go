// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package reconciler

import (
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/physical"
)

func TestBuildRangeManifestDeterministic(t *testing.T) {
	rs := &ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		Entries:  make(map[[32]byte]*physical.Entry),
	}

	for i := 0; i < 200; i++ {
		kid := testKID(i)
		val := []byte{byte(i % 251), byte((i * 7) % 251)}
		vid := sha256.Sum256(val)
		rs.KIDToVID[kid] = vid
		rs.Entries[kid] = &physical.Entry{
			Key:   string([]byte{'k', byte(i % 255)}),
			Value: val,
		}
	}

	cfg := DefaultRangePlanConfig()
	cfg.TargetKeysPerRange = 17
	cfg.MaxTopRanges = 32
	cfg.MinKeysPerRange = 4

	a, err := BuildRangeManifest(rs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := BuildRangeManifest(rs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("range manifests are not deterministic")
	}
	if len(a) == 0 {
		t.Fatalf("expected non-empty range manifest")
	}
	for i := 1; i < len(a); i++ {
		prev := a[i-1]
		cur := a[i]
		if !prev.Span.Valid() || !cur.Span.Valid() {
			t.Fatalf("invalid span in manifest")
		}
		if bytesCmp(prev.Span.EndKID, cur.Span.StartKID) >= 0 {
			t.Fatalf("ranges overlap or are not strictly ordered: %d", i)
		}
	}
}

func TestSplitRange(t *testing.T) {
	span := RangeSpan{
		StartKID: testKID(10),
		EndKID:   testKID(100),
	}
	left, right, ok := SplitRange(span)
	if !ok {
		t.Fatalf("expected split to succeed")
	}
	if !left.Valid() || !right.Valid() {
		t.Fatalf("invalid split spans")
	}
	if bytesCmp(left.EndKID, right.StartKID) >= 0 {
		t.Fatalf("split spans should not overlap")
	}
	if !left.Contains(span.StartKID) {
		t.Fatalf("left span should include original start")
	}
	if !right.Contains(span.EndKID) {
		t.Fatalf("right span should include original end")
	}
}

func TestBuildRangeIBLTFromMap_Filtering(t *testing.T) {
	kidToVID := make(map[[32]byte][32]byte)
	for i := 0; i < 20; i++ {
		kid := testKID(i)
		vid := sha256.Sum256([]byte{byte(i)})
		kidToVID[kid] = vid
	}

	span := RangeSpan{
		StartKID: testKID(5),
		EndKID:   testKID(12),
	}
	got := BuildRangeIBLTFromMap(kidToVID, span, 128)
	want := sketch.NewIBLT(128, sketch.DefaultHashCount)
	for i := 5; i <= 12; i++ {
		kid := testKID(i)
		want.Insert(kid, kidToVID[kid])
	}

	diff, err := got.Subtract(want)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}
	if !diff.IsEmpty() {
		t.Fatalf("range-filtered IBLT does not match expected entries")
	}
}

func TestBuildRangeManifest_NoSingleRangeCollapseForDistributedKIDs(t *testing.T) {
	rs := &ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
	}
	for i := 0; i < 200; i++ {
		var kid [32]byte
		kid[0] = byte(i)
		vid := sha256.Sum256([]byte{byte(i), byte(i >> 1)})
		rs.KIDToVID[kid] = vid
	}

	cfg := DefaultRangePlanConfig()
	cfg.TopLevelHashBits = 8
	cfg.MaxTopRanges = 256

	manifest, err := BuildRangeManifest(rs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(manifest) <= 1 {
		t.Fatalf("expected multiple top-level ranges, got %d", len(manifest))
	}
	for _, desc := range manifest {
		if desc.Span.StartKID[0] != desc.Span.EndKID[0] {
			t.Fatalf("expected fixed hash-interval ranges to keep one top-level prefix bucket")
		}
	}
}

func TestBuildFixedHashRangeManifestFromItems(t *testing.T) {
	items := make([]CheckpointItemMeta, 0, 64)
	for i := 0; i < 64; i++ {
		var kid [32]byte
		kid[0] = byte(i)
		vid := sha256.Sum256([]byte{byte(i)})
		items = append(items, CheckpointItemMeta{KID: kid, VID: vid, Key: "k"})
	}

	cfg := DefaultRangePlanConfig()
	cfg.TopLevelHashBits = 8
	cfg.MaxTopRanges = 256

	manifestA, err := BuildFixedHashRangeManifest(items, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	manifestB, err := BuildFixedHashRangeManifest(items, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(manifestA, manifestB) {
		t.Fatalf("fixed hash manifest should be deterministic")
	}
	if len(manifestA) != len(items) {
		t.Fatalf("expected one non-empty range per populated top-level prefix; got %d want %d", len(manifestA), len(items))
	}
}

func testKID(i int) [32]byte {
	var kid [32]byte
	binary.BigEndian.PutUint64(kid[24:], uint64(i))
	return kid
}

func bytesCmp(a, b [32]byte) int {
	for i := 0; i < 32; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
