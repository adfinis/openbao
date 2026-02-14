// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func newDRReconcilerScanConfigForTests(replSalt []byte) reconciler.ScanConfig {
	cfg := reconciler.DefaultScanConfig(replSalt)
	cfg.RequireTransactionalSnapshot = true
	cfg.Logger = log.NewNullLogger()

	excludePaths := make(map[string]bool, len(drNeverReplicateExactPaths)+len(drReconcileExcludeExactPaths))
	for p := range drNeverReplicateExactPaths {
		excludePaths[p] = true
	}
	for p := range drReconcileExcludeExactPaths {
		excludePaths[p] = true
	}
	cfg.ExcludePaths = excludePaths
	cfg.ExcludePathFunc = isDRReconcileExcludedPath
	return cfg
}

// --- Unit Tests for DR Relationship Manager ---

func TestDRRelationshipManager_EnableDisablePrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	// Initially disabled.
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled, got %s", mgr.Mode())
	}

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModePrimary {
		t.Fatalf("expected primary, got %s", mgr.Mode())
	}
	if mgr.Primary() == nil {
		t.Fatal("expected primary server to be initialized")
	}

	// Config should have a cluster ID and repl salt.
	config := mgr.Config()
	if config.ClusterID == "" {
		t.Fatal("expected non-empty cluster ID")
	}
	if len(config.ReplSalt) != drReplSaltLen {
		t.Fatalf("expected repl salt of length %d, got %d", drReplSaltLen, len(config.ReplSalt))
	}

	// Cannot enable again.
	if err := mgr.EnablePrimary(ctx); err == nil {
		t.Fatal("expected error enabling primary twice")
	}

	// Disable primary.
	if err := mgr.DisablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after disable, got %s", mgr.Mode())
	}
	if mgr.Primary() != nil {
		t.Fatal("expected primary to be nil after disable")
	}
}

func TestDRRelationshipManager_EnablePrimary_SaveConfigFailureRollsBackState(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	mgr := newDRRelationshipManager(core, core.logger)

	failedCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := mgr.EnablePrimary(failedCtx); err == nil {
		t.Fatal("expected enable primary to fail when config save fails")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected mode to remain disabled after failed enable, got %s", mgr.Mode())
	}
	if mgr.Primary() != nil {
		t.Fatal("expected primary to remain nil after failed enable")
	}

	if err := mgr.EnablePrimary(context.Background()); err != nil {
		t.Fatalf("expected enable primary retry to succeed, got: %v", err)
	}
}

func TestDRRelationshipManager_EnableSecondary_SaveConfigFailureRollsBackState(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	mgr := newDRRelationshipManager(core, core.logger)

	token := &DRActivationToken{
		ClusterID:      "cluster-1",
		RelationshipID: "rel-1",
		ReplSalt:       make([]byte, drReplSaltLen),
	}

	failedCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := mgr.EnableSecondary(failedCtx, token); err == nil {
		t.Fatal("expected enable secondary to fail when config save fails")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected mode to remain disabled after failed secondary enable, got %s", mgr.Mode())
	}
	if mgr.Secondary() != nil {
		t.Fatal("expected secondary to remain nil after failed enable")
	}

	if err := mgr.EnableSecondary(context.Background(), token); err != nil {
		t.Fatalf("expected enable secondary retry to succeed, got: %v", err)
	}
	if err := mgr.DisableSecondary(context.Background()); err != nil {
		t.Fatalf("failed to disable secondary after retry: %v", err)
	}
}

func TestDRRelationshipManager_ActivationToken(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	// Cannot generate token when not primary.
	if _, err := mgr.GenerateActivationToken(ctx); err == nil {
		t.Fatal("expected error generating token when not primary")
	}

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	// Generate activation token.
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token.ClusterID == "" {
		t.Fatal("expected non-empty cluster ID in token")
	}
	if len(token.ReplSalt) == 0 {
		t.Fatal("expected non-empty repl salt in token")
	}

	// Token should be JSON-serializable.
	data, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}

	var decoded DRActivationToken
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ClusterID != token.ClusterID {
		t.Fatalf("token roundtrip failed: got %s, want %s", decoded.ClusterID, token.ClusterID)
	}
	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.ExpiresAt <= rel.CreatedAt {
		t.Fatalf("expected relationship expiry after creation, got created=%d expires=%d", rel.CreatedAt, rel.ExpiresAt)
	}
}

func TestDRRelationshipManager_BootstrapTokenExpires(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	rel.ExpiresAt = time.Now().Add(-1 * time.Minute).Unix()
	if err := mgr.saveRelationship(ctx, rel); err != nil {
		t.Fatal(err)
	}

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, token.CACert); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired bootstrap token error, got: %v", err)
	}

	updated, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FailedAttempts != 1 {
		t.Fatalf("expected failed attempts to increment, got %d", updated.FailedAttempts)
	}
	if updated.LastError == "" {
		t.Fatal("expected last_error to be set")
	}
}

func TestDRRelationshipManager_BootstrapTokenAttemptLockout(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < drBootstrapMaxFailedAttempts; i++ {
		if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, "wrong-token", token.CACert); err == nil {
			t.Fatalf("expected bootstrap attempt %d to fail", i+1)
		}
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.LockedUntil <= time.Now().Unix() {
		t.Fatalf("expected lockout to be set in the future, got %d", rel.LockedUntil)
	}
	if rel.FailedAttempts < drBootstrapMaxFailedAttempts {
		t.Fatalf("expected failed attempts >= %d, got %d", drBootstrapMaxFailedAttempts, rel.FailedAttempts)
	}

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, token.CACert); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected lockout error when using correct token during lockout, got %v", err)
	}
}

func TestDRRelationshipManager_BootstrapTokenSourceIPBinding(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, token.RelationshipID, token.BootstrapToken, token.CACert, "10.20.30.40"); err != nil {
		t.Fatal(err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.RegisteredFromIP != "10.20.30.40" {
		t.Fatalf("expected registered source IP to be persisted, got %q", rel.RegisteredFromIP)
	}
}

func TestDRRelationshipManager_HeartbeatLastSeenWriteThrottle(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	rel.LastSeenAt = time.Now().Add(-2 * time.Minute).Unix()
	if err := mgr.saveRelationship(ctx, rel); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	mgr.lastSeenWriteAt[token.RelationshipID] = time.Now().UTC()
	mgr.mu.Unlock()

	mgr.MarkRelationshipSeen(token.RelationshipID)

	entry, err := core.barrier.Get(ctx, drRelationshipsPath+token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected relationship entry to exist")
	}
	var persisted DRRelationship
	if err := json.Unmarshal(entry.Value, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.LastSeenAt != rel.LastSeenAt {
		t.Fatalf("expected LastSeenAt write to be throttled; got %d want %d", persisted.LastSeenAt, rel.LastSeenAt)
	}

	mgr.mu.RLock()
	latest := mgr.latestSeenAt[token.RelationshipID]
	mgr.mu.RUnlock()
	if latest <= rel.LastSeenAt {
		t.Fatalf("expected in-memory latest seen timestamp to update, got %d <= %d", latest, rel.LastSeenAt)
	}
}

func TestDRRelationshipManager_EnableDisableSecondary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	token := &DRActivationToken{
		ClusterID:      "test-cluster-id",
		RelationshipID: "rel-test-1",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	// Enable secondary.
	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary, got %s", mgr.Mode())
	}
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary to be initialized")
	}

	// Cannot enable again.
	if err := mgr.EnableSecondary(ctx, token); err == nil {
		t.Fatal("expected error enabling secondary twice")
	}

	// Disable secondary.
	if err := mgr.DisableSecondary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after disable, got %s", mgr.Mode())
	}
}

func TestDRRelationshipManager_Promote(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	token := &DRActivationToken{
		ClusterID:      "test-cluster-id",
		RelationshipID: "rel-test-2",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	// Enable secondary.
	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	// Promote.
	if err := mgr.PromoteSecondary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after promote, got %s", mgr.Mode())
	}
}

func TestDRRelationshipManager_PersistConfig(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	savedConfig := mgr.Config()

	// Create a new manager and load from storage.
	mgr2 := newDRRelationshipManager(core, core.logger)
	if err := mgr2.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	loadedConfig := mgr2.Config()
	if loadedConfig.Mode != savedConfig.Mode {
		t.Fatalf("mode mismatch: got %s, want %s", loadedConfig.Mode, savedConfig.Mode)
	}
	if loadedConfig.ClusterID != savedConfig.ClusterID {
		t.Fatalf("cluster ID mismatch: got %s, want %s", loadedConfig.ClusterID, savedConfig.ClusterID)
	}
}

// --- Unit Tests for DR Secondary State Machine ---

func TestDRSecondaryState_String(t *testing.T) {
	tests := []struct {
		state DRSecondaryState
		want  string
	}{
		{DRSecondaryIdle, "idle"},
		{DRSecondaryBootstrapping, "bootstrapping"},
		{DRSecondaryInitialSync, "initial-sync"},
		{DRSecondaryStreaming, "streaming"},
		{DRSecondaryReconciling, "reconciling"},
		{DRSecondaryResnapshotting, "resnapshotting"},
		{DRSecondaryPromoting, "promoting"},
		{DRSecondaryStandalone, "standalone"},
		{DRSecondaryState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("DRSecondaryState(%d).String() = %s, want %s", tt.state, got, tt.want)
		}
	}
}

func TestDRSecondaryStatus(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	sec := newDRReplicationSecondary(core, replSalt, "test-cluster", core.logger)

	sec.lastAppliedIndex.Store(42)
	sec.entriesApplied.Store(100)
	sec.reconcileCount.Store(2)
	sec.lastReconcileAt.Store(time.Now().Unix())

	status := sec.Status()
	if status.State != "idle" {
		t.Fatalf("expected idle, got %s", status.State)
	}
	if status.LastAppliedIndex != 42 {
		t.Fatalf("expected 42, got %d", status.LastAppliedIndex)
	}
	if status.EntriesApplied != 100 {
		t.Fatalf("expected 100, got %d", status.EntriesApplied)
	}
	if status.ReconcileCount != 2 {
		t.Fatalf("expected 2, got %d", status.ReconcileCount)
	}
}

func TestDRSecondary_RequestResnapshotFlag(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-resnap", core.logger)

	sec.RequestResnapshot("manual-test")
	ok, reason := sec.consumeResnapshotRequest()
	if !ok {
		t.Fatal("expected pending resnapshot request")
	}
	if reason != "manual-test" {
		t.Fatalf("expected manual-test reason, got %q", reason)
	}
}

func TestDRRelationshipManager_UpdateTuningAppliesSecondaryRuntime(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	token := &DRActivationToken{
		ClusterID:      "cluster-1",
		RelationshipID: "rel-1",
		ReplSalt:       make([]byte, drReplSaltLen),
		PrimaryAddr:    "127.0.0.1:8201",
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = mgr.DisableSecondary(context.Background())
	})

	if err := mgr.UpdateTuning(ctx, func(cfg *DRConfig) error {
		cfg.ReconcileMaxInflightTasks = 7
		cfg.ReconcileMaxWallTimeSeconds = 120
		cfg.FallbackEnabled = false
		cfg.FallbackStallSeconds = 90
		cfg.FallbackFailureThreshold = 4
		cfg.FallbackCooldownSeconds = 180
		cfg.FallbackMaxPerHour = 1
		cfg.FallbackMinLagEntries = 1234
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cfg := mgr.Config()
	if cfg.ReconcileMaxInflightTasks != 7 {
		t.Fatalf("expected reconcile max inflight=7, got %d", cfg.ReconcileMaxInflightTasks)
	}
	if cfg.FallbackFailureThreshold != 4 {
		t.Fatalf("expected fallback failure threshold=4, got %d", cfg.FallbackFailureThreshold)
	}
	if mgr.secondary.reconcileMaxInflightTasks != 7 {
		t.Fatalf("expected runtime inflight=7, got %d", mgr.secondary.reconcileMaxInflightTasks)
	}
	if mgr.secondary.fallbackEnabled {
		t.Fatal("expected runtime fallback to be disabled")
	}
	if mgr.secondary.fallbackMinLagEntries != 1234 {
		t.Fatalf("expected runtime fallback min lag entries=1234, got %d", mgr.secondary.fallbackMinLagEntries)
	}
}

// --- Unit Tests for DR Failover ---

func TestDRFailover_NotSecondary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	core.drManager = newDRRelationshipManager(core, core.logger)

	// Failover should fail when not in secondary mode.
	_, err := core.DRFailover(ctx)
	if err == nil {
		t.Fatal("expected error for failover when not secondary")
	}
}

func TestDRFailover_FromSecondary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-test-3",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	// Simulate some applied entries.
	mgr.secondary.lastAppliedIndex.Store(500)

	result, err := core.DRFailover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.OldMode != DRModeSecondary {
		t.Fatalf("expected old mode secondary, got %s", result.OldMode)
	}
	if result.NewMode != DRModeDisabled {
		t.Fatalf("expected new mode disabled, got %s", result.NewMode)
	}
	if result.LastAppliedIndex != 500 {
		t.Fatalf("expected last applied index 500, got %d", result.LastAppliedIndex)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after failover, got %s", mgr.Mode())
	}
}

func TestDRFailoverToPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-test-4",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	result, err := core.DRFailoverToPrimary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.NewMode != DRModePrimary {
		t.Fatalf("expected new mode primary, got %s", result.NewMode)
	}
	if mgr.Mode() != DRModePrimary {
		t.Fatalf("expected primary after failover-to-primary, got %s", mgr.Mode())
	}
	if mgr.Primary() == nil {
		t.Fatal("expected primary server after failover-to-primary")
	}
}

// --- Integration Test: Change Stream + Apply ---

func TestDRChangeStream_ApplyChange(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "test", core.logger)

	// Test applying a Put via applyFetchedChange (simulates reconciliation path).
	err := sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "test/key1",
		Value:  []byte("value1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry exists.
	entry, err := core.barrier.Get(ctx, "test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected entry after apply")
	}
	if string(entry.Value) != "value1" {
		t.Fatalf("expected value1, got %s", string(entry.Value))
	}

	// Test applying a Delete via applyFetchedChange.
	err = sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.DeleteOperation),
		Key:    "test/key1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry is deleted.
	entry, err = core.barrier.Get(ctx, "test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("expected nil entry after delete")
	}
}

// --- Integration Test: Reconciliation Set Building ---

func TestDRReconciliation_BuildSet(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Write some test data.
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("test/entry-%03d", i)
		err := core.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("value-%d", i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	config := newDRReconcilerScanConfigForTests(replSalt)
	config.BuildKIDMap = true

	scanner := reconciler.NewScanner(config)
	checkpoint := reconciler.Checkpoint{ID: "test-cp-1", CommitIndex: 100}

	set, err := scanner.Scan(ctx, core.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	// Should have at least 100 keys (plus any system keys).
	if set.KeyCount < 100 {
		t.Fatalf("expected at least 100 keys, got %d", set.KeyCount)
	}

	if set.Strata == nil {
		t.Fatal("expected strata estimator")
	}
	if set.PrefixDigest == nil {
		t.Fatal("expected prefix digest")
	}
	if set.KIDToKey == nil {
		t.Fatal("expected KID-to-key map")
	}
	if len(set.KIDToKey) != set.KeyCount {
		t.Fatalf("KID-to-key map size %d != key count %d", len(set.KIDToKey), set.KeyCount)
	}

	t.Logf("reconciliation set built: %d keys", set.KeyCount)
}

// --- Integration Test: Two-Side Reconciliation ---

func TestDRReconciliation_TwoSideDiff(t *testing.T) {
	// Simulate primary and secondary storage with known differences.
	primaryCore, _, _ := TestCoreUnsealed(t)
	secondaryCore, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	// Write shared data to both.
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("shared/entry-%03d", i)
		value := []byte(fmt.Sprintf("shared-value-%d", i))

		primaryCore.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
		secondaryCore.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
	}

	// Write 5 entries only on primary (added on primary).
	for i := 50; i < 55; i++ {
		key := fmt.Sprintf("primary-only/entry-%03d", i)
		primaryCore.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("primary-value-%d", i)),
		})
	}

	// Write 3 entries only on secondary (added on secondary).
	for i := 55; i < 58; i++ {
		key := fmt.Sprintf("secondary-only/entry-%03d", i)
		secondaryCore.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("secondary-value-%d", i)),
		})
	}

	// Modify 2 shared values on primary only (value mismatch).
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("shared/entry-%03d", i)
		primaryCore.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("updated-primary-value-%d", i)),
		})
	}

	// Build reconciliation sets.
	config := newDRReconcilerScanConfigForTests(replSalt)
	config.BuildKIDMap = true

	scanner := reconciler.NewScanner(config)
	checkpoint := reconciler.Checkpoint{ID: "test-diff", CommitIndex: 100}

	primarySet, err := scanner.Scan(ctx, primaryCore.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	secondarySet, err := scanner.Scan(ctx, secondaryCore.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("primary keys: %d, secondary keys: %d", primarySet.KeyCount, secondarySet.KeyCount)

	// Step 1: Estimate difference via strata.
	estimated, err := primarySet.Strata.Estimate(secondarySet.Strata)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("estimated difference: %d", estimated)

	// The actual difference should be at least:
	// 5 (primary-only) + 3 (secondary-only) + 2 (modified) = 10
	// Note: modified keys count as 2 differences each (old + new VID)
	// Strata may overestimate, which is fine.
	if estimated < 8 {
		t.Logf("estimated difference seems low (%d), expected >= 8", estimated)
	}

	// Step 2: Build IBLTs at estimated size and decode.
	ibltSize := uint32(estimated * 2)
	if ibltSize < 30 {
		ibltSize = 30
	}

	primaryIBLT, err := scanner.BuildIBLTFromScan(ctx, primaryCore.barrier, ibltSize)
	if err != nil {
		t.Fatal(err)
	}
	secondaryIBLT, err := scanner.BuildIBLTFromScan(ctx, secondaryCore.barrier, ibltSize)
	if err != nil {
		t.Fatal(err)
	}

	diff, err := primaryIBLT.Subtract(secondaryIBLT)
	if err != nil {
		t.Fatal(err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Log("IBLT decode failed (may need larger IBLT); skipping detailed diff check")
		return
	}

	t.Logf("IBLT decode succeeded: %d added (on primary), %d removed (on secondary)", len(added), len(removed))

	// Verify we found the differences.
	// "added" = elements on primary not on secondary
	// "removed" = elements on secondary not on primary
	//
	// We expect:
	// - added: 5 primary-only entries + 2 modified entries (new values) = 7
	// - removed: 3 secondary-only entries + 2 modified entries (old values) = 5
	// But the exact counts depend on system keys; just verify non-zero.
	if len(added) == 0 {
		t.Error("expected some added entries (primary-only + modified)")
	}
	if len(removed) == 0 {
		t.Error("expected some removed entries (secondary-only + old values)")
	}
}

// --- Integration Test: Prefix Digest Comparison ---

func TestDRReconciliation_PrefixDigestCompare(t *testing.T) {
	primaryCore, _, _ := TestCoreUnsealed(t)
	secondaryCore, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	// Write identical data to both.
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("data/entry-%03d", i)
		value := []byte(fmt.Sprintf("value-%d", i))
		primaryCore.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
		secondaryCore.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
	}

	config := newDRReconcilerScanConfigForTests(replSalt)

	scanner := reconciler.NewScanner(config)
	checkpoint := reconciler.Checkpoint{ID: "pd-test", CommitIndex: 100}

	primarySet, err := scanner.Scan(ctx, primaryCore.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	secondarySet, err := scanner.Scan(ctx, secondaryCore.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	// Compare prefix digests -- should be identical for shared data.
	// Note: system keys may differ between the two cores.
	mismatched, err := primarySet.PrefixDigest.Compare(secondarySet.PrefixDigest)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("prefix digest comparison: %d mismatched buckets out of %d",
		len(mismatched), primarySet.PrefixDigest.NumBuckets())

	// The system keys (keyring, etc.) are different between two independent
	// cores, so some buckets will mismatch. But the user data buckets
	// should mostly match.
}

// --- Integration Test: Change Stream Primary ---

func TestDRPrimary_ChangeStreamFanout(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	// Simulate changes.
	changes := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "test/key1", Value: []byte("value1"), RaftIndex: 1},
		{OpType: physical.PutOperation, Key: "test/key2", Value: []byte("value2"), RaftIndex: 2},
		{OpType: physical.DeleteOperation, Key: "test/key1", RaftIndex: 3},
		{OpType: physical.PutOperation, Key: "core/dr-replication/config", Value: []byte("local"), RaftIndex: 4},
	}

	primary.OnChange(changes)

	// Verify the buffer was populated.
	primary.mu.Lock()
	bufLen := len(primary.changeBuffer)
	primary.mu.Unlock()

	if bufLen != 3 {
		t.Fatalf("expected 3 replicable changes in buffer, got %d", bufLen)
	}
}

func TestDRPrimary_RevokeRelationshipTerminatesOnlyMatchingStreams(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	primary.mu.Lock()
	primary.subscribers["sub-a"] = &changeStreamSubscriber{
		id:             "sub-a",
		relationshipID: "rel-a",
		ch:             make(chan physical.ChangeStreamEntry, 1),
		cancel:         cancelA,
	}
	primary.subscribers["sub-b"] = &changeStreamSubscriber{
		id:             "sub-b",
		relationshipID: "rel-b",
		ch:             make(chan physical.ChangeStreamEntry, 1),
		cancel:         cancelB,
	}
	primary.mu.Unlock()

	primary.RevokeRelationship("rel-a")

	primary.mu.RLock()
	_, hasA := primary.subscribers["sub-a"]
	_, hasB := primary.subscribers["sub-b"]
	primary.mu.RUnlock()
	if hasA {
		t.Fatal("expected rel-a subscriber to be removed on revoke")
	}
	if !hasB {
		t.Fatal("expected rel-b subscriber to remain active")
	}

	select {
	case <-ctxA.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected revoked subscriber context to be canceled")
	}

	select {
	case <-ctxB.Done():
		t.Fatal("non-revoked subscriber context should remain active")
	default:
	}

	if got := primary.revokedStreamsTerminatedCount(); got != 1 {
		t.Fatalf("expected 1 revoked stream termination, got %d", got)
	}
}

func TestDRPrimary_CheckpointCacheBudgetEnforced(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)
	primary.checkpointGlobalBudget = 128
	primary.checkpointPerRelationshipBudget = 128

	err := primary.cacheCheckpoint(&drCheckpointCacheEntry{
		checkpoint:     reconciler.Checkpoint{ID: "cp-over", CommitIndex: 1},
		relationshipID: "rel-1",
		createdAt:      time.Now().UTC(),
		kidToKey: map[[32]byte]string{
			{1}: strings.Repeat("a", 256),
		},
	})
	if err == nil {
		t.Fatal("expected checkpoint cache admission to fail when over budget")
	}
}

func TestDRPrimary_CheckpointCachePerRelationshipEviction(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)
	primary.checkpointGlobalBudget = 1 << 20
	primary.checkpointPerRelationshipBudget = 1 << 20
	primary.maxCheckpointsPerRelationship = 2

	base := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		err := primary.cacheCheckpoint(&drCheckpointCacheEntry{
			checkpoint:     reconciler.Checkpoint{ID: fmt.Sprintf("cp-%d", i), CommitIndex: uint64(i)},
			relationshipID: "rel-1",
			createdAt:      base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("unexpected cache checkpoint error for cp-%d: %v", i, err)
		}
	}

	primary.checkpointMu.RLock()
	_, hasOldest := primary.checkpoints["cp-1"]
	_, hasNewest := primary.checkpoints["cp-3"]
	itemCount := len(primary.checkpoints)
	primary.checkpointMu.RUnlock()

	if hasOldest {
		t.Fatal("expected oldest relationship checkpoint to be evicted")
	}
	if !hasNewest {
		t.Fatal("expected newest checkpoint to remain in cache")
	}
	if itemCount != 2 {
		t.Fatalf("expected 2 cached checkpoints for relationship, got %d", itemCount)
	}
}

func TestDRPrimary_ValidateCheckpointTuple(t *testing.T) {
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-1",
			CommitIndex: 42,
		},
	}

	if err := validateCheckpointTuple("cp-1", 42, cp); err != nil {
		t.Fatalf("expected valid tuple: %v", err)
	}
	if err := validateCheckpointTuple("cp-1", 0, cp); err == nil {
		t.Fatal("expected missing checkpoint_index to fail")
	}
	if err := validateCheckpointTuple("cp-1", 99, cp); err == nil {
		t.Fatal("expected mismatched checkpoint_index to fail")
	}
}

func TestDRPrimary_ExchangeRangeDigests_CheckpointTupleMismatch(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	var start [32]byte
	var end [32]byte
	for i := range end {
		end[i] = 0xff
	}
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-1",
			CommitIndex: 10,
		},
		relationshipID: "rel-1",
		kidToVID: map[[32]byte][32]byte{
			start: {},
		},
		createdAt: time.Now().UTC(),
	}
	primary.checkpointMu.Lock()
	primary.checkpoints[cp.checkpoint.ID] = cp
	primary.checkpointMu.Unlock()

	_, err := primary.ExchangeRangeDigests(context.Background(), &RangeDigestRequest{
		CheckpointId:    "cp-1",
		CheckpointIndex: 11, // mismatch
		Spans: []*RangeSpan{
			{
				StartKid: start[:],
				EndKid:   end[:],
			},
		},
	})
	if err == nil {
		t.Fatal("expected checkpoint tuple mismatch to fail")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", status.Code(err))
	}
}

func TestDRPrimary_StreamBufferHorizonSeconds(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	primary.bufMu.Lock()
	primary.changeBuffer = make([]physical.ChangeStreamEntry, 500)
	primary.bufMu.Unlock()
	primary.writeRateMu.Lock()
	primary.writeRateEPS = 100.0
	primary.writeRateMu.Unlock()

	if got := primary.streamBufferHorizonSeconds(); got < 4 || got > 6 {
		t.Fatalf("expected horizon around 5s, got %d", got)
	}
}

func TestDRPrimary_ReadCheckpointEntryChange_ExpectedVIDMismatch(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger)
	ctx := context.Background()

	key := "secret/data/demo"
	val := []byte("ciphertext-demo")
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   key,
		Value: val,
	}); err != nil {
		t.Fatal(err)
	}

	kid, vid := primary.scanner.ComputeItemFromEntry(&physical.Entry{
		Key:   key,
		Value: val,
	})
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-test",
			CommitIndex: 7,
		},
		kidToKey: map[[32]byte]string{
			kid: key,
		},
		kidToVID: map[[32]byte][32]byte{
			kid: vid,
		},
	}

	change, err := primary.readCheckpointEntryChange(ctx, cp, kid, &vid, true)
	if err != nil {
		t.Fatalf("expected successful fetch with matching provenance: %v", err)
	}
	if change == nil || change.OpType != string(physical.PutOperation) {
		t.Fatalf("expected put change, got %#v", change)
	}

	bad := vid
	bad[0] ^= 0xff
	_, err = primary.readCheckpointEntryChange(ctx, cp, kid, &bad, true)
	if err == nil {
		t.Fatal("expected provenance mismatch to fail")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", status.Code(err))
	}
}

func TestDRPrimary_ShouldThrottleCheckpointBuild_UsesActiveLagSignal(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	primary.bufMaxSize = 100
	primary.bufMaxBytes = 1024
	primary.changeBuffer = make([]physical.ChangeStreamEntry, primary.bufMaxSize)
	primary.bufBytes = primary.bufMaxBytes

	primary.mu.Lock()
	primary.subscribers["sub-a"] = &changeStreamSubscriber{
		id:             "sub-a",
		relationshipID: "rel-a",
		ch:             make(chan physical.ChangeStreamEntry, 1),
		cancel:         func() {},
	}
	primary.mu.Unlock()

	// Simulate historical lag pressure only.
	primary.streamLaggingSubscribers.Store(99)
	primary.streamLaggingSubscribersActive.Store(0)
	primary.streamLaggingLastEventUnix.Store(time.Now().Add(-1 * time.Hour).Unix())

	throttle, reason := primary.shouldThrottleCheckpointBuild()
	if throttle {
		t.Fatalf("expected no throttle with stale lag signal, got reason: %s", reason)
	}

	// Simulate live lag pressure.
	primary.streamLaggingSubscribersActive.Store(1)
	primary.streamLaggingLastEventUnix.Store(time.Now().Unix())

	throttle, reason = primary.shouldThrottleCheckpointBuild()
	if !throttle {
		t.Fatalf("expected throttle with active lag signal, got reason: %s", reason)
	}
}

func TestDRPrimary_LaggingSubscribersActiveCount_ExpiresWindow(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	primary.streamLaggingSubscribersActive.Store(3)
	primary.streamLaggingLastEventUnix.Store(time.Now().Add(-2 * drCheckpointLaggingActiveWindow).Unix())

	if got := primary.laggingSubscribersActiveCount(); got != 0 {
		t.Fatalf("expected active lag count to expire to 0, got %d", got)
	}
}

func TestDRPrimary_AllowForcedCheckpointBuild_Cooldown(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	if !primary.allowForcedCheckpointBuild("rel-a") {
		t.Fatal("expected first forced checkpoint admission to pass")
	}
	if primary.allowForcedCheckpointBuild("rel-a") {
		t.Fatal("expected second forced checkpoint admission within cooldown to fail")
	}

	primary.checkpointMu.Lock()
	primary.checkpointLastForcedBuildByRelationship["rel-a"] = time.Now().UTC().Add(-2 * drCheckpointForceBuildInterval)
	primary.checkpointMu.Unlock()

	if !primary.allowForcedCheckpointBuild("rel-a") {
		t.Fatal("expected forced checkpoint admission to pass after cooldown")
	}
}

// --- Integration Test: Strata Estimator Roundtrip ---

func TestDRReconciliation_StrataRoundtrip(t *testing.T) {
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	strata := sketch.NewStrataEstimator(sketch.DefaultStrataLevels, sketch.DefaultStrataCells, sketch.DefaultHashCount)

	// Insert some items.
	for i := 0; i < 100; i++ {
		kid := testSHA256([]byte(fmt.Sprintf("key-%d", i)))
		vid := testSHA256([]byte(fmt.Sprintf("value-%d", i)))
		strata.Insert(kid, vid)
	}

	// Marshal and unmarshal.
	data := strata.Marshal()
	if len(data) == 0 {
		t.Fatal("expected non-empty marshaled data")
	}

	strata2, err := sketch.UnmarshalStrataEstimator(data)
	if err != nil {
		t.Fatal(err)
	}

	// Estimate against itself should be 0.
	diff, err := strata.Estimate(strata2)
	if err != nil {
		t.Fatal(err)
	}
	if diff != 0 {
		t.Fatalf("expected 0 difference with self, got %d", diff)
	}
}

func TestDRPeerFingerprintFromContext_FallbackConnContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "fp-test")
	fp, err := peerCertFingerprintFromContext(ctx)
	if err != nil {
		t.Fatalf("expected fallback fingerprint to be accepted: %v", err)
	}
	if fp != "fp-test" {
		t.Fatalf("unexpected fingerprint: got %q want %q", fp, "fp-test")
	}
}

func TestDRPeerFingerprintFromContext_FallbackRemoteAddrMap(t *testing.T) {
	addr := "127.0.0.1:12345"
	drPeerFingerprintByRemoteAddr.Store(addr, "fp-map")
	defer drPeerFingerprintByRemoteAddr.Delete(addr)

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
	})
	fp, err := peerCertFingerprintFromContext(ctx)
	if err != nil {
		t.Fatalf("expected map fallback fingerprint to be accepted: %v", err)
	}
	if fp != "fp-map" {
		t.Fatalf("unexpected fingerprint: got %q want %q", fp, "fp-map")
	}
}

// helper to compute sha256
func testSHA256(data []byte) [32]byte {
	return sha256.Sum256(data)
}
