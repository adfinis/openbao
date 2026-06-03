// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

func TestDRPreSeedManifestValidationAcceptsMatchingFreshRelationship(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)

	if err := validateDRPreSeedManifest(manifest, token, nil, now); err != nil {
		t.Fatalf("expected valid pre-seed manifest, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsRelationshipMismatch(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.RelationshipID = "rel-other"

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "relationship_id mismatch") {
		t.Fatalf("expected relationship mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsAlgorithmMismatch(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.ChecksumAlgorithm = "crc64-old"

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "checksum_algorithm mismatch") {
		t.Fatalf("expected checksum algorithm mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsMissingLocalOnlyScrubMetadata(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.LocalOnlyScrubVersion = 0
	manifest.LocalOnlyExactPaths = nil
	manifest.LocalOnlyPrefixes = nil

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "local-only scrub") {
		t.Fatalf("expected local-only scrub metadata failure, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsStalePromotionLineage(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	promotion := &DRPromotionRecord{
		PromotionID:         "promotion-preseed-stale",
		OldPrimaryClusterID: token.ClusterID,
		OldRelationshipID:   token.RelationshipID,
		PromotionClass:      DRPromotionClean,
	}

	err := validateDRPreSeedManifest(manifest, token, promotion, now)
	if err == nil || !strings.Contains(err.Error(), "stale pre-promotion") {
		t.Fatalf("expected stale lineage rejection, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsExpiredManifest(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)

	err := validateDRPreSeedManifest(manifest, token, nil, now.Add(2*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired manifest rejection, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsMissingProvenance(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.ProvenanceSignature = nil

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "provenance signature") {
		t.Fatalf("expected missing provenance rejection, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsTamperedProvenance(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.BundleIntegritySHA256[0] ^= 0xff

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "provenance signature mismatch") {
		t.Fatalf("expected provenance signature mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsWrongProvenanceCA(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	otherCACert, _ := newTestDRTransportCACert(t)
	token.DRTransportCACert = otherCACert.Raw

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "provenance key_id mismatch") {
		t.Fatalf("expected provenance key_id mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsInvalidSegmentMetadata(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture(t)
	manifest.BundleFormat = drPreSeedBundleFormatSegmentedV1
	manifest.BundleSegments = []DRPreSeedSegmentDescriptor{{
		Index:      1,
		EntryCount: 1,
		ByteCount:  1,
		SHA256:     make([]byte, sha256.Size),
		FirstKey:   "secret/data/a",
		LastKey:    "secret/data/a",
	}}

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "segment index mismatch") {
		t.Fatalf("expected segment metadata rejection, got: %v", err)
	}
}

func TestDRRelationshipManagerValidatePreSeedManifestUsesPreservedPromotionLineage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	token, manifest, now := testDRPreSeedManifestFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)
	mgr.config = &DRConfig{
		Mode: DRModeDisabled,
		Promotion: &DRPromotionRecord{
			PromotionID:         "promotion-preseed-manager",
			OldPrimaryClusterID: token.ClusterID,
			OldRelationshipID:   token.RelationshipID,
			PromotionClass:      DRPromotionClean,
		},
	}

	err := mgr.ValidatePreSeedManifest(manifest, token, now)
	if err == nil || !strings.Contains(err.Error(), "stale pre-promotion") {
		t.Fatalf("expected manager to reject stale pre-seed lineage, got: %v", err)
	}
}

func TestDRPreSeedBundleValidationRejectsIntegrityMismatch(t *testing.T) {
	token, bundle, now := testDRPreSeedBundleFixture(t)
	bundle.Entries[0].Value = []byte("tampered")

	err := validateDRPreSeedBundle(bundle, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("expected integrity mismatch, got: %v", err)
	}
}

func TestDRPreSeedBundleValidationAcceptsSegmentedMetadata(t *testing.T) {
	token, bundle, now := testDRPreSeedBundleFixture(t)
	segments, err := buildDRPreSeedBundleSegmentPlan(bundle, testDRPreSeedMaxSingleSegmentBytes(t, bundle.Entries))
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleFormat = drPreSeedBundleFormatSegmentedV1
	bundle.Manifest.BundleSegments = segments
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)

	if err := validateDRPreSeedBundle(bundle, token, nil, now); err != nil {
		t.Fatalf("expected segmented pre-seed bundle to validate, got: %v", err)
	}
}

func TestDRPreSeedBundleValidationRejectsSegmentMetadataMismatch(t *testing.T) {
	token, bundle, now := testDRPreSeedBundleFixture(t)
	segments, err := buildDRPreSeedBundleSegmentPlan(bundle, testDRPreSeedMaxSingleSegmentBytes(t, bundle.Entries))
	if err != nil {
		t.Fatal(err)
	}
	segments[0].ByteCount++
	bundle.Manifest.BundleFormat = drPreSeedBundleFormatSegmentedV1
	bundle.Manifest.BundleSegments = segments
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)

	err = validateDRPreSeedBundle(bundle, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "segment 0 metadata mismatch") {
		t.Fatalf("expected segment metadata mismatch, got: %v", err)
	}
}

func TestDRRelationshipManagerSegmentedPreSeedImportPersistsAcrossRestart(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, bundle, segments, now := testDRPreSeedSegmentedBundleFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := core.physical.Put(ctx, &physical.Entry{Key: "secret/data/stale-segmented", Value: []byte("stale")}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.BeginPreSeedSegmentImport(ctx, &bundle.Manifest, token, now, true); err != nil {
		t.Fatalf("begin segmented pre-seed import failed: %v", err)
	}
	received, expected, err := mgr.ImportPreSeedSegment(ctx, segments[0], token, now)
	if err != nil {
		t.Fatalf("import first segment failed: %v", err)
	}
	if received != 1 || expected != len(segments) {
		t.Fatalf("unexpected received/expected after first segment: %d/%d", received, expected)
	}
	received, expected, err = mgr.ImportPreSeedSegment(ctx, segments[0], token, now)
	if err != nil {
		t.Fatalf("re-import first segment should be idempotent: %v", err)
	}
	if received != 1 || expected != len(segments) {
		t.Fatalf("unexpected received/expected after idempotent segment: %d/%d", received, expected)
	}

	restored := newDRRelationshipManager(core, core.logger)
	for _, segment := range segments[1:] {
		if _, _, err := restored.ImportPreSeedSegment(ctx, segment, token, now); err != nil {
			t.Fatalf("import segment %d after restart failed: %v", segment.SegmentIndex, err)
		}
	}
	if err := restored.CompletePreSeedSegmentImport(ctx, token, now, true); err != nil {
		t.Fatalf("complete segmented pre-seed import failed: %v", err)
	}
	stale, err := core.physical.Get(ctx, "secret/data/stale-segmented")
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("expected stale replicated key to be removed, got %#v", stale)
	}
	for _, entry := range bundle.Entries {
		got, err := core.physical.Get(ctx, entry.Key)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || string(got.Value) != string(entry.Value) {
			t.Fatalf("unexpected imported entry for %q: %#v", entry.Key, got)
		}
	}
	if _, ok, err := loadPreSeedImportStageRecord(ctx, core.physical); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("expected staging record to be removed after completion")
	}
	accepted, err := core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if accepted == nil {
		t.Fatal("expected accepted pre-seed record after segmented import")
	}
}

func TestDRPreSeedImportStageSegmentChunksLargeSegment(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	_, _, segments, _ := testDRPreSeedSegmentedBundleFixture(t)
	segment := *segments[0]
	segment.Entries = append([]DRPreSeedBundleEntry(nil), segments[0].Entries...)
	segment.Entries[0].Value = []byte(strings.Repeat("x", drPreSeedImportStageSegmentChunkBytes*2))

	raw, err := json.Marshal(&segment)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= drPreSeedImportStageSegmentChunkBytes {
		t.Fatalf("test segment was not large enough to require chunks: %d", len(raw))
	}

	backend := &maxValuePhysicalBackend{
		Backend:       core.physical,
		maxValueBytes: drPreSeedImportStageSegmentChunkBytes,
	}
	if err := savePreSeedImportStageSegment(ctx, backend, &segment); err != nil {
		t.Fatalf("chunked stage save failed: %v", err)
	}
	chunks, err := core.physical.List(ctx, preSeedImportStageSegmentChunkPrefixForIndex(segment.SegmentIndex))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple staged chunks, got %d", len(chunks))
	}
	count, err := countPreSeedImportStageSegments(ctx, core.physical)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one staged segment record, got %d", count)
	}

	loaded, ok, err := loadPreSeedImportStageSegment(ctx, core.physical, segment.SegmentIndex)
	if err != nil {
		t.Fatalf("chunked stage load failed: %v", err)
	}
	if !ok {
		t.Fatal("expected staged segment to load")
	}
	same, err := samePreSeedSegment(&segment, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("loaded chunked segment differs from original")
	}
}

func TestDRRelationshipManagerCompleteSegmentedPreSeedImportRejectsIncompleteStage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, bundle, segments, now := testDRPreSeedSegmentedBundleFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := mgr.BeginPreSeedSegmentImport(ctx, &bundle.Manifest, token, now, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.ImportPreSeedSegment(ctx, segments[0], token, now); err != nil {
		t.Fatal(err)
	}
	err := mgr.CompletePreSeedSegmentImport(ctx, token, now, true)
	if err == nil || !strings.Contains(err.Error(), "missing segment") {
		t.Fatalf("expected incomplete import rejection, got: %v", err)
	}
}

func TestDRRelationshipManagerImportPreSeedSegmentRejectsTampering(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, bundle, segments, now := testDRPreSeedSegmentedBundleFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := mgr.BeginPreSeedSegmentImport(ctx, &bundle.Manifest, token, now, true); err != nil {
		t.Fatal(err)
	}
	tampered := *segments[0]
	tampered.Entries = append([]DRPreSeedBundleEntry(nil), segments[0].Entries...)
	tampered.Entries[0].Value = []byte("tampered")
	_, _, err := mgr.ImportPreSeedSegment(ctx, &tampered, token, now)
	if err == nil || !strings.Contains(err.Error(), "metadata mismatch") {
		t.Fatalf("expected tampered segment rejection, got: %v", err)
	}
}

func TestDRPreSeedBundleValidationRejectsLocalOnlyPath(t *testing.T) {
	token, bundle, now := testDRPreSeedBundleFixture(t)
	scanner := drPreSeedScanner(token.ReplSalt, nil)
	localKey := "core/cluster/local/info"
	kid := scanner.ComputeKID(localKey)
	value := []byte("local-cluster")
	vid := scanner.ComputeVIDWithSealWrap(value, false)
	bundle.Entries = append(bundle.Entries, DRPreSeedBundleEntry{
		Key:   localKey,
		Value: value,
		KID:   kid[:],
		VID:   vid[:],
	})
	bundle.EntryCount = len(bundle.Entries)
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)

	err = validateDRPreSeedBundle(bundle, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "local-only") {
		t.Fatalf("expected local-only path rejection, got: %v", err)
	}
}

func TestDRPreSeedBundleValidationRejectsBootstrapOwnedRootKey(t *testing.T) {
	token, bundle, now := testDRPreSeedBundleFixture(t)
	scanner := drPreSeedScanner(token.ReplSalt, nil)
	key := "core/root-key"
	value := []byte("root-key-ciphertext")
	kid := scanner.ComputeKID(key)
	vid := scanner.ComputeVIDWithSealWrap(value, false)
	bundle.Entries = append(bundle.Entries, DRPreSeedBundleEntry{
		Key:   key,
		Value: value,
		KID:   kid[:],
		VID:   vid[:],
	})
	bundle.EntryCount = len(bundle.Entries)
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)

	err = validateDRPreSeedBundle(bundle, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "local-only or excluded") {
		t.Fatalf("expected root-key path rejection, got: %v", err)
	}
}

func TestDRPreSeedSegmentPlanIsDeterministic(t *testing.T) {
	_, bundle, _ := testDRPreSeedBundleFixture(t)
	maxSegmentBytes := testDRPreSeedMaxSingleSegmentBytes(t, bundle.Entries)

	want, err := buildDRPreSeedBundleSegmentPlan(bundle, maxSegmentBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(bundle.Entries) {
		t.Fatalf("expected one segment per fixture entry, got %d segments for %d entries", len(want), len(bundle.Entries))
	}

	shuffled := *bundle
	shuffled.Entries = append([]DRPreSeedBundleEntry(nil), bundle.Entries...)
	for i, j := 0, len(shuffled.Entries)-1; i < j; i, j = i+1, j-1 {
		shuffled.Entries[i], shuffled.Entries[j] = shuffled.Entries[j], shuffled.Entries[i]
	}
	got, err := buildDRPreSeedBundleSegmentPlan(&shuffled, maxSegmentBytes)
	if err != nil {
		t.Fatal(err)
	}

	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("segment plan changed after entry reorder\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestDRPreSeedSegmentPlanRejectsOversizedEntry(t *testing.T) {
	_, bundle, _ := testDRPreSeedBundleFixture(t)

	_, err := buildDRPreSeedBundleSegmentPlan(bundle, 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds segment max bytes") {
		t.Fatalf("expected oversized entry rejection, got: %v", err)
	}
}

func TestDRRelationshipManagerGeneratePreSeedBundleExportsCheckpointArtifact(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	core.clusterAddr.Store("https://primary.test.local:8201")

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	mgr.primary.checkpointArtifacts = newDRCheckpointArtifactStore(core.logger, t.TempDir())
	mgr.primary.checkpointArtifacts.configure(true, time.Hour, drCheckpointArtifactDefaultGlobalBudget, drCheckpointArtifactDefaultPerRelBudget, drCheckpointArtifactDefaultSegmentBytes)
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	replicatedKey := "secret/data/preseed-export"
	replicatedValue := []byte("preseed-export-ciphertext")
	if err := core.physical.Put(ctx, &physical.Entry{Key: replicatedKey, Value: replicatedValue}); err != nil {
		t.Fatal(err)
	}
	mgr.primary.OnChange([]physical.ChangeStreamEntry{{
		OpType:    physical.PutOperation,
		Key:       replicatedKey,
		Value:     replicatedValue,
		RaftIndex: 77,
	}, {
		OpType:    physical.PutOperation,
		Key:       "core/keyring",
		Value:     []byte("excluded-keyring"),
		RaftIndex: 78,
	}, {
		OpType:    physical.PutOperation,
		Key:       "core/root-key",
		Value:     []byte("bootstrap-owned-root-key"),
		RaftIndex: 79,
	}})
	if err := core.physical.Put(ctx, &physical.Entry{Key: "core/cluster/local/preseed-export", Value: []byte("local-only")}); err != nil {
		t.Fatal(err)
	}

	bundle, err := mgr.GeneratePreSeedBundle(ctx, token.RelationshipID, time.Hour)
	if err != nil {
		t.Fatalf("generate pre-seed bundle failed: %v", err)
	}
	if err := validateDRPreSeedBundle(bundle, token, nil, time.Now().UTC()); err != nil {
		t.Fatalf("exported bundle did not validate against activation token: %v", err)
	}
	if bundle.Manifest.RelationshipID != token.RelationshipID {
		t.Fatalf("expected relationship %q, got %q", token.RelationshipID, bundle.Manifest.RelationshipID)
	}
	if bundle.Manifest.CheckpointIndex != 79 {
		t.Fatalf("expected checkpoint index 79, got %d", bundle.Manifest.CheckpointIndex)
	}

	foundReplicated := false
	for _, entry := range bundle.Entries {
		if isDRPreSeedBulkExcludedPath(entry.Key) {
			t.Fatalf("exported bundle contains excluded path %q", entry.Key)
		}
		if entry.Key == replicatedKey {
			foundReplicated = true
			if string(entry.Value) != string(replicatedValue) {
				t.Fatalf("unexpected exported value for %q", replicatedKey)
			}
		}
		if entry.Key == "core/cluster/local/preseed-export" {
			t.Fatal("exported bundle contained local-only key")
		}
		if entry.Key == "core/keyring" {
			t.Fatal("exported bundle contained keyring")
		}
		if entry.Key == "core/root-key" {
			t.Fatal("exported bundle contained root key")
		}
	}
	if !foundReplicated {
		t.Fatalf("expected exported bundle to contain %q", replicatedKey)
	}
}

func TestDRRelationshipManagerGenerateSegmentedPreSeedManifestAndSegment(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	core.clusterAddr.Store("https://primary.test.local:8201")

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	mgr.primary.checkpointArtifacts = newDRCheckpointArtifactStore(core.logger, t.TempDir())
	mgr.primary.checkpointArtifacts.configure(true, time.Hour, drCheckpointArtifactDefaultGlobalBudget, drCheckpointArtifactDefaultPerRelBudget, drCheckpointArtifactDefaultSegmentBytes)
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	changes := make([]physical.ChangeStreamEntry, 0)
	for i := 0; i < 12; i++ {
		key := "secret/data/preseed-segmented-" + strconv.Itoa(i)
		value := []byte(strings.Repeat("segmented-value-", 8) + strconv.Itoa(i))
		if err := core.physical.Put(ctx, &physical.Entry{Key: key, Value: value}); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, physical.ChangeStreamEntry{
			OpType:    physical.PutOperation,
			Key:       key,
			Value:     value,
			RaftIndex: uint64(100 + i),
		})
	}
	mgr.primary.OnChange(changes)

	manifest, entryCount, err := mgr.GenerateSegmentedPreSeedManifest(ctx, token.RelationshipID, 1024, time.Hour)
	if err != nil {
		t.Fatalf("generate segmented pre-seed manifest failed: %v", err)
	}
	if entryCount == 0 {
		t.Fatal("expected segmented export plan to include entries")
	}
	if normalizedDRPreSeedBundleFormat(manifest.BundleFormat) != drPreSeedBundleFormatSegmentedV1 {
		t.Fatalf("expected segmented bundle format, got %q", manifest.BundleFormat)
	}
	if len(manifest.BundleSegments) < 2 {
		t.Fatalf("expected multiple pre-seed segments, got %d", len(manifest.BundleSegments))
	}
	segment, err := mgr.GeneratePreSeedSegment(ctx, manifest, 0)
	if err != nil {
		t.Fatalf("generate pre-seed segment failed: %v", err)
	}
	if segment.SegmentIndex != 0 || segment.EntryCount != manifest.BundleSegments[0].EntryCount {
		t.Fatalf("unexpected segment metadata: %#v", segment)
	}
	if err := validateDRPreSeedSegment(segment, manifest, token, nil, time.Now().UTC()); err != nil {
		t.Fatalf("exported segment did not validate: %v", err)
	}
}

func TestDRPrimaryBuildPreSeedBundleRejectsMissingCheckpointArtifactRecord(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, _, _ := testDRPreSeedManifestFixture(t)

	primary := NewDRReplicationPrimary(core, token.ReplSalt, core.logger, nil)
	primary.checkpointArtifacts = newDRCheckpointArtifactStore(core.logger, t.TempDir())
	primary.checkpointArtifacts.configure(true, time.Hour, drCheckpointArtifactDefaultGlobalBudget, drCheckpointArtifactDefaultPerRelBudget, drCheckpointArtifactDefaultSegmentBytes)
	primary.indexApplied.Store(88)

	key := "secret/data/missing-artifact-record"
	kid := primary.scanner.ComputeKID(key)
	vid := primary.scanner.ComputeVIDWithSealWrap([]byte("value"), false)
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-missing-artifact-record",
			CommitIndex: 88,
		},
		relationshipID:   token.RelationshipID,
		createdAt:        time.Now().UTC(),
		kidToKey:         map[[32]byte]string{kid: key},
		kidToVID:         map[[32]byte][32]byte{kid: vid},
		rangePlanVersion: reconciler.RangePlanVersion,
	}
	if err := primary.cacheCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
	primary.checkpointMu.Lock()
	primary.latestCheckpointByRelationship[token.RelationshipID] = cp.checkpoint.ID
	primary.checkpointMu.Unlock()
	if err := primary.checkpointArtifacts.putArtifact(&drCheckpointArtifact{
		CheckpointID:    cp.checkpoint.ID,
		CheckpointIndex: cp.checkpoint.CommitIndex,
		RelationshipID:  cp.relationshipID,
		CreatedAt:       time.Now().UTC(),
		Path:            t.TempDir(),
		Records:         map[[32]byte]drCheckpointArtifactRecord{},
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := primary.BuildPreSeedBundle(ctx, token.RelationshipID)
	if err == nil || !strings.Contains(err.Error(), "checkpoint_artifact_missing") {
		t.Fatalf("expected missing checkpoint artifact error, got: %v", err)
	}
}

func TestDRRelationshipManagerImportPreSeedBundleReplacesReplicatedStorageAndAcceptsBaseline(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, bundle, now := testDRPreSeedBundleFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := core.physical.Put(ctx, &physical.Entry{Key: "secret/data/stale", Value: []byte("stale")}); err != nil {
		t.Fatal(err)
	}
	if err := core.physical.Put(ctx, &physical.Entry{Key: "core/cluster/local/info", Value: []byte("keep-local")}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ImportPreSeedBundle(ctx, bundle, token, now, true); err != nil {
		t.Fatalf("import pre-seed bundle failed: %v", err)
	}

	stale, err := core.physical.Get(ctx, "secret/data/stale")
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("expected stale replicated key to be removed, got %#v", stale)
	}
	local, err := core.physical.Get(ctx, "core/cluster/local/info")
	if err != nil {
		t.Fatal(err)
	}
	if local == nil || string(local.Value) != "keep-local" {
		t.Fatalf("expected local-only key to be preserved, got %#v", local)
	}
	for _, entry := range bundle.Entries {
		got, err := core.physical.Get(ctx, entry.Key)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || string(got.Value) != string(entry.Value) {
			t.Fatalf("unexpected imported entry for %q: %#v", entry.Key, got)
		}
	}
	accepted, err := core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if accepted == nil {
		t.Fatal("expected accepted pre-seed record after import")
	}
	assertDRPreSeedOptimizerBaseline(t, ctx, core, token, bundle)

	if err := mgr.EnableSecondary(ctx, token, "local-control"); err != nil {
		t.Fatalf("enable secondary with imported pre-seed failed: %v", err)
	}
	defer mgr.DisableSecondary(ctx)
	if got := mgr.Secondary().lastAppliedIndex.Load(); got != bundle.Manifest.CheckpointIndex {
		t.Fatalf("expected last applied index %d, got %d", bundle.Manifest.CheckpointIndex, got)
	}
	assertDRPreSeedOptimizerBaseline(t, ctx, core, token, bundle)
	accepted, err = core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != nil {
		t.Fatalf("expected accepted pre-seed record to be consumed, got %#v", accepted)
	}
}

func TestDRRelationshipManagerAcceptPreSeedManifestRequiresConfirmations(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	err := mgr.AcceptPreSeedManifest(ctx, manifest, token, now, false, true)
	if err == nil || !strings.Contains(err.Error(), "confirm_storage_restored") {
		t.Fatalf("expected storage confirmation error, got: %v", err)
	}
	err = mgr.AcceptPreSeedManifest(ctx, manifest, token, now, true, false)
	if err == nil || !strings.Contains(err.Error(), "confirm_local_only_scrubbed") {
		t.Fatalf("expected local-only scrub confirmation error, got: %v", err)
	}
}

func TestDRPreSeedReconciliationSetFromBundle(t *testing.T) {
	_, bundle, _ := testDRPreSeedBundleFixture(t)

	rs, err := preSeedReconciliationSetFromBundle(bundle)
	if err != nil {
		t.Fatalf("build pre-seed reconciliation set failed: %v", err)
	}
	if rs.Checkpoint.ID != bundle.Manifest.CheckpointID || rs.Checkpoint.CommitIndex != bundle.Manifest.CheckpointIndex {
		t.Fatalf("unexpected checkpoint: %#v", rs.Checkpoint)
	}
	if rs.KeyCount != len(bundle.Entries) || len(rs.KIDToVID) != len(bundle.Entries) || len(rs.KIDToKey) != len(bundle.Entries) {
		t.Fatalf("unexpected set sizes: key_count=%d kid_to_vid=%d kid_to_key=%d entries=%d", rs.KeyCount, len(rs.KIDToVID), len(rs.KIDToKey), len(bundle.Entries))
	}
	for _, entry := range bundle.Entries {
		var kid [32]byte
		var vid [32]byte
		copy(kid[:], entry.KID)
		copy(vid[:], entry.VID)
		if got := rs.KIDToKey[kid]; got != entry.Key {
			t.Fatalf("unexpected key for kid %x: %q", kid, got)
		}
		if got := rs.KIDToVID[kid]; got != vid {
			t.Fatalf("unexpected vid for kid %x: %x", kid, got)
		}
	}
}

func TestDRRelationshipManagerEnableSecondaryAppliesAcceptedPreSeedBaseline(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := mgr.AcceptPreSeedManifest(ctx, manifest, token, now, true, true); err != nil {
		t.Fatalf("accept pre-seed manifest failed: %v", err)
	}
	if err := mgr.EnableSecondary(ctx, token, "local-control"); err != nil {
		t.Fatalf("enable secondary with accepted pre-seed failed: %v", err)
	}
	defer mgr.DisableSecondary(ctx)

	if got := mgr.Secondary().lastAppliedIndex.Load(); got != manifest.CheckpointIndex {
		t.Fatalf("expected last applied index %d, got %d", manifest.CheckpointIndex, got)
	}
	entry, err := core.physical.Get(ctx, drCheckpointHWMPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || strings.TrimSpace(string(entry.Value)) != "42" {
		t.Fatalf("expected checkpoint high-water mark 42, got %#v", entry)
	}
	streamEntry, err := core.physical.Get(ctx, drStreamAppliedIndexStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if streamEntry == nil {
		t.Fatal("expected persisted stream applied index marker")
	}
	var streamMarker drPersistedStreamAppliedIndex
	if err := json.Unmarshal(streamEntry.Value, &streamMarker); err != nil {
		t.Fatal(err)
	}
	if streamMarker.CommitIndex != manifest.CheckpointIndex ||
		streamMarker.RelationshipID != token.RelationshipID ||
		streamMarker.ClusterID != token.ClusterID {
		t.Fatalf("unexpected stream marker: %#v", streamMarker)
	}
	accepted, err := core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != nil {
		t.Fatalf("expected accepted pre-seed record to be consumed, got %#v", accepted)
	}
}

func TestDRRelationshipManagerLoadConfigAppliesAcceptedPreSeedBaseline(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := mgr.AcceptPreSeedManifest(ctx, manifest, token, now, true, true); err != nil {
		t.Fatalf("accept pre-seed manifest failed: %v", err)
	}
	mgr.config = &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      token.ClusterID,
		RelationshipID: token.RelationshipID,
		ReplSalt:       token.ReplSalt,
		PrimaryAddr:    token.PrimaryAddr,
		PrimaryAddrs:   token.PrimaryAddrs,
		PrimaryCACert:  token.DRTransportCACert,
	}
	if err := mgr.saveConfig(ctx); err != nil {
		t.Fatalf("save secondary config failed: %v", err)
	}

	restored := newDRRelationshipManager(core, core.logger)
	if err := restored.LoadConfig(ctx); err != nil {
		t.Fatalf("load config with accepted pre-seed failed: %v", err)
	}
	defer restored.Teardown()

	if restored.Secondary() == nil {
		t.Fatal("expected secondary runtime after load config")
	}
	if got := restored.Secondary().lastAppliedIndex.Load(); got != manifest.CheckpointIndex {
		t.Fatalf("expected restored last applied index %d, got %d", manifest.CheckpointIndex, got)
	}
	entry, err := core.physical.Get(ctx, drCheckpointHWMPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || strings.TrimSpace(string(entry.Value)) != "42" {
		t.Fatalf("expected checkpoint high-water mark 42, got %#v", entry)
	}
	accepted, err := core.physical.Get(ctx, drPreSeedAcceptedStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != nil {
		t.Fatalf("expected accepted pre-seed record to be consumed, got %#v", accepted)
	}
	if got := restored.Secondary().preSeedBaselineIndex.Load(); got != manifest.CheckpointIndex {
		t.Fatalf("expected pre-seed baseline marker %d, got %d", manifest.CheckpointIndex, got)
	}
}

func TestDRRelationshipManagerEnableSecondaryRejectsMismatchedAcceptedPreSeed(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture(t)
	mgr := newDRRelationshipManager(core, core.logger)

	if err := mgr.AcceptPreSeedManifest(ctx, manifest, token, now, true, true); err != nil {
		t.Fatalf("accept pre-seed manifest failed: %v", err)
	}
	otherToken := *token
	otherToken.RelationshipID = "rel-other"
	err := mgr.EnableSecondary(ctx, &otherToken, "local-control")
	if err == nil || !strings.Contains(err.Error(), "accepted DR pre-seed record is incompatible") {
		t.Fatalf("expected accepted pre-seed mismatch, got: %v", err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected mode to remain disabled after rejected pre-seed, got %s", mgr.Mode())
	}
}

func TestDRPreSeedBootstrapPurgeKeepsImportedReplicatedPlane(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, _, _ := testDRPreSeedManifestFixture(t)
	secondary := newDRReplicationSecondary(core, token.ReplSalt, token.RelationshipID, core.logger)
	secondary.preSeedBaselineIndex.Store(42)

	entries := map[string][]byte{
		"logical/secret/data/imported":                 []byte("replicated-data"),
		"core/mounts":                                  []byte("replicated-mounts"),
		"core/keyring":                                 []byte("primary-keyring"),
		"core/root-key":                                []byte("primary-root-key"),
		"core/hsm/barrier-unseal-keys":                 []byte("seal-wrapped-root"),
		"core/seal-config":                             []byte("seal-config"),
		"core/cluster/local/dr/stream-applied-index":   []byte("stream-baseline"),
		"core/cluster/local/dr/flat-accumulator":       []byte("optimizer-state"),
		"core/local-mounts":                            []byte("stale-local-mounts"),
		"core/local-mounts/6e0aa9a6-6259-7189-2294-f3": []byte("stale-local-mount"),
		"core/local-auth":                              []byte("stale-local-auth"),
		"core/local-audit":                             []byte("stale-local-audit"),
		"core/cluster/local/info":                      []byte("stale-cluster-info"),
		"core/leader/leader-uuid":                      []byte("stale-leader"),
		"core/raft/tls":                                []byte("stale-raft-tls"),
		"core/dr-replication/config":                   []byte("stale-dr-config"),
	}
	for key, value := range entries {
		if err := core.physical.Put(ctx, &physical.Entry{Key: key, Value: value}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	if err := secondary.purgePreSeedBootstrapLocalEntries(ctx); err != nil {
		t.Fatalf("pre-seed bootstrap purge failed: %v", err)
	}

	for _, key := range []string{
		"logical/secret/data/imported",
		"core/mounts",
		"core/keyring",
		"core/root-key",
		"core/hsm/barrier-unseal-keys",
		"core/seal-config",
		"core/cluster/local/dr/stream-applied-index",
		"core/cluster/local/dr/flat-accumulator",
	} {
		entry, err := core.physical.Get(ctx, key)
		if err != nil {
			t.Fatalf("get preserved %s: %v", key, err)
		}
		if entry == nil {
			t.Fatalf("expected %s to be preserved", key)
		}
	}

	for _, key := range []string{
		"core/local-mounts",
		"core/local-mounts/6e0aa9a6-6259-7189-2294-f3",
		"core/local-auth",
		"core/local-audit",
		"core/cluster/local/info",
		"core/leader/leader-uuid",
		"core/raft/tls",
		"core/dr-replication/config",
	} {
		entry, err := core.physical.Get(ctx, key)
		if err != nil {
			t.Fatalf("get purged %s: %v", key, err)
		}
		if entry != nil {
			t.Fatalf("expected %s to be purged", key)
		}
	}
}

func assertDRPreSeedOptimizerBaseline(t *testing.T, ctx context.Context, core *Core, token *DRActivationToken, bundle *DRPreSeedBundle) {
	t.Helper()

	rs, err := preSeedReconciliationSetFromBundle(bundle)
	if err != nil {
		t.Fatalf("build expected pre-seed set: %v", err)
	}
	expectedBuckets := drFlatAccumulatorBucketsFromSet(rs)
	snapshotEntry, err := core.physical.Get(ctx, drFlatAccumulatorStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotEntry == nil {
		t.Fatal("expected pre-seed import to persist flat accumulator snapshot")
	}
	var snapshot drFlatAccumulatorPersistedSnapshot
	if err := json.Unmarshal(snapshotEntry.Value, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.RelationshipID != token.RelationshipID ||
		snapshot.ClusterID != token.ClusterID ||
		snapshot.CommitIndex != bundle.Manifest.CheckpointIndex ||
		len(snapshot.Buckets) != drRangeMaxTotalRanges {
		t.Fatalf("unexpected flat accumulator snapshot: %#v", snapshot)
	}
	for i, expected := range expectedBuckets {
		got := snapshot.Buckets[i]
		if got.Checksum != expected.checksum || got.KIDChecksum != expected.kidChecksum || got.Count != expected.count {
			t.Fatalf("bucket %d mismatch: got %#v expected %#v", i, got, expected)
		}
	}

	streamEntry, err := core.physical.Get(ctx, drStreamAppliedIndexStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if streamEntry == nil {
		t.Fatal("expected pre-seed import to persist stream applied index")
	}
	var streamMarker drPersistedStreamAppliedIndex
	if err := json.Unmarshal(streamEntry.Value, &streamMarker); err != nil {
		t.Fatal(err)
	}
	if streamMarker.RelationshipID != token.RelationshipID ||
		streamMarker.ClusterID != token.ClusterID ||
		streamMarker.CommitIndex != bundle.Manifest.CheckpointIndex {
		t.Fatalf("unexpected stream marker: %#v", streamMarker)
	}

	metaEntry, err := core.physical.Get(ctx, drLocalKIDIndexMetaPath)
	if err != nil {
		t.Fatal(err)
	}
	if metaEntry == nil {
		t.Fatal("expected pre-seed import to persist local KID index metadata")
	}
	var meta drLocalKIDIndexMeta
	if err := json.Unmarshal(metaEntry.Value, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.RelationshipID != token.RelationshipID ||
		meta.ClusterID != token.ClusterID ||
		meta.CommitIndex != bundle.Manifest.CheckpointIndex {
		t.Fatalf("unexpected local KID index metadata: %#v", meta)
	}
	for _, entry := range bundle.Entries {
		var kid [32]byte
		var vid [32]byte
		copy(kid[:], entry.KID)
		copy(vid[:], entry.VID)
		indexEntry, err := core.physical.Get(ctx, drLocalKIDIndexEntryStoragePath(kid))
		if err != nil {
			t.Fatal(err)
		}
		if indexEntry == nil {
			t.Fatalf("expected local KID index entry for %q", entry.Key)
		}
		var item drLocalKIDIndexEntry
		if err := json.Unmarshal(indexEntry.Value, &item); err != nil {
			t.Fatal(err)
		}
		if item.RelationshipID != token.RelationshipID ||
			item.ClusterID != token.ClusterID ||
			item.Key != entry.Key ||
			string(item.VID) != string(vid[:]) {
			t.Fatalf("unexpected local KID index entry for %q: %#v", entry.Key, item)
		}
	}
}

func testDRPreSeedManifestFixture(t *testing.T) (*DRActivationToken, *DRPreSeedManifest, time.Time) {
	t.Helper()

	now := time.Now().UTC()
	replSalt := sha256.Sum256([]byte("preseed-test-repl-salt"))
	token := &DRActivationToken{
		ClusterID:      "primary-preseed",
		RelationshipID: "rel-preseed",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       replSalt[:],
	}
	replSaltHash := sha256.Sum256(token.ReplSalt)
	bundleHash := sha256.Sum256([]byte("preseed-bundle"))
	manifest := &DRPreSeedManifest{
		Version:                  drPreSeedManifestVersion,
		PrimaryClusterID:         token.ClusterID,
		RelationshipID:           token.RelationshipID,
		CheckpointID:             "checkpoint-preseed",
		CheckpointIndex:          42,
		CreatedAtUnix:            now.Unix(),
		ExpiresAtUnix:            now.Add(time.Hour).Unix(),
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
		BundleIntegritySHA256:    bundleHash[:],
	}
	testSignDRPreSeedManifest(t, manifest, token)
	return token, manifest, now
}

func testSignDRPreSeedManifest(t *testing.T, manifest *DRPreSeedManifest, token *DRActivationToken) {
	t.Helper()

	caCert, caKey := newTestDRTransportCACert(t)
	token.DRTransportCACert = caCert.Raw
	if err := signDRPreSeedManifestProvenance(manifest, &drTransportCA{
		cert:    caCert,
		certDER: caCert.Raw,
		key:     caKey,
	}); err != nil {
		t.Fatalf("failed to sign test pre-seed manifest: %v", err)
	}
}

func testDRPreSeedBundleFixture(t *testing.T) (*DRActivationToken, *DRPreSeedBundle, time.Time) {
	t.Helper()

	token, manifest, now := testDRPreSeedManifestFixture(t)
	scanner := drPreSeedScanner(token.ReplSalt, nil)
	rawEntries := []struct {
		key      string
		value    []byte
		sealWrap bool
	}{
		{key: "secret/data/app", value: []byte("app-ciphertext")},
		{key: "auth/userpass/users/alice", value: []byte("alice-ciphertext"), sealWrap: true},
	}
	entries := make([]DRPreSeedBundleEntry, 0, len(rawEntries))
	for _, raw := range rawEntries {
		kid := scanner.ComputeKID(raw.key)
		vid := scanner.ComputeVIDWithSealWrap(raw.value, raw.sealWrap)
		entries = append(entries, DRPreSeedBundleEntry{
			Key:      raw.key,
			Value:    append([]byte(nil), raw.value...),
			SealWrap: raw.sealWrap,
			KID:      kid[:],
			VID:      vid[:],
		})
	}
	bundle := &DRPreSeedBundle{
		Version:    drPreSeedBundleVersion,
		Manifest:   *manifest,
		EntryCount: len(entries),
		Entries:    entries,
	}
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)
	return token, bundle, now
}

func testDRPreSeedSegmentedBundleFixture(t *testing.T) (*DRActivationToken, *DRPreSeedBundle, []*DRPreSeedSegment, time.Time) {
	t.Helper()

	token, bundle, now := testDRPreSeedBundleFixture(t)
	segments, err := buildDRPreSeedBundleSegmentPlan(bundle, testDRPreSeedMaxSingleSegmentBytes(t, bundle.Entries))
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleFormat = drPreSeedBundleFormatSegmentedV1
	bundle.Manifest.BundleSegments = segments
	bundle.Manifest.BundleIntegritySHA256 = make([]byte, sha256.Size)
	sum, err := computeDRPreSeedBundleIntegrity(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.BundleIntegritySHA256 = sum
	testSignDRPreSeedManifest(t, &bundle.Manifest, token)

	out := make([]*DRPreSeedSegment, 0, len(segments))
	for i := range segments {
		entries, err := preSeedSegmentEntries(bundle.Entries, segments, i)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, &DRPreSeedSegment{
			Version:      drPreSeedBundleVersion,
			Manifest:     bundle.Manifest,
			SegmentIndex: i,
			EntryCount:   len(entries),
			Entries:      entries,
		})
	}
	return token, bundle, out, now
}

func testDRPreSeedMaxSingleSegmentBytes(t *testing.T, entries []DRPreSeedBundleEntry) int {
	t.Helper()

	maxBytes := uint64(0)
	for i, entry := range canonicalDRPreSeedBundleEntries(entries) {
		descriptor, err := buildDRPreSeedSegmentDescriptor(i, []DRPreSeedBundleEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.ByteCount > maxBytes {
			maxBytes = descriptor.ByteCount
		}
	}
	if maxBytes == 0 {
		t.Fatal("expected non-empty fixture entries")
	}
	return int(maxBytes)
}

type maxValuePhysicalBackend struct {
	physical.Backend
	maxValueBytes int
}

func (b *maxValuePhysicalBackend) Put(ctx context.Context, entry *physical.Entry) error {
	if entry != nil && len(entry.Value) > b.maxValueBytes {
		return fmt.Errorf("test backend rejected value length %d above max %d", len(entry.Value), b.maxValueBytes)
	}
	return b.Backend.Put(ctx, entry)
}
