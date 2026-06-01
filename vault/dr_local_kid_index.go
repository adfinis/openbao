// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc64"
	"sort"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drLocalKIDIndexMetaPath      = "core/cluster/local/dr/kid-index/meta"
	drLocalKIDIndexEntryPath     = "core/cluster/local/dr/kid-index/entries/"
	drLocalKIDIndexDigestPath    = "core/cluster/local/dr/kid-index/digests/"
	drLocalKIDIndexVersion       = 1
	drLocalKIDIndexEntryVersion  = 2
	drLocalKIDIndexDigestVersion = 1
	drLocalKIDIndexLeafBits      = drFlatAccumulatorRangeBits + drRangeMaxSplitDepth
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
	Key            string `json:"key"`
	VID            []byte `json:"vid"`
}

type drLocalKIDIndexLeafDigest struct {
	Version        int    `json:"version"`
	RelationshipID string `json:"relationship_id"`
	ClusterID      string `json:"cluster_id,omitempty"`
	LeafBits       int    `json:"leaf_bits"`
	LeafID         uint64 `json:"leaf_id"`
	Count          uint64 `json:"count"`
	XORKeyHash     []byte `json:"xor_key_hash"`
	XORValueHash   []byte `json:"xor_value_hash"`
	Checksum       uint64 `json:"checksum"`
	KIDChecksum    uint64 `json:"kid_checksum"`
}

type drLocalKIDIndexLeafDigestState struct {
	count        uint64
	xorKeyHash   [32]byte
	xorValueHash [32]byte
	checksum     uint64
	kidChecksum  uint64
}

type drLocalKIDIndexLeafDigestUnderflowError struct {
	kid [32]byte
}

func (e *drLocalKIDIndexLeafDigestUnderflowError) Error() string {
	if e == nil {
		return "local kid index leaf digest underflow"
	}
	return fmt.Sprintf("local kid index leaf digest underflow for kid %x", e.kid)
}

func drLocalKIDIndexRangePrefix(rangeID uint64) string {
	return fmt.Sprintf("%s%04d/", drLocalKIDIndexEntryPath, rangeID)
}

func drLocalKIDIndexEntryStoragePath(kid [32]byte) string {
	return drLocalKIDIndexRangePrefix(reconciler.RangeIDFromKID(kid)) + hex.EncodeToString(kid[:])
}

func drLocalKIDIndexLeafIDFromKID(kid [32]byte) uint64 {
	return (uint64(kid[0]) << 8) | uint64(kid[1])
}

func drLocalKIDIndexDigestRangePrefix(rangeID uint64) string {
	return fmt.Sprintf("%s%04d/", drLocalKIDIndexDigestPath, rangeID)
}

func drLocalKIDIndexDigestStoragePath(leafID uint64) string {
	rangeID := leafID >> uint(drRangeMaxSplitDepth)
	return drLocalKIDIndexDigestRangePrefix(rangeID) + fmt.Sprintf("%04x", leafID)
}

func drLocalKIDIndexEntryLeafListPrefix(leafID uint64) string {
	rangeID := leafID >> uint(drRangeMaxSplitDepth)
	return drLocalKIDIndexRangePrefix(rangeID) + fmt.Sprintf("%04x", leafID)
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
	// Marker-only stream batches do not change storage contents. Keep this
	// helper as a validation hook, but avoid a metadata write unless the index
	// itself changed.
	return nil
}

func (s *drReplicationSecondary) persistLocalKIDIndexChanges(ctx context.Context, writer physical.Backend, index uint64, changes []*EntryChange, deltas []drFlatAccumulatorDelta) error {
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

	deltaByKID := make(map[[32]byte]drFlatAccumulatorDelta, len(deltas))
	for _, delta := range deltas {
		deltaByKID[delta.kid] = delta
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
			delta, deltaOK := deltaByKID[kid]
			if !deltaOK || !delta.newExists {
				return fmt.Errorf("local kid index put for key %q missing accumulator delta", change.Key)
			}
			if ok, err := s.validateLocalKIDIndexDeltaEntry(ctx, writer, kid, change.Key, delta); err != nil || !ok {
				return err
			}
			if err := s.updateLocalKIDIndexLeafDigestForDelta(ctx, writer, delta); err != nil {
				return s.handleLocalKIDIndexDigestUpdateError(ctx, writer, err)
			}
			if err := s.putLocalKIDIndexEntry(ctx, writer, kid, change.Key, delta.newVID); err != nil {
				return err
			}
			updates++
		case physical.DeleteOperation:
			delta, deltaOK := deltaByKID[kid]
			if !deltaOK {
				indexEntry, err := writer.Get(ctx, drLocalKIDIndexEntryStoragePath(kid))
				if err != nil {
					return err
				}
				if indexEntry != nil {
					return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "stream_delete_missing_delta")
				}
				continue
			}
			if ok, err := s.validateLocalKIDIndexDeltaEntry(ctx, writer, kid, change.Key, delta); err != nil || !ok {
				return err
			}
			if !delta.oldExists {
				continue
			}
			if err := s.updateLocalKIDIndexLeafDigestForDelta(ctx, writer, delta); err != nil {
				return s.handleLocalKIDIndexDigestUpdateError(ctx, writer, err)
			}
			if err := writer.Delete(ctx, drLocalKIDIndexEntryStoragePath(kid)); err != nil {
				return err
			}
			updates++
		}
	}
	if updates > 0 {
		s.localKIDIndexUpdates.Add(uint64(updates))
		return s.persistLocalKIDIndexMeta(ctx, writer, index)
	}
	return nil
}

func (s *drReplicationSecondary) validateLocalKIDIndexDeltaEntry(ctx context.Context, reader physical.Backend, kid [32]byte, key string, delta drFlatAccumulatorDelta) (bool, error) {
	entry, err := reader.Get(ctx, drLocalKIDIndexEntryStoragePath(kid))
	if err != nil {
		return false, err
	}
	if entry == nil {
		if delta.oldExists {
			return false, s.invalidatePersistedLocalKIDIndexWithReason(ctx, reader, "stream_delta_entry_missing")
		}
		return true, nil
	}
	item, err := s.decodeLocalKIDIndexEntry(entry.Value)
	if err != nil {
		return false, s.invalidatePersistedLocalKIDIndexWithReason(ctx, reader, "stream_delta_entry_decode_failed")
	}
	if item.kid != kid || item.key != key {
		return false, s.invalidatePersistedLocalKIDIndexWithReason(ctx, reader, "stream_delta_entry_mismatch")
	}
	if delta.oldExists {
		if item.vid != delta.oldVID {
			return false, s.invalidatePersistedLocalKIDIndexWithReason(ctx, reader, "stream_delta_entry_vid_mismatch")
		}
		return true, nil
	}
	return false, s.invalidatePersistedLocalKIDIndexWithReason(ctx, reader, "stream_delta_stale_entry")
}

func (s *drReplicationSecondary) handleLocalKIDIndexDigestUpdateError(ctx context.Context, writer physical.Backend, err error) error {
	if err == nil {
		return nil
	}
	var underflow *drLocalKIDIndexLeafDigestUnderflowError
	if errors.As(err, &underflow) {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "stream_leaf_digest_underflow")
	}
	return err
}

func (s *drReplicationSecondary) localKIDIndexEntryData(kid [32]byte, key string, vid [32]byte) ([]byte, error) {
	entry := drLocalKIDIndexEntry{
		Version:        drLocalKIDIndexEntryVersion,
		RelationshipID: s.relationshipID,
		ClusterID:      s.flatAccumulatorClusterID(),
		KID:            append([]byte(nil), kid[:]...),
		Key:            key,
		VID:            append([]byte(nil), vid[:]...),
	}
	return json.Marshal(entry)
}

func (s *drReplicationSecondary) putLocalKIDIndexEntry(ctx context.Context, writer physical.Backend, kid [32]byte, key string, vid [32]byte) error {
	if key == "" {
		return nil
	}
	data, err := s.localKIDIndexEntryData(kid, key, vid)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drLocalKIDIndexEntryStoragePath(kid),
		Value: data,
	})
}

func localKIDIndexDigestItemHashes(kid [32]byte, vid [32]byte) ([32]byte, [32]byte, uint64, uint64) {
	keyHash := sha256.Sum256(kid[:])
	h := sha256.New()
	h.Write(kid[:])
	h.Write(vid[:])
	var valueHash [32]byte
	copy(valueHash[:], h.Sum(nil))
	kidChecksum := crc64.Checksum(kid[:], drFlatAccumulatorCRCTable)
	valueChecksum := crc64.Checksum(vid[:], drFlatAccumulatorCRCTable)
	return keyHash, valueHash, kidChecksum ^ valueChecksum, kidChecksum
}

func (d *drLocalKIDIndexLeafDigestState) add(kid [32]byte, vid [32]byte) {
	keyHash, valueHash, checksum, kidChecksum := localKIDIndexDigestItemHashes(kid, vid)
	xorHash32(&d.xorKeyHash, keyHash)
	xorHash32(&d.xorValueHash, valueHash)
	d.checksum ^= checksum
	d.kidChecksum ^= kidChecksum
	d.count++
}

func (d *drLocalKIDIndexLeafDigestState) remove(kid [32]byte, vid [32]byte) error {
	if d.count == 0 {
		return &drLocalKIDIndexLeafDigestUnderflowError{kid: kid}
	}
	keyHash, valueHash, checksum, kidChecksum := localKIDIndexDigestItemHashes(kid, vid)
	xorHash32(&d.xorKeyHash, keyHash)
	xorHash32(&d.xorValueHash, valueHash)
	d.checksum ^= checksum
	d.kidChecksum ^= kidChecksum
	d.count--
	return nil
}

func (d drLocalKIDIndexLeafDigestState) rangeDescriptor(span reconciler.RangeSpan) reconciler.RangeDescriptor {
	return reconciler.RangeDescriptor{
		Span:         span,
		Count:        d.count,
		XORKeyHash:   d.xorKeyHash,
		XORValueHash: d.xorValueHash,
	}
}

func (d drLocalKIDIndexLeafDigestState) flatBucket() drFlatAccumulatorBucket {
	return drFlatAccumulatorBucket{
		checksum:    d.checksum,
		kidChecksum: d.kidChecksum,
		count:       d.count,
	}
}

func (s *drReplicationSecondary) updateLocalKIDIndexLeafDigestForDelta(ctx context.Context, writer physical.Backend, delta drFlatAccumulatorDelta) error {
	leafID := drLocalKIDIndexLeafIDFromKID(delta.kid)
	state, _, err := s.loadLocalKIDIndexLeafDigest(ctx, writer, leafID)
	if err != nil {
		return err
	}
	if delta.oldExists {
		if err := state.remove(delta.kid, delta.oldVID); err != nil {
			return err
		}
	}
	if delta.newExists {
		state.add(delta.kid, delta.newVID)
	}
	return s.persistLocalKIDIndexLeafDigest(ctx, writer, leafID, state)
}

func (s *drReplicationSecondary) persistLocalKIDIndexLeafDigest(ctx context.Context, writer physical.Backend, leafID uint64, state drLocalKIDIndexLeafDigestState) error {
	if writer == nil {
		return nil
	}
	path := drLocalKIDIndexDigestStoragePath(leafID)
	if state.count == 0 {
		return writer.Delete(ctx, path)
	}
	entry := drLocalKIDIndexLeafDigest{
		Version:        drLocalKIDIndexDigestVersion,
		RelationshipID: s.relationshipID,
		ClusterID:      s.flatAccumulatorClusterID(),
		LeafBits:       drLocalKIDIndexLeafBits,
		LeafID:         leafID,
		Count:          state.count,
		XORKeyHash:     append([]byte(nil), state.xorKeyHash[:]...),
		XORValueHash:   append([]byte(nil), state.xorValueHash[:]...),
		Checksum:       state.checksum,
		KIDChecksum:    state.kidChecksum,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   path,
		Value: data,
	})
}

func (s *drReplicationSecondary) loadLocalKIDIndexLeafDigest(ctx context.Context, reader physical.Backend, leafID uint64) (drLocalKIDIndexLeafDigestState, bool, error) {
	var state drLocalKIDIndexLeafDigestState
	if reader == nil {
		return state, false, nil
	}
	entry, err := reader.Get(ctx, drLocalKIDIndexDigestStoragePath(leafID))
	if err != nil {
		return state, false, err
	}
	if entry == nil {
		return state, false, nil
	}
	var digest drLocalKIDIndexLeafDigest
	if err := json.Unmarshal(entry.Value, &digest); err != nil {
		return state, false, err
	}
	if digest.Version != drLocalKIDIndexDigestVersion {
		return state, false, fmt.Errorf("local kid index leaf digest version mismatch: %d", digest.Version)
	}
	if digest.RelationshipID != s.relationshipID {
		return state, false, fmt.Errorf("local kid index leaf digest relationship mismatch")
	}
	if digest.ClusterID != "" && digest.ClusterID != s.flatAccumulatorClusterID() {
		return state, false, fmt.Errorf("local kid index leaf digest cluster mismatch")
	}
	if digest.LeafBits != drLocalKIDIndexLeafBits {
		return state, false, fmt.Errorf("local kid index leaf digest bits mismatch: %d", digest.LeafBits)
	}
	if digest.LeafID != leafID {
		return state, false, fmt.Errorf("local kid index leaf digest id mismatch: %d != %d", digest.LeafID, leafID)
	}
	if len(digest.XORKeyHash) != 32 || len(digest.XORValueHash) != 32 {
		return state, false, fmt.Errorf("local kid index leaf digest hash length mismatch")
	}
	state.count = digest.Count
	copy(state.xorKeyHash[:], digest.XORKeyHash)
	copy(state.xorValueHash[:], digest.XORValueHash)
	state.checksum = digest.Checksum
	state.kidChecksum = digest.KIDChecksum
	return state, true, nil
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
		if err := s.deleteLocalKIDIndexDigestRange(ctx, writer, rangeID); err != nil {
			return err
		}
	}
	leafDigests := make(map[uint64]drLocalKIDIndexLeafDigestState)
	for _, kid := range kids {
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
		vid := rs.KIDToVID[kid]
		leafID := drLocalKIDIndexLeafIDFromKID(kid)
		state := leafDigests[leafID]
		state.add(kid, vid)
		leafDigests[leafID] = state
		if err := s.putLocalKIDIndexEntry(ctx, writer, kid, key, vid); err != nil {
			return err
		}
	}
	if err := s.putLocalKIDIndexLeafDigests(ctx, writer, leafDigests); err != nil {
		return err
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
		digestPrefix := drLocalKIDIndexDigestRangePrefix(rangeID)
		digestKeys, err := writer.List(ctx, digestPrefix)
		if err != nil {
			return err
		}
		for _, key := range digestKeys {
			deletePaths = append(deletePaths, digestPrefix+key)
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
	leafDigests := make(map[uint64]drLocalKIDIndexLeafDigestState)
	for _, kid := range kids {
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
		vid := rs.KIDToVID[kid]
		leafID := drLocalKIDIndexLeafIDFromKID(kid)
		state := leafDigests[leafID]
		state.add(kid, vid)
		leafDigests[leafID] = state
		data, err := s.localKIDIndexEntryData(kid, key, vid)
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
	digestEntries, err := s.localKIDIndexLeafDigestEntries(leafDigests)
	if err != nil {
		return err
	}
	for i := 0; i < len(digestEntries); i += batchSize {
		end := i + batchSize
		if end > len(digestEntries) {
			end = len(digestEntries)
		}
		if err := s.applyLocalKIDIndexPutBatch(ctx, writer, digestEntries[i:end]); err != nil {
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

func (s *drReplicationSecondary) localKIDIndexLeafDigestEntries(leafDigests map[uint64]drLocalKIDIndexLeafDigestState) ([]*physical.Entry, error) {
	if len(leafDigests) == 0 {
		return nil, nil
	}
	leafIDs := make([]uint64, 0, len(leafDigests))
	for leafID, state := range leafDigests {
		if state.count == 0 {
			continue
		}
		leafIDs = append(leafIDs, leafID)
	}
	sort.Slice(leafIDs, func(i, j int) bool { return leafIDs[i] < leafIDs[j] })

	entries := make([]*physical.Entry, 0, len(leafIDs))
	for _, leafID := range leafIDs {
		state := leafDigests[leafID]
		entry := drLocalKIDIndexLeafDigest{
			Version:        drLocalKIDIndexDigestVersion,
			RelationshipID: s.relationshipID,
			ClusterID:      s.flatAccumulatorClusterID(),
			LeafBits:       drLocalKIDIndexLeafBits,
			LeafID:         leafID,
			Count:          state.count,
			XORKeyHash:     append([]byte(nil), state.xorKeyHash[:]...),
			XORValueHash:   append([]byte(nil), state.xorValueHash[:]...),
			Checksum:       state.checksum,
			KIDChecksum:    state.kidChecksum,
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		entries = append(entries, &physical.Entry{
			Key:   drLocalKIDIndexDigestStoragePath(leafID),
			Value: data,
		})
	}
	return entries, nil
}

func (s *drReplicationSecondary) putLocalKIDIndexLeafDigests(ctx context.Context, writer physical.Backend, leafDigests map[uint64]drLocalKIDIndexLeafDigestState) error {
	entries, err := s.localKIDIndexLeafDigestEntries(leafDigests)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := writer.Put(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *drReplicationSecondary) deleteLocalKIDIndexRange(ctx context.Context, writer physical.Backend, rangeID uint64) error {
	return deleteLocalKIDIndexRange(ctx, writer, rangeID)
}

func (s *drReplicationSecondary) deleteLocalKIDIndexDigestRange(ctx context.Context, writer physical.Backend, rangeID uint64) error {
	return deleteLocalKIDIndexDigestRange(ctx, writer, rangeID)
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

func deleteLocalKIDIndexDigestRange(ctx context.Context, writer physical.Backend, rangeID uint64) error {
	prefix := drLocalKIDIndexDigestRangePrefix(rangeID)
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
		if err := deleteLocalKIDIndexDigestRange(ctx, writer, rangeID); err != nil {
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
	readSnapshot, rollback, err := beginLocalKIDIndexReadSnapshot(ctx, reader)
	if err != nil {
		return nil, false, "snapshot_begin_failed", err
	}
	if rollback != nil {
		defer rollback()
		reader = readSnapshot
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
	if meta.CommitIndex > expectedIndex {
		return nil, false, "meta_index_mismatch", nil
	}
	cursorIndex, cursorOK, err := s.loadLocalKIDIndexCursor(ctx, reader)
	if err != nil {
		return nil, false, "cursor_invalid", err
	}
	if cursorOK && cursorIndex != expectedIndex {
		return nil, false, "storage_index_mismatch", nil
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
			computedKID := s.scanner.ComputeKID(item.key)
			if computedKID != item.kid {
				return nil, false, "entry_key_mismatch", fmt.Errorf("local kid index entry key %q maps to kid %x, expected %x", item.key, computedKID, item.kid)
			}
			rs.KIDToVID[item.kid] = item.vid
			rs.KIDToKey[item.kid] = item.key
		}
	}
	rs.KeyCount = len(rs.KIDToVID)

	localIndex := reconciler.NewRangeMapIndex(rs.KIDToVID, nil)
	for rangeID := range seenRanges {
		kidChecksum, count := computeFlatAccumulatorKIDMembershipChecksum(localIndex, rangeID)
		expected := expectedBuckets[rangeID]
		if kidChecksum != expected.kidChecksum || count != expected.count {
			return nil, false, "membership_mismatch", fmt.Errorf("local kid index membership mismatch for range %d: index_count=%d accumulator_count=%d index_kid_checksum=%d accumulator_kid_checksum=%d expected_index=%d meta_index=%d cursor_index=%d last_applied_index=%d", rangeID, count, expected.count, kidChecksum, expected.kidChecksum, expectedIndex, meta.CommitIndex, cursorIndex, s.lastAppliedIndex.Load())
		}
	}
	s.localKIDIndexBucketLoads.Add(uint64(len(seenRanges)))
	s.localKIDIndexEntriesLoaded.Add(uint64(len(rs.KIDToVID)))
	return rs, true, "loaded", nil
}

type drLocalKIDIndexDigestSource struct {
	secondary *drReplicationSecondary
	reader    physical.Backend
}

func (s *drReplicationSecondary) prepareLocalKIDIndexDigestSource(
	ctx context.Context,
	reader physical.Backend,
	expectedIndex uint64,
	ranges []uint64,
	expectedBuckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket,
) (*drLocalKIDIndexDigestSource, bool, string, error) {
	if s == nil || reader == nil || expectedIndex == 0 {
		return nil, false, "unavailable", nil
	}
	if ok, reason, err := s.validateLocalKIDIndexReady(ctx, reader, expectedIndex); err != nil || !ok {
		return nil, ok, reason, err
	}
	source := &drLocalKIDIndexDigestSource{secondary: s, reader: reader}
	seen := make(map[uint64]struct{}, len(ranges))
	for _, rangeID := range ranges {
		if rangeID >= drRangeMaxTotalRanges {
			return nil, false, "range_out_of_bounds", fmt.Errorf("local kid index range %d outside max %d", rangeID, drRangeMaxTotalRanges)
		}
		if _, ok := seen[rangeID]; ok {
			continue
		}
		seen[rangeID] = struct{}{}
		_, bucket, err := source.aggregate(ctx, reconciler.SpanFromRangeID(rangeID))
		if err != nil {
			return nil, false, "digest_read_failed", err
		}
		expected := expectedBuckets[rangeID]
		if bucket.count != expected.count || bucket.checksum != expected.checksum || bucket.kidChecksum != expected.kidChecksum {
			return nil, false, "digest_bucket_mismatch", fmt.Errorf("local kid index digest mismatch for range %d: digest_count=%d accumulator_count=%d digest_checksum=%d accumulator_checksum=%d digest_kid_checksum=%d accumulator_kid_checksum=%d", rangeID, bucket.count, expected.count, bucket.checksum, expected.checksum, bucket.kidChecksum, expected.kidChecksum)
		}
	}
	return source, true, "loaded", nil
}

func (s *drReplicationSecondary) validateLocalKIDIndexReady(ctx context.Context, reader physical.Backend, expectedIndex uint64) (bool, string, error) {
	entry, err := reader.Get(ctx, drLocalKIDIndexMetaPath)
	if err != nil {
		return false, "meta_read_failed", err
	}
	if entry == nil {
		return false, "meta_missing", nil
	}
	var meta drLocalKIDIndexMeta
	if err := json.Unmarshal(entry.Value, &meta); err != nil {
		return false, "meta_decode_failed", err
	}
	if err := s.validateLocalKIDIndexMeta(&meta); err != nil {
		return false, "meta_invalid", err
	}
	if meta.CommitIndex > expectedIndex {
		return false, "meta_index_mismatch", nil
	}
	cursorIndex, cursorOK, err := s.loadLocalKIDIndexCursor(ctx, reader)
	if err != nil {
		return false, "cursor_invalid", err
	}
	if cursorOK && cursorIndex != expectedIndex {
		return false, "storage_index_mismatch", nil
	}
	return true, "loaded", nil
}

func (source *drLocalKIDIndexDigestSource) RangeDigest(ctx context.Context, span reconciler.RangeSpan) (reconciler.RangeDescriptor, error) {
	desc, _, err := source.aggregate(ctx, span)
	return desc, err
}

func (source *drLocalKIDIndexDigestSource) aggregate(ctx context.Context, span reconciler.RangeSpan) (reconciler.RangeDescriptor, drFlatAccumulatorBucket, error) {
	var state drLocalKIDIndexLeafDigestState
	if source == nil || source.secondary == nil || source.reader == nil {
		return state.rangeDescriptor(span), state.flatBucket(), nil
	}
	startLeaf, endLeaf := localKIDIndexLeafBoundsForSpan(span)
	for leafID := startLeaf; leafID <= endLeaf; leafID++ {
		leaf, _, err := source.secondary.loadLocalKIDIndexLeafDigest(ctx, source.reader, leafID)
		if err != nil {
			return reconciler.RangeDescriptor{}, drFlatAccumulatorBucket{}, err
		}
		state.count += leaf.count
		xorHash32(&state.xorKeyHash, leaf.xorKeyHash)
		xorHash32(&state.xorValueHash, leaf.xorValueHash)
		state.checksum ^= leaf.checksum
		state.kidChecksum ^= leaf.kidChecksum
		if leafID == endLeaf {
			break
		}
	}
	return state.rangeDescriptor(span), state.flatBucket(), nil
}

func localKIDIndexLeafBoundsForSpan(span reconciler.RangeSpan) (uint64, uint64) {
	start := drLocalKIDIndexLeafIDFromKID(span.StartKID)
	end := drLocalKIDIndexLeafIDFromKID(span.EndKID)
	if end < start {
		return start, start
	}
	return start, end
}

func (s *drReplicationSecondary) loadLocalKIDIndexForSpans(
	ctx context.Context,
	reader physical.Backend,
	expectedIndex uint64,
	spans []reconciler.RangeSpan,
	source *drLocalKIDIndexDigestSource,
) (*reconciler.ReconciliationSet, bool, string, error) {
	if s == nil || reader == nil || expectedIndex == 0 {
		return nil, false, "unavailable", nil
	}
	readSnapshot, rollback, err := beginLocalKIDIndexReadSnapshot(ctx, reader)
	if err != nil {
		return nil, false, "snapshot_begin_failed", err
	}
	if rollback != nil {
		defer rollback()
		reader = readSnapshot
	}
	if ok, reason, err := s.validateLocalKIDIndexReady(ctx, reader, expectedIndex); err != nil || !ok {
		return nil, ok, reason, err
	}
	source = &drLocalKIDIndexDigestSource{secondary: s, reader: reader}

	rs := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{CommitIndex: expectedIndex},
		KIDToVID:   make(map[[32]byte][32]byte),
		KIDToKey:   make(map[[32]byte]string),
	}
	leafIDs := make(map[uint64]struct{})
	for _, span := range spans {
		startLeaf, endLeaf := localKIDIndexLeafBoundsForSpan(span)
		for leafID := startLeaf; leafID <= endLeaf; leafID++ {
			leaf, ok, err := s.loadLocalKIDIndexLeafDigest(ctx, reader, leafID)
			if err != nil {
				return nil, false, "digest_read_failed", err
			}
			if ok && leaf.count > 0 {
				leafIDs[leafID] = struct{}{}
			}
			if leafID == endLeaf {
				break
			}
		}
	}
	sortedLeafIDs := make([]uint64, 0, len(leafIDs))
	for leafID := range leafIDs {
		sortedLeafIDs = append(sortedLeafIDs, leafID)
	}
	sort.Slice(sortedLeafIDs, func(i, j int) bool { return sortedLeafIDs[i] < sortedLeafIDs[j] })

	for _, leafID := range sortedLeafIDs {
		prefix := drLocalKIDIndexEntryLeafListPrefix(leafID)
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
			if drLocalKIDIndexLeafIDFromKID(item.kid) != leafID {
				return nil, false, "entry_leaf_mismatch", fmt.Errorf("local kid index entry %x stored under leaf %d but maps to %d", item.kid, leafID, drLocalKIDIndexLeafIDFromKID(item.kid))
			}
			if !kidInAnyReconcileSpan(item.kid, spans) {
				continue
			}
			computedKID := s.scanner.ComputeKID(item.key)
			if computedKID != item.kid {
				return nil, false, "entry_key_mismatch", fmt.Errorf("local kid index entry key %q maps to kid %x, expected %x", item.key, computedKID, item.kid)
			}
			rs.KIDToVID[item.kid] = item.vid
			rs.KIDToKey[item.kid] = item.key
		}
	}
	rs.KeyCount = len(rs.KIDToVID)
	localIndex := reconciler.NewRangeMapIndex(rs.KIDToVID, nil)
	for _, span := range spans {
		actual := reconciler.BuildRangeDigestFromIndex(localIndex, span)
		expected, err := source.RangeDigest(ctx, span)
		if err != nil {
			return nil, false, "digest_read_failed", err
		}
		if !actual.EqualDigest(expected) {
			return nil, false, "span_digest_mismatch", fmt.Errorf("local kid index span digest mismatch for depth %d: index_count=%d digest_count=%d", span.SplitDepth, actual.Count, expected.Count)
		}
	}
	s.localKIDIndexBucketLoads.Add(uint64(len(spans)))
	s.localKIDIndexEntriesLoaded.Add(uint64(len(rs.KIDToVID)))
	return rs, true, "loaded", nil
}

func kidInAnyReconcileSpan(kid [32]byte, spans []reconciler.RangeSpan) bool {
	for _, span := range spans {
		if span.Contains(kid) {
			return true
		}
	}
	return false
}

func (s *drReplicationSecondary) resetLocalKIDIndexSpansFromSet(ctx context.Context, writer physical.Backend, index uint64, rs *reconciler.ReconciliationSet, spans []reconciler.RangeSpan) error {
	if s == nil || writer == nil || index == 0 || len(spans) == 0 {
		return nil
	}
	if rs == nil {
		return fmt.Errorf("local kid index span reset requires reconciliation set")
	}
	if len(rs.KIDToVID) > 0 && rs.KIDToKey == nil {
		return s.invalidatePersistedLocalKIDIndexWithReason(ctx, writer, "span_reset_missing_key_map")
	}
	leafIDs := make(map[uint64]struct{})
	for _, span := range spans {
		startLeaf, endLeaf := localKIDIndexLeafBoundsForSpan(span)
		for leafID := startLeaf; leafID <= endLeaf; leafID++ {
			leafIDs[leafID] = struct{}{}
			if leafID == endLeaf {
				break
			}
		}
	}
	kids := make([][32]byte, 0, len(rs.KIDToVID))
	for kid := range rs.KIDToVID {
		if !kidInAnyReconcileSpan(kid, spans) {
			continue
		}
		kids = append(kids, kid)
	}
	sort.Slice(kids, func(i, j int) bool {
		return hex.EncodeToString(kids[i][:]) < hex.EncodeToString(kids[j][:])
	})

	if txnBackend, ok := writer.(physical.TransactionalBackend); ok {
		return s.resetLocalKIDIndexSpansFromSetTxn(ctx, txnBackend, index, rs, spans, leafIDs, kids)
	}
	for leafID := range leafIDs {
		prefix := drLocalKIDIndexEntryLeafListPrefix(leafID)
		keys, err := writer.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := writer.Delete(ctx, prefix+key); err != nil {
				return err
			}
		}
		if err := writer.Delete(ctx, drLocalKIDIndexDigestStoragePath(leafID)); err != nil {
			return err
		}
	}
	leafDigests := make(map[uint64]drLocalKIDIndexLeafDigestState)
	for _, kid := range kids {
		key := ""
		if rs.KIDToKey != nil {
			key = rs.KIDToKey[kid]
		}
		if key == "" {
			return fmt.Errorf("local kid index span reset missing key for kid %x", kid)
		}
		if isDRNeverReplicatePath(key) {
			continue
		}
		vid := rs.KIDToVID[kid]
		leafID := drLocalKIDIndexLeafIDFromKID(kid)
		state := leafDigests[leafID]
		state.add(kid, vid)
		leafDigests[leafID] = state
		if err := s.putLocalKIDIndexEntry(ctx, writer, kid, key, vid); err != nil {
			return err
		}
	}
	if err := s.putLocalKIDIndexLeafDigests(ctx, writer, leafDigests); err != nil {
		return err
	}
	s.localKIDIndexResets.Add(1)
	return s.persistLocalKIDIndexMeta(ctx, writer, index)
}

func (s *drReplicationSecondary) resetLocalKIDIndexSpansFromSetTxn(ctx context.Context, writer physical.TransactionalBackend, index uint64, rs *reconciler.ReconciliationSet, spans []reconciler.RangeSpan, leafIDs map[uint64]struct{}, kids [][32]byte) error {
	batchSize := s.reconcilePutBatchEntries
	if batchSize <= 0 {
		batchSize = drDefaultReconcilePutBatchEntries
	}
	deletePaths := make([]string, 0)
	for leafID := range leafIDs {
		prefix := drLocalKIDIndexEntryLeafListPrefix(leafID)
		keys, err := writer.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, key := range keys {
			deletePaths = append(deletePaths, prefix+key)
		}
		deletePaths = append(deletePaths, drLocalKIDIndexDigestStoragePath(leafID))
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
	leafDigests := make(map[uint64]drLocalKIDIndexLeafDigestState)
	for _, kid := range kids {
		if !kidInAnyReconcileSpan(kid, spans) {
			continue
		}
		key := ""
		if rs.KIDToKey != nil {
			key = rs.KIDToKey[kid]
		}
		if key == "" {
			return fmt.Errorf("local kid index span reset missing key for kid %x", kid)
		}
		if isDRNeverReplicatePath(key) {
			continue
		}
		vid := rs.KIDToVID[kid]
		leafID := drLocalKIDIndexLeafIDFromKID(kid)
		state := leafDigests[leafID]
		state.add(kid, vid)
		leafDigests[leafID] = state
		data, err := s.localKIDIndexEntryData(kid, key, vid)
		if err != nil {
			return err
		}
		entries = append(entries, &physical.Entry{
			Key:   drLocalKIDIndexEntryStoragePath(kid),
			Value: data,
		})
	}
	digestEntries, err := s.localKIDIndexLeafDigestEntries(leafDigests)
	if err != nil {
		return err
	}
	entries = append(entries, digestEntries...)
	for i := 0; i < len(entries); i += batchSize {
		end := i + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := s.applyLocalKIDIndexPutBatch(ctx, writer, entries[i:end]); err != nil {
			return err
		}
	}
	s.localKIDIndexResets.Add(1)
	return s.persistLocalKIDIndexMeta(ctx, writer, index)
}

func beginLocalKIDIndexReadSnapshot(ctx context.Context, reader physical.Backend) (physical.Backend, func(), error) {
	if txReader, ok := reader.(physical.Transactional); ok {
		txn, err := txReader.BeginReadOnlyTx(ctx)
		if err != nil {
			return nil, nil, err
		}
		return txn, func() {
			_ = txn.Rollback(ctx)
		}, nil
	}
	return reader, nil, nil
}

func (s *drReplicationSecondary) loadLocalKIDIndexCursor(ctx context.Context, reader physical.Backend) (uint64, bool, error) {
	if s == nil || reader == nil {
		return 0, false, nil
	}
	entry, err := reader.Get(ctx, drFlatAccumulatorCursorStoragePath)
	if err != nil {
		return 0, false, err
	}
	if entry == nil || len(entry.Value) == 0 {
		return 0, false, nil
	}
	var cursor drFlatAccumulatorPersistedCursor
	if err := json.Unmarshal(entry.Value, &cursor); err != nil {
		return 0, false, err
	}
	if err := s.validateFlatAccumulatorCursor(&cursor); err != nil {
		return 0, false, err
	}
	return cursor.CommitIndex, true, nil
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
	key string
	vid [32]byte
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
	if len(item.KID) != 32 {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry has invalid kid length")
	}
	if item.Key == "" {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry has empty key")
	}
	if len(item.VID) != 32 {
		return drDecodedLocalKIDIndexEntry{}, fmt.Errorf("local kid index entry has invalid vid length")
	}
	var out drDecodedLocalKIDIndexEntry
	copy(out.kid[:], item.KID)
	out.key = item.Key
	copy(out.vid[:], item.VID)
	return out, nil
}
