// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package sketch

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// BucketDigestSize is the serialized size of a single bucket:
	//   index (4) + count (8) + xorKeyHash (32) + xorValueHash (32) = 76
	BucketDigestSize = 4 + 8 + 32 + 32
)

// Bucket is a commutative digest for a group of elements sharing a
// common prefix in their KID hash. Two buckets match if and only if
// all three fields match (with overwhelming probability for strong
// hashes).
type Bucket struct {
	// Index is the bucket number (prefix value).
	Index uint32

	// Count is the number of elements in this bucket.
	Count uint64

	// XORKeyHash is the XOR of all SHA-256(KID) values in this bucket.
	XORKeyHash [32]byte

	// XORValueHash is the XOR of all SHA-256(KID || VID) values in
	// this bucket. This detects value changes for the same key.
	XORValueHash [32]byte
}

// Equal returns true if two buckets have identical digests.
func (b *Bucket) Equal(other *Bucket) bool {
	return b.Count == other.Count &&
		b.XORKeyHash == other.XORKeyHash &&
		b.XORValueHash == other.XORValueHash
}

// IsZero returns true if the bucket has no elements.
func (b *Bucket) IsZero() bool {
	return b.Count == 0
}

// PrefixDigest is a flat collection of bucket digests, keyed by the
// first p bits of each element's KID. It provides a cheap "where are
// the differences?" check without building a tree structure.
//
// Usage:
//  1. Both sides build PrefixDigest at the same prefix length (e.g. p=8).
//  2. Compare buckets. Matching buckets are in sync.
//  3. For mismatching buckets, drill down with a longer prefix (p=12, 16...)
//     or switch to an IBLT for that bucket's elements.
type PrefixDigest struct {
	prefixLen uint32
	buckets   []Bucket
}

// NewPrefixDigest creates a new PrefixDigest with 2^prefixLen buckets.
// Typical starting values: prefixLen=8 (256 buckets, ~19 KB serialized).
func NewPrefixDigest(prefixLen uint32) *PrefixDigest {
	if prefixLen > 24 {
		prefixLen = 24 // Cap to avoid excessive memory: 2^24 = 16M buckets
	}
	numBuckets := uint32(1) << prefixLen
	buckets := make([]Bucket, numBuckets)
	for i := uint32(0); i < numBuckets; i++ {
		buckets[i].Index = i
	}
	return &PrefixDigest{
		prefixLen: prefixLen,
		buckets:   buckets,
	}
}

// PrefixLen returns the prefix length in bits.
func (pd *PrefixDigest) PrefixLen() uint32 {
	return pd.prefixLen
}

// NumBuckets returns the number of buckets (2^prefixLen).
func (pd *PrefixDigest) NumBuckets() uint32 {
	return uint32(len(pd.buckets))
}

// Insert adds a (KID, VID) pair to the appropriate bucket.
func (pd *PrefixDigest) Insert(kid, vid [32]byte) {
	idx := pd.bucketIndex(kid)
	b := &pd.buckets[idx]
	b.Count++

	kidHash := hashKIDOnly(kid)
	elemHash := hashElement(kid, vid)
	xor32(&b.XORKeyHash, kidHash)
	xor32(&b.XORValueHash, elemHash)
}

// Delete removes a (KID, VID) pair from the appropriate bucket.
func (pd *PrefixDigest) Delete(kid, vid [32]byte) {
	idx := pd.bucketIndex(kid)
	b := &pd.buckets[idx]
	b.Count--

	kidHash := hashKIDOnly(kid)
	elemHash := hashElement(kid, vid)
	xor32(&b.XORKeyHash, kidHash)
	xor32(&b.XORValueHash, elemHash)
}

// Bucket returns the bucket at the given index.
func (pd *PrefixDigest) Bucket(index uint32) *Bucket {
	if index >= uint32(len(pd.buckets)) {
		return nil
	}
	return &pd.buckets[index]
}

// Compare finds all bucket indices where the two digests differ.
// Both digests must have the same prefix length.
func (pd *PrefixDigest) Compare(other *PrefixDigest) ([]uint32, error) {
	if pd.prefixLen != other.prefixLen {
		return nil, fmt.Errorf("prefix_digest: prefix length mismatch: %d vs %d", pd.prefixLen, other.prefixLen)
	}

	var mismatched []uint32
	for i := uint32(0); i < pd.NumBuckets(); i++ {
		if !pd.buckets[i].Equal(&other.buckets[i]) {
			mismatched = append(mismatched, i)
		}
	}
	return mismatched, nil
}

// DrillDown creates a new PrefixDigest at a deeper prefix level,
// containing only elements that belong to the specified parent buckets
// at the current level. This is used for adaptive deepening.
//
// Note: DrillDown requires access to the original elements, so it
// returns a new empty PrefixDigest that the caller must populate.
// This is intentional: the caller iterates their storage, checks if
// each element falls into a mismatched parent bucket, and inserts it.
func DrillDown(newPrefixLen uint32) *PrefixDigest {
	return NewPrefixDigest(newPrefixLen)
}

// ParentBucket returns the parent bucket index for a KID at the given
// parent prefix length. This is useful during drill-down to filter
// elements by their parent bucket.
func ParentBucket(kid [32]byte, parentPrefixLen uint32) uint32 {
	return extractPrefix(kid, parentPrefixLen)
}

// BucketsForIndices returns a subset of buckets for the given indices.
// Useful for sending only the mismatched buckets over the wire.
func (pd *PrefixDigest) BucketsForIndices(indices []uint32) []Bucket {
	result := make([]Bucket, 0, len(indices))
	for _, idx := range indices {
		if idx < uint32(len(pd.buckets)) {
			result = append(result, pd.buckets[idx])
		}
	}
	return result
}

// Marshal serializes the PrefixDigest for network transport.
// Format: [4 bytes prefixLen][4 bytes numBuckets][buckets...]
func (pd *PrefixDigest) Marshal() []byte {
	numBuckets := pd.NumBuckets()
	buf := make([]byte, 8+int(numBuckets)*BucketDigestSize)
	binary.LittleEndian.PutUint32(buf[0:4], pd.prefixLen)
	binary.LittleEndian.PutUint32(buf[4:8], numBuckets)

	offset := 8
	for i := uint32(0); i < numBuckets; i++ {
		b := &pd.buckets[i]
		binary.LittleEndian.PutUint32(buf[offset:offset+4], b.Index)
		offset += 4
		binary.LittleEndian.PutUint64(buf[offset:offset+8], b.Count)
		offset += 8
		copy(buf[offset:offset+32], b.XORKeyHash[:])
		offset += 32
		copy(buf[offset:offset+32], b.XORValueHash[:])
		offset += 32
	}
	return buf
}

// MarshalBuckets serializes only the specified bucket indices. Useful
// for sending partial digests during adaptive drill-down.
func (pd *PrefixDigest) MarshalBuckets(indices []uint32) []byte {
	n := len(indices)
	buf := make([]byte, 8+n*BucketDigestSize)
	binary.LittleEndian.PutUint32(buf[0:4], pd.prefixLen)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(n))

	offset := 8
	for _, idx := range indices {
		if idx >= uint32(len(pd.buckets)) {
			continue
		}
		b := &pd.buckets[idx]
		binary.LittleEndian.PutUint32(buf[offset:offset+4], b.Index)
		offset += 4
		binary.LittleEndian.PutUint64(buf[offset:offset+8], b.Count)
		offset += 8
		copy(buf[offset:offset+32], b.XORKeyHash[:])
		offset += 32
		copy(buf[offset:offset+32], b.XORValueHash[:])
		offset += 32
	}
	return buf[:offset]
}

// UnmarshalPrefixDigest deserializes a PrefixDigest.
func UnmarshalPrefixDigest(data []byte) (*PrefixDigest, error) {
	if len(data) < 8 {
		return nil, errors.New("prefix_digest: data too short")
	}
	prefixLen := binary.LittleEndian.Uint32(data[0:4])
	numBuckets := binary.LittleEndian.Uint32(data[4:8])

	expected := 8 + int(numBuckets)*BucketDigestSize
	if len(data) != expected {
		return nil, fmt.Errorf("prefix_digest: expected %d bytes, got %d", expected, len(data))
	}

	pd := &PrefixDigest{
		prefixLen: prefixLen,
		buckets:   make([]Bucket, numBuckets),
	}

	offset := 8
	for i := uint32(0); i < numBuckets; i++ {
		b := &pd.buckets[i]
		b.Index = binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4
		b.Count = binary.LittleEndian.Uint64(data[offset : offset+8])
		offset += 8
		copy(b.XORKeyHash[:], data[offset:offset+32])
		offset += 32
		copy(b.XORValueHash[:], data[offset:offset+32])
		offset += 32
	}
	return pd, nil
}

// --- Internal helpers ---

// bucketIndex extracts the first prefixLen bits of the KID to determine
// which bucket an element belongs to.
func (pd *PrefixDigest) bucketIndex(kid [32]byte) uint32 {
	return extractPrefix(kid, pd.prefixLen)
}

// extractPrefix extracts the first prefixLen bits from a 32-byte hash
// and returns them as a uint32.
func extractPrefix(h [32]byte, prefixLen uint32) uint32 {
	if prefixLen == 0 {
		return 0
	}
	// Read the first 4 bytes as a big-endian uint32 (most significant
	// bits first), then shift right to keep only prefixLen bits.
	val := binary.BigEndian.Uint32(h[0:4])
	return val >> (32 - prefixLen)
}

// hashKIDOnly computes SHA-256(kid) for the XORKeyHash accumulator.
// This is separate from hashElement to allow detecting which keys
// differ even if only values changed.
func hashKIDOnly(kid [32]byte) [32]byte {
	return sha256.Sum256(kid[:])
}
