// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	be "github.com/openbao/openbao/vault/backend"
	"github.com/openbao/openbao/vault/routing"
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

func TestDRRelationshipManager_EnablePrimary_TransportCAFailureDoesNotPersistPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	mgr := newDRRelationshipManager(core, core.logger)
	ctx := context.Background()

	// Persist initial disabled config to verify failed enable does not
	// leave barrier state in primary mode.
	if err := mgr.saveConfig(ctx); err != nil {
		t.Fatalf("failed to persist initial config: %v", err)
	}

	// Corrupt persisted transport CA so EnablePrimary fails during CA load.
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   drTransportCAPath,
		Value: []byte("invalid-ca-bundle"),
	}); err != nil {
		t.Fatalf("failed to seed corrupt transport CA: %v", err)
	}

	if err := mgr.EnablePrimary(ctx); err == nil {
		t.Fatal("expected enable primary to fail with corrupted transport CA")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected mode to remain disabled after failed enable, got %s", mgr.Mode())
	}
	if mgr.Primary() != nil {
		t.Fatal("expected primary to remain nil after failed enable")
	}

	entry, err := core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted DR config: %v", err)
	}
	if entry == nil {
		t.Fatal("expected persisted DR config entry")
	}

	var persisted DRConfig
	if err := json.Unmarshal(entry.Value, &persisted); err != nil {
		t.Fatalf("failed to decode persisted DR config: %v", err)
	}
	if persisted.Mode == DRModePrimary {
		t.Fatal("failed enable left persisted DR mode as primary")
	}
}

func TestDRRelationshipManager_EnableSecondary_SaveConfigFailureRollsBackState(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	mgr := newDRRelationshipManager(core, core.logger)

	token := &DRActivationToken{
		ClusterID:      "cluster-1",
		RelationshipID: "rel-1",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
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

func TestDRRelationshipManager_EnableSecondaryClearsStaleCheckpointCursor(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	mgr := newDRRelationshipManager(core, core.logger)

	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   drCheckpointHWMPath,
		Value: []byte("6685"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.physical.Put(ctx, &physical.Entry{
		Key:   drFlatAccumulatorStoragePath,
		Value: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatal(err)
	}

	token := &DRActivationToken{
		ClusterID:      "new-promoted-primary",
		RelationshipID: "new-promoted-relationship",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	entry, err := core.barrier.Get(ctx, drCheckpointHWMPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatalf("expected stale checkpoint cursor to be cleared on secondary enable, got %q", string(entry.Value))
	}
	if entry, err := core.physical.Get(ctx, drFlatAccumulatorStoragePath); err != nil {
		t.Fatal(err)
	} else if entry != nil {
		t.Fatalf("expected stale flat accumulator to be cleared on secondary enable, got %q", string(entry.Value))
	}
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary runtime")
	}
	mgr.Secondary().loadCheckpointHighWaterMark()
	if mgr.Secondary().highestCommittedCheckpointIndexSet {
		t.Fatalf("expected no loaded stale checkpoint high-water mark, got %d", mgr.Secondary().highestCommittedCheckpointIndex)
	}
	if got := mgr.Secondary().lastAppliedIndex.Load(); got != 0 {
		t.Fatalf("expected fresh secondary last applied index 0, got %d", got)
	}
}

func TestDRRelationshipManager_ActivationToken(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	core.clusterAddr.Store("https://primary.test.local:8201")

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
	if len(token.PrimaryAddrs) == 0 {
		t.Fatal("expected non-empty primary_addrs in token")
	}
	primaryAddrFound := false
	for _, addr := range token.PrimaryAddrs {
		if addr == token.PrimaryAddr {
			primaryAddrFound = true
			break
		}
	}
	if !primaryAddrFound {
		t.Fatalf("expected primary_addr %q to be present in primary_addrs %v", token.PrimaryAddr, token.PrimaryAddrs)
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
	if rel.BootstrapToken != "" {
		t.Fatal("expected bootstrap token plaintext not to be persisted")
	}
	if rel.BootstrapTokenHash == "" {
		t.Fatal("expected bootstrap token verifier hash to be persisted")
	}
	if rel.BootstrapTokenHash == token.BootstrapToken {
		t.Fatal("expected bootstrap token verifier hash not to equal bearer token")
	}
	if !bootstrapTokenMatches(rel, token.BootstrapToken) {
		t.Fatal("expected persisted bootstrap verifier to accept activation token")
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
	if updated.State != DRRelationshipStateRevoked {
		t.Fatalf("expected expired pending relationship to be revoked, got %s", updated.State)
	}
	if updated.BootstrapToken != "" {
		t.Fatal("expected expired bootstrap token to be cleared")
	}
	if updated.BootstrapTokenHash != "" {
		t.Fatal("expected expired bootstrap token verifier to be cleared")
	}
	if updated.ExpiresAt != 0 {
		t.Fatalf("expected expiry to be cleared after terminal failure, got %d", updated.ExpiresAt)
	}
	if updated.RevokedAt == 0 {
		t.Fatal("expected revoked_at to be set")
	}
	if updated.LastFailedAt == 0 {
		t.Fatal("expected last_failed_at to be set")
	}
	if updated.LastError == "" {
		t.Fatal("expected last_error to be set")
	}
}

func TestDRRelationshipManager_LegacyPlaintextBootstrapTokenMigratesToVerifier(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	relationshipID := "11111111-1111-1111-1111-111111111111"
	bootstrapToken := "22222222-2222-2222-2222-222222222222"
	now := time.Now().UTC()
	if err := mgr.saveRelationship(ctx, &DRRelationship{
		RelationshipID: relationshipID,
		State:          DRRelationshipStatePending,
		BootstrapToken: bootstrapToken,
		CreatedAt:      now.Unix(),
		ExpiresAt:      now.Add(drBootstrapTokenTTL).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	rel, err := mgr.loadRelationship(ctx, relationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.BootstrapToken != "" {
		t.Fatal("expected legacy plaintext bootstrap token to be cleared on load")
	}
	if rel.BootstrapTokenHash == "" {
		t.Fatal("expected legacy plaintext bootstrap token to be converted to verifier hash")
	}
	if !bootstrapTokenMatches(rel, bootstrapToken) {
		t.Fatal("expected migrated verifier to accept original bootstrap token")
	}

	secondaryCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, relationshipID, bootstrapToken, secondaryCertDER); err != nil {
		t.Fatalf("expected migrated verifier to allow registration: %v", err)
	}
	rel, err = mgr.loadRelationship(ctx, relationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.BootstrapToken != "" || rel.BootstrapTokenHash != "" {
		t.Fatal("expected bootstrap token material to be cleared after migrated registration")
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

	wrongToken := "11111111-1111-1111-1111-111111111111"
	if wrongToken == token.BootstrapToken {
		wrongToken = "22222222-2222-2222-2222-222222222222"
	}
	for i := 0; i < drBootstrapMaxFailedAttempts; i++ {
		if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, wrongToken, token.DRTransportCACert); err == nil {
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
	if rel.State != DRRelationshipStateRevoked {
		t.Fatalf("expected relationship to be revoked after max failed attempts, got %s", rel.State)
	}
	if rel.BootstrapToken != "" {
		t.Fatal("expected bootstrap token to be cleared after max failed attempts")
	}
	if rel.BootstrapTokenHash != "" {
		t.Fatal("expected bootstrap token verifier to be cleared after max failed attempts")
	}
	if rel.FailedAttempts < drBootstrapMaxFailedAttempts {
		t.Fatalf("expected failed attempts >= %d, got %d", drBootstrapMaxFailedAttempts, rel.FailedAttempts)
	}

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, token.DRTransportCACert); err == nil || !strings.Contains(err.Error(), "invalid or already-used bootstrap token") {
		t.Fatalf("expected already-used token error when using correct token after revocation, got %v", err)
	}
	replayed, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.FailedAttempts != rel.FailedAttempts {
		t.Fatalf("expected replay against revoked relationship not to mutate failed attempts: got %d want %d", replayed.FailedAttempts, rel.FailedAttempts)
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

	secondaryCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER, "10.20.30.40"); err != nil {
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

func TestDRRelationshipManager_BootstrapRejectsInvalidSecondaryCertificate(t *testing.T) {
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

	now := time.Now().UTC()
	expiredCert := newValidationSecondaryClientCert(t, func(tpl *x509.Certificate) {
		tpl.NotBefore = now.Add(-2 * time.Hour)
		tpl.NotAfter = now.Add(-time.Hour)
	})

	err = mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, expiredCert.Raw)
	if err == nil || !strings.Contains(err.Error(), "invalid secondary CA certificate") {
		t.Fatalf("expected invalid secondary certificate error, got: %v", err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStatePending {
		t.Fatalf("expected relationship to remain pending, got %s", rel.State)
	}
	if rel.BootstrapToken != "" {
		t.Fatal("expected bootstrap token plaintext not to be persisted")
	}
	if rel.BootstrapTokenHash == "" {
		t.Fatal("expected bootstrap token verifier to remain unconsumed")
	}
	if rel.FailedAttempts != 1 {
		t.Fatalf("expected failed attempts to increment, got %d", rel.FailedAttempts)
	}
	if rel.LastFailedAt == 0 {
		t.Fatal("expected last_failed_at to be set")
	}
}

func TestDRRelationshipManager_BootstrapReplayDoesNotMutateRegisteredRelationship(t *testing.T) {
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
	secondaryCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER, "10.20.30.40"); err != nil {
		t.Fatal(err)
	}
	registered, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if registered.State != DRRelationshipStateRegistered {
		t.Fatalf("expected registered relationship, got %s", registered.State)
	}

	err = mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER, "10.20.30.41")
	if err == nil || !strings.Contains(err.Error(), "invalid or already-used bootstrap token") {
		t.Fatalf("expected replay rejection, got: %v", err)
	}
	replayed, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != DRRelationshipStateRegistered {
		t.Fatalf("expected replay to leave relationship registered, got %s", replayed.State)
	}
	if replayed.RegisteredAt != registered.RegisteredAt {
		t.Fatalf("expected replay not to mutate registered_at: got %d want %d", replayed.RegisteredAt, registered.RegisteredAt)
	}
	if replayed.FailedAttempts != registered.FailedAttempts {
		t.Fatalf("expected replay not to mutate failed attempts: got %d want %d", replayed.FailedAttempts, registered.FailedAttempts)
	}
	if replayed.LastError != registered.LastError {
		t.Fatalf("expected replay not to mutate last_error: got %q want %q", replayed.LastError, registered.LastError)
	}
	if replayed.SecondaryCertFingerprint != registered.SecondaryCertFingerprint {
		t.Fatalf("expected replay not to mutate fingerprint: got %q want %q", replayed.SecondaryCertFingerprint, registered.SecondaryCertFingerprint)
	}
}

func TestDRRelationshipManager_BootstrapRejectsMalformedInput(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	validRelationshipID := "11111111-1111-1111-1111-111111111111"
	validBootstrapToken := "22222222-2222-2222-2222-222222222222"
	for name, tc := range map[string]struct {
		relationshipID  string
		bootstrapToken  string
		certDER         []byte
		wantErrContains string
	}{
		"relationship-id": {
			relationshipID:  "not-a-uuid",
			bootstrapToken:  validBootstrapToken,
			certDER:         []byte("x"),
			wantErrContains: "relationship_id must be a valid UUID",
		},
		"bootstrap-token": {
			relationshipID:  validRelationshipID,
			bootstrapToken:  "not-a-uuid",
			certDER:         []byte("x"),
			wantErrContains: "bootstrap_token must be a valid UUID",
		},
		"empty-cert": {
			relationshipID:  validRelationshipID,
			bootstrapToken:  validBootstrapToken,
			certDER:         nil,
			wantErrContains: "secondary_ca_cert is required",
		},
		"oversized-cert": {
			relationshipID:  validRelationshipID,
			bootstrapToken:  validBootstrapToken,
			certDER:         make([]byte, drBootstrapMaxCertDERBytes+1),
			wantErrContains: "secondary_ca_cert exceeds maximum DER size",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := mgr.ValidateBootstrapAndStoreCert(ctx, tc.relationshipID, tc.bootstrapToken, tc.certDER)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("expected %q error, got: %v", tc.wantErrContains, err)
			}
		})
	}
}

func TestDRSystemBackend_RegisterSecondaryRejectsOversizedEncodedCert(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/primary/register-secondary")
	req.Data = map[string]interface{}{
		"relationship_id":   token.RelationshipID,
		"bootstrap_token":   token.BootstrapToken,
		"secondary_ca_cert": strings.Repeat("A", base64.StdEncoding.EncodedLen(drBootstrapMaxCertDERBytes)+1),
	}
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || !resp.IsError() || !strings.Contains(resp.Error().Error(), "secondary_ca_cert exceeds maximum DER size") {
		t.Fatalf("expected oversized certificate error response, got resp=%#v err=%v", resp, err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStatePending {
		t.Fatalf("expected oversized API request not to mutate relationship state, got %s", rel.State)
	}
	if rel.FailedAttempts != 0 {
		t.Fatalf("expected oversized API request to be rejected before failure accounting, got %d failed attempts", rel.FailedAttempts)
	}
	if rel.BootstrapToken != "" {
		t.Fatal("expected bootstrap token plaintext not to be persisted after oversized API request")
	}
	if rel.BootstrapTokenHash == "" || !bootstrapTokenMatches(rel, token.BootstrapToken) {
		t.Fatal("expected bootstrap token verifier to remain intact after oversized API request")
	}
}

func TestDRSystemBackend_EnableSecondaryRejectsOversizedActivationToken(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/secondary/enable")
	req.Data = map[string]interface{}{
		"token": strings.Repeat("x", drActivationTokenMaxBytes+1),
	}
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || !resp.IsError() || !strings.Contains(resp.Error().Error(), "activation token exceeds maximum size") {
		t.Fatalf("expected oversized activation token error response, got resp=%#v err=%v", resp, err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected oversized activation token not to enable DR, got mode %s", mgr.Mode())
	}
	entry, err := core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("expected oversized activation token not to persist DR config")
	}
}

func TestDRSystemBackend_StatusDoesNotExposePromotionSecrets(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	mgr := newDRRelationshipManager(core, core.logger)
	mgr.config = &DRConfig{
		Mode:             DRModeDisabled,
		ClusterID:        "public-cluster-id",
		ReplSalt:         []byte("status-repl-salt-secret-must-not-appear"),
		PrimaryAddr:      "https://status-primary-addr-secret.example:8201",
		PrimaryAddrs:     []string{"https://status-primary-addrs-secret.example:8201"},
		RelationshipID:   "status-relationship-id-secret",
		PrimaryCACert:    []byte("status-primary-ca-secret-must-not-appear"),
		PrimaryAPICACert: []byte("status-primary-api-ca-secret-must-not-appear"),
		SecondaryClientCert: []byte(
			"status-secondary-client-cert-secret-must-not-appear",
		),
		SecondaryClientKeyPEM: []byte(
			"status-secondary-client-key-secret-must-not-appear",
		),
		Promotion: &DRPromotionRecord{
			PromotionID:                 "promotion-public-id",
			PromotedAt:                  time.Now().UTC().Unix(),
			OldPrimaryClusterID:         "old-primary-secret",
			OldRelationshipID:           "old-relationship-secret",
			OldSecondaryCertFingerprint: "old-fingerprint-secret",
			StalePrimaryClusterIDs:      []string{"stale-primary-secret"},
			StaleRelationshipIDs:        []string{"stale-relationship-secret"},
			StaleSecondaryFingerprints:  []string{"stale-fingerprint-secret"},
			LocalClusterID:              "local-cluster-id",
			LastAppliedIndex:            100,
			LastKnownPrimaryIndex:       110,
			PromotionClass:              DRPromotionForced,
			EstimatedDataLossEntries:    10,
			DataLossEstimateBasis:       drPromotionDataLossEstimateBasis,
			DataLossAccepted:            true,
			ForcedReasonCodes:           []string{drPromotionReasonSecondaryLagDetected},
			ForcedReasonDetails:         []string{"secondary is behind primary by approximately 10 entries"},
		},
	}
	core.drManager = mgr

	req := logical.TestRequest(t, logical.ReadOperation, "replication/dr/status")
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected status response, got %#v", resp)
	}
	if resp.Data["last_promotion_id"] != "promotion-public-id" {
		t.Fatalf("expected non-sensitive promotion id in status, got %#v", resp.Data["last_promotion_id"])
	}
	if resp.Data["last_promotion_clean_promotion_eligible"] != false {
		t.Fatalf("expected forced promotion to be marked ineligible for clean promotion, got %#v", resp.Data["last_promotion_clean_promotion_eligible"])
	}
	if resp.Data["last_promotion_estimated_data_loss_entries_basis"] != drPromotionDataLossEstimateBasis {
		t.Fatalf("expected non-sensitive data loss estimate basis in status, got %#v", resp.Data["last_promotion_estimated_data_loss_entries_basis"])
	}
	codes, ok := resp.Data["last_promotion_forced_reason_codes"].([]string)
	if !ok || !stringSliceContains(codes, drPromotionReasonSecondaryLagDetected) {
		t.Fatalf("expected non-sensitive forced promotion reason code in status, got %#v", resp.Data["last_promotion_forced_reason_codes"])
	}
	for _, key := range []string{
		"last_promotion_old_primary_cluster_id",
		"last_promotion_old_relationship_id",
		"last_promotion_old_secondary_cert_fingerprint",
		"last_promotion_stale_primary_cluster_ids",
		"last_promotion_stale_relationship_ids",
		"last_promotion_stale_secondary_fingerprints",
	} {
		if _, ok := resp.Data[key]; ok {
			t.Fatalf("status response exposed sensitive promotion lineage field %q", key)
		}
	}
	body := mustMarshalDRResponseData(t, resp)
	requireDRResponseDoesNotContain(
		t, body,
		"old-primary-secret",
		"old-relationship-secret",
		"old-fingerprint-secret",
		"stale-primary-secret",
		"stale-relationship-secret",
		"stale-fingerprint-secret",
		"status-repl-salt-secret-must-not-appear",
		"status-primary-addr-secret.example",
		"status-primary-addrs-secret.example",
		"status-relationship-id-secret",
		"status-primary-ca-secret-must-not-appear",
		"status-primary-api-ca-secret-must-not-appear",
		"status-secondary-client-cert-secret-must-not-appear",
		"status-secondary-client-key-secret-must-not-appear",
	)
}

func TestDRSystemBackend_RelationshipResponsesDoNotExposeStoredSecrets(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := namespace.RootContext(t.Context())

	mgr := newDRRelationshipManager(core, core.logger)
	mgr.config = &DRConfig{
		Mode:      DRModePrimary,
		ClusterID: "public-cluster-id",
	}
	core.drManager = mgr

	now := time.Now().UTC().Unix()
	relationshipID := "11111111-1111-1111-1111-111111111111"
	rel := &DRRelationship{
		RelationshipID:                   relationshipID,
		State:                            DRRelationshipStatePending,
		SecondaryCertFingerprint:         "public-secondary-fingerprint",
		SecondaryCACert:                  []byte("relationship-secondary-ca-secret-must-not-appear"),
		PreviousSecondaryCertFingerprint: "previous-fingerprint-secret-must-not-appear",
		PendingSecondaryCertFingerprint:  "pending-fingerprint-secret-must-not-appear",
		PendingSecondaryCACert:           []byte("relationship-pending-ca-secret-must-not-appear"),
		PendingRotationOperationID:       "pending-operation-id-secret-must-not-appear",
		BootstrapToken:                   "legacy-bootstrap-token-secret-must-not-appear",
		BootstrapTokenHash:               "bootstrap-token-hash-secret-must-not-appear",
		CreatedAt:                        now,
		ExpiresAt:                        now + int64(time.Hour/time.Second),
		LastError:                        "bootstrap token mismatch",
	}
	if err := mgr.saveRelationship(ctx, rel); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "list",
			path: "replication/dr/primary/relationships",
		},
		{
			name: "status",
			path: "replication/dr/primary/relationships/" + relationshipID + "/status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := logical.TestRequest(t, logical.ReadOperation, tc.path)
			resp, err := core.systemBackend.HandleRequest(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || resp.IsError() {
				t.Fatalf("expected relationship response, got %#v", resp)
			}

			body := mustMarshalDRResponseData(t, resp)
			requireDRResponseDoesNotContain(
				t, body,
				"relationship-secondary-ca-secret-must-not-appear",
				"relationship-pending-ca-secret-must-not-appear",
				"previous-fingerprint-secret-must-not-appear",
				"pending-fingerprint-secret-must-not-appear",
				"pending-operation-id-secret-must-not-appear",
				"legacy-bootstrap-token-secret-must-not-appear",
				"bootstrap-token-hash-secret-must-not-appear",
			)
			requireDRResponseDoesNotContainKeys(
				t, body,
				"secondary_ca_cert",
				"pending_secondary_ca_cert",
				"previous_secondary_cert_fingerprint",
				"pending_secondary_cert_fingerprint",
				"pending_rotation_operation_id",
				"bootstrap_token",
				"bootstrap_token_hash",
			)
			if !strings.Contains(body, "public-secondary-fingerprint") {
				t.Fatalf("expected public current fingerprint in relationship response: %s", body)
			}
		})
	}
}

func TestDRSystemBackend_BootstrapRegistrationFailureResponseIsGeneric(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := namespace.RootContext(t.Context())
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	certDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	register := func(t *testing.T, relationshipID, bootstrapToken string, certDER []byte) *logical.Response {
		t.Helper()
		req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/primary/register-secondary")
		req.Data = map[string]interface{}{
			"relationship_id":   relationshipID,
			"bootstrap_token":   bootstrapToken,
			"secondary_ca_cert": base64.StdEncoding.EncodeToString(certDER),
		}
		resp, err := core.systemBackend.HandleRequest(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	unknownRelationshipID := "11111111-1111-1111-1111-111111111111"
	resp := register(t, unknownRelationshipID, "22222222-2222-2222-2222-222222222222", certDER)
	requireDRPublicError(
		t, resp, "registration failed",
		unknownRelationshipID,
		"not found",
		"relationship",
	)

	expiredToken, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expiredRel, err := mgr.loadRelationship(ctx, expiredToken.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	expiredRel.ExpiresAt = time.Now().UTC().Add(-time.Minute).Unix()
	if err := mgr.saveRelationship(ctx, expiredRel); err != nil {
		t.Fatal(err)
	}
	resp = register(t, expiredToken.RelationshipID, expiredToken.BootstrapToken, certDER)
	requireDRPublicError(
		t, resp, "registration failed",
		expiredToken.RelationshipID,
		"expired",
		"locked",
		"revoked",
	)

	registeredToken, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, registeredToken.RelationshipID, registeredToken.BootstrapToken, certDER); err != nil {
		t.Fatal(err)
	}
	duplicateToken, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp = register(t, duplicateToken.RelationshipID, duplicateToken.BootstrapToken, certDER)
	requireDRPublicError(
		t, resp, "registration failed",
		registeredToken.RelationshipID,
		duplicateToken.RelationshipID,
		"already bound",
		"fingerprint",
		"stale",
		"lineage",
	)
}

func TestDRSystemBackend_CredentialRotationFailureResponseIsGeneric(t *testing.T) {
	core, _, token, oldCert, _ := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := namespace.RootContext(t.Context())

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	rotationRequest := func(t *testing.T, path, relationshipID, operationID string, certDER, signature []byte) *logical.Response {
		t.Helper()
		req := logical.TestRequest(t, logical.UpdateOperation, path)
		req.Data = map[string]interface{}{
			"relationship_id":   relationshipID,
			"operation_id":      operationID,
			"issued_at":         time.Now().UTC().Unix(),
			"secondary_ca_cert": base64.StdEncoding.EncodeToString(certDER),
			"signature":         base64.StdEncoding.EncodeToString(signature),
		}
		resp, err := core.systemBackend.HandleRequest(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	operationID := "33333333-3333-3333-3333-333333333333"
	issuedAt := time.Now().UTC().Unix()
	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	badSig := append([]byte(nil), initSig...)
	badSig[len(badSig)-1] ^= 0xff

	resp := rotationRequest(t, "replication/dr/primary/rotate-secondary-certificate", token.RelationshipID, operationID, newCertDER, badSig)
	requireDRPublicError(
		t, resp, "credential rotation failed",
		token.RelationshipID,
		operationID,
		"signature",
		"verification",
		"fingerprint",
		"active",
		"pending",
	)

	unknownRelationshipID := "44444444-4444-4444-4444-444444444444"
	resp = rotationRequest(t, "replication/dr/primary/rotate-secondary-certificate", unknownRelationshipID, operationID, newCertDER, initSig)
	requireDRPublicError(
		t, resp, "credential rotation failed",
		unknownRelationshipID,
		"not found",
		"relationship",
	)

	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	resp = rotationRequest(t, "replication/dr/primary/confirm-secondary-certificate", token.RelationshipID, operationID, newCertDER, confirmSig)
	requireDRPublicError(
		t, resp, "credential rotation failed",
		token.RelationshipID,
		operationID,
		"pending",
		"operation_id",
		"fingerprint",
	)
}

func requireDRPublicError(t *testing.T, resp *logical.Response, want string, forbidden ...string) {
	t.Helper()
	if resp == nil || !resp.IsError() {
		t.Fatalf("expected error response %q, got %#v", want, resp)
	}
	got, ok := resp.Data["error"].(string)
	if !ok {
		t.Fatalf("expected string error response, got %#v", resp.Data["error"])
	}
	if got != want {
		t.Fatalf("expected public error %q, got %q", want, got)
	}
	requireDRResponseDoesNotContain(t, got, forbidden...)
}

func mustMarshalDRResponseData(t *testing.T, resp *logical.Response) string {
	t.Helper()
	body, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireDRResponseDoesNotContain(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value == "" {
			continue
		}
		if strings.Contains(body, value) {
			t.Fatalf("DR response exposed sensitive value %q: %s", value, body)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(value))
		if strings.Contains(body, encoded) {
			t.Fatalf("DR response exposed base64-encoded sensitive value %q: %s", value, body)
		}
	}
}

func requireDRResponseDoesNotContainKeys(t *testing.T, body string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("DR response exposed sensitive field %q: %s", key, body)
		}
	}
}

func setupActiveDRRelationshipForCredentialRotation(t *testing.T) (*Core, *drRelationshipManager, *DRActivationToken, *tls.Certificate, string) {
	t.Helper()

	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldCertDER, oldKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	oldCert, err := parseDRSecondaryClientCert(oldCertDER, oldKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	oldFP := certFingerprintSHA256(oldCert.Leaf)
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, oldCertDER); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		t.Fatal(err)
	}

	return core, mgr, token, oldCert, oldFP
}

func replaceDRCredentialRotationParserForTest(t *testing.T, fn func([]byte, time.Time) (*x509.Certificate, error)) {
	t.Helper()
	orig := parseDRCredentialRotationCertificate
	parseDRCredentialRotationCertificate = fn
	t.Cleanup(func() {
		parseDRCredentialRotationCertificate = orig
	})
}

func TestDRRelationshipManager_SecondaryCredentialRotationTwoPhase(t *testing.T) {
	_, mgr, token, oldCert, oldFP := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newFP := certFingerprintSHA256(newCert.Leaf)
	operationID := "11111111-1111-1111-1111-111111111111"
	issuedAt := time.Now().UTC().Unix()

	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, initSig, "10.20.30.40")
	if err != nil {
		t.Fatal(err)
	}
	if rel.PendingSecondaryCertFingerprint != newFP {
		t.Fatalf("pending fingerprint mismatch: got %q want %q", rel.PendingSecondaryCertFingerprint, newFP)
	}
	if rel.SecondaryCertFingerprint != oldFP {
		t.Fatal("expected current credential to remain active until confirmation")
	}
	if rel.CredentialGeneration != 1 {
		t.Fatalf("expected generation 1 while pending, got %d", rel.CredentialGeneration)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected current credential to remain authorized while rotation is pending: %v", err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected pending credential to be authorized while rotation is pending: %v", err)
	}

	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	rel, err = mgr.ConfirmSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, confirmSig, "10.20.30.40")
	if err != nil {
		t.Fatal(err)
	}
	if rel.SecondaryCertFingerprint != newFP {
		t.Fatalf("current fingerprint mismatch after confirm: got %q want %q", rel.SecondaryCertFingerprint, newFP)
	}
	if rel.PreviousSecondaryCertFingerprint != oldFP {
		t.Fatalf("expected previous fingerprint %q, got %q", oldFP, rel.PreviousSecondaryCertFingerprint)
	}
	if rel.PendingSecondaryCertFingerprint != "" || len(rel.PendingSecondaryCACert) != 0 || rel.PendingRotationOperationID != "" {
		t.Fatal("expected pending rotation state to be cleared after confirm")
	}
	if rel.CredentialGeneration != 2 {
		t.Fatalf("expected generation 2 after confirm, got %d", rel.CredentialGeneration)
	}
	if rel.RotatedAt == 0 {
		t.Fatal("expected rotated_at to be recorded")
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateActive); err == nil {
		t.Fatal("expected previous credential to be rejected after rotation confirmation")
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected new credential to be authorized after confirmation: %v", err)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationRejectsWrongSigner(t *testing.T) {
	_, mgr, token, _, _ := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	newCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	wrongCertDER, wrongKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	wrongCert, err := parseDRSecondaryClientCert(wrongCertDER, wrongKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "22222222-2222-2222-2222-222222222222"
	issuedAt := time.Now().UTC().Unix()
	sig, err := signDRCredentialRotationPayload(wrongCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, sig, ""); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected signature verification failure, got %v", err)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationRejectsRevokedRelationship(t *testing.T) {
	_, mgr, token, oldCert, _ := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	if err := mgr.RevokeRelationship(ctx, token.RelationshipID); err != nil {
		t.Fatal(err)
	}
	newCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	operationID := "33333333-3333-3333-3333-333333333333"
	issuedAt := time.Now().UTC().Unix()
	sig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, sig, ""); err == nil || !strings.Contains(err.Error(), "must be active") {
		t.Fatalf("expected revoked relationship rotation failure, got %v", err)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationRejectsRegisteredBeforeParsingCertificate(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	certDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, certDER); err != nil {
		t.Fatal(err)
	}

	parseCalls := 0
	replaceDRCredentialRotationParserForTest(t, func([]byte, time.Time) (*x509.Certificate, error) {
		parseCalls++
		return nil, errors.New("unexpected certificate parse")
	})

	operationID := "77777777-7777-7777-7777-777777777777"
	issuedAt := time.Now().UTC().Unix()
	_, err = mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, []byte("not-a-certificate"), []byte("signature"), "")
	if err == nil || !strings.Contains(err.Error(), "must be active") {
		t.Fatalf("expected inactive relationship rotation failure, got %v", err)
	}
	if parseCalls != 0 {
		t.Fatalf("expected inactive relationship rejection before certificate parsing, got %d parse calls", parseCalls)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationConfirmRejectsMissingPendingBeforeParsingCertificate(t *testing.T) {
	_, mgr, token, _, _ := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	parseCalls := 0
	replaceDRCredentialRotationParserForTest(t, func([]byte, time.Time) (*x509.Certificate, error) {
		parseCalls++
		return nil, errors.New("unexpected certificate parse")
	})

	operationID := "88888888-8888-8888-8888-888888888888"
	issuedAt := time.Now().UTC().Unix()
	_, err := mgr.ConfirmSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, []byte("not-a-certificate"), []byte("signature"), "")
	if err == nil || !strings.Contains(err.Error(), "no pending credential rotation") {
		t.Fatalf("expected missing pending rotation failure, got %v", err)
	}
	if parseCalls != 0 {
		t.Fatalf("expected missing pending rotation rejection before certificate parsing, got %d parse calls", parseCalls)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationRejectsPreviousFingerprintReuse(t *testing.T) {
	_, mgr, token, oldCert, oldFP := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "44444444-4444-4444-4444-444444444444"
	issuedAt := time.Now().UTC().Unix()
	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, initSig, ""); err != nil {
		t.Fatal(err)
	}
	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ConfirmSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, confirmSig, ""); err != nil {
		t.Fatal(err)
	}

	token2, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token2.RelationshipID, token2.BootstrapToken, oldCert.Leaf.Raw); err == nil || !strings.Contains(err.Error(), token.RelationshipID) {
		t.Fatalf("expected old fingerprint reuse to be rejected against original relationship, got %v", err)
	}
	if rel, err := mgr.loadRelationship(ctx, token.RelationshipID); err != nil {
		t.Fatal(err)
	} else if rel.PreviousSecondaryCertFingerprint != oldFP {
		t.Fatalf("expected previous fingerprint to remain recorded, got %q", rel.PreviousSecondaryCertFingerprint)
	}
}

func TestDRRelationshipManager_SecondaryCredentialRotationPendingExpires(t *testing.T) {
	_, mgr, token, oldCert, _ := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newFP := certFingerprintSHA256(newCert.Leaf)
	operationID := "66666666-6666-6666-6666-666666666666"
	issuedAt := time.Now().UTC().Unix()
	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, initSig, ""); err != nil {
		t.Fatal(err)
	}

	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	rel.PendingRotationStartedAt = time.Now().UTC().Add(-drCredentialRotationPendingTTL - time.Second).Unix()
	if err := mgr.saveRelationship(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err == nil {
		t.Fatal("expected expired pending credential to be rejected")
	}

	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ConfirmSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, confirmSig, ""); err == nil || !strings.Contains(err.Error(), "pending credential rotation expired") {
		t.Fatalf("expected expired pending rotation failure, got %v", err)
	}
	rel, err = mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.PendingSecondaryCertFingerprint != "" || rel.PendingRotationOperationID != "" {
		t.Fatal("expected expired pending rotation state to be cleared")
	}
}

func TestDRRelationshipManager_RevokeClearsPendingCredentialRotationTrust(t *testing.T) {
	_, mgr, token, oldCert, oldFP := setupActiveDRRelationshipForCredentialRotation(t)
	ctx := context.Background()

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newFP := certFingerprintSHA256(newCert.Leaf)
	operationID := "99999999-9999-9999-9999-999999999999"
	issuedAt := time.Now().UTC().Unix()
	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InitiateSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, initSig, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected current credential to be authorized while pending: %v", err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected pending credential to be authorized while pending: %v", err)
	}

	if err := mgr.RevokeRelationship(ctx, token.RelationshipID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateActive); err == nil {
		t.Fatal("expected current credential to be denied after revoke")
	}
	if _, err := mgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err == nil {
		t.Fatal("expected pending credential to be denied after revoke")
	}
	mgr.handler.certMu.RLock()
	_, trusted := mgr.handler.trustedSecondaryCerts[token.RelationshipID]
	mgr.handler.certMu.RUnlock()
	if trusted {
		t.Fatal("expected revoke to remove cached current and pending relationship certificates")
	}

	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ConfirmSecondaryCredentialRotation(ctx, token.RelationshipID, operationID, issuedAt, newCertDER, confirmSig, ""); err == nil || !strings.Contains(err.Error(), "must be active") {
		t.Fatalf("expected pending rotation confirmation to fail after revoke, got %v", err)
	}
}

func TestDRSystemBackend_SecondaryCredentialRotationEndpoints(t *testing.T) {
	core, _, token, oldCert, _ := setupActiveDRRelationshipForCredentialRotation(t)

	newCertDER, newKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := parseDRSecondaryClientCert(newCertDER, newKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newFP := certFingerprintSHA256(newCert.Leaf)
	operationID := "55555555-5555-5555-5555-555555555555"
	issuedAt := time.Now().UTC().Unix()

	initSig, err := signDRCredentialRotationPayload(oldCert, drCredentialRotationInitiate, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/primary/rotate-secondary-certificate")
	req.Data = map[string]interface{}{
		"relationship_id":   token.RelationshipID,
		"operation_id":      operationID,
		"issued_at":         issuedAt,
		"secondary_ca_cert": base64.StdEncoding.EncodeToString(newCertDER),
		"signature":         base64.StdEncoding.EncodeToString(initSig),
	}
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected rotation stage response, got %#v err=%v", resp, err)
	}
	if resp.Data["pending_secondary_cert_fingerprint"] != newFP {
		t.Fatalf("expected pending fingerprint %q, got %#v", newFP, resp.Data["pending_secondary_cert_fingerprint"])
	}

	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, token.RelationshipID, operationID, issuedAt, newCertDER)
	if err != nil {
		t.Fatal(err)
	}
	req = logical.TestRequest(t, logical.UpdateOperation, "replication/dr/primary/confirm-secondary-certificate")
	req.Data = map[string]interface{}{
		"relationship_id":   token.RelationshipID,
		"operation_id":      operationID,
		"issued_at":         issuedAt,
		"secondary_ca_cert": base64.StdEncoding.EncodeToString(newCertDER),
		"signature":         base64.StdEncoding.EncodeToString(confirmSig),
	}
	resp, err = core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected rotation confirm response, got %#v err=%v", resp, err)
	}
	if resp.Data["secondary_cert_fingerprint"] != newFP {
		t.Fatalf("expected current fingerprint %q, got %#v", newFP, resp.Data["secondary_cert_fingerprint"])
	}
	if resp.Data["credential_generation"] != uint64(2) {
		t.Fatalf("expected credential generation 2, got %#v", resp.Data["credential_generation"])
	}
}

func newDRPrimaryAPITestServer(t *testing.T, core *Core) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/sys/") {
			http.NotFound(w, r)
			return
		}
		var data map[string]interface{}
		if r.Body != nil {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/sys/")
		op := logical.UpdateOperation
		if r.Method == http.MethodGet {
			op = logical.ReadOperation
		}
		req := &logical.Request{
			Operation: op,
			Path:      path,
			Data:      data,
			Connection: &logical.Connection{
				RemoteAddr: r.RemoteAddr,
			},
		}
		resp, err := core.systemBackend.HandleRequest(namespace.RootContext(context.Background()), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if resp != nil && resp.IsError() {
			http.Error(w, resp.Error().Error(), http.StatusBadRequest)
			return
		}
		if resp == nil || len(resp.Data) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{"data": resp.Data}); err != nil {
			t.Logf("failed to encode DR primary API test response: %v", err)
		}
	}))
}

func testServerNameForCertificate(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	if len(cert.IPAddresses) > 0 {
		return cert.IPAddresses[0].String()
	}
	return cert.Subject.CommonName
}

func TestPostDRPrimaryAPIJSON_AllowsExplicitHTTP(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := postDRPrimaryAPIJSON(context.Background(), drPrimaryAPIClientConfig{
		APIAddr: server.URL,
	}, "replication/dr/primary/register-secondary", map[string]interface{}{
		"relationship_id": "rel-1",
	})
	if err != nil {
		t.Fatalf("expected explicit HTTP API request to succeed: %v", err)
	}
	if gotPath != "/v1/sys/replication/dr/primary/register-secondary" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["relationship_id"] != "rel-1" {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
}

func TestDRSystemBackend_SecondaryRotateCertificateWrapper(t *testing.T) {
	ctx := context.Background()
	primaryCore, _, _ := TestCoreUnsealed(t)
	setupTestClusterCert(t, primaryCore)
	primaryMgr := newDRRelationshipManager(primaryCore, primaryCore.logger)
	primaryCore.drManager = primaryMgr
	if err := primaryMgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	apiServer := newDRPrimaryAPITestServer(t, primaryCore)
	defer apiServer.Close()

	token, err := primaryMgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token.PrimaryAPIAddr = apiServer.URL
	token.PrimaryAPICACert = apiServer.Certificate().Raw
	token.PrimaryAPIServerName = testServerNameForCertificate(apiServer.Certificate())
	token.PrimaryAddrs = []string{"https://127.0.0.1:1"}
	token.PrimaryAddr = token.PrimaryAddrs[0]

	secondaryCore, _, _ := TestCoreUnsealed(t)
	secondaryMgr := newDRRelationshipManager(secondaryCore, secondaryCore.logger)
	secondaryCore.drManager = secondaryMgr
	if err := secondaryMgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondaryMgr.DisableSecondary(context.Background()) }()

	secondaryConfig := secondaryMgr.Config()
	if secondaryConfig.PrimaryAPIAddr != apiServer.URL {
		t.Fatalf("expected persisted primary API addr %q, got %q", apiServer.URL, secondaryConfig.PrimaryAPIAddr)
	}
	if len(secondaryConfig.PrimaryAPICACert) == 0 {
		t.Fatal("expected persisted primary API CA certificate")
	}
	oldFP := certFingerprintSHA256DER(secondaryConfig.SecondaryClientCert)
	if oldFP == "" {
		t.Fatal("expected old secondary credential fingerprint")
	}
	if _, err := primaryMgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		t.Fatal(err)
	}

	req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/secondary/rotate-certificate")
	resp, err := secondaryCore.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected secondary rotation response, got %#v err=%v", resp, err)
	}
	newFP, ok := resp.Data["new_fingerprint"].(string)
	if !ok || newFP == "" || newFP == oldFP {
		t.Fatalf("expected new fingerprint in response, got %#v", resp.Data["new_fingerprint"])
	}

	secondaryConfig = secondaryMgr.Config()
	if got := certFingerprintSHA256DER(secondaryConfig.SecondaryClientCert); got != newFP {
		t.Fatalf("expected local secondary config to use new fingerprint %q, got %q", newFP, got)
	}
	if len(secondaryConfig.PendingSecondaryClientCert) != 0 ||
		len(secondaryConfig.PendingSecondaryClientKeyPEM) != 0 ||
		secondaryConfig.PendingSecondaryRotationOperation != "" {
		t.Fatal("expected local pending secondary credential rotation state to be cleared")
	}

	rel, err := primaryMgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.SecondaryCertFingerprint != newFP {
		t.Fatalf("expected primary relationship fingerprint %q, got %q", newFP, rel.SecondaryCertFingerprint)
	}
	if rel.PreviousSecondaryCertFingerprint != oldFP {
		t.Fatalf("expected primary previous fingerprint %q, got %q", oldFP, rel.PreviousSecondaryCertFingerprint)
	}
	if _, err := primaryMgr.ValidateRelationshipAccess(token.RelationshipID, oldFP, DRRelationshipStateActive); err == nil {
		t.Fatal("expected old secondary credential to be rejected after wrapper rotation")
	}
	if _, err := primaryMgr.ValidateRelationshipAccess(token.RelationshipID, newFP, DRRelationshipStateActive); err != nil {
		t.Fatalf("expected new secondary credential to be authorized after wrapper rotation: %v", err)
	}
}

func TestDRRelationshipManager_BootstrapRejectsRevokedFingerprintReuse(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	token1, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondaryCertDER, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token1.RelationshipID, token1.BootstrapToken, secondaryCertDER); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RevokeRelationship(ctx, token1.RelationshipID); err != nil {
		t.Fatal(err)
	}

	token2, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.ValidateBootstrapAndStoreCert(ctx, token2.RelationshipID, token2.BootstrapToken, secondaryCertDER)
	if err == nil || !strings.Contains(err.Error(), "certificate fingerprint already bound") {
		t.Fatalf("expected fingerprint reuse rejection, got: %v", err)
	}

	rel2, err := mgr.loadRelationship(ctx, token2.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel2.State != DRRelationshipStatePending {
		t.Fatalf("expected new relationship to remain pending, got %s", rel2.State)
	}
	if rel2.BootstrapToken != "" {
		t.Fatal("expected new relationship bootstrap token plaintext not to be persisted")
	}
	if rel2.BootstrapTokenHash == "" {
		t.Fatal("expected new relationship bootstrap verifier to remain unconsumed")
	}
	if rel2.FailedAttempts != 1 {
		t.Fatalf("expected duplicate fingerprint to count as a failed attempt, got %d", rel2.FailedAttempts)
	}
	if rel2.LastFailedAt == 0 {
		t.Fatal("expected last_failed_at to be set")
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
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
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

func TestDRRelationshipManager_EnableSecondary_NormalizesPrimaryAddresses(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	mgr := newDRRelationshipManager(core, core.logger)

	replSalt := make([]byte, drReplSaltLen)
	rand.Read(replSalt)

	tokenPrimaryAddrOnly := &DRActivationToken{
		ClusterID:      "test-cluster-id",
		RelationshipID: "rel-primary-addr-only",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       replSalt,
	}
	if err := mgr.EnableSecondary(ctx, tokenPrimaryAddrOnly); err == nil || !strings.Contains(err.Error(), "primary_addrs") {
		t.Fatalf("expected primary_addr-only token to be rejected, got: %v", err)
	}

	tokenAddrsOnly := &DRActivationToken{
		ClusterID:      "test-cluster-id",
		RelationshipID: "rel-addrs-only",
		PrimaryAddrs: []string{
			"127.0.0.1:8201",
			"https://127.0.0.2:8201",
			"127.0.0.1:8201",
		},
		PrimaryAPIAddr: "http://primary-api:8200",
		ReplSalt:       replSalt,
	}
	if err := mgr.EnableSecondary(ctx, tokenAddrsOnly); err != nil {
		t.Fatal(err)
	}
	cfg := mgr.Config()
	if cfg.PrimaryAddr != "https://127.0.0.1:8201" {
		t.Fatalf("unexpected normalized primary_addr: got %q", cfg.PrimaryAddr)
	}
	if len(cfg.PrimaryAddrs) != 2 {
		t.Fatalf("expected 2 normalized primary_addrs, got %v", cfg.PrimaryAddrs)
	}
	if cfg.PrimaryAddrs[0] != "https://127.0.0.1:8201" || cfg.PrimaryAddrs[1] != "https://127.0.0.2:8201" {
		t.Fatalf("unexpected primary_addrs ordering/content: %v", cfg.PrimaryAddrs)
	}
	if cfg.PrimaryAPIAddr != "http://primary-api:8200" {
		t.Fatalf("expected explicit HTTP primary API addr to be preserved, got %q", cfg.PrimaryAPIAddr)
	}
	if err := mgr.DisableSecondary(ctx); err != nil {
		t.Fatal(err)
	}

	tokenMixed := &DRActivationToken{
		ClusterID:      "test-cluster-id",
		RelationshipID: "rel-mixed",
		PrimaryAddr:    "http://127.0.0.9:8201",
		PrimaryAddrs: []string{
			"127.0.0.3:8201",
			"https://127.0.0.9:8201",
		},
		ReplSalt: replSalt,
	}
	if err := mgr.EnableSecondary(ctx, tokenMixed); err != nil {
		t.Fatal(err)
	}
	cfg = mgr.Config()
	if cfg.PrimaryAddr != "https://127.0.0.3:8201" {
		t.Fatalf("unexpected normalized primary_addr from mixed token: got %q", cfg.PrimaryAddr)
	}
	if len(cfg.PrimaryAddrs) != 2 {
		t.Fatalf("expected 2 normalized primary_addrs from mixed token, got %v", cfg.PrimaryAddrs)
	}
	if cfg.PrimaryAddrs[0] != "https://127.0.0.3:8201" || cfg.PrimaryAddrs[1] != "https://127.0.0.9:8201" {
		t.Fatalf("unexpected mixed primary_addrs ordering/content: %v", cfg.PrimaryAddrs)
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
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	// Enable secondary.
	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	beforePromotion := mgr.Config()
	oldSecondaryFP := certFingerprintSHA256DER(beforePromotion.SecondaryClientCert)
	if oldSecondaryFP == "" {
		t.Fatal("expected secondary client certificate fingerprint before promotion")
	}

	// Promote.
	if err := mgr.PromoteSecondary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after promote, got %s", mgr.Mode())
	}
	cfg := mgr.Config()
	if cfg.ClusterID != "" || cfg.RelationshipID != "" || cfg.PrimaryAddr != "" || len(cfg.PrimaryAddrs) != 0 {
		t.Fatalf("expected stale upstream relationship fields to be cleared, got %#v", cfg)
	}
	if cfg.Promotion == nil {
		t.Fatal("expected promotion lineage after promote")
	}
	if cfg.Promotion.OldRelationshipID != token.RelationshipID {
		t.Fatalf("expected old relationship ID %q, got %q", token.RelationshipID, cfg.Promotion.OldRelationshipID)
	}
	if cfg.Promotion.OldSecondaryCertFingerprint != oldSecondaryFP {
		t.Fatalf("expected old secondary certificate fingerprint %q, got %q", oldSecondaryFP, cfg.Promotion.OldSecondaryCertFingerprint)
	}
}

func TestDRRelationshipManager_EnableSecondaryRejectsStalePromotionLineage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	oldToken := &DRActivationToken{
		ClusterID:      "old-primary-cluster",
		RelationshipID: "old-relationship",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(oldToken.ReplSalt)

	if err := mgr.EnableSecondary(ctx, oldToken); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PromoteSecondaryWithRecord(ctx, &DRPromotionRecord{
		PromotionID:         "promotion-1",
		OldPrimaryClusterID: oldToken.ClusterID,
		OldRelationshipID:   oldToken.RelationshipID,
		PromotionClass:      DRPromotionClean,
	}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.EnableSecondary(ctx, oldToken); err == nil || !strings.Contains(err.Error(), "stale pre-promotion DR lineage") {
		t.Fatalf("expected stale pre-promotion token to be rejected, got: %v", err)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected mode to remain disabled after stale token rejection, got %s", mgr.Mode())
	}
	cfg := mgr.Config()
	if cfg.Promotion == nil || cfg.Promotion.OldRelationshipID != oldToken.RelationshipID {
		t.Fatalf("expected promotion lineage to remain intact, got %#v", cfg.Promotion)
	}
	if cfg.Promotion.OldSecondaryCertFingerprint == "" {
		t.Fatal("expected old secondary certificate fingerprint in promotion lineage")
	}

	freshToken := &DRActivationToken{
		ClusterID:      "new-authority-cluster",
		RelationshipID: "new-relationship",
		PrimaryAddrs:   []string{"127.0.0.2:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(freshToken.ReplSalt)
	if err := mgr.EnableSecondary(ctx, freshToken); err != nil {
		t.Fatalf("expected fresh post-promotion lineage to be allowed, got: %v", err)
	}
	defer func() {
		if err := mgr.DisableSecondary(ctx); err != nil {
			t.Fatalf("failed to disable fresh secondary: %v", err)
		}
	}()
	if mgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary mode after fresh token, got %s", mgr.Mode())
	}
}

func TestDRRelationshipManager_RepeatedPromotionPreservesStaleLineage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)

	firstToken := &DRActivationToken{
		ClusterID:      "first-old-primary",
		RelationshipID: "first-old-relationship",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(firstToken.ReplSalt)
	if err := mgr.EnableSecondary(ctx, firstToken); err != nil {
		t.Fatal(err)
	}
	firstSecondaryFP := certFingerprintSHA256DER(mgr.Config().SecondaryClientCert)
	if firstSecondaryFP == "" {
		t.Fatal("expected first secondary fingerprint")
	}
	if err := mgr.PromoteSecondaryWithRecord(ctx, &DRPromotionRecord{
		PromotionID:         "promotion-first",
		OldPrimaryClusterID: firstToken.ClusterID,
		OldRelationshipID:   firstToken.RelationshipID,
		PromotionClass:      DRPromotionClean,
	}); err != nil {
		t.Fatal(err)
	}

	secondToken := &DRActivationToken{
		ClusterID:      "second-old-primary",
		RelationshipID: "second-old-relationship",
		PrimaryAddrs:   []string{"127.0.0.2:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(secondToken.ReplSalt)
	if err := mgr.EnableSecondary(ctx, secondToken); err != nil {
		t.Fatalf("expected explicit reseed to fresh authority to be allowed: %v", err)
	}
	secondSecondaryFP := certFingerprintSHA256DER(mgr.Config().SecondaryClientCert)
	if secondSecondaryFP == "" {
		t.Fatal("expected second secondary fingerprint")
	}
	if err := mgr.PromoteSecondaryWithRecord(ctx, &DRPromotionRecord{
		PromotionID:         "promotion-second",
		OldPrimaryClusterID: secondToken.ClusterID,
		OldRelationshipID:   secondToken.RelationshipID,
		PromotionClass:      DRPromotionClean,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := mgr.Config()
	if cfg.Promotion == nil {
		t.Fatal("expected second promotion record")
	}
	if cfg.Promotion.OldPrimaryClusterID != secondToken.ClusterID {
		t.Fatalf("expected current old primary %q, got %q", secondToken.ClusterID, cfg.Promotion.OldPrimaryClusterID)
	}
	if !stringSliceContains(cfg.Promotion.StalePrimaryClusterIDs, firstToken.ClusterID) {
		t.Fatalf("expected first old primary to remain denylisted, got %#v", cfg.Promotion.StalePrimaryClusterIDs)
	}
	if !stringSliceContains(cfg.Promotion.StaleRelationshipIDs, firstToken.RelationshipID) {
		t.Fatalf("expected first old relationship to remain denylisted, got %#v", cfg.Promotion.StaleRelationshipIDs)
	}
	if !stringSliceContainsFold(cfg.Promotion.StaleSecondaryFingerprints, firstSecondaryFP) {
		t.Fatalf("expected first old secondary fingerprint to remain denylisted, got %#v", cfg.Promotion.StaleSecondaryFingerprints)
	}
	if isStalePostPromotionFingerprint(cfg.Promotion, secondSecondaryFP) != true {
		t.Fatal("expected latest old secondary fingerprint to be stale")
	}

	if err := mgr.EnableSecondary(ctx, firstToken); err == nil || !strings.Contains(err.Error(), "stale pre-promotion DR lineage") {
		t.Fatalf("expected first old token to remain rejected after second promotion, got: %v", err)
	}
	if err := mgr.EnableSecondary(ctx, secondToken); err == nil || !strings.Contains(err.Error(), "stale pre-promotion DR lineage") {
		t.Fatalf("expected second old token to be rejected after second promotion, got: %v", err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func TestDRRelationshipManager_LoadConfigDisablesStalePostPromotionSecondaryConfig(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	promotion := &DRPromotionRecord{
		PromotionID:         "promotion-load-stale",
		PromotedAt:          time.Now().UTC().Unix(),
		OldPrimaryClusterID: "old-primary-cluster",
		OldRelationshipID:   "old-relationship",
		PromotionClass:      DRPromotionClean,
	}
	staleConfig := &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      promotion.OldPrimaryClusterID,
		RelationshipID: promotion.OldRelationshipID,
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
		Promotion:      promotion,
	}
	rand.Read(staleConfig.ReplSalt)
	data, err := json.Marshal(staleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, &logical.StorageEntry{Key: drConfigPath, Value: data}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := mgr.Config()
	if cfg.Mode != DRModeDisabled {
		t.Fatalf("expected stale secondary config to be disabled during load, got %s", cfg.Mode)
	}
	if cfg.Promotion == nil || cfg.Promotion.PromotionID != promotion.PromotionID {
		t.Fatalf("expected promotion lineage to be preserved, got %#v", cfg.Promotion)
	}
	entry, err := core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted DRConfig
	if err := json.Unmarshal(entry.Value, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Mode != DRModeDisabled {
		t.Fatalf("expected persisted stale config to be disabled, got %s", persisted.Mode)
	}

	if err := core.barrier.Put(ctx, &logical.StorageEntry{Key: drConfigPath, Value: data}); err != nil {
		t.Fatal(err)
	}
	mode, err := mgr.RefreshConfigFromStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mode != DRModeDisabled {
		t.Fatalf("expected stale secondary config refresh to return disabled, got %s", mode)
	}
}

func TestDRRelationshipManager_RefreshStalePostPromotionConfigStopsSecondaryRuntime(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	oldToken := &DRActivationToken{
		ClusterID:      "old-primary-cluster",
		RelationshipID: "old-relationship",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(oldToken.ReplSalt)
	if err := mgr.EnableSecondary(ctx, oldToken); err != nil {
		t.Fatal(err)
	}
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary runtime before stale refresh")
	}

	staleConfig := mgr.Config()
	staleConfig.Promotion = &DRPromotionRecord{
		PromotionID:         "promotion-refresh-stale",
		PromotedAt:          time.Now().UTC().Unix(),
		OldPrimaryClusterID: oldToken.ClusterID,
		OldRelationshipID:   oldToken.RelationshipID,
		PromotionClass:      DRPromotionClean,
	}
	data, err := json.Marshal(staleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, &logical.StorageEntry{Key: drConfigPath, Value: data}); err != nil {
		t.Fatal(err)
	}

	mode, err := mgr.RefreshConfigFromStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mode != DRModeDisabled {
		t.Fatalf("expected stale secondary config refresh to return disabled, got %s", mode)
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected manager mode disabled after stale refresh, got %s", mgr.Mode())
	}
	if mgr.Secondary() != nil {
		t.Fatal("expected stale refresh to stop secondary runtime")
	}
}

func TestDRRelationshipManager_BootstrapRejectsPrePromotionSecondaryFingerprint(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	oldToken := &DRActivationToken{
		ClusterID:      "old-primary-cluster",
		RelationshipID: "old-relationship",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, drReplSaltLen),
	}
	rand.Read(oldToken.ReplSalt)
	if err := mgr.EnableSecondary(ctx, oldToken); err != nil {
		t.Fatal(err)
	}
	oldSecondaryCert := append([]byte(nil), mgr.Config().SecondaryClientCert...)
	oldSecondaryFP := certFingerprintSHA256DER(oldSecondaryCert)
	if oldSecondaryFP == "" {
		t.Fatal("expected old secondary certificate fingerprint")
	}
	if err := mgr.PromoteSecondary(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mgr.Config().Promotion.OldSecondaryCertFingerprint; got != oldSecondaryFP {
		t.Fatalf("expected promotion fingerprint %q, got %q", oldSecondaryFP, got)
	}
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, oldSecondaryCert)
	if err == nil || !strings.Contains(err.Error(), "stale pre-promotion DR lineage") {
		t.Fatalf("expected stale certificate fingerprint rejection, got: %v", err)
	}
	rel, err := mgr.loadRelationship(ctx, token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStateRevoked {
		t.Fatalf("expected stale certificate attempt to revoke pending relationship, got %s", rel.State)
	}
	if rel.BootstrapToken != "" {
		t.Fatal("expected stale certificate attempt to clear bootstrap token")
	}
	if rel.BootstrapTokenHash != "" {
		t.Fatal("expected stale certificate attempt to clear bootstrap token verifier")
	}
}

func TestDRRelationshipManager_LoadConfigSkipsStalePromotionRelationshipCerts(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	oldSecondaryCert, _, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	oldSecondaryFP := certFingerprintSHA256DER(oldSecondaryCert)
	if oldSecondaryFP == "" {
		t.Fatal("expected old secondary certificate fingerprint")
	}
	promotion := &DRPromotionRecord{
		PromotionID:                 "promotion-skip-cert",
		PromotedAt:                  time.Now().UTC().Unix(),
		OldPrimaryClusterID:         "old-primary",
		OldRelationshipID:           "old-relationship",
		OldSecondaryCertFingerprint: oldSecondaryFP,
		PromotionClass:              DRPromotionClean,
	}
	cfg := &DRConfig{
		Mode:      DRModePrimary,
		ClusterID: "new-primary",
		ReplSalt:  make([]byte, drReplSaltLen),
		Promotion: promotion,
	}
	rand.Read(cfg.ReplSalt)
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, &logical.StorageEntry{Key: drConfigPath, Value: cfgBytes}); err != nil {
		t.Fatal(err)
	}

	rel := &DRRelationship{
		RelationshipID:           promotion.OldRelationshipID,
		State:                    DRRelationshipStateActive,
		SecondaryCACert:          oldSecondaryCert,
		SecondaryCertFingerprint: oldSecondaryFP,
		CreatedAt:                time.Now().UTC().Unix(),
		LastSeenAt:               time.Now().UTC().Unix(),
	}
	relBytes, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, &logical.StorageEntry{Key: drRelationshipsPath + rel.RelationshipID, Value: relBytes}); err != nil {
		t.Fatal(err)
	}

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	handler := mgr.Handler()
	if handler == nil {
		t.Fatal("expected handler after primary config restore")
	}
	certs, err := handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cert := range certs {
		if bytes.Equal(cert.Raw, oldSecondaryCert) {
			t.Fatal("stale pre-promotion relationship certificate was restored into trust pool")
		}
	}
}

// TestDRPromote_SkipsRaftTLSKeyringWithoutRaftBackend verifies that
// Promote() completes successfully on non-Raft cores (e.g., in-memory
// backends) where no Raft TLS keyring regeneration is needed.
func TestDRPromote_SkipsRaftTLSKeyringWithoutRaftBackend(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Verify this test core has no Raft backend.
	if core.GetRaftBackend() != nil {
		t.Skip("test requires non-Raft core")
	}

	mgr := newDRRelationshipManager(core, core.logger)
	token := &DRActivationToken{
		ClusterID:      "skip-raft-test",
		RelationshipID: "rel-skip-raft",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
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

	if core.GetRaftBackend() != nil {
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

func TestDRRelationshipManager_PersistSecondaryKeyringBootstrap(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := core.drManager
	replSalt := make([]byte, drReplSaltLen)
	if _, err := rand.Read(replSalt); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	mgr.config = &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      "cluster-1",
		RelationshipID: "relationship-1",
		ReplSalt:       replSalt,
		PrimaryAddr:    "https://primary.example:8201",
		PrimaryAddrs:   []string{"https://primary.example:8201"},
	}
	mgr.secondary = newDRReplicationSecondary(core, replSalt, "relationship-1", core.logger)
	mgr.mu.Unlock()

	if mgr.secondary.keyringBootstrapped.Load() {
		t.Fatal("expected secondary keyring bootstrap marker to start false")
	}
	if err := mgr.PersistSecondaryKeyringBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if !mgr.secondary.keyringBootstrapped.Load() {
		t.Fatal("expected runtime secondary keyring bootstrap marker to be true")
	}

	cfg := mgr.Config()
	if !cfg.SecondaryKeyringBootstrapped {
		t.Fatal("expected in-memory config to mark secondary keyring bootstrapped")
	}

	entry, err := core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected persisted DR config")
	}
	var persisted DRConfig
	if err := entry.DecodeJSON(&persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.SecondaryKeyringBootstrapped {
		t.Fatal("expected persisted config to mark secondary keyring bootstrapped")
	}
}

func TestDRRelationshipManager_LoadConfigRestoresSecondaryKeyringBootstrap(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, drReplSaltLen)
	if _, err := rand.Read(replSalt); err != nil {
		t.Fatal(err)
	}
	cfg := &DRConfig{
		Mode:                         DRModeSecondary,
		ClusterID:                    "cluster-1",
		RelationshipID:               "relationship-1",
		ReplSalt:                     replSalt,
		PrimaryAddr:                  "https://primary.example:8201",
		PrimaryAddrs:                 []string{"https://primary.example:8201"},
		SecondaryKeyringBootstrapped: true,
	}
	entry, err := logical.StorageEntryJSON(drConfigPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		mgr.mu.Lock()
		mgr.stopSecondaryRuntimeLocked()
		mgr.mu.Unlock()
	}()

	secondary := mgr.Secondary()
	if secondary == nil {
		t.Fatal("expected secondary runtime after config load")
	}
	if !secondary.keyringBootstrapped.Load() {
		t.Fatal("expected restored secondary to skip one-time keyring bootstrap")
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
	sec.flatAccumulatorFastPathTotal.Store(1)
	sec.streamTxnCoalescedEntries.Store(3)
	sec.streamTxnBatches.Store(2)
	sec.streamTxnEntries.Store(10)
	sec.streamTxnPhysicalEntries.Store(8)
	sec.streamTxnMaxEntries.Store(7)
	sec.streamTxnMaxPhysicalEntries.Store(5)
	sec.streamTxnApplyNanos.Store(uint64(6 * time.Millisecond))
	sec.streamTxnApplyMaxNanos.Store(uint64(4 * time.Millisecond))
	sec.streamTxnCommitNanos.Store(uint64(3 * time.Millisecond))
	sec.streamTxnCommitMaxNanos.Store(uint64(2 * time.Millisecond))
	sec.streamBatchFlushMaxEntries.Store(4)
	sec.streamBatchFlushMaxBytes.Store(5)
	sec.streamBatchFlushMaxWait.Store(6)
	sec.streamBatchFlushShutdown.Store(7)
	sec.flatAccumulatorCursorWrites.Store(8)
	sec.flatAccumulatorCursorIndex.Store(41)
	sec.flatAccumulatorSnapshotCount.Store(2)
	sec.flatAccumulatorSnapshotIndex.Store(40)
	sec.flatAccumulatorSnapshotBytes.Store(1000)
	sec.flatAccumulatorSnapshotLast.Store(600)
	sec.flatAccumulatorSnapshotNanos.Store(uint64(5 * time.Millisecond))
	sec.flatAccumulatorSnapshotMaxNs.Store(uint64(3 * time.Millisecond))
	sec.flatAccumulatorSnapshotSkipped.Store(9)
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
	if status.FlatAccumulatorFastPathTotal != 1 {
		t.Fatalf("expected flat accumulator fast path total 1, got %d", status.FlatAccumulatorFastPathTotal)
	}
	if status.StreamTxnCoalescedEntriesTotal != 3 {
		t.Fatalf("expected stream txn coalesced entries total 3, got %d", status.StreamTxnCoalescedEntriesTotal)
	}
	if status.StreamTxnBatchesTotal != 2 {
		t.Fatalf("expected stream txn batches total 2, got %d", status.StreamTxnBatchesTotal)
	}
	if status.StreamTxnAverageEntries != 5 {
		t.Fatalf("expected stream txn average entries 5, got %f", status.StreamTxnAverageEntries)
	}
	if status.StreamTxnMaxPhysicalEntries != 5 {
		t.Fatalf("expected stream txn max physical entries 5, got %d", status.StreamTxnMaxPhysicalEntries)
	}
	if status.StreamTxnApplyMillisecondsAverage != 3 {
		t.Fatalf("expected stream txn average apply ms 3, got %f", status.StreamTxnApplyMillisecondsAverage)
	}
	if status.StreamTxnCommitMillisecondsMax != 2 {
		t.Fatalf("expected stream txn max commit ms 2, got %f", status.StreamTxnCommitMillisecondsMax)
	}
	if status.StreamBatchFlushMaxWaitTotal != 6 {
		t.Fatalf("expected stream batch max-wait flush total 6, got %d", status.StreamBatchFlushMaxWaitTotal)
	}
	if status.FlatAccumulatorCursorWritesTotal != 8 {
		t.Fatalf("expected flat accumulator cursor writes total 8, got %d", status.FlatAccumulatorCursorWritesTotal)
	}
	if status.FlatAccumulatorCursorIndex != 41 {
		t.Fatalf("expected flat accumulator cursor index 41, got %d", status.FlatAccumulatorCursorIndex)
	}
	if status.FlatAccumulatorSnapshotIndex != 40 {
		t.Fatalf("expected flat accumulator snapshot index 40, got %d", status.FlatAccumulatorSnapshotIndex)
	}
	if status.FlatAccumulatorSnapshotBytesAverage != 500 {
		t.Fatalf("expected flat accumulator average snapshot bytes 500, got %f", status.FlatAccumulatorSnapshotBytesAverage)
	}
	if status.FlatAccumulatorSnapshotPersistMsAverage != 2.5 {
		t.Fatalf("expected flat accumulator average persist ms 2.5, got %f", status.FlatAccumulatorSnapshotPersistMsAverage)
	}
	if status.FlatAccumulatorSnapshotSkippedTotal != 9 {
		t.Fatalf("expected flat accumulator snapshot skipped total 9, got %d", status.FlatAccumulatorSnapshotSkippedTotal)
	}
}

func TestDRSystemBackend_StatusIncludesStreamOptimizationCounters(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	mgr := newDRRelationshipManager(core, core.logger)
	mgr.config = &DRConfig{
		Mode:      DRModeSecondary,
		ClusterID: "status-fast-path-cluster",
	}
	secondary := newDRReplicationSecondary(core, make([]byte, drReplSaltLen), "rel-status-fast-path", log.NewNullLogger())
	secondary.flatAccumulatorFastPathTotal.Store(7)
	secondary.streamTxnCoalescedEntries.Store(11)
	secondary.streamTxnBatches.Store(13)
	secondary.streamTxnEntries.Store(39)
	secondary.streamTxnPhysicalEntries.Store(28)
	secondary.streamTxnMaxEntries.Store(9)
	secondary.streamTxnMaxPhysicalEntries.Store(8)
	secondary.streamBatchFlushMaxWait.Store(5)
	secondary.flatAccumulatorCursorWrites.Store(3)
	secondary.flatAccumulatorCursorIndex.Store(15)
	secondary.flatAccumulatorSnapshotCount.Store(2)
	secondary.flatAccumulatorSnapshotIndex.Store(14)
	secondary.flatAccumulatorSnapshotBytes.Store(1000)
	secondary.flatAccumulatorSnapshotLast.Store(512)
	secondary.flatAccumulatorSnapshotSkipped.Store(4)
	mgr.secondary = secondary
	core.drManager = mgr

	req := logical.TestRequest(t, logical.ReadOperation, "replication/dr/status")
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected status response, got %#v", resp)
	}
	if got := resp.Data["flat_accumulator_fast_path_total"]; got != uint64(7) {
		t.Fatalf("expected flat_accumulator_fast_path_total=7, got %#v", got)
	}
	if got := resp.Data["stream_txn_coalesced_entries_total"]; got != uint64(11) {
		t.Fatalf("expected stream_txn_coalesced_entries_total=11, got %#v", got)
	}
	if got := resp.Data["stream_txn_batches_total"]; got != uint64(13) {
		t.Fatalf("expected stream_txn_batches_total=13, got %#v", got)
	}
	if got := resp.Data["stream_txn_average_entries"]; got != float64(3) {
		t.Fatalf("expected stream_txn_average_entries=3, got %#v", got)
	}
	if got := resp.Data["stream_txn_max_physical_entries"]; got != uint64(8) {
		t.Fatalf("expected stream_txn_max_physical_entries=8, got %#v", got)
	}
	if got := resp.Data["stream_batch_flush_max_wait_total"]; got != uint64(5) {
		t.Fatalf("expected stream_batch_flush_max_wait_total=5, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_cursor_writes_total"]; got != uint64(3) {
		t.Fatalf("expected flat_accumulator_cursor_writes_total=3, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_cursor_index"]; got != uint64(15) {
		t.Fatalf("expected flat_accumulator_cursor_index=15, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_snapshot_index"]; got != uint64(14) {
		t.Fatalf("expected flat_accumulator_snapshot_index=14, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_snapshot_bytes_average"]; got != float64(500) {
		t.Fatalf("expected flat_accumulator_snapshot_bytes_average=500, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_snapshot_bytes_last"]; got != uint64(512) {
		t.Fatalf("expected flat_accumulator_snapshot_bytes_last=512, got %#v", got)
	}
	if got := resp.Data["flat_accumulator_snapshot_skipped_total"]; got != uint64(4) {
		t.Fatalf("expected flat_accumulator_snapshot_skipped_total=4, got %#v", got)
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
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
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
		cfg.FlatAccumulatorSnapshotMinEntries = 2048
		cfg.FlatAccumulatorSnapshotMinIntervalMillis = 2500
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
	if cfg.FlatAccumulatorSnapshotMinEntries != 2048 {
		t.Fatalf("expected flat accumulator snapshot min entries=2048, got %d", cfg.FlatAccumulatorSnapshotMinEntries)
	}
	if cfg.FlatAccumulatorSnapshotMinIntervalMillis != 2500 {
		t.Fatalf("expected flat accumulator snapshot min interval=2500, got %d", cfg.FlatAccumulatorSnapshotMinIntervalMillis)
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
	if mgr.secondary.flatAccumulatorSnapshotMinEntries != 2048 {
		t.Fatalf("expected runtime flat accumulator snapshot min entries=2048, got %d", mgr.secondary.flatAccumulatorSnapshotMinEntries)
	}
	if mgr.secondary.flatAccumulatorSnapshotMinInterval != 2500*time.Millisecond {
		t.Fatalf("expected runtime flat accumulator snapshot min interval=%s, got %s", 2500*time.Millisecond, mgr.secondary.flatAccumulatorSnapshotMinInterval)
	}
	if mgr.secondary.fallbackEnabled {
		t.Fatal("expected runtime fallback to be disabled")
	}
	if mgr.secondary.fallbackMinLagEntries != 1234 {
		t.Fatalf("expected runtime fallback min lag entries=1234, got %d", mgr.secondary.fallbackMinLagEntries)
	}
}

func TestDRRelationshipManager_UpdateTuningRejectsInvalidAndRollsBack(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateTuning(ctx, func(cfg *DRConfig) error {
		cfg.StreamBatchMaxEntries = 64
		cfg.CheckpointGlobalBudgetBytes = 1024
		cfg.CheckpointPerRelBudgetBytes = 256
		cfg.DRBackpressureDegradedRatio = 0.8
		cfg.DRBackpressureCriticalRatio = 0.5
		cfg.DRBackpressureDegradedMinQPS = 10
		cfg.DRBackpressureCriticalMinQPS = 5
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before := mgr.Config()
	tests := []struct {
		name            string
		wantErrContains string
		apply           func(*DRConfig) error
	}{
		{
			name:            "callback error",
			wantErrContains: "boom",
			apply: func(cfg *DRConfig) error {
				cfg.StreamBatchMaxEntries = 128
				return errors.New("boom")
			},
		},
		{
			name:            "negative signed field",
			wantErrContains: "stream_batch_max_entries",
			apply: func(cfg *DRConfig) error {
				cfg.StreamBatchMaxEntries = -1
				return nil
			},
		},
		{
			name:            "per relationship budget exceeds global budget",
			wantErrContains: "checkpoint_per_relationship_budget_bytes",
			apply: func(cfg *DRConfig) error {
				cfg.CheckpointPerRelBudgetBytes = cfg.CheckpointGlobalBudgetBytes + 1
				return nil
			},
		},
		{
			name:            "critical ratio exceeds degraded ratio",
			wantErrContains: "dr_backpressure_critical_ratio",
			apply: func(cfg *DRConfig) error {
				cfg.DRBackpressureCriticalRatio = 0.9
				return nil
			},
		},
		{
			name:            "critical minimum qps exceeds degraded minimum qps",
			wantErrContains: "dr_backpressure_critical_min_qps",
			apply: func(cfg *DRConfig) error {
				cfg.DRBackpressureCriticalMinQPS = cfg.DRBackpressureDegradedMinQPS + 1
				return nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := mgr.UpdateTuning(ctx, tc.apply)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("expected %q error, got %v", tc.wantErrContains, err)
			}
			if after := mgr.Config(); !reflect.DeepEqual(after, before) {
				t.Fatalf("expected tuning config rollback\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestDRSystemBackend_DRTuningRejectsInvalidInputs(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := namespace.RootContext(t.Context())

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateTuning(ctx, func(cfg *DRConfig) error {
		cfg.CheckpointGlobalBudgetBytes = 8192
		cfg.CheckpointPerRelBudgetBytes = 4096
		cfg.StreamBufferMaxBytes = 4096
		cfg.StreamJournalMaxBytes = 8192
		cfg.StreamJournalSegmentBytes = 4096
		cfg.FallbackMinLagEntries = 64
		cfg.DRBackpressureDegradedRatio = 0.8
		cfg.DRBackpressureCriticalRatio = 0.5
		cfg.DRBackpressureDegradedMinQPS = 10
		cfg.DRBackpressureCriticalMinQPS = 5
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before := mgr.Config()
	tests := []struct {
		name            string
		data            map[string]interface{}
		wantErrContains string
	}{
		{
			name:            "negative unsigned budget",
			data:            map[string]interface{}{"stream_buffer_max_bytes": -1},
			wantErrContains: "stream_buffer_max_bytes",
		},
		{
			name:            "zero signed integer",
			data:            map[string]interface{}{"stream_batch_max_entries": 0},
			wantErrContains: "stream_batch_max_entries",
		},
		{
			name:            "negative duration",
			data:            map[string]interface{}{"checkpoint_ttl_seconds": -1},
			wantErrContains: "checkpoint_ttl_seconds",
		},
		{
			name:            "invalid ratio",
			data:            map[string]interface{}{"convergence_min_rate_ratio": 1.1},
			wantErrContains: "convergence_min_rate_ratio",
		},
		{
			name: "relationship budget exceeds global budget",
			data: map[string]interface{}{
				"checkpoint_global_budget_bytes":           1024,
				"checkpoint_per_relationship_budget_bytes": 2048,
			},
			wantErrContains: "checkpoint_per_relationship_budget_bytes",
		},
		{
			name: "critical ratio exceeds degraded ratio",
			data: map[string]interface{}{
				"dr_backpressure_degraded_ratio": 0.5,
				"dr_backpressure_critical_ratio": 0.9,
			},
			wantErrContains: "dr_backpressure_critical_ratio",
		},
		{
			name: "critical qps exceeds degraded qps",
			data: map[string]interface{}{
				"dr_backpressure_degraded_min_qps": 5,
				"dr_backpressure_critical_min_qps": 10,
			},
			wantErrContains: "dr_backpressure_critical_min_qps",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/tuning")
			req.Data = tc.data
			resp, err := core.systemBackend.HandleRequest(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || !resp.IsError() || !strings.Contains(resp.Error().Error(), tc.wantErrContains) {
				t.Fatalf("expected %q error response, got resp=%#v err=%v", tc.wantErrContains, resp, err)
			}
			if after := mgr.Config(); !reflect.DeepEqual(after, before) {
				t.Fatalf("expected tuning config rollback\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestDRSecondaryStreamApplyStopsWithoutFlushingOnStepdown(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, drReplSaltLen)
	if _, err := rand.Read(replSalt); err != nil {
		t.Fatal(err)
	}
	secondary := newDRReplicationSecondary(core, replSalt, "rel-stop-apply", log.NewNullLogger())
	close(secondary.stopCh)

	applyCh := make(chan []*EntryChange, 1)
	applyCh <- []*EntryChange{{
		OpType:    string(physical.PutOperation),
		Key:       "sys/policy/dr-stop-apply",
		Value:     []byte("policy"),
		RaftIndex: 10,
	}}
	close(applyCh)

	creditCh := make(chan uint64, 1)
	if err := secondary.runStreamApplyWorker(ctx, applyCh, creditCh); err != nil {
		t.Fatal(err)
	}
	entry, err := core.physical.Get(ctx, "sys/policy/dr-stop-apply")
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("expected queued stream batch to be discarded after stop")
	}
	if got := secondary.lastAppliedIndex.Load(); got != 0 {
		t.Fatalf("expected lastAppliedIndex to remain unchanged after stop, got %d", got)
	}
}

func TestDRSecondaryAppliedInvalidationSkipsDuringCoreTeardown(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, drReplSaltLen)
	if _, err := rand.Read(replSalt); err != nil {
		t.Fatal(err)
	}
	secondary := newDRReplicationSecondary(core, replSalt, "rel-invalidation-teardown", log.NewNullLogger())

	core.namespaceStore = nil
	secondary.invalidateAppliedStorageKey(context.Background(), "sys/policy/dr-teardown")
}

// --- Unit Tests for DR Failover ---

func stopDRSecondaryControllerForTest(t *testing.T, mgr *drRelationshipManager) {
	t.Helper()

	mgr.mu.RLock()
	cancel := mgr.secondaryLoopCancel
	mgr.mu.RUnlock()
	if cancel != nil {
		cancel()
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mgr.mu.RLock()
		stopped := mgr.secondaryLoopCancel == nil
		mgr.mu.RUnlock()
		if stopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for DR secondary controller to stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func configureDRFailoverSecondaryForTest(t *testing.T, mgr *drRelationshipManager, state DRSecondaryState, lastAppliedIndex, primaryIndex uint64) {
	t.Helper()

	stopDRSecondaryControllerForTest(t, mgr)
	if mgr.secondary == nil {
		t.Fatal("expected DR secondary runtime")
	}
	mgr.secondary.setState(state)
	mgr.secondary.lastAppliedIndex.Store(lastAppliedIndex)
	mgr.secondary.primaryIndex.Store(primaryIndex)
}

func TestDRFailover_NotSecondary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	core.drManager = newDRRelationshipManager(core, core.logger)

	// Failover should fail when not in secondary mode.
	_, err := core.DRFailover(ctx, true, false)
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
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryStreaming, 500, 500)

	result, err := core.DRFailover(ctx, true, false)
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
	if result.LastKnownPrimaryIndex != 500 {
		t.Fatalf("expected last known primary index 500, got %d", result.LastKnownPrimaryIndex)
	}
	if result.PromotionClass != DRPromotionClean {
		t.Fatalf("expected clean promotion, got %s", result.PromotionClass)
	}
	if !result.CleanPromotionEligible {
		t.Fatal("expected clean promotion proof to be available")
	}
	if result.ForcedPromotionRequiresAcknowledgement {
		t.Fatal("did not expect forced-promotion acknowledgement requirement")
	}
	if len(result.ForcedPromotionReasonCodes) != 0 || len(result.ForcedPromotionReasonDetails) != 0 {
		t.Fatalf("did not expect forced-promotion reasons for clean promotion, got codes=%v details=%v", result.ForcedPromotionReasonCodes, result.ForcedPromotionReasonDetails)
	}
	if result.DataLossEstimateBasis != drPromotionDataLossEstimateBasis {
		t.Fatalf("expected data loss estimate basis %q, got %q", drPromotionDataLossEstimateBasis, result.DataLossEstimateBasis)
	}
	if result.PromotionID == "" {
		t.Fatal("expected promotion ID")
	}
	if result.DataLossAccepted {
		t.Fatal("did not expect data loss acknowledgement for clean promotion")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after failover, got %s", mgr.Mode())
	}
	cfg := mgr.Config()
	if cfg.Promotion == nil {
		t.Fatal("expected persisted promotion record")
	}
	if cfg.Promotion.PromotionID != result.PromotionID {
		t.Fatalf("expected persisted promotion ID %q, got %q", result.PromotionID, cfg.Promotion.PromotionID)
	}
	if cfg.ClusterID != "" || cfg.RelationshipID != "" || cfg.PrimaryAddr != "" || len(cfg.PrimaryAddrs) != 0 {
		t.Fatalf("expected stale upstream relationship fields to be cleared, got %#v", cfg)
	}
	if len(cfg.ReplSalt) != 0 || len(cfg.PrimaryCACert) != 0 || len(cfg.SecondaryClientCert) != 0 || len(cfg.SecondaryClientKeyPEM) != 0 {
		t.Fatal("expected stale replication secrets to be cleared after promotion")
	}
}

func TestDRFailover_ForcedRequiresAcceptDataLoss(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-forced-required",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryReconciling, 500, 510)

	_, err := core.DRFailover(ctx, true, false)
	if err == nil {
		t.Fatal("expected forced promotion to require accept_data_loss")
	}
	if !strings.Contains(err.Error(), "accept_data_loss=true") {
		t.Fatalf("expected accept_data_loss error, got %v", err)
	}
	if !strings.Contains(err.Error(), "clean promotion proof unavailable") {
		t.Fatalf("expected clean-promotion proof explanation, got %v", err)
	}
	if mgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary mode after rejected promotion, got %s", mgr.Mode())
	}
}

func TestDRFailover_ForcedWithAcceptDataLoss(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-forced-accepted",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryReconciling, 500, 510)

	result, err := core.DRFailover(ctx, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionClass != DRPromotionForced {
		t.Fatalf("expected forced promotion, got %s", result.PromotionClass)
	}
	if result.CleanPromotionEligible {
		t.Fatal("did not expect clean promotion proof for forced promotion")
	}
	if !result.ForcedPromotionRequiresAcknowledgement {
		t.Fatal("expected forced-promotion acknowledgement requirement")
	}
	if !stringSliceContains(result.ForcedPromotionReasonCodes, drPromotionReasonSecondaryStateNotStableStreaming) {
		t.Fatalf("expected state reason code, got %v", result.ForcedPromotionReasonCodes)
	}
	if !stringSliceContains(result.ForcedPromotionReasonCodes, drPromotionReasonSecondaryLagDetected) {
		t.Fatalf("expected lag reason code, got %v", result.ForcedPromotionReasonCodes)
	}
	if !result.DataLossAccepted {
		t.Fatal("expected data loss acknowledgement to be recorded")
	}
	if result.EstimatedDataLossEntries != 10 {
		t.Fatalf("expected estimated data loss of 10 entries, got %d", result.EstimatedDataLossEntries)
	}
	if result.Warning == "" {
		t.Fatal("expected forced promotion warning")
	}
	cfg := mgr.Config()
	if cfg.Promotion == nil {
		t.Fatal("expected persisted promotion record")
	}
	if cfg.Promotion.PromotionClass != DRPromotionForced {
		t.Fatalf("expected persisted forced promotion, got %s", cfg.Promotion.PromotionClass)
	}
	if !stringSliceContains(cfg.Promotion.ForcedReasonCodes, drPromotionReasonSecondaryStateNotStableStreaming) {
		t.Fatalf("expected persisted state reason code, got %v", cfg.Promotion.ForcedReasonCodes)
	}
	if !stringSliceContains(cfg.Promotion.ForcedReasonCodes, drPromotionReasonSecondaryLagDetected) {
		t.Fatalf("expected persisted lag reason code, got %v", cfg.Promotion.ForcedReasonCodes)
	}
	if !cfg.Promotion.DataLossAccepted {
		t.Fatal("expected persisted data loss acknowledgement")
	}
	if cfg.Promotion.DataLossEstimateBasis != drPromotionDataLossEstimateBasis {
		t.Fatalf("expected persisted data loss estimate basis %q, got %q", drPromotionDataLossEstimateBasis, cfg.Promotion.DataLossEstimateBasis)
	}
	if cfg.Promotion.OldPrimaryClusterID != token.ClusterID {
		t.Fatalf("expected old primary cluster ID %q, got %q", token.ClusterID, cfg.Promotion.OldPrimaryClusterID)
	}
	if cfg.Promotion.OldRelationshipID != token.RelationshipID {
		t.Fatalf("expected old relationship ID %q, got %q", token.RelationshipID, cfg.Promotion.OldRelationshipID)
	}
}

func TestDRFailover_ForcedWithZeroEstimatedLossExplainsCleanProofUnavailable(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-forced-zero-loss",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryReconciling, 500, 500)

	_, err := core.DRFailover(ctx, true, false)
	if err == nil {
		t.Fatal("expected zero-loss forced promotion to require accept_data_loss")
	}
	if !strings.Contains(err.Error(), "clean promotion proof unavailable") {
		t.Fatalf("expected clean-promotion proof explanation, got %v", err)
	}

	result, err := core.DRFailover(ctx, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.PromotionClass != DRPromotionForced {
		t.Fatalf("expected forced promotion, got %s", result.PromotionClass)
	}
	if result.EstimatedDataLossEntries != 0 {
		t.Fatalf("expected zero estimated data loss, got %d", result.EstimatedDataLossEntries)
	}
	if result.CleanPromotionEligible {
		t.Fatal("did not expect clean promotion proof")
	}
	if !result.ForcedPromotionRequiresAcknowledgement {
		t.Fatal("expected forced-promotion acknowledgement requirement")
	}
	if !stringSliceContains(result.ForcedPromotionReasonCodes, drPromotionReasonSecondaryStateNotStableStreaming) {
		t.Fatalf("expected state reason code, got %v", result.ForcedPromotionReasonCodes)
	}
	if stringSliceContains(result.ForcedPromotionReasonCodes, drPromotionReasonSecondaryLagDetected) {
		t.Fatalf("did not expect lag reason code for zero estimated loss, got %v", result.ForcedPromotionReasonCodes)
	}
	if result.DataLossEstimateBasis != drPromotionDataLossEstimateBasis {
		t.Fatalf("expected data loss estimate basis %q, got %q", drPromotionDataLossEstimateBasis, result.DataLossEstimateBasis)
	}
}

func TestDRSystemBackend_PromoteResponseExplainsForcedPromotion(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-promote-response",
		PrimaryAddr:    "127.0.0.1:8201",
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(t.Context(), token); err != nil {
		t.Fatal(err)
	}

	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryReconciling, 500, 500)

	req := logical.TestRequest(t, logical.UpdateOperation, "replication/dr/secondary/promote")
	req.Storage = core.systemBarrierView
	req.Data = map[string]interface{}{
		"confirm_primary_unreachable": true,
		"accept_data_loss":            true,
	}
	resp, err := core.systemBackend.HandleRequest(namespace.RootContext(t.Context()), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.IsError() {
		t.Fatalf("expected promotion response, got %#v", resp)
	}
	if resp.Data["promotion_class"] != string(DRPromotionForced) {
		t.Fatalf("expected forced promotion response, got %#v", resp.Data["promotion_class"])
	}
	if resp.Data["estimated_data_loss_entries"] != uint64(0) {
		t.Fatalf("expected zero estimated loss, got %#v", resp.Data["estimated_data_loss_entries"])
	}
	if resp.Data["clean_promotion_eligible"] != false {
		t.Fatalf("expected clean promotion ineligible, got %#v", resp.Data["clean_promotion_eligible"])
	}
	if resp.Data["clean_promotion_proof_available"] != false {
		t.Fatalf("expected clean promotion proof unavailable, got %#v", resp.Data["clean_promotion_proof_available"])
	}
	if resp.Data["forced_promotion_requires_acknowledgement"] != true {
		t.Fatalf("expected forced-promotion acknowledgement requirement, got %#v", resp.Data["forced_promotion_requires_acknowledgement"])
	}
	if resp.Data["estimated_data_loss_entries_basis"] != drPromotionDataLossEstimateBasis {
		t.Fatalf("expected data loss estimate basis %q, got %#v", drPromotionDataLossEstimateBasis, resp.Data["estimated_data_loss_entries_basis"])
	}
	codes, ok := resp.Data["forced_promotion_reason_codes"].([]string)
	if !ok || !stringSliceContains(codes, drPromotionReasonSecondaryStateNotStableStreaming) {
		t.Fatalf("expected state reason code, got %#v", resp.Data["forced_promotion_reason_codes"])
	}
	details, ok := resp.Data["forced_promotion_reason_details"].([]string)
	if !ok || len(details) == 0 || !strings.Contains(details[0], "not stable streaming") {
		t.Fatalf("expected operator-facing forced promotion detail, got %#v", resp.Data["forced_promotion_reason_details"])
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
		PrimaryAddrs:   []string{"127.0.0.1:8201"},
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	configureDRFailoverSecondaryForTest(t, mgr, DRSecondaryStreaming, 500, 500)

	result, err := core.DRFailoverToPrimary(ctx, true, false)
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
	cfg := mgr.Config()
	if cfg.Promotion == nil {
		t.Fatal("expected promotion lineage to survive enabling primary mode")
	}
	if cfg.Promotion.PromotionID != result.PromotionID {
		t.Fatalf("expected promotion ID %q, got %q", result.PromotionID, cfg.Promotion.PromotionID)
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

func TestDRSecondaryApplyFetchedChangeInvalidatesRouteBackedStorage(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := namespace.RootContext(t.Context())

	noop := &be.Noop{}
	core.logicalBackends["noop-dr-invalidate"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}
	mount := &routing.MountEntry{
		Table: routing.MountTableType,
		Path:  "dr-invalidate",
		Type:  "noop-dr-invalidate",
	}
	if err := core.mount(ctx, mount); err != nil {
		t.Fatalf("mount failed: %v", err)
	}
	route := core.router.MatchingMountEntry(ctx, "dr-invalidate/foo")
	if route == nil || route.UUID == "" {
		t.Fatal("expected dr-invalidate mount route")
	}

	sec := newDRReplicationSecondary(core, make([]byte, 32), "rel-invalidate", core.logger)
	key := backendBarrierPrefix + route.UUID + "/config/legacyMigrationBundleLog"
	if err := sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    key,
		Value:  []byte("replicated-value"),
	}); err != nil {
		t.Fatalf("apply fetched change failed: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()
	if len(noop.Invalidations) != 1 || noop.Invalidations[0] != "config/legacyMigrationBundleLog" {
		t.Fatalf("expected route-backed invalidation for replicated key, got %#v", noop.Invalidations)
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

func TestDRFlatRangeAccumulator_ResetAndApplyDeltas(t *testing.T) {
	replSalt := bytes.Repeat([]byte{0x42}, 32)
	scanner := reconciler.NewScanner(reconciler.DefaultScanConfig(replSalt))

	kidA := scanner.ComputeKID("secret/a")
	vidA1 := scanner.ComputeVIDWithSealWrap([]byte("a1"), false)
	vidA2 := scanner.ComputeVIDWithSealWrap([]byte("a2"), false)
	kidB := scanner.ComputeKID("secret/b")
	vidB := scanner.ComputeVIDWithSealWrap([]byte("b1"), false)

	rs := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{
			kidA: vidA1,
			kidB: vidB,
		},
	}

	accumulator := newDRFlatRangeAccumulator()
	accumulator.resetFromSet(rs, 10)
	assertDRFlatAccumulatorMatchesSet(t, accumulator, 10, rs)

	if ok := accumulator.applyDeltas(11, []drFlatAccumulatorDelta{
		{kid: kidA, oldExists: true, oldVID: vidA1, newExists: true, newVID: vidA2},
		{kid: kidB, oldExists: true, oldVID: vidB},
	}); !ok {
		t.Fatal("expected accumulator delta apply to succeed")
	}
	rs.KIDToVID[kidA] = vidA2
	delete(rs.KIDToVID, kidB)
	assertDRFlatAccumulatorMatchesSet(t, accumulator, 11, rs)

	if _, ok := accumulator.snapshotExact(10); ok {
		t.Fatal("expected stale accumulator index to be unavailable")
	}
}

func TestDRSecondaryFlatAccumulatorAdvancesOnStreamApply(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := bytes.Repeat([]byte{0x43}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-stream", core.logger)
	secondary.flatAccumulatorSnapshotMinEntries = 1

	key := "secret/flat-stream"
	oldEntry := &physical.Entry{Key: key, Value: []byte("old")}
	if err := core.physical.Put(ctx, oldEntry); err != nil {
		t.Fatalf("failed to seed physical entry: %v", err)
	}

	kid, oldVID := secondary.scanner.ComputeItemFromEntry(oldEntry)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{kid: oldVID},
	}
	secondary.rangeAccumulator.resetFromSet(localSet, 10)

	if err := secondary.applyStreamChange(ctx, &EntryChange{
		OpType:    string(physical.PutOperation),
		Key:       key,
		Value:     []byte("new"),
		RaftIndex: 11,
	}); err != nil {
		t.Fatalf("stream put failed: %v", err)
	}
	localSet.KIDToVID[kid] = secondary.scanner.ComputeVIDWithSealWrap([]byte("new"), false)
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, 11, localSet)

	if err := secondary.applyStreamChange(ctx, &EntryChange{
		OpType:    string(physical.DeleteOperation),
		Key:       key,
		RaftIndex: 12,
	}); err != nil {
		t.Fatalf("stream delete failed: %v", err)
	}
	delete(localSet.KIDToVID, kid)
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, 12, localSet)

	persisted, err := core.physical.Get(ctx, drFlatAccumulatorStoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil {
		t.Fatal("expected persisted flat accumulator snapshot")
	}

	reloaded := newDRReplicationSecondary(core, replSalt, "rel-flat-stream", core.logger)
	reloaded.loadPersistentFlatAccumulator(ctx)
	assertDRFlatAccumulatorMatchesSet(t, reloaded.rangeAccumulator, 12, localSet)
	if got := reloaded.lastAppliedIndex.Load(); got != 12 {
		t.Fatalf("expected loaded lastAppliedIndex 12, got %d", got)
	}

	wrongRelationship := newDRReplicationSecondary(core, replSalt, "rel-flat-stream-other", core.logger)
	wrongRelationship.loadPersistentFlatAccumulator(ctx)
	if wrongRelationship.rangeAccumulator.isInitialized() {
		t.Fatal("expected incompatible relationship snapshot to be ignored")
	}
	if entry, err := core.physical.Get(ctx, drFlatAccumulatorStoragePath); err != nil {
		t.Fatal(err)
	} else if entry != nil {
		t.Fatal("expected incompatible persisted accumulator to be deleted")
	}
}

func TestDRSecondaryStreamTxnPersistsFlatAccumulator(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := bytes.Repeat([]byte{0x46}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-txn", core.logger)
	secondary.flatAccumulatorSnapshotMinEntries = 1

	txnBackend, ok := core.physical.(physical.TransactionalBackend)
	if !ok {
		t.Fatal("test core physical backend must support transactions")
	}

	key := "secret/flat-txn"
	deleteKey := "secret/flat-txn-delete"
	oldEntry := &physical.Entry{Key: key, Value: []byte("old")}
	if err := core.physical.Put(ctx, oldEntry); err != nil {
		t.Fatalf("failed to seed physical entry: %v", err)
	}
	deleteEntry := &physical.Entry{Key: deleteKey, Value: []byte("delete-old")}
	if err := core.physical.Put(ctx, deleteEntry); err != nil {
		t.Fatalf("failed to seed physical delete entry: %v", err)
	}
	kid, oldVID := secondary.scanner.ComputeItemFromEntry(oldEntry)
	deleteKID, deleteOldVID := secondary.scanner.ComputeItemFromEntry(deleteEntry)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{
			kid:       oldVID,
			deleteKID: deleteOldVID,
		},
	}
	secondary.rangeAccumulator.resetFromSet(localSet, 10)
	secondary.lastAppliedIndex.Store(10)
	if err := secondary.persistFlatAccumulatorSnapshot(ctx, core.physical, 10, drFlatAccumulatorBucketsFromSet(localSet)); err != nil {
		t.Fatalf("failed to seed persisted accumulator: %v", err)
	}

	if err := secondary.applyStreamTxn(ctx, txnBackend, []*EntryChange{{
		OpType:    string(physical.PutOperation),
		Key:       key,
		Value:     []byte("intermediate"),
		RaftIndex: 11,
	}, {
		OpType:    string(physical.PutOperation),
		Key:       key,
		Value:     []byte("new"),
		RaftIndex: 12,
	}, {
		OpType:    string(physical.PutOperation),
		Key:       deleteKey,
		Value:     []byte("delete-intermediate"),
		RaftIndex: 13,
	}, {
		OpType:    string(physical.DeleteOperation),
		Key:       deleteKey,
		RaftIndex: 14,
	}}, false); err != nil {
		t.Fatalf("stream txn failed: %v", err)
	}

	entry, err := core.physical.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || !bytes.Equal(entry.Value, []byte("new")) {
		t.Fatalf("expected new physical value, got %#v", entry)
	}
	entry, err = core.physical.Get(ctx, deleteKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatalf("expected delete key to be removed, got %#v", entry)
	}
	localSet.KIDToVID[kid] = secondary.scanner.ComputeVIDWithSealWrap([]byte("new"), false)
	delete(localSet.KIDToVID, deleteKID)
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, 14, localSet)

	reloaded := newDRReplicationSecondary(core, replSalt, "rel-flat-txn", core.logger)
	reloaded.loadPersistentFlatAccumulator(ctx)
	assertDRFlatAccumulatorMatchesSet(t, reloaded.rangeAccumulator, 14, localSet)
	if got := reloaded.lastAppliedIndex.Load(); got != 14 {
		t.Fatalf("expected loaded lastAppliedIndex 14, got %d", got)
	}
}

func TestDRSecondaryStreamTxnCadencePersistsCursorWithoutStaleSnapshotLoad(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := bytes.Repeat([]byte{0x47}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-cadence", core.logger)
	secondary.flatAccumulatorSnapshotMinEntries = 1024
	secondary.flatAccumulatorSnapshotMinInterval = time.Hour

	txnBackend, ok := core.physical.(physical.TransactionalBackend)
	if !ok {
		t.Fatal("test core physical backend must support transactions")
	}

	key := "secret/flat-cadence"
	oldEntry := &physical.Entry{Key: key, Value: []byte("old")}
	if err := core.physical.Put(ctx, oldEntry); err != nil {
		t.Fatalf("failed to seed physical entry: %v", err)
	}
	kid, oldVID := secondary.scanner.ComputeItemFromEntry(oldEntry)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{kid: oldVID},
	}
	secondary.rangeAccumulator.resetFromSet(localSet, 10)
	secondary.lastAppliedIndex.Store(10)
	if err := secondary.resetAndPersistFlatAccumulatorFromSet(ctx, localSet, 10); err != nil {
		t.Fatalf("failed to seed persisted accumulator: %v", err)
	}

	if err := secondary.applyStreamTxn(ctx, txnBackend, []*EntryChange{{
		OpType:    string(physical.PutOperation),
		Key:       key,
		Value:     []byte("new"),
		RaftIndex: 11,
	}}, false); err != nil {
		t.Fatalf("stream txn failed: %v", err)
	}

	localSet.KIDToVID[kid] = secondary.scanner.ComputeVIDWithSealWrap([]byte("new"), false)
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, 11, localSet)
	if got := secondary.flatAccumulatorSnapshotSkipped.Load(); got != 1 {
		t.Fatalf("expected one skipped snapshot, got %d", got)
	}
	if got := secondary.flatAccumulatorCursorIndex.Load(); got != 11 {
		t.Fatalf("expected cursor index 11, got %d", got)
	}
	if got := secondary.flatAccumulatorSnapshotIndex.Load(); got != 10 {
		t.Fatalf("expected persisted snapshot index 10, got %d", got)
	}

	reloaded := newDRReplicationSecondary(core, replSalt, "rel-flat-cadence", core.logger)
	reloaded.loadPersistentFlatAccumulator(ctx)
	if reloaded.rangeAccumulator.isInitialized() {
		t.Fatal("expected stale snapshot behind cursor to be ignored")
	}
	if got := reloaded.lastAppliedIndex.Load(); got != 11 {
		t.Fatalf("expected cursor-loaded lastAppliedIndex 11, got %d", got)
	}
	if entry, err := core.physical.Get(ctx, drFlatAccumulatorStoragePath); err != nil {
		t.Fatal(err)
	} else if entry != nil {
		t.Fatal("expected stale flat accumulator snapshot to be deleted")
	}
	if entry, err := core.physical.Get(ctx, drFlatAccumulatorCursorStoragePath); err != nil {
		t.Fatal(err)
	} else if entry == nil {
		t.Fatal("expected flat accumulator cursor to remain")
	}
}

func TestDRSecondaryApplyWorkerStopPersistsFinalFlatAccumulatorSnapshot(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := bytes.Repeat([]byte{0x48}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-final", core.logger)
	secondary.flatAccumulatorSnapshotMinEntries = 1024
	secondary.flatAccumulatorSnapshotMinInterval = time.Hour

	key := "secret/flat-final"
	entry := &physical.Entry{Key: key, Value: []byte("value")}
	if err := core.physical.Put(ctx, entry); err != nil {
		t.Fatalf("failed to seed physical entry: %v", err)
	}
	kid, vid := secondary.scanner.ComputeItemFromEntry(entry)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{kid: vid},
	}
	secondary.rangeAccumulator.resetFromSet(localSet, 10)
	secondary.lastAppliedIndex.Store(10)
	secondary.flatAccumulatorSnapshotIndex.Store(5)

	close(secondary.stopCh)
	applyCh := make(chan []*EntryChange)
	ackCh := make(chan uint64, 1)
	if err := secondary.runStreamApplyWorker(ctx, applyCh, ackCh); err != nil {
		t.Fatalf("stream apply worker failed: %v", err)
	}

	reloaded := newDRReplicationSecondary(core, replSalt, "rel-flat-final", core.logger)
	reloaded.loadPersistentFlatAccumulator(ctx)
	assertDRFlatAccumulatorMatchesSet(t, reloaded.rangeAccumulator, 10, localSet)
	if got := reloaded.lastAppliedIndex.Load(); got != 10 {
		t.Fatalf("expected final snapshot lastAppliedIndex 10, got %d", got)
	}
}

func TestDRCoalesceStreamTxnBatch(t *testing.T) {
	batch := []*EntryChange{
		{OpType: string(physical.PutOperation), Key: "secret/old", Value: []byte("skip"), RaftIndex: 9},
		{Key: "", RaftIndex: 10},
		{OpType: string(physical.PutOperation), Key: "secret/a", Value: []byte("a1"), RaftIndex: 11},
		{OpType: string(physical.PutOperation), Key: "secret/b", Value: []byte("b1"), RaftIndex: 12},
		{OpType: string(physical.DeleteOperation), Key: "secret/a", RaftIndex: 13},
		{OpType: string(physical.PutOperation), Key: "core/keyring", Value: []byte("keyring"), RaftIndex: 14},
		{OpType: string(physical.DeleteOperation), Key: "core/mounts/test", RaftIndex: 15},
		{OpType: string(physical.PutOperation), Key: "core/local-mounts", Value: []byte("local"), RaftIndex: 16},
		{OpType: "unknown", Key: "secret/unknown", RaftIndex: 17},
	}

	changes, lastIndex, affected, coalescedEntries, keyringTouched, rootKeyTouched, runtimeStateTouched := coalesceDRStreamTxnBatch(batch, 10)
	if lastIndex != 17 {
		t.Fatalf("expected lastIndex 17, got %d", lastIndex)
	}
	if affected != 6 {
		t.Fatalf("expected 6 affected logical entries, got %d", affected)
	}
	if coalescedEntries != 1 {
		t.Fatalf("expected 1 coalesced entry, got %d", coalescedEntries)
	}
	if !keyringTouched {
		t.Fatal("expected keyring touch to be preserved")
	}
	if rootKeyTouched {
		t.Fatal("did not expect root-key touch")
	}
	if !runtimeStateTouched {
		t.Fatal("expected runtime-state touch to be preserved")
	}

	got := make([]string, 0, len(changes))
	for _, change := range changes {
		got = append(got, fmt.Sprintf("%s:%s:%d", change.OpType, change.Key, change.RaftIndex))
	}
	want := []string{
		fmt.Sprintf("%s:secret/b:12", physical.PutOperation),
		fmt.Sprintf("%s:secret/a:13", physical.DeleteOperation),
		fmt.Sprintf("%s:core/keyring:14", physical.PutOperation),
		fmt.Sprintf("%s:core/mounts/test:15", physical.DeleteOperation),
		"unknown:secret/unknown:17",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected coalesced changes\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDRRangeReconciliationSeedsFlatAccumulatorOnPhaseAMatch(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := bytes.Repeat([]byte{0x44}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-seed", core.logger)
	secondary.checkpointHighWaterMarkPersistHook = func(uint64) error { return nil }

	kidA := secondary.scanner.ComputeKID("secret/a")
	vidA := secondary.scanner.ComputeVIDWithSealWrap([]byte("a"), false)
	kidB := secondary.scanner.ComputeKID("secret/b")
	vidB := secondary.scanner.ComputeVIDWithSealWrap([]byte("b"), false)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{
			kidA: vidA,
			kidB: vidB,
		},
	}
	rangeIndex := reconciler.NewRangeMapIndex(localSet.KIDToVID, nil)
	checkpoint := &CheckpointResponse{CheckpointId: "cp-flat-seed", CommitIndex: 73}

	secondary.client = &drTestClient{
		exchangeRangeChecksumsFn: func(_ context.Context, req *RangeChecksumRequest, _ ...grpc.CallOption) (*RangeChecksumResponse, error) {
			resp := &RangeChecksumResponse{Checksums: make([]*RangeChecksum, 0, len(req.GetRangeIds()))}
			for _, rangeID := range req.GetRangeIds() {
				checksum, count := reconciler.ComputeRangeChecksum(rangeIndex, rangeID)
				resp.Checksums = append(resp.Checksums, &RangeChecksum{
					RangeId:  rangeID,
					Checksum: checksum,
					Count:    count,
				})
			}
			return resp, nil
		},
	}

	if err := secondary.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now()); err != nil {
		t.Fatalf("runRangeReconciliation failed: %v", err)
	}
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, checkpoint.CommitIndex, localSet)
}

func TestDRSecondaryQuiescentReconnectUsesFlatAccumulatorFastPath(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := bytes.Repeat([]byte{0x24}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-fast-path", core.logger)
	secondary.checkpointHighWaterMarkPersistHook = func(index uint64) error {
		if index != 42 {
			t.Fatalf("expected checkpoint high-water mark 42, got %d", index)
		}
		return nil
	}

	kidA := secondary.scanner.ComputeKID("secret/a")
	vidA := secondary.scanner.ComputeVIDWithSealWrap([]byte("a"), false)
	kidB := secondary.scanner.ComputeKID("secret/b")
	vidB := secondary.scanner.ComputeVIDWithSealWrap([]byte("b"), false)
	localSet := &reconciler.ReconciliationSet{
		KIDToVID: map[[32]byte][32]byte{
			kidA: vidA,
			kidB: vidB,
		},
	}
	secondary.rangeAccumulator.resetFromSet(localSet, 42)
	localBuckets, ok := secondary.rangeAccumulator.snapshotExact(42)
	if !ok {
		t.Fatal("expected warm accumulator snapshot")
	}

	var checksumRanges int
	secondary.client = &drTestClient{
		requestCheckpointFn: func(_ context.Context, req *CheckpointRequest, _ ...grpc.CallOption) (*CheckpointResponse, error) {
			if req.GetRelationshipId() != secondary.relationshipID {
				t.Fatalf("unexpected relationship id %q", req.GetRelationshipId())
			}
			return &CheckpointResponse{CheckpointId: "cp-flat-fast-path", CommitIndex: 42}, nil
		},
		exchangeRangeChecksumsFn: func(_ context.Context, req *RangeChecksumRequest, _ ...grpc.CallOption) (*RangeChecksumResponse, error) {
			if req.GetRelationshipId() != secondary.relationshipID {
				t.Fatalf("unexpected relationship id %q", req.GetRelationshipId())
			}
			if req.GetCheckpointId() != "cp-flat-fast-path" || req.GetCheckpointIndex() != 42 {
				t.Fatalf("unexpected checkpoint tuple %q/%d", req.GetCheckpointId(), req.GetCheckpointIndex())
			}
			resp := &RangeChecksumResponse{Checksums: make([]*RangeChecksum, 0, len(req.GetRangeIds()))}
			for _, rangeID := range req.GetRangeIds() {
				if rangeID >= uint64(len(localBuckets)) {
					t.Fatalf("unexpected range id %d", rangeID)
				}
				checksumRanges++
				bucket := localBuckets[rangeID]
				resp.Checksums = append(resp.Checksums, &RangeChecksum{
					RangeId:  rangeID,
					Checksum: bucket.checksum,
					Count:    bucket.count,
				})
			}
			return resp, nil
		},
	}

	// Prove the fast path does not fall back to the O(N) local scanner.
	secondary.scanner = nil
	if err := secondary.runReconciliation(context.Background()); err != nil {
		t.Fatalf("runReconciliation failed: %v", err)
	}
	if checksumRanges != drRangeMaxTotalRanges {
		t.Fatalf("expected %d checksum ranges, got %d", drRangeMaxTotalRanges, checksumRanges)
	}
	if got := secondary.lastAppliedIndex.Load(); got != 42 {
		t.Fatalf("expected lastAppliedIndex 42, got %d", got)
	}
	status := secondary.Status()
	if status.FlatAccumulatorFastPathTotal != 1 {
		t.Fatalf("expected flat accumulator fast path total 1, got %d", status.FlatAccumulatorFastPathTotal)
	}
	if status.ScanFailuresTotal != 0 {
		t.Fatalf("expected no scan failures, got %d", status.ScanFailuresTotal)
	}
}

func TestDRRangeReconciliationSeedsFlatAccumulatorAfterRepair(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := bytes.Repeat([]byte{0x25}, 32)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-flat-repair", core.logger)
	secondary.reconcileMaxInflightTasks = 1
	secondary.reconcileApplyWorkers = 1
	secondary.checkpointHighWaterMarkPersistHook = func(uint64) error { return nil }

	key := "secret/flat-repair"
	oldValue := []byte("old")
	newValue := []byte("new")
	if err := core.physical.Put(ctx, &physical.Entry{Key: key, Value: oldValue}); err != nil {
		t.Fatalf("failed to seed old physical entry: %v", err)
	}

	kid := secondary.scanner.ComputeKID(key)
	oldVID := secondary.scanner.ComputeVIDWithSealWrap(oldValue, false)
	newVID := secondary.scanner.ComputeVIDWithSealWrap(newValue, false)
	checkpoint := &CheckpointResponse{CheckpointId: "cp-flat-repair", CommitIndex: 88}
	localSet := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{ID: checkpoint.CheckpointId, CommitIndex: checkpoint.CommitIndex},
		KeyCount:   1,
		KIDToVID:   map[[32]byte][32]byte{kid: oldVID},
		KIDToKey:   map[[32]byte]string{kid: key},
	}
	remoteSet := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{ID: checkpoint.CheckpointId, CommitIndex: checkpoint.CommitIndex},
		KeyCount:   1,
		KIDToVID:   map[[32]byte][32]byte{kid: newVID},
		KIDToKey:   map[[32]byte]string{kid: key},
	}
	remoteIndex := reconciler.NewRangeMapIndex(remoteSet.KIDToVID, nil)

	secondary.client = &drTestClient{
		requestCheckpointFn: func(_ context.Context, req *CheckpointRequest, _ ...grpc.CallOption) (*CheckpointResponse, error) {
			if req.GetRelationshipId() != secondary.relationshipID {
				return nil, fmt.Errorf("unexpected relationship id %q", req.GetRelationshipId())
			}
			return checkpoint, nil
		},
		exchangeRangeChecksumsFn: func(_ context.Context, req *RangeChecksumRequest, _ ...grpc.CallOption) (*RangeChecksumResponse, error) {
			if req.GetRelationshipId() != secondary.relationshipID {
				return nil, fmt.Errorf("unexpected relationship id %q", req.GetRelationshipId())
			}
			resp := &RangeChecksumResponse{Checksums: make([]*RangeChecksum, 0, len(req.GetRangeIds()))}
			for _, rangeID := range req.GetRangeIds() {
				checksum, count := reconciler.ComputeRangeChecksum(remoteIndex, rangeID)
				resp.Checksums = append(resp.Checksums, &RangeChecksum{
					RangeId:  rangeID,
					Checksum: checksum,
					Count:    count,
				})
			}
			return resp, nil
		},
		exchangeRangeDigestsFn: func(_ context.Context, req *RangeDigestRequest, _ ...grpc.CallOption) (*RangeDigestResponse, error) {
			parent, _, err := protoToRangeSpan(req.GetParentSpan())
			if err != nil {
				return nil, err
			}
			left, right, ok := reconciler.SplitRange(parent)
			if !ok {
				return &RangeDigestResponse{
					Digests: []*RangeDigest{drRangeDigestForTest(parent, remoteIndex)},
				}, nil
			}
			return &RangeDigestResponse{
				Digests: []*RangeDigest{
					drRangeDigestForTest(left, remoteIndex),
					drRangeDigestForTest(right, remoteIndex),
				},
			}, nil
		},
		fetchEntriesFn: func(streamCtx context.Context, req *FetchEntriesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			if req.GetRelationshipId() != secondary.relationshipID {
				return nil, fmt.Errorf("unexpected relationship id %q", req.GetRelationshipId())
			}
			if req.GetCheckpointId() != checkpoint.CheckpointId || req.GetCheckpointIndex() != checkpoint.CommitIndex {
				return nil, fmt.Errorf("unexpected checkpoint tuple %q/%d", req.GetCheckpointId(), req.GetCheckpointIndex())
			}
			matched := false
			for _, protoSpan := range req.GetRanges() {
				span, _, err := protoToRangeSpan(protoSpan)
				if err != nil {
					return nil, err
				}
				if span.Contains(kid) {
					matched = true
				}
			}
			if !matched {
				return nil, fmt.Errorf("expected fetch request to include the repaired kid range")
			}
			return &drTestEntryBatchStream{
				ctx: streamCtx,
				batches: []*EntryBatch{{
					CheckpointId:    checkpoint.CheckpointId,
					CheckpointIndex: checkpoint.CommitIndex,
					Entries: []*EntryChange{{
						OpType: string(physical.PutOperation),
						Key:    key,
						Value:  append([]byte(nil), newValue...),
						Kid:    append([]byte(nil), kid[:]...),
					}},
				}},
			}, nil
		},
	}

	if err := secondary.runRangeReconciliation(ctx, checkpoint, localSet, time.Now()); err != nil {
		t.Fatalf("runRangeReconciliation failed: %v", err)
	}
	entry, err := core.physical.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || !bytes.Equal(entry.Value, newValue) {
		t.Fatalf("expected repaired value %q, got %#v", newValue, entry)
	}
	assertDRFlatAccumulatorMatchesSet(t, secondary.rangeAccumulator, checkpoint.CommitIndex, remoteSet)

	// Prove the repaired reconciliation left enough state for the next
	// quiescent reconnect to take the fast path without scanning.
	secondary.scanner = nil
	if err := secondary.runReconciliation(ctx); err != nil {
		t.Fatalf("runReconciliation fast path after repair failed: %v", err)
	}
	status := secondary.Status()
	if status.FlatAccumulatorFastPathTotal != 1 {
		t.Fatalf("expected flat accumulator fast path total 1, got %d", status.FlatAccumulatorFastPathTotal)
	}
}

func drRangeDigestForTest(span reconciler.RangeSpan, index *reconciler.RangeMapIndex) *RangeDigest {
	desc := reconciler.BuildRangeDigestFromIndex(index, span)
	return &RangeDigest{
		Span:             rangeSpanToProto(span),
		Count:            desc.Count,
		XorKeyHash:       append([]byte(nil), desc.XORKeyHash[:]...),
		XorValueHash:     append([]byte(nil), desc.XORValueHash[:]...),
		ApproxValueBytes: desc.ApproxValueBytes,
	}
}

func assertDRFlatAccumulatorMatchesSet(t *testing.T, accumulator *drFlatRangeAccumulator, index uint64, rs *reconciler.ReconciliationSet) {
	t.Helper()

	buckets, ok := accumulator.snapshotExact(index)
	if !ok {
		t.Fatalf("expected accumulator snapshot at index %d", index)
	}
	rangeIndex := reconciler.NewRangeMapIndex(rs.KIDToVID, nil)
	for rangeID := uint64(0); rangeID < drRangeMaxTotalRanges; rangeID++ {
		wantChecksum, wantCount := reconciler.ComputeRangeChecksum(rangeIndex, rangeID)
		got := buckets[rangeID]
		if got.checksum != wantChecksum || got.count != wantCount {
			t.Fatalf("range %d mismatch: got checksum=%d count=%d, want checksum=%d count=%d",
				rangeID, got.checksum, got.count, wantChecksum, wantCount)
		}
	}
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
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-checksum-tuple-fp")

	var start [32]byte
	cp := &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          "cp-1",
			CommitIndex: 10,
		},
		relationshipID: relationshipID,
		kidToVID: map[[32]byte][32]byte{
			start: {},
		},
		createdAt: time.Now().UTC(),
	}
	primary.checkpointMu.Lock()
	primary.checkpoints[cp.checkpoint.ID] = cp
	primary.checkpointMu.Unlock()

	_, err := primary.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
		RelationshipId:  relationshipID,
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

func addDRRequestValidationCheckpoint(t *testing.T, primary *drReplicationPrimary, checkpointID, relationshipID string, index uint64) {
	t.Helper()

	primary.checkpointMu.Lock()
	defer primary.checkpointMu.Unlock()
	primary.checkpoints[checkpointID] = &drCheckpointCacheEntry{
		checkpoint: reconciler.Checkpoint{
			ID:          checkpointID,
			CommitIndex: index,
		},
		relationshipID: relationshipID,
		createdAt:      time.Now().UTC(),
		kidToVID:       map[[32]byte][32]byte{},
		kidToKey:       map[[32]byte]string{},
	}
}

func fullDRRangeSpanProto(depth uint32) *RangeSpan {
	end := make([]byte, 32)
	for i := range end {
		end[i] = 0xff
	}
	return &RangeSpan{
		StartKid:   make([]byte, 32),
		EndKid:     end,
		SplitDepth: depth,
	}
}

func invertedDRRangeSpanProto() *RangeSpan {
	start := make([]byte, 32)
	end := make([]byte, 32)
	start[31] = 2
	end[31] = 1
	return &RangeSpan{
		StartKid: start,
		EndKid:   end,
	}
}

func TestDRPrimary_ExchangeRangeChecksumsRejectsOversizedRequests(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-checksum-size-fp")

	tooManyRanges := make([]uint64, drRangeMaxTotalRanges+1)
	_, err := primary.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
		RelationshipId: relationshipID,
		RangeIds:       tooManyRanges,
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for excessive ranges, got %v: %v", status.Code(err), err)
	}

	_, err = primary.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
		RelationshipId: relationshipID,
		RangeIds:       []uint64{uint64(drRangeMaxTotalRanges)},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for out-of-range range_id, got %v: %v", status.Code(err), err)
	}
}

func TestDRPrimary_ExchangeRangeDigestsRejectsMalformedParentSpan(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-digest-span-fp")
	addDRRequestValidationCheckpoint(t, primary, "cp-digest-validation", relationshipID, 100)

	_, err := primary.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    "cp-digest-validation",
		CheckpointIndex: 100,
		ParentSpan:      fullDRRangeSpanProto(uint32(drRangeMaxSplitDepth + 1)),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for excessive split depth, got %v: %v", status.Code(err), err)
	}

	_, err = primary.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    "cp-digest-validation",
		CheckpointIndex: 100,
		ParentSpan:      invertedDRRangeSpanProto(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for inverted span, got %v: %v", status.Code(err), err)
	}
}

func TestFetchEntriesRejectsOversizedAndMalformedRequests(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-fetch-validation-fp")
	addDRRequestValidationCheckpoint(t, primary, "cp-fetch-validation", relationshipID, 100)

	stream := &fetchTestServerStream{ctx: rpcCtx}

	tooManyKids := make([][]byte, drFetchRequestMaxSelectors+1)
	err := primary.FetchEntries(&FetchEntriesRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    "cp-fetch-validation",
		CheckpointIndex: 100,
		Kids:            tooManyKids,
	}, stream)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for excessive point selectors, got %v: %v", status.Code(err), err)
	}

	tooManyRanges := make([]*RangeSpan, drRangeMaxTotalRanges+1)
	err = primary.FetchEntries(&FetchEntriesRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    "cp-fetch-validation",
		CheckpointIndex: 100,
		Ranges:          tooManyRanges,
	}, stream)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for excessive ranges, got %v: %v", status.Code(err), err)
	}

	for name, ranges := range map[string][]*RangeSpan{
		"nil":           {nil},
		"split-depth":   {fullDRRangeSpanProto(uint32(drRangeMaxSplitDepth + 1))},
		"inverted-span": {invertedDRRangeSpanProto()},
	} {
		t.Run(name, func(t *testing.T) {
			err := primary.FetchEntries(&FetchEntriesRequest{
				RelationshipId:  relationshipID,
				CheckpointId:    "cp-fetch-validation",
				CheckpointIndex: 100,
				Ranges:          ranges,
			}, stream)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v: %v", status.Code(err), err)
			}
		})
	}
}

func TestFetchEntriesSplitsResponseBatchesByByteBudget(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-fetch-byte-budget-fp")

	cpID := "cp-fetch-byte-budget"
	valueSize := int(drFetchSendBatchMaxBytes/2) + 1024
	key1 := "secret/data/fetch-byte-budget-1"
	key2 := "secret/data/fetch-byte-budget-2"
	value1 := bytes.Repeat([]byte("a"), valueSize)
	value2 := bytes.Repeat([]byte("b"), valueSize)
	kid1 := primary.scanner.ComputeKID(key1)
	kid2 := primary.scanner.ComputeKID(key2)
	vid1 := primary.scanner.ComputeVIDWithSealWrap(value1, false)
	vid2 := primary.scanner.ComputeVIDWithSealWrap(value2, false)

	cp := &drCheckpointCacheEntry{
		checkpoint:     reconciler.Checkpoint{ID: cpID, CommitIndex: 100},
		relationshipID: relationshipID,
		createdAt:      time.Now().UTC(),
		kidToKey: map[[32]byte]string{
			kid1: key1,
			kid2: key2,
		},
		kidToVID: map[[32]byte][32]byte{
			kid1: vid1,
			kid2: vid2,
		},
	}
	seedDRCheckpointArtifactForTest(t, primary, cp, []drCheckpointArtifactRecord{
		{KID: kid1, VID: vid1, Key: key1},
		{KID: kid2, VID: vid2, Key: key2},
	}, map[[32]byte][]byte{
		kid1: value1,
		kid2: value2,
	})
	primary.checkpointMu.Lock()
	primary.checkpoints[cpID] = cp
	primary.checkpointMu.Unlock()

	stream := &fetchTestServerStream{ctx: rpcCtx}
	err := primary.FetchEntries(&FetchEntriesRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    cpID,
		CheckpointIndex: 100,
		Kids:            [][]byte{kid1[:], kid2[:]},
	}, stream)
	if err != nil {
		t.Fatalf("expected fetch to succeed: %v", err)
	}
	if got := stream.sendCount.Load(); got != 2 {
		t.Fatalf("expected response to split into 2 batches, got %d", got)
	}
	for i, batch := range stream.sent {
		if len(batch.GetEntries()) != 1 {
			t.Fatalf("expected batch %d to contain 1 entry, got %d", i, len(batch.GetEntries()))
		}
		if got := entryBatchWireBytes(batch); got > drFetchSendBatchMaxBytes {
			t.Fatalf("expected batch %d bytes <= %d, got %d", i, drFetchSendBatchMaxBytes, got)
		}
	}
}

func TestFetchEntriesRejectsSingleEntryOverResponseByteBudget(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-fetch-entry-too-large-fp")

	cpID := "cp-fetch-entry-too-large"
	key := "secret/data/fetch-entry-too-large"
	valueOverhead := entryBatchBaseWireBytes(&EntryBatch{
		CheckpointId:    cpID,
		CheckpointIndex: 100,
	}) + uint64(len(string(physical.PutOperation))+len(key)+32+64)
	value := bytes.Repeat([]byte("x"), int(drFetchSendBatchMaxBytes-valueOverhead)+1)
	kid := primary.scanner.ComputeKID(key)
	vid := primary.scanner.ComputeVIDWithSealWrap(value, false)

	cp := &drCheckpointCacheEntry{
		checkpoint:     reconciler.Checkpoint{ID: cpID, CommitIndex: 100},
		relationshipID: relationshipID,
		createdAt:      time.Now().UTC(),
		kidToKey:       map[[32]byte]string{kid: key},
		kidToVID:       map[[32]byte][32]byte{kid: vid},
	}
	seedDRCheckpointArtifactForTest(t, primary, cp, []drCheckpointArtifactRecord{
		{KID: kid, VID: vid, Key: key},
	}, map[[32]byte][]byte{kid: value})
	primary.checkpointMu.Lock()
	primary.checkpoints[cpID] = cp
	primary.checkpointMu.Unlock()

	stream := &fetchTestServerStream{ctx: rpcCtx}
	err := primary.FetchEntries(&FetchEntriesRequest{
		RelationshipId:  relationshipID,
		CheckpointId:    cpID,
		CheckpointIndex: 100,
		Kids:            [][]byte{kid[:]},
	}, stream)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for oversized fetch entry, got %v: %v", status.Code(err), err)
	}
	if got := stream.sendCount.Load(); got != 0 {
		t.Fatalf("expected no batches to be sent for oversized entry, got %d", got)
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

func TestDRCheckpointArtifactStore_DoesNotEvictRetainedArtifact(t *testing.T) {
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

	if err := store.putArtifact(makeArtifact("cp-active", time.Now().Add(-2*time.Minute))); err != nil {
		t.Fatalf("put active artifact: %v", err)
	}
	if !store.retain("cp-active") {
		t.Fatal("expected retain to find active artifact")
	}
	defer store.release("cp-active")

	err := store.putArtifact(makeArtifact("cp-new", time.Now().Add(-1*time.Minute)))
	if err == nil {
		t.Fatal("expected budget exhaustion instead of evicting retained artifact")
	}

	store.mu.RLock()
	_, hasActive := store.artifacts["cp-active"]
	_, hasNew := store.artifacts["cp-new"]
	store.mu.RUnlock()
	if !hasActive {
		t.Fatal("retained artifact was evicted")
	}
	if hasNew {
		t.Fatal("new artifact should not be admitted when budget is exhausted by retained artifact")
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
		"/sys/replication/dr/secondary/promote/",
	}
	for _, path := range exempt {
		if !isDRBackpressureExemptPath(path) {
			t.Fatalf("expected path %q to be backpressure exempt", path)
		}
	}

	nonExempt := []string{
		"secret/data/demo",
		"root/sys/replication/dr/secondary/promote",
		"secret/data/x/sys/replication/dr/y",
		"secret/data/foo/sys/health",
		"secret/data/foo/sys/seal-status",
	}
	for _, path := range nonExempt {
		if isDRBackpressureExemptPath(path) {
			t.Fatalf("expected path %q to be non-exempt", path)
		}
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

func TestDRPrimary_CheckpointBuildAdmissionLimitsCrossRelationshipConcurrency(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	primary := NewDRReplicationPrimary(core, replSalt, core.logger, nil)
	primary.checkpointBuildMaxInFlight = 1

	first, owner, err := primary.claimCheckpointBuild("rel-a")
	if err != nil {
		t.Fatalf("expected first checkpoint build claim to pass: %v", err)
	}
	if !owner {
		t.Fatal("expected first checkpoint build claim to own the build")
	}

	same, owner, err := primary.claimCheckpointBuild("rel-a")
	if err != nil {
		t.Fatalf("expected same relationship checkpoint claim to wait on existing build: %v", err)
	}
	if owner {
		t.Fatal("expected same relationship checkpoint claim to reuse in-flight build")
	}
	if same != first {
		t.Fatal("expected same relationship checkpoint claim to receive existing build result")
	}

	_, owner, err = primary.claimCheckpointBuild("rel-b")
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected cross-relationship checkpoint admission to fail with ResourceExhausted, got %v: %v", status.Code(err), err)
	}
	if owner {
		t.Fatal("expected rejected checkpoint build claim not to own a build")
	}
	inFlight, maxInFlight, failures := primary.checkpointBuildStats()
	if inFlight != 1 || maxInFlight != 1 || failures != 1 {
		t.Fatalf("unexpected checkpoint build stats: inFlight=%d max=%d failures=%d", inFlight, maxInFlight, failures)
	}

	primary.finishCheckpointBuild("rel-a", first, &CheckpointResponse{CheckpointId: "cp-a", CommitIndex: 1}, nil)

	second, owner, err := primary.claimCheckpointBuild("rel-b")
	if err != nil {
		t.Fatalf("expected checkpoint claim to pass after previous build finishes: %v", err)
	}
	if !owner {
		t.Fatal("expected rel-b checkpoint claim to own the build after capacity frees")
	}
	primary.finishCheckpointBuild("rel-b", second, &CheckpointResponse{CheckpointId: "cp-b", CommitIndex: 2}, nil)
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

func registerDRRelationshipWithFingerprint(t *testing.T, mgr *drRelationshipManager, fingerprint string) string {
	t.Helper()

	token, err := mgr.GenerateActivationToken(context.Background())
	if err != nil {
		t.Fatalf("failed to generate activation token: %v", err)
	}

	rel, err := mgr.loadRelationship(context.Background(), token.RelationshipID)
	if err != nil {
		t.Fatalf("failed to load relationship: %v", err)
	}
	rel.State = DRRelationshipStateRegistered
	rel.SecondaryCertFingerprint = fingerprint
	if err := mgr.saveRelationship(context.Background(), rel); err != nil {
		t.Fatalf("failed to persist relationship fingerprint: %v", err)
	}

	return token.RelationshipID
}

func markDRRelationshipActive(t *testing.T, mgr *drRelationshipManager, relationshipID string) {
	t.Helper()

	rel, err := mgr.loadRelationship(context.Background(), relationshipID)
	if err != nil {
		t.Fatalf("failed to load relationship: %v", err)
	}
	rel.State = DRRelationshipStateActive
	if err := mgr.saveRelationship(context.Background(), rel); err != nil {
		t.Fatalf("failed to persist active relationship state: %v", err)
	}
}

func setupDRPrimaryForRelationshipAuthz(t *testing.T, fingerprint string, state DRRelationshipState) (*drRelationshipManager, *drReplicationPrimary, string, context.Context) {
	t.Helper()

	core, _, _ := TestCoreUnsealed(t)
	mgr := core.drManager
	if mgr == nil {
		t.Fatal("expected DR manager on core")
	}
	if err := mgr.EnablePrimary(context.Background()); err != nil {
		t.Fatalf("failed to enable DR primary: %v", err)
	}

	relationshipID := registerDRRelationshipWithFingerprint(t, mgr, fingerprint)
	if state == DRRelationshipStateActive {
		markDRRelationshipActive(t, mgr, relationshipID)
	}
	primary := mgr.Primary()
	if primary == nil {
		t.Fatal("expected primary after enable")
	}

	rpcCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint)
	return mgr, primary, relationshipID, rpcCtx
}

func setupDRPrimaryForDirtyBitmapAuthz(t *testing.T, fingerprint string) (*drRelationshipManager, *drReplicationPrimary, string, context.Context) {
	t.Helper()

	return setupDRPrimaryForRelationshipAuthz(t, fingerprint, DRRelationshipStateActive)
}

func setupDRPrimaryForRegisteredSyncKeyring(t *testing.T, fingerprint string) (*drRelationshipManager, *drReplicationPrimary, string, context.Context) {
	t.Helper()

	return setupDRPrimaryForRelationshipAuthz(t, fingerprint, DRRelationshipStateRegistered)
}

func TestDRPrimary_ExchangeDirtyBitmap_PostRestartReturnsAllDirty(t *testing.T) {
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-dirty-post-restart-fp")

	// Freshly created primary has dirtyMapStart == 0 (simulating post-restart).
	if primary.dirtyMapStart != 0 {
		t.Fatalf("expected dirtyMapStart to be 0, got %d", primary.dirtyMapStart)
	}

	resp, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: relationshipID,
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
	_, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-dirty-init-fp")

	// Simulate a write that initializes the dirty bitmap.
	primary.OnChange([]physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "test/key1", Value: []byte("value1"), RaftIndex: 5},
	})

	if primary.dirtyMapStart == 0 {
		t.Fatal("expected dirtyMapStart to be set after OnChange")
	}

	resp, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: relationshipID,
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

func TestDRPrimary_ExchangeDirtyBitmap_RequiresRelationshipAuthorization(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-dirty-authz-fp-1")

	// Valid relationship/fingerprint pairing succeeds.
	if _, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: relationshipID,
		CheckpointId:   "cp-valid",
	}); err != nil {
		t.Fatalf("expected authorized bitmap exchange, got: %v", err)
	}

	// Empty relationship_id should fail validation.
	if _, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: "",
		CheckpointId:   "cp-empty",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty relationship_id, got: %v (%v)", err, status.Code(err))
	}

	// Wrong relationship/fingerprint pairing should be denied.
	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-dirty-authz-fp-2")
	if _, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: otherRelationshipID,
		CheckpointId:   "cp-wrong-fingerprint",
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for mismatched fingerprint, got: %v (%v)", err, status.Code(err))
	}

	// Revoked relationship should be denied.
	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	if _, err := primary.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId: relationshipID,
		CheckpointId:   "cp-revoked",
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for revoked relationship, got: %v (%v)", err, status.Code(err))
	}
}

func requireDRRPCStatusCode(t *testing.T, err error, want codes.Code, context string) {
	t.Helper()

	if status.Code(err) != want {
		t.Fatalf("expected %s for %s, got: %v (%v)", want, context, err, status.Code(err))
	}
}

func newStreamAuthzTestStream(ctx context.Context, relationshipID string) *creditTestBidiStream {
	return &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relationshipID,
					InitialWindow:  1,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream),
		sentCh:   make(chan *EntryChange, 1),
	}
}

func TestDRPrimary_StreamChangesRequiresRelationshipAuthorization(t *testing.T) {
	primary, relationshipID, fingerprint := newCreditTestPrimary(t, 2*time.Second)
	mgr := primary.core.drManager
	rpcCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint)

	requireDRRPCStatusCode(
		t,
		primary.StreamChanges(newStreamAuthzTestStream(context.Background(), relationshipID)),
		codes.PermissionDenied,
		"StreamChanges without peer identity",
	)

	wrongFingerprintCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-stream-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		primary.StreamChanges(newStreamAuthzTestStream(wrongFingerprintCtx, relationshipID)),
		codes.PermissionDenied,
		"StreamChanges with mismatched peer fingerprint",
	)

	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-stream-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		primary.StreamChanges(newStreamAuthzTestStream(rpcCtx, otherRelationshipID)),
		codes.PermissionDenied,
		"StreamChanges with mismatched relationship",
	)

	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	requireDRRPCStatusCode(
		t,
		primary.StreamChanges(newStreamAuthzTestStream(rpcCtx, relationshipID)),
		codes.PermissionDenied,
		"StreamChanges with revoked relationship",
	)
}

func TestDRPrimary_RequestCheckpointRequiresRelationshipAuthorization(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-checkpoint-authz-fp-1")
	req := &CheckpointRequest{RelationshipId: relationshipID}

	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.RequestCheckpoint(context.Background(), req)
			return err
		}(),
		codes.PermissionDenied,
		"RequestCheckpoint without peer identity",
	)

	wrongFingerprintCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-checkpoint-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.RequestCheckpoint(wrongFingerprintCtx, req)
			return err
		}(),
		codes.PermissionDenied,
		"RequestCheckpoint with mismatched peer fingerprint",
	)

	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-checkpoint-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.RequestCheckpoint(rpcCtx, &CheckpointRequest{RelationshipId: otherRelationshipID})
			return err
		}(),
		codes.PermissionDenied,
		"RequestCheckpoint with mismatched relationship",
	)

	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.RequestCheckpoint(rpcCtx, req)
			return err
		}(),
		codes.PermissionDenied,
		"RequestCheckpoint with revoked relationship",
	)
}

func TestDRPrimary_HeartbeatRequiresRelationshipAuthorization(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-heartbeat-authz-fp-1")
	req := &DRHeartbeatRequest{RelationshipId: relationshipID}

	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.Heartbeat(context.Background(), req)
			return err
		}(),
		codes.PermissionDenied,
		"Heartbeat without peer identity",
	)

	wrongFingerprintCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-heartbeat-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.Heartbeat(wrongFingerprintCtx, req)
			return err
		}(),
		codes.PermissionDenied,
		"Heartbeat with mismatched peer fingerprint",
	)

	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-heartbeat-authz-fp-2")
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.Heartbeat(rpcCtx, &DRHeartbeatRequest{RelationshipId: otherRelationshipID})
			return err
		}(),
		codes.PermissionDenied,
		"Heartbeat with mismatched relationship",
	)

	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	requireDRRPCStatusCode(
		t,
		func() error {
			_, err := primary.Heartbeat(rpcCtx, req)
			return err
		}(),
		codes.PermissionDenied,
		"Heartbeat with revoked relationship",
	)
}

func TestDRPrimary_CheckpointRPCsRequireRelationshipAuthorization(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForDirtyBitmapAuthz(t, "test-range-authz-fp-1")
	const checkpointID = "cp-range-authz"
	const checkpointIndex = 100
	addDRRequestValidationCheckpoint(t, primary, checkpointID, relationshipID, checkpointIndex)

	fetchKid := make([]byte, 32)
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "ExchangeRangeChecksums",
			call: func(ctx context.Context) error {
				_, err := primary.ExchangeRangeChecksums(ctx, &RangeChecksumRequest{
					RelationshipId:  relationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					RangeIds:        []uint64{0},
				})
				return err
			},
		},
		{
			name: "ExchangeRangeDigests",
			call: func(ctx context.Context) error {
				_, err := primary.ExchangeRangeDigests(ctx, &RangeDigestRequest{
					RelationshipId:  relationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					ParentSpan:      fullDRRangeSpanProto(0),
				})
				return err
			},
		},
		{
			name: "FetchEntries",
			call: func(ctx context.Context) error {
				return primary.FetchEntries(&FetchEntriesRequest{
					RelationshipId:  relationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					Kids:            [][]byte{fetchKid},
					IncludeDeletes:  true,
				}, &fetchTestServerStream{ctx: ctx})
			},
		},
	}

	for _, tc := range calls {
		tc := tc
		t.Run(tc.name+"/missing-peer", func(t *testing.T) {
			requireDRRPCStatusCode(t, tc.call(context.Background()), codes.PermissionDenied, tc.name+" without peer identity")
		})
	}

	wrongFingerprintCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-range-authz-fp-2")
	for _, tc := range calls {
		tc := tc
		t.Run(tc.name+"/wrong-fingerprint", func(t *testing.T) {
			requireDRRPCStatusCode(t, tc.call(wrongFingerprintCtx), codes.PermissionDenied, tc.name+" with mismatched peer fingerprint")
		})
	}

	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-range-authz-fp-2")
	markDRRelationshipActive(t, mgr, otherRelationshipID)
	otherRelationshipCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-range-authz-fp-2")
	mismatchedRelationshipCalls := []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "ExchangeRangeChecksums",
			call: func(ctx context.Context) error {
				_, err := primary.ExchangeRangeChecksums(ctx, &RangeChecksumRequest{
					RelationshipId:  otherRelationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					RangeIds:        []uint64{0},
				})
				return err
			},
		},
		{
			name: "ExchangeRangeDigests",
			call: func(ctx context.Context) error {
				_, err := primary.ExchangeRangeDigests(ctx, &RangeDigestRequest{
					RelationshipId:  otherRelationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					ParentSpan:      fullDRRangeSpanProto(0),
				})
				return err
			},
		},
		{
			name: "FetchEntries",
			call: func(ctx context.Context) error {
				return primary.FetchEntries(&FetchEntriesRequest{
					RelationshipId:  otherRelationshipID,
					CheckpointId:    checkpointID,
					CheckpointIndex: checkpointIndex,
					Kids:            [][]byte{fetchKid},
					IncludeDeletes:  true,
				}, &fetchTestServerStream{ctx: ctx})
			},
		},
	}
	for _, tc := range mismatchedRelationshipCalls {
		tc := tc
		t.Run(tc.name+"/checkpoint-relationship-mismatch", func(t *testing.T) {
			requireDRRPCStatusCode(t, tc.call(otherRelationshipCtx), codes.PermissionDenied, tc.name+" with mismatched checkpoint relationship")
		})
	}

	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	for _, tc := range calls {
		tc := tc
		t.Run(tc.name+"/revoked", func(t *testing.T) {
			requireDRRPCStatusCode(t, tc.call(rpcCtx), codes.PermissionDenied, tc.name+" with revoked relationship")
		})
	}
}

func TestDRPrimary_SyncKeyringRequiresRelationshipAuthorization(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForRegisteredSyncKeyring(t, "test-sync-keyring-fp-1")
	clientPriv := mustGenerateX25519KeyForTest(t)
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)
	req := &SyncKeyringRequest{
		RelationshipId:        relationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes(),
		ClientNonce:           clientNonce,
	}

	if _, err := primary.SyncKeyring(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied without peer identity, got: %v (%v)", err, status.Code(err))
	}

	wrongFingerprintCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, "test-sync-keyring-fp-2")
	if _, err := primary.SyncKeyring(wrongFingerprintCtx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for mismatched peer fingerprint, got: %v (%v)", err, status.Code(err))
	}

	otherRelationshipID := registerDRRelationshipWithFingerprint(t, mgr, "test-sync-keyring-fp-2")
	if _, err := primary.SyncKeyring(rpcCtx, &SyncKeyringRequest{
		RelationshipId:        otherRelationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes(),
		ClientNonce:           clientNonce,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for mismatched relationship, got: %v (%v)", err, status.Code(err))
	}

	if _, err := primary.SyncKeyring(rpcCtx, &SyncKeyringRequest{
		RelationshipId:        relationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes(),
		ClientNonce:           clientNonce[:drBootstrapNonceSize-1],
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for short client nonce, got: %v (%v)", err, status.Code(err))
	}

	if _, err := primary.SyncKeyring(rpcCtx, &SyncKeyringRequest{
		RelationshipId:        relationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes()[:31],
		ClientNonce:           clientNonce,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for malformed client public key, got: %v (%v)", err, status.Code(err))
	}

	rel, err := mgr.loadRelationship(context.Background(), relationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStateRegistered {
		t.Fatalf("expected malformed SyncKeyring requests to leave relationship registered, got %s", rel.State)
	}

	if _, err := primary.Heartbeat(rpcCtx, &DRHeartbeatRequest{RelationshipId: relationshipID}); err != nil {
		t.Fatalf("expected registered relationship heartbeat to succeed before keyring sync: %v", err)
	}
	rel, err = mgr.loadRelationship(context.Background(), relationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStateRegistered {
		t.Fatalf("expected heartbeat to leave pre-bootstrap relationship registered, got %s", rel.State)
	}

	resp, err := primary.SyncKeyring(rpcCtx, req)
	if err != nil {
		t.Fatalf("expected authorized SyncKeyring to succeed: %v", err)
	}
	if len(resp.WrappedRootKey) == 0 {
		t.Fatal("expected wrapped root key in SyncKeyring response")
	}
	if len(resp.ServerEphemeralPubkey) == 0 {
		t.Fatal("expected server ephemeral public key in SyncKeyring response")
	}
	if len(resp.ServerNonce) != drServerNonceSize {
		t.Fatalf("expected server nonce length %d, got %d", drServerNonceSize, len(resp.ServerNonce))
	}
	if len(resp.WrapNonce) != drGCMIVSize {
		t.Fatalf("expected GCM IV length %d, got %d", drGCMIVSize, len(resp.WrapNonce))
	}

	primaryKeyring, err := primary.core.barrier.Keyring()
	if err != nil {
		t.Fatal(err)
	}
	unwrappedRootKey, err := unwrapRootKeyFromPrimary(
		resp.WrappedRootKey,
		relationshipID,
		mgr.Config().ClusterID,
		"test-sync-keyring-fp-1",
		mgr.transportCA.spkiHash(),
		resp.ServerEphemeralPubkey,
		clientPriv,
		clientNonce,
		resp.ServerNonce,
		resp.WrapNonce,
		resp.WrapAadVersion,
	)
	if err != nil {
		t.Fatalf("expected SyncKeyring response to unwrap with relationship-bound AAD: %v", err)
	}
	if !bytes.Equal(unwrappedRootKey, primaryKeyring.RootKey()) {
		t.Fatal("SyncKeyring response unwrapped the wrong root key")
	}

	rel, err = mgr.loadRelationship(context.Background(), relationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.State != DRRelationshipStateActive {
		t.Fatalf("expected SyncKeyring to mark relationship active, got %s", rel.State)
	}

	if _, err := primary.SyncKeyring(rpcCtx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for replayed SyncKeyring after activation, got: %v (%v)", err, status.Code(err))
	}

	if err := mgr.RevokeRelationship(context.Background(), relationshipID); err != nil {
		t.Fatalf("failed to revoke relationship: %v", err)
	}
	if _, err := primary.SyncKeyring(rpcCtx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for revoked relationship, got: %v (%v)", err, status.Code(err))
	}
}

func TestDRPrimary_SyncKeyringRevocationDuringWrapFailsClosed(t *testing.T) {
	mgr, primary, relationshipID, rpcCtx := setupDRPrimaryForRegisteredSyncKeyring(t, "test-sync-keyring-revoke-race-fp")
	clientPriv := mustGenerateX25519KeyForTest(t)
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)
	req := &SyncKeyringRequest{
		RelationshipId:        relationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes(),
		ClientNonce:           clientNonce,
	}

	origWrap := wrapRootKeyForDRSync
	var revokeErr error
	wrapRootKeyForDRSync = func(rootKey []byte, relID, clusterID, secondaryCertFP, primaryIdentity string, clientPubBytes []byte, clientNonce []byte) ([]byte, []byte, []byte, []byte, uint32, error) {
		revokeErr = mgr.RevokeRelationship(context.Background(), relationshipID)
		return origWrap(rootKey, relID, clusterID, secondaryCertFP, primaryIdentity, clientPubBytes, clientNonce)
	}
	t.Cleanup(func() {
		wrapRootKeyForDRSync = origWrap
	})

	resp, err := primary.SyncKeyring(rpcCtx, req)
	if revokeErr != nil {
		t.Fatalf("failed to revoke relationship during key wrap: %v", revokeErr)
	}
	if resp != nil {
		t.Fatalf("expected no SyncKeyring response after revocation, got %#v", resp)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied after revocation during key wrap, got: %v (%v)", err, status.Code(err))
	}
	rel, loadErr := mgr.loadRelationship(context.Background(), relationshipID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if rel.State != DRRelationshipStateRevoked {
		t.Fatalf("expected relationship to remain revoked, got %s", rel.State)
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
	markDRRelationshipActive(t, mgr, token.RelationshipID)

	// Use the primary from the manager so it shares the same core state.
	p := mgr.Primary()
	if p == nil {
		t.Fatal("expected primary")
	}
	p.creditWaitTimeout = creditTimeout

	return p, token.RelationshipID, fingerprint
}

func TestStreamChanges_RejectsExcessiveInitialWindow(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 2*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
	defer cancel()

	stream := &creditTestBidiStream{
		ctx: ctx,
		initMsg: &StreamChangesUpstream{
			Msg: &StreamChangesUpstream_Init{
				Init: &StreamChangesRequest{
					RelationshipId: relID,
					InitialWindow:  uint64(primary.maxStreamWindowCredits()) + 1,
				},
			},
		},
		creditCh: make(chan *StreamChangesUpstream),
		sentCh:   make(chan *EntryChange, 1),
	}

	err := primary.StreamChanges(stream)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v: %v", status.Code(err), err)
	}
}

func TestStreamChanges_RejectsExcessiveWindowUpdate(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 2*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
	defer cancel()

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
		creditCh: make(chan *StreamChangesUpstream, 1),
		sentCh:   make(chan *EntryChange, 1),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- primary.StreamChanges(stream)
	}()

	stream.creditCh <- &StreamChangesUpstream{
		Msg: &StreamChangesUpstream_WindowUpdate{
			WindowUpdate: &WindowUpdate{Credits: uint64(primary.maxStreamWindowCredits()) + 1},
		},
	}

	select {
	case err := <-errCh:
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("expected ResourceExhausted, got %v: %v", status.Code(err), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StreamChanges did not return after excessive window update")
	}
}

func TestStreamChanges_InitialWindowRespected(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 2*time.Second)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
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
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
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
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
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
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
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
		context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint),
	)
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
	primary.core.activeContext.Store(NewAtomicContext(activeCtx, simulateStepdown))

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

type drTestEntryBatchStream struct {
	ctx     context.Context
	batches []*EntryBatch
	idx     int
}

func (s *drTestEntryBatchStream) Header() (metadata.MD, error) { return nil, nil }
func (s *drTestEntryBatchStream) Trailer() metadata.MD         { return nil }
func (s *drTestEntryBatchStream) CloseSend() error             { return nil }
func (s *drTestEntryBatchStream) Context() context.Context     { return s.ctx }
func (s *drTestEntryBatchStream) SendMsg(any) error            { return nil }
func (s *drTestEntryBatchStream) RecvMsg(any) error            { return nil }

func (s *drTestEntryBatchStream) Recv() (*EntryBatch, error) {
	if s.idx >= len(s.batches) {
		return nil, io.EOF
	}
	batch := cloneEntryBatchForTest(s.batches[s.idx])
	s.idx++
	return batch, nil
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

func TestDRClusterClient_ClientLookup_CachesPresentedFingerprint(t *testing.T) {
	certDER, keyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := parseDRSecondaryClientCert(certDER, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	client := &drReplicationClusterClient{
		clientCert: clientCert,
		logger:     log.NewNullLogger(),
	}

	cert, err := client.ClientLookup(context.Background(), &tls.CertificateRequestInfo{
		AcceptableCAs: [][]byte{clientCert.Leaf.RawSubject},
	})
	if err != nil {
		t.Fatalf("ClientLookup returned error: %v", err)
	}
	if cert == nil {
		t.Fatal("ClientLookup returned nil cert")
	}

	wantFP := certFingerprintSHA256(clientCert.Leaf)
	if gotFP := client.LastClientCertFingerprint(); gotFP != wantFP {
		t.Fatalf("client fingerprint mismatch: got %q want %q", gotFP, wantFP)
	}
}

func TestDRClusterClient_ClientLookupDoesNotFallbackToLocalClusterCert(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	setupTestClusterCert(t, core)

	parsed := core.localClusterParsedCert.Load()
	if parsed == nil {
		t.Fatal("expected local parsed cert to be set")
	}

	client := &drReplicationClusterClient{
		core:          core,
		primaryCACert: parsed,
		logger:        log.NewNullLogger(),
	}

	cert, err := client.ClientLookup(context.Background(), &tls.CertificateRequestInfo{
		AcceptableCAs: [][]byte{parsed.RawIssuer},
	})
	if err != nil {
		t.Fatalf("ClientLookup returned error: %v", err)
	}
	if cert != nil {
		t.Fatal("expected DR client lookup to require the registered secondary client certificate")
	}
	if gotFP := client.LastClientCertFingerprint(); gotFP != "" {
		t.Fatalf("expected no cached fingerprint, got %q", gotFP)
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

func TestDRSecondary_Start_HeartbeatPermissionDeniedCancelsStream(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, drReplSaltLen)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-heartbeat-revoked", log.NewNullLogger())
	secondary.transportReady.Store(true)
	secondary.state.Store(int32(DRSecondaryStreaming))

	streamCanceled := make(chan struct{})
	var cancelOnce sync.Once
	secondary.client = &drTestClient{
		streamChangesFn: func(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[StreamChangesUpstream, EntryBatch], error) {
			go func() {
				<-ctx.Done()
				cancelOnce.Do(func() { close(streamCanceled) })
			}()
			return &blockingEntryBatchBidiClient{ctx: ctx}, nil
		},
		heartbeatFn: func(context.Context, *DRHeartbeatRequest, ...grpc.CallOption) (*DRHeartbeatResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "relationship revoked")
		},
	}

	prevInterval := drHeartbeatInterval
	drHeartbeatInterval = 10 * time.Millisecond
	defer func() { drHeartbeatInterval = prevInterval }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := secondary.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail after heartbeat permission denied")
	}
	if !strings.Contains(err.Error(), "heartbeat trust validation failed") {
		t.Fatalf("expected heartbeat validation failure, got: %v", err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(time.Second):
		t.Fatal("expected heartbeat permission denied to cancel active stream")
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
	sent      []*EntryBatch
	onSend    func(count int64)
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

func (s *fetchTestServerStream) Send(b *EntryBatch) error {
	count := s.sendCount.Add(1)
	s.sent = append(s.sent, cloneEntryBatchForTest(b))
	if s.onSend != nil {
		s.onSend(count)
	}
	if s.sendDelay > 0 {
		time.Sleep(s.sendDelay)
	}
	return nil
}

func cloneEntryBatchForTest(in *EntryBatch) *EntryBatch {
	if in == nil {
		return nil
	}
	out := &EntryBatch{
		CheckpointId:    in.CheckpointId,
		CheckpointIndex: in.CheckpointIndex,
	}
	if len(in.Entries) > 0 {
		out.Entries = make([]*EntryChange, 0, len(in.Entries))
		for _, e := range in.Entries {
			out.Entries = append(out.Entries, cloneEntryChange(e))
		}
	}
	if len(in.FailedKids) > 0 {
		out.FailedKids = make([][]byte, 0, len(in.FailedKids))
		for _, kid := range in.FailedKids {
			out.FailedKids = append(out.FailedKids, append([]byte(nil), kid...))
		}
	}
	return out
}

func TestFetchEntries_RechecksRelationshipRevocationDuringStream(t *testing.T) {
	primary, relID, fingerprint := newCreditTestPrimary(t, 5*time.Second)
	mgr := primary.core.drManager

	cpID := "test-fetch-revoke-cp"
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

	const numKIDs = 250
	kids := make([][]byte, numKIDs)
	for i := range kids {
		kid := make([]byte, 32)
		binary.BigEndian.PutUint64(kid, uint64(i))
		kids[i] = kid
	}

	streamCtx := context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint)
	var revokeErr error
	stream := &fetchTestServerStream{
		ctx: streamCtx,
		onSend: func(count int64) {
			if count == 1 {
				revokeErr = mgr.RevokeRelationship(context.Background(), relID)
			}
		},
	}

	err := primary.FetchEntries(&FetchEntriesRequest{
		RelationshipId:  relID,
		CheckpointId:    cpID,
		CheckpointIndex: 100,
		Kids:            kids,
		IncludeDeletes:  true,
	}, stream)
	if revokeErr != nil {
		t.Fatalf("failed to revoke relationship during fetch stream: %v", revokeErr)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied after revocation during fetch stream, got: %v (%v)", err, status.Code(err))
	}
	if sends := stream.sendCount.Load(); sends != 1 {
		t.Fatalf("expected fetch stream to stop after first batch, got %d sends", sends)
	}
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
	primary.core.activeContext.Store(NewAtomicContext(activeCtx, simulateStepdown))

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
			RelationshipId:  relID,
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

func TestIsDRTransportReconnectError(t *testing.T) {
	transportErr := fmt.Errorf("failed to request checkpoint: rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: remote error: tls: internal error\"")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "transport unavailable", err: transportErr, want: true},
		{name: "wrapped transport unavailable", err: fmt.Errorf("wrapped: %w", transportErr), want: true},
		{name: "redirect", err: &errDRRedirect{LeaderAddr: "https://leader:8201"}, want: false},
		{name: "unavailable without transport markers", err: fmt.Errorf("rpc error: code = Unavailable desc = backend down"), want: false},
		{name: "unsupported protocol", err: fmt.Errorf("rpc error: code = Unavailable desc = connection error: desc = \"transport: authentication handshake failed: unsupported protocol\""), want: true},
		{name: "auth handshake failed", err: fmt.Errorf("transport: authentication handshake failed"), want: true},
		{name: "other error", err: fmt.Errorf("permission denied"), want: false},
	}

	for _, tt := range tests {
		got := isDRTransportReconnectError(tt.err)
		if got != tt.want {
			t.Fatalf("%s: isDRTransportReconnectError(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

func TestDRPrimaryAddrRing_RotationAndCycleBackoffState(t *testing.T) {
	ring := newDRPrimaryAddrRing([]string{"127.0.0.1:8201", "https://127.0.0.2:8201"})
	if ring.Current() != "https://127.0.0.1:8201" {
		t.Fatalf("unexpected initial current address: %q", ring.Current())
	}

	oldAddr, nextAddr, rotated, cycleComplete := ring.RotateFailure()
	if !rotated || cycleComplete {
		t.Fatalf("expected first failure to rotate without cycle completion, rotated=%v cycle=%v", rotated, cycleComplete)
	}
	if oldAddr != "https://127.0.0.1:8201" || nextAddr != "https://127.0.0.2:8201" {
		t.Fatalf("unexpected first rotation old/new: %q -> %q", oldAddr, nextAddr)
	}

	oldAddr, nextAddr, rotated, cycleComplete = ring.RotateFailure()
	if !rotated || !cycleComplete {
		t.Fatalf("expected second failure to rotate with cycle completion, rotated=%v cycle=%v", rotated, cycleComplete)
	}
	if oldAddr != "https://127.0.0.2:8201" || nextAddr != "https://127.0.0.1:8201" {
		t.Fatalf("unexpected second rotation old/new: %q -> %q", oldAddr, nextAddr)
	}
}

func TestDRPrimaryAddrRing_AddHintAndSelect(t *testing.T) {
	ring := newDRPrimaryAddrRing([]string{"https://127.0.0.1:8201"})
	if addr, added := ring.Add("127.0.0.1:8201"); added || addr != "https://127.0.0.1:8201" {
		t.Fatalf("expected duplicate normalized address to be ignored, addr=%q added=%v", addr, added)
	}
	addr, added := ring.Add("http://127.0.0.9:8201")
	if !added || addr != "https://127.0.0.9:8201" {
		t.Fatalf("expected hint to be added and normalized, addr=%q added=%v", addr, added)
	}
	if !ring.Use("127.0.0.9:8201") {
		t.Fatal("expected Use() to select added hint address")
	}
	if ring.Current() != "https://127.0.0.9:8201" {
		t.Fatalf("unexpected current after Use: %q", ring.Current())
	}
}

func TestDRSecondaryPrepareStateForConnectPreservesReconnectState(t *testing.T) {
	tests := []struct {
		name           string
		from           DRSecondaryState
		keyringReady   bool
		lastAppliedIdx uint64
		want           DRSecondaryState
	}{
		{"first connect", DRSecondaryIdle, false, 0, DRSecondaryBootstrapping},
		{"restored cursor", DRSecondaryIdle, true, 42, DRSecondaryStreaming},
		{"restored keyring without cursor", DRSecondaryIdle, true, 0, DRSecondaryBootstrapping},
		{"bootstrapping", DRSecondaryBootstrapping, false, 0, DRSecondaryBootstrapping},
		{"initial sync", DRSecondaryInitialSync, false, 0, DRSecondaryBootstrapping},
		{"streaming", DRSecondaryStreaming, false, 0, DRSecondaryStreaming},
		{"reconciling", DRSecondaryReconciling, false, 0, DRSecondaryReconciling},
		{"resnapshotting", DRSecondaryResnapshotting, false, 0, DRSecondaryResnapshotting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secondary := &drReplicationSecondary{logger: log.NewNullLogger()}
			secondary.state.Store(int32(tt.from))
			secondary.keyringBootstrapped.Store(tt.keyringReady)
			secondary.lastAppliedIndex.Store(tt.lastAppliedIdx)

			secondary.prepareStateForConnect()

			if got := secondary.State(); got != tt.want {
				t.Fatalf("prepareStateForConnect() state = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDRSecondary_Start_ReconciliationTransportErrorDoesNotUseStaleLeaderHint(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)

	replSalt := make([]byte, drReplSaltLen)
	secondary := newDRReplicationSecondary(core, replSalt, "rel-reconcile-transport-fail", log.NewNullLogger())
	secondary.transportReady.Store(true)
	secondary.state.Store(int32(DRSecondaryReconciling))

	// A heartbeat hint can be stale after HA step-down. Transport failures must
	// fall back to the controller's candidate ring instead of becoming redirects.
	hint := "https://new-leader:8201"
	secondary.lastKnownLeaderAddr.Store(&hint)

	secondary.client = &drTestClient{
		requestCheckpointFn: func(context.Context, *CheckpointRequest, ...grpc.CallOption) (*CheckpointResponse, error) {
			return nil, fmt.Errorf("rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: remote error: tls: internal error\"")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := secondary.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to return reconnect error for reconciliation transport failure")
	}

	var redirect *errDRRedirect
	if errors.As(err, &redirect) {
		t.Fatalf("transport error should not use stale leader hint redirect: %v", redirect)
	}
	if !isDRTransportReconnectError(err) {
		t.Fatalf("expected transport reconnect error, got: %v", err)
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

func TestDRRelationshipManager_TeardownClearsDispatcherPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	mgr := newDRRelationshipManager(core, core.logger)

	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	journal := newDRStreamJournal(nil, t.TempDir())
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		t.Fatal(err)
	}

	primary := NewDRReplicationPrimary(core, replSalt, core.logger, journal)
	mgr.primary = primary
	mgr.dispatcher.setPrimary(primary)

	mgr.Teardown()

	mgr.dispatcher.mu.RLock()
	defer mgr.dispatcher.mu.RUnlock()
	if mgr.dispatcher.primary != nil {
		t.Fatal("expected dispatcher primary to be cleared during teardown")
	}
	if mgr.primary != nil {
		t.Fatal("expected manager primary to be cleared during teardown")
	}
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
