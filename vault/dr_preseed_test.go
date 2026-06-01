// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

func TestDRPreSeedManifestValidationAcceptsMatchingFreshRelationship(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture()

	if err := validateDRPreSeedManifest(manifest, token, nil, now); err != nil {
		t.Fatalf("expected valid pre-seed manifest, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsRelationshipMismatch(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture()
	manifest.RelationshipID = "rel-other"

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "relationship_id mismatch") {
		t.Fatalf("expected relationship mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsAlgorithmMismatch(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture()
	manifest.ChecksumAlgorithm = "crc64-old"

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "checksum_algorithm mismatch") {
		t.Fatalf("expected checksum algorithm mismatch, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsMissingLocalOnlyScrubMetadata(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture()
	manifest.LocalOnlyScrubVersion = 0
	manifest.LocalOnlyExactPaths = nil
	manifest.LocalOnlyPrefixes = nil

	err := validateDRPreSeedManifest(manifest, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "local-only scrub") {
		t.Fatalf("expected local-only scrub metadata failure, got: %v", err)
	}
}

func TestDRPreSeedManifestValidationRejectsStalePromotionLineage(t *testing.T) {
	token, manifest, now := testDRPreSeedManifestFixture()
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
	token, manifest, now := testDRPreSeedManifestFixture()

	err := validateDRPreSeedManifest(manifest, token, nil, now.Add(2*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired manifest rejection, got: %v", err)
	}
}

func TestDRRelationshipManagerValidatePreSeedManifestUsesPreservedPromotionLineage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	token, manifest, now := testDRPreSeedManifestFixture()
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

	err = validateDRPreSeedBundle(bundle, token, nil, now)
	if err == nil || !strings.Contains(err.Error(), "local-only or excluded") {
		t.Fatalf("expected root-key path rejection, got: %v", err)
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

func TestDRPrimaryBuildPreSeedBundleRejectsMissingCheckpointArtifactRecord(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, _, _ := testDRPreSeedManifestFixture()

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

	if err := mgr.EnableSecondary(ctx, token, "local-control"); err != nil {
		t.Fatalf("enable secondary with imported pre-seed failed: %v", err)
	}
	defer mgr.DisableSecondary(ctx)
	if got := mgr.Secondary().lastAppliedIndex.Load(); got != bundle.Manifest.CheckpointIndex {
		t.Fatalf("expected last applied index %d, got %d", bundle.Manifest.CheckpointIndex, got)
	}
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
	token, manifest, now := testDRPreSeedManifestFixture()
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

func TestDRRelationshipManagerEnableSecondaryAppliesAcceptedPreSeedBaseline(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture()
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
	token, manifest, now := testDRPreSeedManifestFixture()
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
}

func TestDRRelationshipManagerEnableSecondaryRejectsMismatchedAcceptedPreSeed(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	token, manifest, now := testDRPreSeedManifestFixture()
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

func testDRPreSeedManifestFixture() (*DRActivationToken, *DRPreSeedManifest, time.Time) {
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
		BundleIntegrityAlgorithm: drPreSeedBundleIntegrityAlgorithm,
		BundleIntegritySHA256:    bundleHash[:],
	}
	return token, manifest, now
}

func testDRPreSeedBundleFixture(t *testing.T) (*DRActivationToken, *DRPreSeedBundle, time.Time) {
	t.Helper()

	token, manifest, now := testDRPreSeedManifestFixture()
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
	return token, bundle, now
}
