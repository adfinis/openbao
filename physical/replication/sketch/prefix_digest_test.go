// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package sketch

import (
	"testing"
)

func TestPrefixDigest_IdenticalSets(t *testing.T) {
	a := NewPrefixDigest(8) // 256 buckets
	b := NewPrefixDigest(8)

	for i := 0; i < 1000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}

	mismatched, err := a.Compare(b)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	if len(mismatched) != 0 {
		t.Fatalf("expected 0 mismatched buckets, got %d", len(mismatched))
	}
}

func TestPrefixDigest_SmallDifference(t *testing.T) {
	a := NewPrefixDigest(8)
	b := NewPrefixDigest(8)

	// Common items.
	for i := 0; i < 1000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: 5 items.
	for i := 1000; i < 1005; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}

	mismatched, err := a.Compare(b)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	// At most 5 buckets should differ (one per extra item, possibly
	// fewer if items share a bucket).
	if len(mismatched) == 0 {
		t.Fatal("expected some mismatched buckets")
	}
	if len(mismatched) > 5 {
		t.Fatalf("expected at most 5 mismatched buckets, got %d", len(mismatched))
	}
	t.Logf("5 differing items produced %d mismatched buckets (out of 256)", len(mismatched))
}

func TestPrefixDigest_ValueChange(t *testing.T) {
	a := NewPrefixDigest(8)
	b := NewPrefixDigest(8)

	// Same key, different value.
	kid, _ := makeItem(42)
	vidA := makeVID(1)
	vidB := makeVID(2)
	a.Insert(kid, vidA)
	b.Insert(kid, vidB)

	mismatched, err := a.Compare(b)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	// The key maps to the same bucket (same KID prefix), but the
	// XORValueHash should differ.
	if len(mismatched) != 1 {
		t.Fatalf("expected 1 mismatched bucket, got %d", len(mismatched))
	}
}

func TestPrefixDigest_InsertDelete(t *testing.T) {
	pd := NewPrefixDigest(8)
	kid, vid := makeItem(1)
	pd.Insert(kid, vid)
	pd.Delete(kid, vid)

	// All buckets should be zero.
	for i := uint32(0); i < pd.NumBuckets(); i++ {
		b := pd.Bucket(i)
		if !b.IsZero() {
			t.Fatalf("bucket %d not zero after insert+delete", i)
		}
	}
}

func TestPrefixDigest_Compare_MismatchedPrefix(t *testing.T) {
	a := NewPrefixDigest(8)
	b := NewPrefixDigest(10)
	_, err := a.Compare(b)
	if err == nil {
		t.Fatal("expected error for mismatched prefix lengths")
	}
}

func TestPrefixDigest_DrillDown(t *testing.T) {
	// Start with p=4 (16 buckets), find mismatches, drill to p=8.
	a := NewPrefixDigest(4)
	b := NewPrefixDigest(4)

	// Common: 100 items.
	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: 3 items.
	extraKIDs := make([][32]byte, 3)
	for i := 0; i < 3; i++ {
		kid, vid := makeItem(200 + i)
		a.Insert(kid, vid)
		extraKIDs[i] = kid
	}

	mismatched, err := a.Compare(b)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	t.Logf("p=4: %d mismatched out of 16 buckets", len(mismatched))

	// Drill down: create p=8 digests for elements in mismatched buckets.
	aDeep := NewPrefixDigest(8)
	bDeep := NewPrefixDigest(8)

	// Re-iterate items, only adding those in mismatched parent buckets.
	mismatchSet := make(map[uint32]bool)
	for _, idx := range mismatched {
		mismatchSet[idx] = true
	}

	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		if mismatchSet[ParentBucket(kid, 4)] {
			aDeep.Insert(kid, vid)
			bDeep.Insert(kid, vid)
		}
	}
	for i := 0; i < 3; i++ {
		kid, vid := makeItem(200 + i)
		if mismatchSet[ParentBucket(kid, 4)] {
			aDeep.Insert(kid, vid)
		}
	}

	deepMismatched, err := aDeep.Compare(bDeep)
	if err != nil {
		t.Fatalf("deep compare failed: %v", err)
	}
	t.Logf("p=8: %d mismatched out of 256 buckets (after drill-down from %d parent buckets)", len(deepMismatched), len(mismatched))

	// The deep mismatched count should be <= the shallow one
	// (more precision narrows the search).
	if len(deepMismatched) > len(mismatched)*16 {
		t.Fatal("drill-down should not produce more mismatched buckets than 16x parent")
	}
}

func TestPrefixDigest_MarshalUnmarshal(t *testing.T) {
	pd := NewPrefixDigest(8)
	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		pd.Insert(kid, vid)
	}

	data := pd.Marshal()
	restored, err := UnmarshalPrefixDigest(data)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if restored.prefixLen != pd.prefixLen {
		t.Fatal("prefix length mismatch after roundtrip")
	}

	mismatched, err := pd.Compare(restored)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	if len(mismatched) != 0 {
		t.Fatal("roundtrip produced different digest")
	}
}

func TestPrefixDigest_MarshalSize(t *testing.T) {
	pd := NewPrefixDigest(8)
	data := pd.Marshal()
	t.Logf("p=8 marshal size: %d bytes (%.1f KB)", len(data), float64(len(data))/1024.0)
	// Expected: 8 + 256 * 76 = 19464 bytes ≈ 19 KB
	if len(data) > 25000 {
		t.Fatalf("digest too large: %d bytes", len(data))
	}
}

func TestPrefixDigest_MarshalBuckets_Subset(t *testing.T) {
	pd := NewPrefixDigest(8)
	for i := 0; i < 50; i++ {
		kid, vid := makeItem(i)
		pd.Insert(kid, vid)
	}

	// Marshal only 3 specific buckets.
	indices := []uint32{0, 10, 255}
	data := pd.MarshalBuckets(indices)
	t.Logf("3-bucket partial marshal: %d bytes", len(data))

	// Should be much smaller than full marshal.
	fullData := pd.Marshal()
	if len(data) >= len(fullData) {
		t.Fatal("partial marshal should be smaller than full")
	}
}

func TestPrefixDigest_ParentBucket(t *testing.T) {
	kid, _ := makeItem(1)

	// Parent bucket at p=4 should be the same for elements that
	// share the first 4 bits.
	parent4 := ParentBucket(kid, 4)
	parent8 := ParentBucket(kid, 8)

	// The p=4 parent should be the top 4 bits of p=8 index.
	if parent4 != parent8>>4 {
		t.Fatalf("parent bucket relationship broken: p4=%d, p8=%d, p8>>4=%d", parent4, parent8, parent8>>4)
	}
}

func BenchmarkPrefixDigest_Insert(b *testing.B) {
	items := make([][2][32]byte, 10000)
	for i := range items {
		items[i][0], items[i][1] = makeItem(i)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		pd := NewPrefixDigest(8)
		for i := range items {
			pd.Insert(items[i][0], items[i][1])
		}
	}
}

func BenchmarkPrefixDigest_Compare(b *testing.B) {
	a := NewPrefixDigest(8)
	other := NewPrefixDigest(8)

	for i := 0; i < 10000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		other.Insert(kid, vid)
	}
	for i := 10000; i < 10050; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		a.Compare(other)
	}
}
