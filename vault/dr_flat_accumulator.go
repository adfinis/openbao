// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"sync"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

var drFlatAccumulatorCRCTable = crc64.MakeTable(crc64.ISO)

const (
	drFlatAccumulatorStoragePath       = "core/cluster/local/dr/flat-accumulator"
	drFlatAccumulatorSnapshotVersion   = 1
	drFlatAccumulatorRangeBits         = 10
	drFlatAccumulatorChecksumAlgorithm = "crc64-iso-xor-kid-vid-v1"
)

type drFlatAccumulatorBucket struct {
	checksum uint64
	count    uint64
}

type drFlatAccumulatorPersistedBucket struct {
	Checksum uint64 `json:"checksum"`
	Count    uint64 `json:"count"`
}

type drFlatAccumulatorPersistedSnapshot struct {
	Version           int                                `json:"version"`
	RelationshipID    string                             `json:"relationship_id"`
	ClusterID         string                             `json:"cluster_id,omitempty"`
	RangeBits         int                                `json:"range_bits"`
	RangeCount        int                                `json:"range_count"`
	ChecksumAlgorithm string                             `json:"checksum_algorithm"`
	CommitIndex       uint64                             `json:"commit_index"`
	Buckets           []drFlatAccumulatorPersistedBucket `json:"buckets"`
}

type drFlatAccumulatorDelta struct {
	kid [32]byte

	oldExists bool
	oldVID    [32]byte
	newExists bool
	newVID    [32]byte
}

type drFlatAccumulatorPendingState struct {
	exists bool
	vid    [32]byte
}

type drFlatRangeAccumulator struct {
	mu          sync.RWMutex
	initialized bool
	index       uint64
	buckets     [drRangeMaxTotalRanges]drFlatAccumulatorBucket
}

func newDRFlatRangeAccumulator() *drFlatRangeAccumulator {
	return &drFlatRangeAccumulator{}
}

func (a *drFlatRangeAccumulator) isInitialized() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.initialized
}

func (a *drFlatRangeAccumulator) resetFromSet(rs *reconciler.ReconciliationSet, index uint64) {
	if a == nil || rs == nil {
		return
	}

	buckets := drFlatAccumulatorBucketsFromSet(rs)
	a.mu.Lock()
	a.buckets = buckets
	a.index = index
	a.initialized = true
	a.mu.Unlock()
}

func (a *drFlatRangeAccumulator) invalidate() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.initialized = false
	a.index = 0
	a.buckets = [drRangeMaxTotalRanges]drFlatAccumulatorBucket{}
	a.mu.Unlock()
}

func (a *drFlatRangeAccumulator) snapshotExact(index uint64) ([drRangeMaxTotalRanges]drFlatAccumulatorBucket, bool) {
	if a == nil {
		return [drRangeMaxTotalRanges]drFlatAccumulatorBucket{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.initialized || a.index != index {
		return [drRangeMaxTotalRanges]drFlatAccumulatorBucket{}, false
	}
	return a.buckets, true
}

func (a *drFlatRangeAccumulator) snapshot() (uint64, [drRangeMaxTotalRanges]drFlatAccumulatorBucket, bool) {
	if a == nil {
		return 0, [drRangeMaxTotalRanges]drFlatAccumulatorBucket{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.initialized {
		return 0, [drRangeMaxTotalRanges]drFlatAccumulatorBucket{}, false
	}
	return a.index, a.buckets, true
}

func (a *drFlatRangeAccumulator) replace(index uint64, buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.buckets = buckets
	a.index = index
	a.initialized = true
	a.mu.Unlock()
}

func (a *drFlatRangeAccumulator) advanceIndex(index uint64) {
	if a == nil || index == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return
	}
	if index > a.index {
		a.index = index
	}
}

func (a *drFlatRangeAccumulator) applyDeltas(index uint64, deltas []drFlatAccumulatorDelta) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return false
	}

	if !applyFlatAccumulatorDeltasToBuckets(&a.buckets, deltas) {
		a.initialized = false
		return false
	}

	if index > a.index {
		a.index = index
	}
	return true
}

func drFlatAccumulatorBucketsFromSet(rs *reconciler.ReconciliationSet) [drRangeMaxTotalRanges]drFlatAccumulatorBucket {
	var buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	if rs == nil {
		return buckets
	}
	for kid, vid := range rs.KIDToVID {
		rangeID := reconciler.RangeIDFromKID(kid)
		if rangeID >= uint64(len(buckets)) {
			continue
		}
		addFlatAccumulatorContribution(&buckets[rangeID], kid, vid)
	}
	return buckets
}

func applyFlatAccumulatorDeltasToBuckets(buckets *[drRangeMaxTotalRanges]drFlatAccumulatorBucket, deltas []drFlatAccumulatorDelta) bool {
	if buckets == nil {
		return false
	}
	for _, delta := range deltas {
		rangeID := reconciler.RangeIDFromKID(delta.kid)
		if rangeID >= uint64(len(buckets)) {
			return false
		}
		bucket := &buckets[rangeID]
		if delta.oldExists {
			if bucket.count == 0 {
				return false
			}
			removeFlatAccumulatorContribution(bucket, delta.kid, delta.oldVID)
		}
		if delta.newExists {
			addFlatAccumulatorContribution(bucket, delta.kid, delta.newVID)
		}
	}
	return true
}

func (s *drReplicationSecondary) flatAccumulatorClusterID() string {
	if s == nil || s.core == nil || s.core.drManager == nil {
		return ""
	}
	cfg := s.core.drManager.Config()
	return cfg.ClusterID
}

func (s *drReplicationSecondary) persistFlatAccumulatorSnapshot(
	ctx context.Context,
	writer physical.Backend,
	index uint64,
	buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket,
) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	snapshot := drFlatAccumulatorPersistedSnapshot{
		Version:           drFlatAccumulatorSnapshotVersion,
		RelationshipID:    s.relationshipID,
		ClusterID:         s.flatAccumulatorClusterID(),
		RangeBits:         drFlatAccumulatorRangeBits,
		RangeCount:        drRangeMaxTotalRanges,
		ChecksumAlgorithm: drFlatAccumulatorChecksumAlgorithm,
		CommitIndex:       index,
		Buckets:           make([]drFlatAccumulatorPersistedBucket, drRangeMaxTotalRanges),
	}
	for i, bucket := range buckets {
		snapshot.Buckets[i] = drFlatAccumulatorPersistedBucket{
			Checksum: bucket.checksum,
			Count:    bucket.count,
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drFlatAccumulatorStoragePath,
		Value: data,
	})
}

func (s *drReplicationSecondary) deletePersistedFlatAccumulator(ctx context.Context, writer physical.Backend) error {
	if s == nil || writer == nil {
		return nil
	}
	return writer.Delete(ctx, drFlatAccumulatorStoragePath)
}

func (s *drReplicationSecondary) resetAndPersistFlatAccumulatorFromSet(ctx context.Context, rs *reconciler.ReconciliationSet, index uint64) error {
	if s == nil || s.rangeAccumulator == nil || rs == nil || index == 0 {
		return nil
	}
	buckets := drFlatAccumulatorBucketsFromSet(rs)
	if err := s.persistFlatAccumulatorSnapshot(ctx, s.core.physical, index, buckets); err != nil {
		return err
	}
	s.rangeAccumulator.replace(index, buckets)
	return nil
}

func (s *drReplicationSecondary) loadPersistentFlatAccumulator(ctx context.Context) {
	if s == nil || s.rangeAccumulator == nil || s.core == nil || s.core.physical == nil {
		return
	}
	entry, err := s.core.physical.Get(ctx, drFlatAccumulatorStoragePath)
	if err != nil {
		s.logger.Warn("failed to load DR flat accumulator snapshot", "error", err)
		return
	}
	if entry == nil || len(entry.Value) == 0 {
		return
	}

	var snapshot drFlatAccumulatorPersistedSnapshot
	if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
		s.logger.Warn("discarding unreadable DR flat accumulator snapshot", "error", err)
		_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
		return
	}
	if err := s.validateFlatAccumulatorSnapshot(&snapshot); err != nil {
		s.logger.Warn("discarding incompatible DR flat accumulator snapshot", "error", err)
		_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
		return
	}

	var buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	for i, bucket := range snapshot.Buckets {
		buckets[i] = drFlatAccumulatorBucket{
			checksum: bucket.Checksum,
			count:    bucket.Count,
		}
	}
	s.rangeAccumulator.replace(snapshot.CommitIndex, buckets)
	if snapshot.CommitIndex > s.lastAppliedIndex.Load() {
		s.setLastAppliedIndex(snapshot.CommitIndex)
	}
	s.logger.Info("loaded DR flat accumulator snapshot", "commit_index", snapshot.CommitIndex)
}

func (s *drReplicationSecondary) validateFlatAccumulatorSnapshot(snapshot *drFlatAccumulatorPersistedSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("snapshot is nil")
	}
	if snapshot.Version != drFlatAccumulatorSnapshotVersion {
		return fmt.Errorf("unsupported version %d", snapshot.Version)
	}
	if snapshot.RelationshipID == "" || snapshot.RelationshipID != s.relationshipID {
		return fmt.Errorf("relationship_id mismatch")
	}
	if clusterID := s.flatAccumulatorClusterID(); clusterID != "" && snapshot.ClusterID != "" && snapshot.ClusterID != clusterID {
		return fmt.Errorf("cluster_id mismatch")
	}
	if snapshot.RangeBits != drFlatAccumulatorRangeBits {
		return fmt.Errorf("range_bits mismatch")
	}
	if snapshot.RangeCount != drRangeMaxTotalRanges || len(snapshot.Buckets) != drRangeMaxTotalRanges {
		return fmt.Errorf("range_count mismatch")
	}
	if snapshot.ChecksumAlgorithm != drFlatAccumulatorChecksumAlgorithm {
		return fmt.Errorf("checksum_algorithm mismatch")
	}
	if snapshot.CommitIndex == 0 {
		return fmt.Errorf("commit_index is zero")
	}
	return nil
}

func (s *drReplicationSecondary) accumulatorDeltaForChange(
	ctx context.Context,
	reader physical.Backend,
	change *EntryChange,
	keyOverride string,
	pending map[string]drFlatAccumulatorPendingState,
) (drFlatAccumulatorDelta, bool, error) {
	var delta drFlatAccumulatorDelta
	if s == nil || s.rangeAccumulator == nil || !s.rangeAccumulator.isInitialized() {
		return delta, false, nil
	}
	if reader == nil {
		return delta, false, fmt.Errorf("storage reader unavailable")
	}
	if change == nil {
		return delta, false, nil
	}

	key := keyOverride
	if key == "" {
		key = change.Key
	}
	if key == "" || isDRNeverReplicatePath(key) {
		return delta, false, nil
	}

	op := physical.Operation(change.OpType)
	if op != physical.PutOperation && op != physical.DeleteOperation {
		return delta, false, nil
	}

	kid := s.scanner.ComputeKID(key)
	delta.kid = kid

	old, ok := pending[key]
	if !ok {
		entry, err := reader.Get(ctx, key)
		if err != nil {
			return delta, false, err
		}
		if entry != nil {
			old.exists = true
			old.vid = s.scanner.ComputeVIDWithSealWrap(entry.Value, entry.SealWrap)
		}
	}
	if old.exists {
		delta.oldExists = true
		delta.oldVID = old.vid
	}

	switch op {
	case physical.PutOperation:
		delta.newExists = true
		delta.newVID = s.scanner.ComputeVIDWithSealWrap(change.Value, change.SealWrap)
		pending[key] = drFlatAccumulatorPendingState{exists: true, vid: delta.newVID}
	case physical.DeleteOperation:
		pending[key] = drFlatAccumulatorPendingState{}
	}

	return delta, delta.oldExists || delta.newExists, nil
}

func addFlatAccumulatorContribution(bucket *drFlatAccumulatorBucket, kid, vid [32]byte) {
	bucket.count++
	bucket.checksum ^= flatAccumulatorContribution(kid, vid)
}

func removeFlatAccumulatorContribution(bucket *drFlatAccumulatorBucket, kid, vid [32]byte) {
	bucket.count--
	bucket.checksum ^= flatAccumulatorContribution(kid, vid)
}

func flatAccumulatorContribution(kid, vid [32]byte) uint64 {
	kSum := crc64.Checksum(kid[:], drFlatAccumulatorCRCTable)
	vSum := crc64.Checksum(vid[:], drFlatAccumulatorCRCTable)
	return kSum ^ vSum
}
