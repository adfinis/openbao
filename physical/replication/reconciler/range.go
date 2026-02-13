// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package reconciler

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// RangePlanVersion identifies the current deterministic range planning
	// algorithm emitted in checkpoint responses.
	RangePlanVersion uint32 = 1

	defaultRangeTargetKeysPerRange   = 12000
	defaultRangeTargetValueBytes     = 8 << 20 // 8 MiB
	defaultRangeMaxTopRanges         = 256
	defaultRangeMinKeysPerRange      = 512
	defaultRangeMaxIBLTCellsPerRange = 32768
	defaultRangeTopLevelHashBits     = 8
)

// RangePlanConfig configures deterministic top-level range manifest planning.
type RangePlanConfig struct {
	TargetKeysPerRange       int
	TargetValueBytesPerRange uint64
	MaxTopRanges             int
	MinKeysPerRange          int
	MaxIBLTCellsPerRange     uint32
	TopLevelHashBits         int
}

// DefaultRangePlanConfig returns balanced defaults suitable for production.
func DefaultRangePlanConfig() RangePlanConfig {
	return RangePlanConfig{
		TargetKeysPerRange:       defaultRangeTargetKeysPerRange,
		TargetValueBytesPerRange: defaultRangeTargetValueBytes,
		MaxTopRanges:             defaultRangeMaxTopRanges,
		MinKeysPerRange:          defaultRangeMinKeysPerRange,
		MaxIBLTCellsPerRange:     defaultRangeMaxIBLTCellsPerRange,
		TopLevelHashBits:         defaultRangeTopLevelHashBits,
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
type RangeDescriptor struct {
	Span               RangeSpan
	Count              uint64
	XORKeyHash         [32]byte
	XORValueHash       [32]byte
	SuggestedIBLTCells uint32
	ApproxValueBytes   uint64
}

// EqualDigest compares two range descriptors by digest fields.
func (d RangeDescriptor) EqualDigest(other RangeDescriptor) bool {
	return d.Count == other.Count &&
		d.XORKeyHash == other.XORKeyHash &&
		d.XORValueHash == other.XORValueHash
}

// RangeIndex is a checkpoint-local range index.
type RangeIndex struct {
	Ranges []RangeDescriptor
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
			buildRangeDescriptor(keys, kidToVID, entries, cfg.MaxIBLTCellsPerRange, 0),
		}, nil
	}

	bits := cfg.TopLevelHashBits
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
		desc := buildRangeDescriptor(keys[start:idx], kidToVID, entries, cfg.MaxIBLTCellsPerRange, 0)
		ranges = append(ranges, desc)
	}

	if idx < len(keys) {
		// Defensive fallback if keys remain due an unexpected prefix calculation.
		desc := buildRangeDescriptor(keys[idx:], kidToVID, entries, cfg.MaxIBLTCellsPerRange, 0)
		ranges = append(ranges, desc)
	}
	return ranges, nil
}

// BuildRangeDigest computes a range digest from a scanned reconciliation set.
func BuildRangeDigest(rs *ReconciliationSet, span RangeSpan, maxIBLTCells uint32) RangeDescriptor {
	return BuildRangeDigestFromMap(rs.KIDToVID, rs.Entries, span, maxIBLTCells)
}

// BuildRangeDigestFromMap computes a range digest from KID/VID and optional entries.
func BuildRangeDigestFromMap(kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry, span RangeSpan, maxIBLTCells uint32) RangeDescriptor {
	keys := make([][32]byte, 0, len(kidToVID))
	for kid := range kidToVID {
		if span.Contains(kid) {
			keys = append(keys, kid)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})
	return buildRangeDescriptor(keys, kidToVID, entries, maxIBLTCells, span.SplitDepth)
}

// ComputeRangeDigestFromItems computes a range digest from compact
// checkpoint metadata items.
func ComputeRangeDigestFromItems(items []CheckpointItemMeta, span RangeSpan, maxIBLTCells uint32) RangeDescriptor {
	kidToVID := make(map[[32]byte][32]byte, len(items))
	for _, item := range items {
		kidToVID[item.KID] = item.VID
	}
	return BuildRangeDigestFromMap(kidToVID, nil, span, maxIBLTCells)
}

// BuildRangeIBLT builds an IBLT for a specific KID range from a reconciliation set.
func BuildRangeIBLT(rs *ReconciliationSet, span RangeSpan, numCells uint32) *sketch.IBLT {
	return BuildRangeIBLTFromMap(rs.KIDToVID, span, numCells)
}

// BuildRangeIBLTFromMap builds an IBLT for a specific KID range.
func BuildRangeIBLTFromMap(kidToVID map[[32]byte][32]byte, span RangeSpan, numCells uint32) *sketch.IBLT {
	if numCells < sketch.DefaultHashCount {
		numCells = sketch.DefaultHashCount
	}
	iblt := sketch.NewIBLT(numCells, sketch.DefaultHashCount)
	for kid, vid := range kidToVID {
		if span.Contains(kid) {
			iblt.Insert(kid, vid)
		}
	}
	return iblt
}

// BuildRangePrefixDigest builds a prefix digest at a specific prefix length for
// a specific KID range from a reconciliation set.
func BuildRangePrefixDigest(rs *ReconciliationSet, span RangeSpan, prefixLen uint32) *sketch.PrefixDigest {
	return BuildRangePrefixDigestFromMap(rs.KIDToVID, span, prefixLen)
}

// BuildRangePrefixDigestFromMap builds a prefix digest for a range.
func BuildRangePrefixDigestFromMap(kidToVID map[[32]byte][32]byte, span RangeSpan, prefixLen uint32) *sketch.PrefixDigest {
	pd := sketch.NewPrefixDigest(prefixLen)
	for kid, vid := range kidToVID {
		if span.Contains(kid) {
			pd.Insert(kid, vid)
		}
	}
	return pd
}

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
	if cfg.MaxIBLTCellsPerRange == 0 {
		cfg.MaxIBLTCellsPerRange = def.MaxIBLTCellsPerRange
	}
	if cfg.TopLevelHashBits <= 0 {
		cfg.TopLevelHashBits = def.TopLevelHashBits
	}
	if cfg.TopLevelHashBits > 24 {
		cfg.TopLevelHashBits = 24
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

func buildRangeDescriptor(keys [][32]byte, kidToVID map[[32]byte][32]byte, entries map[[32]byte]*physical.Entry, maxIBLTCells uint32, splitDepth uint32) RangeDescriptor {
	var desc RangeDescriptor
	if len(keys) == 0 {
		desc.SuggestedIBLTCells = sketch.DefaultHashCount
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

	// Conservative suggestion. This is a hint only; caller may clamp further.
	suggested := uint32(math.Ceil(float64(desc.Count) * 0.5))
	if suggested < sketch.DefaultHashCount {
		suggested = sketch.DefaultHashCount
	}
	if maxIBLTCells > 0 && suggested > maxIBLTCells {
		suggested = maxIBLTCells
	}
	desc.SuggestedIBLTCells = suggested
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
