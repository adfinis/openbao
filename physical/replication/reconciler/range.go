// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package reconciler

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash/crc64"
	"math/big"
	"sort"

	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// RangePlanVersion identifies the current deterministic range planning
	// algorithm emitted in checkpoint responses.
	RangePlanVersion uint32 = 1

	defaultRangeTargetKeysPerRange = 12000
	defaultRangeTargetValueBytes   = 8 << 20 // 8 MiB
	defaultRangeMaxTopRanges       = 256
	defaultRangeMinKeysPerRange    = 512
)

// RangePlanConfig configures deterministic top-level range manifest planning.
type RangePlanConfig struct {
	TargetKeysPerRange       int
	TargetValueBytesPerRange uint64
	MaxTopRanges             int
	MinKeysPerRange          int
}

// DefaultRangePlanConfig returns balanced defaults suitable for production.
func DefaultRangePlanConfig() RangePlanConfig {
	return RangePlanConfig{
		TargetKeysPerRange:       defaultRangeTargetKeysPerRange,
		TargetValueBytesPerRange: defaultRangeTargetValueBytes,
		MaxTopRanges:             defaultRangeMaxTopRanges,
		MinKeysPerRange:          defaultRangeMinKeysPerRange,
	}
}

// RangeSpan defines an inclusive KID interval.
type RangeSpan struct {
	StartKID   [32]byte
	EndKID     [32]byte
	SplitDepth uint32
}

// Contains returns true if the KID is in this inclusive span.
func (s RangeSpan) Contains(kid [32]byte) bool {
	return bytes.Compare(kid[:], s.StartKID[:]) >= 0 &&
		bytes.Compare(kid[:], s.EndKID[:]) <= 0
}

// Valid reports whether the span bounds are valid.
func (s RangeSpan) Valid() bool {
	return bytes.Compare(s.StartKID[:], s.EndKID[:]) <= 0
}

// SplitRange splits an inclusive span into two child inclusive spans.
// ok=false when the span cannot be split further.
func SplitRange(span RangeSpan) (left RangeSpan, right RangeSpan, ok bool) {
	if !span.Valid() {
		return RangeSpan{}, RangeSpan{}, false
	}
	if bytes.Equal(span.StartKID[:], span.EndKID[:]) {
		return RangeSpan{}, RangeSpan{}, false
	}

	mid := midpoint(span.StartKID, span.EndKID)
	next, carry := incrementKID(mid)
	if carry || bytes.Compare(next[:], span.EndKID[:]) > 0 {
		return RangeSpan{}, RangeSpan{}, false
	}

	left = RangeSpan{
		StartKID:   span.StartKID,
		EndKID:     mid,
		SplitDepth: span.SplitDepth + 1,
	}
	right = RangeSpan{
		StartKID:   next,
		EndKID:     span.EndKID,
		SplitDepth: span.SplitDepth + 1,
	}
	return left, right, true
}

// RangeDescriptor summarizes a deterministic KID range in a checkpoint.
// It is the authoritative equality predicate for Phase B drill-down:
// two ranges are considered equal if and only if (Count, XORKeyHash,
// XORValueHash) all match. See buildRangeDescriptor for construction
// details and invariants.
type RangeDescriptor struct {
	Span             RangeSpan
	Count            uint64
	XORKeyHash       [32]byte
	XORValueHash     [32]byte
	ApproxValueBytes uint64
}

// EqualDigest compares two range descriptors by digest fields.
// This is the authoritative set-equality check for reconciliation.
func (d RangeDescriptor) EqualDigest(other RangeDescriptor) bool {
	return d.Count == other.Count &&
		d.XORKeyHash == other.XORKeyHash &&
		d.XORValueHash == other.XORValueHash
}

// RangeIndex is a checkpoint-local range index.
type RangeIndex struct {
	Ranges []RangeDescriptor
}

// RangeMapIndex provides sorted-KID iteration over a checkpoint-local
// KID->VID map for efficient repeated span operations.
type RangeMapIndex struct {
	sortedKids [][32]byte
	kidToVID   map[[32]byte][32]byte
	entries    map[[32]byte]*physical.Entry
}

// NewRangeMapIndex builds a sorted index for range-bounded operations.
func NewRangeMapIndex(kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry) *RangeMapIndex {
	keys := make([][32]byte, 0, len(kidToVID))
	for kid := range kidToVID {
		keys = append(keys, kid)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})
	return &RangeMapIndex{
		sortedKids: keys,
		kidToVID:   kidToVID,
		entries:    entries,
	}
}

// RangeKeys returns sorted keys inside the inclusive span.
func (i *RangeMapIndex) RangeKeys(span RangeSpan) [][32]byte {
	if i == nil || len(i.sortedKids) == 0 {
		return nil
	}
	start := sort.Search(len(i.sortedKids), func(idx int) bool {
		return bytes.Compare(i.sortedKids[idx][:], span.StartKID[:]) >= 0
	})
	end := sort.Search(len(i.sortedKids), func(idx int) bool {
		return bytes.Compare(i.sortedKids[idx][:], span.EndKID[:]) > 0
	})
	if start >= end || start >= len(i.sortedKids) {
		return nil
	}
	return i.sortedKids[start:end]
}

// BuildRangeManifest computes deterministic top-level ranges from a scanned
// reconciliation set.
func BuildRangeManifest(rs *ReconciliationSet, cfg RangePlanConfig) ([]RangeDescriptor, error) {
	if rs == nil {
		return nil, fmt.Errorf("range manifest: reconciliation set is nil")
	}
	return buildRangeManifestFromMaps(rs.KIDToVID, rs.Entries, cfg)
}

// BuildFixedHashRangeManifest computes deterministic top-level ranges from a
// compact metadata item list.
func BuildFixedHashRangeManifest(items []CheckpointItemMeta, cfg RangePlanConfig) ([]RangeDescriptor, error) {
	if len(items) == 0 {
		return nil, nil
	}
	kidToVID := make(map[[32]byte][32]byte, len(items))
	for _, item := range items {
		kidToVID[item.KID] = item.VID
	}
	return buildRangeManifestFromMaps(kidToVID, nil, cfg)
}

func buildRangeManifestFromMaps(kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry, cfg RangePlanConfig) ([]RangeDescriptor, error) {
	cfg = normalizeRangePlanConfig(cfg)
	if len(kidToVID) == 0 {
		return nil, nil
	}

	keys := make([][32]byte, 0, len(kidToVID))
	for kid := range kidToVID {
		keys = append(keys, kid)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	// Use deterministic hash-interval planning at the top-level for
	// checkpoint-to-checkpoint stability under skew.
	if cfg.MaxTopRanges <= 1 {
		return []RangeDescriptor{
			buildRangeDescriptor(keys, kidToVID, entries, 0),
		}, nil
	}

	bits := 8
	for bits > 1 && (1<<bits) > cfg.MaxTopRanges {
		bits--
	}
	if bits < 1 {
		bits = 1
	}
	bucketCount := 1 << bits
	ranges := make([]RangeDescriptor, 0, minInt(bucketCount, len(keys)))

	idx := 0
	for bucket := 0; bucket < bucketCount && idx < len(keys); bucket++ {
		start := idx
		for idx < len(keys) && kidPrefix(keys[idx], bits) == bucket {
			idx++
		}
		if start == idx {
			continue
		}
		desc := buildRangeDescriptor(keys[start:idx], kidToVID, entries, 0)
		ranges = append(ranges, desc)
	}

	if idx < len(keys) {
		// Defensive fallback if keys remain due an unexpected prefix calculation.
		desc := buildRangeDescriptor(keys[idx:], kidToVID, entries, 0)
		ranges = append(ranges, desc)
	}
	return ranges, nil
}

// BuildRangeDigest computes a range digest from a scanned reconciliation set.
func BuildRangeDigest(rs *ReconciliationSet, span RangeSpan) RangeDescriptor {
	return BuildRangeDigestFromMap(rs.KIDToVID, rs.Entries, span)
}

// BuildRangeDigestFromMap computes a range digest from KID/VID and optional entries.
func BuildRangeDigestFromMap(kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry, span RangeSpan) RangeDescriptor {
	index := NewRangeMapIndex(kidToVID, entries)
	return BuildRangeDigestFromIndex(index, span)
}

// BuildRangeDigestFromIndex computes a range digest using a pre-sorted index.
func BuildRangeDigestFromIndex(index *RangeMapIndex, span RangeSpan) RangeDescriptor {
	if index == nil {
		var empty RangeDescriptor
		empty.Span = span
		return empty
	}
	keys := index.RangeKeys(span)
	return buildRangeDescriptor(keys, index.kidToVID, index.entries, span.SplitDepth)
}

// ComputeRangeDigestFromItems computes a range digest from compact
// checkpoint metadata items.
func ComputeRangeDigestFromItems(items []CheckpointItemMeta, span RangeSpan) RangeDescriptor {
	kidToVID := make(map[[32]byte][32]byte, len(items))
	for _, item := range items {
		kidToVID[item.KID] = item.VID
	}
	return BuildRangeDigestFromMap(kidToVID, nil, span)
}

// RangeIDFromKID derives the 10-bit range ID from a KID.
func RangeIDFromKID(kid [32]byte) uint64 {
	return (uint64(kid[0]) << 2) | (uint64(kid[1]) >> 6)
}

// SpanFromRangeID returns the inclusive KID span for a given range ID.
func SpanFromRangeID(rangeID uint64) RangeSpan {
	var start, end [32]byte
	b0 := byte(rangeID >> 2)
	b1 := byte((rangeID & 3) << 6)
	start[0] = b0
	start[1] = b1

	end[0] = b0
	end[1] = b1 | 0x3F // Set lower 6 bits of byte 1 to 1s
	for i := 2; i < 32; i++ {
		end[i] = 0xFF
	}
	return RangeSpan{StartKID: start, EndKID: end}
}

// ComputeRangeChecksum computes a coarse CRC64-XOR checksum of (KID, VID)
// pairs in the range. This is a fast, non-cryptographic prefilter for
// Phase A reconciliation. A match (checksum + count equal) causes the
// range to be treated as converged without entering Phase B drill-down.
//
// The false-equality probability per range is bounded by 2^-64 (the
// CRC64 component; count must also match). This is acceptable for the
// non-Byzantine DR threat model.
//
// The checksum is order-independent (XOR is commutative), which is
// correct for set comparison: two sets with the same (KID, VID) pairs
// produce the same checksum regardless of iteration order.
//
// Invariant: assumes a canonical set where each KID appears at most
// once. Duplicate KIDs would cause XOR cancellation (even multiplicities
// become invisible).
func ComputeRangeChecksum(index *RangeMapIndex, rangeID uint64) (uint64, uint64) {
	if index == nil {
		return 0, 0
	}
	span := SpanFromRangeID(rangeID)
	keys := index.RangeKeys(span)
	if len(keys) == 0 {
		return 0, 0
	}

	crcTable := crc64.MakeTable(crc64.ISO)
	var checksum uint64

	for _, kid := range keys {
		vid, ok := index.kidToVID[kid]
		if !ok {
			continue
		}
		kSum := crc64.Checksum(kid[:], crcTable)
		vSum := crc64.Checksum(vid[:], crcTable)
		checksum ^= (kSum ^ vSum)
	}
	return checksum, uint64(len(keys))
}

// BuildRangePrefixDigest builds a prefix digest at a specific prefix length for
// a specific KID range from a reconciliation set.

func normalizeRangePlanConfig(cfg RangePlanConfig) RangePlanConfig {
	def := DefaultRangePlanConfig()
	if cfg.TargetKeysPerRange <= 0 {
		cfg.TargetKeysPerRange = def.TargetKeysPerRange
	}
	if cfg.TargetValueBytesPerRange == 0 {
		cfg.TargetValueBytesPerRange = def.TargetValueBytesPerRange
	}
	if cfg.MaxTopRanges <= 0 {
		cfg.MaxTopRanges = def.MaxTopRanges
	}
	if cfg.MinKeysPerRange <= 0 {
		cfg.MinKeysPerRange = def.MinKeysPerRange
	}
	if cfg.MinKeysPerRange > cfg.TargetKeysPerRange {
		cfg.MinKeysPerRange = cfg.TargetKeysPerRange
	}
	return cfg
}

func kidPrefix(kid [32]byte, bits int) int {
	if bits <= 0 {
		return 0
	}
	fullBytes := bits / 8
	remBits := bits % 8
	val := 0
	for i := 0; i < fullBytes && i < len(kid); i++ {
		val = (val << 8) | int(kid[i])
	}
	if remBits > 0 && fullBytes < len(kid) {
		val = (val << remBits) | int(kid[fullBytes]>>(8-remBits))
	}
	return val
}

// buildRangeDescriptor computes the authoritative RangeDescriptor for
// Phase B drill-down. The descriptor contains:
//
//   - count: number of items in the range
//   - XORKeyHash: XOR of SHA256(KID) for each item (256-bit)
//   - XORValueHash: XOR of SHA256(KID || VID) for each item (256-bit)
//
// This proves equality of the *set* of (KID, VID) pairs, not ordering.
// The false-equality probability is bounded by 2^-256, which is
// cryptographically negligible.
//
// Invariant: assumes a canonical set where each KID appears at most
// once per checkpoint. Duplicates would cause XOR cancellation.
// This is enforced by the scanner's KIDToVID map (last-writer-wins)
// and by immutable checkpoint artifacts being point-in-time snapshots.
func buildRangeDescriptor(keys [][32]byte, kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry, splitDepth uint32) RangeDescriptor {
	var desc RangeDescriptor
	if len(keys) == 0 {
		return desc
	}
	desc.Span = RangeSpan{
		StartKID:   keys[0],
		EndKID:     keys[len(keys)-1],
		SplitDepth: splitDepth,
	}
	desc.Count = uint64(len(keys))

	for _, kid := range keys {
		vid := kidToVID[kid]
		kidHash := sha256.Sum256(kid[:])
		xor32(&desc.XORKeyHash, kidHash)

		h := sha256.New()
		h.Write(kid[:])
		h.Write(vid[:])
		var elem [32]byte
		copy(elem[:], h.Sum(nil))
		xor32(&desc.XORValueHash, elem)

		if entries != nil {
			if e := entries[kid]; e != nil {
				desc.ApproxValueBytes += uint64(len(e.Value))
			}
		}
	}

	return desc
}

func midpoint(a, b [32]byte) [32]byte {
	var out [32]byte
	ai := new(big.Int).SetBytes(a[:])
	bi := new(big.Int).SetBytes(b[:])
	sum := new(big.Int).Add(ai, bi)
	sum.Rsh(sum, 1)
	buf := sum.Bytes()
	copy(out[32-len(buf):], buf)
	return out
}

func incrementKID(in [32]byte) (out [32]byte, carry bool) {
	out = in
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			return out, false
		}
	}
	return out, true
}

func xor32(dst *[32]byte, src [32]byte) {
	for i := 0; i < 32; i++ {
		dst[i] ^= src[i]
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
