// Copyright (c) OpenBao a]Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

// Package sketch implements probabilistic data structures for set
// reconciliation, specifically designed for disaster-recovery replication
// in OpenBao. The core structures are:
//
//   - IBLT (Invertible Bloom Lookup Table): enables O(d) set difference
//     computation where d is the number of differing elements.
//   - Strata Estimator: estimates the size of set differences.
//   - Prefix Digest: flat bucket summaries for adaptive divergence detection.
//
// Prior art:
//   - Goodrich & Mitzenmacher, "Invertible Bloom Lookup Tables" (2011)
//   - Eppstein et al., "What's the Difference?" (SIGCOMM 2011)
package sketch

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	// DefaultHashCount is the number of hash functions used for cell
	// indexing in the IBLT. 3 is standard in the literature and provides
	// a good decode success rate when the table is sized at ~1.5x the
	// expected difference.
	DefaultHashCount = 3

	// cellSize is the serialized byte size of a single IBLT cell:
	//   count (8) + keySum (32) + keyHashSum (32) + valueSum (32) = 104
	cellSize = 8 + 32 + 32 + 32
)

// Cell is a single bucket in the IBLT.
type Cell struct {
	// Count tracks the net number of items mapped to this cell.
	// Positive after Insert, negative after Delete or Subtract.
	Count int64

	// KeySum is the XOR of all KIDs mapped to this cell.
	KeySum [32]byte

	// KeyHashSum is the XOR of Hash(KID) for all items in this cell.
	// Used during peeling to verify that a cell with Count=+/-1 truly
	// contains a single element (not a collision).
	KeyHashSum [32]byte

	// ValueSum is the XOR of all VIDs mapped to this cell.
	ValueSum [32]byte
}

// IBLT is an Invertible Bloom Lookup Table for set reconciliation.
//
// It supports inserting (KID, VID) pairs and computing the symmetric
// difference between two sets by subtracting one IBLT from another,
// then "peeling" (decoding) the result.
//
// The decode succeeds with high probability when the number of
// differing elements is at most ~numCells/1.5.
type IBLT struct {
	cells     []Cell
	numCells  uint32
	hashCount uint32
}

// DiffEntry represents a single element recovered during IBLT decoding.
type DiffEntry struct {
	KID [32]byte
	VID [32]byte
}

// NewIBLT creates a new IBLT with the given number of cells and hash
// functions. For best decode rates, numCells should be ~1.5x the
// expected number of differences.
func NewIBLT(numCells, hashCount uint32) *IBLT {
	if hashCount == 0 {
		hashCount = DefaultHashCount
	}
	if numCells < hashCount {
		numCells = hashCount
	}
	// Round up to a multiple of hashCount so partition-based hashing
	// divides evenly.
	if rem := numCells % hashCount; rem != 0 {
		numCells += hashCount - rem
	}
	return &IBLT{
		cells:     make([]Cell, numCells),
		numCells:  numCells,
		hashCount: hashCount,
	}
}

// NewIBLTForDiff creates an IBLT optimally sized for an expected
// difference of d elements. Uses ~1.5x overhead and default hash count.
func NewIBLTForDiff(d uint32) *IBLT {
	numCells := uint32(math.Ceil(float64(d) * 1.5))
	if numCells < 3 {
		numCells = 3
	}
	return NewIBLT(numCells, DefaultHashCount)
}

// NumCells returns the number of cells in this IBLT.
func (t *IBLT) NumCells() uint32 {
	return t.numCells
}

// HashCount returns the number of hash functions used.
func (t *IBLT) HashCount() uint32 {
	return t.hashCount
}

// Insert adds a (KID, VID) pair to the IBLT.
func (t *IBLT) Insert(kid, vid [32]byte) {
	elemHash := hashElement(kid, vid)
	indices := t.cellIndices(elemHash)
	for _, idx := range indices {
		c := &t.cells[idx]
		c.Count++
		xor32(&c.KeySum, kid)
		xor32(&c.KeyHashSum, elemHash)
		xor32(&c.ValueSum, vid)
	}
}

// Delete removes a (KID, VID) pair from the IBLT. This is the inverse
// of Insert; the element must have been previously inserted.
func (t *IBLT) Delete(kid, vid [32]byte) {
	elemHash := hashElement(kid, vid)
	indices := t.cellIndices(elemHash)
	for _, idx := range indices {
		c := &t.cells[idx]
		c.Count--
		xor32(&c.KeySum, kid)
		xor32(&c.KeyHashSum, elemHash)
		xor32(&c.ValueSum, vid)
	}
}

// Subtract computes the cell-wise difference (t - other) and returns
// a new IBLT representing the symmetric difference. Both IBLTs must
// have the same numCells and hashCount.
func (t *IBLT) Subtract(other *IBLT) (*IBLT, error) {
	if t.numCells != other.numCells || t.hashCount != other.hashCount {
		return nil, errors.New("iblt: cannot subtract IBLTs with different parameters")
	}

	result := NewIBLT(t.numCells, t.hashCount)
	for i := uint32(0); i < t.numCells; i++ {
		a := &t.cells[i]
		b := &other.cells[i]
		r := &result.cells[i]
		r.Count = a.Count - b.Count
		xorCopy(&r.KeySum, a.KeySum, b.KeySum)
		xorCopy(&r.KeyHashSum, a.KeyHashSum, b.KeyHashSum)
		xorCopy(&r.ValueSum, a.ValueSum, b.ValueSum)
	}
	return result, nil
}

// Decode attempts to recover all differing elements from the IBLT.
// This is typically called on the result of Subtract().
//
// Returns:
//   - added: elements present in the "positive" set (Count > 0 side)
//   - removed: elements present in the "negative" set (Count < 0 side)
//   - ok: true if decoding succeeded (all cells are zero after peeling)
//
// If ok is false, the IBLT may have been too small for the actual
// difference, or hash collisions prevented full decoding.
func (t *IBLT) Decode() (added []DiffEntry, removed []DiffEntry, ok bool) {
	// Work on a copy so the original is not modified.
	work := NewIBLT(t.numCells, t.hashCount)
	for i := range t.cells {
		work.cells[i] = t.cells[i]
	}

	// Peeling loop: repeatedly find cells with count +/-1, extract the
	// element, and remove it from all other cells it maps to.
	changed := true
	for changed {
		changed = false
		for i := uint32(0); i < work.numCells; i++ {
			c := &work.cells[i]
			if c.Count != 1 && c.Count != -1 {
				continue
			}

			// Verify this is a pure cell (contains exactly one element):
			// Hash(KeySum || ValueSum) should equal KeyHashSum.
			expectedHash := hashElement(c.KeySum, c.ValueSum)
			if expectedHash != c.KeyHashSum {
				continue
			}

			kid := c.KeySum
			vid := c.ValueSum

			if c.Count == 1 {
				added = append(added, DiffEntry{KID: kid, VID: vid})
				// Remove this positive element from all cells.
				work.Delete(kid, vid)
			} else {
				removed = append(removed, DiffEntry{KID: kid, VID: vid})
				// Cancel this negative element by inserting it.
				work.Insert(kid, vid)
			}
			changed = true
		}
	}

	// Check if all cells are empty (successful decode).
	ok = true
	for i := uint32(0); i < work.numCells; i++ {
		if work.cells[i].Count != 0 {
			ok = false
			break
		}
	}

	return added, removed, ok
}

// IsEmpty returns true if all cells have count zero.
func (t *IBLT) IsEmpty() bool {
	for i := uint32(0); i < t.numCells; i++ {
		if t.cells[i].Count != 0 {
			return false
		}
	}
	return true
}

// Marshal serializes the IBLT into a byte slice for network transport.
// Format: [4 bytes numCells][4 bytes hashCount][cells...]
// Each cell: [8 bytes count][32 bytes keySum][32 bytes keyHashSum][32 bytes valueSum]
func (t *IBLT) Marshal() []byte {
	buf := make([]byte, 8+int(t.numCells)*cellSize)
	binary.LittleEndian.PutUint32(buf[0:4], t.numCells)
	binary.LittleEndian.PutUint32(buf[4:8], t.hashCount)

	offset := 8
	for i := uint32(0); i < t.numCells; i++ {
		c := &t.cells[i]
		binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(c.Count))
		offset += 8
		copy(buf[offset:offset+32], c.KeySum[:])
		offset += 32
		copy(buf[offset:offset+32], c.KeyHashSum[:])
		offset += 32
		copy(buf[offset:offset+32], c.ValueSum[:])
		offset += 32
	}
	return buf
}

// UnmarshalIBLT deserializes an IBLT from a byte slice produced by Marshal.
func UnmarshalIBLT(data []byte) (*IBLT, error) {
	if len(data) < 8 {
		return nil, errors.New("iblt: data too short for header")
	}
	numCells := binary.LittleEndian.Uint32(data[0:4])
	hashCount := binary.LittleEndian.Uint32(data[4:8])

	expected := 8 + int(numCells)*cellSize
	if len(data) != expected {
		return nil, fmt.Errorf("iblt: expected %d bytes, got %d", expected, len(data))
	}

	t := NewIBLT(numCells, hashCount)
	offset := 8
	for i := uint32(0); i < numCells; i++ {
		c := &t.cells[i]
		c.Count = int64(binary.LittleEndian.Uint64(data[offset : offset+8]))
		offset += 8
		copy(c.KeySum[:], data[offset:offset+32])
		offset += 32
		copy(c.KeyHashSum[:], data[offset:offset+32])
		offset += 32
		copy(c.ValueSum[:], data[offset:offset+32])
		offset += 32
	}
	return t, nil
}

// --- Internal helpers ---

// hashElement computes SHA-256(kid || vid) as the combined element hash.
// This is used for both cell index mapping and the purity checksum
// (KeyHashSum). Using the combined hash ensures that items with the
// same KID but different VID map to different cells, enabling correct
// reconciliation of value changes.
func hashElement(kid, vid [32]byte) [32]byte {
	var buf [64]byte
	copy(buf[0:32], kid[:])
	copy(buf[32:64], vid[:])
	return sha256.Sum256(buf[:])
}

// cellIndices computes the hashCount cell indices for a given element.
// Uses partition-based hashing: the table is divided into hashCount
// equal sections, and each hash function selects one cell from its
// own section. This guarantees:
//   - Each element maps to exactly one cell per section (no duplicates).
//   - Two different elements are less likely to collide across all
//     sections, improving peeling success rates.
func (t *IBLT) cellIndices(elemHash [32]byte) []uint32 {
	sectionSize := t.numCells / t.hashCount
	if sectionSize == 0 {
		sectionSize = 1
	}

	indices := make([]uint32, t.hashCount)
	for i := uint32(0); i < t.hashCount; i++ {
		// Each hash function uses a different 8-byte chunk of the
		// 32-byte element hash. Since hashCount <= 4 and we have
		// 32 bytes, this gives independent hash material per section.
		offset := i * 8
		if offset+8 > 32 {
			// Fallback for hashCount > 4: rehash with section index.
			var buf [36]byte
			copy(buf[0:32], elemHash[:])
			binary.LittleEndian.PutUint32(buf[32:36], i)
			rehashed := sha256.Sum256(buf[:])
			h := binary.LittleEndian.Uint64(rehashed[0:8])
			indices[i] = i*sectionSize + uint32(h%uint64(sectionSize))
		} else {
			h := binary.LittleEndian.Uint64(elemHash[offset : offset+8])
			indices[i] = i*sectionSize + uint32(h%uint64(sectionSize))
		}
	}
	return indices
}

// xor32 XORs b into a in-place: a ^= b.
func xor32(a *[32]byte, b [32]byte) {
	for i := 0; i < 32; i++ {
		a[i] ^= b[i]
	}
}

// xorCopy sets dst = a XOR b.
func xorCopy(dst *[32]byte, a, b [32]byte) {
	for i := 0; i < 32; i++ {
		dst[i] = a[i] ^ b[i]
	}
}
