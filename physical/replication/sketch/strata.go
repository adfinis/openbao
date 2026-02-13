// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package sketch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

const (
	// DefaultStrataLevels is the number of IBLT strata. Each level
	// catches elements whose hash has exactly that many trailing zeros.
	// 32 levels covers differences up to ~2^32.
	DefaultStrataLevels = 32

	// DefaultStrataCells is the number of cells per stratum IBLT.
	// 80 is the value recommended by Eppstein et al. (SIGCOMM 2011).
	DefaultStrataCells = 80
)

// StrataEstimator estimates the number of differing elements between
// two sets using layered IBLTs. Each stratum captures elements whose
// KID hash has a specific number of trailing zero bits.
//
// To estimate: subtract two StrataEstimators level-by-level, then
// decode from the highest level down. When decoding fails at level i,
// the estimated difference is count * 2^i, where count is the number
// of elements decoded so far.
//
// Reference: Eppstein et al., "What's the Difference?" (SIGCOMM 2011)
type StrataEstimator struct {
	levels    int
	cellsPer  uint32
	hashCount uint32
	strata    []*IBLT
}

// NewStrataEstimator creates a new estimator with the given number of
// levels and cells per level.
func NewStrataEstimator(levels int, cellsPerLevel, hashCount uint32) *StrataEstimator {
	if levels <= 0 {
		levels = DefaultStrataLevels
	}
	if cellsPerLevel == 0 {
		cellsPerLevel = DefaultStrataCells
	}
	if hashCount == 0 {
		hashCount = DefaultHashCount
	}

	se := &StrataEstimator{
		levels:    levels,
		cellsPer:  cellsPerLevel,
		hashCount: hashCount,
		strata:    make([]*IBLT, levels),
	}
	for i := 0; i < levels; i++ {
		se.strata[i] = NewIBLT(cellsPerLevel, hashCount)
	}
	return se
}

// NewDefaultStrataEstimator creates an estimator with default parameters.
func NewDefaultStrataEstimator() *StrataEstimator {
	return NewStrataEstimator(DefaultStrataLevels, DefaultStrataCells, DefaultHashCount)
}

// Levels returns the number of strata levels.
func (se *StrataEstimator) Levels() int {
	return se.levels
}

// Insert adds a (KID, VID) pair into the appropriate stratum.
// The stratum is determined by the number of trailing zero bits in
// the element hash.
func (se *StrataEstimator) Insert(kid, vid [32]byte) {
	level := se.stratum(kid, vid)
	se.strata[level].Insert(kid, vid)
}

// Delete removes a (KID, VID) pair from the appropriate stratum.
func (se *StrataEstimator) Delete(kid, vid [32]byte) {
	level := se.stratum(kid, vid)
	se.strata[level].Delete(kid, vid)
}

// Estimate computes the estimated number of set differences by
// subtracting the other estimator and decoding strata from highest
// to lowest.
func (se *StrataEstimator) Estimate(other *StrataEstimator) (int, error) {
	if se.levels != other.levels {
		return 0, fmt.Errorf("strata: level count mismatch: %d vs %d", se.levels, other.levels)
	}

	count := 0
	for i := se.levels - 1; i >= 0; i-- {
		diff, err := se.strata[i].Subtract(other.strata[i])
		if err != nil {
			return 0, fmt.Errorf("strata: subtract failed at level %d: %w", i, err)
		}

		added, removed, ok := diff.Decode()
		if !ok {
			// Decode failed at this level. Estimate = count * 2^(i+1).
			// The +1 accounts for the failed level containing roughly
			// as many elements as we've decoded so far.
			if count == 0 {
				// If we haven't decoded anything yet, this means the
				// difference is very large. Return a rough lower bound.
				return 1 << uint(i+1), nil
			}
			return count * (1 << uint(i+1)), nil
		}
		count += len(added) + len(removed)
	}

	// All levels decoded successfully; count is exact.
	return count, nil
}

// Marshal serializes the StrataEstimator for network transport.
// Format: [4 bytes levels][4 bytes cellsPer][4 bytes hashCount][strata IBLTs...]
func (se *StrataEstimator) Marshal() []byte {
	// Compute size of each stratum IBLT.
	ibltSize := se.strata[0].Marshal()
	perIBLT := len(ibltSize)

	buf := make([]byte, 12+se.levels*perIBLT)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(se.levels))
	binary.LittleEndian.PutUint32(buf[4:8], se.cellsPer)
	binary.LittleEndian.PutUint32(buf[8:12], se.hashCount)

	// First stratum is already marshaled.
	copy(buf[12:12+perIBLT], ibltSize)
	offset := 12 + perIBLT

	for i := 1; i < se.levels; i++ {
		data := se.strata[i].Marshal()
		copy(buf[offset:offset+perIBLT], data)
		offset += perIBLT
	}
	return buf
}

// UnmarshalStrataEstimator deserializes a StrataEstimator.
func UnmarshalStrataEstimator(data []byte) (*StrataEstimator, error) {
	if len(data) < 12 {
		return nil, errors.New("strata: data too short for header")
	}
	levels := int(binary.LittleEndian.Uint32(data[0:4]))
	cellsPer := binary.LittleEndian.Uint32(data[4:8])
	hashCount := binary.LittleEndian.Uint32(data[8:12])

	if levels <= 0 || levels > 64 {
		return nil, fmt.Errorf("strata: invalid level count: %d", levels)
	}

	// Compute expected per-IBLT size. We need to create a temp IBLT
	// to get the actual numCells (after rounding).
	tmpIBLT := NewIBLT(cellsPer, hashCount)
	perIBLT := 8 + int(tmpIBLT.NumCells())*cellSize

	expected := 12 + levels*perIBLT
	if len(data) != expected {
		return nil, fmt.Errorf("strata: expected %d bytes, got %d", expected, len(data))
	}

	se := &StrataEstimator{
		levels:    levels,
		cellsPer:  cellsPer,
		hashCount: hashCount,
		strata:    make([]*IBLT, levels),
	}

	offset := 12
	for i := 0; i < levels; i++ {
		iblt, err := UnmarshalIBLT(data[offset : offset+perIBLT])
		if err != nil {
			return nil, fmt.Errorf("strata: unmarshal IBLT at level %d: %w", i, err)
		}
		se.strata[i] = iblt
		offset += perIBLT
	}
	return se, nil
}

// stratum returns the level index for a given element.
// Level = number of trailing zero bits in the element hash, capped
// at levels-1.
func (se *StrataEstimator) stratum(kid, vid [32]byte) int {
	h := hashElement(kid, vid)
	// Use the first 8 bytes as a uint64 for trailing zero counting.
	val := binary.LittleEndian.Uint64(h[0:8])
	if val == 0 {
		return se.levels - 1
	}
	tz := bits.TrailingZeros64(val)
	if tz >= se.levels {
		return se.levels - 1
	}
	return tz
}
