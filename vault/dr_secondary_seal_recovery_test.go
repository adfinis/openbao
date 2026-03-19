// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"errors"
	"fmt"
	"testing"

	log "github.com/hashicorp/go-hclog"
	raftstorage "github.com/openbao/openbao/physical/raft"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/sdk/v2/physical/inmem"
	"github.com/openbao/openbao/vault/barrier"
)

func TestDRBootstrapPhysicalRecursiveDelete_PreservesCriticalPaths(t *testing.T) {
	ctx := context.Background()
	backend, err := inmem.NewInmem(nil, log.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}

	core := &Core{
		physical: backend,
		logger:   log.NewNullLogger(),
	}
	secondary := &drReplicationSecondary{
		core:   core,
		logger: log.NewNullLogger(),
	}

	preserved := []string{
		"core/keyring",
		"core/root-key",
		"core/hsm/barrier-unseal-keys",
		"core/seal-config",
		"core/recovery-config",
		"core/recovery-key",
		"core/cluster/local/info",
	}
	removed := []string{
		"core/raft/tls",
		"core/dr-replication/config",
		"core/leader/foo",
		"foo/bar",
	}

	for _, key := range append(append([]string{}, preserved...), removed...) {
		if err := core.physical.Put(ctx, &physical.Entry{Key: key, Value: []byte("v")}); err != nil {
			t.Fatalf("failed to seed %q: %v", key, err)
		}
	}

	var deleted int
	if err := secondary.physicalRecursiveDelete(ctx, "", &deleted); err != nil {
		t.Fatal(err)
	}

	for _, key := range preserved {
		entry, err := core.physical.Get(ctx, key)
		if err != nil {
			t.Fatalf("failed to read preserved key %q: %v", key, err)
		}
		if entry == nil {
			t.Fatalf("expected preserved key %q to remain", key)
		}
	}

	for _, key := range removed {
		entry, err := core.physical.Get(ctx, key)
		if err != nil {
			t.Fatalf("failed to read removed key %q: %v", key, err)
		}
		if entry != nil {
			t.Fatalf("expected key %q to be deleted", key)
		}
	}
}

func TestShouldRecreateRaftTLSKeyringForDRSecondary(t *testing.T) {
	active := &raftstorage.TLSKey{ID: "key-1"}
	valid := &raftstorage.TLSKeyring{
		Keys:        []*raftstorage.TLSKey{active},
		ActiveKeyID: "key-1",
	}
	noActive := &raftstorage.TLSKeyring{
		Keys:        []*raftstorage.TLSKey{active},
		ActiveKeyID: "different",
	}

	tests := []struct {
		name          string
		isDRSecondary bool
		keyring       *raftstorage.TLSKeyring
		err           error
		want          bool
	}{
		{name: "non-dr missing", isDRSecondary: false, err: errRaftTLSKeyringMissing, want: false},
		{name: "dr missing", isDRSecondary: true, err: errRaftTLSKeyringMissing, want: true},
		{name: "dr corrupt", isDRSecondary: true, err: fmt.Errorf("%w: decode failed", errRaftTLSKeyringCorrupt), want: true},
		{name: "dr generic read error", isDRSecondary: true, err: errors.New("backend unavailable"), want: false},
		{name: "dr nil keyring", isDRSecondary: true, keyring: nil, want: true},
		{name: "dr no active key", isDRSecondary: true, keyring: noActive, want: true},
		{name: "dr valid", isDRSecondary: true, keyring: valid, want: false},
	}

	for _, tt := range tests {
		got := shouldRecreateRaftTLSKeyringForDRSecondary(tt.isDRSecondary, tt.keyring, tt.err)
		if got != tt.want {
			t.Fatalf("%s: got=%v want=%v", tt.name, got, tt.want)
		}
	}
}

func TestCoreIsPersistedDRSecondary(t *testing.T) {
	ctx := context.Background()
	core, _, _ := TestCoreUnsealed(t)

	isSecondary, err := core.isPersistedDRSecondary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if isSecondary {
		t.Fatal("expected no persisted DR mode to be non-secondary")
	}

	cfg := &DRConfig{Mode: DRModeSecondary}
	entry, err := logical.StorageEntryJSON(drConfigPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	isSecondary, err = core.isPersistedDRSecondary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !isSecondary {
		t.Fatal("expected persisted DR secondary mode")
	}

	cfg = &DRConfig{Mode: DRModePrimary}
	entry, err = logical.StorageEntryJSON(drConfigPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.barrier.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	isSecondary, err = core.isPersistedDRSecondary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if isSecondary {
		t.Fatal("expected persisted DR primary mode to not be secondary")
	}

	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   drConfigPath,
		Value: []byte("{not-json"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = core.isPersistedDRSecondary(ctx)
	if err == nil {
		t.Fatal("expected decode error for malformed DR config")
	}
}

func TestDRManagerPersistConfigSnapshot(t *testing.T) {
	ctx := context.Background()
	core, _, _ := TestCoreUnsealed(t)

	mgr := core.drManager
	if mgr == nil {
		t.Fatal("expected core to have drManager")
	}

	mgr.mu.Lock()
	mgr.config = &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      "cluster-1",
		RelationshipID: "rel-1",
		ReplSalt:       []byte{1, 2, 3, 4},
		PrimaryAddr:    "https://rws-bao-01:8201",
		PrimaryAddrs:   []string{"https://rws-bao-01:8201"},
	}
	mgr.mu.Unlock()

	if err := mgr.PersistConfigSnapshot(ctx); err != nil {
		t.Fatalf("persist config snapshot failed: %v", err)
	}

	entry, err := core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	if entry == nil {
		t.Fatal("expected persisted DR config entry")
	}

	var cfg DRConfig
	if err := entry.DecodeJSON(&cfg); err != nil {
		t.Fatalf("failed to decode persisted config: %v", err)
	}
	if cfg.Mode != DRModeSecondary {
		t.Fatalf("expected persisted mode secondary, got %q", cfg.Mode)
	}

	mgr.mu.Lock()
	mgr.config = nil
	mgr.mu.Unlock()
	if err := mgr.PersistConfigSnapshot(ctx); err == nil {
		t.Fatal("expected error when persisting nil DR config")
	}
}

func TestCoreIsDRSecondaryRuntimeMode(t *testing.T) {
	ctx := context.Background()
	core, _, _ := TestCoreUnsealed(t)

	mgr := core.drManager
	if mgr == nil {
		t.Fatal("expected core to have drManager")
	}

	// Runtime DR mode is authoritative, even with unreadable persisted config.
	mgr.mu.Lock()
	mgr.config = &DRConfig{Mode: DRModeSecondary}
	mgr.mu.Unlock()
	if err := core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   drConfigPath,
		Value: []byte("{not-json"),
	}); err != nil {
		t.Fatal(err)
	}

	isSecondary, err := core.isDRSecondaryRuntimeMode(ctx)
	if err != nil {
		t.Fatalf("unexpected runtime mode error: %v", err)
	}
	if !isSecondary {
		t.Fatal("expected runtime DR secondary mode")
	}

	// Disabled runtime mode falls back to persisted config and errors if unreadable.
	mgr.mu.Lock()
	mgr.config = &DRConfig{Mode: DRModeDisabled}
	mgr.mu.Unlock()
	_, err = core.isDRSecondaryRuntimeMode(ctx)
	if err == nil {
		t.Fatal("expected persisted-config decode error when runtime mode is indeterminate")
	}
}

func TestIsBenignLeadershipSetupError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "wrapped context canceled", err: fmt.Errorf("wrap: %w", context.Canceled), want: true},
		{name: "barrier sealed", err: barrier.ErrBarrierSealed, want: true},
		{name: "wrapped barrier sealed", err: fmt.Errorf("wrapped: %w", barrier.ErrBarrierSealed), want: true},
		{name: "other", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		got := isBenignLeadershipSetupError(tt.err)
		if got != tt.want {
			t.Fatalf("%s: got=%v want=%v", tt.name, got, tt.want)
		}
	}
}

type testHALock struct{}

func (l *testHALock) Lock(stopCh <-chan struct{}) (<-chan struct{}, error) {
	ch := make(chan struct{})
	return ch, nil
}

func (l *testHALock) Unlock() error {
	return nil
}

func (l *testHALock) Value() (bool, string, error) {
	return true, "test", nil
}

func TestShouldRunSecondaryControllerLocked(t *testing.T) {
	mgr := &drRelationshipManager{
		core: &Core{},
		secondary: &drReplicationSecondary{
			logger: log.NewNullLogger(),
		},
		logger: log.NewNullLogger(),
	}

	// Non-standby state should run.
	if !mgr.shouldRunSecondaryControllerLocked() {
		t.Fatal("expected controller to run when node is not standby")
	}

	// Standby without HA lock must not run.
	mgr.core.standby.Store(true)
	if mgr.shouldRunSecondaryControllerLocked() {
		t.Fatal("expected standby node without HA lock to skip controller")
	}

	// Active HA node (lock held) can run even before standby flag clears.
	mgr.core.heldHALock = &testHALock{}
	if !mgr.shouldRunSecondaryControllerLocked() {
		t.Fatal("expected node holding HA lock to run controller")
	}

	// Missing secondary instance must skip.
	mgr.secondary = nil
	if mgr.shouldRunSecondaryControllerLocked() {
		t.Fatal("expected nil secondary to skip controller")
	}
}

func TestDRSecondarySelfHealWriteAllowed(t *testing.T) {
	core := &Core{}

	// Non-standby nodes can perform write-based self-heal.
	if !core.drSecondarySelfHealWriteAllowed() {
		t.Fatal("expected write-based self-heal to be allowed on non-standby node")
	}

	// Standby nodes must not attempt write-based self-heal.
	core.standby.Store(true)
	if core.drSecondarySelfHealWriteAllowed() {
		t.Fatal("expected write-based self-heal to be blocked on standby node")
	}
}
