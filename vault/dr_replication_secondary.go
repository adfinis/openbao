// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// drNeverReplicateExactPaths lists storage paths that must never be
// replicated from the primary to the secondary. These entries are
// cluster-local and copying them across clusters can break unseal/mount
// behavior.
var drNeverReplicateExactPaths = map[string]bool{
	"core/hsm/barrier-unseal-keys": true, // stored root key, encrypted by local seal
	"core/seal-config":             true, // local seal configuration
	"core/local-mounts":            true, // local mount table root
	"core/local-auth":              true, // local auth mount table root
	"core/local-audit":             true, // local audit mount table root
}

// drNeverReplicatePrefixes lists path prefixes that must never be
// replicated (root or namespaced forms).
var drNeverReplicatePrefixes = []string{
	"core/local-mounts/",
	"core/local-auth/",
	"core/local-audit/",
}

// drReconcileExcludeExactPaths lists storage paths excluded from
// reconciliation in addition to drNeverReplicate*.
var drReconcileExcludeExactPaths = map[string]bool{
	"core/keyring": true, // encrypted with root key; handled by SyncKeyring
}

// isDRNeverReplicatePath returns true if the path should never be
// replicated from primary to secondary.
func isDRNeverReplicatePath(path string) bool {
	return drPathMatches(path, drNeverReplicateExactPaths, drNeverReplicatePrefixes)
}

// isDRReconcileExcludedPath returns true if the path should be
// excluded from the reconciler scanner.
func isDRReconcileExcludedPath(path string) bool {
	if isDRNeverReplicatePath(path) {
		return true
	}
	return drPathMatches(path, drReconcileExcludeExactPaths, nil)
}

func drPathMatches(path string, exact map[string]bool, prefixes []string) bool {
	path = strings.TrimPrefix(path, "/")
	if exact[path] {
		return true
	}
	for p := range exact {
		if strings.HasSuffix(path, "/"+p) {
			return true
		}
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) || strings.Contains(path, "/"+prefix) {
			return true
		}
	}
	return false
}

// DRSecondaryState represents the current state of the DR secondary.
type DRSecondaryState int32

const (
	DRSecondaryIdle          DRSecondaryState = iota
	DRSecondaryBootstrapping                  // Initial setup / token exchange
	DRSecondaryInitialSync                    // First full sync from primary
	DRSecondaryStreaming                      // Normal mode: receiving change stream
	DRSecondaryReconciling                    // Recovery mode: IBLT/prefix digest reconciliation
	DRSecondaryPromoting                      // Failover in progress
	DRSecondaryStandalone                     // Post-promotion: now an independent primary
)

const (
	drRangeTargetKeysPerRange            = 12000
	drRangeTargetValueBytes              = 8 << 20 // 8 MiB
	drRangeMaxTopRanges                  = 256
	drRangeMaxTotalRanges                = 1024
	drRangeMaxSplitDepth                 = 6
	drSecondaryRangeMaxIBLTCellsPerRange = 32768

	drRangeMaxReconcileRPCBytes = 512 << 20
	drRangeMaxReconcileWallTime = 30 * time.Minute
	drRangeMaxInflightTasks     = 16
)

func (s DRSecondaryState) String() string {
	switch s {
	case DRSecondaryIdle:
		return "idle"
	case DRSecondaryBootstrapping:
		return "bootstrapping"
	case DRSecondaryInitialSync:
		return "initial-sync"
	case DRSecondaryStreaming:
		return "streaming"
	case DRSecondaryReconciling:
		return "reconciling"
	case DRSecondaryPromoting:
		return "promoting"
	case DRSecondaryStandalone:
		return "standalone"
	default:
		return "unknown"
	}
}

// drReplicationSecondary manages the secondary side of DR replication.
// It connects to the primary's gRPC service, receives the change
// stream, detects gaps, runs IBLT reconciliation, and applies
// changes to the local storage.
type drReplicationSecondary struct {
	logger log.Logger
	core   *Core

	// scanner builds reconciliation sets from local storage.
	scanner *reconciler.Scanner

	// client is the gRPC client connection to the primary.
	client DRReplicationClient

	// conn is the underlying gRPC connection (for cleanup).
	conn *grpc.ClientConn

	// state tracks the secondary's replication state.
	state atomic.Int32

	// lastAppliedIndex is the primary's Raft index of the last
	// successfully applied entry.
	lastAppliedIndex atomic.Uint64

	// relationshipID identifies this DR relationship.
	relationshipID string

	// replSalt is the shared HMAC key for KID derivation.
	replSalt []byte

	// primaryCACert is the primary's TLS CA certificate for mTLS.
	primaryCACert []byte

	// stopCh signals all goroutines to stop.
	stopCh chan struct{}
	stopMu sync.Mutex

	// keyringBootstrapped tracks whether the keyring has been synced.
	keyringBootstrapped atomic.Bool

	// transportReady indicates mTLS transport has been configured.
	transportReady atomic.Bool

	// metrics
	entriesApplied               atomic.Uint64
	reconcileCount               atomic.Uint64
	lastReconcileAt              atomic.Int64 // unix timestamp
	streamDisconnects            atomic.Uint64
	connectRetries               atomic.Uint64
	connectFailures              atomic.Uint64
	reconcileRangesInflight      atomic.Int64
	reconcileRangesFailed        atomic.Int64
	reconcileBudgetRemainingByte atomic.Int64
	rangeSplitCount              atomic.Uint64
	reconcileRPCBytesUsed        atomic.Uint64

	sessionMu                  sync.RWMutex
	activeCheckpointID         string
	activeCheckpointIndex      uint64
	sessionStart               time.Time
	streamPausedAt             uint64
	lastReconcileFailReason    string
	lastRangeManifestCount     int
	lastReconcileFailureByType map[string]uint64
}

// newDRReplicationSecondary creates a new secondary replication manager.
func newDRReplicationSecondary(core *Core, replSalt []byte, relationshipID string, logger log.Logger) *drReplicationSecondary {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true // Secondary needs reverse KID->key mapping for deletes
	config.Logger = logger.Named("reconciler")

	// Keep exact exclusions for hot-path lookup and include predicate-based
	// exclusions for namespaced/prefix paths.
	excludePaths := make(map[string]bool, len(drNeverReplicateExactPaths)+len(drReconcileExcludeExactPaths))
	for p := range drNeverReplicateExactPaths {
		excludePaths[p] = true
	}
	for p := range drReconcileExcludeExactPaths {
		excludePaths[p] = true
	}
	config.ExcludePaths = excludePaths
	config.ExcludePathFunc = isDRReconcileExcludedPath

	return &drReplicationSecondary{
		logger:                     logger.Named("dr-secondary"),
		core:                       core,
		scanner:                    reconciler.NewScanner(config),
		relationshipID:             relationshipID,
		replSalt:                   replSalt,
		stopCh:                     make(chan struct{}),
		lastReconcileFailureByType: make(map[string]uint64),
	}
}

// Connect establishes the gRPC connection to the primary.
// The connection is strictly mTLS-only via DRReplicationALPN and fails
// closed if transport credentials cannot be established.
func (s *drReplicationSecondary) Connect(ctx context.Context, primaryAddr string, opts ...grpc.DialOption) error {
	s.logger.Info("connecting to primary", "addr", primaryAddr)

	// Strip the scheme (https://) from the address -- gRPC expects host:port only.
	primaryAddr = strings.TrimPrefix(primaryAddr, "https://")
	primaryAddr = strings.TrimPrefix(primaryAddr, "http://")

	if len(s.primaryCACert) == 0 {
		return fmt.Errorf("dr-secondary: missing primary CA certificate; mTLS is required")
	}

	cl := s.core.getClusterListener()
	if cl == nil {
		return fmt.Errorf("dr-secondary: cluster listener not available; mTLS transport cannot be established")
	}

	parsedCert, err := x509.ParseCertificate(s.primaryCACert)
	if err != nil {
		return fmt.Errorf("dr-secondary: failed to parse primary CA cert: %w", err)
	}

	client := &drReplicationClusterClient{
		core:          s.core,
		primaryCACert: parsedCert,
	}
	// Ensure client registration is idempotent across reconnects.
	cl.RemoveClient(consts.DRReplicationALPN)
	cl.AddClient(consts.DRReplicationALPN, client)

	dialerFunc := cl.GetContextDialerFunc(ctx, consts.DRReplicationALPN)
	opts = append(opts,
		grpc.WithContextDialer(dialerFunc),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	s.transportReady.Store(true)
	s.logger.Info("using mTLS dialer for DR replication")

	conn, err := grpc.NewClient(primaryAddr, opts...)
	if err != nil {
		return fmt.Errorf("dr-secondary: failed to connect to primary: %w", err)
	}

	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = conn
	s.client = NewDRReplicationClient(conn)
	s.setState(DRSecondaryBootstrapping)

	s.logger.Info("connected to primary", "addr", primaryAddr)
	return nil
}

// Start begins the replication loop: stream changes from the primary,
// detect gaps, and reconcile as needed.
func (s *drReplicationSecondary) Start(ctx context.Context) error {
	s.logger.Info("starting DR secondary replication")
	if !s.transportReady.Load() {
		return fmt.Errorf("DR secondary transport not initialized")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		default:
		}

		switch s.State() {
		case DRSecondaryBootstrapping, DRSecondaryInitialSync:
			// Bootstrap keyring from primary if not yet done.
			if !s.keyringBootstrapped.Load() {
				if err := s.bootstrapKeyring(ctx); err != nil {
					s.logger.Error("keyring bootstrap failed", "error", err)
					time.Sleep(5 * time.Second)
					continue
				}
			}

			// Run initial reconciliation to sync from primary.
			s.setState(DRSecondaryReconciling)
			if err := s.runReconciliation(ctx); err != nil {
				s.logger.Error("initial reconciliation failed", "error", err)
				class := s.markReconcileFailure(err)
				time.Sleep(s.nextReconcileRetryDelay(class))
				continue
			}
			s.markReconcileSuccess()

			// After initial reconciliation, the storage contains the
			// primary's data (including token salt, mount table, etc.)
			// but the core's in-memory caches are stale. Invalidate
			// critical caches so the next request sees primary data.
			s.invalidateCoreCaches(ctx)

			// Rebuild in-memory mount/auth state so secondary read traffic
			// can route to replicated mounts (e.g. KV) without requiring
			// a promotion.
			if err := s.reloadCoreState(ctx); err != nil {
				s.logger.Error("failed to reload replicated core state", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}

			s.setState(DRSecondaryStreaming)

		case DRSecondaryStreaming:
			// Connect to change stream.
			if err := s.runStream(ctx); err != nil {
				s.logger.Warn("change stream disconnected", "error", err)
				s.streamDisconnects.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_disconnects"}, 1)
				// Fall back to reconciliation.
				s.setState(DRSecondaryReconciling)
				continue
			}

		case DRSecondaryReconciling:
			if err := s.runReconciliation(ctx); err != nil {
				s.logger.Error("reconciliation failed", "error", err)
				class := s.markReconcileFailure(err)
				time.Sleep(s.nextReconcileRetryDelay(class))
				continue
			}
			s.markReconcileSuccess()
			s.setState(DRSecondaryStreaming)

		case DRSecondaryPromoting, DRSecondaryStandalone:
			// Replication stopped; exit loop.
			return nil

		default:
			s.setState(DRSecondaryBootstrapping)
		}
	}
}

// Stop gracefully stops the replication loop.
func (s *drReplicationSecondary) Stop() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	select {
	case <-s.stopCh:
		// Already stopped.
	default:
		close(s.stopCh)
	}

	if s.conn != nil {
		s.conn.Close()
	}

	// Remove the mTLS client from the cluster listener to avoid
	// leaking a stale client after stop or promote.
	if cl := s.core.getClusterListener(); cl != nil {
		cl.RemoveClient(consts.DRReplicationALPN)
	}
}

// Promote transitions the secondary to a standalone primary.
func (s *drReplicationSecondary) Promote() error {
	s.logger.Info("promoting DR secondary to primary")
	s.setState(DRSecondaryPromoting)

	// Stop receiving changes from primary.
	s.Stop()

	ctx := context.Background()

	// Reload the core's in-memory state from storage. After DR
	// replication the storage contains the primary's data (mount
	// table, auth backends, policies, etc.) but the secondary's
	// in-memory state still reflects its original initialization.
	// Reloading ensures the promoted node can serve requests using
	// the primary's configuration.
	if err := s.reloadCoreState(ctx); err != nil {
		s.logger.Error("failed to reload core state during promotion", "error", err)
		return fmt.Errorf("failed to reload core state: %w", err)
	}

	// Transition the core to primary mode.
	s.setState(DRSecondaryStandalone)
	s.logger.Info("DR secondary promoted to standalone primary")
	return nil
}

// reloadCoreState reloads the critical in-memory subsystems from
// storage. This is necessary after DR promotion because the storage
// now contains the primary's data but the in-memory caches still
// reflect the secondary's original initialization.
//
// We cannot do a full unload/reload cycle because that would destroy
// the running system backend, token store, and other critical
// singleton mounts. Instead we take a targeted approach: load the
// primary's mount table and mount any NEW entries that don't already
// exist in the router.
func (s *drReplicationSecondary) reloadCoreState(ctx context.Context) error {
	// Purge physical cache first to ensure all reads go to storage.
	if s.core.physicalCache != nil {
		s.core.physicalCache.Purge(ctx)
	}

	// Invalidate the token store salt so it reloads from primary data.
	if s.core.tokenStore != nil {
		s.core.tokenStore.Invalidate(ctx, "token/salt")
	}

	// Load the primary's mount table from storage. This replaces the
	// in-memory mount table but doesn't touch the router or backends.
	s.logger.Info("reloading mount table from storage")
	if err := s.core.loadMounts(ctx); err != nil {
		return fmt.Errorf("failed to reload mounts: %w", err)
	}

	// Mount any new entries from the primary that aren't already
	// registered in the router (e.g., the KV engine the primary had).
	s.logger.Info("mounting new entries from primary")
	if err := s.mountNewEntries(ctx); err != nil {
		return fmt.Errorf("failed to mount new entries: %w", err)
	}

	// Reload auth backends from storage.
	s.logger.Info("reloading auth backends from storage")
	if err := s.core.loadCredentials(ctx); err != nil {
		return fmt.Errorf("failed to reload credentials: %w", err)
	}

	s.logger.Info("core state reloaded from storage")
	return nil
}

// mountNewEntries iterates the loaded mount table and initializes
// backends for any mount entries not already present in the router.
// Existing mounts (sys/, identity/, cubbyhole/) are left untouched.
func (s *drReplicationSecondary) mountNewEntries(ctx context.Context) error {
	if s.core.mounts == nil {
		return nil
	}

	for _, entry := range s.core.mounts.Entries {
		// Check if this mount is already in the router.
		nsCtx := namespace.ContextWithNamespace(ctx, entry.namespace)
		if s.core.router.MatchingMount(nsCtx, entry.Path) != "" {
			continue
		}

		s.logger.Info("mounting new entry from primary", "path", entry.Path, "type", entry.Type)

		view, err := s.core.mountEntryView(entry)
		if err != nil {
			s.logger.Error("failed to create view for mount", "path", entry.Path, "error", err)
			continue
		}

		sysView := s.core.mountEntrySysView(entry)
		backend, sha256, err := s.core.newLogicalBackend(ctx, entry, sysView, view)
		if err != nil {
			s.logger.Error("failed to create backend for mount", "path", entry.Path, "error", err)
			continue
		}
		entry.RunningSha256 = sha256

		if err := s.core.router.Mount(backend, entry.Path, entry, view); err != nil {
			s.logger.Error("failed to router-mount entry", "path", entry.Path, "error", err)
			continue
		}

		// Initialize the backend.
		if backend != nil {
			if err := backend.Initialize(ctx, &logical.InitializationRequest{Storage: view}); err != nil {
				s.logger.Error("failed to initialize backend", "path", entry.Path, "error", err)
			}
		}
	}

	return nil
}

// State returns the current secondary state.
func (s *drReplicationSecondary) State() DRSecondaryState {
	return DRSecondaryState(s.state.Load())
}

func (s *drReplicationSecondary) setState(state DRSecondaryState) {
	old := DRSecondaryState(s.state.Swap(int32(state)))
	if old != state {
		s.logger.Info("state transition", "from", old.String(), "to", state.String())
	}
}

// Status returns a snapshot of the secondary's replication status.
func (s *drReplicationSecondary) Status() DRSecondaryStatus {
	remaining := s.reconcileBudgetRemainingByte.Load()
	if remaining < 0 {
		remaining = 0
	}
	s.sessionMu.RLock()
	activeID := s.activeCheckpointID
	activeIndex := s.activeCheckpointIndex
	failReason := s.lastReconcileFailReason
	rangeManifestCount := s.lastRangeManifestCount
	s.sessionMu.RUnlock()
	return DRSecondaryStatus{
		State:                          s.State().String(),
		RelationshipID:                 s.relationshipID,
		LastAppliedIndex:               s.lastAppliedIndex.Load(),
		EntriesApplied:                 s.entriesApplied.Load(),
		ReconcileCount:                 s.reconcileCount.Load(),
		LastReconcileAt:                time.Unix(s.lastReconcileAt.Load(), 0),
		ConnectRetries:                 s.connectRetries.Load(),
		ConnectFailures:                s.connectFailures.Load(),
		ReconcileRangesInflight:        s.reconcileRangesInflight.Load(),
		ReconcileRangesFailed:          s.reconcileRangesFailed.Load(),
		ReconcileBudgetRemainingBytes:  uint64(remaining),
		ReconcileActiveCheckpointID:    activeID,
		ReconcileActiveCheckpointIndex: activeIndex,
		ReconcileFailReasonLast:        failReason,
		RangeManifestCount:             rangeManifestCount,
		RangeSplitCount:                s.rangeSplitCount.Load(),
		ReconcileRPCBytesUsed:          s.reconcileRPCBytesUsed.Load(),
	}
}

// DRSecondaryStatus is a point-in-time snapshot of replication status.
type DRSecondaryStatus struct {
	State                          string
	RelationshipID                 string
	LastAppliedIndex               uint64
	EntriesApplied                 uint64
	ReconcileCount                 uint64
	LastReconcileAt                time.Time
	ConnectRetries                 uint64
	ConnectFailures                uint64
	ReconcileRangesInflight        int64
	ReconcileRangesFailed          int64
	ReconcileBudgetRemainingBytes  uint64
	ReconcileActiveCheckpointID    string
	ReconcileActiveCheckpointIndex uint64
	ReconcileFailReasonLast        string
	RangeManifestCount             int
	RangeSplitCount                uint64
	ReconcileRPCBytesUsed          uint64
}

// --- Keyring bootstrap ---

// isStopped returns true if the stop channel has been closed.
func (s *drReplicationSecondary) isStopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// bootstrapKeyring fetches the primary's root key, encrypted keyring,
// and root-key entry, then adopts them on the secondary. The ordering
// is critical:
//
//  1. Set the primary's root key in-memory so ReloadKeyring can use it.
//  2. Write the primary's core/keyring and core/root-key to physical storage.
//  3. ReloadKeyring – decrypts core/keyring with the (now correct) root key.
//  4. Persist the primary's root key under the secondary's seal so it
//     survives restarts (the operator still uses the secondary's own
//     unseal keys, which decrypt to the primary's root key).
func (s *drReplicationSecondary) bootstrapKeyring(ctx context.Context) error {
	s.logger.Info("bootstrapping keyring from primary")

	curve := ecdh.X25519()
	clientPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate client ephemeral key: %w", err)
	}

	clientNonce := make([]byte, drBootstrapNonceSize)
	if _, err := rand.Read(clientNonce); err != nil {
		return fmt.Errorf("failed to generate client bootstrap nonce: %w", err)
	}

	resp, err := s.client.SyncKeyring(ctx, &SyncKeyringRequest{
		RelationshipId:        s.relationshipID,
		ClientEphemeralPubkey: clientPriv.PublicKey().Bytes(),
		ClientNonce:           clientNonce,
	})
	if err != nil {
		return fmt.Errorf("SyncKeyring RPC failed: %w", err)
	}

	// Check for cancellation after RPC completes.
	if s.isStopped() {
		return fmt.Errorf("bootstrap aborted: secondary stopped")
	}

	rootKey, err := unwrapRootKeyFromPrimary(
		resp.WrappedRootKey,
		s.relationshipID,
		resp.ServerEphemeralPubkey,
		clientPriv,
		clientNonce,
		resp.WrapNonce,
		resp.WrapAadVersion,
	)
	if err != nil {
		return fmt.Errorf("failed to unwrap root key from primary: %w", err)
	}

	// Step 1: Set the primary's plaintext root key in the barrier's
	// in-memory keyring. This is needed because ReloadKeyring uses
	// keyring.RootKey() to decrypt core/keyring.
	if err := s.core.barrier.SetRootKey(rootKey); err != nil {
		return fmt.Errorf("failed to set primary root key: %w", err)
	}
	s.logger.Info("primary root key set in barrier")

	// Step 2: Write the primary's encrypted keyring blob to physical storage.
	if len(resp.KeyringEntry) > 0 {
		if err := s.core.physical.Put(ctx, &physical.Entry{
			Key:   "core/keyring",
			Value: resp.KeyringEntry,
		}); err != nil {
			return fmt.Errorf("failed to write keyring: %w", err)
		}
	}

	// Step 3: Write the primary's root key entry to physical storage.
	if len(resp.RootKeyEntry) > 0 {
		if err := s.core.physical.Put(ctx, &physical.Entry{
			Key:   "core/root-key",
			Value: resp.RootKeyEntry,
		}); err != nil {
			return fmt.Errorf("failed to write root key entry: %w", err)
		}
	}

	// Step 4: Reload the barrier keyring. Now that the in-memory root
	// key matches the primary's, this will successfully decrypt the
	// primary's core/keyring blob and load all term keys.
	if err := s.core.barrier.ReloadKeyring(ctx); err != nil {
		return fmt.Errorf("failed to reload keyring after bootstrap: %w", err)
	}
	s.logger.Info("barrier keyring reloaded with primary's keys")

	// Check for cancellation before persisting to seal.
	if s.isStopped() {
		return fmt.Errorf("bootstrap aborted: secondary stopped")
	}

	// Step 5: Persist the primary's root key under the secondary's seal
	// so the secondary can unseal after a restart. The operator still
	// provides the secondary's own unseal keys, which decrypt to get
	// the primary's root key, which then decrypts the primary's keyring.
	if err := s.core.seal.SetStoredKeys(ctx, [][]byte{rootKey}); err != nil {
		return fmt.Errorf("failed to persist primary root key in seal: %w", err)
	}
	s.logger.Info("primary root key persisted in secondary seal")

	// Step 6: Purge stale barrier entries from the secondary's own
	// initialization. After the root key swap, these entries are
	// encrypted with the old root key and can no longer be decrypted.
	// Leaving them in storage causes API failures ("decryption failed")
	// when OpenBao tries to read mount tables, tokens, etc.
	if err := s.purgeStaleBarrierEntries(ctx); err != nil {
		return fmt.Errorf("failed to purge stale entries: %w", err)
	}

	s.keyringBootstrapped.Store(true)
	s.logger.Info("keyring bootstrap complete")
	return nil
}

// drBootstrapPreservePaths lists physical storage paths that must be
// preserved during the post-keyring-bootstrap purge. These are either
// already replaced with the primary's versions, encrypted by the local
// seal (not the barrier), or critical for local cluster operation.
var drBootstrapPreservePaths = map[string]bool{
	"core/keyring":                 true, // replaced with primary's during bootstrap
	"core/root-key":                true, // replaced with primary's during bootstrap
	"core/hsm/barrier-unseal-keys": true, // encrypted by local seal, just updated
	"core/seal-config":             true, // unencrypted local seal config
}

// purgeStaleBarrierEntries removes all entries from physical storage
// that were written by the secondary's own initialization and are now
// unreadable after the root key swap. This gives reconciliation a
// clean slate to apply the primary's data.
func (s *drReplicationSecondary) purgeStaleBarrierEntries(ctx context.Context) error {
	s.logger.Info("purging stale secondary barrier entries")

	var deleted int
	err := s.physicalRecursiveDelete(ctx, "", &deleted)
	if err != nil {
		return err
	}

	s.logger.Info("stale barrier entries purged", "deleted", deleted)
	return nil
}

// physicalRecursiveDelete walks physical storage from the given prefix
// and deletes all entries not in the preservation list. Entries listed
// in drBootstrapPreservePaths are kept. Directories are listed
// recursively.
func (s *drReplicationSecondary) physicalRecursiveDelete(ctx context.Context, prefix string, deleted *int) error {
	keys, err := s.core.physical.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("failed to list physical storage at %q: %w", prefix, err)
	}

	for _, key := range keys {
		fullPath := prefix + key

		// Recurse into directories (keys ending with /).
		if strings.HasSuffix(key, "/") {
			if err := s.physicalRecursiveDelete(ctx, fullPath, deleted); err != nil {
				return err
			}
			continue
		}

		// Skip preserved paths.
		if drBootstrapPreservePaths[fullPath] {
			continue
		}

		// Delete the entry.
		if err := s.core.physical.Delete(ctx, fullPath); err != nil {
			s.logger.Warn("failed to delete stale entry", "path", fullPath, "error", err)
			continue
		}
		*deleted++
	}

	return nil
}

// invalidateCoreCaches flushes critical in-memory caches after the
// initial reconciliation so that subsequent API requests see the
// primary's data rather than stale secondary state.
func (s *drReplicationSecondary) invalidateCoreCaches(ctx context.Context) {
	s.logger.Info("invalidating core caches after initial sync")

	// Invalidate the token store's salt cache. The salt determines how
	// token IDs are hashed for lookup. After reconciliation the primary's
	// salt is in storage; clearing the cache forces a reload on the next
	// token check, enabling the primary's root token to work.
	if s.core.tokenStore != nil {
		s.core.tokenStore.Invalidate(ctx, "token/salt")
		s.logger.Info("token store salt cache invalidated")
	}

	// Invalidate the physical cache to ensure subsequent barrier reads
	// go to storage rather than returning stale cached entries.
	if s.core.physicalCache != nil {
		s.core.physicalCache.Purge(ctx)
		s.logger.Info("physical cache purged")
	}
}

// --- Stream mode ---

// runStream connects to the primary's change stream and applies
// entries as they arrive. Returns when the stream disconnects or a
// gap is detected.
func (s *drReplicationSecondary) runStream(ctx context.Context) error {
	s.logger.Info("starting change stream",
		"last_applied_index", s.lastAppliedIndex.Load())

	stream, err := s.client.StreamChanges(ctx, &StreamChangesRequest{
		RelationshipId:   s.relationshipID,
		LastAppliedIndex: s.lastAppliedIndex.Load(),
	})
	if err != nil {
		return fmt.Errorf("failed to open change stream: %w", err)
	}

	// Track the expected next index for gap detection.
	expectedNext := s.lastAppliedIndex.Load() + 1

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		default:
		}

		change, err := stream.Recv()
		if err == io.EOF {
			return fmt.Errorf("change stream ended")
		}
		if err != nil {
			return fmt.Errorf("change stream error: %w", err)
		}

		current := s.lastAppliedIndex.Load()
		if change.RaftIndex < current {
			// Stale delivery on reconnect/buffer replay.
			continue
		}

		// Gap detection: if we receive an index higher than expected,
		// entries were dropped. Fall back to reconciliation.
		if change.RaftIndex > expectedNext {
			s.logger.Warn("gap detected in change stream, falling back to reconciliation",
				"expected_index", expectedNext,
				"received_index", change.RaftIndex,
				"gap_size", change.RaftIndex-expectedNext)
			s.streamDisconnects.Add(1)
			return fmt.Errorf("change stream gap detected: expected index %d, got %d",
				expectedNext, change.RaftIndex)
		}

		applyStart := time.Now()
		if err := s.applyStreamChange(ctx, change); err != nil {
			return fmt.Errorf("failed to apply change: %w", err)
		}
		metrics.MeasureSince([]string{"replication", "dr", "secondary", "apply_latency"}, applyStart)

		s.lastAppliedIndex.Store(change.RaftIndex)
		s.entriesApplied.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "entries_applied"}, 1)
		metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(change.RaftIndex))
		expectedNext = change.RaftIndex + 1
	}
}

// applyStreamChange applies a single entry change from the FSM change
// stream to local physical storage. Stream values are already
// barrier-encrypted (they come from the primary's Raft log), so they
// must be written directly to the physical backend to avoid
// double-encryption.
func (s *drReplicationSecondary) applyStreamChange(ctx context.Context, change *EntryChange) error {
	// Skip cluster-local paths that should never be replicated.
	if isDRNeverReplicatePath(change.Key) {
		s.logger.Debug("skipping cluster-local path in stream", "key", change.Key)
		return nil
	}

	switch physical.Operation(change.OpType) {
	case physical.PutOperation:
		if err := s.core.physical.Put(ctx, &physical.Entry{
			Key:      change.Key,
			Value:    change.Value,
			SealWrap: change.SealWrap,
		}); err != nil {
			return err
		}

		// If the keyring or root key was updated, reload the barrier
		// so new encryption terms are picked up immediately.
		//
		// For root key rotation the order matters:
		//   1. ReloadRootKey reads core/root-key (encrypted with the
		//      active term key) and updates the in-memory root key.
		//   2. ReloadKeyring uses the (now updated) root key to decrypt
		//      core/keyring and load all term keys.
		//   3. Persist the root key under the secondary's seal so it
		//      survives restarts.
		//
		// If core/keyring arrives before core/root-key (ordering in
		// the Raft log), step 2 may fail temporarily. When core/root-key
		// arrives next, both steps succeed. Failures are non-fatal.
		if change.Key == "core/keyring" || change.Key == "core/root-key" {
			s.logger.Info("keyring/root-key update detected via stream", "key", change.Key)

			if err := s.core.barrier.ReloadRootKey(ctx); err != nil {
				s.logger.Warn("failed to reload root key after stream update", "error", err)
			}
			if err := s.core.barrier.ReloadKeyring(ctx); err != nil {
				s.logger.Warn("failed to reload keyring after stream update",
					"key", change.Key, "error", err)
			}

			// Persist updated root key under secondary's seal for restart survival.
			if keyring, err := s.core.barrier.Keyring(); err == nil {
				if err := s.core.seal.SetStoredKeys(ctx, [][]byte{keyring.RootKey()}); err != nil {
					s.logger.Error("failed to persist rotated root key in seal", "error", err)
				}
			}
		}
		return nil

	case physical.DeleteOperation:
		return s.core.physical.Delete(ctx, change.Key)

	default:
		s.logger.Warn("unknown operation type in change stream",
			"op_type", change.OpType, "key", change.Key)
		return nil
	}
}

// applyFetchedChange applies a single entry change received via
// FetchEntries (reconciliation). These values were read through the
// primary's barrier (decrypted), so they must be written through the
// secondary's barrier to re-encrypt them.
//
// kidToKey is an optional map for resolving KID-based deletes (where the
// primary sends a delete with only the KID, not the key). Pass nil if
// no KID resolution is needed.
func (s *drReplicationSecondary) applyFetchedChange(ctx context.Context, change *EntryChange, kidToKey ...map[[32]byte]string) error {
	var kidMap map[[32]byte]string
	if len(kidToKey) > 0 {
		kidMap = kidToKey[0]
	}
	return s.applyFetchedChangeWithKIDMap(ctx, change, kidMap)
}

func (s *drReplicationSecondary) applyFetchedChangeWithKIDMap(ctx context.Context, change *EntryChange, kidToKey map[[32]byte]string) error {
	// Skip cluster-local paths that should never be replicated.
	if change.Key != "" && isDRNeverReplicatePath(change.Key) {
		s.logger.Debug("skipping cluster-local path in fetch", "key", change.Key)
		return nil
	}

	switch physical.Operation(change.OpType) {
	case physical.PutOperation:
		entry := &logical.StorageEntry{
			Key:      change.Key,
			Value:    change.Value,
			SealWrap: change.SealWrap,
		}
		if err := s.core.barrier.Put(ctx, entry); err != nil {
			return err
		}

		// If the keyring or root key was updated, reload the barrier.
		// Same rotation-safe ordering as applyStreamChange: reload
		// root key first, then keyring, then persist to seal.
		if change.Key == "core/keyring" || change.Key == "core/root-key" {
			s.logger.Info("keyring/root-key update detected via fetch", "key", change.Key)

			if err := s.core.barrier.ReloadRootKey(ctx); err != nil {
				s.logger.Warn("failed to reload root key after fetch update", "error", err)
			}
			if err := s.core.barrier.ReloadKeyring(ctx); err != nil {
				s.logger.Warn("failed to reload keyring after fetch update",
					"key", change.Key, "error", err)
			}

			// Persist updated root key under secondary's seal.
			if keyring, err := s.core.barrier.Keyring(); err == nil {
				if err := s.core.seal.SetStoredKeys(ctx, [][]byte{keyring.RootKey()}); err != nil {
					s.logger.Error("failed to persist rotated root key in seal", "error", err)
				}
			}
		}
		return nil

	case physical.DeleteOperation:
		if change.Key != "" {
			return s.core.barrier.Delete(ctx, change.Key)
		}
		// If Key is empty but KID is present, resolve via local KID map.
		if len(change.Kid) == 32 && kidToKey != nil {
			var kid [32]byte
			copy(kid[:], change.Kid)
			if key, ok := kidToKey[kid]; ok {
				if isDRNeverReplicatePath(key) {
					return nil
				}
				return s.core.barrier.Delete(ctx, key)
			}
			s.logger.Warn("delete with KID but key not found in local map",
				"kid_prefix", fmt.Sprintf("%x", change.Kid[:8]))
		}
		return nil

	default:
		s.logger.Warn("unknown operation type in fetched change",
			"op_type", change.OpType, "key", change.Key)
		return nil
	}
}

// --- Reconciliation mode ---

// runReconciliation performs the full IBLT/prefix digest reconciliation
// protocol against the primary.
func (s *drReplicationSecondary) runReconciliation(ctx context.Context) error {
	s.logger.Info("starting reconciliation")
	startTime := time.Now()
	s.reconcileRPCBytesUsed.Store(0)
	s.rangeSplitCount.Store(0)
	s.reconcileBudgetRemainingByte.Store(int64(drRangeMaxReconcileRPCBytes))
	defer func() {
		metrics.MeasureSince([]string{"replication", "dr", "secondary", "reconciliation_duration"}, startTime)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "reconciliation_count"}, 1)
	}()

	// Step 1: Request a checkpoint from the primary.
	checkpoint, err := s.client.RequestCheckpoint(ctx, &CheckpointRequest{
		RelationshipId: s.relationshipID,
	})
	if err != nil {
		return fmt.Errorf("failed to request checkpoint: %w", err)
	}
	s.logger.Info("checkpoint established",
		"checkpoint_id", checkpoint.CheckpointId,
		"primary_commit_index", checkpoint.CommitIndex)
	s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex, len(checkpoint.GetTopRanges()))
	defer s.endReconcileSession()

	// Step 2: Build local reconciliation set.
	localCheckpoint := reconciler.Checkpoint{
		ID:          checkpoint.CheckpointId,
		CommitIndex: checkpoint.CommitIndex,
	}
	localSet, err := s.scanner.Scan(ctx, s.core.barrier, localCheckpoint)
	if err != nil {
		return fmt.Errorf("failed to scan local storage: %w", err)
	}
	s.logger.Info("local scan complete", "keys", localSet.KeyCount)

	// Range-first reconciliation path (primary advertises deterministic
	// top-level ranges). Falls back to legacy global strata/IBLT flow when
	// top_ranges are not provided.
	if len(checkpoint.GetTopRanges()) > 0 {
		if err := s.runRangeReconciliation(ctx, checkpoint, localSet, startTime); err != nil {
			return err
		}
		return nil
	}

	// Step 3: Exchange strata estimators to estimate difference size.
	primaryStrata, err := s.client.ExchangeStrataEstimator(ctx, &StrataMessage{
		CheckpointId:    checkpoint.CheckpointId,
		StrataData:      localSet.Strata.Marshal(),
		CheckpointIndex: checkpoint.CommitIndex,
	})
	if err != nil {
		return fmt.Errorf("failed to exchange strata: %w", err)
	}
	if err := s.assertActiveCheckpoint(primaryStrata.GetCheckpointId(), primaryStrata.GetCheckpointIndex()); err != nil {
		return fmt.Errorf("checkpoint conflict: %w", err)
	}

	remoteStrata, err := sketch.UnmarshalStrataEstimator(primaryStrata.StrataData)
	if err != nil {
		return fmt.Errorf("failed to unmarshal primary strata: %w", err)
	}

	estimatedDiff, err := localSet.Strata.Estimate(remoteStrata)
	if err != nil {
		return fmt.Errorf("failed to estimate difference: %w", err)
	}
	s.logger.Info("difference estimated", "estimated_diff", estimatedDiff)

	if estimatedDiff == 0 {
		s.logger.Info("no differences detected; reconciliation complete",
			"duration", time.Since(startTime))
		s.lastAppliedIndex.Store(checkpoint.CommitIndex)
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		return nil
	}

	// Step 4: Exchange IBLTs to decode the actual differences.
	ibltCells := uint32(math.Ceil(float64(estimatedDiff) * 1.5))
	if ibltCells < 3 {
		ibltCells = 3
	}

	// Build local IBLT at the right size.
	localIBLT, err := s.scanner.BuildIBLTFromScan(ctx, s.core.barrier, ibltCells)
	if err != nil {
		return fmt.Errorf("failed to build local IBLT: %w", err)
	}

	primaryIBLTMsg, err := s.client.ExchangeIBLT(ctx, &IBLTMessage{
		CheckpointId:    checkpoint.CheckpointId,
		NumCells:        ibltCells,
		CheckpointIndex: checkpoint.CommitIndex,
	})
	if err != nil {
		return fmt.Errorf("failed to exchange IBLT: %w", err)
	}
	if err := s.assertActiveCheckpoint(primaryIBLTMsg.GetCheckpointId(), primaryIBLTMsg.GetCheckpointIndex()); err != nil {
		return fmt.Errorf("checkpoint conflict: %w", err)
	}

	remoteIBLT, err := sketch.UnmarshalIBLT(primaryIBLTMsg.IbltData)
	if err != nil {
		return fmt.Errorf("failed to unmarshal primary IBLT: %w", err)
	}

	// Subtract: primary - secondary to find differences.
	diff, err := remoteIBLT.Subtract(localIBLT)
	if err != nil {
		return fmt.Errorf("IBLT subtract failed: %w", err)
	}

	added, removed, ok := diff.Decode()
	if !ok {
		s.logger.Warn("IBLT decode failed; falling back to prefix digest reconciliation")
		return s.runPrefixDigestReconciliation(ctx, checkpoint)
	}

	s.logger.Info("IBLT decode succeeded",
		"added_on_primary", len(added),
		"removed_on_primary", len(removed))

	// Step 5: Fetch entries for KIDs that exist on primary but not secondary
	// (or have different values).
	if len(added) > 0 {
		if err := s.fetchAndApplyEntries(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, added, localSet.KIDToKey); err != nil {
			return fmt.Errorf("failed to fetch entries: %w", err)
		}
	}

	// Step 6: Delete entries that exist on secondary but not primary.
	if len(removed) > 0 {
		s.logger.Info("removing entries not on primary", "count", len(removed))
		if err := s.applyRemovedEntries(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, localSet, removed); err != nil {
			return err
		}
	}

	s.lastAppliedIndex.Store(checkpoint.CommitIndex)
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())

	s.logger.Info("reconciliation complete",
		"added", len(added),
		"removed", len(removed),
		"duration", time.Since(startTime))

	return nil
}

type drRangeTask struct {
	span   reconciler.RangeSpan
	remote reconciler.RangeDescriptor
}

type drRangeBudget struct {
	start         time.Time
	rpcBytes      uint64
	rangesHandled int
	rangesSplit   int
}

func (b *drRangeBudget) check() error {
	if time.Since(b.start) > drRangeMaxReconcileWallTime {
		return fmt.Errorf("budget_exceeded: reconciliation wall-time exceeded")
	}
	if b.rpcBytes > drRangeMaxReconcileRPCBytes {
		return fmt.Errorf("budget_exceeded: reconcile RPC bytes exceeded (%d > %d)", b.rpcBytes, drRangeMaxReconcileRPCBytes)
	}
	return nil
}

func (b *drRangeBudget) addRPC(n uint64) error {
	b.rpcBytes += n
	return b.check()
}

func (s *drReplicationSecondary) runRangeReconciliation(ctx context.Context, checkpoint *CheckpointResponse, localSet *reconciler.ReconciliationSet, startTime time.Time) error {
	ownedSession := false
	if err := s.assertActiveCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex, len(checkpoint.TopRanges))
		ownedSession = true
	}
	if ownedSession {
		defer s.endReconcileSession()
	}
	s.logger.Info("starting range-first reconciliation",
		"checkpoint_id", checkpoint.CheckpointId,
		"top_ranges", len(checkpoint.TopRanges),
		"range_plan_version", checkpoint.RangePlanVersion)
	s.logger.Debug("range reconcile profile",
		"target_keys_per_range", drRangeTargetKeysPerRange,
		"target_value_bytes", drRangeTargetValueBytes,
		"max_inflight_range_tasks", drRangeMaxInflightTasks)
	if len(checkpoint.TopRanges) > drRangeMaxTopRanges {
		return fmt.Errorf("reconcile failure [invalid_range_manifest]: top range count %d exceeds max %d", len(checkpoint.TopRanges), drRangeMaxTopRanges)
	}

	budget := &drRangeBudget{start: startTime}
	queue := make([]drRangeTask, 0, len(checkpoint.TopRanges))
	s.reconcileRangesFailed.Store(0)
	s.reconcileRangesInflight.Store(0)
	s.reconcileBudgetRemainingByte.Store(int64(drRangeMaxReconcileRPCBytes))
	s.rangeSplitCount.Store(0)
	s.reconcileRPCBytesUsed.Store(0)

	for _, rd := range checkpoint.TopRanges {
		remote, err := protoRangeDigestToDescriptor(rd)
		if err != nil {
			return fmt.Errorf("reconcile failure [invalid_range_manifest]: %w", err)
		}
		local := reconciler.BuildRangeDigest(localSet, remote.Span, drSecondaryRangeMaxIBLTCellsPerRange)
		if !local.EqualDigest(remote) {
			queue = append(queue, drRangeTask{span: remote.Span, remote: remote})
		}
	}

	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_total"}, float32(len(checkpoint.TopRanges)))
	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_mismatched"}, float32(len(queue)))
	s.reconcileRangesInflight.Store(int64(len(queue)))
	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_inflight"}, float32(len(queue)))

	if len(queue) == 0 {
		s.reconcileBudgetRemainingByte.Store(int64(drRangeMaxReconcileRPCBytes))
		s.lastAppliedIndex.Store(checkpoint.CommitIndex)
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		s.logger.Info("range reconciliation: no mismatched ranges")
		return nil
	}
	var ibltCellsUsed uint64

	for i := 0; i < len(queue); i++ {
		if err := budget.check(); err != nil {
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
			s.reconcileRangesFailed.Add(1)
			s.reconcileRangesInflight.Store(int64(len(queue) - i))
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}

		task := queue[i]
		remainingQueue := len(queue) - i - 1
		if remainingQueue < 0 {
			remainingQueue = 0
		}
		s.reconcileRangesInflight.Store(int64(remainingQueue))
		metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_inflight"}, float32(remainingQueue))
		budget.rangesHandled++
		if budget.rangesHandled > drRangeMaxTotalRanges {
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [budget_exceeded]: range task count exceeded")
		}

		localDesc := reconciler.BuildRangeDigest(localSet, task.span, drSecondaryRangeMaxIBLTCellsPerRange)
		estimatedDiff := estimateRangeDiff(localDesc, task.remote)
		ibltCells := clampIBLTCells(uint32(math.Ceil(float64(estimatedDiff)*1.5)), drSecondaryRangeMaxIBLTCellsPerRange)

		localIBLT := reconciler.BuildRangeIBLT(localSet, task.span, ibltCells)
		req := &IBLTMessage{
			CheckpointId:    checkpoint.CheckpointId,
			NumCells:        ibltCells,
			Span:            rangeSpanToProto(task.span),
			CheckpointIndex: checkpoint.CommitIndex,
		}
		primaryIBLTMsg, err := s.client.ExchangeIBLT(ctx, req)
		if err != nil {
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [rpc_failed]: exchange IBLT failed: %w", err)
		}
		if err := s.assertActiveCheckpoint(primaryIBLTMsg.GetCheckpointId(), primaryIBLTMsg.GetCheckpointIndex()); err != nil {
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [checkpoint_conflict]: %w", err)
		}
		ibltCellsUsed += uint64(ibltCells)
		metrics.SetGauge([]string{"replication", "dr", "reconcile", "iblt_cells_used"}, float32(ibltCellsUsed))
		if err := budget.addRPC(uint64(len(primaryIBLTMsg.GetIbltData()) + 256)); err != nil {
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}
		metrics.SetGauge([]string{"replication", "dr", "reconcile", "rpc_bytes_used"}, float32(budget.rpcBytes))
		s.reconcileRPCBytesUsed.Store(budget.rpcBytes)
		if budget.rpcBytes >= drRangeMaxReconcileRPCBytes {
			s.reconcileBudgetRemainingByte.Store(0)
		} else {
			s.reconcileBudgetRemainingByte.Store(int64(drRangeMaxReconcileRPCBytes - budget.rpcBytes))
		}

		remoteIBLT, err := sketch.UnmarshalIBLT(primaryIBLTMsg.IbltData)
		if err != nil {
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [decode_failed]: unmarshal IBLT: %w", err)
		}
		diff, err := remoteIBLT.Subtract(localIBLT)
		if err != nil {
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [decode_failed]: IBLT subtract: %w", err)
		}
		added, removed, ok := diff.Decode()
		if ok {
			if len(added) > 0 {
				if err := s.fetchAndApplyEntriesWithBudget(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, added, localSet.KIDToKey, budget); err != nil {
					s.reconcileRangesFailed.Add(1)
					return fmt.Errorf("reconcile failure [apply_failed]: %w", err)
				}
			}
			if len(removed) > 0 {
				if err := s.applyRemovedEntries(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, localSet, removed); err != nil {
					s.reconcileRangesFailed.Add(1)
					return fmt.Errorf("reconcile failure [apply_failed]: %w", err)
				}
			}
			continue
		}

		metrics.IncrCounter([]string{"replication", "dr", "reconcile", "range_decode_failures"}, 1)
		// Adaptive split first.
		if task.span.SplitDepth < drRangeMaxSplitDepth && len(queue)+1 < drRangeMaxTotalRanges {
			left, right, splitOK := reconciler.SplitRange(task.span)
			if splitOK {
				queue = append(queue, drRangeTask{span: left}, drRangeTask{span: right})
				budget.rangesSplit++
				s.rangeSplitCount.Store(uint64(budget.rangesSplit))
				metrics.IncrCounter([]string{"replication", "dr", "reconcile", "ranges_split"}, 1)
				continue
			}
		}

		// Optional in-range prefix refinement for stubborn ranges.
		if err := s.runRangePrefixRefinement(ctx, checkpoint, localSet, task.span, budget); err != nil {
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [decode_exhausted]: %w", err)
		}
	}
	s.reconcileRangesInflight.Store(0)
	if budget.rpcBytes >= drRangeMaxReconcileRPCBytes {
		s.reconcileBudgetRemainingByte.Store(0)
	} else {
		s.reconcileBudgetRemainingByte.Store(int64(drRangeMaxReconcileRPCBytes - budget.rpcBytes))
	}

	s.lastAppliedIndex.Store(checkpoint.CommitIndex)
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("range-first reconciliation complete",
		"ranges_handled", budget.rangesHandled,
		"ranges_split", budget.rangesSplit,
		"rpc_bytes", budget.rpcBytes,
		"duration", time.Since(startTime))
	return nil
}

func (s *drReplicationSecondary) runRangePrefixRefinement(ctx context.Context, checkpoint *CheckpointResponse, localSet *reconciler.ReconciliationSet, span reconciler.RangeSpan, budget *drRangeBudget) error {
	var finalPrefix uint32
	var mismatched []uint32

	for _, p := range []uint32{8, 12, 16} {
		localPD := reconciler.BuildRangePrefixDigest(localSet, span, p)
		resp, err := s.client.ExchangePrefixDigests(ctx, &PrefixDigestRequest{
			CheckpointId:    checkpoint.CheckpointId,
			PrefixLength:    p,
			Span:            rangeSpanToProto(span),
			CheckpointIndex: checkpoint.CommitIndex,
		})
		if err != nil {
			return fmt.Errorf("prefix refinement RPC failed: %w", err)
		}
		if err := budget.addRPC(uint64(len(resp.Buckets)*96 + 128)); err != nil {
			return err
		}

		remotePD := sketch.NewPrefixDigest(resp.PrefixLength)
		for _, b := range resp.Buckets {
			var xorKey, xorVal [32]byte
			copy(xorKey[:], b.XorKeyHash)
			copy(xorVal[:], b.XorValueHash)
			bucket := remotePD.Bucket(b.Index)
			if bucket != nil {
				bucket.Count = b.Count
				bucket.XORKeyHash = xorKey
				bucket.XORValueHash = xorVal
			}
		}

		mm, err := localPD.Compare(remotePD)
		if err != nil {
			return fmt.Errorf("prefix refinement compare failed: %w", err)
		}
		if len(mm) == 0 {
			return nil
		}
		mismatched = mm
		finalPrefix = p
		if p == 16 || len(mm) <= 16 {
			break
		}
	}

	if len(mismatched) == 0 {
		return nil
	}
	if err := budget.check(); err != nil {
		return err
	}

	fetchStream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
		CheckpointId:       checkpoint.CheckpointId,
		IncludeDeletes:     true,
		BucketPrefixLength: finalPrefix,
		BucketIndices:      mismatched,
		Ranges:             []*RangeSpan{rangeSpanToProto(span)},
		CheckpointIndex:    checkpoint.CommitIndex,
	})
	if err != nil {
		return fmt.Errorf("prefix refinement fetch failed: %w", err)
	}

	primaryKeys := make(map[string]bool)
	var failedKids [][]byte
	applyFailures := 0
	applyExamples := make([]string, 0, 5)

	for {
		batch, err := fetchStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("prefix refinement fetch stream error: %w", err)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}
		if err := budget.addRPC(uint64(len(batch.Entries)*128 + len(batch.FailedKids)*32)); err != nil {
			return err
		}
		for _, entry := range batch.Entries {
			if err := s.applyFetchedChange(ctx, entry, localSet.KIDToKey); err != nil {
				applyFailures++
				label := entry.Key
				if label == "" && len(entry.Kid) >= 8 {
					label = fmt.Sprintf("kid:%x", entry.Kid[:8])
				}
				if len(applyExamples) < cap(applyExamples) {
					applyExamples = append(applyExamples, label)
				}
			}
			if entry.Key != "" {
				primaryKeys[entry.Key] = true
			}
		}
		if len(batch.FailedKids) > 0 {
			failedKids = append(failedKids, batch.FailedKids...)
		}
	}

	if len(failedKids) > 0 {
		return fmt.Errorf("prefix refinement unresolved failed kids: %d", len(failedKids))
	}
	if applyFailures > 0 {
		return fmt.Errorf("prefix refinement apply failures: %d (examples: %v)", applyFailures, applyExamples)
	}

	// Remove secondary-only entries inside this span and mismatched buckets.
	if err := s.validateDeleteSafety(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("checkpoint conflict: %w", err)
	}
	mismatchSet := make(map[uint32]bool, len(mismatched))
	for _, idx := range mismatched {
		mismatchSet[idx] = true
	}
	deleteFailures := 0
	deleteExamples := make([]string, 0, 5)
	for kid, key := range localSet.KIDToKey {
		if !span.Contains(kid) {
			continue
		}
		bucketIdx := sketch.ParentBucket(kid, finalPrefix)
		if !mismatchSet[bucketIdx] {
			continue
		}
		if primaryKeys[key] || isDRNeverReplicatePath(key) {
			continue
		}
		if err := s.core.barrier.Delete(ctx, key); err != nil {
			deleteFailures++
			if len(deleteExamples) < cap(deleteExamples) {
				deleteExamples = append(deleteExamples, key)
			}
		}
	}
	if deleteFailures > 0 {
		return fmt.Errorf("prefix refinement delete failures: %d (examples: %v)", deleteFailures, deleteExamples)
	}
	return nil
}

func protoRangeDigestToDescriptor(d *RangeDigest) (reconciler.RangeDescriptor, error) {
	if d == nil || d.Span == nil {
		return reconciler.RangeDescriptor{}, fmt.Errorf("missing range digest span")
	}
	if len(d.Span.StartKid) != 32 || len(d.Span.EndKid) != 32 {
		return reconciler.RangeDescriptor{}, fmt.Errorf("range span start/end must be 32 bytes")
	}
	if len(d.XorKeyHash) != 32 || len(d.XorValueHash) != 32 {
		return reconciler.RangeDescriptor{}, fmt.Errorf("range digest xor hashes must be 32 bytes")
	}
	var span reconciler.RangeSpan
	copy(span.StartKID[:], d.Span.StartKid)
	copy(span.EndKID[:], d.Span.EndKid)
	span.SplitDepth = d.Span.SplitDepth
	if !span.Valid() {
		return reconciler.RangeDescriptor{}, fmt.Errorf("invalid range span bounds")
	}

	var xorKey, xorVal [32]byte
	copy(xorKey[:], d.XorKeyHash)
	copy(xorVal[:], d.XorValueHash)
	return reconciler.RangeDescriptor{
		Span:               span,
		Count:              d.Count,
		XORKeyHash:         xorKey,
		XORValueHash:       xorVal,
		SuggestedIBLTCells: d.SuggestedIbltCells,
		ApproxValueBytes:   d.ApproxValueBytes,
	}, nil
}

func rangeSpanToProto(span reconciler.RangeSpan) *RangeSpan {
	return &RangeSpan{
		StartKid:   span.StartKID[:],
		EndKid:     span.EndKID[:],
		SplitDepth: span.SplitDepth,
	}
}

func estimateRangeDiff(local, remote reconciler.RangeDescriptor) int {
	if local.Count == remote.Count &&
		local.XORKeyHash == remote.XORKeyHash &&
		local.XORValueHash == remote.XORValueHash {
		return 0
	}
	// If remote is unknown (split children), derive a conservative estimate.
	if remote.Count == 0 && remote.XORKeyHash == ([32]byte{}) && remote.XORValueHash == ([32]byte{}) {
		est := int(local.Count / 8)
		if est < 1 {
			est = 1
		}
		return est
	}
	if local.Count > remote.Count {
		return int(local.Count - remote.Count + 1)
	}
	return int(remote.Count - local.Count + 1)
}

func clampIBLTCells(cells uint32, max uint32) uint32 {
	if cells < sketch.DefaultHashCount {
		cells = sketch.DefaultHashCount
	}
	if max > 0 && cells > max {
		return max
	}
	return cells
}

func (s *drReplicationSecondary) applyRemovedEntries(ctx context.Context, checkpointID string, checkpointIndex uint64, localSet *reconciler.ReconciliationSet, removed []sketch.DiffEntry) error {
	if len(removed) == 0 {
		return nil
	}
	if err := s.validateDeleteSafety(checkpointID, checkpointIndex); err != nil {
		return fmt.Errorf("checkpoint conflict: %w", err)
	}
	deleteFailures := 0
	deleteExamples := make([]string, 0, 5)
	if localSet.KIDToKey != nil {
		for _, entry := range removed {
			if key, ok := localSet.KIDToKey[entry.KID]; ok {
				if isDRNeverReplicatePath(key) {
					continue
				}
				if err := s.core.barrier.Delete(ctx, key); err != nil {
					deleteFailures++
					if len(deleteExamples) < cap(deleteExamples) {
						deleteExamples = append(deleteExamples, key)
					}
				}
			}
		}
	}
	if deleteFailures > 0 {
		return fmt.Errorf("reconciliation delete failed for %d entries (examples: %v)", deleteFailures, deleteExamples)
	}
	return nil
}

// runPrefixDigestReconciliation is the fallback when IBLT decode fails.
// It uses adaptive prefix digest drill-down to identify divergent
// regions, then fetches entries for those regions.
func (s *drReplicationSecondary) runPrefixDigestReconciliation(ctx context.Context, checkpoint *CheckpointResponse) error {
	s.logger.Info("starting prefix digest reconciliation",
		"checkpoint_id", checkpoint.CheckpointId)

	// Build local prefix digest at p=8.
	localCheckpoint := reconciler.Checkpoint{
		ID:          checkpoint.CheckpointId,
		CommitIndex: checkpoint.CommitIndex,
	}
	localSet, err := s.scanner.Scan(ctx, s.core.barrier, localCheckpoint)
	if err != nil {
		return fmt.Errorf("failed to scan for prefix digest: %w", err)
	}

	// Get primary's prefix digests.
	resp, err := s.client.ExchangePrefixDigests(ctx, &PrefixDigestRequest{
		CheckpointId:    checkpoint.CheckpointId,
		PrefixLength:    localSet.PrefixDigest.PrefixLen(),
		CheckpointIndex: checkpoint.CommitIndex,
	})
	if err != nil {
		return fmt.Errorf("failed to exchange prefix digests: %w", err)
	}

	// Compare buckets to find mismatches.
	remotePD := sketch.NewPrefixDigest(resp.PrefixLength)
	for _, b := range resp.Buckets {
		var xorKey, xorVal [32]byte
		copy(xorKey[:], b.XorKeyHash)
		copy(xorVal[:], b.XorValueHash)

		bucket := remotePD.Bucket(b.Index)
		if bucket != nil {
			bucket.Count = b.Count
			bucket.XORKeyHash = xorKey
			bucket.XORValueHash = xorVal
		}
	}

	mismatched, err := localSet.PrefixDigest.Compare(remotePD)
	if err != nil {
		return fmt.Errorf("prefix digest compare failed: %w", err)
	}

	s.logger.Info("prefix digest comparison",
		"mismatched_buckets", len(mismatched),
		"total_buckets", localSet.PrefixDigest.NumBuckets())

	if len(mismatched) == 0 {
		s.logger.Info("no differences found via prefix digest")
		s.lastAppliedIndex.Store(checkpoint.CommitIndex)
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		return nil
	}

	// Request ALL entries in mismatched buckets from the primary using
	// bucket-based fetching. This ensures primary-only entries (not known
	// to the secondary) are included in the response.
	s.logger.Info("fetching entries for mismatched buckets",
		"bucket_count", len(mismatched),
		"prefix_length", localSet.PrefixDigest.PrefixLen())

	fetchStream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
		CheckpointId:       checkpoint.CheckpointId,
		IncludeDeletes:     true,
		BucketPrefixLength: localSet.PrefixDigest.PrefixLen(),
		BucketIndices:      mismatched,
		CheckpointIndex:    checkpoint.CommitIndex,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch entries: %w", err)
	}

	// Track which keys the primary sent so we can delete local-only
	// entries in the mismatched buckets afterwards.
	primaryKeys := make(map[string]bool)
	appliedCount := 0
	applyFailures := 0
	applyExamples := make([]string, 0, 5)
	var failedKids [][]byte

	for {
		batch, err := fetchStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("fetch stream error: %w", err)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}

		for _, entry := range batch.Entries {
			if err := s.applyFetchedChange(ctx, entry, localSet.KIDToKey); err != nil {
				applyFailures++
				label := entry.Key
				if label == "" && len(entry.Kid) >= 8 {
					label = fmt.Sprintf("kid:%x", entry.Kid[:8])
				}
				if len(applyExamples) < cap(applyExamples) {
					applyExamples = append(applyExamples, label)
				}
			}
			if entry.Key != "" {
				primaryKeys[entry.Key] = true
			}
			appliedCount++
		}

		if len(batch.FailedKids) > 0 {
			failedKids = append(failedKids, batch.FailedKids...)
		}
	}
	if len(failedKids) > 0 {
		retryStream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
			CheckpointId:    checkpoint.CheckpointId,
			Kids:            failedKids,
			IncludeDeletes:  true,
			CheckpointIndex: checkpoint.CommitIndex,
		})
		if err != nil {
			return fmt.Errorf("failed to retry prefix-digest failed kids: %w", err)
		}
		failedKids = failedKids[:0]
		for {
			batch, err := retryStream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("prefix-digest retry fetch stream error: %w", err)
			}
			if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
				return fmt.Errorf("checkpoint conflict: %w", err)
			}
			for _, entry := range batch.Entries {
				if err := s.applyFetchedChange(ctx, entry, localSet.KIDToKey); err != nil {
					applyFailures++
					label := entry.Key
					if label == "" && len(entry.Kid) >= 8 {
						label = fmt.Sprintf("kid:%x", entry.Kid[:8])
					}
					if len(applyExamples) < cap(applyExamples) {
						applyExamples = append(applyExamples, label)
					}
				}
			}
			if len(batch.FailedKids) > 0 {
				failedKids = append(failedKids, batch.FailedKids...)
			}
		}
		if len(failedKids) > 0 {
			return fmt.Errorf("prefix-digest fetch failed for %d keys after retry", len(failedKids))
		}
	}
	if applyFailures > 0 {
		return fmt.Errorf("failed to apply %d fetched prefix-digest entries (examples: %v)", applyFailures, applyExamples)
	}

	// Delete entries that exist locally in mismatched buckets but
	// were not sent by the primary (secondary-only entries).
	if localSet.KIDToKey != nil {
		if err := s.validateDeleteSafety(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}
		mismatchSet := make(map[uint32]bool, len(mismatched))
		for _, idx := range mismatched {
			mismatchSet[idx] = true
		}
		deletedCount := 0
		deleteFailures := 0
		deleteExamples := make([]string, 0, 5)
		for kid, key := range localSet.KIDToKey {
			bucketIdx := sketch.ParentBucket(kid, localSet.PrefixDigest.PrefixLen())
			if !mismatchSet[bucketIdx] {
				continue
			}
			if primaryKeys[key] {
				continue // Primary also has this key; keep it.
			}
			if isDRNeverReplicatePath(key) {
				continue
			}
			if err := s.core.barrier.Delete(ctx, key); err != nil {
				deleteFailures++
				if len(deleteExamples) < cap(deleteExamples) {
					deleteExamples = append(deleteExamples, key)
				}
			} else {
				deletedCount++
			}
		}
		if deleteFailures > 0 {
			return fmt.Errorf("failed to delete %d secondary-only entries (examples: %v)", deleteFailures, deleteExamples)
		}
		if deletedCount > 0 {
			s.logger.Info("deleted secondary-only entries in mismatched buckets",
				"count", deletedCount)
		}
	}

	s.lastAppliedIndex.Store(checkpoint.CommitIndex)
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())

	s.logger.Info("prefix digest reconciliation complete",
		"applied", appliedCount)
	return nil
}

// fetchAndApplyEntries requests entries from the primary by KID and
// applies them to local storage. The kidToKey map is used to resolve
// KID-based deletes returned by the primary.
func (s *drReplicationSecondary) fetchAndApplyEntries(ctx context.Context, checkpointID string, checkpointIndex uint64, entries []sketch.DiffEntry, kidToKey map[[32]byte]string) error {
	return s.fetchAndApplyEntriesWithBudget(ctx, checkpointID, checkpointIndex, entries, kidToKey, nil)
}

func (s *drReplicationSecondary) fetchAndApplyEntriesWithBudget(ctx context.Context, checkpointID string, checkpointIndex uint64, entries []sketch.DiffEntry, kidToKey map[[32]byte]string, budget *drRangeBudget) error {
	kids := make([][]byte, len(entries))
	items := make([]*FetchItem, 0, len(entries))
	expectedByKid := make(map[[32]byte][32]byte, len(entries))
	for i, e := range entries {
		kids[i] = make([]byte, 32)
		copy(kids[i], e.KID[:])
		item := &FetchItem{
			Kid:         make([]byte, 32),
			ExpectedVid: make([]byte, 32),
		}
		copy(item.Kid, e.KID[:])
		copy(item.ExpectedVid, e.VID[:])
		items = append(items, item)
		expectedByKid[e.KID] = e.VID
	}

	stream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
		CheckpointId:    checkpointID,
		CheckpointIndex: checkpointIndex,
		Kids:            kids,
		Items:           items,
		IncludeDeletes:  true,
	})
	if err != nil {
		return fmt.Errorf("failed to open fetch stream: %w", err)
	}

	count := 0
	var failedKids [][]byte
	applyFailures := 0
	applyExamples := make([]string, 0, 5)
	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("fetch stream error: %w", err)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}
		if budget != nil {
			if err := budget.addRPC(uint64(len(batch.Entries)*128 + len(batch.FailedKids)*32)); err != nil {
				return err
			}
		}

		for _, entry := range batch.Entries {
			if err := s.applyFetchedChange(ctx, entry, kidToKey); err != nil {
				applyFailures++
				label := entry.Key
				if label == "" && len(entry.Kid) >= 8 {
					label = fmt.Sprintf("kid:%x", entry.Kid[:8])
				}
				if len(applyExamples) < cap(applyExamples) {
					applyExamples = append(applyExamples, label)
				}
			}
			count++
		}
		if len(batch.FailedKids) > 0 {
			failedKids = append(failedKids, batch.FailedKids...)
		}
	}

	if len(failedKids) > 0 {
		s.logger.Warn("retrying failed fetch kids", "count", len(failedKids))
		retryItems := make([]*FetchItem, 0, len(failedKids))
		for _, kidBytes := range failedKids {
			if len(kidBytes) != 32 {
				continue
			}
			var kid [32]byte
			copy(kid[:], kidBytes)
			if vid, ok := expectedByKid[kid]; ok {
				retryItems = append(retryItems, &FetchItem{
					Kid:         append([]byte(nil), kidBytes...),
					ExpectedVid: append([]byte(nil), vid[:]...),
				})
			}
		}
		retryStream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
			CheckpointId:    checkpointID,
			CheckpointIndex: checkpointIndex,
			Kids:            failedKids,
			Items:           retryItems,
			IncludeDeletes:  true,
		})
		if err != nil {
			return fmt.Errorf("failed to retry fetch for failed kids: %w", err)
		}

		failedKids = failedKids[:0]
		for {
			batch, err := retryStream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("retry fetch stream error: %w", err)
			}
			if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
				return fmt.Errorf("checkpoint conflict: %w", err)
			}
			if budget != nil {
				if err := budget.addRPC(uint64(len(batch.Entries)*128 + len(batch.FailedKids)*32)); err != nil {
					return err
				}
			}
			for _, entry := range batch.Entries {
				if err := s.applyFetchedChange(ctx, entry, kidToKey); err != nil {
					applyFailures++
					label := entry.Key
					if label == "" && len(entry.Kid) >= 8 {
						label = fmt.Sprintf("kid:%x", entry.Kid[:8])
					}
					if len(applyExamples) < cap(applyExamples) {
						applyExamples = append(applyExamples, label)
					}
				}
				count++
			}
			if len(batch.FailedKids) > 0 {
				failedKids = append(failedKids, batch.FailedKids...)
			}
		}
		if len(failedKids) > 0 {
			return fmt.Errorf("fetch failed for %d keys after retry", len(failedKids))
		}
	}
	if applyFailures > 0 {
		return fmt.Errorf("failed to apply %d fetched entries (examples: %v)", applyFailures, applyExamples)
	}

	s.logger.Info("fetched and applied entries", "count", count)
	return nil
}
