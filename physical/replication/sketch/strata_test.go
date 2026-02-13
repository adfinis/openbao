// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package sketch

import (
	"crypto/sha256"
	"fmt"
	"math"
	"testing"
)

func TestStrataEstimator_IdenticalSets(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	for i := 0; i < 1000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}
	if est != 0 {
		t.Fatalf("expected estimate 0 for identical sets, got %d", est)
	}
}

func TestStrataEstimator_SmallDifference(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	// Common: 1000 items.
	for i := 0; i < 1000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: 10 items.
	for i := 1000; i < 1010; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}

	// Estimate should be exact or close for small differences
	// (all strata should decode successfully).
	if est != 10 {
		t.Fatalf("expected exact estimate of 10, got %d", est)
	}
}

func TestStrataEstimator_SymmetricDifference(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	// Common: 500 items.
	for i := 0; i < 500; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: 25 items.
	for i := 500; i < 525; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	// Only in B: 25 items.
	for i := 600; i < 625; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}

	// Exact: 50 differences total (25 + 25).
	if est != 50 {
		t.Fatalf("expected exact estimate of 50, got %d", est)
	}
}

func TestStrataEstimator_MediumDifference(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	// Common: 10000 items.
	for i := 0; i < 10000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: 200 items.
	for i := 10000; i < 10200; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	// Only in B: 200 items.
	for i := 20000; i < 20200; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}

	// For medium differences, the estimate may not be exact due to
	// strata overflow, but should be within 2x of actual.
	actual := 400
	ratio := float64(est) / float64(actual)
	t.Logf("actual=%d, estimate=%d, ratio=%.2f", actual, est, ratio)
	if ratio < 0.5 || ratio > 2.0 {
		t.Fatalf("estimate %d too far from actual %d (ratio %.2f)", est, actual, ratio)
	}
}

func TestStrataEstimator_LargeDifference(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	// Only in A: 5000 items.
	for i := 0; i < 5000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	// Only in B: 5000 items (completely different set).
	for i := 10000; i < 15000; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}

	// For large differences, the estimate is approximate. Allow wider
	// tolerance (within order of magnitude).
	actual := 10000
	ratio := float64(est) / float64(actual)
	t.Logf("actual=%d, estimate=%d, ratio=%.2f", actual, est, ratio)
	if ratio < 0.1 || ratio > 10.0 {
		t.Fatalf("estimate %d wildly off from actual %d (ratio %.2f)", est, actual, ratio)
	}
}

func TestStrataEstimator_ValueChanges(t *testing.T) {
	a := NewDefaultStrataEstimator()
	b := NewDefaultStrataEstimator()

	// Same 100 keys, but 15 have different values.
	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		if i < 15 {
			// Different value in B for these keys.
			vid2 := makeVID(i + 10000)
			b.Insert(kid, vid2)
		} else {
			b.Insert(kid, vid)
		}
	}

	est, err := a.Estimate(b)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}

	// Value changes produce 2 differences per key (old in A, new in B).
	actual := 30
	if est != actual {
		// Allow some tolerance for strata overflow.
		ratio := float64(est) / float64(actual)
		if ratio < 0.5 || ratio > 2.0 {
			t.Fatalf("estimate %d too far from actual %d", est, actual)
		}
	}
}

func TestStrataEstimator_MarshalUnmarshal(t *testing.T) {
	se := NewDefaultStrataEstimator()
	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		se.Insert(kid, vid)
	}

	data := se.Marshal()
	restored, err := UnmarshalStrataEstimator(data)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if restored.levels != se.levels {
		t.Fatalf("levels mismatch: %d vs %d", restored.levels, se.levels)
	}

	// Estimating against restored should give 0 difference.
	est, err := se.Estimate(restored)
	if err != nil {
		t.Fatalf("estimate failed: %v", err)
	}
	if est != 0 {
		t.Fatalf("expected 0 after roundtrip, got %d", est)
	}
}

func TestStrataEstimator_MarshalSize(t *testing.T) {
	se := NewDefaultStrataEstimator()
	data := se.Marshal()
	sizeKB := float64(len(data)) / 1024.0
	t.Logf("strata estimator marshal size: %.1f KB (%d bytes, %d levels)", sizeKB, len(data), se.levels)

	// Should be roughly 32 * 80 * 104 bytes + overhead ≈ 260 KB.
	// This is larger than the ~80 KB estimate in the plan because
	// cellSize is 104 bytes (not ~32 bytes).
	if sizeKB > 512 {
		t.Fatalf("strata estimator too large: %.1f KB", sizeKB)
	}
}

func TestStrataEstimator_UnmarshalErrors(t *testing.T) {
	_, err := UnmarshalStrataEstimator([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for short data")
	}
}

// makeVID creates a deterministic VID from an integer (different series
// from makeItem's VID).
func makeVID(i int) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("alt-val-%d", i)))
}

// BenchmarkStrataEstimator benchmarks insert performance.
func BenchmarkStrataEstimator_Insert(b *testing.B) {
	items := make([][2][32]byte, 10000)
	for i := range items {
		items[i][0], items[i][1] = makeItem(i)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		se := NewDefaultStrataEstimator()
		for i := range items {
			se.Insert(items[i][0], items[i][1])
		}
	}
}

func BenchmarkStrataEstimator_Estimate(b *testing.B) {
	a := NewDefaultStrataEstimator()
	other := NewDefaultStrataEstimator()

	for i := 0; i < 10000; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		other.Insert(kid, vid)
	}
	// Add 100 extra to a.
	for i := 10000; i < 10100; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		a.Estimate(other)
	}
}

func init() {
	// Ensure imports are used.
	_ = math.Ceil
	_ = sha256.Sum256
	_ = fmt.Sprintf
}
