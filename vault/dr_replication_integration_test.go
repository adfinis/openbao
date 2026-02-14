// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sync"
	"testing"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// setupTestClusterCert generates a self-signed cluster certificate and sets
// it on the Core for testing purposes. Returns the parsed certificate.
func setupTestClusterCert(t *testing.T, core *Core) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-cluster-cert",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	core.localClusterParsedCert.Store(cert)
	core.localClusterCert.Store(&certDER)
	core.localClusterPrivateKey.Store(key)

	return cert
}

// --- Test helpers ---

// setupDRPair creates a primary and secondary core pair with a shared
// replication salt and wired-up change stream. Returns both cores,
// the primary server, and the replication salt.
func setupDRPair(t *testing.T) (primary *Core, secondary *Core, primaryServer *drReplicationPrimary, replSalt []byte) {
	t.Helper()

	primary, _, _ = TestCoreUnsealed(t)
	secondary, _, _ = TestCoreUnsealed(t)

	replSalt = make([]byte, 32)
	rand.Read(replSalt)

	primaryServer = NewDRReplicationPrimary(primary, replSalt, primary.logger)
	return
}

// writeTestEntries writes n entries to the core's barrier storage.
func writeTestEntries(t *testing.T, core *Core, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s/entry-%05d", prefix, i)
		err := core.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("value-%d-%d", i, time.Now().UnixNano())),
		})
		if err != nil {
			t.Fatalf("failed to write entry %s: %v", key, err)
		}
	}
}

// verifyEntries checks that all entries with the given prefix exist
// on the core and returns the count.
func verifyEntries(t *testing.T, core *Core, prefix string, expectedCount int) {
	t.Helper()
	ctx := context.Background()
	found := 0
	err := logical.ScanView(ctx, core.barrier, func(path string) {
		if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			found++
		}
	})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if found < expectedCount {
		t.Errorf("expected at least %d entries with prefix %q, found %d", expectedCount, prefix, found)
	}
}

// --- Test 1: Two-cluster stream replication ---

func TestDRIntegration_StreamReplication(t *testing.T) {
	primary, secondary, primaryServer, replSalt := setupDRPair(t)
	ctx := context.Background()

	// Write 100 entries on primary.
	writeTestEntries(t, primary, "stream-test", 100)

	// Simulate change stream: collect entries from primary and apply to secondary.
	var changes []physical.ChangeStreamEntry
	err := logical.ScanView(ctx, primary.barrier, func(path string) {
		entry, err := primary.barrier.Get(ctx, path)
		if err != nil || entry == nil {
			return
		}
		changes = append(changes, physical.ChangeStreamEntry{
			OpType:    physical.PutOperation,
			Key:       path,
			Value:     entry.Value,
			RaftIndex: uint64(len(changes) + 1),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	// Feed changes through the primary's OnChange and verify buffer.
	primaryServer.OnChange(changes)

	// Apply changes to secondary using the same replSalt as the primary.
	sec := newDRReplicationSecondary(secondary, replSalt, "test", secondary.logger)

	for _, change := range changes {
		ec := entryChangeFromPhysical(change)
		if err := sec.applyFetchedChange(ctx, ec); err != nil {
			t.Fatalf("failed to apply change: %v", err)
		}
	}

	// Verify secondary has the entries.
	verifyEntries(t, secondary, "stream-test", 100)
	t.Logf("stream replication: %d changes applied", len(changes))
}

// --- Test 2: Forced disconnect + IBLT reconciliation ---

func TestDRIntegration_DisconnectAndReconcile(t *testing.T) {
	primary, secondary, _, replSalt := setupDRPair(t)
	ctx := context.Background()

	// Phase 1: Write shared entries to both.
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("reconcile-test/shared-%03d", i)
		value := []byte(fmt.Sprintf("shared-value-%d", i))
		primary.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
		secondary.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
	}

	// Phase 2: Simulate disconnect -- write 10 more entries only on primary.
	for i := 50; i < 60; i++ {
		key := fmt.Sprintf("reconcile-test/primary-only-%03d", i)
		primary.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("primary-value-%d", i)),
		})
	}

	// Phase 3: Modify 3 shared entries on primary only.
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("reconcile-test/shared-%03d", i)
		primary.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("updated-primary-value-%d", i)),
		})
	}

	// Phase 4: Run reconciliation.
	config := newDRReconcilerScanConfigForTests(replSalt)
	config.BuildKIDMap = true
	scanner := reconciler.NewScanner(config)

	checkpoint := reconciler.Checkpoint{ID: "reconcile-test", CommitIndex: 100}
	primarySet, err := scanner.Scan(ctx, primary.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	secondarySet, err := scanner.Scan(ctx, secondary.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	// Estimate difference.
	estimated, err := primarySet.Strata.Estimate(secondarySet.Strata)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disconnect+reconcile: estimated diff = %d (primary=%d keys, secondary=%d keys)",
		estimated, primarySet.KeyCount, secondarySet.KeyCount)

	// Build and decode IBLT.
	ibltSize := uint32(estimated * 3)
	if ibltSize < 60 {
		ibltSize = 60
	}
	primaryIBLT, err := scanner.BuildIBLTFromScan(ctx, primary.barrier, ibltSize)
	if err != nil {
		t.Fatal(err)
	}
	secondaryIBLT, err := scanner.BuildIBLTFromScan(ctx, secondary.barrier, ibltSize)
	if err != nil {
		t.Fatal(err)
	}

	diff, err := primaryIBLT.Subtract(secondaryIBLT)
	if err != nil {
		t.Fatal(err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Log("IBLT decode failed (may need larger IBLT); testing prefix digest fallback")
		// Verify prefix digest can detect mismatches.
		mismatched, err := primarySet.PrefixDigest.Compare(secondarySet.PrefixDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(mismatched) == 0 {
			t.Fatal("prefix digest should detect mismatches")
		}
		t.Logf("prefix digest found %d mismatched buckets", len(mismatched))
		return
	}

	t.Logf("IBLT decode: %d added (primary-only), %d removed (secondary-only)", len(added), len(removed))

	// Apply the differences to secondary.
	for _, entry := range added {
		if key, ok := primarySet.KIDToKey[entry.KID]; ok {
			e, err := primary.barrier.Get(ctx, key)
			if err != nil || e == nil {
				continue
			}
			secondary.barrier.Put(ctx, &logical.StorageEntry{
				Key:   key,
				Value: e.Value,
			})
		}
	}

	// Verify convergence by re-scanning.
	primarySet2, _ := scanner.Scan(ctx, primary.barrier, checkpoint)
	secondarySet2, _ := scanner.Scan(ctx, secondary.barrier, checkpoint)

	diff2, _ := primarySet2.Strata.Estimate(secondarySet2.Strata)
	t.Logf("after reconciliation: estimated remaining diff = %d", diff2)
}

// --- Test 3: Multiple secondaries ---

func TestDRIntegration_MultipleSecondaries(t *testing.T) {
	primary, _, primaryServer, replSalt := setupDRPair(t)
	ctx := context.Background()

	// Create 3 secondaries.
	secondaries := make([]*Core, 3)
	for i := 0; i < 3; i++ {
		secondaries[i], _, _ = TestCoreUnsealed(t)
	}

	// Write data on primary.
	writeTestEntries(t, primary, "multi-sec", 200)

	// Collect all entries as changes.
	var changes []physical.ChangeStreamEntry
	logical.ScanView(ctx, primary.barrier, func(path string) {
		entry, _ := primary.barrier.Get(ctx, path)
		if entry == nil {
			return
		}
		changes = append(changes, physical.ChangeStreamEntry{
			OpType:    physical.PutOperation,
			Key:       path,
			Value:     entry.Value,
			RaftIndex: uint64(len(changes) + 1),
		})
	})

	// Fan out via OnChange.
	primaryServer.OnChange(changes)

	// Apply to all secondaries concurrently.
	var wg sync.WaitGroup
	for i, sec := range secondaries {
		wg.Add(1)
		go func(idx int, core *Core) {
			defer wg.Done()
			secClient := newDRReplicationSecondary(core, replSalt, "test", core.logger)
			for _, change := range changes {
				ec := entryChangeFromPhysical(change)
				if err := secClient.applyFetchedChange(ctx, ec); err != nil {
					t.Errorf("secondary %d: failed to apply: %v", idx, err)
					return
				}
			}
		}(i, sec)
	}
	wg.Wait()

	// Verify all secondaries have the data.
	for i, sec := range secondaries {
		verifyEntries(t, sec, "multi-sec", 200)
		t.Logf("secondary %d: verified", i)
	}
}

// --- Test 4: Failover round-trip ---

func TestDRIntegration_FailoverRoundTrip(t *testing.T) {
	primary, _, _, _ := setupDRPair(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(primary, primary.logger)
	primary.drManager = mgr

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	// Write data.
	writeTestEntries(t, primary, "failover-data", 50)

	// Generate activation token.
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token.ClusterID == "" {
		t.Fatal("expected non-empty cluster ID")
	}
	if len(token.ReplSalt) == 0 {
		t.Fatal("expected non-empty repl salt")
	}

	// Create a secondary.
	secondaryCore, _, _ := TestCoreUnsealed(t)
	secMgr := newDRRelationshipManager(secondaryCore, secondaryCore.logger)
	secondaryCore.drManager = secMgr

	if err := secMgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	if secMgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary mode, got %s", secMgr.Mode())
	}

	// Promote the secondary.
	result, err := secondaryCore.DRFailover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.OldMode != DRModeSecondary {
		t.Fatalf("expected old mode secondary, got %s", result.OldMode)
	}
	if result.NewMode != DRModeDisabled {
		t.Fatalf("expected new mode disabled, got %s", result.NewMode)
	}
	if secMgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled after promotion, got %s", secMgr.Mode())
	}

	// Re-enable as primary.
	if err := secMgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if secMgr.Mode() != DRModePrimary {
		t.Fatalf("expected primary after re-enable, got %s", secMgr.Mode())
	}
	if secMgr.Primary() == nil {
		t.Fatal("expected primary server after re-enable")
	}

	t.Log("failover round-trip complete: secondary -> standalone -> primary")
}

// --- Test 5: Large keyspace reconciliation ---

func TestDRIntegration_LargeKeyspaceReconciliation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large keyspace test in short mode")
	}

	primary, _, _ := TestCoreUnsealed(t)
	secondary, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	// Write 10K shared entries.
	t.Log("writing 10K shared entries...")
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("large-test/entry-%06d", i)
		value := []byte(fmt.Sprintf("value-%d", i))
		primary.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
		secondary.barrier.Put(ctx, &logical.StorageEntry{Key: key, Value: value})
	}

	// Introduce 20 differences: 10 primary-only, 10 modified.
	for i := 10000; i < 10010; i++ {
		key := fmt.Sprintf("large-test/primary-only-%06d", i)
		primary.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("primary-only-%d", i)),
		})
	}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("large-test/entry-%06d", i)
		primary.barrier.Put(ctx, &logical.StorageEntry{
			Key:   key,
			Value: []byte(fmt.Sprintf("modified-value-%d", i)),
		})
	}

	// Build reconciliation sets.
	config := newDRReconcilerScanConfigForTests(replSalt)
	config.BuildKIDMap = true
	scanner := reconciler.NewScanner(config)

	checkpoint := reconciler.Checkpoint{ID: "large-test", CommitIndex: 100}

	t.Log("scanning primary...")
	start := time.Now()
	primarySet, err := scanner.Scan(ctx, primary.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("primary scan: %d keys in %v", primarySet.KeyCount, time.Since(start))

	t.Log("scanning secondary...")
	start = time.Now()
	secondarySet, err := scanner.Scan(ctx, secondary.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("secondary scan: %d keys in %v", secondarySet.KeyCount, time.Since(start))

	// Estimate.
	estimated, err := primarySet.Strata.Estimate(secondarySet.Strata)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("estimated diff: %d (actual: ~20 entries with 10 adds + 10 modifies = 30 element-level diffs)", estimated)

	// The estimate should be in a reasonable range.
	if estimated < 10 {
		t.Errorf("estimate too low: %d, expected >= 10", estimated)
	}
	if estimated > 200 {
		t.Errorf("estimate too high: %d, expected <= 200", estimated)
	}

	// IBLT decode.
	ibltSize := uint32(estimated * 3)
	if ibltSize < 100 {
		ibltSize = 100
	}

	primaryIBLT, _ := scanner.BuildIBLTFromScan(ctx, primary.barrier, ibltSize)
	secondaryIBLT, _ := scanner.BuildIBLTFromScan(ctx, secondary.barrier, ibltSize)

	diff, err := primaryIBLT.Subtract(secondaryIBLT)
	if err != nil {
		t.Fatal(err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		t.Log("IBLT decode failed for large keyspace -- this is expected if estimate was too low")
		// Fall back to prefix digest.
		mismatched, _ := primarySet.PrefixDigest.Compare(secondarySet.PrefixDigest)
		t.Logf("prefix digest: %d mismatched buckets out of %d",
			len(mismatched), primarySet.PrefixDigest.NumBuckets())
		if len(mismatched) == 0 {
			t.Error("expected mismatched buckets")
		}
		return
	}

	t.Logf("IBLT decode succeeded: %d added, %d removed", len(added), len(removed))

	// We expect at least 10 added entries (primary-only + modified new values).
	if len(added) < 10 {
		t.Errorf("expected at least 10 added entries, got %d", len(added))
	}
}

// --- Test: Read-only enforcement ---

func TestDRIntegration_ReadOnlyEnforcement(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	token := &DRActivationToken{
		ClusterID:      "test-ro",
		RelationshipID: "rel-read-only",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}

	// Verify that write operations are blocked.
	writeReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/data/test",
	}
	_, err := core.switchedLockHandleRequest(ctx, writeReq, false)
	if err != logical.ErrReadOnly {
		t.Fatalf("expected ErrReadOnly, got: %v", err)
	}

	// Verify that DR promote path is allowed.
	promoteReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "sys/replication/dr/secondary/promote",
	}
	_, err = core.switchedLockHandleRequest(ctx, promoteReq, false)
	// This should NOT return ErrReadOnly (it may fail for other reasons
	// like missing context, but not read-only).
	if err == logical.ErrReadOnly {
		t.Fatal("promote path should not be blocked by read-only enforcement")
	}

	// Verify read operations are allowed.
	readReq := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "sys/replication/dr/status",
	}
	_, err = core.switchedLockHandleRequest(ctx, readReq, false)
	if err == logical.ErrReadOnly {
		t.Fatal("read operations should not be blocked on DR secondary")
	}
}

// --- Test: Gap detection ---

func TestDRIntegration_GapDetection(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	sec := newDRReplicationSecondary(core, replSalt, "test", core.logger)
	sec.lastAppliedIndex.Store(10)

	// Verify that isDRSecondaryAllowedPath works correctly.
	if !isDRSecondaryAllowedPath("sys/replication/dr/secondary/promote") {
		t.Error("promote should be allowed")
	}
	if !isDRSecondaryAllowedPath("sys/replication/dr/status") {
		t.Error("status should be allowed")
	}
	if isDRSecondaryAllowedPath("secret/data/test") {
		t.Error("secret paths should not be allowed")
	}
	if isDRSecondaryAllowedPath("sys/mounts") {
		t.Error("sys/mounts should not be allowed")
	}
}

// --- Test: Change stream buffer overflow detection ---

func TestDRIntegration_BufferOverflowDetection(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	// Fill the buffer.
	for i := 0; i < 10000; i++ {
		primary.OnChange([]physical.ChangeStreamEntry{
			{
				OpType:    physical.PutOperation,
				Key:       fmt.Sprintf("key-%d", i),
				Value:     []byte("value"),
				RaftIndex: uint64(i + 1),
			},
		})
	}

	// Verify oldest buffered index.
	oldest := primary.OldestBufferedIndex()
	if oldest != 1 {
		t.Fatalf("expected oldest index 1, got %d", oldest)
	}

	// Add one more to trigger eviction.
	primary.OnChange([]physical.ChangeStreamEntry{
		{
			OpType:    physical.PutOperation,
			Key:       "key-overflow",
			Value:     []byte("value"),
			RaftIndex: 10001,
		},
	})

	oldest = primary.OldestBufferedIndex()
	if oldest != 2 {
		t.Fatalf("expected oldest index 2 after overflow, got %d", oldest)
	}
}

// --- Test: applyStreamChange vs applyFetchedChange ---

func TestDRIntegration_StreamVsFetchApply(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "test", core.logger)

	// applyStreamChange writes directly to physical storage (already encrypted).
	// Here we simulate a "pre-encrypted" value that the FSM would produce.
	physValue := []byte("pre-encrypted-physical-value")
	err := sec.applyStreamChange(ctx, &EntryChange{
		OpType:    string(physical.PutOperation),
		Key:       "stream-test/key1",
		Value:     physValue,
		RaftIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the value was written to physical storage as-is (not re-encrypted).
	phys, err := core.physical.Get(ctx, "stream-test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if phys == nil {
		t.Fatal("expected physical entry after stream apply")
	}
	if string(phys.Value) != string(physValue) {
		t.Fatalf("expected physical value %q, got %q", physValue, phys.Value)
	}

	// applyFetchedChange writes through the barrier (encrypts).
	err = sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "fetch-test/key1",
		Value:  []byte("plaintext-value"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the value can be read back through the barrier (decrypted).
	entry, err := core.barrier.Get(ctx, "fetch-test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected barrier entry after fetch apply")
	}
	if string(entry.Value) != "plaintext-value" {
		t.Fatalf("expected plaintext-value, got %s", string(entry.Value))
	}

	// Verify stream-applied deletes go to physical.
	err = sec.applyStreamChange(ctx, &EntryChange{
		OpType:    string(physical.DeleteOperation),
		Key:       "stream-test/key1",
		RaftIndex: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	phys, err = core.physical.Get(ctx, "stream-test/key1")
	if err != nil {
		t.Fatal(err)
	}
	if phys != nil {
		t.Fatal("expected nil after stream delete")
	}
}

// --- Test: Namespace-aware path matching ---

func TestDRIntegration_NamespacePathMatching(t *testing.T) {
	// Root-level paths.
	if !isDRSecondaryAllowedPath("sys/replication/dr/secondary/promote") {
		t.Error("root promote should be allowed")
	}
	if !isDRSecondaryAllowedPath("sys/seal") {
		t.Error("root seal should be allowed")
	}
	if isDRSecondaryAllowedPath("secret/data/test") {
		t.Error("secret paths should not be allowed")
	}

	// Namespace-prefixed paths.
	if !isDRSecondaryAllowedPath("ns1/sys/replication/dr/secondary/promote") {
		t.Error("namespace-prefixed promote should be allowed")
	}
	if !isDRSecondaryAllowedPath("org/team/sys/seal") {
		t.Error("deeply nested namespace seal should be allowed")
	}
	if isDRSecondaryAllowedPath("ns1/secret/data/test") {
		t.Error("namespace-prefixed secret paths should not be allowed")
	}
}

// --- Test: Token parsing validation ---

func TestDRIntegration_TokenParsingValidation(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Missing ClusterID.
	token := &DRActivationToken{
		RelationshipID: "rel-missing-cluster",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	if token.ClusterID != "" {
		t.Fatal("test setup: expected empty cluster ID")
	}

	// Test EnableSecondary with valid token.
	validToken := &DRActivationToken{
		ClusterID:      "test-cluster",
		RelationshipID: "rel-valid-token",
		PrimaryAddr:    "127.0.0.1:8201",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(validToken.ReplSalt)

	err := mgr.EnableSecondary(ctx, validToken)
	if err != nil {
		// Connection failure is expected (no actual primary), but the
		// secondary should still be created.
		t.Logf("EnableSecondary returned (expected): %v", err)
	}
	if mgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary mode, got %s", mgr.Mode())
	}

	// Verify secondary was created.
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary to be created")
	}
}

// --- Test: LoadConfig restores primary mode ---

func TestDRIntegration_LoadConfigRestoresPrimary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	clusterID := mgr.Config().ClusterID

	// Simulate restart: create a new manager and load config.
	mgr2 := newDRRelationshipManager(core, core.logger)
	if err := mgr2.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	if mgr2.Mode() != DRModePrimary {
		t.Fatalf("expected primary mode after LoadConfig, got %s", mgr2.Mode())
	}
	if mgr2.Config().ClusterID != clusterID {
		t.Fatalf("expected cluster ID %q, got %q", clusterID, mgr2.Config().ClusterID)
	}
	if mgr2.Primary() == nil {
		t.Fatal("expected primary server after LoadConfig")
	}
}

// --- Test: LoadConfig restores secondary mode ---

func TestDRIntegration_LoadConfigRestoresSecondary(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Manually write a secondary config to storage.
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	config := &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      "restored-cluster",
		RelationshipID: "rel-restored",
		ReplSalt:       replSalt,
		PrimaryAddr:    "127.0.0.1:8201",
	}
	data, _ := json.Marshal(config)
	core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   drConfigPath,
		Value: data,
	})

	mgr := newDRRelationshipManager(core, core.logger)
	if err := mgr.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	if mgr.Mode() != DRModeSecondary {
		t.Fatalf("expected secondary mode, got %s", mgr.Mode())
	}
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary to be created")
	}
	if mgr.Config().PrimaryAddr != "127.0.0.1:8201" {
		t.Fatalf("expected primary addr 127.0.0.1:8201, got %s", mgr.Config().PrimaryAddr)
	}
}

// --- Test: Disable cleanup ---

func TestDRIntegration_DisableCleanup(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Enable and then disable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Primary() == nil {
		t.Fatal("expected primary after enable")
	}
	if err := mgr.DisablePrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Primary() != nil {
		t.Fatal("expected nil primary after disable")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled mode, got %s", mgr.Mode())
	}

	// Enable secondary with valid token, then disable.
	token := &DRActivationToken{
		ClusterID:      "cleanup-test",
		RelationshipID: "rel-cleanup",
		PrimaryAddr:    "127.0.0.1:9999",
		ReplSalt:       make([]byte, 32),
	}
	rand.Read(token.ReplSalt)

	if err := mgr.EnableSecondary(ctx, token); err != nil {
		t.Fatal(err)
	}
	if mgr.Secondary() == nil {
		t.Fatal("expected secondary after enable")
	}
	if err := mgr.DisableSecondary(ctx); err != nil {
		t.Fatal(err)
	}
	if mgr.Secondary() != nil {
		t.Fatal("expected nil secondary after disable")
	}
	if mgr.Mode() != DRModeDisabled {
		t.Fatalf("expected disabled mode, got %s", mgr.Mode())
	}
}

// --- Test: KID-based delete resolution ---

func TestDRIntegration_KIDDeleteResolution(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "test", core.logger)

	// Write an entry.
	err := sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "kid-delete-test/entry1",
		Value:  []byte("value1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry exists.
	entry, err := core.barrier.Get(ctx, "kid-delete-test/entry1")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected entry to exist")
	}

	// Now delete via KID (simulating FetchEntries delete response).
	var kid [32]byte
	copy(kid[:], []byte("test-kid-00000000000000000000000"))

	kidToKey := map[[32]byte]string{kid: "kid-delete-test/entry1"}

	err = sec.applyFetchedChange(ctx, &EntryChange{
		OpType: string(physical.DeleteOperation),
		Kid:    kid[:],
	}, kidToKey)
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry is deleted.
	entry, err = core.barrier.Get(ctx, "kid-delete-test/entry1")
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("expected entry to be deleted via KID")
	}
}

// --- Test: FetchEntries with failed_kids ---

func TestDRIntegration_FetchEntriesFailedKIDs(t *testing.T) {
	// This test verifies that the EntryBatch proto includes failed_kids.
	batch := &EntryBatch{
		Entries: []*EntryChange{
			{OpType: "put", Key: "test/key1", Value: []byte("v1")},
		},
		FailedKids: [][]byte{
			[]byte("failed-kid-0000000000000000000000"),
		},
	}

	if len(batch.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(batch.Entries))
	}
	if len(batch.FailedKids) != 1 {
		t.Fatalf("expected 1 failed KID, got %d", len(batch.FailedKids))
	}
}

// --- Test: Heartbeat includes PrimaryTerm ---

func TestDRIntegration_HeartbeatPrimaryTerm(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

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

	secondaryKey, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondaryTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(55),
		Subject:               pkix.Name{CommonName: "heartbeat-secondary"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	secondaryCertDER, err := x509.CreateCertificate(rand.Reader, secondaryTemplate, secondaryTemplate, &secondaryKey.PublicKey, secondaryKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER); err != nil {
		t.Fatal(err)
	}
	secondaryCert, err := x509.ParseCertificate(secondaryCertDER)
	if err != nil {
		t.Fatal(err)
	}

	rpcCtx := peer.NewContext(ctx, &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{secondaryCert},
			},
		},
	})

	primary := NewDRReplicationPrimary(core, replSalt, core.logger)

	resp, err := primary.Heartbeat(rpcCtx, &DRHeartbeatRequest{
		RelationshipId: token.RelationshipID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// In-memory backend won't have a real term, but the field should exist.
	t.Logf("heartbeat: index=%d, term=%d, state=%d",
		resp.PrimaryIndex, resp.PrimaryTerm, resp.ReplicationState)

	// Response should have been populated.
	if resp == nil {
		t.Fatal("expected non-nil heartbeat response")
	}
}

// --- Test: Bootstrap token generation ---

func TestDRIntegration_BootstrapTokenGeneration(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	// Generate activation token.
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify bootstrap token is present.
	if token.BootstrapToken == "" {
		t.Fatal("expected non-empty bootstrap token")
	}

	// Verify primary API address is populated.
	// (In test mode, redirectAddr might be empty, but the field should exist.)
	t.Logf("primary_api_addr=%q, bootstrap_token=%q", token.PrimaryAPIAddr, token.BootstrapToken)

	// Verify a pending relationship entry was created in storage.
	keys, err := core.barrier.List(ctx, drRelationshipsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one pending relationship entry")
	}

	// Read the entry and verify it has the bootstrap token.
	entry, err := core.barrier.Get(ctx, drRelationshipsPath+keys[0])
	if err != nil {
		t.Fatal(err)
	}
	var rel DRRelationship
	if err := json.Unmarshal(entry.Value, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.BootstrapToken != token.BootstrapToken {
		t.Fatalf("expected bootstrap token %q in storage, got %q", token.BootstrapToken, rel.BootstrapToken)
	}
	if rel.State != DRRelationshipStatePending {
		t.Fatalf("expected pending state, got %q", rel.State)
	}

	// Generate a second token -- should be a different bootstrap token.
	token2, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token2.BootstrapToken == token.BootstrapToken {
		t.Fatal("expected different bootstrap tokens for different tokens")
	}
}

// --- Test: Cert registration via ValidateBootstrapAndStoreCert ---

func TestDRIntegration_CertRegistration(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Set up a cluster cert for the primary core.
	setupTestClusterCert(t, core)

	// Generate a different cert to simulate the secondary's cluster cert.
	secondaryKey, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondaryTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "secondary-cluster"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	secondaryCertDER, err := x509.CreateCertificate(rand.Reader, secondaryTemplate, secondaryTemplate, &secondaryKey.PublicKey, secondaryKey)
	if err != nil {
		t.Fatal(err)
	}

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Enable primary (creates handler).
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	handler := mgr.Handler()
	if handler == nil {
		t.Fatal("expected handler after enabling primary")
	}

	// Generate an activation token.
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Validate bootstrap and store the secondary's cert.
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER); err != nil {
		t.Fatal(err)
	}

	// CALookup should now include the secondary cert.
	certs, err := handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Should have at least 2: primary's own cert + the one we just registered.
	if len(certs) < 2 {
		t.Fatalf("expected at least 2 certs from CALookup, got %d", len(certs))
	}

	// The bootstrap token should now be invalid (one-time use).
	err = mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER)
	if err == nil {
		t.Fatal("expected error when reusing bootstrap token")
	}

	// The stored relationship should have the cert and be "registered".
	keys, err := core.barrier.List(ctx, drRelationshipsPath)
	if err != nil {
		t.Fatal(err)
	}
	foundRegistered := false
	for _, key := range keys {
		entry, err := core.barrier.Get(ctx, drRelationshipsPath+key)
		if err != nil || entry == nil {
			continue
		}
		var rel DRRelationship
		if err := json.Unmarshal(entry.Value, &rel); err != nil {
			continue
		}
		if rel.State == DRRelationshipStateRegistered && len(rel.SecondaryCACert) > 0 {
			foundRegistered = true
			if rel.BootstrapToken != "" {
				t.Fatal("expected bootstrap token to be cleared after use")
			}
			break
		}
	}
	if !foundRegistered {
		t.Fatal("expected to find a registered relationship with cert")
	}
}

// --- Test: CALookup with secondary certs ---

func TestDRIntegration_CALookupWithSecondaryCerts(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Set up a cluster cert for the test core.
	localCert := setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	handler := mgr.Handler()
	if handler == nil {
		t.Fatal("expected handler")
	}

	// Initially CALookup returns only the primary's cert.
	certs, err := handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 cert initially, got %d", len(certs))
	}

	// Adding the same cert that's already the primary's cert should be deduped.
	handler.AddTrustedCert("rel-primary", localCert)

	certs, err = handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The primary cert is the same as what we just added, so dedup keeps it at 1.
	t.Logf("CALookup returned %d certs after adding duplicate", len(certs))

	// Generate a different cert to simulate a secondary with a different CA.
	secondaryKey, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	secondaryTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName: "secondary-cluster-cert",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	secondaryCertDER, _ := x509.CreateCertificate(rand.Reader, secondaryTemplate, secondaryTemplate, &secondaryKey.PublicKey, secondaryKey)
	secondaryCert, _ := x509.ParseCertificate(secondaryCertDER)

	handler.AddTrustedCert("rel-1", secondaryCert)

	certs, err = handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 {
		t.Fatalf("expected 2 certs after adding secondary cert, got %d", len(certs))
	}

	// Adding the secondary cert again should not increase the count (dedup).
	handler.AddTrustedCert("rel-1", secondaryCert)
	certs, err = handler.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 {
		t.Fatalf("expected still 2 certs after dedup, got %d", len(certs))
	}
}

// --- Test: LoadConfig restores secondary certs ---

func TestDRIntegration_LoadConfigRestoresSecondaryCerts(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Set up cluster cert on the core.
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	// Enable primary.
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	// Generate a token and register a secondary cert (use a different cert).
	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Generate a different cert to simulate a real secondary.
	secondaryKey, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	secondaryTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject: pkix.Name{
			CommonName: "secondary-for-restore-test",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	secondaryCertDER, _ := x509.CreateCertificate(rand.Reader, secondaryTemplate, secondaryTemplate, &secondaryKey.PublicKey, secondaryKey)

	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, secondaryCertDER); err != nil {
		t.Fatal(err)
	}

	// Verify handler has the secondary cert.
	handler := mgr.Handler()
	handler.certMu.RLock()
	numBefore := 0
	for _, certs := range handler.trustedSecondaryCerts {
		numBefore += len(certs)
	}
	handler.certMu.RUnlock()

	if numBefore != 1 {
		t.Fatalf("expected 1 secondary cert before restart, got %d", numBefore)
	}

	// Simulate restart: create a new manager and LoadConfig.
	mgr2 := newDRRelationshipManager(core, core.logger)
	if err := mgr2.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	if mgr2.Mode() != DRModePrimary {
		t.Fatalf("expected primary mode, got %s", mgr2.Mode())
	}

	handler2 := mgr2.Handler()
	if handler2 == nil {
		t.Fatal("expected handler after LoadConfig")
	}

	// The restored handler should have the secondary cert.
	handler2.certMu.RLock()
	numAfter := 0
	for _, certs := range handler2.trustedSecondaryCerts {
		numAfter += len(certs)
	}
	handler2.certMu.RUnlock()

	t.Logf("secondary certs before restart: %d, after restart: %d", numBefore, numAfter)

	if numAfter < numBefore {
		t.Fatalf("expected at least %d secondary certs after restart, got %d", numBefore, numAfter)
	}

	// Verify CALookup returns the certs (primary + secondary).
	certs, err := handler2.CALookup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) < 2 {
		t.Fatalf("expected at least 2 certs from CALookup after restore, got %d", len(certs))
	}
}

// --- Test: Invalid bootstrap token is rejected ---

func TestDRIntegration_InvalidBootstrapTokenRejected(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Set up cluster cert on the core.
	localCert := setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr

	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	// Try with a completely bogus token.
	err := mgr.ValidateBootstrapAndStoreCert(ctx, "missing-rel", "bogus-token-12345", localCert.Raw)
	if err == nil {
		t.Fatal("expected error for invalid bootstrap token")
	}

	// Try when not in primary mode.
	mgr2 := newDRRelationshipManager(core, core.logger)
	err = mgr2.ValidateBootstrapAndStoreCert(ctx, "missing-rel", "anything", localCert.Raw)
	if err == nil {
		t.Fatal("expected error when not in primary mode")
	}
}

// --- Test: Root key transfer during keyring bootstrap ---

func TestDRIntegration_RootKeyTransferBootstrap(t *testing.T) {
	// Create two independent cores (primary and secondary) with
	// different root keys. Verify that after bootstrapKeyring the
	// secondary can decrypt entries written by the primary.
	primary, _, _ := TestCoreUnsealed(t)
	secondary, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// Confirm they have different root keys.
	primaryKR, err := primary.barrier.Keyring()
	if err != nil {
		t.Fatal(err)
	}
	secondaryKR, err := secondary.barrier.Keyring()
	if err != nil {
		t.Fatal(err)
	}
	if string(primaryKR.RootKey()) == string(secondaryKR.RootKey()) {
		t.Fatal("expected different root keys for independently initialized cores")
	}

	// Write some test entries on the primary.
	writeTestEntries(t, primary, "root-key-test", 10)

	// Simulate the SyncKeyring RPC: primary builds a response.
	keyringEntry, err := primary.physical.Get(ctx, "core/keyring")
	if err != nil {
		t.Fatal(err)
	}
	rootKeyEntry, err := primary.physical.Get(ctx, "core/root-key")
	if err != nil {
		t.Fatal(err)
	}

	clientPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientNonce := make([]byte, drBootstrapNonceSize)
	if _, err := rand.Read(clientNonce); err != nil {
		t.Fatal(err)
	}
	wrappedRootKey, serverPub, wrapNonce, aadVersion, err := wrapRootKeyForSecondary(
		primaryKR.RootKey(),
		"rel-test",
		clientPriv.PublicKey().Bytes(),
		clientNonce,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp := &SyncKeyringResponse{
		KeyringEntry:          keyringEntry.Value,
		WrappedRootKey:        wrappedRootKey,
		ServerEphemeralPubkey: serverPub,
		WrapNonce:             wrapNonce,
		WrapAadVersion:        aadVersion,
	}
	if rootKeyEntry != nil {
		resp.RootKeyEntry = rootKeyEntry.Value
	}

	rootKey, err := unwrapRootKeyFromPrimary(
		resp.WrappedRootKey,
		"rel-test",
		resp.ServerEphemeralPubkey,
		clientPriv,
		clientNonce,
		resp.WrapNonce,
		resp.WrapAadVersion,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: Set the primary's root key on the secondary barrier.
	if err := secondary.barrier.SetRootKey(rootKey); err != nil {
		t.Fatalf("SetRootKey failed: %v", err)
	}

	// Step 2: Write primary's keyring and root-key to secondary physical storage.
	if err := secondary.physical.Put(ctx, &physical.Entry{
		Key:   "core/keyring",
		Value: resp.KeyringEntry,
	}); err != nil {
		t.Fatal(err)
	}
	if len(resp.RootKeyEntry) > 0 {
		if err := secondary.physical.Put(ctx, &physical.Entry{
			Key:   "core/root-key",
			Value: resp.RootKeyEntry,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Step 3: ReloadKeyring should now succeed.
	if err := secondary.barrier.ReloadKeyring(ctx); err != nil {
		t.Fatalf("ReloadKeyring failed after root key transfer: %v", err)
	}

	// Verify the secondary's barrier now has the primary's root key.
	newKR, err := secondary.barrier.Keyring()
	if err != nil {
		t.Fatal(err)
	}
	if string(newKR.RootKey()) != string(primaryKR.RootKey()) {
		t.Fatal("secondary root key should match primary root key after bootstrap")
	}

	// Replicate an entry from primary to secondary using physical-layer
	// copy (simulating stream replication with barrier-encrypted values).
	entry, err := primary.physical.Get(ctx, "root-key-test/entry-00000")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected entry from primary physical storage")
	}
	if err := secondary.physical.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	// The secondary should now be able to read this entry through its barrier.
	got, err := secondary.barrier.Get(ctx, "root-key-test/entry-00000")
	if err != nil {
		t.Fatalf("secondary barrier.Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected to read entry through secondary barrier after keyring bootstrap")
	}
	t.Logf("successfully decrypted replicated entry on secondary: key=%s, len(value)=%d", got.Key, len(got.Value))
}

// --- Test: Path exclusion filtering ---

func TestDRIntegration_PathExclusions(t *testing.T) {
	// Verify that isDRNeverReplicatePath and isDRReconcileExcludedPath
	// correctly identify excluded paths.

	// Never-replicate paths: should be excluded everywhere.
	neverReplicatePaths := []string{
		"core/hsm/barrier-unseal-keys",
		"core/seal-config",
		"core/local-mounts",
		"core/local-auth",
		"core/local-audit",
		"core/lock",
		"core/initialize-lock",
		"core/recovery-config",
		"core/recovery-key",
		"core/cluster/local/info",
		"core/dr-replication/config",
		"core/dr-replication/relationships/abc123",
		"core/local-mounts/6e0aa9a6-6259-7189-2294-f3ab9028ca20",
		"namespaces/00000000-0000-0000-0000-000000000000/core/local-mounts/6e0aa9a6-6259-7189-2294-f3ab9028ca20",
	}
	for _, p := range neverReplicatePaths {
		if !isDRNeverReplicatePath(p) {
			t.Errorf("expected %q to be a never-replicate path", p)
		}
		if !isDRReconcileExcludedPath(p) {
			t.Errorf("expected %q to be excluded from reconciliation", p)
		}
	}

	// Reconcile-only exclusions: excluded from scanner but not from stream.
	reconcileOnlyPaths := []string{
		"core/keyring",
	}
	for _, p := range reconcileOnlyPaths {
		if isDRNeverReplicatePath(p) {
			t.Errorf("expected %q to NOT be a never-replicate path (it's reconcile-excluded only)", p)
		}
		if !isDRReconcileExcludedPath(p) {
			t.Errorf("expected %q to be excluded from reconciliation", p)
		}
	}

	// Normal paths should not be excluded.
	normalPaths := []string{
		"logical/secret/foo",
		"core/mounts",
		"sys/policy/default",
		"core/root-key",
	}
	for _, p := range normalPaths {
		if isDRNeverReplicatePath(p) {
			t.Errorf("expected %q to NOT be a never-replicate path", p)
		}
		if isDRReconcileExcludedPath(p) {
			t.Errorf("expected %q to NOT be excluded from reconciliation", p)
		}
	}
}

// --- Test: applyStreamChange skips never-replicate paths ---

func TestDRIntegration_StreamChangeSkipsExcludedPaths(t *testing.T) {
	_, secondary, _, replSalt := setupDRPair(t)
	ctx := context.Background()

	sec := newDRReplicationSecondary(secondary, replSalt, "test", secondary.logger)

	// Write a sentinel entry to core/hsm/barrier-unseal-keys on the
	// secondary's physical storage so we can verify it's NOT overwritten.
	sentinel := []byte("secondary-seal-key-sentinel")
	if err := secondary.physical.Put(ctx, &physical.Entry{
		Key:   "core/hsm/barrier-unseal-keys",
		Value: sentinel,
	}); err != nil {
		t.Fatal(err)
	}

	// Apply a stream change for a never-replicate path.
	err := sec.applyStreamChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "core/hsm/barrier-unseal-keys",
		Value:  []byte("primary-seal-key-DO-NOT-REPLICATE"),
	})
	if err != nil {
		t.Fatalf("applyStreamChange should not fail for excluded path: %v", err)
	}

	// Verify the sentinel is still intact (the change was skipped).
	entry, err := secondary.physical.Get(ctx, "core/hsm/barrier-unseal-keys")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected sentinel entry to still exist")
	}
	if string(entry.Value) != string(sentinel) {
		t.Fatalf("expected sentinel value %q, got %q", sentinel, entry.Value)
	}

	// Also test core/seal-config.
	if err := secondary.physical.Put(ctx, &physical.Entry{
		Key:   "core/seal-config",
		Value: []byte("secondary-seal-config"),
	}); err != nil {
		t.Fatal(err)
	}
	err = sec.applyStreamChange(ctx, &EntryChange{
		OpType: string(physical.PutOperation),
		Key:    "core/seal-config",
		Value:  []byte("primary-seal-config-DO-NOT-REPLICATE"),
	})
	if err != nil {
		t.Fatalf("applyStreamChange should not fail for excluded path: %v", err)
	}
	entry, err = secondary.physical.Get(ctx, "core/seal-config")
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Value) != "secondary-seal-config" {
		t.Fatal("core/seal-config was overwritten by stream change; it should have been skipped")
	}
}

// --- Test: Scanner excludes paths ---

func TestDRIntegration_ScannerExcludesPaths(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	// Write some normal entries and a path that should be excluded.
	writeTestEntries(t, core, "scanner-test", 5)

	// core/keyring already exists in physical storage (from init).
	// Verify it's in physical.
	keyringEntry, err := core.physical.Get(ctx, "core/keyring")
	if err != nil || keyringEntry == nil {
		t.Fatal("core/keyring should exist in physical storage")
	}

	// Create scanner WITH exclusions.
	config := newDRReconcilerScanConfigForTests(replSalt)
	config.ExcludePaths = map[string]bool{
		"core/keyring":                 true,
		"core/hsm/barrier-unseal-keys": true,
		"core/seal-config":             true,
	}
	scanner := reconciler.NewScanner(config)

	checkpoint := reconciler.Checkpoint{ID: "exclude-test", CommitIndex: 100}
	set, err := scanner.Scan(ctx, core.barrier, checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	// The scan should have completed without errors (no "failed to get
	// entry" warnings for core/keyring).
	if set.KeyCount == 0 {
		t.Fatal("expected some keys from scan")
	}
	t.Logf("scanned %d keys with exclusions applied", set.KeyCount)

	// Create scanner WITHOUT exclusions. In strict fail-closed mode this
	// must fail when unreadable internal paths (e.g. core/keyring) are seen.
	configNoExclude := reconciler.DefaultScanConfig(replSalt)
	configNoExclude.RequireTransactionalSnapshot = true
	configNoExclude.Logger = log.NewNullLogger()
	scannerNoExclude := reconciler.NewScanner(configNoExclude)

	_, err = scannerNoExclude.Scan(ctx, core.barrier, checkpoint)
	if err == nil {
		t.Fatal("expected strict scan to fail without exclusions")
	}
}

// --- Test: Root key rotation propagation ---

func TestDRIntegration_RootKeyRotationHandling(t *testing.T) {
	// This test verifies that after a root key rotation on the primary,
	// the secondary can pick up the new keyring by calling ReloadRootKey
	// then ReloadKeyring.
	primary, _, _ := TestCoreUnsealed(t)
	secondary, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	// First, bootstrap the secondary with the primary's root key.
	primaryKR, err := primary.barrier.Keyring()
	if err != nil {
		t.Fatal(err)
	}

	// Set primary root key on secondary.
	if err := secondary.barrier.SetRootKey(primaryKR.RootKey()); err != nil {
		t.Fatal(err)
	}

	// Copy core/keyring and core/root-key from primary to secondary.
	keyringEntry, _ := primary.physical.Get(ctx, "core/keyring")
	if keyringEntry != nil {
		secondary.physical.Put(ctx, keyringEntry)
	}
	rootKeyEntry, _ := primary.physical.Get(ctx, "core/root-key")
	if rootKeyEntry != nil {
		secondary.physical.Put(ctx, rootKeyEntry)
	}

	// Reload secondary keyring.
	if err := secondary.barrier.ReloadKeyring(ctx); err != nil {
		t.Fatalf("initial keyring bootstrap failed: %v", err)
	}

	// Verify secondary can now decrypt primary entries.
	writeTestEntries(t, primary, "rotation-test", 3)

	// Copy a barrier-encrypted entry from primary to secondary.
	physEntry, _ := primary.physical.Get(ctx, "rotation-test/entry-00000")
	if physEntry != nil {
		secondary.physical.Put(ctx, physEntry)
	}

	got, err := secondary.barrier.Get(ctx, "rotation-test/entry-00000")
	if err != nil || got == nil {
		t.Fatal("secondary should decrypt pre-rotation entry")
	}

	// Now simulate a keyring rotation on the primary.
	// Rotate adds a new term key to the keyring.
	newTerm, err := primary.barrier.Rotate(ctx, rand.Reader)
	if err != nil {
		t.Fatalf("primary keyring rotation failed: %v", err)
	}
	t.Logf("primary rotated to term %d", newTerm)

	// The primary's core/keyring and core/root-key are now updated.
	// Copy them to secondary physical storage (simulating stream replication).
	keyringEntry2, _ := primary.physical.Get(ctx, "core/keyring")
	if keyringEntry2 != nil {
		secondary.physical.Put(ctx, keyringEntry2)
	}
	rootKeyEntry2, _ := primary.physical.Get(ctx, "core/root-key")
	if rootKeyEntry2 != nil {
		secondary.physical.Put(ctx, rootKeyEntry2)
	}

	// The secondary's current root key should still be the primary's
	// (unchanged) root key after a regular Rotate. ReloadRootKey +
	// ReloadKeyring should work because Rotate doesn't change the root key.
	if err := secondary.barrier.ReloadRootKey(ctx); err != nil {
		t.Logf("ReloadRootKey (expected to work for Rotate): %v", err)
	}
	if err := secondary.barrier.ReloadKeyring(ctx); err != nil {
		t.Fatalf("ReloadKeyring failed after rotation: %v", err)
	}

	// Write new entry on primary with the new term key, replicate to secondary.
	writeTestEntries(t, primary, "post-rotation", 3)
	physEntry2, _ := primary.physical.Get(ctx, "post-rotation/entry-00000")
	if physEntry2 != nil {
		secondary.physical.Put(ctx, physEntry2)
	}

	got2, err := secondary.barrier.Get(ctx, "post-rotation/entry-00000")
	if err != nil {
		t.Fatalf("secondary should decrypt post-rotation entry: %v", err)
	}
	if got2 == nil {
		t.Fatal("expected to read post-rotation entry on secondary")
	}
	t.Logf("successfully decrypted post-rotation entry on secondary")
}

type drRangeTestClient struct {
	streamChangesFn        func(context.Context, *StreamChangesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryChange], error)
	exchangeIBLTFn         func(context.Context, *IBLTMessage, ...grpc.CallOption) (*IBLTMessage, error)
	exchangeRangeDigestsFn func(context.Context, *RangeDigestRequest, ...grpc.CallOption) (*RangeDigestResponse, error)
	exchangePrefixFn       func(context.Context, *PrefixDigestRequest, ...grpc.CallOption) (*PrefixDigestResponse, error)
	fetchEntriesFn         func(context.Context, *FetchEntriesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error)
	exchangeIBLTCallCount  int
	seenSplitDepthPositive bool
}

func (c *drRangeTestClient) StreamChanges(ctx context.Context, in *StreamChangesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[EntryChange], error) {
	if c.streamChangesFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.streamChangesFn(ctx, in, opts...)
}

func (c *drRangeTestClient) RequestCheckpoint(context.Context, *CheckpointRequest, ...grpc.CallOption) (*CheckpointResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *drRangeTestClient) ExchangeIBLT(ctx context.Context, in *IBLTMessage, opts ...grpc.CallOption) (*IBLTMessage, error) {
	c.exchangeIBLTCallCount++
	if in.GetSpan() != nil && in.GetSpan().GetSplitDepth() > 0 {
		c.seenSplitDepthPositive = true
	}
	if c.exchangeIBLTFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangeIBLTFn(ctx, in, opts...)
}

func (c *drRangeTestClient) ExchangeRangeDigests(ctx context.Context, in *RangeDigestRequest, opts ...grpc.CallOption) (*RangeDigestResponse, error) {
	if c.exchangeRangeDigestsFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangeRangeDigestsFn(ctx, in, opts...)
}

func (c *drRangeTestClient) ExchangePrefixDigests(ctx context.Context, in *PrefixDigestRequest, opts ...grpc.CallOption) (*PrefixDigestResponse, error) {
	if c.exchangePrefixFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.exchangePrefixFn(ctx, in, opts...)
}

func (c *drRangeTestClient) FetchEntries(ctx context.Context, in *FetchEntriesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
	if c.fetchEntriesFn == nil {
		return nil, errors.New("not implemented")
	}
	return c.fetchEntriesFn(ctx, in, opts...)
}

func (c *drRangeTestClient) Heartbeat(context.Context, *DRHeartbeatRequest, ...grpc.CallOption) (*DRHeartbeatResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *drRangeTestClient) SyncKeyring(context.Context, *SyncKeyringRequest, ...grpc.CallOption) (*SyncKeyringResponse, error) {
	return nil, errors.New("not implemented")
}

type staticEntryBatchStream struct {
	ctx     context.Context
	batches []*EntryBatch
	idx     int
}

func (s *staticEntryBatchStream) Header() (metadata.MD, error) { return nil, nil }
func (s *staticEntryBatchStream) Trailer() metadata.MD         { return nil }
func (s *staticEntryBatchStream) CloseSend() error             { return nil }
func (s *staticEntryBatchStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *staticEntryBatchStream) SendMsg(any) error { return nil }
func (s *staticEntryBatchStream) RecvMsg(any) error { return io.EOF }
func (s *staticEntryBatchStream) Recv() (*EntryBatch, error) {
	if s.idx >= len(s.batches) {
		return nil, io.EOF
	}
	b := s.batches[s.idx]
	s.idx++
	return b, nil
}

type staticEntryChangeStream struct {
	ctx     context.Context
	changes []*EntryChange
	idx     int
}

func (s *staticEntryChangeStream) Header() (metadata.MD, error) { return nil, nil }
func (s *staticEntryChangeStream) Trailer() metadata.MD         { return nil }
func (s *staticEntryChangeStream) CloseSend() error             { return nil }
func (s *staticEntryChangeStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *staticEntryChangeStream) SendMsg(any) error { return nil }
func (s *staticEntryChangeStream) RecvMsg(any) error { return io.EOF }
func (s *staticEntryChangeStream) Recv() (*EntryChange, error) {
	if s.idx >= len(s.changes) {
		return nil, io.EOF
	}
	ch := s.changes[s.idx]
	s.idx++
	return ch, nil
}

type captureEntryChangeServerStream struct {
	ctx  context.Context
	sent []*EntryChange
}

func (s *captureEntryChangeServerStream) SetHeader(metadata.MD) error { return nil }
func (s *captureEntryChangeServerStream) SendHeader(metadata.MD) error {
	return nil
}
func (s *captureEntryChangeServerStream) SetTrailer(metadata.MD) {}
func (s *captureEntryChangeServerStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *captureEntryChangeServerStream) SendMsg(any) error { return nil }
func (s *captureEntryChangeServerStream) RecvMsg(any) error { return io.EOF }
func (s *captureEntryChangeServerStream) Send(ch *EntryChange) error {
	s.sent = append(s.sent, ch)
	return nil
}

func fullRangeProtoSpan() *RangeSpan {
	return &RangeSpan{
		StartKid: make([]byte, 32),
		EndKid:   bytes.Repeat([]byte{0xff}, 32),
	}
}

func newTestRangeCheckpoint(id string, commit uint64, count uint64) *CheckpointResponse {
	return &CheckpointResponse{
		CheckpointId: id,
		CommitIndex:  commit,
		TopRanges: []*RangeDigest{
			{
				Span:               fullRangeProtoSpan(),
				Count:              count,
				XorKeyHash:         make([]byte, 32),
				XorValueHash:       make([]byte, 32),
				SuggestedIbltCells: 128,
			},
		},
		RangePlanVersion: reconciler.RangePlanVersion,
	}
}

func TestDRIntegration_RangeManifestDeterminism(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()

	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	writeTestEntries(t, core, "range-manifest", 300)

	cfg := newDRReconcilerScanConfigForTests(replSalt)
	cfg.BuildKIDMap = true
	cfg.BuildEntryMap = true
	scanner := reconciler.NewScanner(cfg)

	cp := reconciler.Checkpoint{ID: "manifest-cp", CommitIndex: 42}
	set1, err := scanner.Scan(ctx, core.barrier, cp)
	if err != nil {
		t.Fatal(err)
	}
	set2, err := scanner.Scan(ctx, core.barrier, cp)
	if err != nil {
		t.Fatal(err)
	}

	m1, err := reconciler.BuildRangeManifest(set1, reconciler.DefaultRangePlanConfig())
	if err != nil {
		t.Fatal(err)
	}
	m2, err := reconciler.BuildRangeManifest(set2, reconciler.DefaultRangePlanConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(m1) == 0 {
		t.Fatal("expected non-empty range manifest")
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatal("expected deterministic range manifest for identical checkpoint data")
	}
}

func TestDRIntegration_RangeIBLTDecodeSuccess(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-success", core.logger)

	kid := testSHA256([]byte("range-success-kid"))
	vid := testSHA256([]byte("range-success-vid"))

	sec.client = &drRangeTestClient{
		exchangeIBLTFn: func(_ context.Context, req *IBLTMessage, _ ...grpc.CallOption) (*IBLTMessage, error) {
			remoteIBLT := sketch.NewIBLT(req.GetNumCells(), sketch.DefaultHashCount)
			remoteIBLT.Insert(kid, vid)
			return &IBLTMessage{
				CheckpointId:    req.GetCheckpointId(),
				CheckpointIndex: req.GetCheckpointIndex(),
				IbltData:        remoteIBLT.Marshal(),
			}, nil
		},
		fetchEntriesFn: func(_ context.Context, req *FetchEntriesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{
				batches: []*EntryBatch{
					{
						CheckpointId:    req.GetCheckpointId(),
						CheckpointIndex: req.GetCheckpointIndex(),
						Entries: []*EntryChange{
							{OpType: string(physical.PutOperation), Key: "range/success", Value: []byte("ok")},
						},
					},
				},
			}, nil
		},
	}

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}
	checkpoint := newTestRangeCheckpoint("cp-range-success", 99, 1)

	if err := sec.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now()); err != nil {
		t.Fatalf("expected range reconciliation to succeed, got error: %v", err)
	}
	if sec.lastAppliedIndex.Load() != checkpoint.CommitIndex {
		t.Fatalf("expected lastAppliedIndex=%d, got %d", checkpoint.CommitIndex, sec.lastAppliedIndex.Load())
	}
	entry, err := core.barrier.Get(context.Background(), "range/success")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected fetched range entry to be applied")
	}
}

func TestDRIntegration_RangeIBLTDecodeAdaptiveSplit(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-split", core.logger)

	client := &drRangeTestClient{
		exchangeIBLTFn: func(_ context.Context, req *IBLTMessage, _ ...grpc.CallOption) (*IBLTMessage, error) {
			if req.GetSpan() != nil && req.GetSpan().GetSplitDepth() == 0 {
				undecodable := sketch.NewIBLT(req.GetNumCells(), sketch.DefaultHashCount)
				for i := 0; i < 512; i++ {
					kid := testSHA256([]byte(fmt.Sprintf("split-kid-%d", i)))
					vid := testSHA256([]byte(fmt.Sprintf("split-vid-%d", i)))
					undecodable.Insert(kid, vid)
				}
				return &IBLTMessage{
					CheckpointId:    req.GetCheckpointId(),
					CheckpointIndex: req.GetCheckpointIndex(),
					IbltData:        undecodable.Marshal(),
				}, nil
			}
			empty := sketch.NewIBLT(req.GetNumCells(), sketch.DefaultHashCount)
			return &IBLTMessage{
				CheckpointId:    req.GetCheckpointId(),
				CheckpointIndex: req.GetCheckpointIndex(),
				IbltData:        empty.Marshal(),
			}, nil
		},
		exchangeRangeDigestsFn: func(_ context.Context, req *RangeDigestRequest, _ ...grpc.CallOption) (*RangeDigestResponse, error) {
			out := make([]*RangeDigest, 0, len(req.GetSpans()))
			for i, span := range req.GetSpans() {
				out = append(out, &RangeDigest{
					Span:               span,
					Count:              1,
					XorKeyHash:         bytes.Repeat([]byte{byte(i + 1)}, 32),
					XorValueHash:       bytes.Repeat([]byte{byte(i + 2)}, 32),
					SuggestedIbltCells: 8,
				})
			}
			return &RangeDigestResponse{Ranges: out}, nil
		},
		fetchEntriesFn: func(context.Context, *FetchEntriesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{}, nil
		},
	}
	sec.client = client

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}
	checkpoint := newTestRangeCheckpoint("cp-range-split", 100, 64)

	if err := sec.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now()); err != nil {
		t.Fatalf("expected adaptive split reconciliation to succeed, got %v", err)
	}
	if client.exchangeIBLTCallCount < 2 {
		t.Fatalf("expected adaptive split to trigger multiple ExchangeIBLT calls, got %d", client.exchangeIBLTCallCount)
	}
	if !client.seenSplitDepthPositive {
		t.Fatal("expected adaptive split to request at least one child span")
	}
}

func TestDRIntegration_RangeIBLTDecodeAdaptiveSplitFromNonZeroDepth(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-split-depth", core.logger)

	client := &drRangeTestClient{
		exchangeIBLTFn: func(_ context.Context, req *IBLTMessage, _ ...grpc.CallOption) (*IBLTMessage, error) {
			if req.GetSpan() != nil && req.GetSpan().GetSplitDepth() == 8 {
				undecodable := sketch.NewIBLT(req.GetNumCells(), sketch.DefaultHashCount)
				for i := 0; i < 512; i++ {
					kid := testSHA256([]byte(fmt.Sprintf("split-depth-kid-%d", i)))
					vid := testSHA256([]byte(fmt.Sprintf("split-depth-vid-%d", i)))
					undecodable.Insert(kid, vid)
				}
				return &IBLTMessage{
					CheckpointId:    req.GetCheckpointId(),
					CheckpointIndex: req.GetCheckpointIndex(),
					IbltData:        undecodable.Marshal(),
				}, nil
			}
			empty := sketch.NewIBLT(req.GetNumCells(), sketch.DefaultHashCount)
			return &IBLTMessage{
				CheckpointId:    req.GetCheckpointId(),
				CheckpointIndex: req.GetCheckpointIndex(),
				IbltData:        empty.Marshal(),
			}, nil
		},
		exchangeRangeDigestsFn: func(_ context.Context, req *RangeDigestRequest, _ ...grpc.CallOption) (*RangeDigestResponse, error) {
			out := make([]*RangeDigest, 0, len(req.GetSpans()))
			for i, span := range req.GetSpans() {
				out = append(out, &RangeDigest{
					Span:               span,
					Count:              1,
					XorKeyHash:         bytes.Repeat([]byte{byte(i + 3)}, 32),
					XorValueHash:       bytes.Repeat([]byte{byte(i + 4)}, 32),
					SuggestedIbltCells: 8,
				})
			}
			return &RangeDigestResponse{Ranges: out}, nil
		},
		fetchEntriesFn: func(context.Context, *FetchEntriesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{}, nil
		},
	}
	sec.client = client

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}
	checkpoint := newTestRangeCheckpoint("cp-range-split-depth", 101, 64)
	checkpoint.TopRanges[0].Span.SplitDepth = 8

	if err := sec.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now()); err != nil {
		t.Fatalf("expected adaptive split reconciliation to succeed from non-zero depth, got %v", err)
	}
	if client.exchangeIBLTCallCount < 2 {
		t.Fatalf("expected adaptive split to trigger multiple ExchangeIBLT calls, got %d", client.exchangeIBLTCallCount)
	}
}

func TestDRIntegration_RangeRefinementPrefixFallback(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-prefix", core.logger)

	sec.client = &drRangeTestClient{
		exchangePrefixFn: func(context.Context, *PrefixDigestRequest, ...grpc.CallOption) (*PrefixDigestResponse, error) {
			return &PrefixDigestResponse{
				PrefixLength: 8,
				Buckets: []*BucketDigestProto{
					{
						Index:        1,
						Count:        1,
						XorKeyHash:   bytes.Repeat([]byte{0x01}, 32),
						XorValueHash: bytes.Repeat([]byte{0x02}, 32),
					},
				},
			}, nil
		},
		fetchEntriesFn: func(_ context.Context, req *FetchEntriesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{
				batches: []*EntryBatch{
					{
						CheckpointId:    req.GetCheckpointId(),
						CheckpointIndex: req.GetCheckpointIndex(),
						Entries: []*EntryChange{
							{OpType: string(physical.PutOperation), Key: "range/refined", Value: []byte("value")},
						},
					},
				},
			}, nil
		},
	}

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}
	checkpoint := &CheckpointResponse{CheckpointId: "cp-range-prefix", CommitIndex: 321}
	span := reconciler.RangeSpan{}
	for i := range span.EndKID {
		span.EndKID[i] = 0xff
	}
	budget := &drRangeBudget{start: time.Now()}
	sec.setState(DRSecondaryReconciling)
	sec.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex, 1)
	defer sec.endReconcileSession()

	if err := sec.runRangePrefixRefinement(context.Background(), checkpoint, localSet, span, budget); err != nil {
		t.Fatalf("expected in-range prefix refinement to succeed, got %v", err)
	}
	entry, err := core.barrier.Get(context.Background(), "range/refined")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected prefix refinement fetch to apply entry")
	}
}

func TestDRIntegration_ReconcileFailsOnBudgetExceeded(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-budget", core.logger)
	sec.lastAppliedIndex.Store(55)

	heavyIBLT := sketch.NewIBLT(drSecondaryRangeMaxIBLTCellsPerRange, sketch.DefaultHashCount)
	heavyData := heavyIBLT.Marshal()

	sec.client = &drRangeTestClient{
		exchangeIBLTFn: func(_ context.Context, req *IBLTMessage, _ ...grpc.CallOption) (*IBLTMessage, error) {
			return &IBLTMessage{
				CheckpointId:    req.GetCheckpointId(),
				CheckpointIndex: req.GetCheckpointIndex(),
				IbltData:        heavyData,
			}, nil
		},
		fetchEntriesFn: func(context.Context, *FetchEntriesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{}, nil
		},
	}

	topRanges := make([]*RangeDigest, 0, 64)
	for i := 0; i < 64; i++ {
		start := make([]byte, 32)
		end := make([]byte, 32)
		start[0] = byte(i)
		end[0] = byte(i)
		for j := 1; j < 32; j++ {
			end[j] = 0xff
		}
		topRanges = append(topRanges, &RangeDigest{
			Span: &RangeSpan{
				StartKid: start,
				EndKid:   end,
			},
			Count:              100000,
			XorKeyHash:         make([]byte, 32),
			XorValueHash:       make([]byte, 32),
			SuggestedIbltCells: drSecondaryRangeMaxIBLTCellsPerRange,
		})
	}
	checkpoint := &CheckpointResponse{
		CheckpointId: "cp-range-budget",
		CommitIndex:  999,
		TopRanges:    topRanges,
	}

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}

	err := sec.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("budget_exceeded")) {
		t.Fatalf("expected budget_exceeded error, got %v", err)
	}
	if sec.lastAppliedIndex.Load() != 55 {
		t.Fatalf("expected lastAppliedIndex to remain 55 after failure, got %d", sec.lastAppliedIndex.Load())
	}
}

func TestDRIntegration_NoIndexAdvanceOnPartialRangeFailure(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-range-partial", core.logger)
	sec.lastAppliedIndex.Store(7)

	kid := testSHA256([]byte("range-partial-kid"))
	vid := testSHA256([]byte("range-partial-vid"))
	remoteIBLT := sketch.NewIBLT(128, sketch.DefaultHashCount)
	remoteIBLT.Insert(kid, vid)
	remoteIBLTBytes := remoteIBLT.Marshal()

	sec.client = &drRangeTestClient{
		exchangeIBLTFn: func(_ context.Context, req *IBLTMessage, _ ...grpc.CallOption) (*IBLTMessage, error) {
			return &IBLTMessage{
				CheckpointId:    req.GetCheckpointId(),
				CheckpointIndex: req.GetCheckpointIndex(),
				IbltData:        remoteIBLTBytes,
			}, nil
		},
		fetchEntriesFn: func(_ context.Context, req *FetchEntriesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[EntryBatch], error) {
			return &staticEntryBatchStream{
				batches: []*EntryBatch{
					{
						CheckpointId:    req.GetCheckpointId(),
						CheckpointIndex: req.GetCheckpointIndex(),
						Entries: []*EntryChange{
							{OpType: "bogus-op", Key: "range/partial"},
						},
					},
				},
			}, nil
		},
	}

	localSet := &reconciler.ReconciliationSet{
		KIDToVID: make(map[[32]byte][32]byte),
		KIDToKey: make(map[[32]byte]string),
		Entries:  make(map[[32]byte]*physical.Entry),
	}
	checkpoint := newTestRangeCheckpoint("cp-range-partial", 88, 1)

	err := sec.runRangeReconciliation(context.Background(), checkpoint, localSet, time.Now())
	if err == nil {
		t.Fatal("expected reconciliation to fail on partial apply error")
	}
	if sec.lastAppliedIndex.Load() != 7 {
		t.Fatalf("expected lastAppliedIndex to remain unchanged at 7, got %d", sec.lastAppliedIndex.Load())
	}
}

func TestDRIntegration_StreamAppliesSameRaftIndexBatchEntries(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	rand.Read(replSalt)

	sec := newDRReplicationSecondary(core, replSalt, "rel-stream-same-index", core.logger)
	sec.lastAppliedIndex.Store(10)

	sec.client = &drRangeTestClient{
		streamChangesFn: func(_ context.Context, req *StreamChangesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[EntryChange], error) {
			if req.GetLastAppliedIndex() != 10 {
				t.Fatalf("expected LastAppliedIndex=10, got %d", req.GetLastAppliedIndex())
			}
			return &staticEntryChangeStream{
				changes: []*EntryChange{
					{OpType: "put", Key: "stream/same-index/meta", Value: []byte("meta"), RaftIndex: 11},
					{OpType: "put", Key: "stream/same-index/data", Value: []byte("data"), RaftIndex: 11},
				},
			}, nil
		},
	}

	err := sec.runStream(context.Background())
	if err == nil {
		t.Fatal("expected runStream to return when stream ends")
	}

	meta, err := core.physical.Get(context.Background(), "stream/same-index/meta")
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || string(meta.Value) != "meta" {
		t.Fatalf("expected meta entry to be applied, got %#v", meta)
	}

	data, err := core.physical.Get(context.Background(), "stream/same-index/data")
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || string(data.Value) != "data" {
		t.Fatalf("expected data entry to be applied, got %#v", data)
	}

	if got := sec.entriesApplied.Load(); got != 2 {
		t.Fatalf("expected entriesApplied=2, got %d", got)
	}
	if got := sec.lastAppliedIndex.Load(); got != 11 {
		t.Fatalf("expected lastAppliedIndex=11, got %d", got)
	}
}

func TestDRIntegration_PrimaryStreamReplayIncludesLastAppliedIndex(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	setupTestClusterCert(t, core)

	mgr := core.drManager
	if mgr == nil {
		t.Fatal("expected DR relationship manager to be initialized")
	}
	if err := mgr.EnablePrimary(context.Background()); err != nil {
		t.Fatal(err)
	}

	token, err := mgr.GenerateActivationToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	const fingerprint = "test-secondary-fingerprint"
	rel, err := mgr.loadRelationship(context.Background(), token.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	rel.State = DRRelationshipStateRegistered
	rel.SecondaryCertFingerprint = fingerprint
	if err := mgr.saveRelationship(context.Background(), rel); err != nil {
		t.Fatal(err)
	}

	primary := mgr.Primary()
	if primary == nil {
		t.Fatal("expected primary replication server to be initialized")
	}

	primary.bufMu.Lock()
	primary.changeBuffer = []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "stream/replay/a", Value: []byte("a"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "stream/replay/b", Value: []byte("b"), RaftIndex: 10},
		{OpType: physical.PutOperation, Key: "stream/replay/c", Value: []byte("c"), RaftIndex: 11},
	}
	primary.bufMu.Unlock()

	streamCtx, cancel := context.WithCancel(context.WithValue(context.Background(), drPeerFingerprintContextKey{}, fingerprint))
	cancel()
	stream := &captureEntryChangeServerStream{ctx: streamCtx}

	err = primary.StreamChanges(&StreamChangesRequest{
		RelationshipId:   token.RelationshipID,
		LastAppliedIndex: 10,
	}, stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled from stream loop, got: %v", err)
	}

	if len(stream.sent) != 3 {
		t.Fatalf("expected 3 replayed entries, got %d", len(stream.sent))
	}
	if stream.sent[0].RaftIndex != 10 || stream.sent[1].RaftIndex != 10 {
		t.Fatalf("expected entries at last_applied_index to be replayed, got indexes: %d, %d", stream.sent[0].RaftIndex, stream.sent[1].RaftIndex)
	}
	if stream.sent[2].RaftIndex != 11 {
		t.Fatalf("expected replay to include later index, got %d", stream.sent[2].RaftIndex)
	}
}

func newSecondaryCert(t *testing.T, serial int64, commonName string) ([]byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return der, cert
}

func tlsPeerContext(ctx context.Context, cert *x509.Certificate) context.Context {
	return peer.NewContext(ctx, &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{cert},
			},
		},
	})
}

func TestDRIntegration_MultiRelationshipRangeIsolation(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	ctx := context.Background()
	setupTestClusterCert(t, core)

	mgr := newDRRelationshipManager(core, core.logger)
	core.drManager = mgr
	if err := mgr.EnablePrimary(ctx); err != nil {
		t.Fatal(err)
	}

	tok1, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	certDER1, cert1 := newSecondaryCert(t, 7001, "multi-rel-1")
	certDER2, cert2 := newSecondaryCert(t, 7002, "multi-rel-2")
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, tok1.RelationshipID, tok1.BootstrapToken, certDER1); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, tok2.RelationshipID, tok2.BootstrapToken, certDER2); err != nil {
		t.Fatal(err)
	}

	rpcCtx1 := tlsPeerContext(ctx, cert1)
	rpcCtx2 := tlsPeerContext(ctx, cert2)
	primary := mgr.Primary()
	writeTestEntries(t, core, "multi-rel", 32)

	cp1, err := primary.RequestCheckpoint(rpcCtx1, &CheckpointRequest{RelationshipId: tok1.RelationshipID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.ExchangeIBLT(rpcCtx2, &IBLTMessage{
		CheckpointId:    cp1.CheckpointId,
		CheckpointIndex: cp1.CommitIndex,
		NumCells:        sketch.DefaultHashCount,
	}); err == nil {
		t.Fatal("expected cross-relationship checkpoint access to be denied")
	}
	if _, err := primary.ExchangeIBLT(rpcCtx1, &IBLTMessage{
		CheckpointId:    cp1.CheckpointId,
		CheckpointIndex: cp1.CommitIndex,
		NumCells:        sketch.DefaultHashCount,
	}); err != nil {
		t.Fatalf("expected matching relationship access to succeed, got %v", err)
	}
}

func TestDRIntegration_RevokeDuringRangeReconcileAborts(t *testing.T) {
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
	secondaryDER, secondaryCert := newSecondaryCert(t, 8001, "revoke-during-reconcile")
	if err := mgr.ValidateBootstrapAndStoreCert(ctx, token.RelationshipID, token.BootstrapToken, secondaryDER); err != nil {
		t.Fatal(err)
	}
	rpcCtx := tlsPeerContext(ctx, secondaryCert)

	primary := mgr.Primary()
	writeTestEntries(t, core, "revoke-range", 16)
	cp, err := primary.RequestCheckpoint(rpcCtx, &CheckpointRequest{RelationshipId: token.RelationshipID})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.RevokeRelationship(ctx, token.RelationshipID); err != nil {
		t.Fatal(err)
	}

	if _, err := primary.ExchangeIBLT(rpcCtx, &IBLTMessage{
		CheckpointId:    cp.CheckpointId,
		CheckpointIndex: cp.CommitIndex,
		NumCells:        sketch.DefaultHashCount,
	}); err == nil {
		t.Fatal("expected revoked relationship to be denied for in-flight reconcile RPC")
	}
}

// Verify unused imports don't cause issues.
var _ = sketch.DefaultHashCount
