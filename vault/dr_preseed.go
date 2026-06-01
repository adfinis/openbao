// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drPreSeedManifestVersion          = 1
	drPreSeedBundleVersion            = 1
	drPreSeedAcceptedRecordVersion    = 1
	drPreSeedLocalOnlyScrubVersion    = 1
	drPreSeedBundleIntegrityAlgorithm = "sha256"
	drPreSeedBundleFormatInlineJSONV1 = "inline-json-v1"
	drPreSeedBundleFormatSegmentedV1  = "segmented-json-v1"
	drPreSeedAcceptedStoragePath      = "core/cluster/local/dr/preseed/accepted"
	drPreSeedBundleMaxBytes           = 512 << 20
)

var drPreSeedBootstrapOwnedExactPaths = map[string]bool{
	"core/root-key": true,
}

func isDRPreSeedBulkExcludedPath(path string) bool {
	return isDRReconcileExcludedPath(path) || drPathMatches(path, drPreSeedBootstrapOwnedExactPaths, nil)
}

// DRPreSeedManifest is the relationship-bound metadata that lets an operator
// restore a DR-aware base copy before the secondary catches up through normal
// stream replay or checkpoint reconciliation.
type DRPreSeedManifest struct {
	Version                  int                          `json:"version"`
	PrimaryClusterID         string                       `json:"primary_cluster_id"`
	RelationshipID           string                       `json:"relationship_id"`
	CheckpointID             string                       `json:"checkpoint_id"`
	CheckpointIndex          uint64                       `json:"checkpoint_index"`
	CreatedAtUnix            int64                        `json:"created_at_unix"`
	ExpiresAtUnix            int64                        `json:"expires_at_unix,omitempty"`
	ReplSaltSHA256           []byte                       `json:"repl_salt_sha256"`
	RangePlanVersion         uint32                       `json:"range_plan_version"`
	RangeBits                int                          `json:"range_bits"`
	RangeCount               int                          `json:"range_count"`
	ChecksumAlgorithm        string                       `json:"checksum_algorithm"`
	ValueDomain              string                       `json:"value_domain"`
	LocalOnlyScrubVersion    int                          `json:"local_only_scrub_version"`
	LocalOnlyExactPaths      []string                     `json:"local_only_exact_paths"`
	LocalOnlyPrefixes        []string                     `json:"local_only_prefixes"`
	AccumulatorSnapshotVer   int                          `json:"accumulator_snapshot_version,omitempty"`
	LocalKIDIndexVersion     int                          `json:"local_kid_index_version,omitempty"`
	BundleFormat             string                       `json:"bundle_format,omitempty"`
	BundleSegments           []DRPreSeedSegmentDescriptor `json:"bundle_segments,omitempty"`
	BundleIntegrityAlgorithm string                       `json:"bundle_integrity_algorithm"`
	BundleIntegritySHA256    []byte                       `json:"bundle_integrity_sha256"`
}

// DRPreSeedBundle is a checkpoint-bound, relationship-bound physical seed
// artifact containing only replicated below-barrier storage entries.
type DRPreSeedBundle struct {
	Version    int                    `json:"version"`
	Manifest   DRPreSeedManifest      `json:"manifest"`
	EntryCount int                    `json:"entry_count"`
	Entries    []DRPreSeedBundleEntry `json:"entries"`
}

type DRPreSeedBundleEntry struct {
	Key      string `json:"key"`
	Value    []byte `json:"value"`
	SealWrap bool   `json:"seal_wrap,omitempty"`
	KID      []byte `json:"kid"`
	VID      []byte `json:"vid"`
}

type DRPreSeedSegmentDescriptor struct {
	Index      int    `json:"index"`
	EntryCount int    `json:"entry_count"`
	ByteCount  uint64 `json:"byte_count"`
	SHA256     []byte `json:"sha256"`
	FirstKey   string `json:"first_key,omitempty"`
	LastKey    string `json:"last_key,omitempty"`
}

type drPreSeedAcceptedRecord struct {
	Version                  int               `json:"version"`
	AcceptedAtUnix           int64             `json:"accepted_at_unix"`
	Manifest                 DRPreSeedManifest `json:"manifest"`
	ConfirmStorageRestored   bool              `json:"confirm_storage_restored"`
	ConfirmLocalOnlyScrubbed bool              `json:"confirm_local_only_scrubbed"`
}

type drPreSeedBundleIntegrityPayload struct {
	Version          int                                    `json:"version"`
	PrimaryClusterID string                                 `json:"primary_cluster_id"`
	RelationshipID   string                                 `json:"relationship_id"`
	CheckpointID     string                                 `json:"checkpoint_id"`
	CheckpointIndex  uint64                                 `json:"checkpoint_index"`
	BundleFormat     string                                 `json:"bundle_format"`
	BundleSegments   []DRPreSeedSegmentDescriptor           `json:"bundle_segments,omitempty"`
	EntryCount       int                                    `json:"entry_count"`
	Entries          []drPreSeedBundleIntegrityPayloadEntry `json:"entries"`
}

type drPreSeedBundleIntegrityPayloadEntry struct {
	Key         string `json:"key"`
	SealWrap    bool   `json:"seal_wrap,omitempty"`
	KID         []byte `json:"kid"`
	VID         []byte `json:"vid"`
	ValueSHA256 []byte `json:"value_sha256"`
}

func (m *drRelationshipManager) GeneratePreSeedManifest(ctx context.Context, relationshipID string, bundleIntegritySHA256 []byte, ttl time.Duration) (*DRPreSeedManifest, error) {
	if m == nil {
		return nil, fmt.Errorf("DR relationship manager is nil")
	}
	if relationshipID == "" {
		return nil, fmt.Errorf("relationship_id is required")
	}
	if len(bundleIntegritySHA256) != sha256.Size {
		return nil, fmt.Errorf("bundle_integrity_sha256 must be %d bytes", sha256.Size)
	}

	m.mu.Lock()
	if m.config.Mode != DRModePrimary {
		m.mu.Unlock()
		return nil, fmt.Errorf("not in DR primary mode")
	}
	clusterID := m.config.ClusterID
	replSalt := append([]byte(nil), m.config.ReplSalt...)
	promotion := m.config.Promotion
	primary := m.primary
	m.mu.Unlock()

	if primary == nil {
		return nil, fmt.Errorf("DR primary runtime not initialized")
	}
	if isStalePostPromotionLineage(promotion, clusterID, relationshipID) {
		return nil, fmt.Errorf("relationship references stale pre-promotion DR lineage")
	}
	if len(replSalt) != drReplSaltLen {
		return nil, fmt.Errorf("DR primary has invalid replication salt")
	}

	now := time.Now().UTC()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if err := validatePreSeedRelationshipState(rel, now); err != nil {
		return nil, err
	}

	checkpoint, err := primary.BuildPreSeedCheckpoint(ctx, relationshipID)
	if err != nil {
		return nil, err
	}

	rel, err = m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if err := validatePreSeedRelationshipState(rel, time.Now().UTC()); err != nil {
		return nil, err
	}

	return newDRPreSeedManifest(clusterID, relationshipID, replSalt, checkpoint, bundleIntegritySHA256, ttl, now), nil
}

func (m *drRelationshipManager) GeneratePreSeedBundle(ctx context.Context, relationshipID string, ttl time.Duration) (*DRPreSeedBundle, error) {
	if m == nil {
		return nil, fmt.Errorf("DR relationship manager is nil")
	}
	if relationshipID == "" {
		return nil, fmt.Errorf("relationship_id is required")
	}

	m.mu.Lock()
	if m.config.Mode != DRModePrimary {
		m.mu.Unlock()
		return nil, fmt.Errorf("not in DR primary mode")
	}
	clusterID := m.config.ClusterID
	replSalt := append([]byte(nil), m.config.ReplSalt...)
	promotion := m.config.Promotion
	primary := m.primary
	m.mu.Unlock()

	if primary == nil {
		return nil, fmt.Errorf("DR primary runtime not initialized")
	}
	if isStalePostPromotionLineage(promotion, clusterID, relationshipID) {
		return nil, fmt.Errorf("relationship references stale pre-promotion DR lineage")
	}
	if len(replSalt) != drReplSaltLen {
		return nil, fmt.Errorf("DR primary has invalid replication salt")
	}

	now := time.Now().UTC()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if err := validatePreSeedRelationshipState(rel, now); err != nil {
		return nil, err
	}

	checkpoint, entries, err := primary.BuildPreSeedBundle(ctx, relationshipID)
	if err != nil {
		return nil, err
	}

	rel, err = m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if err := validatePreSeedRelationshipState(rel, time.Now().UTC()); err != nil {
		return nil, err
	}

	manifest := newDRPreSeedManifest(clusterID, relationshipID, replSalt, checkpoint, make([]byte, sha256.Size), ttl, now)
	bundle := &DRPreSeedBundle{
		Version:    drPreSeedBundleVersion,
		Manifest:   *manifest,
		EntryCount: len(entries),
		Entries:    entries,
	}
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		return nil, err
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	return bundle, nil
}

func (m *drRelationshipManager) ValidatePreSeedManifest(manifest *DRPreSeedManifest, token *DRActivationToken, now time.Time) error {
	if m == nil {
		return fmt.Errorf("DR relationship manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return validateDRPreSeedManifest(manifest, token, m.config.Promotion, now)
}

func (m *drRelationshipManager) AcceptPreSeedManifest(ctx context.Context, manifest *DRPreSeedManifest, token *DRActivationToken, now time.Time, confirmStorageRestored bool, confirmLocalOnlyScrubbed bool) error {
	if m == nil {
		return fmt.Errorf("DR relationship manager is nil")
	}
	if !confirmStorageRestored {
		return fmt.Errorf("confirm_storage_restored=true is required")
	}
	if !confirmLocalOnlyScrubbed {
		return fmt.Errorf("confirm_local_only_scrubbed=true is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.Mode != DRModeDisabled {
		return fmt.Errorf("pre-seed manifest can only be accepted before DR secondary mode is enabled")
	}
	if err := validateDRPreSeedManifest(manifest, token, m.config.Promotion, now); err != nil {
		return err
	}
	record := drPreSeedAcceptedRecord{
		Version:                  drPreSeedAcceptedRecordVersion,
		AcceptedAtUnix:           now.Unix(),
		Manifest:                 *manifest,
		ConfirmStorageRestored:   confirmStorageRestored,
		ConfirmLocalOnlyScrubbed: confirmLocalOnlyScrubbed,
	}
	return m.saveAcceptedPreSeedLocked(ctx, &record)
}

func (m *drRelationshipManager) ImportPreSeedBundle(ctx context.Context, bundle *DRPreSeedBundle, token *DRActivationToken, now time.Time, confirmReplaceReplicatedStorage bool) error {
	if m == nil {
		return fmt.Errorf("DR relationship manager is nil")
	}
	if !confirmReplaceReplicatedStorage {
		return fmt.Errorf("confirm_replace_replicated_storage=true is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.Mode != DRModeDisabled {
		return fmt.Errorf("pre-seed bundle can only be imported before DR secondary mode is enabled")
	}
	if err := validateDRPreSeedBundle(bundle, token, m.config.Promotion, now); err != nil {
		return err
	}

	existing, err := scanDRPreSeedReplicatedKeys(ctx, m.core.physical, token.ReplSalt, m.logger)
	if err != nil {
		return err
	}

	writer := m.core.physical
	var tx physical.Transaction
	if txBackend, ok := m.core.physical.(physical.Transactional); ok {
		tx, err = txBackend.BeginTx(ctx)
		if err != nil {
			return fmt.Errorf("begin DR pre-seed import transaction: %w", err)
		}
		writer = tx
		defer tx.Rollback(ctx)
	}

	existingKeys := make([]string, 0, len(existing.KIDToKey))
	for _, key := range existing.KIDToKey {
		existingKeys = append(existingKeys, key)
	}
	sort.Strings(existingKeys)
	for _, key := range existingKeys {
		if err := writer.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete existing replicated pre-seed key %q: %w", key, err)
		}
	}

	entries := append([]DRPreSeedBundleEntry(nil), bundle.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		return bytes.Compare(entries[i].KID, entries[j].KID) < 0
	})
	for _, entry := range entries {
		if err := writer.Put(ctx, &physical.Entry{
			Key:      entry.Key,
			Value:    append([]byte(nil), entry.Value...),
			SealWrap: entry.SealWrap,
		}); err != nil {
			return fmt.Errorf("write pre-seed key %q: %w", entry.Key, err)
		}
	}

	record := drPreSeedAcceptedRecord{
		Version:                  drPreSeedAcceptedRecordVersion,
		AcceptedAtUnix:           now.Unix(),
		Manifest:                 bundle.Manifest,
		ConfirmStorageRestored:   true,
		ConfirmLocalOnlyScrubbed: true,
	}
	if err := saveAcceptedPreSeed(ctx, writer, &record); err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit DR pre-seed import transaction: %w", err)
		}
	}
	return nil
}

func validateDRPreSeedManifest(manifest *DRPreSeedManifest, token *DRActivationToken, promotion *DRPromotionRecord, now time.Time) error {
	if manifest == nil {
		return fmt.Errorf("pre-seed manifest is nil")
	}
	if token == nil {
		return fmt.Errorf("activation token is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if manifest.Version != drPreSeedManifestVersion {
		return fmt.Errorf("unsupported pre-seed manifest version %d", manifest.Version)
	}
	if token.ClusterID == "" || manifest.PrimaryClusterID == "" || manifest.PrimaryClusterID != token.ClusterID {
		return fmt.Errorf("primary_cluster_id mismatch")
	}
	if token.RelationshipID == "" || manifest.RelationshipID == "" || manifest.RelationshipID != token.RelationshipID {
		return fmt.Errorf("relationship_id mismatch")
	}
	if isStalePostPromotionActivationToken(promotion, token) || isStalePostPromotionLineage(promotion, manifest.PrimaryClusterID, manifest.RelationshipID) {
		return fmt.Errorf("pre-seed manifest references stale pre-promotion DR lineage")
	}
	if len(token.ReplSalt) != drReplSaltLen {
		return fmt.Errorf("activation token has invalid repl_salt length")
	}
	if len(manifest.ReplSaltSHA256) != sha256.Size {
		return fmt.Errorf("repl_salt_sha256 missing or invalid")
	}
	sum := sha256.Sum256(token.ReplSalt)
	if !bytes.Equal(manifest.ReplSaltSHA256, sum[:]) {
		return fmt.Errorf("repl_salt_sha256 mismatch")
	}

	if manifest.CheckpointID == "" {
		return fmt.Errorf("checkpoint_id is required")
	}
	if manifest.CheckpointIndex == 0 {
		return fmt.Errorf("checkpoint_index is required")
	}
	if manifest.CreatedAtUnix == 0 {
		return fmt.Errorf("created_at_unix is required")
	}
	if manifest.ExpiresAtUnix != 0 && now.Unix() > manifest.ExpiresAtUnix {
		return fmt.Errorf("pre-seed manifest expired")
	}

	if manifest.RangePlanVersion != reconciler.RangePlanVersion {
		return fmt.Errorf("range_plan_version mismatch")
	}
	if manifest.RangeBits != drFlatAccumulatorRangeBits {
		return fmt.Errorf("range_bits mismatch")
	}
	if manifest.RangeCount != drRangeMaxTotalRanges {
		return fmt.Errorf("range_count mismatch")
	}
	if manifest.ChecksumAlgorithm != drFlatAccumulatorChecksumAlgorithm {
		return fmt.Errorf("checksum_algorithm mismatch")
	}
	if manifest.ValueDomain != string(reconciler.ValueDomainCiphertext) {
		return fmt.Errorf("value_domain mismatch")
	}
	if manifest.AccumulatorSnapshotVer != 0 && manifest.AccumulatorSnapshotVer != drFlatAccumulatorSnapshotVersion {
		return fmt.Errorf("accumulator_snapshot_version mismatch")
	}
	if manifest.LocalKIDIndexVersion != 0 && manifest.LocalKIDIndexVersion != drLocalKIDIndexVersion {
		return fmt.Errorf("local_kid_index_version mismatch")
	}
	if err := validateDRPreSeedBundleArtifactMetadata(manifest); err != nil {
		return err
	}
	if manifest.BundleIntegrityAlgorithm != drPreSeedBundleIntegrityAlgorithm {
		return fmt.Errorf("bundle_integrity_algorithm mismatch")
	}
	if len(manifest.BundleIntegritySHA256) != sha256.Size {
		return fmt.Errorf("bundle_integrity_sha256 missing or invalid")
	}
	if err := validateDRPreSeedLocalOnlyScrub(manifest); err != nil {
		return err
	}
	return nil
}

func validateDRPreSeedBundle(bundle *DRPreSeedBundle, token *DRActivationToken, promotion *DRPromotionRecord, now time.Time) error {
	if bundle == nil {
		return fmt.Errorf("pre-seed bundle is nil")
	}
	if bundle.Version != drPreSeedBundleVersion {
		return fmt.Errorf("unsupported pre-seed bundle version %d", bundle.Version)
	}
	if bundle.EntryCount != len(bundle.Entries) {
		return fmt.Errorf("pre-seed bundle entry_count mismatch")
	}
	if err := validateDRPreSeedManifest(&bundle.Manifest, token, promotion, now); err != nil {
		return err
	}
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		return err
	}
	if !bytes.Equal(bundle.Manifest.BundleIntegritySHA256, sum) {
		return fmt.Errorf("pre-seed bundle integrity mismatch")
	}

	scanner := drPreSeedScanner(token.ReplSalt, nil)
	seenKeys := make(map[string]struct{}, len(bundle.Entries))
	seenKIDs := make(map[[32]byte]struct{}, len(bundle.Entries))
	for _, entry := range bundle.Entries {
		if entry.Key == "" {
			return fmt.Errorf("pre-seed bundle entry key is required")
		}
		if isDRPreSeedBulkExcludedPath(entry.Key) {
			return fmt.Errorf("pre-seed bundle contains local-only or excluded path %q", entry.Key)
		}
		if len(entry.KID) != sha256.Size {
			return fmt.Errorf("pre-seed bundle entry %q has invalid kid", entry.Key)
		}
		if len(entry.VID) != sha256.Size {
			return fmt.Errorf("pre-seed bundle entry %q has invalid vid", entry.Key)
		}
		if _, ok := seenKeys[entry.Key]; ok {
			return fmt.Errorf("pre-seed bundle contains duplicate key %q", entry.Key)
		}
		seenKeys[entry.Key] = struct{}{}

		var kid [32]byte
		copy(kid[:], entry.KID)
		if _, ok := seenKIDs[kid]; ok {
			return fmt.Errorf("pre-seed bundle contains duplicate kid for key %q", entry.Key)
		}
		seenKIDs[kid] = struct{}{}
		expectedKID := scanner.ComputeKID(entry.Key)
		if kid != expectedKID {
			return fmt.Errorf("pre-seed bundle entry %q kid mismatch", entry.Key)
		}

		var vid [32]byte
		copy(vid[:], entry.VID)
		expectedVID := scanner.ComputeVIDWithSealWrap(entry.Value, entry.SealWrap)
		if vid != expectedVID {
			return fmt.Errorf("pre-seed bundle entry %q vid mismatch", entry.Key)
		}
	}
	if err := validateDRPreSeedBundleSegments(bundle); err != nil {
		return err
	}
	return nil
}

func newDRPreSeedManifest(clusterID, relationshipID string, replSalt []byte, checkpoint *CheckpointResponse, bundleIntegritySHA256 []byte, ttl time.Duration, now time.Time) *DRPreSeedManifest {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	replSaltHash := sha256.Sum256(replSalt)
	manifest := &DRPreSeedManifest{
		Version:                  drPreSeedManifestVersion,
		PrimaryClusterID:         clusterID,
		RelationshipID:           relationshipID,
		CreatedAtUnix:            now.Unix(),
		ReplSaltSHA256:           replSaltHash[:],
		RangePlanVersion:         reconciler.RangePlanVersion,
		RangeBits:                drFlatAccumulatorRangeBits,
		RangeCount:               drRangeMaxTotalRanges,
		ChecksumAlgorithm:        drFlatAccumulatorChecksumAlgorithm,
		ValueDomain:              string(reconciler.ValueDomainCiphertext),
		LocalOnlyScrubVersion:    drPreSeedLocalOnlyScrubVersion,
		LocalOnlyExactPaths:      currentDRPreSeedLocalOnlyExactPaths(),
		LocalOnlyPrefixes:        currentDRPreSeedLocalOnlyPrefixes(),
		AccumulatorSnapshotVer:   drFlatAccumulatorSnapshotVersion,
		LocalKIDIndexVersion:     drLocalKIDIndexVersion,
		BundleFormat:             drPreSeedBundleFormatInlineJSONV1,
		BundleIntegrityAlgorithm: drPreSeedBundleIntegrityAlgorithm,
		BundleIntegritySHA256:    append([]byte(nil), bundleIntegritySHA256...),
	}
	if checkpoint != nil {
		manifest.CheckpointID = checkpoint.GetCheckpointId()
		manifest.CheckpointIndex = checkpoint.GetCommitIndex()
		if checkpoint.GetRangePlanVersion() != 0 {
			manifest.RangePlanVersion = checkpoint.GetRangePlanVersion()
		}
	}
	if ttl > 0 {
		manifest.ExpiresAtUnix = now.Add(ttl).Unix()
	}
	return manifest
}

func validatePreSeedRelationshipState(rel *DRRelationship, now time.Time) error {
	if rel == nil {
		return fmt.Errorf("relationship is required")
	}
	switch rel.State {
	case DRRelationshipStatePending:
		if rel.ExpiresAt > 0 && rel.ExpiresAt <= now.Unix() {
			return fmt.Errorf("relationship %q bootstrap material expired", rel.RelationshipID)
		}
	case DRRelationshipStateRegistered, DRRelationshipStateActive:
	case DRRelationshipStateRevoked:
		return fmt.Errorf("relationship %q is revoked", rel.RelationshipID)
	default:
		return fmt.Errorf("relationship %q is not usable for pre-seed: %s", rel.RelationshipID, rel.State)
	}
	return nil
}

func validateDRPreSeedLocalOnlyScrub(manifest *DRPreSeedManifest) error {
	if manifest.LocalOnlyScrubVersion != drPreSeedLocalOnlyScrubVersion {
		return fmt.Errorf("local-only scrub version mismatch")
	}
	if len(manifest.LocalOnlyExactPaths) == 0 || len(manifest.LocalOnlyPrefixes) == 0 {
		return fmt.Errorf("local-only scrub metadata missing")
	}
	if missing := missingStrings(currentDRPreSeedLocalOnlyExactPaths(), manifest.LocalOnlyExactPaths); len(missing) > 0 {
		return fmt.Errorf("local-only scrub metadata missing exact path %q", missing[0])
	}
	if missing := missingStrings(currentDRPreSeedLocalOnlyPrefixes(), manifest.LocalOnlyPrefixes); len(missing) > 0 {
		return fmt.Errorf("local-only scrub metadata missing prefix %q", missing[0])
	}
	return nil
}

func normalizedDRPreSeedBundleFormat(format string) string {
	if format == "" {
		return drPreSeedBundleFormatInlineJSONV1
	}
	return format
}

func validateDRPreSeedBundleArtifactMetadata(manifest *DRPreSeedManifest) error {
	switch normalizedDRPreSeedBundleFormat(manifest.BundleFormat) {
	case drPreSeedBundleFormatInlineJSONV1:
		if len(manifest.BundleSegments) != 0 {
			return fmt.Errorf("inline pre-seed bundle must not declare segment metadata")
		}
		return nil
	case drPreSeedBundleFormatSegmentedV1:
		return validateDRPreSeedSegmentDescriptors(manifest.BundleSegments)
	default:
		return fmt.Errorf("unsupported pre-seed bundle format %q", manifest.BundleFormat)
	}
}

func validateDRPreSeedSegmentDescriptors(segments []DRPreSeedSegmentDescriptor) error {
	if len(segments) == 0 {
		return fmt.Errorf("segmented pre-seed bundle requires segment metadata")
	}
	var previousLastKey string
	for i, segment := range segments {
		if segment.Index != i {
			return fmt.Errorf("pre-seed bundle segment index mismatch at offset %d", i)
		}
		if segment.EntryCount <= 0 {
			return fmt.Errorf("pre-seed bundle segment %d has invalid entry_count", i)
		}
		if segment.ByteCount == 0 {
			return fmt.Errorf("pre-seed bundle segment %d has invalid byte_count", i)
		}
		if len(segment.SHA256) != sha256.Size {
			return fmt.Errorf("pre-seed bundle segment %d has invalid sha256", i)
		}
		if segment.FirstKey == "" || segment.LastKey == "" {
			return fmt.Errorf("pre-seed bundle segment %d is missing key bounds", i)
		}
		if segment.FirstKey > segment.LastKey {
			return fmt.Errorf("pre-seed bundle segment %d has invalid key bounds", i)
		}
		if i > 0 && previousLastKey >= segment.FirstKey {
			return fmt.Errorf("pre-seed bundle segment %d key bounds overlap previous segment", i)
		}
		previousLastKey = segment.LastKey
	}
	return nil
}

func validateDRPreSeedBundleSegments(bundle *DRPreSeedBundle) error {
	if normalizedDRPreSeedBundleFormat(bundle.Manifest.BundleFormat) != drPreSeedBundleFormatSegmentedV1 {
		return nil
	}

	entries := canonicalDRPreSeedBundleEntries(bundle.Entries)
	offset := 0
	for _, expected := range bundle.Manifest.BundleSegments {
		if expected.EntryCount > len(entries)-offset {
			return fmt.Errorf("pre-seed bundle segment %d entry_count exceeds bundle entries", expected.Index)
		}
		actual, err := buildDRPreSeedSegmentDescriptor(expected.Index, entries[offset:offset+expected.EntryCount])
		if err != nil {
			return err
		}
		if expected.ByteCount != actual.ByteCount ||
			expected.FirstKey != actual.FirstKey ||
			expected.LastKey != actual.LastKey ||
			!bytes.Equal(expected.SHA256, actual.SHA256) {
			return fmt.Errorf("pre-seed bundle segment %d metadata mismatch", expected.Index)
		}
		offset += expected.EntryCount
	}
	if offset != len(entries) {
		return fmt.Errorf("pre-seed bundle segment metadata does not cover all entries")
	}
	return nil
}

func (m *drRelationshipManager) loadAcceptedPreSeedLocked(ctx context.Context) (*drPreSeedAcceptedRecord, bool, error) {
	if m == nil || m.core == nil || m.core.physical == nil {
		return nil, false, nil
	}
	entry, err := m.core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read accepted DR pre-seed record: %w", err)
	}
	if entry == nil || len(entry.Value) == 0 {
		return nil, false, nil
	}
	var record drPreSeedAcceptedRecord
	if err := json.Unmarshal(entry.Value, &record); err != nil {
		return nil, false, fmt.Errorf("failed to decode accepted DR pre-seed record: %w", err)
	}
	if record.Version != drPreSeedAcceptedRecordVersion {
		return nil, false, fmt.Errorf("unsupported accepted DR pre-seed record version %d", record.Version)
	}
	if !record.ConfirmStorageRestored {
		return nil, false, fmt.Errorf("accepted DR pre-seed record is missing storage restore confirmation")
	}
	if !record.ConfirmLocalOnlyScrubbed {
		return nil, false, fmt.Errorf("accepted DR pre-seed record is missing local-only scrub confirmation")
	}
	return &record, true, nil
}

func (m *drRelationshipManager) saveAcceptedPreSeedLocked(ctx context.Context, record *drPreSeedAcceptedRecord) error {
	if m == nil || m.core == nil || m.core.physical == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed record")
	}
	return saveAcceptedPreSeed(ctx, m.core.physical, record)
}

func saveAcceptedPreSeed(ctx context.Context, writer physical.Backend, record *drPreSeedAcceptedRecord) error {
	if writer == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed record")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal accepted DR pre-seed record: %w", err)
	}
	return writer.Put(ctx, &physical.Entry{
		Key:   drPreSeedAcceptedStoragePath,
		Value: data,
	})
}

func (m *drRelationshipManager) deleteAcceptedPreSeedLocked(ctx context.Context) error {
	if m == nil || m.core == nil || m.core.physical == nil {
		return nil
	}
	if err := m.core.physical.Delete(ctx, drPreSeedAcceptedStoragePath); err != nil {
		return fmt.Errorf("failed to delete accepted DR pre-seed record: %w", err)
	}
	return nil
}

func (m *drRelationshipManager) applyAcceptedPreSeedLocked(ctx context.Context, record *drPreSeedAcceptedRecord, token *DRActivationToken) error {
	if record == nil {
		return nil
	}
	if err := validateDRPreSeedManifest(&record.Manifest, token, m.config.Promotion, time.Now().UTC()); err != nil {
		return fmt.Errorf("accepted DR pre-seed record is incompatible with activation token: %w", err)
	}
	if m.secondary == nil {
		return fmt.Errorf("DR secondary runtime not initialized")
	}
	index := record.Manifest.CheckpointIndex
	if err := m.secondary.commitCheckpointIndex(index); err != nil {
		return fmt.Errorf("failed to persist DR pre-seed checkpoint baseline: %w", err)
	}
	m.secondary.setLastAppliedIndex(index)
	if err := m.secondary.persistStreamAppliedIndex(ctx, m.core.physical, index); err != nil {
		return fmt.Errorf("failed to persist DR pre-seed stream baseline: %w", err)
	}
	return m.deleteAcceptedPreSeedLocked(ctx)
}

func currentDRPreSeedLocalOnlyExactPaths() []string {
	paths := make([]string, 0, len(drNeverReplicateExactPaths))
	for path := range drNeverReplicateExactPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func currentDRPreSeedLocalOnlyPrefixes() []string {
	prefixes := append([]string(nil), drNeverReplicatePrefixes...)
	sort.Strings(prefixes)
	return prefixes
}

func scanDRPreSeedReplicatedKeys(ctx context.Context, backend physical.Backend, replSalt []byte, logger log.Logger) (*reconciler.ReconciliationSet, error) {
	if backend == nil {
		return nil, fmt.Errorf("physical storage is not available for DR pre-seed import")
	}
	scanner := drPreSeedScanner(replSalt, logger)
	rs, err := scanner.ScanPhysical(ctx, backend, reconciler.Checkpoint{ID: "preseed-import-existing"})
	if err != nil {
		return nil, fmt.Errorf("scan existing replicated pre-seed keys: %w", err)
	}
	return rs, nil
}

func drPreSeedScanner(replSalt []byte, logger log.Logger) *reconciler.Scanner {
	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true
	config.RequireTransactionalSnapshot = false
	config.ValueDomain = reconciler.ValueDomainCiphertext
	config.ExcludePathFunc = isDRPreSeedBulkExcludedPath
	if logger != nil {
		config.Logger = logger.Named("preseed")
	}
	return reconciler.NewScanner(config)
}

func buildDRPreSeedBundleSegmentPlan(bundle *DRPreSeedBundle, maxSegmentBytes int) ([]DRPreSeedSegmentDescriptor, error) {
	if bundle == nil {
		return nil, fmt.Errorf("pre-seed bundle is nil")
	}
	if maxSegmentBytes <= 0 {
		return nil, fmt.Errorf("pre-seed segment max bytes must be positive")
	}
	entries := canonicalDRPreSeedBundleEntries(bundle.Entries)
	if len(entries) == 0 {
		return nil, nil
	}

	segments := make([]DRPreSeedSegmentDescriptor, 0)
	current := make([]DRPreSeedBundleEntry, 0)
	for _, entry := range entries {
		candidate := append(append([]DRPreSeedBundleEntry(nil), current...), entry)
		candidateDescriptor, err := buildDRPreSeedSegmentDescriptor(len(segments), candidate)
		if err != nil {
			return nil, err
		}
		if len(current) > 0 && candidateDescriptor.ByteCount > uint64(maxSegmentBytes) {
			descriptor, err := buildDRPreSeedSegmentDescriptor(len(segments), current)
			if err != nil {
				return nil, err
			}
			segments = append(segments, descriptor)
			current = []DRPreSeedBundleEntry{entry}
			candidateDescriptor, err = buildDRPreSeedSegmentDescriptor(len(segments), current)
			if err != nil {
				return nil, err
			}
		} else {
			current = candidate
		}
		if candidateDescriptor.ByteCount > uint64(maxSegmentBytes) {
			return nil, fmt.Errorf("pre-seed bundle entry %q exceeds segment max bytes", entry.Key)
		}
	}
	if len(current) > 0 {
		descriptor, err := buildDRPreSeedSegmentDescriptor(len(segments), current)
		if err != nil {
			return nil, err
		}
		segments = append(segments, descriptor)
	}
	return segments, nil
}

func buildDRPreSeedSegmentDescriptor(index int, entries []DRPreSeedBundleEntry) (DRPreSeedSegmentDescriptor, error) {
	if len(entries) == 0 {
		return DRPreSeedSegmentDescriptor{}, fmt.Errorf("pre-seed segment %d has no entries", index)
	}
	data, err := marshalDRPreSeedSegmentPayload(entries)
	if err != nil {
		return DRPreSeedSegmentDescriptor{}, err
	}
	sum := sha256.Sum256(data)
	return DRPreSeedSegmentDescriptor{
		Index:      index,
		EntryCount: len(entries),
		ByteCount:  uint64(len(data)),
		SHA256:     sum[:],
		FirstKey:   entries[0].Key,
		LastKey:    entries[len(entries)-1].Key,
	}, nil
}

func marshalDRPreSeedSegmentPayload(entries []DRPreSeedBundleEntry) ([]byte, error) {
	payload := struct {
		Version int                                    `json:"version"`
		Entries []drPreSeedBundleIntegrityPayloadEntry `json:"entries"`
	}{
		Version: drPreSeedBundleVersion,
		Entries: make([]drPreSeedBundleIntegrityPayloadEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		valueHash := sha256.Sum256(entry.Value)
		payload.Entries = append(payload.Entries, drPreSeedBundleIntegrityPayloadEntry{
			Key:         entry.Key,
			SealWrap:    entry.SealWrap,
			KID:         append([]byte(nil), entry.KID...),
			VID:         append([]byte(nil), entry.VID...),
			ValueSHA256: valueHash[:],
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal pre-seed segment payload: %w", err)
	}
	return data, nil
}

func canonicalDRPreSeedBundleEntries(raw []DRPreSeedBundleEntry) []DRPreSeedBundleEntry {
	entries := make([]DRPreSeedBundleEntry, 0, len(raw))
	entries = append(entries, raw...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		return bytes.Compare(entries[i].KID, entries[j].KID) < 0
	})
	return entries
}

func computeDRPreSeedBundleIntegrity(bundle *DRPreSeedBundle) ([]byte, error) {
	if bundle == nil {
		return nil, fmt.Errorf("pre-seed bundle is nil")
	}
	entries := canonicalDRPreSeedBundleEntries(bundle.Entries)

	payload := drPreSeedBundleIntegrityPayload{
		Version:          bundle.Version,
		PrimaryClusterID: bundle.Manifest.PrimaryClusterID,
		RelationshipID:   bundle.Manifest.RelationshipID,
		CheckpointID:     bundle.Manifest.CheckpointID,
		CheckpointIndex:  bundle.Manifest.CheckpointIndex,
		BundleFormat:     normalizedDRPreSeedBundleFormat(bundle.Manifest.BundleFormat),
		BundleSegments:   append([]DRPreSeedSegmentDescriptor(nil), bundle.Manifest.BundleSegments...),
		EntryCount:       bundle.EntryCount,
		Entries:          make([]drPreSeedBundleIntegrityPayloadEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		valueHash := sha256.Sum256(entry.Value)
		payload.Entries = append(payload.Entries, drPreSeedBundleIntegrityPayloadEntry{
			Key:         entry.Key,
			SealWrap:    entry.SealWrap,
			KID:         append([]byte(nil), entry.KID...),
			VID:         append([]byte(nil), entry.VID...),
			ValueSHA256: valueHash[:],
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal pre-seed bundle integrity payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

func missingStrings(required, got []string) []string {
	seen := make(map[string]struct{}, len(got))
	for _, value := range got {
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	missing := make([]string, 0)
	for _, value := range required {
		if _, ok := seen[value]; !ok {
			missing = append(missing, value)
		}
	}
	return missing
}
