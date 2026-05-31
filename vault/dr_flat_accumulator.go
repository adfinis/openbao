// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"sort"
	"strconv"
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
	drFlatAccumulatorDeltaStoragePath  = "core/cluster/local/dr/flat-accumulator-deltas/"
	drStreamAppliedIndexStoragePath    = "core/cluster/local/dr/stream-applied-index"
	drFlatAccumulatorSnapshotVersion   = 1
	drFlatAccumulatorCursorVersion     = 1
	drFlatAccumulatorDeltaVersion      = 1
	drStreamAppliedIndexVersion        = 1
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

type drFlatAccumulatorPersistedDeltaBatch struct {
	Version             int                                   `json:"version"`
	RelationshipID      string                                `json:"relationship_id"`
	ClusterID           string                                `json:"cluster_id,omitempty"`
	RangeBits           int                                   `json:"range_bits"`
	ChecksumAlgorithm   string                                `json:"checksum_algorithm"`
	CommitIndex         uint64                                `json:"commit_index"`
	SnapshotCommitIndex uint64                                `json:"snapshot_commit_index,omitempty"`
	Deltas              []drFlatAccumulatorPersistedDeltaItem `json:"deltas,omitempty"`
}

type drFlatAccumulatorPersistedDeltaItem struct {
	KID       []byte `json:"kid"`
	OldExists bool   `json:"old_exists,omitempty"`
	OldVID    []byte `json:"old_vid,omitempty"`
	NewExists bool   `json:"new_exists,omitempty"`
	NewVID    []byte `json:"new_vid,omitempty"`
}

type drPersistedStreamAppliedIndex struct {
	Version        int    `json:"version"`
	RelationshipID string `json:"relationship_id"`
	ClusterID      string `json:"cluster_id,omitempty"`
	CommitIndex    uint64 `json:"commit_index"`
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

func (s *drReplicationSecondary) persistStreamAppliedIndex(ctx context.Context, writer physical.Backend, index uint64) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	marker := drPersistedStreamAppliedIndex{
		Version:        drStreamAppliedIndexVersion,
		RelationshipID: s.relationshipID,
		ClusterID:      s.flatAccumulatorClusterID(),
		CommitIndex:    index,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drStreamAppliedIndexStoragePath,
		Value: data,
	})
}

func drFlatAccumulatorDeltaPath(index uint64) string {
	return fmt.Sprintf("%s%020d", drFlatAccumulatorDeltaStoragePath, index)
}

func drFlatAccumulatorDeltaIndexFromKey(key string) (uint64, bool) {
	index, err := strconv.ParseUint(key, 10, 64)
	return index, err == nil && index > 0
}

func drFlatAccumulatorBytesFromArray(value [32]byte) []byte {
	out := make([]byte, 32)
	copy(out, value[:])
	return out
}

func drFlatAccumulatorArrayFromBytes(name string, value []byte) ([32]byte, error) {
	var out [32]byte
	if len(value) != len(out) {
		return out, fmt.Errorf("%s length mismatch", name)
	}
	copy(out[:], value)
	return out, nil
}

func drFlatAccumulatorPersistedDeltasFromMemory(deltas []drFlatAccumulatorDelta) []drFlatAccumulatorPersistedDeltaItem {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]drFlatAccumulatorPersistedDeltaItem, 0, len(deltas))
	for _, delta := range deltas {
		item := drFlatAccumulatorPersistedDeltaItem{
			KID:       drFlatAccumulatorBytesFromArray(delta.kid),
			OldExists: delta.oldExists,
			NewExists: delta.newExists,
		}
		if delta.oldExists {
			item.OldVID = drFlatAccumulatorBytesFromArray(delta.oldVID)
		}
		if delta.newExists {
			item.NewVID = drFlatAccumulatorBytesFromArray(delta.newVID)
		}
		out = append(out, item)
	}
	return out
}

func drFlatAccumulatorDeltasFromPersisted(items []drFlatAccumulatorPersistedDeltaItem) ([]drFlatAccumulatorDelta, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]drFlatAccumulatorDelta, 0, len(items))
	for i, item := range items {
		kid, err := drFlatAccumulatorArrayFromBytes(fmt.Sprintf("deltas[%d].kid", i), item.KID)
		if err != nil {
			return nil, err
		}
		delta := drFlatAccumulatorDelta{
			kid:       kid,
			oldExists: item.OldExists,
			newExists: item.NewExists,
		}
		if item.OldExists {
			oldVID, err := drFlatAccumulatorArrayFromBytes(fmt.Sprintf("deltas[%d].old_vid", i), item.OldVID)
			if err != nil {
				return nil, err
			}
			delta.oldVID = oldVID
		}
		if item.NewExists {
			newVID, err := drFlatAccumulatorArrayFromBytes(fmt.Sprintf("deltas[%d].new_vid", i), item.NewVID)
			if err != nil {
				return nil, err
			}
			delta.newVID = newVID
		}
		out = append(out, delta)
	}
	return out, nil
}

func (s *drReplicationSecondary) persistFlatAccumulatorDeltaBatch(
	ctx context.Context,
	writer physical.Backend,
	index uint64,
	snapshotIndex uint64,
	deltas []drFlatAccumulatorDelta,
) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}

	batch := drFlatAccumulatorPersistedDeltaBatch{
		Version:             drFlatAccumulatorDeltaVersion,
		RelationshipID:      s.relationshipID,
		ClusterID:           s.flatAccumulatorClusterID(),
		RangeBits:           drFlatAccumulatorRangeBits,
		ChecksumAlgorithm:   drFlatAccumulatorChecksumAlgorithm,
		CommitIndex:         index,
		SnapshotCommitIndex: snapshotIndex,
		Deltas:              drFlatAccumulatorPersistedDeltasFromMemory(deltas),
	}

	path := drFlatAccumulatorDeltaPath(index)
	if existing, err := writer.Get(ctx, path); err != nil {
		return err
	} else if existing != nil && len(existing.Value) > 0 {
		var persisted drFlatAccumulatorPersistedDeltaBatch
		if err := json.Unmarshal(existing.Value, &persisted); err != nil {
			return fmt.Errorf("read existing flat accumulator delta batch: %w", err)
		}
		if err := s.validateFlatAccumulatorDeltaBatch(&persisted); err != nil {
			return fmt.Errorf("validate existing flat accumulator delta batch: %w", err)
		}
		batch.Deltas = append(persisted.Deltas, batch.Deltas...)
	}

	data, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if err := writer.Put(ctx, &physical.Entry{
		Key:   path,
		Value: data,
	}); err != nil {
		return err
	}
	s.recordFlatAccumulatorDeltaPersist(index, len(deltas))
	return nil
}

func (s *drReplicationSecondary) recordFlatAccumulatorDeltaPersist(index uint64, deltaCount int) {
	if s == nil || index == 0 {
		return
	}
	s.flatAccumulatorDeltaBatches.Add(1)
	if deltaCount > 0 {
		s.flatAccumulatorDeltaEntries.Add(uint64(deltaCount))
	}
	for {
		oldest := s.flatAccumulatorDeltaOldestIndex.Load()
		if oldest != 0 && oldest <= index {
			break
		}
		if s.flatAccumulatorDeltaOldestIndex.CompareAndSwap(oldest, index) {
			break
		}
	}
	atomicMaxUint64(&s.flatAccumulatorDeltaNewestIndex, index)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_batches_total"}, 1)
	if deltaCount > 0 {
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_entries_total"}, float32(deltaCount))
	}
	metrics.SetGauge([]string{"replication", "dr", "secondary", "flat_accumulator_delta_newest_index"}, float32(index))
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
	deltas []drFlatAccumulatorDelta,
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
		if err := s.deletePersistedFlatAccumulatorDeltas(ctx, writer, index); err != nil {
			return err
		}
	} else {
		if err := s.persistFlatAccumulatorDeltaBatch(ctx, writer, index, snapshotIndex, deltas); err != nil {
			return err
		}
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
	if err := s.persistFlatAccumulatorState(ctx, s.core.physical, index, buckets, true, nil); err != nil {
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
	if err := s.deletePersistedFlatAccumulatorDeltas(ctx, writer, 0); err != nil {
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
	if err := s.deletePersistedStreamAppliedIndex(ctx, writer); err != nil {
		return err
	}
	if err := s.deletePersistedLocalKIDIndex(ctx, writer); err != nil {
		return err
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
	if err := s.persistFlatAccumulatorState(ctx, s.core.physical, index, buckets, true, nil); err != nil {
		return err
	}
	s.rangeAccumulator.replace(index, buckets)
	if err := s.resetLocalKIDIndexFromSet(ctx, s.core.physical, index, rs); err != nil {
		return err
	}
	if err := s.persistStreamAppliedIndex(ctx, s.core.physical, index); err != nil {
		return err
	}
	return nil
}

func (s *drReplicationSecondary) loadPersistentStreamAppliedIndex(ctx context.Context) {
	if s == nil || s.core == nil || s.core.physical == nil {
		return
	}
	entry, err := s.core.physical.Get(ctx, drStreamAppliedIndexStoragePath)
	if err != nil {
		s.logger.Warn("failed to load DR stream applied index", "error", err)
		return
	}
	if entry == nil || len(entry.Value) == 0 {
		return
	}

	var marker drPersistedStreamAppliedIndex
	if err := json.Unmarshal(entry.Value, &marker); err != nil {
		s.logger.Warn("discarding unreadable DR stream applied index", "error", err)
		_ = s.deletePersistedStreamAppliedIndex(ctx, s.core.physical)
		return
	}
	if err := s.validateStreamAppliedIndex(&marker); err != nil {
		s.logger.Warn("discarding incompatible DR stream applied index", "error", err)
		_ = s.deletePersistedStreamAppliedIndex(ctx, s.core.physical)
		return
	}
	if marker.CommitIndex > s.lastAppliedIndex.Load() {
		s.setLastAppliedIndex(marker.CommitIndex)
	}
	s.logger.Info("loaded DR stream applied index", "commit_index", marker.CommitIndex)
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
		buckets, ok, err := s.replayPersistedFlatAccumulatorDeltas(ctx, snapshot, cursor)
		if err != nil {
			s.flatAccumulatorDeltaReplayFailures.Add(1)
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_replay_failures_total"}, 1)
			s.logger.Warn("discarding stale DR flat accumulator snapshot after delta replay failed",
				"snapshot_commit_index", snapshot.CommitIndex,
				"cursor_commit_index", cursor.CommitIndex,
				"error", err)
			_ = s.deletePersistedFlatAccumulator(ctx, s.core.physical)
			s.setLastAppliedIndex(cursor.CommitIndex)
			return
		}
		if ok {
			s.rangeAccumulator.replace(cursor.CommitIndex, buckets)
			s.flatAccumulatorSnapshotIndex.Store(snapshot.CommitIndex)
			s.flatAccumulatorSnapshotLastAt.Store(time.Now().UnixNano())
			if err := s.refreshFlatAccumulatorDeltaIndexStats(ctx, s.core.physical); err != nil {
				s.logger.Warn("failed to refresh DR flat accumulator delta stats", "error", err)
			}
			s.setLastAppliedIndex(cursor.CommitIndex)
			s.logger.Info("loaded DR flat accumulator snapshot with delta replay",
				"snapshot_commit_index", snapshot.CommitIndex,
				"cursor_commit_index", cursor.CommitIndex)
			return
		}
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

func (s *drReplicationSecondary) replayPersistedFlatAccumulatorDeltas(
	ctx context.Context,
	snapshot drFlatAccumulatorPersistedSnapshot,
	cursor drFlatAccumulatorPersistedCursor,
) ([drRangeMaxTotalRanges]drFlatAccumulatorBucket, bool, error) {
	var buckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	if cursor.CommitIndex <= snapshot.CommitIndex {
		return buckets, false, nil
	}
	for i, bucket := range snapshot.Buckets {
		buckets[i] = drFlatAccumulatorBucket{
			checksum: bucket.Checksum,
			count:    bucket.Count,
		}
	}

	keys, err := s.core.physical.List(ctx, drFlatAccumulatorDeltaStoragePath)
	if err != nil {
		return buckets, false, err
	}
	sort.Strings(keys)

	var replayedBatches uint64
	var replayedEntries uint64
	var lastDeltaIndex uint64
	for _, key := range keys {
		index, ok := drFlatAccumulatorDeltaIndexFromKey(key)
		if !ok {
			continue
		}
		if index <= snapshot.CommitIndex || index > cursor.CommitIndex {
			continue
		}
		entry, err := s.core.physical.Get(ctx, drFlatAccumulatorDeltaStoragePath+key)
		if err != nil {
			return buckets, false, err
		}
		if entry == nil || len(entry.Value) == 0 {
			return buckets, false, fmt.Errorf("missing flat accumulator delta batch %d", index)
		}
		var batch drFlatAccumulatorPersistedDeltaBatch
		if err := json.Unmarshal(entry.Value, &batch); err != nil {
			return buckets, false, err
		}
		if err := s.validateFlatAccumulatorDeltaBatch(&batch); err != nil {
			return buckets, false, err
		}
		if batch.CommitIndex != index {
			return buckets, false, fmt.Errorf("delta batch key/index mismatch: key=%d batch=%d", index, batch.CommitIndex)
		}
		if batch.SnapshotCommitIndex != 0 && batch.SnapshotCommitIndex != snapshot.CommitIndex {
			return buckets, false, fmt.Errorf("delta batch snapshot index mismatch: batch=%d snapshot=%d", batch.SnapshotCommitIndex, snapshot.CommitIndex)
		}
		deltas, err := drFlatAccumulatorDeltasFromPersisted(batch.Deltas)
		if err != nil {
			return buckets, false, err
		}
		if !applyFlatAccumulatorDeltasToBuckets(&buckets, deltas) {
			return buckets, false, fmt.Errorf("delta batch %d failed accumulator apply", index)
		}
		replayedBatches++
		replayedEntries += uint64(len(deltas))
		lastDeltaIndex = index
	}

	if lastDeltaIndex != cursor.CommitIndex {
		return buckets, false, fmt.Errorf("flat accumulator delta coverage ended at %d, cursor at %d", lastDeltaIndex, cursor.CommitIndex)
	}
	s.flatAccumulatorDeltaReplayCount.Add(1)
	s.flatAccumulatorDeltaReplayBatches.Add(replayedBatches)
	s.flatAccumulatorDeltaReplayEntries.Add(replayedEntries)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_replay_total"}, 1)
	if replayedBatches > 0 {
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_replay_batches_total"}, float32(replayedBatches))
	}
	if replayedEntries > 0 {
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_delta_replay_entries_total"}, float32(replayedEntries))
	}
	return buckets, true, nil
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

func (s *drReplicationSecondary) validateStreamAppliedIndex(marker *drPersistedStreamAppliedIndex) error {
	if marker == nil {
		return fmt.Errorf("stream applied index is nil")
	}
	if marker.Version != drStreamAppliedIndexVersion {
		return fmt.Errorf("unsupported stream applied index version %d", marker.Version)
	}
	if marker.RelationshipID == "" || marker.RelationshipID != s.relationshipID {
		return fmt.Errorf("relationship_id mismatch")
	}
	if clusterID := s.flatAccumulatorClusterID(); clusterID != "" && marker.ClusterID != "" && marker.ClusterID != clusterID {
		return fmt.Errorf("cluster_id mismatch")
	}
	if marker.CommitIndex == 0 {
		return fmt.Errorf("commit_index is zero")
	}
	return nil
}

func (s *drReplicationSecondary) validateFlatAccumulatorDeltaBatch(batch *drFlatAccumulatorPersistedDeltaBatch) error {
	if batch == nil {
		return fmt.Errorf("delta batch is nil")
	}
	if batch.Version != drFlatAccumulatorDeltaVersion {
		return fmt.Errorf("unsupported delta batch version %d", batch.Version)
	}
	if batch.RelationshipID == "" || batch.RelationshipID != s.relationshipID {
		return fmt.Errorf("relationship_id mismatch")
	}
	if clusterID := s.flatAccumulatorClusterID(); clusterID != "" && batch.ClusterID != "" && batch.ClusterID != clusterID {
		return fmt.Errorf("cluster_id mismatch")
	}
	if batch.RangeBits != drFlatAccumulatorRangeBits {
		return fmt.Errorf("range_bits mismatch")
	}
	if batch.ChecksumAlgorithm != drFlatAccumulatorChecksumAlgorithm {
		return fmt.Errorf("checksum_algorithm mismatch")
	}
	if batch.CommitIndex == 0 {
		return fmt.Errorf("commit_index is zero")
	}
	if batch.SnapshotCommitIndex > batch.CommitIndex {
		return fmt.Errorf("snapshot_commit_index is ahead of commit_index")
	}
	if _, err := drFlatAccumulatorDeltasFromPersisted(batch.Deltas); err != nil {
		return err
	}
	return nil
}

func (s *drReplicationSecondary) deletePersistedStreamAppliedIndex(ctx context.Context, writer physical.Backend) error {
	return deletePersistedStreamAppliedIndex(ctx, writer)
}

func deletePersistedStreamAppliedIndex(ctx context.Context, writer physical.Backend) error {
	if writer == nil {
		return nil
	}
	return writer.Delete(ctx, drStreamAppliedIndexStoragePath)
}

func (s *drReplicationSecondary) deletePersistedFlatAccumulatorDeltas(ctx context.Context, writer physical.Backend, throughIndex uint64) error {
	if s == nil || writer == nil {
		return nil
	}
	keys, err := writer.List(ctx, drFlatAccumulatorDeltaStoragePath)
	if err != nil {
		return err
	}
	for _, key := range keys {
		index, ok := drFlatAccumulatorDeltaIndexFromKey(key)
		if !ok {
			continue
		}
		if throughIndex == 0 || index <= throughIndex {
			if err := writer.Delete(ctx, drFlatAccumulatorDeltaStoragePath+key); err != nil {
				return err
			}
		}
	}
	if throughIndex == 0 {
		s.flatAccumulatorDeltaOldestIndex.Store(0)
		s.flatAccumulatorDeltaNewestIndex.Store(0)
		return nil
	}
	return s.refreshFlatAccumulatorDeltaIndexStats(ctx, writer)
}

func (s *drReplicationSecondary) refreshFlatAccumulatorDeltaIndexStats(ctx context.Context, reader physical.Backend) error {
	if s == nil || reader == nil {
		return nil
	}
	keys, err := reader.List(ctx, drFlatAccumulatorDeltaStoragePath)
	if err != nil {
		return err
	}
	var oldest uint64
	var newest uint64
	for _, key := range keys {
		index, ok := drFlatAccumulatorDeltaIndexFromKey(key)
		if !ok {
			continue
		}
		if oldest == 0 || index < oldest {
			oldest = index
		}
		if index > newest {
			newest = index
		}
	}
	s.flatAccumulatorDeltaOldestIndex.Store(oldest)
	s.flatAccumulatorDeltaNewestIndex.Store(newest)
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
