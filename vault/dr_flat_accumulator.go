// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"sync"
	"time"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

var drFlatAccumulatorCRCTable = crc64.MakeTable(crc64.ISO)

const (
	drFlatAccumulatorStoragePath       = "core/cluster/local/dr/flat-accumulator"
	drFlatAccumulatorCursorStoragePath = "core/cluster/local/dr/flat-accumulator-cursor"
	drFlatAccumulatorSnapshotVersion   = 1
	drFlatAccumulatorCursorVersion     = 1
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

type drFlatAccumulatorPersistedCursor struct {
	Version             int    `json:"version"`
	RelationshipID      string `json:"relationship_id"`
	ClusterID           string `json:"cluster_id,omitempty"`
	CommitIndex         uint64 `json:"commit_index"`
	SnapshotCommitIndex uint64 `json:"snapshot_commit_index,omitempty"`
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
	start := time.Now()
	err = writer.Put(ctx, &physical.Entry{
		Key:   drFlatAccumulatorStoragePath,
		Value: data,
	})
	if err == nil {
		s.recordFlatAccumulatorSnapshotPersist(index, len(data), time.Since(start))
	}
	return err
}

func (s *drReplicationSecondary) recordFlatAccumulatorSnapshotPersist(index uint64, bytes int, duration time.Duration) {
	if s == nil || bytes < 0 {
		return
	}
	byteCount := uint64(bytes)
	nanos := durationNanos(duration)

	s.flatAccumulatorSnapshotCount.Add(1)
	s.flatAccumulatorSnapshotIndex.Store(index)
	s.flatAccumulatorSnapshotLastAt.Store(time.Now().UnixNano())
	s.flatAccumulatorSnapshotBytes.Add(byteCount)
	s.flatAccumulatorSnapshotLast.Store(byteCount)
	s.flatAccumulatorSnapshotNanos.Add(nanos)
	atomicMaxUint64(&s.flatAccumulatorSnapshotMaxNs, nanos)

	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_snapshot_persists_total"}, 1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_snapshot_bytes_total"}, float32(byteCount))
	metrics.SetGauge([]string{"replication", "dr", "secondary", "flat_accumulator_snapshot_bytes_last"}, float32(byteCount))
	metrics.MeasureSince([]string{"replication", "dr", "secondary", "flat_accumulator_snapshot_persist_duration"}, time.Now().Add(-duration))
}

func (s *drReplicationSecondary) persistFlatAccumulatorCursor(
	ctx context.Context,
	writer physical.Backend,
	index uint64,
	snapshotIndex uint64,
) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	cursor := drFlatAccumulatorPersistedCursor{
		Version:             drFlatAccumulatorCursorVersion,
		RelationshipID:      s.relationshipID,
		ClusterID:           s.flatAccumulatorClusterID(),
		CommitIndex:         index,
		SnapshotCommitIndex: snapshotIndex,
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	if err := writer.Put(ctx, &physical.Entry{
		Key:   drFlatAccumulatorCursorStoragePath,
		Value: data,
	}); err != nil {
		return err
	}
	s.flatAccumulatorCursorWrites.Add(1)
	s.flatAccumulatorCursorIndex.Store(index)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_cursor_writes_total"}, 1)
	metrics.SetGauge([]string{"replication", "dr", "secondary", "flat_accumulator_cursor_index"}, float32(index))
	return nil
}

func (s *drReplicationSecondary) shouldPersistFlatAccumulatorSnapshot(index uint64, force bool) bool {
	if s == nil || index == 0 {
		return false
	}
	if force {
		return true
	}
	lastIndex := s.flatAccumulatorSnapshotIndex.Load()
	if lastIndex == 0 || index <= lastIndex {
		return true
	}
	if minEntries := s.flatAccumulatorSnapshotMinEntries; minEntries > 0 && index-lastIndex >= minEntries {
		return true
	}
	if minInterval := s.flatAccumulatorSnapshotMinInterval; minInterval > 0 {
		lastAt := s.flatAccumulatorSnapshotLastAt.Load()
		if lastAt == 0 || time.Since(time.Unix(0, lastAt)) >= minInterval {
			return true
		}
	}
	return false
}

func (s *drReplicationSecondary) persistFlatAccumulatorState(
	ctx context.Context,
	writer physical.Backend,
	index uint64,
	buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket,
	forceSnapshot bool,
) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	snapshotIndex := s.flatAccumulatorSnapshotIndex.Load()
	if s.shouldPersistFlatAccumulatorSnapshot(index, forceSnapshot) {
		if err := s.persistFlatAccumulatorSnapshot(ctx, writer, index, buckets); err != nil {
			return err
		}
		snapshotIndex = index
	} else {
		s.flatAccumulatorSnapshotSkipped.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_snapshot_skipped_total"}, 1)
	}
	return s.persistFlatAccumulatorCursor(ctx, writer, index, snapshotIndex)
}

func (s *drReplicationSecondary) persistCurrentFlatAccumulatorSnapshot(ctx context.Context) {
	if s == nil || s.rangeAccumulator == nil || s.core == nil || s.core.physical == nil {
		return
	}
	index := s.lastAppliedIndex.Load()
	if index == 0 {
		return
	}
	buckets, ok := s.rangeAccumulator.snapshotExact(index)
	if !ok {
		return
	}
	if err := s.persistFlatAccumulatorState(ctx, s.core.physical, index, buckets, true); err != nil {
		s.logger.Warn("failed to persist final DR flat accumulator snapshot", "commit_index", index, "error", err)
	}
}

func (s *drReplicationSecondary) deletePersistedFlatAccumulator(ctx context.Context, writer physical.Backend) error {
	if s == nil || writer == nil {
		return nil
	}
	if err := writer.Delete(ctx, drFlatAccumulatorStoragePath); err != nil {
		return err
	}
	s.flatAccumulatorSnapshotIndex.Store(0)
	return nil
}

func (s *drReplicationSecondary) deletePersistedFlatAccumulatorState(ctx context.Context, writer physical.Backend) error {
	if err := s.deletePersistedFlatAccumulator(ctx, writer); err != nil {
		return err
	}
	if s == nil || writer == nil {
		return nil
	}
	if err := writer.Delete(ctx, drFlatAccumulatorCursorStoragePath); err != nil {
		return err
	}
	s.flatAccumulatorCursorIndex.Store(0)
	s.flatAccumulatorSnapshotIndex.Store(0)
	return nil
}

func (s *drReplicationSecondary) resetAndPersistFlatAccumulatorFromSet(ctx context.Context, rs *reconciler.ReconciliationSet, index uint64) error {
	if s == nil || s.rangeAccumulator == nil || rs == nil || index == 0 {
		return nil
	}
	buckets := drFlatAccumulatorBucketsFromSet(rs)
	if err := s.persistFlatAccumulatorSnapshot(ctx, s.core.physical, index, buckets); err != nil {
		return err
	}
	if err := s.persistFlatAccumulatorCursor(ctx, s.core.physical, index, index); err != nil {
		return err
	}
	s.rangeAccumulator.replace(index, buckets)
	return nil
}

func (s *drReplicationSecondary) loadPersistentFlatAccumulator(ctx context.Context) {
	if s == nil || s.rangeAccumulator == nil || s.core == nil || s.core.physical == nil {
		return
	}
	cursor, cursorOK := s.loadPersistentFlatAccumulatorCursor(ctx)
	entry, err := s.core.physical.Get(ctx, drFlatAccumulatorStoragePath)
	if err != nil {
		s.logger.Warn("failed to load DR flat accumulator snapshot", "error", err)
		if cursorOK {
			s.setLastAppliedIndex(cursor.CommitIndex)
		}
		return
	}
	if entry == nil || len(entry.Value) == 0 {
		if cursorOK {
			s.setLastAppliedIndex(cursor.CommitIndex)
			s.logger.Info("loaded DR flat accumulator cursor without snapshot", "commit_index", cursor.CommitIndex)
		}
		return
	}

	var snapshot drFlatAccumulatorPersistedSnapshot
	if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
		s.logger.Warn("discarding unreadable DR flat accumulator snapshot", "error", err)
		_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
		if cursorOK {
			s.setLastAppliedIndex(cursor.CommitIndex)
		}
		return
	}
	if err := s.validateFlatAccumulatorSnapshot(&snapshot); err != nil {
		s.logger.Warn("discarding incompatible DR flat accumulator snapshot", "error", err)
		_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
		if cursorOK {
			s.setLastAppliedIndex(cursor.CommitIndex)
		}
		return
	}
	if cursorOK && cursor.CommitIndex > snapshot.CommitIndex {
		s.logger.Info("discarding stale DR flat accumulator snapshot behind cursor",
			"snapshot_commit_index", snapshot.CommitIndex,
			"cursor_commit_index", cursor.CommitIndex)
		_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
		s.setLastAppliedIndex(cursor.CommitIndex)
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
	s.flatAccumulatorSnapshotIndex.Store(snapshot.CommitIndex)
	s.flatAccumulatorSnapshotLastAt.Store(time.Now().UnixNano())
	if snapshot.CommitIndex > s.lastAppliedIndex.Load() {
		s.setLastAppliedIndex(snapshot.CommitIndex)
	}
	s.logger.Info("loaded DR flat accumulator snapshot", "commit_index", snapshot.CommitIndex)
}

func (s *drReplicationSecondary) loadPersistentFlatAccumulatorCursor(ctx context.Context) (drFlatAccumulatorPersistedCursor, bool) {
	var cursor drFlatAccumulatorPersistedCursor
	if s == nil || s.core == nil || s.core.physical == nil {
		return cursor, false
	}
	entry, err := s.core.physical.Get(ctx, drFlatAccumulatorCursorStoragePath)
	if err != nil {
		s.logger.Warn("failed to load DR flat accumulator cursor", "error", err)
		return cursor, false
	}
	if entry == nil || len(entry.Value) == 0 {
		return cursor, false
	}
	if err := json.Unmarshal(entry.Value, &cursor); err != nil {
		s.logger.Warn("discarding unreadable DR flat accumulator cursor", "error", err)
		_ = s.core.physical.Delete(ctx, drFlatAccumulatorCursorStoragePath)
		return cursor, false
	}
	if err := s.validateFlatAccumulatorCursor(&cursor); err != nil {
		s.logger.Warn("discarding incompatible DR flat accumulator cursor", "error", err)
		_ = s.core.physical.Delete(ctx, drFlatAccumulatorCursorStoragePath)
		return cursor, false
	}
	s.flatAccumulatorCursorIndex.Store(cursor.CommitIndex)
	return cursor, true
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

func (s *drReplicationSecondary) validateFlatAccumulatorCursor(cursor *drFlatAccumulatorPersistedCursor) error {
	if cursor == nil {
		return fmt.Errorf("cursor is nil")
	}
	if cursor.Version != drFlatAccumulatorCursorVersion {
		return fmt.Errorf("unsupported cursor version %d", cursor.Version)
	}
	if cursor.RelationshipID == "" || cursor.RelationshipID != s.relationshipID {
		return fmt.Errorf("relationship_id mismatch")
	}
	if clusterID := s.flatAccumulatorClusterID(); clusterID != "" && cursor.ClusterID != "" && cursor.ClusterID != clusterID {
		return fmt.Errorf("cluster_id mismatch")
	}
	if cursor.CommitIndex == 0 {
		return fmt.Errorf("commit_index is zero")
	}
	if cursor.SnapshotCommitIndex > cursor.CommitIndex {
		return fmt.Errorf("snapshot_commit_index is ahead of commit_index")
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
