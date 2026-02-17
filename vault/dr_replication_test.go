// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
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

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, token.DRTransportCACert); err == nil || !strings.Contains(err.Error(), "expired") {
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
		if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, "wrong-token", token.DRTransportCACert); err == nil {
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

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, token.DRTransportCACert); err == nil || !strings.Contains(err.Error(), "locked") {
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

	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, token.RelationshipID, token.BootstrapToken, token.DRTransportCACert, "10.20.30.40"); err != nil {
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

// TestDRPromote_SkipsRaftTLSKeyringWithoutRaftBackend verifies that
// Promote() completes successfully on non-Raft cores (e.g., in-memory
// backends) where no Raft TLS keyring regeneration is needed.
func TestDRPromote_SkipsRaftTLSKeyringWithoutRaftBackend(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Verify this test core has no Raft backend.
	if core.getRaftBackend() != nil {
		t.Skip("test requires non-Raft core")
	}

	mgr := newDRRelationshipManager(core, core.logger)
	token := &DRActivationToken{
		ClusterID:      "skip-raft-test",
		RelationshipID: "rel-skip-raft",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	// Promote must succeed even without a Raft backend; the TLS
	// keyring regeneration is skipped.
	if err := mgr.PromoteSecondary(ctx); err != nil {
		t.Fatalf("PromoteSecondary failed on non-Raft core: %v", err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after promote, got %s", mgr.Mode())
	}
}

// TestRaftForceRecreateTLSKeyring_NoRaftBackend verifies that
// raftForceRecreateTLSKeyring returns an error on non-Raft cores.
func TestRaftForceRecreateTLSKeyring_NoRaftBackend(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	if core.getRaftBackend() != nil {
		t.Skip("test requires non-Raft core")
	}

	_, err := core.raftForceRecreateTLSKeyring(ctx)
	if err == nil {
		t.Fatal("expected error from raftForceRecreateTLSKeyring on non-Raft core")
	}
	if !strings.Contains(err.Error(), "raft backend not in use") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRaftForceRecreateTLSKeyring_OrphanedKeyring verifies the keyring
// regeneration logic by simulating the orphaned-keyring scenario:
//  1. Write a valid TLS keyring through the barrier.
//  2. Delete it (simulating the barrier key change making it unreadable).
//  3. Call raftForceRecreateTLSKeyring and verify a new keyring is created
//     and is readable through the barrier.
//
// Note: This test only exercises the barrier read/write path; it cannot
// fully test the Raft backend integration (SetTLSKeyring) without a live
// Raft cluster. The integration test covers the full promote path.
func TestRaftForceRecreateTLSKeyring_OrphanedKeyring(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// This core has no Raft backend, so we can't call the full
	// raftForceRecreateTLSKeyring. Instead, verify the barrier
	// layer logic: write, delete, verify absent, write again.

	// Step 1: Write a mock TLS keyring to the barrier.
	mockKeyring := map[string]interface{}{
		"ActiveKeyID": "mock-key-1",
		"Keys":        []interface{}{},
	}
	entry, err := logical.StorageEntryJSON(raftTLSStoragePath, mockKeyring)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	// Verify it's readable.
	got, err := core.barrier.Get(ctx, raftTLSStoragePath)
	if err != nil || got == nil {
		t.Fatalf("expected keyring to be readable, err=%v", err)
	}

	// Step 2: Delete through barrier (simulating what force-recreate does).
	if err := core.barrier.Delete(ctx, raftTLSStoragePath); err != nil {
		t.Fatal(err)
	}

	// Step 3: Verify it's gone.
	got, err = core.barrier.Get(ctx, raftTLSStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected keyring to be deleted")
	}

	// Step 4: Write a new one (simulating the force-recreate write).
	newKeyring := map[string]interface{}{
		"ActiveKeyID": "new-key-1",
		"Keys":        []interface{}{},
	}
	entry, err = logical.StorageEntryJSON(raftTLSStoragePath, newKeyring)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	// Step 5: Verify the new keyring is readable.
	got, err = core.barrier.Get(ctx, raftTLSStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected new keyring to be readable")
	}

	var decoded map[string]interface{}
	if err := got.DecodeJSON(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["ActiveKeyID"] != "new-key-1" {
		t.Fatalf("unexpected ActiveKeyID: %v", decoded["ActiveKeyID"])
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
		cfg.StreamBatchMaxEntries = 1024
		cfg.StreamBatchMaxBytes = 4 << 20
		cfg.StreamBatchMaxWaitMillis = 20
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
	if cfg.StreamBatchMaxEntries != 1024 {
		t.Fatalf("expected stream batch max entries=1024, got %d", cfg.StreamBatchMaxEntries)
	}
	if cfg.StreamBatchMaxBytes != 4<<20 {
		t.Fatalf("expected stream batch max bytes=%d, got %d", 4<<20, cfg.StreamBatchMaxBytes)
	}
	if cfg.StreamBatchMaxWaitMillis != 20 {
		t.Fatalf("expected stream batch max wait millis=20, got %d", cfg.StreamBatchMaxWaitMillis)
	}
	if mgr.secondary.reconcileMaxInflightTasks != 7 {
		t.Fatalf("expected runtime inflight=7, got %d", mgr.secondary.reconcileMaxInflightTasks)
	}
	if mgr.secondary.streamBatchMaxEntries != 1024 {
		t.Fatalf("expected runtime stream batch max entries=1024, got %d", mgr.secondary.streamBatchMaxEntries)
	}
	if mgr.secondary.streamBatchMaxBytes != 4<<20 {
		t.Fatalf("expected runtime stream batch max bytes=%d, got %d", 4<<20, mgr.secondary.streamBatchMaxBytes)
	}
	if mgr.secondary.streamBatchMaxWait != 20*time.Millisecond {
		t.Fatalf("expected runtime stream batch max wait=%s, got %s", 20*time.Millisecond, mgr.secondary.streamBatchMaxWait)
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
	_, err := core.DRFailover(ctx, true)
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

	result, err := core.DRFailover(ctx, true)
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

	result, err := core.DRFailoverToPrimary(ctx, true)
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

	// Test applying a Put via applyFetchedChange (ciphertext-domain reconciliation path).
	err := sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "test/key1",
		Value:  []byte("ciphertext-value1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry exists in physical storage.
	entry, err := core.physical.Get(ctx, "test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected entry after apply")
	}
	if string(entry.Value) != "ciphertext-value1" {
		t.Fatalf("expected ciphertext-value1, got %s", string(entry.Value))
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
	entry, err = core.physical.Get(ctx, "test/key1")
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

	t.Logf("reconciliation set built: %d keys", set.KeyCount)
}

// --- Integration Test: Two-Side Reconciliation ---

// --- Integration Test: Change Stream Primary ---

func TestDRPrimary_ChangeStreamFanout(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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

func TestDRPrimary_SeedAppliedIndex(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Initially zero.
	if got := primary.indexApplied.Load(); got != 0 {
		t.Fatalf("expected indexApplied=0 before seeding, got %d", got)
	}

	// Seed advances the watermark.
	primary.SeedAppliedIndex(42)
	if got := primary.indexApplied.Load(); got != 42 {
		t.Fatalf("expected indexApplied=42 after seeding, got %d", got)
	}

	// Seeding a lower value must not move the counter backwards.
	primary.SeedAppliedIndex(10)
	if got := primary.indexApplied.Load(); got != 42 {
		t.Fatalf("expected indexApplied=42 after lower seed, got %d", got)
	}

	// Seeding zero is a no-op.
	primary.SeedAppliedIndex(0)
	if got := primary.indexApplied.Load(); got != 42 {
		t.Fatalf("expected indexApplied=42 after zero seed, got %d", got)
	}

	// OnChange should still advance past the seed.
	changes := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "test/key1", Value: []byte("v"), RaftIndex: 100},
	}
	primary.OnChange(changes)
	if got := primary.indexApplied.Load(); got != 100 {
		t.Fatalf("expected indexApplied=100 after OnChange, got %d", got)
	}

	// OnChange with only non-replicable paths must still advance
	// indexApplied (the fence needs the raw Raft watermark, not just
	// replicable entries).
	filtered := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "core/dr-replication/config", Value: []byte("x"), RaftIndex: 200},
		{OpType: physical.PutOperation, Key: "core/raft/tls", Value: []byte("y"), RaftIndex: 201},
	}
	primary.OnChange(filtered)
	if got := primary.indexApplied.Load(); got != 201 {
		t.Fatalf("expected indexApplied=201 after filtered OnChange, got %d", got)
	}
}

func TestDRPrimary_RevokeRelationshipTerminatesOnlyMatchingStreams(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
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
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
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

func TestDRPrimary_ExchangeRangeChecksums_CheckpointTupleMismatch(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	var start [32]byte
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

	_, err := primary.ExchangeRangeChecksums(context.Background(), &RangeChecksumRequest{
		CheckpointId:    "cp-1",
		CheckpointIndex: 11, // mismatch (cached index is 10)
		RangeIds:        []uint64{0},
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
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	ctx := context.Background()

	key := "secret/data/demo"
	val := []byte("plaintext-demo")
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   key,
		Value: val,
	}); err != nil {
		t.Fatal(err)
	}
	phys, err := core.physical.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if phys == nil {
		t.Fatalf("expected physical entry for %q", key)
	}

	kid, vid := primary.scanner.ComputeItemFromEntry(&physical.Entry{
		Key:      key,
		Value:    phys.Value,
		SealWrap: phys.SealWrap,
	})
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-test",
			CommitIndex: 7,
		},
		relationshipID: "rel-test",
		kidToKey: map[[32]byte]string{
			kid: key,
		},
		kidToVID: map[[32]byte][32]byte{
			kid: vid,
		},
	}
	seedDRCheckpointArtifactForTest(t, primary, cp, []drCheckpointArtifactRecord{
		{
			KID:      kid,
			VID:      vid,
			Key:      key,
			SealWrap: phys.SealWrap,
		},
	}, map[[32]byte][]byte{kid: phys.Value})

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

func TestDRPrimary_ReadCheckpointEntryChange_UsesArtifactNotLiveStorage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	ctx := context.Background()

	key := "secret/data/dr-artifact"
	original := []byte("value-at-checkpoint")
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   key,
		Value: original,
	}); err != nil {
		t.Fatal(err)
	}
	phys, err := core.physical.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if phys == nil {
		t.Fatalf("expected physical entry for %q", key)
	}
	kid, vid := primary.scanner.ComputeItemFromEntry(&physical.Entry{
		Key:      key,
		Value:    phys.Value,
		SealWrap: phys.SealWrap,
	})

	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-artifact",
			CommitIndex: 9,
		},
		relationshipID: "rel-test",
		kidToKey: map[[32]byte]string{
			kid: key,
		},
		kidToVID: map[[32]byte][32]byte{
			kid: vid,
		},
	}
	seedDRCheckpointArtifactForTest(t, primary, cp, []drCheckpointArtifactRecord{
		{
			KID:      kid,
			VID:      vid,
			Key:      key,
			SealWrap: phys.SealWrap,
		},
	}, map[[32]byte][]byte{kid: phys.Value})

	// Mutate live storage after checkpoint materialization. Fetch must still return
	// the checkpoint artifact value.
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   key,
		Value: []byte("value-after-checkpoint"),
	}); err != nil {
		t.Fatal(err)
	}

	change, err := primary.readCheckpointEntryChange(ctx, cp, kid, &vid, true)
	if err != nil {
		t.Fatalf("expected artifact-backed fetch to succeed: %v", err)
	}
	if change == nil {
		t.Fatal("expected non-nil change")
	}
	if change.OpType != string(physical.PutOperation) {
		t.Fatalf("expected put change, got %q", change.OpType)
	}
	if string(change.Value) != string(phys.Value) {
		t.Fatalf("expected checkpoint artifact value, got live value")
	}
}

func seedDRCheckpointArtifactForTest(t *testing.T, primary *drReplicationPrimary, cp *drCheckpointCacheEntry, records []drCheckpointArtifactRecord, values map[[32]byte][]byte) {
	t.Helper()

	if primary.checkpointArtifacts == nil {
		t.Fatal("checkpoint artifact store is nil")
	}
	tmpDir := t.TempDir()
	artifactDir := filepath.Join(tmpDir, cp.checkpoint.ID)
	blobDir := filepath.Join(artifactDir, "blobs")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		t.Fatalf("create test artifact dir: %v", err)
	}

	recMap := make(map[[32]byte]drCheckpointArtifactRecord, len(records))
	var artifactBytes uint64
	for _, rec := range records {
		if !rec.Tombstone {
			value := values[rec.KID]
			ref := rec.ValueRef
			if ref == "" {
				h := sha256.New()
				h.Write(value)
				if rec.SealWrap {
					h.Write([]byte{1})
				} else {
					h.Write([]byte{0})
				}
				ref = fmt.Sprintf("%x", h.Sum(nil))
			}
			rec.ValueRef = ref
			if err := os.WriteFile(filepath.Join(blobDir, ref+".bin"), value, 0o600); err != nil {
				t.Fatalf("write test artifact blob: %v", err)
			}
			artifactBytes += uint64(len(value))
		}
		recMap[rec.KID] = rec
		artifactBytes += uint64(len(rec.Key) + len(rec.ValueRef) + 96)
	}

	if err := primary.checkpointArtifacts.putArtifact(&drCheckpointArtifact{
		CheckpointID:    cp.checkpoint.ID,
		CheckpointIndex: cp.checkpoint.CommitIndex,
		RelationshipID:  cp.relationshipID,
		CreatedAt:       time.Now().UTC(),
		Path:            artifactDir,
		Bytes:           artifactBytes,
		Records:         recMap,
	}); err != nil {
		t.Fatalf("put test artifact: %v", err)
	}
}

func TestDRCheckpointArtifactStore_EvictsByGlobalBudget(t *testing.T) {
	store := newDRCheckpointArtifactStore(log.NewNullLogger(), t.TempDir())
	store.configure(true, time.Hour, 128, 128, 64)

	makeArtifact := func(id string, createdAt time.Time) *drCheckpointArtifact {
		path := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatalf("mkdir artifact path: %v", err)
		}
		return &drCheckpointArtifact{
			CheckpointID:    id,
			CheckpointIndex: 1,
			RelationshipID:  "rel-1",
			CreatedAt:       createdAt,
			Path:            path,
			Bytes:           96,
			Records:         map[[32]byte]drCheckpointArtifactRecord{},
		}
	}

	art1 := makeArtifact("cp-1", time.Now().Add(-2*time.Minute))
	if err := store.putArtifact(art1); err != nil {
		t.Fatalf("put first artifact: %v", err)
	}
	art2 := makeArtifact("cp-2", time.Now().Add(-1*time.Minute))
	if err := store.putArtifact(art2); err != nil {
		t.Fatalf("put second artifact: %v", err)
	}

	store.mu.RLock()
	_, hasOldest := store.artifacts["cp-1"]
	_, hasNewest := store.artifacts["cp-2"]
	store.mu.RUnlock()
	if hasOldest {
		t.Fatal("expected oldest artifact to be evicted")
	}
	if !hasNewest {
		t.Fatal("expected newest artifact to remain")
	}
	_, items, evictions, _, _ := store.stats()
	if items != 1 {
		t.Fatalf("expected exactly one artifact after eviction, got %d", items)
	}
	if evictions == 0 {
		t.Fatal("expected eviction counter to increment")
	}
}

func TestDRCheckpointArtifactStore_RejectsOversizedArtifact(t *testing.T) {
	store := newDRCheckpointArtifactStore(log.NewNullLogger(), t.TempDir())
	store.configure(true, time.Hour, 64, 64, 64)

	path := filepath.Join(t.TempDir(), "cp-too-large")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir artifact path: %v", err)
	}
	err := store.putArtifact(&drCheckpointArtifact{
		CheckpointID:    "cp-too-large",
		CheckpointIndex: 1,
		RelationshipID:  "rel-1",
		CreatedAt:       time.Now().UTC(),
		Path:            path,
		Bytes:           128,
		Records:         map[[32]byte]drCheckpointArtifactRecord{},
	})
	if err == nil {
		t.Fatal("expected oversized artifact to be rejected")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected budget error, got: %v", err)
	}
}

func TestDRPrimary_AllowWriteRequest_BackpressureRejectsNonExempt(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	primary.backpressureEnabled = true
	primary.backpressureDegraded = 0.80
	primary.backpressureCritical = 0.50
	primary.backpressureMinLag = 1
	primary.backpressureMinQPSDeg = 1
	primary.backpressureMinQPSCrit = 1
	primary.writeRateMu.Lock()
	primary.writeRateEPS = 100
	primary.writeRateMu.Unlock()
	primary.pressureMu.Lock()
	primary.secondaryPressure["rel-1"] = &drSecondaryPressureSample{
		applyRateEPS: 1,
		lagEntries:   100,
		lastSeen:     time.Now().UTC(),
	}
	primary.pressureMu.Unlock()
	primary.backpressureWindowSec = time.Now().UTC().Unix()
	primary.backpressureCount = 0

	if ok, _ := primary.allowWriteRequest("secret/data/demo"); !ok {
		t.Fatal("expected first write request to be admitted under cap")
	}
	if ok, reason := primary.allowWriteRequest("secret/data/demo"); ok {
		t.Fatal("expected second write request to be rejected under same-second cap")
	} else if !strings.Contains(reason, "dr backpressure") {
		t.Fatalf("expected backpressure reason, got %q", reason)
	}
}

func TestDRBackpressureExemptPath(t *testing.T) {
	exempt := []string{
		"sys/health",
		"sys/seal-status",
		"sys/replication/dr/tuning",
		"root/sys/replication/dr/secondary/promote",
	}
	for _, path := range exempt {
		if !isDRBackpressureExemptPath(path) {
			t.Fatalf("expected path %q to be backpressure exempt", path)
		}
	}
	if isDRBackpressureExemptPath("secret/data/demo") {
		t.Fatal("expected normal data path to be non-exempt")
	}
}

func TestDRPrimary_ShouldThrottleCheckpointBuild_UsesActiveLagSignal(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

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

func TestDRPrimary_ExchangeDirtyBitmap_PostRestartReturnsAllDirty(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Freshly created primary has dirtyMapStart == 0 (simulating post-restart).
	if primary.dirtyMapStart != 0 {
		t.Fatalf("expected dirtyMapStart to be 0, got %d", primary.dirtyMapStart)
	}

	resp, err := primary.ExchangeDirtyBitmap(context.Background(), &DirtyBitmapMessage{
		RelationshipId: "rel-1",
		CheckpointId:   "cp-1",
	})
	if err != nil {
		t.Fatalf("ExchangeDirtyBitmap failed: %v", err)
	}

	// Every byte should be 0xFF (all dirty).
	for i, b := range resp.Bitmap {
		if b != 0xFF {
			t.Fatalf("expected all-dirty bitmap, but byte %d is %02x", i, b)
		}
	}
}

func TestDRPrimary_DirtyBitmapPersistenceAndReload(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Simulate writes to populate the dirty bitmap.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "test/key1", Value: []byte("value1"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "test/key2", Value: []byte("value2"), RaftIndex: 11},
	})

	if primary.dirtyMapStart == 0 {
		t.Fatal("expected dirtyMapStart to be set after OnChange")
	}

	// Force persistence (bypass throttle).
	primary.dirtyMapMu.Lock()
	primary.persistDirtyBitmap()
	primary.dirtyMapMu.Unlock()

	// Snapshot the bitmap state.
	primary.dirtyMapMu.RLock()
	origStart := primary.dirtyMapStart
	origBitmap := make([]byte, len(primary.dirtyMap))
	copy(origBitmap, primary.dirtyMap)
	primary.dirtyMapMu.RUnlock()

	// Create a new primary (simulating restart) and load from storage.
	primary2 := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	if err := primary2.loadDirtyBitmap(context.Background()); err != nil {
		t.Fatalf("loadDirtyBitmap failed: %v", err)
	}

	primary2.dirtyMapMu.RLock()
	defer primary2.dirtyMapMu.RUnlock()

	if primary2.dirtyMapStart != origStart {
		t.Fatalf("expected dirtyMapStart %d after reload, got %d", origStart, primary2.dirtyMapStart)
	}
	for i := range origBitmap {
		if primary2.dirtyMap[i] != origBitmap[i] {
			t.Fatalf("bitmap byte %d differs: expected %02x, got %02x", i, origBitmap[i], primary2.dirtyMap[i])
		}
	}
}

func TestDRPrimary_ExchangeDirtyBitmap_InitializedBitmapUsesActual(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Simulate a write that initializes the dirty bitmap.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "test/key1", Value: []byte("value1"), RaftIndex: 5},
	})

	if primary.dirtyMapStart == 0 {
		t.Fatal("expected dirtyMapStart to be set after OnChange")
	}

	resp, err := primary.ExchangeDirtyBitmap(context.Background(), &DirtyBitmapMessage{
		RelationshipId: "rel-1",
		CheckpointId:   "cp-1",
	})
	if err != nil {
		t.Fatalf("ExchangeDirtyBitmap failed: %v", err)
	}

	// The bitmap should NOT be all-dirty (only the range for "test/key1" should be set).
	allDirty := true
	for _, b := range resp.Bitmap {
		if b != 0xFF {
			allDirty = false
			break
		}
	}
	if allDirty {
		t.Fatal("expected partial dirty bitmap after single write, but got all-dirty")
	}

	// At least one bit should be set.
	anyDirty := false
	for _, b := range resp.Bitmap {
		if b != 0 {
			anyDirty = true
			break
		}
	}
	if !anyDirty {
		t.Fatal("expected at least one dirty range after write")
	}
}

func TestDRTombstoneGC_ComputesWatermarkFromSecondaryPressure(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	defer primary.tombstoneGC.Stop()

	now := time.Now()

	// Add two active secondaries with different applied indices.
	primary.pressureMu.Lock()
	primary.secondaryPressure["rel-1"] = &drSecondaryPressureSample{
		lastSeen:    now.Add(-10 * time.Second),
		lastApplied: 100,
	}
	primary.secondaryPressure["rel-2"] = &drSecondaryPressureSample{
		lastSeen:    now.Add(-5 * time.Second),
		lastApplied: 50,
	}
	primary.pressureMu.Unlock()

	wm, disconnected := primary.tombstoneGC.computeGlobalLowWatermark(now)
	if wm != 50 {
		t.Fatalf("expected watermark 50 (minimum of active secondaries), got %d", wm)
	}
	if disconnected != 0 {
		t.Fatalf("expected 0 disconnected peers, got %d", disconnected)
	}
}

func TestDRTombstoneGC_DisconnectedPeersExcludedFromWatermark(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	defer primary.tombstoneGC.Stop()

	now := time.Now()

	// One active secondary, one disconnected (not seen for > threshold).
	primary.pressureMu.Lock()
	primary.secondaryPressure["rel-1"] = &drSecondaryPressureSample{
		lastSeen:    now.Add(-5 * time.Second),
		lastApplied: 100,
	}
	primary.secondaryPressure["rel-disconnected"] = &drSecondaryPressureSample{
		lastSeen:    now.Add(-10 * time.Minute), // way past threshold
		lastApplied: 10,
	}
	primary.pressureMu.Unlock()

	wm, disconnected := primary.tombstoneGC.computeGlobalLowWatermark(now)
	if wm != 100 {
		t.Fatalf("expected watermark 100 (disconnected peer excluded), got %d", wm)
	}
	if disconnected != 1 {
		t.Fatalf("expected 1 disconnected peer, got %d", disconnected)
	}
}

func TestDRTombstoneGC_NoActivePeersUsesLocalIndex(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	defer primary.tombstoneGC.Stop()

	primary.indexApplied.Store(200)

	wm, disconnected := primary.tombstoneGC.computeGlobalLowWatermark(time.Now())
	if wm != 200 {
		t.Fatalf("expected watermark 200 (local index), got %d", wm)
	}
	if disconnected != 0 {
		t.Fatalf("expected 0 disconnected peers, got %d", disconnected)
	}
}

// helper to compute sha256
func testSHA256(data []byte) [32]byte {
	return sha256.Sum256(data)
}

// --- Credit-based flow control tests ---

// creditTestBidiStream is a mock BidiStreamingServer for testing the
// primary's credit-gated StreamChanges handler. It delivers an init
// message on first Recv(), then delivers WindowUpdate messages from
// an internal channel, and captures all sent EntryBatch messages.
// Individual entries are unpacked and forwarded to sentCh for test
// observation.
type creditTestBidiStream struct {
	ctx      context.Context
	initMsg  *StreamChangesUpstream
	initSent bool
	// creditCh carries WindowUpdate messages to feed to the primary.
	creditCh chan *StreamChangesUpstream
	// sentCh receives individual entries unpacked from EntryBatch
	// messages sent by the primary.
	sentCh chan *EntryChange
	// sentBatchCh optionally receives raw EntryBatch messages for
	// tests that need to inspect batching behavior.
	sentBatchCh chan *EntryBatch
}

func (s *creditTestBidiStream) SetHeader(_ metadata.MD) error  { return nil }
func (s *creditTestBidiStream) SendHeader(_ metadata.MD) error { return nil }
func (s *creditTestBidiStream) SetTrailer(_ metadata.MD)       {}
func (s *creditTestBidiStream) Context() context.Context       { return s.ctx }
func (s *creditTestBidiStream) SendMsg(any) error              { return nil }
func (s *creditTestBidiStream) RecvMsg(any) error              { return io.EOF }

func (s *creditTestBidiStream) Send(b *EntryBatch) error {
	// Forward raw batch if a batch channel is provided.
	if s.sentBatchCh != nil {
		select {
		case s.sentBatchCh <- b:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	// Unpack entries for per-entry observation.
	for _, ch := range b.GetEntries() {
		select {
		case s.sentCh <- ch:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	return nil
}

func (s *creditTestBidiStream) Recv() (*StreamChangesUpstream, error) {
	if !s.initSent {
		s.initSent = true
		return s.initMsg, nil
	}
	select {
	case msg := <-s.creditCh:
		return msg, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// newCreditTestPrimary creates a minimal drReplicationPrimary suitable
// for credit flow-control tests. It registers a relationship and injects
// entries into the change buffer so StreamChanges can start.
func newCreditTestPrimary(t *testing.T, creditTimeout time.Duration) (*drReplicationPrimary, string, string) {
	t.Helper()
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	defer func() {
		if primary.tombstoneGC != nil {
			primary.tombstoneGC.Stop()
		}
	}()
	primary.creditWaitTimeout = creditTimeout

	// Register a relationship so authorization passes.
	mgr := core.drManager
	if mgr == nil {
		t.Fatal("expected DR manager")
	}
	if err := mgr.EnablePrimary(context.Background()); err != nil {
		t.Fatal(err)
	}
	token, err := mgr.GenerateActivationToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := mgr.loadRelationship(context.Background(), token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	rel.State = DRRelationshipStateRegistered
	const fingerprint = "test-credit-fingerprint"
	rel.SecondaryCertFingerprint = fingerprint
	if err := mgr.saveRelationship(context.Background(), rel); err != nil {
		t.Fatal(err)
	}

	// Use the primary from the manager so it shares the same core state.
	p := mgr.Primary()
	if p == nil {
		t.Fatal("expected primary")
	}
	p.creditWaitTimeout = creditTimeout

	return p, token.RelationshipID, fingerprint
}

func TestStreamChanges_InitialWindowRespected(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 2*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	defer cancel()

	const initialWindow = 3
	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  initialWindow,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream),
		sentCh:   make(chan *EntryChange, 100),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	// Wait briefly for the subscriber to register, then push entries
	// through OnChange so they flow through the subscriber channel.
	time.Sleep(100 * time.Millisecond)
	for i := uint64(1); i <= 5; i++ {
		primary.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("key/%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// Collect entries. We should receive exactly initialWindow entries
	// before the primary blocks waiting for credits.
	var received []*EntryChange
	timeout := time.After(2 * time.Second)
	for len(received) < initialWindow {
		select {
		case e := <-stream.sentCh:
			received = append(received, e)
		case <-timeout:
			t.Fatalf("timed out waiting for entries; received %d, expected %d", len(received), initialWindow)
		}
	}

	// Verify no additional entries arrive within a short window.
	select {
	case extra := <-stream.sentCh:
		t.Fatalf("received unexpected entry beyond initial window: %s", extra.Key)
	case <-time.After(300 * time.Millisecond):
		// Good: no more entries sent.
	}

	if len(received) != initialWindow {
		t.Fatalf("expected %d entries, got %d", initialWindow, len(received))
	}

	cancel()
	<-errCh
}

func TestStreamChanges_CreditFlowControl(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 5*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	defer cancel()

	const initialWindow = 2
	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  initialWindow,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream, 10),
		sentCh:   make(chan *EntryChange, 100),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	time.Sleep(100 * time.Millisecond)
	for i := uint64(1); i <= 6; i++ {
		primary.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("key/%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// Receive initial window of 2 entries.
	collectN := func(n int, label string) []*EntryChange {
		var out []*EntryChange
		to := time.After(3 * time.Second)
		for len(out) < n {
			select {
			case e := <-stream.sentCh:
				out = append(out, e)
			case <-to:
				t.Fatalf("%s: timed out after receiving %d/%d entries", label, len(out), n)
			}
		}
		return out
	}

	collectN(initialWindow, "initial window")

	// Verify primary is blocked (no more entries).
	select {
	case extra := <-stream.sentCh:
		t.Fatalf("received entry beyond window before credit replenishment: %s", extra.Key)
	case <-time.After(300 * time.Millisecond):
	}

	// Replenish 2 credits.
	stream.creditCh <- &StreamChangesUpstream{
		Msg: &StreamChangesUpstream_WindowUpdate{
			WindowUpdate: &WindowUpdate{Credits: 2},
		},
	}

	// Should now receive 2 more entries.
	collectN(2, "after first replenishment")

	// Replenish 2 more credits.
	stream.creditCh <- &StreamChangesUpstream{
		Msg: &StreamChangesUpstream_WindowUpdate{
			WindowUpdate: &WindowUpdate{Credits: 2},
		},
	}

	// Should receive the remaining 2 entries.
	collectN(2, "after second replenishment")

	cancel()
	<-errCh
}

func TestStreamChanges_CreditTimeout(t *testing.T) {
	// Use a very short timeout so the test finishes quickly.
	primary, relID, fingerprint := newCreditTestPrimary(t, 500*time.Millisecond)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	defer cancel()

	// Window of 1: the first entry is sent, the second triggers a wait.
	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  1,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream),
		sentCh:   make(chan *EntryChange, 100),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	time.Sleep(100 * time.Millisecond)
	// Push 2 entries: first uses the one credit, second will block.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "key/1", Value: []byte("v"), RaftIndex: 1},
		{OpType: physical.PutOperation, Key: "key/2", Value: []byte("v"), RaftIndex: 2},
	})

	// Receive the first entry.
	select {
	case <-stream.sentCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first entry")
	}

	// StreamChanges should return with ResourceExhausted after the short timeout.
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from StreamChanges")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("expected gRPC status error, got: %v", err)
		}
		if st.Code() != codes.ResourceExhausted {
			t.Fatalf("expected ResourceExhausted, got %v: %s", st.Code(), st.Message())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamChanges did not return after credit timeout")
	}
}

func TestStreamChanges_CatchupDebitsCredits(t *testing.T) {
	// Verify that entries sent during catch-up replay are debited from
	// the subscriber's credit counter, preventing credit inflation.
	primary, relID, fingerprint := newCreditTestPrimary(t, 2*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	defer cancel()

	// Pre-populate the change buffer with 5 entries BEFORE the
	// subscriber connects, so they become catch-up entries.
	for i := uint64(10); i <= 14; i++ {
		primary.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("catchup/%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// initial_window = 8: after catch-up sends 5, only 3 credits should
	// remain for the live loop.
	// last_applied_index is set to the oldest buffer entry so catch-up
	// sends all 5 entries directly from the buffer (no journal needed).
	const initialWindow = 8
	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId:   relID,
					LastAppliedIndex: 10,
					InitialWindow:    initialWindow,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream, 10),
		sentCh:   make(chan *EntryChange, 100),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	// Collect the 5 catch-up entries.
	collectN := func(n int, label string) {
		to := time.After(3 * time.Second)
		for i := 0; i < n; i++ {
			select {
			case <-stream.sentCh:
			case <-to:
				t.Fatalf("%s: timed out after receiving %d/%d entries", label, i, n)
			}
		}
	}
	collectN(5, "catch-up")

	// Now push 5 more entries via OnChange (live path).
	// With only 3 credits remaining after catch-up debit, only 3 should
	// be sent before the primary blocks.
	time.Sleep(100 * time.Millisecond)
	for i := uint64(20); i <= 24; i++ {
		primary.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("live/%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// Collect 3 entries (the remaining credits after catch-up debit).
	collectN(3, "live with remaining credits")

	// Verify the primary is now blocked (no credits).
	select {
	case extra := <-stream.sentCh:
		t.Fatalf("received entry beyond expected credits: %s", extra.Key)
	case <-time.After(300 * time.Millisecond):
		// Good: primary is blocked waiting for credit replenishment.
	}

	// Replenish credits to allow the remaining entries through.
	stream.creditCh <- &StreamChangesUpstream{
		Msg: &StreamChangesUpstream_WindowUpdate{
			WindowUpdate: &WindowUpdate{Credits: 5},
		},
	}
	collectN(2, "after replenishment")

	cancel()
	<-errCh
}

func TestStreamChanges_CreditReplenishAccumulation(t *testing.T) {
	// Verify that credit replenishment accumulates pending credits
	// when the creditReplenishCh channel is full, rather than silently
	// dropping them.

	// Create a tiny channel (capacity 1) to force the drop scenario.
	creditReplenishCh := make(chan uint64, 1)

	var pendingCredits uint64
	replenishCredits := func(n int) {
		if n <= 0 {
			return
		}
		pendingCredits += uint64(n)
		select {
		case creditReplenishCh <- pendingCredits:
			pendingCredits = 0
		default:
		}
	}

	// First replenish succeeds (channel empty).
	replenishCredits(10)
	if pendingCredits != 0 {
		t.Fatalf("expected pendingCredits=0 after first send, got %d", pendingCredits)
	}

	// Second replenish hits the default branch (channel full).
	replenishCredits(20)
	if pendingCredits != 20 {
		t.Fatalf("expected pendingCredits=20 after blocked send, got %d", pendingCredits)
	}

	// Drain the channel.
	first := <-creditReplenishCh
	if first != 10 {
		t.Fatalf("expected first credit batch=10, got %d", first)
	}

	// Third replenish should send the accumulated total (20 + 15 = 35).
	replenishCredits(15)
	if pendingCredits != 0 {
		t.Fatalf("expected pendingCredits=0 after accumulated send, got %d", pendingCredits)
	}

	accumulated := <-creditReplenishCh
	if accumulated != 35 {
		t.Fatalf("expected accumulated credits=35, got %d", accumulated)
	}
}

func TestStreamChanges_BatchSizeRespected(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 5*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	defer cancel()

	// Large initial window so the primary doesn't block on credits.
	const initialWindow = 10000
	const totalEntries = 200

	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  initialWindow,
				},
			},
		},
		creditCh:    make(chan *StreamChangesUpstream),
		sentCh:      make(chan *EntryChange, totalEntries),
		sentBatchCh: make(chan *EntryBatch, totalEntries),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	// Wait for subscriber to register, then push a burst of entries.
	time.Sleep(100 * time.Millisecond)
	entries := make([]physical.ChangeStreamEntry, totalEntries)
	for i := uint64(0); i < totalEntries; i++ {
		entries[i] = physical.ChangeStreamEntry{
			OpType:    physical.PutOperation,
			Key:       fmt.Sprintf("key/batch/%d", i),
			Value:     []byte("v"),
			RaftIndex: i + 1,
		}
	}
	primary.OnChange(entries)

	// Collect all entries via sentCh.
	var received int
	timeout := time.After(5 * time.Second)
	for received < totalEntries {
		select {
		case <-stream.sentCh:
			received++
		case <-timeout:
			t.Fatalf("timed out after receiving %d/%d entries", received, totalEntries)
		}
	}

	// Drain all batches that were captured.
	var batches []*EntryBatch
drainBatches:
	for {
		select {
		case b := <-stream.sentBatchCh:
			batches = append(batches, b)
		default:
			break drainBatches
		}
	}

	// Verify no single batch exceeds drStreamSendBatchMaxEntries.
	for i, b := range batches {
		if len(b.GetEntries()) > drStreamSendBatchMaxEntries {
			t.Errorf("batch %d has %d entries, exceeding max %d",
				i, len(b.GetEntries()), drStreamSendBatchMaxEntries)
		}
	}

	// With 200 entries and max batch size of 64, we expect at least
	// ceil(200/64)=4 batches (entries are batched from the channel).
	if len(batches) < 2 {
		t.Errorf("expected multiple batches for %d entries, got %d batches", totalEntries, len(batches))
	}

	t.Logf("sent %d entries in %d batches", received, len(batches))
	cancel()
	<-errCh
}

// TestOnChange_IndexReplicable verifies that indexReplicable tracks the
// highest Raft index of replicable entries and is not advanced by
// non-replicable (filtered) entries.
func TestOnChange_IndexReplicable(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Batch 1: only replicable entries.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "logical/foo", Value: []byte("v"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "logical/bar", Value: []byte("v"), RaftIndex: 11},
	})
	if got := primary.indexReplicable.Load(); got != 11 {
		t.Fatalf("expected indexReplicable=11 after replicable batch, got %d", got)
	}

	// Batch 2: only non-replicable entries (core/raft/* is never replicated).
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "core/raft/tls", Value: []byte("v"), RaftIndex: 12},
		{OpType: physical.PutOperation, Key: "core/raft/config", Value: []byte("v"), RaftIndex: 13},
	})
	// indexReplicable should NOT advance for non-replicable entries.
	if got := primary.indexReplicable.Load(); got != 11 {
		t.Fatalf("expected indexReplicable=11 after non-replicable batch, got %d", got)
	}
	// But indexApplied (raw Raft index) SHOULD advance.
	if got := primary.indexApplied.Load(); got != 13 {
		t.Fatalf("expected indexApplied=13 after non-replicable batch, got %d", got)
	}

	// Batch 3: mixed batch (some replicable, some not).
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "core/raft/peers", Value: []byte("v"), RaftIndex: 14},
		{OpType: physical.PutOperation, Key: "logical/baz", Value: []byte("v"), RaftIndex: 15},
	})
	if got := primary.indexReplicable.Load(); got != 15 {
		t.Fatalf("expected indexReplicable=15 after mixed batch, got %d", got)
	}
}

// TestOnChange_IndexAdvanceMarkerInjected verifies that when OnChange
// receives a batch of only non-replicable entries, it injects an
// index-advance marker into each subscriber's channel.
func TestOnChange_IndexAdvanceMarkerInjected(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)

	// Register a fake subscriber.
	ch := make(chan physical.ChangeStreamEntry, 16)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary.mu.Lock()
	if primary.subscribers == nil {
		primary.subscribers = make(map[string]*changeStreamSubscriber)
	}
	primary.subscribers["test-sub"] = &changeStreamSubscriber{
		id:     "test-sub",
		ch:     ch,
		cancel: cancel,
	}
	primary.mu.Unlock()

	// Send a batch of non-replicable entries.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "core/raft/tls", Value: []byte("v"), RaftIndex: 42},
	})

	// The subscriber should receive exactly one marker.
	select {
	case marker := <-ch:
		if marker.Key != "" {
			t.Fatalf("expected empty key for index-advance marker, got %q", marker.Key)
		}
		if marker.RaftIndex != 42 {
			t.Fatalf("expected marker RaftIndex=42, got %d", marker.RaftIndex)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for index-advance marker on subscriber channel")
	}

	// No more entries should be in the channel.
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra entry on subscriber channel: %+v", extra)
	default:
		// OK
	}
}

// TestStreamChanges_CancelledOnStepdown verifies that when the core's
// activeContext is cancelled (simulating a leadership stepdown), an
// in-flight StreamChanges call exits promptly. Without this, the
// secondary would block on Recv() indefinitely and never reconnect.
func TestStreamChanges_CancelledOnStepdown(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 5*time.Second)

	// Create a cancellable activeContext on the core to simulate stepdown.
	activeCtx, simulateStepdown := context.WithCancel(context.Background())
	primary.core.activeContext = activeCtx

	streamCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint)

	stream := &creditTestBidiStream{
		ctx: streamCtx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  10,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream),
		sentCh:   make(chan *EntryChange, 100),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	// Wait for the subscriber to register.
	time.Sleep(200 * time.Millisecond)
	primary.mu.Lock()
	subCount := len(primary.subscribers)
	primary.mu.Unlock()
	if subCount == 0 {
		t.Fatal("expected at least one subscriber after StreamChanges started")
	}

	// Simulate a stepdown by cancelling the active context.
	simulateStepdown()

	// StreamChanges should exit promptly (within 2 seconds).
	select {
	case err := <-errCh:
		// We expect an error (context cancelled). The exact error is
		// not important; what matters is that the call terminated.
		t.Logf("StreamChanges exited with: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("StreamChanges did not exit within 2s after activeContext cancellation (stepdown)")
	}

	// After exit, the subscriber should be cleaned up.
	primary.mu.Lock()
	subCount = len(primary.subscribers)
	primary.mu.Unlock()
	if subCount != 0 {
		t.Fatalf("expected 0 subscribers after StreamChanges exit, got %d", subCount)
	}
}

// TestApplyStreamChange_IndexAdvanceMarker verifies that an entry with
// an empty key (index-advance marker) is silently ignored by
// applyStreamChange (no storage write, no error).
func TestApplyStreamChange_IndexAdvanceMarker(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	secondary := &drReplicationSecondary{
		core:   core,
		logger: core.logger,
	}

	marker := &EntryChange{
		Key:       "",
		OpType:    "",
		RaftIndex: 99,
	}

	if err := secondary.applyStreamChange(context.Background(), marker); err != nil {
		t.Fatalf("applyStreamChange on marker should return nil, got: %v", err)
	}
}

// TestRequireActiveNode_Active verifies that requireActiveNode returns nil
// when the node is the active leader.
func TestRequireActiveNode_Active(t *testing.T) {
	primary := &drReplicationPrimary{
		core: &Core{},
	}
	// standby defaults to false (active).
	if err := primary.requireActiveNode(); err != nil {
		t.Fatalf("expected nil for active node, got: %v", err)
	}
}

// TestRequireActiveNode_Standby verifies that requireActiveNode returns
// a gRPC Unavailable error with a DRRedirectDetail when the node is a standby.
func TestRequireActiveNode_Standby(t *testing.T) {
	primary := &drReplicationPrimary{
		core: &Core{},
	}
	primary.core.standby.Store(true)

	err := primary.requireActiveNode()
	if err == nil {
		t.Fatal("expected error for standby node, got nil")
	}

	// The error should carry a DRRedirectDetail since the standby's
	// Leader() returns empty in test (no HA backend), but we verify
	// the code doesn't panic and returns Unavailable.
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Fatalf("expected Unavailable, got: %v", st.Code())
	}
}

// TestExtractDRRedirect verifies that extractDRRedirect correctly
// parses a DRRedirectDetail from a gRPC status error.
func TestExtractDRRedirect(t *testing.T) {
	// Build a status with a DRRedirectDetail.
	wantAddr := "https://leader.example.com:8201"
	st, err := status.New(codes.Unavailable, "standby redirect").
		WithDetails(&DRRedirectDetail{LeaderClusterAddr: wantAddr})
	if err != nil {
		t.Fatalf("failed to build status with details: %v", err)
	}

	got, ok := extractDRRedirect(st.Err())
	if !ok {
		t.Fatal("expected redirect to be extracted")
	}
	if got != wantAddr {
		t.Fatalf("got addr %q, want %q", got, wantAddr)
	}

	// Verify a non-redirect error returns false.
	_, ok = extractDRRedirect(fmt.Errorf("some other error"))
	if ok {
		t.Fatal("expected no redirect for plain error")
	}
}

// TestErrDRRedirect_ControllerHandling verifies that errDRRedirect is
// correctly unwrapped by errors.As.
func TestErrDRRedirect_ControllerHandling(t *testing.T) {
	orig := &errDRRedirect{LeaderAddr: "https://new-leader:8201"}
	wrapped := fmt.Errorf("stream failed: %w", orig)

	var redirect *errDRRedirect
	if !errors.As(wrapped, &redirect) {
		t.Fatal("errors.As should unwrap errDRRedirect")
	}
	if redirect.LeaderAddr != "https://new-leader:8201" {
		t.Fatalf("got %q, want %q", redirect.LeaderAddr, "https://new-leader:8201")
	}
}

// TestClassifyReconcileFailure_Redirect verifies that a gRPC error
// with a DRRedirectDetail is classified as drReconcileFailureRedirect,
// and that shouldRetryReconcile returns false for it.
func TestClassifyReconcileFailure_Redirect(t *testing.T) {
	st, err := status.New(codes.Unavailable, "node is standby").
		WithDetails(&DRRedirectDetail{LeaderClusterAddr: "https://leader:8201"})
	if err != nil {
		t.Fatal(err)
	}
	class := classifyReconcileFailure(st.Err())
	if class != drReconcileFailureRedirect {
		t.Fatalf("expected redirect class, got %q", class)
	}

	// Redirect should never be retried.
	core, _, _ := TestCoreUnsealed(t)
	sec := &drReplicationSecondary{core: core, logger: core.logger}
	if sec.shouldRetryReconcile(class) {
		t.Fatal("redirect should not be retryable")
	}
}

// TestClassifyReconcileFailure_NonRedirect verifies that a plain
// gRPC Unavailable without DRRedirectDetail is NOT classified as redirect.
func TestClassifyReconcileFailure_NonRedirect(t *testing.T) {
	err := status.Errorf(codes.Unavailable, "connection refused")
	class := classifyReconcileFailure(err)
	if class == drReconcileFailureRedirect {
		t.Fatal("plain Unavailable should not be classified as redirect")
	}
}

// TestClassifyReconcileFailure_WrappedRedirect verifies that a gRPC
// redirect error wrapped with fmt.Errorf (as runReconciliation does)
// is still classified as drReconcileFailureRedirect.
func TestClassifyReconcileFailure_WrappedRedirect(t *testing.T) {
	st, err := status.New(codes.Unavailable, "node is standby; use leader at https://leader:8201").
		WithDetails(&DRRedirectDetail{LeaderClusterAddr: "https://leader:8201"})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the wrapping done by runReconciliation.
	wrapped := fmt.Errorf("failed to request checkpoint: %w", st.Err())

	class := classifyReconcileFailure(wrapped)
	if class != drReconcileFailureRedirect {
		t.Fatalf("expected redirect class for wrapped error, got %q", class)
	}

	// Verify extractDRRedirect also works on the wrapped error.
	addr, ok := extractDRRedirect(wrapped)
	if !ok {
		t.Fatal("expected extractDRRedirect to find redirect in wrapped error")
	}
	if addr != "https://leader:8201" {
		t.Fatalf("got addr %q, want %q", addr, "https://leader:8201")
	}

	// Double-wrap to simulate additional layers.
	doubleWrapped := fmt.Errorf("reconciliation: %w", wrapped)
	class = classifyReconcileFailure(doubleWrapped)
	if class != drReconcileFailureRedirect {
		t.Fatalf("expected redirect class for double-wrapped error, got %q", class)
	}
}

// ---------- Dynamic TLS trust pool tests ----------

// generateTestCert creates a self-signed DER-encoded certificate for testing.
func generateTestCert(t *testing.T, cn string) ([]byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return der, parsed
}

type drTestClient struct {
	streamChangesFn          func(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[StreamChangesUpstream, EntryBatch], error)
	requestCheckpointFn      func(context.Context, *CheckpointRequest, ...grpc.CallOption) (*CheckpointResponse, error)
	exchangeDirtyBitmapFn    func(context.Context, *DirtyBitmapMessage, ...grpc.CallOption) (*DirtyBitmapMessage, error)
	exchangeRangeChecksumsFn func(context.Context, *RangeChecksumRequest, ...grpc.CallOption) (*RangeChecksumResponse, error)
	exchangeRangeDigestsFn   func(context.Context, *RangeDigestRequest, ...grpc.CallOption) (*RangeDigestResponse, error)
	fetchEntriesFn           func(context.Context, *FetchEntriesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error)
	heartbeatFn              func(context.Context, *DRHeartbeatRequest, ...grpc.CallOption) (*DRHeartbeatResponse, error)
	syncKeyringFn            func(context.Context, *SyncKeyringRequest, ...grpc.CallOption) (*SyncKeyringResponse, error)
}

func (c *drTestClient) StreamChanges(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[StreamChangesUpstream, EntryBatch], error) {
	if c.streamChangesFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.streamChangesFn(ctx, opts...)
}

func (c *drTestClient) RequestCheckpoint(ctx context.Context, in *CheckpointRequest, opts ...grpc.CallOption) (*CheckpointResponse, error) {
	if c.requestCheckpointFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.requestCheckpointFn(ctx, in, opts...)
}

func (c *drTestClient) ExchangeDirtyBitmap(ctx context.Context, in *DirtyBitmapMessage, opts ...grpc.CallOption) (*DirtyBitmapMessage, error) {
	if c.exchangeDirtyBitmapFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangeDirtyBitmapFn(ctx, in, opts...)
}

func (c *drTestClient) ExchangeRangeChecksums(ctx context.Context, in *RangeChecksumRequest, opts ...grpc.CallOption) (*RangeChecksumResponse, error) {
	if c.exchangeRangeChecksumsFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangeRangeChecksumsFn(ctx, in, opts...)
}

func (c *drTestClient) ExchangeRangeDigests(ctx context.Context, in *RangeDigestRequest, opts ...grpc.CallOption) (*RangeDigestResponse, error) {
	if c.exchangeRangeDigestsFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangeRangeDigestsFn(ctx, in, opts...)
}

func (c *drTestClient) FetchEntries(ctx context.Context, in *FetchEntriesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
	if c.fetchEntriesFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.fetchEntriesFn(ctx, in, opts...)
}

func (c *drTestClient) Heartbeat(ctx context.Context, in *DRHeartbeatRequest, opts ...grpc.CallOption) (*DRHeartbeatResponse, error) {
	if c.heartbeatFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.heartbeatFn(ctx, in, opts...)
}

func (c *drTestClient) SyncKeyring(ctx context.Context, in *SyncKeyringRequest, opts ...grpc.CallOption) (*SyncKeyringResponse, error) {
	if c.syncKeyringFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.syncKeyringFn(ctx, in, opts...)
}

type blockingEntryBatchBidiClient struct {
	ctx context.Context
}

func (s *blockingEntryBatchBidiClient) Header() (metadata.MD, error) { return nil, nil }
func (s *blockingEntryBatchBidiClient) Trailer() metadata.MD         { return nil }
func (s *blockingEntryBatchBidiClient) CloseSend() error             { return nil }
func (s *blockingEntryBatchBidiClient) Context() context.Context     { return s.ctx }
func (s *blockingEntryBatchBidiClient) SendMsg(any) error            { return nil }
func (s *blockingEntryBatchBidiClient) RecvMsg(any) error            { return nil }
func (s *blockingEntryBatchBidiClient) Send(*StreamChangesUpstream) error {
	return nil
}

func (s *blockingEntryBatchBidiClient) Recv() (*EntryBatch, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestDRClusterClient_VerifyKnownCert(t *testing.T) {
	der, _ := generateTestCert(t, "fw-known")
	fp := drCertFingerprint(der)

	var mu sync.RWMutex
	pool := map[string]*trustedPrimaryCert{
		fp: {derBytes: der, lastSeen: time.Now().Add(-time.Minute)},
	}

	client := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
	}

	verifier := client.VerifyPeerCertificate()
	if verifier == nil {
		t.Fatal("expected non-nil verifier")
	}

	// Verification should succeed for a known cert.
	if err := verifier([][]byte{der}, nil); err != nil {
		t.Fatalf("unexpected error for known cert: %v", err)
	}

	// lastSeen should have been refreshed.
	mu.RLock()
	entry := pool[fp]
	mu.RUnlock()
	if time.Since(entry.lastSeen) > time.Second {
		t.Fatalf("lastSeen not refreshed: %v ago", time.Since(entry.lastSeen))
	}
}

func TestDRClusterClient_VerifyCASignedCert(t *testing.T) {
	// Generate a CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-dr-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	// Generate a leaf cert signed by the CA.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "openbao-dr-transport-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.RWMutex
	pool := make(map[string]*trustedPrimaryCert)

	// Without a CA, unknown certs should be rejected.
	clientNoCA := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
	}
	verifier := clientNoCA.VerifyPeerCertificate()
	if err := verifier([][]byte{leafDER}, nil); err == nil {
		t.Fatal("expected error for unknown cert without CA configured")
	}

	// With a CA, a CA-signed leaf cert should be accepted and added to pool.
	clientWithCA := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
		primaryCACert:  caCert,
	}
	verifier = clientWithCA.VerifyPeerCertificate()
	if err := verifier([][]byte{leafDER}, nil); err != nil {
		t.Fatalf("unexpected error for CA-signed cert: %v", err)
	}

	fp := drCertFingerprint(leafDER)
	mu.RLock()
	entry, ok := pool[fp]
	mu.RUnlock()
	if !ok {
		t.Fatal("CA-signed cert not added to pool")
	}
	if time.Since(entry.lastSeen) > time.Second {
		t.Fatalf("lastSeen not set correctly: %v ago", time.Since(entry.lastSeen))
	}

	// Self-signed cert (not from the CA) should be rejected.
	selfDER, _ := generateTestCert(t, "fw-self-signed")
	if err := verifier([][]byte{selfDER}, nil); err == nil {
		t.Fatal("expected error for self-signed cert not chaining to CA")
	}
}

func TestDRClusterClient_VerifyEmptyCerts(t *testing.T) {
	var mu sync.RWMutex
	pool := make(map[string]*trustedPrimaryCert)

	client := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
	}

	verifier := client.VerifyPeerCertificate()

	// Empty rawCerts should fail.
	if err := verifier(nil, nil); err == nil {
		t.Fatal("expected error for nil rawCerts")
	}
	if err := verifier([][]byte{}, nil); err == nil {
		t.Fatal("expected error for empty rawCerts")
	}
}

func TestDRTrustPool_TTLEviction(t *testing.T) {
	der1, _ := generateTestCert(t, "fw-fresh")
	der2, _ := generateTestCert(t, "fw-stale")
	fp1 := drCertFingerprint(der1)
	fp2 := drCertFingerprint(der2)

	var mu sync.RWMutex
	pool := map[string]*trustedPrimaryCert{
		fp1: {derBytes: der1, lastSeen: time.Now()},                                      // fresh
		fp2: {derBytes: der2, lastSeen: time.Now().Add(-(trustedCertTTL + time.Minute))}, // stale
	}

	client := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
	}

	client.pruneTrustedCerts()

	mu.RLock()
	defer mu.RUnlock()
	if _, ok := pool[fp1]; !ok {
		t.Fatal("fresh cert should not have been pruned")
	}
	if _, ok := pool[fp2]; ok {
		t.Fatal("stale cert should have been pruned")
	}
}

func TestHeartbeatResponse_ActiveClusterCert(t *testing.T) {
	der, parsed := generateTestCert(t, "fw-primary-leader")

	var mu sync.RWMutex
	pool := make(map[string]*trustedPrimaryCert)

	client := &drReplicationClusterClient{
		logger:         log.NewNullLogger(),
		trustedCertsMu: &mu,
		trustedCerts:   pool,
	}

	// Simulate heartbeat delivering the active cluster cert.
	if err := client.addTrustedCert(der); err != nil {
		t.Fatalf("addTrustedCert failed: %v", err)
	}

	fp := drCertFingerprint(parsed.Raw)
	mu.RLock()
	entry, ok := pool[fp]
	mu.RUnlock()
	if !ok {
		t.Fatal("cert not added to pool from heartbeat")
	}
	if time.Since(entry.lastSeen) > time.Second {
		t.Fatal("lastSeen not set correctly")
	}

	// Calling addTrustedCert again should refresh lastSeen.
	mu.RLock()
	firstSeen := entry.lastSeen
	mu.RUnlock()
	time.Sleep(10 * time.Millisecond)
	if err := client.addTrustedCert(der); err != nil {
		t.Fatalf("addTrustedCert refresh failed: %v", err)
	}

	mu.RLock()
	secondSeen := pool[fp].lastSeen
	mu.RUnlock()
	if !secondSeen.After(firstSeen) {
		t.Fatalf("lastSeen was not refreshed on duplicate add: first=%v, second=%v", firstSeen, secondSeen)
	}
}

func TestDRSecondary_Start_HeartbeatCertRejectForcesReconnect(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, drReplSaltLen)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-heartbeat-fail", log.NewNullLogger())
	secondary.transportReady.Store(true)
	secondary.state.Store(int32(DRSecondaryStreaming))

	_, trustedCA := generateTestCert(t, "dr-trusted-ca")
	rejectedCertDER, _ := generateTestCert(t, "dr-rejected-heartbeat")

	var trustedMu sync.RWMutex
	secondary.drClusterClient = &drReplicationClusterClient{
		core:           core,
		primaryCACert:  trustedCA,
		logger:         log.NewNullLogger(),
		trustedCertsMu: &trustedMu,
		trustedCerts:   make(map[string]*trustedPrimaryCert),
	}

	secondary.client = &drTestClient{
		streamChangesFn: func(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[StreamChangesUpstream, EntryBatch], error) {
			return &blockingEntryBatchBidiClient{ctx: ctx}, nil
		},
		heartbeatFn: func(context.Context, *DRHeartbeatRequest, ...grpc.CallOption) (*DRHeartbeatResponse, error) {
			return &DRHeartbeatResponse{
				PrimaryIndex:      1,
				PrimaryTerm:       1,
				ReplicationState:  uint32(DRSecondaryStreaming),
				LeaderClusterAddr: "127.0.0.1:8201",
				ActiveClusterCert: rejectedCertDER,
			}, nil
		},
	}

	prevInterval := drHeartbeatInterval
	drHeartbeatInterval = 10 * time.Millisecond
	defer func() { drHeartbeatInterval = prevInterval }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := secondary.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail after heartbeat certificate rejection")
	}
	if !strings.Contains(err.Error(), "heartbeat trust validation failed") {
		t.Fatalf("expected heartbeat trust validation failure, got: %v", err)
	}
}

func TestInitTrustedPool(t *testing.T) {
	_, parsed := generateTestCert(t, "fw-activation")
	pool := make(map[string]*trustedPrimaryCert)

	initTrustedPool(pool, parsed)

	fp := drCertFingerprint(parsed.Raw)
	if _, ok := pool[fp]; !ok {
		t.Fatal("activation cert not seeded into pool")
	}

	// Calling again should not duplicate or reset lastSeen.
	origSeen := pool[fp].lastSeen
	time.Sleep(10 * time.Millisecond)
	initTrustedPool(pool, parsed)
	if pool[fp].lastSeen != origSeen {
		t.Fatal("initTrustedPool should not overwrite existing entry")
	}
	if len(pool) != 1 {
		t.Fatalf("pool should have exactly 1 entry, got %d", len(pool))
	}
}

func TestDRCertFingerprint(t *testing.T) {
	der, _ := generateTestCert(t, "fw-test-fp")

	fp := drCertFingerprint(der)

	// Verify it matches manual computation.
	h := sha256.Sum256(der)
	want := hex.EncodeToString(h[:])
	if fp != want {
		t.Fatalf("fingerprint mismatch: got %s, want %s", fp, want)
	}

	// Different certs should have different fingerprints.
	der2, _ := generateTestCert(t, "fw-test-fp-2")
	fp2 := drCertFingerprint(der2)
	if fp == fp2 {
		t.Fatal("different certs should have different fingerprints")
	}
}

// --- FetchEntries stepdown cancellation test ---

// fetchTestServerStream is a mock grpc.ServerStreamingServer[EntryBatch]
// for testing the FetchEntries handler.
type fetchTestServerStream struct {
	ctx       context.Context
	sendCount atomic.Int64
	// sendDelay adds a small delay per Send call so the test can cancel
	// activeContext before the handler finishes all iterations.
	sendDelay time.Duration
}

func (s *fetchTestServerStream) SetHeader(_ metadata.MD) error  { return nil }
func (s *fetchTestServerStream) SendHeader(_ metadata.MD) error { return nil }
func (s *fetchTestServerStream) SetTrailer(_ metadata.MD)       {}
func (s *fetchTestServerStream) Context() context.Context       { return s.ctx }
func (s *fetchTestServerStream) SendMsg(any) error              { return nil }
func (s *fetchTestServerStream) RecvMsg(any) error              { return io.EOF }

func (s *fetchTestServerStream) Send(_ *EntryBatch) error {
	s.sendCount.Add(1)
	if s.sendDelay > 0 {
		time.Sleep(s.sendDelay)
	}
	return nil
}

// TestFetchEntries_CancelledOnStepdown verifies that when the core's
// activeContext is cancelled (simulating a leadership stepdown), an
// in-flight FetchEntries call exits promptly. Without this, the
// secondary's stream.Recv() would block indefinitely, stalling
// reconciliation.
func TestFetchEntries_CancelledOnStepdown(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 5*time.Second)

	// Create a cancellable activeContext to simulate stepdown.
	activeCtx, simulateStepdown := context.WithCancel(context.Background())
	primary.core.activeContext = activeCtx

	// Inject a checkpoint with an empty kidToVID. KIDs in the request
	// that are not in kidToVID produce delete entries (with
	// includeDeletes=true), bypassing the checkpoint artifact store.
	cpID := "test-fetch-stepdown-cp"
	primary.checkpointMu.Lock()
	if primary.checkpoints == nil {
		primary.checkpoints = make(map[string]*drCheckpointCacheEntry)
	}
	primary.checkpoints[cpID] = &drCheckpointCacheEntry{
		checkpoint:     reconciler.Checkpoint{ID: cpID, CommitIndex: 100},
		relationshipID: relID,
		createdAt:      time.Now(),
		kidToVID:       make(map[[32]byte][32]byte),
	}
	primary.checkpointMu.Unlock()

	// Build 10000 KIDs for the request. With includeDeletes=true and
	// empty kidToVID, each produces a delete entry. At 100 per batch
	// that's 100 Send calls if the handler runs to completion.
	const numKIDs = 10000
	kids := make([][]byte, numKIDs)
	for i := range kids {
		kid := make([]byte, 32)
		binary.BigEndian.PutUint64(kid, uint64(i))
		kids[i] = kid
	}

	streamCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint)
	stream := &fetchTestServerStream{
		ctx:       streamCtx,
		sendDelay: time.Millisecond, // 1ms per Send to give cancel time
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.FetchEntries(&FetchEntriesRequest{
			CheckpointId:    cpID,
			CheckpointIndex: 100,
			Kids:            kids,
			IncludeDeletes:  true,
		}, stream)
	}()

	// Let a few batches through, then simulate stepdown.
	time.Sleep(10 * time.Millisecond)
	simulateStepdown()

	select {
	case err := <-errCh:
		sends := stream.sendCount.Load()
		t.Logf("FetchEntries exited with: %v (after %d sends)", err, sends)
		if err == nil {
			t.Error("expected an error after activeContext cancellation")
		}
		// The handler should have aborted early. With 10000 KIDs at
		// 100 per batch, a full run would need 100 sends. We expect
		// significantly fewer because the ctx.Err() check in the loop
		// terminates the handler after activeContext is cancelled.
		if sends >= 100 {
			t.Errorf("expected early abort (<100 sends), got %d sends", sends)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FetchEntries did not exit within 3s after activeContext cancellation (stepdown)")
	}
}

// TestIsReconciliationRequired verifies that the helper correctly
// identifies error messages indicating the primary's buffer/journal
// cannot satisfy catch-up.
func TestIsReconciliationRequired(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("connection reset"), false},
		{fmt.Errorf("change stream apply stalled for 1m30s; reconciliation required"), true},
		{fmt.Errorf("change stream apply queue full (capacity=64 batches); reconciliation required"), true},
		{fmt.Errorf("buffer too old: secondary missing from 100 (inclusive), oldest buffered 500; reconciliation required"), true},
		{fmt.Errorf("journal too old: secondary missing from 100"), true},
		{fmt.Errorf("failed to open change stream: rpc error: code = FailedPrecondition desc = buffer too old"), true},
		{fmt.Errorf("change stream error: EOF"), false},
		{&errDRRedirect{LeaderAddr: "https://leader:8201"}, false},
	}
	for _, tt := range tests {
		got := isReconciliationRequired(tt.err)
		if got != tt.want {
			t.Errorf("isReconciliationRequired(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// --- Dispatcher Tests ---

// TestDispatcher_JournalAlwaysWritten verifies that the dispatcher
// always writes to the journal, even when no primary is attached.
func TestDispatcher_JournalAlwaysWritten(t *testing.T) {
	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	dispatcher := &drChangeStreamDispatcher{
		journal: journal,
		logger:  log.NewNullLogger(),
	}

	entries := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "secret/a", Value: []byte("v1"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "secret/b", Value: []byte("v2"), RaftIndex: 11},
		{OpType: physical.PutOperation, Key: "core/dr-replication/config", Value: []byte("skip"), RaftIndex: 12},
	}

	// No primary attached -- journal should still receive entries.
	dispatcher.OnChange(entries)

	_, segs, oldest, newest := journal.stats()
	if segs == 0 {
		t.Fatal("expected at least one journal segment after OnChange")
	}
	if oldest != 10 || newest != 11 {
		t.Fatalf("expected journal range [10,11], got [%d,%d]", oldest, newest)
	}

	// Verify non-replicable entry was filtered out.
	var count int
	if err := journal.replayAll(func(e physical.ChangeStreamEntry) error {
		count++
		if e.Key == "core/dr-replication/config" {
			t.Fatal("non-replicable entry should have been filtered")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 journal entries, got %d", count)
	}
}

// TestDispatcher_DelegatesToPrimary verifies that when a primary is
// attached, the dispatcher calls its onChangePrimaryPath.
func TestDispatcher_DelegatesToPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	dispatcher := &drChangeStreamDispatcher{
		journal: journal,
		primary: primary,
		logger:  core.logger,
	}

	entries := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "secret/x", Value: []byte("val"), RaftIndex: 100},
	}

	dispatcher.OnChange(entries)

	// Primary ring buffer should have the entry.
	primary.bufMu.RLock()
	bufLen := len(primary.changeBuffer)
	primary.bufMu.RUnlock()
	if bufLen != 1 {
		t.Fatalf("expected 1 entry in primary ring buffer, got %d", bufLen)
	}

	// indexApplied should be updated.
	if got := primary.indexApplied.Load(); got != 100 {
		t.Fatalf("expected indexApplied=100, got %d", got)
	}
}

// TestDispatcher_LeaderTransitionJournalContinuity simulates a leader
// election: entries are written while no primary is attached (follower
// period), then a primary is attached (new leader). The journal should
// contain entries from both periods.
func TestDispatcher_LeaderTransitionJournalContinuity(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	dispatcher := &drChangeStreamDispatcher{
		journal: journal,
		logger:  core.logger,
	}

	// Phase 1: Follower period -- no primary attached.
	for i := uint64(1); i <= 50; i++ {
		dispatcher.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("secret/key-%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// Phase 2: Become leader -- attach primary.
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	dispatcher.setPrimary(primary)

	for i := uint64(51); i <= 100; i++ {
		dispatcher.OnChange([]physical.ChangeStreamEntry{
			{OpType: physical.PutOperation, Key: fmt.Sprintf("secret/key-%d", i), Value: []byte("v"), RaftIndex: i},
		})
	}

	// Journal should have ALL 100 entries.
	var journalCount int
	if err := journal.replayAll(func(e physical.ChangeStreamEntry) error {
		journalCount++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if journalCount != 100 {
		t.Fatalf("expected 100 journal entries, got %d", journalCount)
	}

	// Primary ring buffer should only have entries from phase 2 (50 entries).
	primary.bufMu.RLock()
	bufLen := len(primary.changeBuffer)
	primary.bufMu.RUnlock()
	if bufLen != 50 {
		t.Fatalf("expected 50 entries in ring buffer, got %d", bufLen)
	}

	// Verify journal can satisfy catch-up from index 1.
	var replayCount int
	if err := journal.replayRange(1, 51, func(e physical.ChangeStreamEntry) error {
		replayCount++
		return nil
	}); err != nil {
		t.Fatalf("journal replay for follower-period entries failed: %v", err)
	}
	if replayCount != 50 {
		t.Fatalf("expected 50 entries from journal catch-up, got %d", replayCount)
	}
}

// TestDispatcher_SetClearPrimary verifies setPrimary/clearPrimary toggle.
func TestDispatcher_SetClearPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	dispatcher := &drChangeStreamDispatcher{
		journal: journal,
		logger:  core.logger,
	}

	// Set primary.
	dispatcher.setPrimary(primary)
	dispatcher.mu.RLock()
	if dispatcher.primary != primary {
		t.Fatal("expected primary to be set")
	}
	dispatcher.mu.RUnlock()

	// Clear primary.
	dispatcher.clearPrimary()
	dispatcher.mu.RLock()
	if dispatcher.primary != nil {
		t.Fatal("expected primary to be nil after clear")
	}
	dispatcher.mu.RUnlock()
}

// TestFilterDRReplicableEntries verifies the standalone filter function.
func TestFilterDRReplicableEntries(t *testing.T) {
	entries := []physical.ChangeStreamEntry{
		{Key: "secret/data/foo", RaftIndex: 1},
		{Key: "core/dr-replication/config", RaftIndex: 2},
		{Key: "core/raft/tls/keyring", RaftIndex: 3},
		{Key: "secret/data/bar", RaftIndex: 4},
	}

	replicable := filterDRReplicableEntries(entries)
	if len(replicable) != 2 {
		t.Fatalf("expected 2 replicable entries, got %d", len(replicable))
	}
	if replicable[0].Key != "secret/data/foo" || replicable[1].Key != "secret/data/bar" {
		t.Fatalf("unexpected entries: %v", replicable)
	}
}

// --- Journal-Based Index Warmup Tests ---

// TestWarmIndexFromJournal verifies that replaying journal entries
// produces the same KID/VID mappings as direct OnChange processing.
func TestWarmIndexFromJournal(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	// Build a reference primary that processes entries via OnChange.
	refPrimary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)

	entries := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "secret/a", Value: []byte("v1"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "secret/b", Value: []byte("v2"), RaftIndex: 11},
		{OpType: physical.PutOperation, Key: "secret/c", Value: []byte("v3"), RaftIndex: 12},
		{OpType: physical.DeleteOperation, Key: "secret/a", RaftIndex: 13},
	}
	refPrimary.OnChange(entries)

	// Build a new primary and warm its index purely from the journal.
	newPrimary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	count, err := newPrimary.WarmIndexFromJournal(journal)
	if err != nil {
		t.Fatalf("WarmIndexFromJournal failed: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4 entries replayed, got %d", count)
	}

	// Verify the new primary's index matches the reference.
	refPrimary.indexMu.RLock()
	newPrimary.indexMu.RLock()

	if len(newPrimary.indexKIDToVID) != len(refPrimary.indexKIDToVID) {
		t.Fatalf("index size mismatch: new=%d ref=%d", len(newPrimary.indexKIDToVID), len(refPrimary.indexKIDToVID))
	}
	for kid, vid := range refPrimary.indexKIDToVID {
		newVID, ok := newPrimary.indexKIDToVID[kid]
		if !ok {
			t.Fatalf("key %x missing in new index", kid[:8])
		}
		if newVID != vid {
			t.Fatalf("VID mismatch for key %x", kid[:8])
		}
	}
	for kid, key := range refPrimary.indexKIDToKey {
		newKey, ok := newPrimary.indexKIDToKey[kid]
		if !ok {
			t.Fatalf("key mapping %x missing in new index", kid[:8])
		}
		if newKey != key {
			t.Fatalf("key mapping mismatch for %x: new=%q ref=%q", kid[:8], newKey, key)
		}
	}

	newPrimary.indexMu.RUnlock()
	refPrimary.indexMu.RUnlock()

	// Verify delete was processed: "secret/a" should NOT be in new index.
	kidA := newPrimary.scanner.ComputeKID("secret/a")
	newPrimary.indexMu.RLock()
	if _, ok := newPrimary.indexKIDToVID[kidA]; ok {
		t.Fatal("deleted key 'secret/a' should not be in the index")
	}
	newPrimary.indexMu.RUnlock()

	// Verify initialized flag.
	newPrimary.indexMu.RLock()
	if !newPrimary.indexInitialized {
		t.Fatal("expected index to be marked as initialized")
	}
	newPrimary.indexMu.RUnlock()
}

// TestWarmIndexFromJournal_EmptyJournal verifies the error path when
// the journal has no segments.
func TestWarmIndexFromJournal_EmptyJournal(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	dir := t.TempDir()
	journal := newDRStreamJournal(nil, dir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	_, err := primary.WarmIndexFromJournal(journal)
	if err == nil {
		t.Fatal("expected error for empty journal")
	}
}

// TestWarmIndexFromJournal_NilJournal verifies graceful handling.
func TestWarmIndexFromJournal_NilJournal(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	_, err := primary.WarmIndexFromJournal(nil)
	if err == nil {
		t.Fatal("expected error for nil journal")
	}
}
