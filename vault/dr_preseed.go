// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-uuid"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drPreSeedManifestVersion               = 1
	drPreSeedBundleVersion                 = 1
	drPreSeedAcceptedRecordVersion         = 1
	drPreSeedLocalOnlyScrubVersion         = 1
	drPreSeedBundleIntegrityAlgorithm      = "sha256"
	drPreSeedProvenanceAlgorithm           = "ecdsa-sha256-dr-transport-ca-v1"
	drPreSeedBundleFormatInlineJSONV1      = "inline-json-v1"
	drPreSeedBundleFormatSegmentedV1       = "segmented-json-v1"
	drPreSeedAcceptedStoragePath           = "core/cluster/local/dr/preseed/accepted"
	drPreSeedImportStageRecordVersion      = 1
	drPreSeedImportStageRecordPath         = "core/cluster/local/dr/preseed/staging/record"
	drPreSeedImportStageSegmentPrefix      = "core/cluster/local/dr/preseed/staging/segments/"
	drPreSeedImportStageSegmentVersion     = 1
	drPreSeedImportStageSegmentMetaPrefix  = drPreSeedImportStageSegmentPrefix + "meta/"
	drPreSeedImportStageSegmentChunkPrefix = drPreSeedImportStageSegmentPrefix + "chunks/"
	drPreSeedImportStageSegmentChunkBytes  = 512 << 10
	drPreSeedDefaultSegmentBytes           = 64 << 20
	drPreSeedBundleMaxBytes                = 512 << 20
	drPreSeedImportTxnMaxOps               = 1000
	drPreSeedImportTxnMaxBytes             = 4 << 20
)

const (
	drPreSeedExportPlanStateRunning  = "running"
	drPreSeedExportPlanStateComplete = "complete"
	drPreSeedExportPlanStateFailed   = "failed"
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
	ProvenanceAlgorithm      string                       `json:"provenance_algorithm"`
	ProvenanceKeyID          string                       `json:"provenance_key_id"`
	ProvenanceSignature      []byte                       `json:"provenance_signature"`
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

type DRPreSeedSegment struct {
	Version      int                    `json:"version"`
	Manifest     DRPreSeedManifest      `json:"manifest"`
	SegmentIndex int                    `json:"segment_index"`
	EntryCount   int                    `json:"entry_count"`
	Entries      []DRPreSeedBundleEntry `json:"entries"`
}

type DRPreSeedExportPlanStatus struct {
	PlanID          string             `json:"plan_id"`
	State           string             `json:"state"`
	Manifest        *DRPreSeedManifest `json:"manifest,omitempty"`
	EntryCount      int                `json:"entry_count,omitempty"`
	Error           string             `json:"error,omitempty"`
	StartedAtUnix   int64              `json:"started_at_unix"`
	CompletedAtUnix int64              `json:"completed_at_unix,omitempty"`
}

type drPreSeedExportPlanJob struct {
	planID          string
	state           string
	manifest        *DRPreSeedManifest
	entryCount      int
	err             string
	startedAtUnix   int64
	completedAtUnix int64
}

type drPreSeedAcceptedRecord struct {
	Version                  int               `json:"version"`
	AcceptedAtUnix           int64             `json:"accepted_at_unix"`
	Manifest                 DRPreSeedManifest `json:"manifest"`
	ConfirmStorageRestored   bool              `json:"confirm_storage_restored"`
	ConfirmLocalOnlyScrubbed bool              `json:"confirm_local_only_scrubbed"`
}

type drPreSeedImportStageRecord struct {
	Version                         int               `json:"version"`
	StartedAtUnix                   int64             `json:"started_at_unix"`
	UpdatedAtUnix                   int64             `json:"updated_at_unix"`
	Manifest                        DRPreSeedManifest `json:"manifest"`
	ConfirmReplaceReplicatedStorage bool              `json:"confirm_replace_replicated_storage"`
}

type drPreSeedImportStageSegmentRecord struct {
	Version      int    `json:"version"`
	SegmentIndex int    `json:"segment_index"`
	ByteCount    int    `json:"byte_count"`
	ChunkCount   int    `json:"chunk_count"`
	SHA256       []byte `json:"sha256"`
}

type drPreSeedImportOp struct {
	DeleteKey string
	PutEntry  *physical.Entry
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

type drPreSeedManifestProvenancePayload struct {
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
	ProvenanceAlgorithm      string                       `json:"provenance_algorithm"`
	ProvenanceKeyID          string                       `json:"provenance_key_id"`
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
	transportCA := m.transportCA
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

	manifest := newDRPreSeedManifest(clusterID, relationshipID, replSalt, checkpoint, bundleIntegritySHA256, ttl, now)
	if err := signDRPreSeedManifestProvenance(manifest, transportCA); err != nil {
		return nil, err
	}
	return manifest, nil
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
	transportCA := m.transportCA
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
	if err := signDRPreSeedManifestProvenance(&bundle.Manifest, transportCA); err != nil {
		return nil, err
	}
	return bundle, nil
}

func (m *drRelationshipManager) GenerateSegmentedPreSeedManifest(ctx context.Context, relationshipID string, segmentMaxBytes int, ttl time.Duration) (*DRPreSeedManifest, int, error) {
	if m == nil {
		return nil, 0, fmt.Errorf("DR relationship manager is nil")
	}
	if relationshipID == "" {
		return nil, 0, fmt.Errorf("relationship_id is required")
	}
	if segmentMaxBytes <= 0 {
		return nil, 0, fmt.Errorf("segment_max_bytes must be > 0")
	}

	m.mu.Lock()
	if m.config.Mode != DRModePrimary {
		m.mu.Unlock()
		return nil, 0, fmt.Errorf("not in DR primary mode")
	}
	clusterID := m.config.ClusterID
	replSalt := append([]byte(nil), m.config.ReplSalt...)
	promotion := m.config.Promotion
	primary := m.primary
	transportCA := m.transportCA
	m.mu.Unlock()

	if primary == nil {
		return nil, 0, fmt.Errorf("DR primary runtime not initialized")
	}
	if isStalePostPromotionLineage(promotion, clusterID, relationshipID) {
		return nil, 0, fmt.Errorf("relationship references stale pre-promotion DR lineage")
	}
	if len(replSalt) != drReplSaltLen {
		return nil, 0, fmt.Errorf("DR primary has invalid replication salt")
	}

	now := time.Now().UTC()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, 0, err
	}
	if err := validatePreSeedRelationshipState(rel, now); err != nil {
		return nil, 0, err
	}

	checkpoint, err := primary.BuildPreSeedCheckpoint(ctx, relationshipID)
	if err != nil {
		return nil, 0, err
	}

	rel, err = m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, 0, err
	}
	if err := validatePreSeedRelationshipState(rel, time.Now().UTC()); err != nil {
		return nil, 0, err
	}

	metadata, err := primary.BuildPreSeedBundleEntryMetadata(ctx, relationshipID, checkpoint.GetCheckpointId(), checkpoint.GetCommitIndex())
	if err != nil {
		return nil, 0, err
	}
	segments, err := buildDRPreSeedBundleSegmentPlanFromPayloadEntries(metadata, segmentMaxBytes)
	if err != nil {
		return nil, 0, err
	}
	if len(segments) == 0 {
		return nil, 0, fmt.Errorf("pre-seed segmented export has no replicated entries")
	}

	manifest := newDRPreSeedManifest(clusterID, relationshipID, replSalt, checkpoint, make([]byte, sha256.Size), ttl, now)
	manifest.BundleFormat = drPreSeedBundleFormatSegmentedV1
	manifest.BundleSegments = segments
	sum, err := computeDRPreSeedBundleIntegrityFromPayloadEntries(drPreSeedBundleVersion, *manifest, len(metadata), metadata)
	if err != nil {
		return nil, 0, err
	}
	manifest.BundleIntegritySHA256 = sum
	if err := signDRPreSeedManifestProvenance(manifest, transportCA); err != nil {
		return nil, 0, err
	}
	return manifest, len(metadata), nil
}

func (m *drRelationshipManager) StartSegmentedPreSeedManifestPlan(ctx context.Context, relationshipID string, segmentMaxBytes int, ttl time.Duration) (*DRPreSeedExportPlanStatus, error) {
	if m == nil {
		return nil, fmt.Errorf("DR relationship manager is nil")
	}
	if relationshipID == "" {
		return nil, fmt.Errorf("relationship_id is required")
	}
	if segmentMaxBytes <= 0 {
		return nil, fmt.Errorf("segment_max_bytes must be > 0")
	}
	planID, err := uuid.GenerateUUID()
	if err != nil {
		return nil, fmt.Errorf("generate pre-seed export plan id: %w", err)
	}
	startedAt := time.Now().UTC().Unix()
	job := &drPreSeedExportPlanJob{
		planID:        planID,
		state:         drPreSeedExportPlanStateRunning,
		startedAtUnix: startedAt,
	}

	m.preSeedPlanMu.Lock()
	if m.preSeedPlanJobs == nil {
		m.preSeedPlanJobs = make(map[string]*drPreSeedExportPlanJob)
	}
	m.preSeedPlanJobs[planID] = job
	m.preSeedPlanMu.Unlock()

	go func() {
		manifest, entryCount, err := m.GenerateSegmentedPreSeedManifest(context.Background(), relationshipID, segmentMaxBytes, ttl)
		m.preSeedPlanMu.Lock()
		defer m.preSeedPlanMu.Unlock()
		if err != nil {
			job.state = drPreSeedExportPlanStateFailed
			job.err = err.Error()
			job.completedAtUnix = time.Now().UTC().Unix()
			return
		}
		job.state = drPreSeedExportPlanStateComplete
		job.manifest = manifest
		job.entryCount = entryCount
		job.completedAtUnix = time.Now().UTC().Unix()
	}()

	return job.status(), nil
}

func (m *drRelationshipManager) GetSegmentedPreSeedManifestPlan(planID string) (*DRPreSeedExportPlanStatus, error) {
	if m == nil {
		return nil, fmt.Errorf("DR relationship manager is nil")
	}
	if planID == "" {
		return nil, fmt.Errorf("plan_id is required")
	}
	m.preSeedPlanMu.Lock()
	defer m.preSeedPlanMu.Unlock()
	job, ok := m.preSeedPlanJobs[planID]
	if !ok {
		return nil, fmt.Errorf("pre-seed export plan not found")
	}
	return job.status(), nil
}

func (j *drPreSeedExportPlanJob) status() *DRPreSeedExportPlanStatus {
	if j == nil {
		return nil
	}
	var manifest *DRPreSeedManifest
	if j.manifest != nil {
		copyManifest := *j.manifest
		copyManifest.ReplSaltSHA256 = append([]byte(nil), j.manifest.ReplSaltSHA256...)
		copyManifest.LocalOnlyExactPaths = append([]string(nil), j.manifest.LocalOnlyExactPaths...)
		copyManifest.LocalOnlyPrefixes = append([]string(nil), j.manifest.LocalOnlyPrefixes...)
		copyManifest.BundleSegments = append([]DRPreSeedSegmentDescriptor(nil), j.manifest.BundleSegments...)
		copyManifest.BundleIntegritySHA256 = append([]byte(nil), j.manifest.BundleIntegritySHA256...)
		copyManifest.ProvenanceSignature = append([]byte(nil), j.manifest.ProvenanceSignature...)
		manifest = &copyManifest
	}
	return &DRPreSeedExportPlanStatus{
		PlanID:          j.planID,
		State:           j.state,
		Manifest:        manifest,
		EntryCount:      j.entryCount,
		Error:           j.err,
		StartedAtUnix:   j.startedAtUnix,
		CompletedAtUnix: j.completedAtUnix,
	}
}

func (m *drRelationshipManager) GeneratePreSeedSegment(ctx context.Context, manifest *DRPreSeedManifest, segmentIndex int) (*DRPreSeedSegment, error) {
	if m == nil {
		return nil, fmt.Errorf("DR relationship manager is nil")
	}
	if manifest == nil {
		return nil, fmt.Errorf("pre-seed manifest is nil")
	}
	if segmentIndex < 0 {
		return nil, fmt.Errorf("segment_index must be >= 0")
	}

	primary, err := m.validatePrimaryPreSeedManifest(ctx, manifest, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if normalizedDRPreSeedBundleFormat(manifest.BundleFormat) != drPreSeedBundleFormatSegmentedV1 {
		return nil, fmt.Errorf("pre-seed manifest is not segmented")
	}
	if segmentIndex >= len(manifest.BundleSegments) {
		return nil, fmt.Errorf("segment_index %d out of range", segmentIndex)
	}
	segmentEntries, err := primary.BuildPreSeedSegmentEntries(ctx, manifest.RelationshipID, manifest.CheckpointID, manifest.CheckpointIndex, manifest.BundleSegments, segmentIndex)
	if err != nil {
		return nil, err
	}
	segment := &DRPreSeedSegment{
		Version:      drPreSeedBundleVersion,
		Manifest:     *manifest,
		SegmentIndex: segmentIndex,
		EntryCount:   len(segmentEntries),
		Entries:      segmentEntries,
	}
	if err := validateDRPreSeedSegment(segment, manifest, nil, nil, time.Now().UTC()); err != nil {
		return nil, err
	}
	return segment, nil
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
	return m.importPreSeedBundleLocked(ctx, bundle, token, now)
}

func (m *drRelationshipManager) BeginPreSeedSegmentImport(ctx context.Context, manifest *DRPreSeedManifest, token *DRActivationToken, now time.Time, confirmReplaceReplicatedStorage bool) error {
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
		return fmt.Errorf("pre-seed segment import can only be started before DR secondary mode is enabled")
	}
	if err := validateDRPreSeedManifest(manifest, token, m.config.Promotion, now); err != nil {
		return err
	}
	if normalizedDRPreSeedBundleFormat(manifest.BundleFormat) != drPreSeedBundleFormatSegmentedV1 {
		return fmt.Errorf("pre-seed manifest is not segmented")
	}
	if err := deletePreSeedImportStage(ctx, m.core.physical); err != nil {
		return err
	}
	record := &drPreSeedImportStageRecord{
		Version:                         drPreSeedImportStageRecordVersion,
		StartedAtUnix:                   now.Unix(),
		UpdatedAtUnix:                   now.Unix(),
		Manifest:                        *manifest,
		ConfirmReplaceReplicatedStorage: confirmReplaceReplicatedStorage,
	}
	return savePreSeedImportStageRecord(ctx, m.core.physical, record)
}

func (m *drRelationshipManager) ImportPreSeedSegment(ctx context.Context, segment *DRPreSeedSegment, token *DRActivationToken, now time.Time) (int, int, error) {
	if m == nil {
		return 0, 0, fmt.Errorf("DR relationship manager is nil")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.Mode != DRModeDisabled {
		return 0, 0, fmt.Errorf("pre-seed segment can only be imported before DR secondary mode is enabled")
	}
	record, ok, err := loadPreSeedImportStageRecord(ctx, m.core.physical)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, fmt.Errorf("pre-seed segment import has not been started")
	}
	if err := validateDRPreSeedSegment(segment, &record.Manifest, token, m.config.Promotion, now); err != nil {
		return 0, 0, err
	}
	existing, ok, err := loadPreSeedImportStageSegment(ctx, m.core.physical, segment.SegmentIndex)
	if err != nil {
		return 0, 0, err
	}
	if ok {
		same, err := samePreSeedSegment(existing, segment)
		if err != nil {
			return 0, 0, err
		}
		if !same {
			return 0, 0, fmt.Errorf("pre-seed segment %d already staged with different content", segment.SegmentIndex)
		}
		received, err := countPreSeedImportStageSegments(ctx, m.core.physical)
		return received, len(record.Manifest.BundleSegments), err
	}
	if err := savePreSeedImportStageSegment(ctx, m.core.physical, segment); err != nil {
		return 0, 0, err
	}
	record.UpdatedAtUnix = now.Unix()
	if err := savePreSeedImportStageRecord(ctx, m.core.physical, record); err != nil {
		return 0, 0, err
	}
	received, err := countPreSeedImportStageSegments(ctx, m.core.physical)
	return received, len(record.Manifest.BundleSegments), err
}

func (m *drRelationshipManager) CompletePreSeedSegmentImport(ctx context.Context, token *DRActivationToken, now time.Time, confirmReplaceReplicatedStorage bool) error {
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
		return fmt.Errorf("pre-seed segment import can only be completed before DR secondary mode is enabled")
	}
	record, ok, err := loadPreSeedImportStageRecord(ctx, m.core.physical)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pre-seed segment import has not been started")
	}
	if !record.ConfirmReplaceReplicatedStorage {
		return fmt.Errorf("staged pre-seed import is missing replacement confirmation")
	}
	if err := validateDRPreSeedManifest(&record.Manifest, token, m.config.Promotion, now); err != nil {
		return err
	}
	entries := make([]DRPreSeedBundleEntry, 0)
	for i := range record.Manifest.BundleSegments {
		segment, ok, err := loadPreSeedImportStageSegment(ctx, m.core.physical, i)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("pre-seed segment import incomplete: missing segment %d", i)
		}
		if err := validateDRPreSeedSegment(segment, &record.Manifest, token, m.config.Promotion, now); err != nil {
			return err
		}
		entries = append(entries, segment.Entries...)
	}
	bundle := &DRPreSeedBundle{
		Version:    drPreSeedBundleVersion,
		Manifest:   record.Manifest,
		EntryCount: len(entries),
		Entries:    entries,
	}
	if err := validateDRPreSeedBundle(bundle, token, m.config.Promotion, now); err != nil {
		return err
	}
	if err := m.importPreSeedBundleLocked(ctx, bundle, token, now); err != nil {
		return err
	}
	return deletePreSeedImportStage(ctx, m.core.physical)
}

func (m *drRelationshipManager) importPreSeedBundleLocked(ctx context.Context, bundle *DRPreSeedBundle, token *DRActivationToken, now time.Time) error {
	existing, err := scanDRPreSeedReplicatedKeys(ctx, m.core.physical, token.ReplSalt, m.logger)
	if err != nil {
		return err
	}

	batch := newDRPreSeedImportBatch(m.core.physical)
	existingKeys := make([]string, 0, len(existing.KIDToKey))
	for _, key := range existing.KIDToKey {
		existingKeys = append(existingKeys, key)
	}
	sort.Strings(existingKeys)
	for _, key := range existingKeys {
		if err := batch.addDelete(ctx, key); err != nil {
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
		if err := batch.addPut(ctx, &physical.Entry{
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
	accepted, err := acceptedPreSeedEntry(&record)
	if err != nil {
		return err
	}
	if err := batch.addPut(ctx, accepted); err != nil {
		return err
	}
	if err := batch.flush(ctx); err != nil {
		return err
	}
	return m.persistPreSeedOptimizerBaselineLocked(ctx, bundle, token)
}

func (m *drRelationshipManager) persistPreSeedOptimizerBaselineLocked(ctx context.Context, bundle *DRPreSeedBundle, token *DRActivationToken) error {
	if m == nil || m.core == nil || m.core.physical == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed optimizer baseline")
	}
	if bundle == nil || token == nil {
		return fmt.Errorf("pre-seed optimizer baseline requires bundle and activation token")
	}
	rs, err := preSeedReconciliationSetFromBundle(bundle)
	if err != nil {
		return err
	}
	secondary := m.secondary
	if secondary == nil {
		secondary = newDRReplicationSecondary(m.core, token.ReplSalt, token.RelationshipID, m.logger)
		secondary.clusterID = token.ClusterID
	}
	if err := secondary.deletePersistedFlatAccumulatorState(ctx, m.core.physical); err != nil {
		return fmt.Errorf("failed to reset DR pre-seed optimizer baseline: %w", err)
	}
	if err := secondary.resetAndPersistFlatAccumulatorFromSet(ctx, rs, bundle.Manifest.CheckpointIndex); err != nil {
		return fmt.Errorf("failed to persist DR pre-seed optimizer baseline: %w", err)
	}
	return nil
}

func preSeedReconciliationSetFromBundle(bundle *DRPreSeedBundle) (*reconciler.ReconciliationSet, error) {
	if bundle == nil {
		return nil, fmt.Errorf("pre-seed bundle is nil")
	}
	if bundle.Manifest.CheckpointID == "" || bundle.Manifest.CheckpointIndex == 0 {
		return nil, fmt.Errorf("pre-seed bundle manifest checkpoint is incomplete")
	}
	rs := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{
			ID:          bundle.Manifest.CheckpointID,
			CommitIndex: bundle.Manifest.CheckpointIndex,
		},
		KIDToKey: make(map[[32]byte]string, len(bundle.Entries)),
		KIDToVID: make(map[[32]byte][32]byte, len(bundle.Entries)),
	}
	for _, entry := range bundle.Entries {
		if entry.Key == "" {
			return nil, fmt.Errorf("pre-seed bundle entry key is required")
		}
		if isDRPreSeedBulkExcludedPath(entry.Key) {
			return nil, fmt.Errorf("pre-seed bundle contains local-only or excluded path %q", entry.Key)
		}
		if len(entry.KID) != sha256.Size {
			return nil, fmt.Errorf("pre-seed bundle entry %q has invalid kid", entry.Key)
		}
		if len(entry.VID) != sha256.Size {
			return nil, fmt.Errorf("pre-seed bundle entry %q has invalid vid", entry.Key)
		}
		var kid [32]byte
		var vid [32]byte
		copy(kid[:], entry.KID)
		copy(vid[:], entry.VID)
		if existing, ok := rs.KIDToKey[kid]; ok {
			return nil, fmt.Errorf("pre-seed bundle contains duplicate kid for keys %q and %q", existing, entry.Key)
		}
		rs.KIDToKey[kid] = entry.Key
		rs.KIDToVID[kid] = vid
	}
	rs.KeyCount = len(rs.KIDToVID)
	return rs, nil
}

type drPreSeedImportBatch struct {
	backend physical.Backend
	ops     []drPreSeedImportOp
	bytes   int
}

func newDRPreSeedImportBatch(backend physical.Backend) *drPreSeedImportBatch {
	return &drPreSeedImportBatch{backend: backend}
}

func (b *drPreSeedImportBatch) addDelete(ctx context.Context, key string) error {
	op := drPreSeedImportOp{DeleteKey: key}
	return b.add(ctx, op, len(key))
}

func (b *drPreSeedImportBatch) addPut(ctx context.Context, entry *physical.Entry) error {
	if entry == nil {
		return nil
	}
	op := drPreSeedImportOp{PutEntry: entry}
	return b.add(ctx, op, len(entry.Key)+len(entry.Value))
}

func (b *drPreSeedImportBatch) add(ctx context.Context, op drPreSeedImportOp, opBytes int) error {
	if b == nil || b.backend == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed import")
	}
	if len(b.ops) > 0 && (len(b.ops)+1 > drPreSeedImportTxnMaxOps || b.bytes+opBytes > drPreSeedImportTxnMaxBytes) {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	b.ops = append(b.ops, op)
	b.bytes += opBytes
	return nil
}

func (b *drPreSeedImportBatch) flush(ctx context.Context) error {
	if b == nil || len(b.ops) == 0 {
		return nil
	}

	writer := b.backend
	var tx physical.Transaction
	if txBackend, ok := b.backend.(physical.Transactional); ok {
		var err error
		tx, err = txBackend.BeginTx(ctx)
		if err != nil {
			return fmt.Errorf("begin DR pre-seed import transaction: %w", err)
		}
		writer = tx
		defer tx.Rollback(ctx)
	}

	for _, op := range b.ops {
		switch {
		case op.DeleteKey != "":
			if err := writer.Delete(ctx, op.DeleteKey); err != nil {
				return err
			}
		case op.PutEntry != nil:
			if err := writer.Put(ctx, op.PutEntry); err != nil {
				return err
			}
		}
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit DR pre-seed import transaction: %w", err)
		}
	}
	b.ops = nil
	b.bytes = 0
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
	if err := validateDRPreSeedManifestProvenance(manifest, token); err != nil {
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

func validateDRPreSeedSegment(segment *DRPreSeedSegment, manifest *DRPreSeedManifest, token *DRActivationToken, promotion *DRPromotionRecord, now time.Time) error {
	if segment == nil {
		return fmt.Errorf("pre-seed segment is nil")
	}
	if manifest == nil {
		return fmt.Errorf("pre-seed manifest is nil")
	}
	if token != nil {
		if err := validateDRPreSeedManifest(manifest, token, promotion, now); err != nil {
			return err
		}
	}
	if normalizedDRPreSeedBundleFormat(manifest.BundleFormat) != drPreSeedBundleFormatSegmentedV1 {
		return fmt.Errorf("pre-seed manifest is not segmented")
	}
	if segment.Version != drPreSeedBundleVersion {
		return fmt.Errorf("unsupported pre-seed segment version %d", segment.Version)
	}
	if segment.SegmentIndex < 0 || segment.SegmentIndex >= len(manifest.BundleSegments) {
		return fmt.Errorf("pre-seed segment index %d out of range", segment.SegmentIndex)
	}
	if segment.EntryCount != len(segment.Entries) {
		return fmt.Errorf("pre-seed segment entry_count mismatch")
	}
	if !preSeedSegmentManifestMatches(manifest, &segment.Manifest) {
		return fmt.Errorf("pre-seed segment manifest mismatch")
	}
	actual, err := buildDRPreSeedSegmentDescriptor(segment.SegmentIndex, canonicalDRPreSeedBundleEntries(segment.Entries))
	if err != nil {
		return err
	}
	expected := manifest.BundleSegments[segment.SegmentIndex]
	if expected.EntryCount != actual.EntryCount ||
		expected.ByteCount != actual.ByteCount ||
		expected.FirstKey != actual.FirstKey ||
		expected.LastKey != actual.LastKey ||
		!bytes.Equal(expected.SHA256, actual.SHA256) {
		return fmt.Errorf("pre-seed segment %d metadata mismatch", segment.SegmentIndex)
	}
	if token != nil {
		scanner := drPreSeedScanner(token.ReplSalt, nil)
		seenKeys := make(map[string]struct{}, len(segment.Entries))
		seenKIDs := make(map[[32]byte]struct{}, len(segment.Entries))
		for _, entry := range segment.Entries {
			if entry.Key == "" {
				return fmt.Errorf("pre-seed segment entry key is required")
			}
			if isDRPreSeedBulkExcludedPath(entry.Key) {
				return fmt.Errorf("pre-seed segment contains local-only or excluded path %q", entry.Key)
			}
			if len(entry.KID) != sha256.Size {
				return fmt.Errorf("pre-seed segment entry %q has invalid kid", entry.Key)
			}
			if len(entry.VID) != sha256.Size {
				return fmt.Errorf("pre-seed segment entry %q has invalid vid", entry.Key)
			}
			if _, ok := seenKeys[entry.Key]; ok {
				return fmt.Errorf("pre-seed segment contains duplicate key %q", entry.Key)
			}
			seenKeys[entry.Key] = struct{}{}
			var kid [32]byte
			copy(kid[:], entry.KID)
			if _, ok := seenKIDs[kid]; ok {
				return fmt.Errorf("pre-seed segment contains duplicate kid for key %q", entry.Key)
			}
			seenKIDs[kid] = struct{}{}
			expectedKID := scanner.ComputeKID(entry.Key)
			if kid != expectedKID {
				return fmt.Errorf("pre-seed segment entry %q kid mismatch", entry.Key)
			}
			var vid [32]byte
			copy(vid[:], entry.VID)
			expectedVID := scanner.ComputeVIDWithSealWrap(entry.Value, entry.SealWrap)
			if vid != expectedVID {
				return fmt.Errorf("pre-seed segment entry %q vid mismatch", entry.Key)
			}
		}
	}
	return nil
}

func preSeedSegmentManifestMatches(expected *DRPreSeedManifest, got *DRPreSeedManifest) bool {
	if expected == nil || got == nil {
		return false
	}
	expectedDigest, err := drPreSeedManifestProvenanceDigest(expected)
	if err != nil {
		return false
	}
	gotDigest, err := drPreSeedManifestProvenanceDigest(got)
	if err != nil {
		return false
	}
	return bytes.Equal(expectedDigest, gotDigest) &&
		bytes.Equal(expected.ProvenanceSignature, got.ProvenanceSignature)
}

func (m *drRelationshipManager) validatePrimaryPreSeedManifest(ctx context.Context, manifest *DRPreSeedManifest, now time.Time) (*drReplicationPrimary, error) {
	if manifest == nil {
		return nil, fmt.Errorf("pre-seed manifest is nil")
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
	transportCA := m.transportCA
	m.mu.Unlock()
	if primary == nil {
		return nil, fmt.Errorf("DR primary runtime not initialized")
	}
	token := &DRActivationToken{
		ClusterID:      clusterID,
		RelationshipID: manifest.RelationshipID,
		ReplSalt:       replSalt,
	}
	if transportCA != nil {
		token.DRTransportCACert = transportCA.certDER
	}
	if err := validateDRPreSeedManifest(manifest, token, promotion, now); err != nil {
		return nil, err
	}
	rel, err := m.loadRelationship(ctx, manifest.RelationshipID)
	if err != nil {
		return nil, err
	}
	if err := validatePreSeedRelationshipState(rel, now); err != nil {
		return nil, err
	}
	return primary, nil
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
	entry, err := acceptedPreSeedEntry(record)
	if err != nil {
		return err
	}
	return writer.Put(ctx, entry)
}

func acceptedPreSeedEntry(record *drPreSeedAcceptedRecord) (*physical.Entry, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal accepted DR pre-seed record: %w", err)
	}
	return &physical.Entry{
		Key:   drPreSeedAcceptedStoragePath,
		Value: data,
	}, nil
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

func loadPreSeedImportStageRecord(ctx context.Context, backend physical.Backend) (*drPreSeedImportStageRecord, bool, error) {
	if backend == nil {
		return nil, false, fmt.Errorf("physical storage is not available for DR pre-seed staging")
	}
	entry, err := backend.Get(ctx, drPreSeedImportStageRecordPath)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read DR pre-seed import stage: %w", err)
	}
	if entry == nil || len(entry.Value) == 0 {
		return nil, false, nil
	}
	var record drPreSeedImportStageRecord
	if err := json.Unmarshal(entry.Value, &record); err != nil {
		return nil, false, fmt.Errorf("failed to decode DR pre-seed import stage: %w", err)
	}
	if record.Version != drPreSeedImportStageRecordVersion {
		return nil, false, fmt.Errorf("unsupported DR pre-seed import stage version %d", record.Version)
	}
	return &record, true, nil
}

func savePreSeedImportStageRecord(ctx context.Context, backend physical.Backend, record *drPreSeedImportStageRecord) error {
	if backend == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed staging")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal DR pre-seed import stage: %w", err)
	}
	return backend.Put(ctx, &physical.Entry{
		Key:   drPreSeedImportStageRecordPath,
		Value: data,
	})
}

func preSeedImportStageSegmentPath(index int) string {
	return drPreSeedImportStageSegmentMetaPrefix + strconv.FormatInt(int64(index), 10)
}

func preSeedImportStageSegmentChunkPrefixForIndex(index int) string {
	return drPreSeedImportStageSegmentChunkPrefix + strconv.FormatInt(int64(index), 10) + "/"
}

func preSeedImportStageSegmentChunkPath(index int, chunk int) string {
	return preSeedImportStageSegmentChunkPrefixForIndex(index) + strconv.FormatInt(int64(chunk), 10)
}

func loadPreSeedImportStageSegment(ctx context.Context, backend physical.Backend, index int) (*DRPreSeedSegment, bool, error) {
	if backend == nil {
		return nil, false, fmt.Errorf("physical storage is not available for DR pre-seed staging")
	}
	entry, err := backend.Get(ctx, preSeedImportStageSegmentPath(index))
	if err != nil {
		return nil, false, fmt.Errorf("failed to read DR pre-seed import segment %d: %w", index, err)
	}
	if entry == nil || len(entry.Value) == 0 {
		return nil, false, nil
	}
	var record drPreSeedImportStageSegmentRecord
	if err := json.Unmarshal(entry.Value, &record); err != nil {
		return nil, false, fmt.Errorf("failed to decode DR pre-seed import segment record %d: %w", index, err)
	}
	if record.Version != drPreSeedImportStageSegmentVersion {
		return nil, false, fmt.Errorf("unsupported DR pre-seed import segment record version %d", record.Version)
	}
	if record.SegmentIndex != index {
		return nil, false, fmt.Errorf("DR pre-seed import segment record index mismatch: got %d want %d", record.SegmentIndex, index)
	}
	if record.ByteCount <= 0 || record.ChunkCount <= 0 {
		return nil, false, fmt.Errorf("DR pre-seed import segment %d has invalid chunk metadata", index)
	}
	if len(record.SHA256) != sha256.Size {
		return nil, false, fmt.Errorf("DR pre-seed import segment %d has invalid checksum", index)
	}

	data := bytes.NewBuffer(make([]byte, 0, record.ByteCount))
	for i := 0; i < record.ChunkCount; i++ {
		chunk, err := backend.Get(ctx, preSeedImportStageSegmentChunkPath(index, i))
		if err != nil {
			return nil, false, fmt.Errorf("failed to read DR pre-seed import segment %d chunk %d: %w", index, i, err)
		}
		if chunk == nil {
			return nil, false, fmt.Errorf("DR pre-seed import segment %d chunk %d is missing", index, i)
		}
		data.Write(chunk.Value)
	}
	raw := data.Bytes()
	if len(raw) != record.ByteCount {
		return nil, false, fmt.Errorf("DR pre-seed import segment %d byte count mismatch: got %d want %d", index, len(raw), record.ByteCount)
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], record.SHA256) {
		return nil, false, fmt.Errorf("DR pre-seed import segment %d checksum mismatch", index)
	}
	var segment DRPreSeedSegment
	if err := json.Unmarshal(raw, &segment); err != nil {
		return nil, false, fmt.Errorf("failed to decode DR pre-seed import segment %d: %w", index, err)
	}
	return &segment, true, nil
}

func savePreSeedImportStageSegment(ctx context.Context, backend physical.Backend, segment *DRPreSeedSegment) error {
	if backend == nil {
		return fmt.Errorf("physical storage is not available for DR pre-seed staging")
	}
	if segment == nil {
		return fmt.Errorf("DR pre-seed import segment is nil")
	}
	data, err := json.Marshal(segment)
	if err != nil {
		return fmt.Errorf("failed to marshal DR pre-seed import segment %d: %w", segment.SegmentIndex, err)
	}
	if err := deletePhysicalPrefix(ctx, backend, preSeedImportStageSegmentChunkPrefixForIndex(segment.SegmentIndex)); err != nil {
		return err
	}
	if err := backend.Delete(ctx, preSeedImportStageSegmentPath(segment.SegmentIndex)); err != nil {
		return fmt.Errorf("failed to delete stale DR pre-seed import segment record %d: %w", segment.SegmentIndex, err)
	}

	chunkCount := (len(data) + drPreSeedImportStageSegmentChunkBytes - 1) / drPreSeedImportStageSegmentChunkBytes
	for i := 0; i < chunkCount; i++ {
		start := i * drPreSeedImportStageSegmentChunkBytes
		end := start + drPreSeedImportStageSegmentChunkBytes
		if end > len(data) {
			end = len(data)
		}
		if err := backend.Put(ctx, &physical.Entry{
			Key:   preSeedImportStageSegmentChunkPath(segment.SegmentIndex, i),
			Value: append([]byte(nil), data[start:end]...),
		}); err != nil {
			return fmt.Errorf("failed to write DR pre-seed import segment %d chunk %d: %w", segment.SegmentIndex, i, err)
		}
	}
	sum := sha256.Sum256(data)
	record := drPreSeedImportStageSegmentRecord{
		Version:      drPreSeedImportStageSegmentVersion,
		SegmentIndex: segment.SegmentIndex,
		ByteCount:    len(data),
		ChunkCount:   chunkCount,
		SHA256:       sum[:],
	}
	recordData, err := json.Marshal(&record)
	if err != nil {
		return fmt.Errorf("failed to marshal DR pre-seed import segment record %d: %w", segment.SegmentIndex, err)
	}
	return backend.Put(ctx, &physical.Entry{
		Key:   preSeedImportStageSegmentPath(segment.SegmentIndex),
		Value: recordData,
	})
}

func samePreSeedSegment(a, b *DRPreSeedSegment) (bool, error) {
	aBytes, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aBytes, bBytes), nil
}

func countPreSeedImportStageSegments(ctx context.Context, backend physical.Backend) (int, error) {
	if backend == nil {
		return 0, fmt.Errorf("physical storage is not available for DR pre-seed staging")
	}
	keys, err := backend.List(ctx, drPreSeedImportStageSegmentMetaPrefix)
	if err != nil {
		return 0, fmt.Errorf("failed to list DR pre-seed import segments: %w", err)
	}
	return len(keys), nil
}

func deletePreSeedImportStage(ctx context.Context, backend physical.Backend) error {
	if backend == nil {
		return nil
	}
	if err := deletePhysicalPrefix(ctx, backend, drPreSeedImportStageSegmentPrefix); err != nil {
		return err
	}
	if err := backend.Delete(ctx, drPreSeedImportStageRecordPath); err != nil {
		return fmt.Errorf("failed to delete DR pre-seed import stage: %w", err)
	}
	return nil
}

func deletePhysicalPrefix(ctx context.Context, backend physical.Backend, prefix string) error {
	keys, err := backend.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("failed to list physical prefix %q: %w", prefix, err)
	}
	for _, key := range keys {
		fullKey := prefix + key
		if strings.HasSuffix(key, "/") {
			if err := deletePhysicalPrefix(ctx, backend, fullKey); err != nil {
				return err
			}
			continue
		}
		if err := backend.Delete(ctx, fullKey); err != nil {
			return fmt.Errorf("failed to delete physical key %q: %w", fullKey, err)
		}
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
	m.secondary.preSeedBaselineIndex.Store(index)
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
	return buildDRPreSeedBundleSegmentPlanFromPayloadEntries(preSeedPayloadEntriesFromBundleEntries(bundle.Entries), maxSegmentBytes)
}

func buildDRPreSeedBundleSegmentPlanFromPayloadEntries(entries []drPreSeedBundleIntegrityPayloadEntry, maxSegmentBytes int) ([]DRPreSeedSegmentDescriptor, error) {
	if maxSegmentBytes <= 0 {
		return nil, fmt.Errorf("pre-seed segment max bytes must be positive")
	}
	entries = canonicalDRPreSeedBundleIntegrityPayloadEntries(entries)
	if len(entries) == 0 {
		return nil, nil
	}

	segments := make([]DRPreSeedSegmentDescriptor, 0)
	current := make([]drPreSeedBundleIntegrityPayloadEntryJSON, 0)
	for _, entry := range entries {
		entryJSON, err := marshalDRPreSeedBundleIntegrityPayloadEntry(entry)
		if err != nil {
			return nil, err
		}
		candidateByteCount := drPreSeedSegmentPayloadJSONLen(append(current, entryJSON))
		if len(current) > 0 && candidateByteCount > maxSegmentBytes {
			descriptor, err := buildDRPreSeedSegmentDescriptorFromEntryJSON(len(segments), current)
			if err != nil {
				return nil, err
			}
			segments = append(segments, descriptor)
			current = []drPreSeedBundleIntegrityPayloadEntryJSON{entryJSON}
			candidateByteCount = drPreSeedSegmentPayloadJSONLen(current)
		} else {
			current = append(current, entryJSON)
		}
		if candidateByteCount > maxSegmentBytes {
			return nil, fmt.Errorf("pre-seed bundle entry %q exceeds segment max bytes", entry.Key)
		}
	}
	if len(current) > 0 {
		descriptor, err := buildDRPreSeedSegmentDescriptorFromEntryJSON(len(segments), current)
		if err != nil {
			return nil, err
		}
		segments = append(segments, descriptor)
	}
	return segments, nil
}

func preSeedSegmentEntries(entries []DRPreSeedBundleEntry, segments []DRPreSeedSegmentDescriptor, segmentIndex int) ([]DRPreSeedBundleEntry, error) {
	if segmentIndex < 0 || segmentIndex >= len(segments) {
		return nil, fmt.Errorf("pre-seed segment index %d out of range", segmentIndex)
	}
	sortedEntries := canonicalDRPreSeedBundleEntries(entries)
	offset := 0
	for i := 0; i < segmentIndex; i++ {
		offset += segments[i].EntryCount
	}
	count := segments[segmentIndex].EntryCount
	if count < 0 || offset+count > len(sortedEntries) {
		return nil, fmt.Errorf("pre-seed segment %d entry_count exceeds bundle entries", segmentIndex)
	}
	return append([]DRPreSeedBundleEntry(nil), sortedEntries[offset:offset+count]...), nil
}

func buildDRPreSeedSegmentDescriptor(index int, entries []DRPreSeedBundleEntry) (DRPreSeedSegmentDescriptor, error) {
	return buildDRPreSeedSegmentDescriptorFromPayloadEntries(index, preSeedPayloadEntriesFromBundleEntries(entries))
}

func buildDRPreSeedSegmentDescriptorFromPayloadEntries(index int, entries []drPreSeedBundleIntegrityPayloadEntry) (DRPreSeedSegmentDescriptor, error) {
	if len(entries) == 0 {
		return DRPreSeedSegmentDescriptor{}, fmt.Errorf("pre-seed segment %d has no entries", index)
	}
	entries = canonicalDRPreSeedBundleIntegrityPayloadEntries(entries)
	entryJSONs, err := marshalDRPreSeedBundleIntegrityPayloadEntryJSONs(entries)
	if err != nil {
		return DRPreSeedSegmentDescriptor{}, err
	}
	return buildDRPreSeedSegmentDescriptorFromEntryJSON(index, entryJSONs)
}

func buildDRPreSeedSegmentDescriptorFromEntryJSON(index int, entryJSONs []drPreSeedBundleIntegrityPayloadEntryJSON) (DRPreSeedSegmentDescriptor, error) {
	if len(entryJSONs) == 0 {
		return DRPreSeedSegmentDescriptor{}, fmt.Errorf("pre-seed segment %d has no entries", index)
	}
	data := marshalDRPreSeedSegmentPayloadEntryJSONs(entryJSONs)
	sum := sha256.Sum256(data)
	return DRPreSeedSegmentDescriptor{
		Index:      index,
		EntryCount: len(entryJSONs),
		ByteCount:  uint64(len(data)),
		SHA256:     sum[:],
		FirstKey:   entryJSONs[0].Entry.Key,
		LastKey:    entryJSONs[len(entryJSONs)-1].Entry.Key,
	}, nil
}

func marshalDRPreSeedSegmentPayload(entries []DRPreSeedBundleEntry) ([]byte, error) {
	return marshalDRPreSeedSegmentPayloadEntries(preSeedPayloadEntriesFromBundleEntries(entries))
}

func marshalDRPreSeedSegmentPayloadEntries(entries []drPreSeedBundleIntegrityPayloadEntry) ([]byte, error) {
	entries = canonicalDRPreSeedBundleIntegrityPayloadEntries(entries)
	entryJSONs, err := marshalDRPreSeedBundleIntegrityPayloadEntryJSONs(entries)
	if err != nil {
		return nil, err
	}
	return marshalDRPreSeedSegmentPayloadEntryJSONs(entryJSONs), nil
}

type drPreSeedBundleIntegrityPayloadEntryJSON struct {
	Entry drPreSeedBundleIntegrityPayloadEntry
	JSON  []byte
}

func marshalDRPreSeedBundleIntegrityPayloadEntryJSONs(entries []drPreSeedBundleIntegrityPayloadEntry) ([]drPreSeedBundleIntegrityPayloadEntryJSON, error) {
	entryJSONs := make([]drPreSeedBundleIntegrityPayloadEntryJSON, 0, len(entries))
	for _, entry := range entries {
		entryJSON, err := marshalDRPreSeedBundleIntegrityPayloadEntry(entry)
		if err != nil {
			return nil, err
		}
		entryJSONs = append(entryJSONs, entryJSON)
	}
	return entryJSONs, nil
}

func marshalDRPreSeedBundleIntegrityPayloadEntry(entry drPreSeedBundleIntegrityPayloadEntry) (drPreSeedBundleIntegrityPayloadEntryJSON, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return drPreSeedBundleIntegrityPayloadEntryJSON{}, fmt.Errorf("marshal pre-seed segment entry payload: %w", err)
	}
	return drPreSeedBundleIntegrityPayloadEntryJSON{
		Entry: entry,
		JSON:  data,
	}, nil
}

func drPreSeedSegmentPayloadJSONLen(entryJSONs []drPreSeedBundleIntegrityPayloadEntryJSON) int {
	if len(entryJSONs) == 0 {
		return len(`{"version":1,"entries":[]}`)
	}
	n := len(`{"version":1,"entries":[`) + len(`]}`)
	for i, entryJSON := range entryJSONs {
		if i > 0 {
			n++
		}
		n += len(entryJSON.JSON)
	}
	return n
}

func marshalDRPreSeedSegmentPayloadEntryJSONs(entryJSONs []drPreSeedBundleIntegrityPayloadEntryJSON) []byte {
	var buf bytes.Buffer
	buf.Grow(drPreSeedSegmentPayloadJSONLen(entryJSONs))
	buf.WriteString(`{"version":1,"entries":[`)
	for i, entryJSON := range entryJSONs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(entryJSON.JSON)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
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

func preSeedPayloadEntriesFromBundleEntries(raw []DRPreSeedBundleEntry) []drPreSeedBundleIntegrityPayloadEntry {
	entries := canonicalDRPreSeedBundleEntries(raw)
	payload := make([]drPreSeedBundleIntegrityPayloadEntry, 0, len(entries))
	for _, entry := range entries {
		valueHash := sha256.Sum256(entry.Value)
		payload = append(payload, drPreSeedBundleIntegrityPayloadEntry{
			Key:         entry.Key,
			SealWrap:    entry.SealWrap,
			KID:         append([]byte(nil), entry.KID...),
			VID:         append([]byte(nil), entry.VID...),
			ValueSHA256: valueHash[:],
		})
	}
	return payload
}

func canonicalDRPreSeedBundleIntegrityPayloadEntries(raw []drPreSeedBundleIntegrityPayloadEntry) []drPreSeedBundleIntegrityPayloadEntry {
	entries := make([]drPreSeedBundleIntegrityPayloadEntry, 0, len(raw))
	for _, entry := range raw {
		entries = append(entries, drPreSeedBundleIntegrityPayloadEntry{
			Key:         entry.Key,
			SealWrap:    entry.SealWrap,
			KID:         append([]byte(nil), entry.KID...),
			VID:         append([]byte(nil), entry.VID...),
			ValueSHA256: append([]byte(nil), entry.ValueSHA256...),
		})
	}
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
	return computeDRPreSeedBundleIntegrityFromPayloadEntries(bundle.Version, bundle.Manifest, bundle.EntryCount, preSeedPayloadEntriesFromBundleEntries(bundle.Entries))
}

func computeDRPreSeedBundleIntegrityFromPayloadEntries(version int, manifest DRPreSeedManifest, entryCount int, entries []drPreSeedBundleIntegrityPayloadEntry) ([]byte, error) {
	entries = canonicalDRPreSeedBundleIntegrityPayloadEntries(entries)
	payload := drPreSeedBundleIntegrityPayload{
		Version:          version,
		PrimaryClusterID: manifest.PrimaryClusterID,
		RelationshipID:   manifest.RelationshipID,
		CheckpointID:     manifest.CheckpointID,
		CheckpointIndex:  manifest.CheckpointIndex,
		BundleFormat:     normalizedDRPreSeedBundleFormat(manifest.BundleFormat),
		BundleSegments:   append([]DRPreSeedSegmentDescriptor(nil), manifest.BundleSegments...),
		EntryCount:       entryCount,
		Entries:          entries,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal pre-seed bundle integrity payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

func signDRPreSeedManifestProvenance(manifest *DRPreSeedManifest, ca *drTransportCA) error {
	if manifest == nil {
		return fmt.Errorf("pre-seed manifest is nil")
	}
	if ca == nil || ca.cert == nil || ca.key == nil {
		return fmt.Errorf("DR transport CA is required to sign pre-seed manifest provenance")
	}
	keyID, err := drPreSeedProvenanceKeyID(ca.cert)
	if err != nil {
		return err
	}
	manifest.ProvenanceAlgorithm = drPreSeedProvenanceAlgorithm
	manifest.ProvenanceKeyID = keyID
	manifest.ProvenanceSignature = nil

	digest, err := drPreSeedManifestProvenanceDigest(manifest)
	if err != nil {
		return err
	}
	signature, err := ecdsa.SignASN1(rand.Reader, ca.key, digest)
	if err != nil {
		return fmt.Errorf("sign pre-seed manifest provenance: %w", err)
	}
	manifest.ProvenanceSignature = signature
	return nil
}

func validateDRPreSeedManifestProvenance(manifest *DRPreSeedManifest, token *DRActivationToken) error {
	if manifest == nil {
		return fmt.Errorf("pre-seed manifest is nil")
	}
	if token == nil {
		return fmt.Errorf("activation token is required")
	}
	if manifest.ProvenanceAlgorithm != drPreSeedProvenanceAlgorithm {
		return fmt.Errorf("unsupported pre-seed provenance algorithm %q", manifest.ProvenanceAlgorithm)
	}
	if manifest.ProvenanceKeyID == "" {
		return fmt.Errorf("pre-seed provenance key_id is required")
	}
	if len(manifest.ProvenanceSignature) == 0 {
		return fmt.Errorf("pre-seed provenance signature is required")
	}
	if len(token.DRTransportCACert) == 0 {
		return fmt.Errorf("activation token missing DR transport CA certificate for pre-seed provenance")
	}
	cert, err := x509.ParseCertificate(token.DRTransportCACert)
	if err != nil {
		return fmt.Errorf("invalid DR transport CA certificate for pre-seed provenance: %w", err)
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return fmt.Errorf("pre-seed provenance DR transport certificate is not a CA")
	}
	keyID, err := drPreSeedProvenanceKeyID(cert)
	if err != nil {
		return err
	}
	if manifest.ProvenanceKeyID != keyID {
		return fmt.Errorf("pre-seed provenance key_id mismatch")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("pre-seed provenance DR transport CA key is not ECDSA")
	}
	digest, err := drPreSeedManifestProvenanceDigest(manifest)
	if err != nil {
		return err
	}
	if !ecdsa.VerifyASN1(pub, digest, manifest.ProvenanceSignature) {
		return fmt.Errorf("pre-seed provenance signature mismatch")
	}
	return nil
}

func drPreSeedProvenanceKeyID(cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", fmt.Errorf("DR transport CA certificate is required for pre-seed provenance")
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal pre-seed provenance public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

func drPreSeedManifestProvenanceDigest(manifest *DRPreSeedManifest) ([]byte, error) {
	payload := drPreSeedManifestProvenancePayload{
		Version:                  manifest.Version,
		PrimaryClusterID:         manifest.PrimaryClusterID,
		RelationshipID:           manifest.RelationshipID,
		CheckpointID:             manifest.CheckpointID,
		CheckpointIndex:          manifest.CheckpointIndex,
		CreatedAtUnix:            manifest.CreatedAtUnix,
		ExpiresAtUnix:            manifest.ExpiresAtUnix,
		ReplSaltSHA256:           append([]byte(nil), manifest.ReplSaltSHA256...),
		RangePlanVersion:         manifest.RangePlanVersion,
		RangeBits:                manifest.RangeBits,
		RangeCount:               manifest.RangeCount,
		ChecksumAlgorithm:        manifest.ChecksumAlgorithm,
		ValueDomain:              manifest.ValueDomain,
		LocalOnlyScrubVersion:    manifest.LocalOnlyScrubVersion,
		LocalOnlyExactPaths:      append([]string(nil), manifest.LocalOnlyExactPaths...),
		LocalOnlyPrefixes:        append([]string(nil), manifest.LocalOnlyPrefixes...),
		AccumulatorSnapshotVer:   manifest.AccumulatorSnapshotVer,
		LocalKIDIndexVersion:     manifest.LocalKIDIndexVersion,
		BundleFormat:             normalizedDRPreSeedBundleFormat(manifest.BundleFormat),
		BundleSegments:           append([]DRPreSeedSegmentDescriptor(nil), manifest.BundleSegments...),
		BundleIntegrityAlgorithm: manifest.BundleIntegrityAlgorithm,
		BundleIntegritySHA256:    append([]byte(nil), manifest.BundleIntegritySHA256...),
		ProvenanceAlgorithm:      manifest.ProvenanceAlgorithm,
		ProvenanceKeyID:          manifest.ProvenanceKeyID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal pre-seed provenance payload: %w", err)
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
