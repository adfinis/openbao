// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drLocalKIDIndexMetaPath     = "core/cluster/local/dr/kid-index/meta"
	drLocalKIDIndexEntryPath    = "core/cluster/local/dr/kid-index/entries/"
	drLocalKIDIndexVersion      = 1
	drLocalKIDIndexEntryVersion = 1
)

type drLocalKIDIndexMeta struct {
	Version        int    `json:"version"`
	RelationshipID string `json:"relationship_id"`
	ClusterID      string `json:"cluster_id,omitempty"`
	RangeBits      int    `json:"range_bits"`
	RangeCount     int    `json:"range_count"`
	CommitIndex    uint64 `json:"commit_index"`
}

type drLocalKIDIndexEntry struct {
	Version        int    `json:"version"`
	RelationshipID string `json:"relationship_id"`
	ClusterID      string `json:"cluster_id,omitempty"`
	KID            []byte `json:"kid"`
	VID            []byte `json:"vid"`
	Key            string `json:"key"`
}

func drLocalKIDIndexRangePrefix(rangeID uint64) string {
	return fmt.Sprintf("%s%04d/", drLocalKIDIndexEntryPath, rangeID)
}

func drLocalKIDIndexEntryStoragePath(kid [32]byte) string {
	return drLocalKIDIndexRangePrefix(reconciler.RangeIDFromKID(kid)) + hex.EncodeToString(kid[:])
}

func (s *drReplicationSecondary) persistLocalKIDIndexMeta(ctx context.Context, writer physical.Backend, index uint64) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	meta := drLocalKIDIndexMeta{
		Version:        drLocalKIDIndexVersion,
		RelationshipID: s.relationshipID,
		ClusterID:      s.flatAccumulatorClusterID(),
		RangeBits:      drFlatAccumulatorRangeBits,
		RangeCount:     drRangeMaxTotalRanges,
		CommitIndex:    index,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drLocalKIDIndexMetaPath,
		Value: data,
	})
}

func (s *drReplicationSecondary) advanceLocalKIDIndexMetaIfCurrent(ctx context.Context, writer physical.Backend, fromIndex, toIndex uint64) error {
	if s == nil || writer == nil || fromIndex == 0 || toIndex == 0 || fromIndex == toIndex {
		return nil
	}
	entry, err := writer.Get(ctx, drLocalKIDIndexMetaPath)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	var meta drLocalKIDIndexMeta
	if err := json.Unmarshal(entry.Value, &meta); err != nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "advance_meta_decode_failed")
	}
	if err := s.validateLocalKIDIndexMeta(&meta); err != nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "advance_meta_invalid")
	}
	if meta.CommitIndex != fromIndex {
		return nil
	}
	return s.persistLocalKIDIndexMeta(ctx, writer, toIndex)
}

func (s *drReplicationSecondary) persistLocalKIDIndexChanges(ctx context.Context, writer physical.Backend, index uint64, changes []*EntryChange) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	entry, err := writer.Get(ctx, drLocalKIDIndexMetaPath)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	var meta drLocalKIDIndexMeta
	if err := json.Unmarshal(entry.Value, &meta); err != nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "stream_delta_meta_decode_failed")
	}
	if err := s.validateLocalKIDIndexMeta(&meta); err != nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "stream_delta_meta_invalid")
	}
	if meta.CommitIndex > index {
		return nil
	}

	updates := 0
	for _, change := range changes {
		if change == nil || change.Key == "" || isDRNeverReplicatePath(change.Key) {
			continue
		}
		kid, ok := s.kidFromEntryChange(change)
		if !ok {
			return fmt.Errorf("local kid index change has no resolvable kid for key %q", change.Key)
		}
		switch physical.Operation(change.OpType) {
		case physical.PutOperation:
			vid := s.scanner.ComputeVIDWithSealWrap(change.Value, change.SealWrap)
			if err := s.putLocalKIDIndexEntry(ctx, writer, kid, vid, change.Key); err != nil {
				return err
			}
			updates++
		case physical.DeleteOperation:
			if err := writer.Delete(ctx, drLocalKIDIndexEntryStoragePath(kid)); err != nil {
				return err
			}
			updates++
		}
	}
	if updates > 0 {
		s.localKIDIndexUpdates.Add(uint64(updates))
	}
	return s.persistLocalKIDIndexMeta(ctx, writer, index)
}

func (s *drReplicationSecondary) localKIDIndexEntryData(kid, vid [32]byte, key string) ([]byte, error) {
	entry := drLocalKIDIndexEntry{
		Version:        drLocalKIDIndexEntryVersion,
		RelationshipID: s.relationshipID,
		ClusterID:      s.flatAccumulatorClusterID(),
		KID:            append([]byte(nil), kid[:]...),
		VID:            append([]byte(nil), vid[:]...),
		Key:            key,
	}
	return json.Marshal(entry)
}

func (s *drReplicationSecondary) putLocalKIDIndexEntry(ctx context.Context, writer physical.Backend, kid, vid [32]byte, key string) error {
	if key == "" {
		return nil
	}
	data, err := s.localKIDIndexEntryData(kid, vid, key)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drLocalKIDIndexEntryStoragePath(kid),
		Value: data,
	})
}

func (s *drReplicationSecondary) resetLocalKIDIndexFromSet(ctx context.Context, writer physical.Backend, index uint64, rs *reconciler.ReconciliationSet) error {
	ranges := make([]uint64, 0, drRangeMaxTotalRanges)
	for i := 0; i < drRangeMaxTotalRanges; i++ {
		ranges = append(ranges, uint64(i))
	}
	return s.resetLocalKIDIndexRangesFromSet(ctx, writer, index, rs, ranges)
}

func (s *drReplicationSecondary) resetLocalKIDIndexRangesFromSet(ctx context.Context, writer physical.Backend, index uint64, rs *reconciler.ReconciliationSet, ranges []uint64) error {
	if s == nil || writer == nil || index == 0 {
		return nil
	}
	if rs == nil {
		return fmt.Errorf("local kid index reset requires reconciliation set")
	}
	if len(rs.KIDToVID) > 0 && rs.KIDToKey == nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "reset_missing_key_map")
	}
	rangeSet := make(map[uint64]struct{}, len(ranges))
	for _, rangeID := range ranges {
		if rangeID >= drRangeMaxTotalRanges {
			return fmt.Errorf("local kid index reset range %d outside max %d", rangeID, drRangeMaxTotalRanges)
		}
		rangeSet[rangeID] = struct{}{}
	}

	kids := make([][32]byte, 0, len(rs.KIDToVID))
	for kid := range rs.KIDToVID {
		if _, ok := rangeSet[reconciler.RangeIDFromKID(kid)]; ok {
			kids = append(kids, kid)
		}
	}
	sort.Slice(kids, func(i, j int) bool {
		return hex.EncodeToString(kids[i][:]) < hex.EncodeToString(kids[j][:])
	})

	if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "reset_rebuild_start"); err != nil {
		return err
	}
	if txnBackend, ok := writer.(physical.TransactionalBackend); ok {
		if err := s.resetLocalKIDIndexRangesFromSetTxn(ctx, txnBackend, index, rs, rangeSet, kids); err != nil {
			return err
		}
		s.localKIDIndexResets.Add(1)
		return s.persistLocalKIDIndexMeta(ctx, writer, index)
	}

	for rangeID := range rangeSet {
		if err := s.deleteLocalKIDIndexRange(ctx, writer, rangeID); err != nil {
			return err
		}
	}
	for _, kid := range kids {
		vid := rs.KIDToVID[kid]
		key := ""
		if rs.KIDToKey != nil {
			key = rs.KIDToKey[kid]
		}
		if key == "" {
			return fmt.Errorf("local kid index reset missing key for kid %x", kid)
		}
		if isDRNeverReplicatePath(key) {
			continue
		}
		if err := s.putLocalKIDIndexEntry(ctx, writer, kid, vid, key); err != nil {
			return err
		}
	}
	s.localKIDIndexResets.Add(1)
	return s.persistLocalKIDIndexMeta(ctx, writer, index)
}

func (s *drReplicationSecondary) resetLocalKIDIndexRangesFromSetTxn(ctx context.Context, writer physical.TransactionalBackend, index uint64, rs *reconciler.ReconciliationSet, rangeSet map[uint64]struct{}, kids [][32]byte) error {
	batchSize := s.reconcilePutBatchEntries
	if batchSize <= 0 {
		batchSize = drDefaultReconcilePutBatchEntries
	}

	deletePaths := make([]string, 0)
	for rangeID := range rangeSet {
		prefix := drLocalKIDIndexRangePrefix(rangeID)
		keys, err := writer.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, key := range keys {
			deletePaths = append(deletePaths, prefix+key)
		}
	}
	for i := 0; i < len(deletePaths); i += batchSize {
		end := i + batchSize
		if end > len(deletePaths) {
			end = len(deletePaths)
		}
		if err := s.applyLocalKIDIndexDeleteBatch(ctx, writer, deletePaths[i:end]); err != nil {
			return err
		}
	}

	entries := make([]*physical.Entry, 0, len(kids))
	for _, kid := range kids {
		vid := rs.KIDToVID[kid]
		key := ""
		if rs.KIDToKey != nil {
			key = rs.KIDToKey[kid]
		}
		if key == "" {
			return fmt.Errorf("local kid index reset missing key for kid %x", kid)
		}
		if isDRNeverReplicatePath(key) {
			continue
		}
		data, err := s.localKIDIndexEntryData(kid, vid, key)
		if err != nil {
			return err
		}
		entries = append(entries, &physical.Entry{
			Key:   drLocalKIDIndexEntryStoragePath(kid),
			Value: data,
		})
	}
	for i := 0; i < len(entries); i += batchSize {
		end := i + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := s.applyLocalKIDIndexPutBatch(ctx, writer, entries[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *drReplicationSecondary) applyLocalKIDIndexDeleteBatch(ctx context.Context, writer physical.TransactionalBackend, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := writer.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, key := range keys {
		if err := tx.Delete(ctx, key); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *drReplicationSecondary) applyLocalKIDIndexPutBatch(ctx context.Context, writer physical.TransactionalBackend, entries []*physical.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := writer.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, entry := range entries {
		if err := tx.Put(ctx, entry); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *drReplicationSecondary) deleteLocalKIDIndexRange(ctx context.Context, writer physical.Backend, rangeID uint64) error {
	return deleteLocalKIDIndexRange(ctx, writer, rangeID)
}

func deleteLocalKIDIndexRange(ctx context.Context, writer physical.Backend, rangeID uint64) error {
	prefix := drLocalKIDIndexRangePrefix(rangeID)
	keys, err := writer.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := writer.Delete(ctx, prefix+key); err != nil {
			return err
		}
	}
	return nil
}

func (s *drReplicationSecondary) deletePersistedLocalKIDIndex(ctx context.Context, writer physical.Backend) error {
	return deletePersistedLocalKIDIndex(ctx, writer)
}

func (s *drReplicationSecondary) invalidatePersistedLocalKIDIndex(ctx context.Context, writer physical.Backend) error {
	return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "unspecified")
}

func (s *drReplicationSecondary) invalidatePersistedLocalKIDIndexWithReason(ctx context.Context, writer physical.Backend, reason string) error {
	if writer == nil {
		return nil
	}
	s.recordLocalKIDIndexInvalidation(reason)
	return writer.Delete(ctx, drLocalKIDIndexMetaPath)
}

func deletePersistedLocalKIDIndex(ctx context.Context, writer physical.Backend) error {
	if writer == nil {
		return nil
	}
	if err := writer.Delete(ctx, drLocalKIDIndexMetaPath); err != nil {
		return err
	}
	for rangeID := uint64(0); rangeID < drRangeMaxTotalRanges; rangeID++ {
		if err := deleteLocalKIDIndexRange(ctx, writer, rangeID); err != nil {
			return err
		}
	}
	return nil
}

func (s *drReplicationSecondary) loadLocalKIDIndexForRanges(
	ctx context.Context,
	reader physical.Backend,
	expectedIndex uint64,
	ranges []uint64,
	expectedBuckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket,
) (*reconciler.ReconciliationSet, bool, string, error) {
	if s == nil || reader == nil || expectedIndex == 0 {
		return nil, false, "unavailable", nil
	}
	entry, err := reader.Get(ctx, drLocalKIDIndexMetaPath)
	if err != nil {
		return nil, false, "meta_read_failed", err
	}
	if entry == nil {
		return nil, false, "meta_missing", nil
	}
	var meta drLocalKIDIndexMeta
	if err := json.Unmarshal(entry.Value, &meta); err != nil {
		return nil, false, "meta_decode_failed", err
	}
	if err := s.validateLocalKIDIndexMeta(&meta); err != nil {
		return nil, false, "meta_invalid", err
	}
	if meta.CommitIndex != expectedIndex {
		return nil, false, "meta_index_mismatch", nil
	}

	rs := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{CommitIndex: expectedIndex},
		KIDToVID:   make(map[[32]byte][32]byte),
		KIDToKey:   make(map[[32]byte]string),
	}
	seenRanges := make(map[uint64]struct{}, len(ranges))
	for _, rangeID := range ranges {
		if rangeID >= drRangeMaxTotalRanges {
			return nil, false, "range_out_of_bounds", fmt.Errorf("local kid index range %d outside max %d", rangeID, drRangeMaxTotalRanges)
		}
		if _, ok := seenRanges[rangeID]; ok {
			continue
		}
		seenRanges[rangeID] = struct{}{}
		prefix := drLocalKIDIndexRangePrefix(rangeID)
		keys, err := reader.List(ctx, prefix)
		if err != nil {
			return nil, false, "entry_list_failed", err
		}
		for _, key := range keys {
			entry, err := reader.Get(ctx, prefix+key)
			if err != nil {
				return nil, false, "entry_read_failed", err
			}
			if entry == nil {
				continue
			}
			item, err := s.decodeLocalKIDIndexEntry(entry.Value)
			if err != nil {
				return nil, false, "entry_decode_failed", err
			}
			rangeForKID := reconciler.RangeIDFromKID(item.kid)
			if rangeForKID != rangeID {
				return nil, false, "entry_range_mismatch", fmt.Errorf("local kid index entry %x stored under range %d but maps to %d", item.kid, rangeID, rangeForKID)
			}
			rs.KIDToVID[item.kid] = item.vid
			rs.KIDToKey[item.kid] = item.key
		}
	}
	rs.KeyCount = len(rs.KIDToVID)

	localIndex := reconciler.NewRangeMapIndex(rs.KIDToVID, nil)
	for rangeID := range seenRanges {
		checksum, count := reconciler.ComputeRangeChecksum(localIndex, rangeID)
		expected := expectedBuckets[rangeID]
		if checksum != expected.checksum || count != expected.count {
			return nil, false, "bucket_mismatch", fmt.Errorf("local kid index bucket mismatch for range %d: index_count=%d accumulator_count=%d", rangeID, count, expected.count)
		}
	}
	s.localKIDIndexBucketLoads.Add(uint64(len(seenRanges)))
	s.localKIDIndexEntriesLoaded.Add(uint64(len(rs.KIDToVID)))
	return rs, true, "loaded", nil
}

func (s *drReplicationSecondary) validateLocalKIDIndexMeta(meta *drLocalKIDIndexMeta) error {
	if meta == nil {
		return fmt.Errorf("local kid index meta is nil")
	}
	if meta.Version != drLocalKIDIndexVersion {
		return fmt.Errorf("local kid index version mismatch: %d", meta.Version)
	}
	if meta.RelationshipID != s.relationshipID {
		return fmt.Errorf("local kid index relationship mismatch")
	}
	if meta.ClusterID != "" && meta.ClusterID != s.flatAccumulatorClusterID() {
		return fmt.Errorf("local kid index cluster mismatch")
	}
	if meta.RangeBits != drFlatAccumulatorRangeBits || meta.RangeCount != drRangeMaxTotalRanges {
		return fmt.Errorf("local kid index range metadata mismatch")
	}
	return nil
}

type drDecodedLocalKIDIndexEntry struct {
	kid [32]byte
	vid [32]byte
	key string
}

func (s *drReplicationSecondary) decodeLocalKIDIndexEntry(data []byte) (drDecodedLocalKIDIndexEntry, error) {
	var item drLocalKIDIndexEntry
	if err := json.Unmarshal(data, &item); err != nil {
		return drDecodedLocalKIDIndexEntry{}, err
	}
	if item.Version != drLocalKIDIndexEntryVersion {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry version mismatch: %d", item.Version)
	}
	if item.RelationshipID != s.relationshipID {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry relationship mismatch")
	}
	if item.ClusterID != "" && item.ClusterID != s.flatAccumulatorClusterID() {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry cluster mismatch")
	}
	if len(item.KID) != 32 || len(item.VID) != 32 {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry has invalid kid/vid length")
	}
	if item.Key == "" {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry has empty key")
	}
	var out drDecodedLocalKIDIndexEntry
	copy(out.kid[:], item.KID)
	copy(out.vid[:], item.VID)
	out.key = item.Key
	return out, nil
}
