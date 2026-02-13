// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package sketch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"
)

// makeItem creates a deterministic (KID, VID) pair from an integer.
func makeItem(i int) (kid, vid [32]byte) {
	kid = sha256.Sum256([]byte(fmt.Sprintf("key-%d", i)))
	vid = sha256.Sum256([]byte(fmt.Sprintf("val-%d", i)))
	return
}

// makeRandomItem creates a random (KID, VID) pair.
func makeRandomItem() (kid, vid [32]byte) {
	rand.Read(kid[:])
	rand.Read(vid[:])
	return
}

func TestIBLT_InsertAndDecode_SingleElement(t *testing.T) {
	iblt := NewIBLT(10, DefaultHashCount)
	kid, vid := makeItem(1)
	iblt.Insert(kid, vid)

	added, removed, ok := iblt.Decode()
	if !ok {
		t.Fatal("decode failed for single element")
	}
	if len(added) != 1 {
		t.Fatalf("expected 1 added, got %d", len(added))
	}
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed, got %d", len(removed))
	}
	if added[0].KID != kid || added[0].VID != vid {
		t.Fatal("decoded element does not match inserted element")
	}
}

func TestIBLT_InsertDelete_Empty(t *testing.T) {
	iblt := NewIBLT(10, DefaultHashCount)
	kid, vid := makeItem(1)
	iblt.Insert(kid, vid)
	iblt.Delete(kid, vid)

	if !iblt.IsEmpty() {
		t.Fatal("IBLT should be empty after insert+delete of same element")
	}

	added, removed, ok := iblt.Decode()
	if !ok {
		t.Fatal("decode failed on empty IBLT")
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("expected 0/0, got %d/%d", len(added), len(removed))
	}
}

func TestIBLT_Subtract_IdenticalSets(t *testing.T) {
	a := NewIBLT(100, DefaultHashCount)
	b := NewIBLT(100, DefaultHashCount)

	for i := 0; i < 50; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}

	diff, err := a.Subtract(b)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	if !diff.IsEmpty() {
		t.Fatal("difference of identical sets should be empty")
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Fatal("decode failed")
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("expected 0/0, got %d/%d", len(added), len(removed))
	}
}

func TestIBLT_Subtract_SmallDifference(t *testing.T) {
	// Set A has items 0-99, set B has items 0-89 (missing 90-99).
	// Difference: 10 items only in A.
	numCells := uint32(30) // ~1.5x * 10 = 15, use 30 for comfort
	a := NewIBLT(numCells, DefaultHashCount)
	b := NewIBLT(numCells, DefaultHashCount)

	for i := 0; i < 100; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	for i := 0; i < 90; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	diff, err := a.Subtract(b)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Fatal("decode failed")
	}
	if len(added) != 10 {
		t.Fatalf("expected 10 added, got %d", len(added))
	}
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed, got %d", len(removed))
	}

	// Verify all decoded items are from the expected range (90-99).
	expectedKIDs := make(map[[32]byte]bool)
	for i := 90; i < 100; i++ {
		kid, _ := makeItem(i)
		expectedKIDs[kid] = true
	}
	for _, e := range added {
		if !expectedKIDs[e.KID] {
			t.Errorf("unexpected KID in decoded results")
		}
	}
}

func TestIBLT_Subtract_SymmetricDifference(t *testing.T) {
	// A has items 0-9 + 20-29, B has items 0-9 + 30-39.
	// Symmetric difference: 20-29 (only in A) and 30-39 (only in B).
	numCells := uint32(40)
	a := NewIBLT(numCells, DefaultHashCount)
	b := NewIBLT(numCells, DefaultHashCount)

	// Common items.
	for i := 0; i < 10; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A.
	for i := 20; i < 30; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	// Only in B.
	for i := 30; i < 40; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	diff, err := a.Subtract(b)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Fatal("decode failed")
	}
	if len(added) != 10 {
		t.Fatalf("expected 10 added (in A not B), got %d", len(added))
	}
	if len(removed) != 10 {
		t.Fatalf("expected 10 removed (in B not A), got %d", len(removed))
	}
}

func TestIBLT_Subtract_ValueMismatch(t *testing.T) {
	// Same key in both sets but different values. Should appear as
	// one "added" (A's version) and one "removed" (B's version).
	numCells := uint32(10)
	a := NewIBLT(numCells, DefaultHashCount)
	b := NewIBLT(numCells, DefaultHashCount)

	kid := sha256.Sum256([]byte("shared-key"))
	vidA := sha256.Sum256([]byte("value-A"))
	vidB := sha256.Sum256([]byte("value-B"))

	a.Insert(kid, vidA)
	b.Insert(kid, vidB)

	diff, err := a.Subtract(b)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Fatal("decode failed")
	}
	if len(added) != 1 || len(removed) != 1 {
		t.Fatalf("expected 1/1, got %d/%d", len(added), len(removed))
	}
	if added[0].KID != kid || added[0].VID != vidA {
		t.Error("added entry doesn't match A's version")
	}
	if removed[0].KID != kid || removed[0].VID != vidB {
		t.Error("removed entry doesn't match B's version")
	}
}

func TestIBLT_Subtract_MismatchedParams(t *testing.T) {
	a := NewIBLT(10, 3)
	b := NewIBLT(20, 3)
	_, err := a.Subtract(b)
	if err == nil {
		t.Fatal("expected error for mismatched numCells")
	}

	c := NewIBLT(10, 3)
	d := NewIBLT(10, 4)
	_, err = c.Subtract(d)
	if err == nil {
		t.Fatal("expected error for mismatched hashCount")
	}
}

func TestIBLT_MarshalUnmarshal(t *testing.T) {
	iblt := NewIBLT(50, DefaultHashCount)
	for i := 0; i < 20; i++ {
		kid, vid := makeItem(i)
		iblt.Insert(kid, vid)
	}

	data := iblt.Marshal()
	restored, err := UnmarshalIBLT(data)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if restored.numCells != iblt.numCells || restored.hashCount != iblt.hashCount {
		t.Fatal("restored parameters don't match")
	}

	// Subtract original from restored should be empty.
	diff, err := iblt.Subtract(restored)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}
	if !diff.IsEmpty() {
		t.Fatal("marshal/unmarshal round-trip produced different IBLT")
	}
}

func TestIBLT_UnmarshalErrors(t *testing.T) {
	_, err := UnmarshalIBLT([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for short data")
	}

	// Valid header but wrong body length.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:4], 10)
	binary.LittleEndian.PutUint32(buf[4:8], 3)
	_, err = UnmarshalIBLT(buf)
	if err == nil {
		t.Fatal("expected error for truncated body")
	}
}

func TestNewIBLTForDiff(t *testing.T) {
	iblt := NewIBLTForDiff(10)
	if iblt.numCells < 15 {
		t.Fatalf("expected at least 15 cells for d=10, got %d", iblt.numCells)
	}

	iblt = NewIBLTForDiff(0)
	if iblt.numCells < 3 {
		t.Fatalf("expected at least 3 cells for d=0, got %d", iblt.numCells)
	}
}

func TestIBLT_LargerDifference(t *testing.T) {
	// Test with a larger difference to verify decode reliability.
	d := 100
	numCells := uint32(d * 2) // 2x overhead for safety
	a := NewIBLT(numCells, DefaultHashCount)
	b := NewIBLT(numCells, DefaultHashCount)

	// Common: 500 items.
	for i := 0; i < 500; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
		b.Insert(kid, vid)
	}
	// Only in A: items 500-549.
	for i := 500; i < 500+d/2; i++ {
		kid, vid := makeItem(i)
		a.Insert(kid, vid)
	}
	// Only in B: items 600-649.
	for i := 600; i < 600+d/2; i++ {
		kid, vid := makeItem(i)
		b.Insert(kid, vid)
	}

	diff, err := a.Subtract(b)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Fatal("decode failed for d=100 with 2x overhead")
	}
	if len(added) != d/2 {
		t.Fatalf("expected %d added, got %d", d/2, len(added))
	}
	if len(removed) != d/2 {
		t.Fatalf("expected %d removed, got %d", d/2, len(removed))
	}
}

// BenchmarkIBLT_InsertDecode benchmarks insert + decode for varying sizes.
func BenchmarkIBLT_InsertDecode(b *testing.B) {
	for _, d := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("d=%d", d), func(b *testing.B) {
			items := make([][2][32]byte, d)
			for i := 0; i < d; i++ {
				items[i][0], items[i][1] = makeItem(i)
			}

			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				iblt := NewIBLTForDiff(uint32(d))
				for i := 0; i < d; i++ {
					iblt.Insert(items[i][0], items[i][1])
				}
				iblt.Decode()
			}
		})
	}
}
