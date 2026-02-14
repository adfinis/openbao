// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"sort"
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
	"core/lock":                    true, // local init/leadership coordination lock
	"core/initialize-lock":         true, // local init lock state
	"core/recovery-config":         true, // local recovery seal config
	"core/recovery-key":            true, // local recovery key material
	"core/cluster/local/info":      true, // local cluster identity info
	"core/dr-replication/config":   true, // local DR manager mode/config
}

// drNeverReplicatePrefixes lists path prefixes that must never be
// replicated (root or namespaced forms).
var drNeverReplicatePrefixes = []string{
	"core/local-mounts/",
	"core/local-auth/",
	"core/local-audit/",
	"core/cluster/local/",
	"core/leader/",
	"core/raft/",
	"core/dr-replication/",
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
	DRSecondaryIdle           DRSecondaryState = iota
	DRSecondaryBootstrapping                   // Initial setup / token exchange
	DRSecondaryInitialSync                     // First full sync from primary
	DRSecondaryStreaming                       // Normal mode: receiving change stream
	DRSecondaryReconciling                     // Recovery mode: IBLT/prefix digest reconciliation
	DRSecondaryResnapshotting                  // Hard-cutover full copy fallback
	DRSecondaryPromoting                       // Failover in progress
	DRSecondaryStandalone                      // Post-promotion: now an independent primary
)

const (
	drRangeTargetKeysPerRange            = 12000
	drRangeTargetValueBytes              = 8 << 20 // 8 MiB
	drRangeMaxTopRanges                  = 256
	drRangeMaxTotalRanges                = 1024
	drRangeMaxOutstandingTasks           = 384
	drRangeMaxSessionSplits              = 192
	drRangeMaxSplitDepth                 = 6
	drSecondaryRangeMaxIBLTCellsPerRange = 32768

	drDefaultReconcileMaxRPCBytes      = 128 << 20
	drDefaultReconcileMaxWallTime      = 30 * time.Minute
	drDefaultReconcileMaxInflightTasks = 16
	drDefaultReconcileStallAbort       = 90 * time.Second
	drDefaultRPCDeadline               = 30 * time.Second
	drDefaultStreamBatchMaxEntries     = 256
	drDefaultStreamBatchMaxBytes       = 1 << 20 // 1 MiB
	drDefaultStreamBatchMaxWait        = 10 * time.Millisecond

	drDefaultFallbackEnabled          = true
	drDefaultFallbackStall            = 180 * time.Second
	drDefaultFallbackFailureThreshold = 3
	drDefaultFallbackWindow           = 10 * time.Minute
	drDefaultFallbackCooldown         = 10 * time.Minute
	drDefaultFallbackMaxPerHour       = 2
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
	case DRSecondaryResnapshotting:
		return "resnapshotting"
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

	streamMu     sync.Mutex
	streamCancel context.CancelFunc

	// state tracks the secondary's replication state.
	state atomic.Int32

	// lastAppliedIndex is the primary's Raft index of the last
	// successfully applied entry.
	lastAppliedIndex atomic.Uint64
	lastAppliedAt    atomic.Int64 // unix timestamp

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
	reconcileQueueDepth          atomic.Int64
	reconcileStalled             atomic.Uint64
	lastReconcileActivityAt      atomic.Int64 // unix timestamp

	sessionMu                  sync.RWMutex
	activeCheckpointID         string
	activeCheckpointIndex      uint64
	sessionStart               time.Time
	streamPausedAt             uint64
	lastReconcileFailReason    string
	lastRangeManifestCount     int
	lastReconcileFailureByType map[string]uint64

	// Additional heavy-load observability counters.
	scanFailures            atomic.Uint64
	checkpointConflicts     atomic.Uint64
	reconcileRetries        atomic.Uint64
	reconcileTaskRetries    atomic.Uint64
	reconcileDecodeFailures atomic.Uint64

	// Runtime tunables.
	reconcileMaxRPCBytes      uint64
	reconcileMaxWallTime      time.Duration
	reconcileMaxInflightTasks int
	reconcileStallAbort       time.Duration
	rpcDeadline               time.Duration
	streamBatchMaxEntries     int
	streamBatchMaxBytes       int
	streamBatchMaxWait        time.Duration

	primaryIndex atomic.Uint64

	fallbackEnabled          bool
	fallbackStall            time.Duration
	fallbackFailureThreshold int
	fallbackMinLagEntries    uint64
	fallbackCooldown         time.Duration
	fallbackMaxPerHour       int
	fallbackWindow           time.Duration
	fallbackActive           atomic.Bool
	fallbackCount            atomic.Uint64
	fallbackLastAt           atomic.Int64

	fallbackMu             sync.Mutex
	fallbackFailureEvents  []time.Time
	fallbackTriggerEvents  []time.Time
	fallbackLastReason     string
	manualResnapshotReason string
	manualResnapshotReq    atomic.Bool

	reconcileTasksHandled   atomic.Uint64
	reconcileTaskRateMillis atomic.Uint64
}

// newDRReplicationSecondary creates a new secondary replication manager.
func newDRReplicationSecondary(core *Core, replSalt []byte, relationshipID string, logger log.Logger) *drReplicationSecondary {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true // Secondary needs reverse KID->key mapping for deletes
	config.RequireTransactionalSnapshot = true
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

	sec := &drReplicationSecondary{
		logger:                     logger.Named("dr-secondary"),
		core:                       core,
		scanner:                    reconciler.NewScanner(config),
		relationshipID:             relationshipID,
		replSalt:                   replSalt,
		stopCh:                     make(chan struct{}),
		lastReconcileFailureByType: make(map[string]uint64),
		reconcileMaxRPCBytes:       drDefaultReconcileMaxRPCBytes,
		reconcileMaxWallTime:       drDefaultReconcileMaxWallTime,
		reconcileMaxInflightTasks:  drDefaultReconcileMaxInflightTasks,
		reconcileStallAbort:        drDefaultReconcileStallAbort,
		rpcDeadline:                drDefaultRPCDeadline,
		streamBatchMaxEntries:      drDefaultStreamBatchMaxEntries,
		streamBatchMaxBytes:        drDefaultStreamBatchMaxBytes,
		streamBatchMaxWait:         drDefaultStreamBatchMaxWait,
		fallbackEnabled:            drDefaultFallbackEnabled,
		fallbackStall:              drDefaultFallbackStall,
		fallbackFailureThreshold:   drDefaultFallbackFailureThreshold,
		fallbackCooldown:           drDefaultFallbackCooldown,
		fallbackMaxPerHour:         drDefaultFallbackMaxPerHour,
		fallbackWindow:             drDefaultFallbackWindow,
	}
	now := time.Now().UTC().Unix()
	sec.lastAppliedAt.Store(now)
	sec.lastReconcileActivityAt.Store(now)
	return sec
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
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go s.runHeartbeatLoop(heartbeatCtx)

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
				s.reconcileRetries.Add(1)
				if !s.shouldRetryReconcile(class) {
					cooldown := s.retryCapCooldown(class)
					s.logger.Warn("reconciliation retry cap reached; entering cooldown",
						"class", class,
						"cooldown", cooldown)
					time.Sleep(cooldown)
					continue
				}
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
			if requested, _ := s.consumeResnapshotRequest(); requested {
				s.setState(DRSecondaryResnapshotting)
				continue
			}
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
			if requested, reason := s.consumeResnapshotRequest(); requested {
				s.setState(DRSecondaryResnapshotting)
				s.setFallbackLastReason(reason)
				continue
			}
			if err := s.runReconciliation(ctx); err != nil {
				s.logger.Error("reconciliation failed", "error", err)
				class := s.markReconcileFailure(err)
				s.recordFallbackFailure(class)
				if s.shouldTriggerFallback(class) {
					s.logger.Warn("triggering automatic resnapshot fallback",
						"class", class,
						"last_applied_index", s.lastAppliedIndex.Load(),
						"primary_index", s.primaryIndex.Load())
					s.setFallbackLastReason(fmt.Sprintf("auto:%s", class))
					s.setState(DRSecondaryResnapshotting)
					continue
				}
				s.reconcileRetries.Add(1)
				if !s.shouldRetryReconcile(class) {
					cooldown := s.retryCapCooldown(class)
					s.logger.Warn("reconciliation retry cap reached; entering cooldown",
						"class", class,
						"cooldown", cooldown)
					time.Sleep(cooldown)
					continue
				}
				time.Sleep(s.nextReconcileRetryDelay(class))
				continue
			}
			s.markReconcileSuccess()
			s.setState(DRSecondaryStreaming)

		case DRSecondaryResnapshotting:
			reason := s.getFallbackLastReason()
			if reason == "" {
				reason = "manual"
			}
			if err := s.performResnapshot(ctx, reason); err != nil {
				s.logger.Error("resnapshot fallback failed", "error", err)
				class := s.markReconcileFailure(wrapReconcileFailure(drReconcileFailureApplyFailed, "resnapshot", err))
				s.reconcileRetries.Add(1)
				time.Sleep(s.nextReconcileRetryDelay(class))
				s.setState(DRSecondaryReconciling)
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
	s.cancelActiveStream()

	// Remove the mTLS client from the cluster listener to avoid
	// leaking a stale client after stop or promote.
	if cl := s.core.getClusterListener(); cl != nil {
		cl.RemoveClient(consts.DRReplicationALPN)
	}
}

func (s *drReplicationSecondary) setActiveStreamCancel(cancel context.CancelFunc) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	s.streamCancel = cancel
}

func (s *drReplicationSecondary) cancelActiveStream() {
	s.streamMu.Lock()
	cancel := s.streamCancel
	s.streamCancel = nil
	s.streamMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RequestResnapshot asks the secondary loop to perform a full-copy fallback.
func (s *drReplicationSecondary) RequestResnapshot(reason string) {
	if reason == "" {
		reason = "manual"
	}
	s.fallbackMu.Lock()
	s.manualResnapshotReason = reason
	s.fallbackMu.Unlock()
	s.manualResnapshotReq.Store(true)
	s.cancelActiveStream()
}

func (s *drReplicationSecondary) consumeResnapshotRequest() (bool, string) {
	if !s.manualResnapshotReq.Swap(false) {
		return false, ""
	}
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	reason := s.manualResnapshotReason
	s.manualResnapshotReason = ""
	if reason == "" {
		reason = "manual"
	}
	return true, reason
}

func (s *drReplicationSecondary) setFallbackLastReason(reason string) {
	s.fallbackMu.Lock()
	s.fallbackLastReason = reason
	s.fallbackMu.Unlock()
}

func (s *drReplicationSecondary) getFallbackLastReason() string {
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	return s.fallbackLastReason
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

func (s *drReplicationSecondary) setLastAppliedIndex(index uint64) {
	s.lastAppliedIndex.Store(index)
	s.lastAppliedAt.Store(time.Now().UTC().Unix())
}

func (s *drReplicationSecondary) markReconcileActivityNow() {
	s.lastReconcileActivityAt.Store(time.Now().UTC().Unix())
}

func (s *drReplicationSecondary) reconcileStuckSeconds(now time.Time) int64 {
	lastActivity := s.lastReconcileActivityAt.Load()
	if lastActivity <= 0 {
		return 0
	}
	secs := int64(now.Sub(time.Unix(lastActivity, 0)).Seconds())
	if secs < 0 {
		return 0
	}
	return secs
}

func (s *drReplicationSecondary) isReconcileStalled(now time.Time) (bool, time.Duration) {
	stallAfter := s.reconcileStallAbort
	if stallAfter <= 0 {
		return false, 0
	}
	lastActivity := s.lastReconcileActivityAt.Load()
	if lastActivity <= 0 {
		return false, 0
	}
	since := now.Sub(time.Unix(lastActivity, 0))
	return since >= stallAfter, since
}

// Status returns a snapshot of the secondary's replication status.
func (s *drReplicationSecondary) Status() DRSecondaryStatus {
	remaining := s.reconcileBudgetRemainingByte.Load()
	if remaining < 0 {
		remaining = 0
	}
	now := time.Now().UTC()
	lastAppliedAt := s.lastAppliedAt.Load()
	lastAppliedAgeSeconds := int64(0)
	if lastAppliedAt > 0 {
		lastAppliedAgeSeconds = int64(now.Sub(time.Unix(lastAppliedAt, 0)).Seconds())
		if lastAppliedAgeSeconds < 0 {
			lastAppliedAgeSeconds = 0
		}
	}
	reconcileStuckSeconds := int64(0)
	if s.State() == DRSecondaryReconciling {
		reconcileStuckSeconds = s.reconcileStuckSeconds(now)
	}
	s.sessionMu.RLock()
	activeID := s.activeCheckpointID
	activeIndex := s.activeCheckpointIndex
	failReason := s.lastReconcileFailReason
	rangeManifestCount := s.lastRangeManifestCount
	s.sessionMu.RUnlock()
	fallbackLastAt := time.Unix(s.fallbackLastAt.Load(), 0)
	taskRate := float64(s.reconcileTaskRateMillis.Load()) / 1000.0
	return DRSecondaryStatus{
		State:                          s.State().String(),
		RelationshipID:                 s.relationshipID,
		PrimaryIndex:                   s.primaryIndex.Load(),
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
		ScanFailuresTotal:              s.scanFailures.Load(),
		CheckpointConflictsTotal:       s.checkpointConflicts.Load(),
		ReconcileRetriesTotal:          s.reconcileRetries.Load(),
		ReconcileQueueDepth:            s.reconcileQueueDepth.Load(),
		ReconcileTaskRetriesTotal:      s.reconcileTaskRetries.Load(),
		ReconcileDecodeFailuresTotal:   s.reconcileDecodeFailures.Load(),
		ReconcileStalledTotal:          s.reconcileStalled.Load(),
		ReconcileStuckSeconds:          reconcileStuckSeconds,
		LastAppliedAgeSeconds:          lastAppliedAgeSeconds,
		ReconcileMaxRPCBytes:           s.reconcileMaxRPCBytes,
		ReconcileMaxWallTimeSeconds:    int64(s.reconcileMaxWallTime / time.Second),
		ReconcileMaxInflightTasks:      s.reconcileMaxInflightTasks,
		StreamBatchMaxEntries:          s.streamBatchMaxEntries,
		StreamBatchMaxBytes:            s.streamBatchMaxBytes,
		StreamBatchMaxWaitMilliseconds: int64(s.streamBatchMaxWait / time.Millisecond),
		FallbackActive:                 s.fallbackActive.Load(),
		FallbackCount:                  s.fallbackCount.Load(),
		FallbackLastReason:             s.getFallbackLastReason(),
		FallbackLastAt:                 fallbackLastAt,
		ReconcileTaskRate:              taskRate,
	}
}

// DRSecondaryStatus is a point-in-time snapshot of replication status.
type DRSecondaryStatus struct {
	State                          string
	RelationshipID                 string
	PrimaryIndex                   uint64
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
	ScanFailuresTotal              uint64
	CheckpointConflictsTotal       uint64
	ReconcileRetriesTotal          uint64
	ReconcileQueueDepth            int64
	ReconcileTaskRetriesTotal      uint64
	ReconcileDecodeFailuresTotal   uint64
	ReconcileStalledTotal          uint64
	ReconcileStuckSeconds          int64
	LastAppliedAgeSeconds          int64
	ReconcileMaxRPCBytes           uint64
	ReconcileMaxWallTimeSeconds    int64
	ReconcileMaxInflightTasks      int
	StreamBatchMaxEntries          int
	StreamBatchMaxBytes            int
	StreamBatchMaxWaitMilliseconds int64
	FallbackActive                 bool
	FallbackCount                  uint64
	FallbackLastReason             string
	FallbackLastAt                 time.Time
	ReconcileTaskRate              float64
}

func (s *drReplicationSecondary) applyRuntimeTuning(cfg *DRConfig) {
	if cfg == nil {
		return
	}
	// Bool needs an explicit default; keep existing if config did not
	// set any fallback fields and fallback_enabled is false-by-zero.
	if cfg.FallbackEnabled || cfg.FallbackStallSeconds > 0 || cfg.FallbackFailureThreshold > 0 || cfg.FallbackMinLagEntries > 0 || cfg.FallbackCooldownSeconds > 0 || cfg.FallbackMaxPerHour > 0 {
		s.fallbackEnabled = cfg.FallbackEnabled
	}
	if cfg.ReconcileMaxRPCBytes > 0 {
		s.reconcileMaxRPCBytes = cfg.ReconcileMaxRPCBytes
	}
	if cfg.ReconcileMaxWallTimeSeconds > 0 {
		s.reconcileMaxWallTime = time.Duration(cfg.ReconcileMaxWallTimeSeconds) * time.Second
	}
	if cfg.ReconcileMaxInflightTasks > 0 {
		s.reconcileMaxInflightTasks = cfg.ReconcileMaxInflightTasks
	}
	if cfg.StreamBatchMaxEntries > 0 {
		s.streamBatchMaxEntries = cfg.StreamBatchMaxEntries
	}
	if cfg.StreamBatchMaxBytes > 0 {
		s.streamBatchMaxBytes = cfg.StreamBatchMaxBytes
	}
	if cfg.StreamBatchMaxWaitMillis > 0 {
		s.streamBatchMaxWait = time.Duration(cfg.StreamBatchMaxWaitMillis) * time.Millisecond
	}
	if cfg.ReconcileMaxWallTimeSeconds > 0 {
		// Stall abort should remain below wall-time to force controlled rollover.
		wall := time.Duration(cfg.ReconcileMaxWallTimeSeconds) * time.Second
		if wall > 0 {
			stall := wall / 3
			if stall < 30*time.Second {
				stall = 30 * time.Second
			}
			if stall > 2*time.Minute {
				stall = 2 * time.Minute
			}
			s.reconcileStallAbort = stall
		}
	}
	if cfg.FallbackStallSeconds > 0 {
		s.fallbackStall = time.Duration(cfg.FallbackStallSeconds) * time.Second
	}
	if cfg.FallbackFailureThreshold > 0 {
		s.fallbackFailureThreshold = cfg.FallbackFailureThreshold
	}
	if cfg.FallbackMinLagEntries > 0 {
		s.fallbackMinLagEntries = cfg.FallbackMinLagEntries
	}
	if cfg.FallbackCooldownSeconds > 0 {
		s.fallbackCooldown = time.Duration(cfg.FallbackCooldownSeconds) * time.Second
	}
	if cfg.FallbackMaxPerHour > 0 {
		s.fallbackMaxPerHour = cfg.FallbackMaxPerHour
	}
}

func (s *drReplicationSecondary) rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := s.rpcDeadline
	if deadline <= 0 {
		deadline = drDefaultRPCDeadline
	}
	return context.WithTimeout(ctx, deadline)
}

func (s *drReplicationSecondary) runHeartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
		}

		if s.client == nil {
			continue
		}
		rpcCtx, cancel := s.rpcContext(ctx)
		resp, err := s.client.Heartbeat(rpcCtx, &DRHeartbeatRequest{
			RelationshipId: s.relationshipID,
			AppliedIndex:   s.lastAppliedIndex.Load(),
		})
		cancel()
		if err != nil || resp == nil {
			continue
		}
		s.primaryIndex.Store(resp.PrimaryIndex)
	}
}

func (s *drReplicationSecondary) recordFallbackFailure(class drReconcileFailureClass) {
	switch class {
	case drReconcileFailureBudgetExceeded, drReconcileFailureStalled, drReconcileFailureDecodeExhausted:
	default:
		return
	}
	now := time.Now().UTC()
	window := s.fallbackWindow
	if window <= 0 {
		window = drDefaultFallbackWindow
	}

	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	cutoff := now.Add(-window)
	dst := s.fallbackFailureEvents[:0]
	for _, ts := range s.fallbackFailureEvents {
		if ts.After(cutoff) {
			dst = append(dst, ts)
		}
	}
	s.fallbackFailureEvents = append(dst, now)
}

func (s *drReplicationSecondary) shouldTriggerFallback(class drReconcileFailureClass) bool {
	if !s.fallbackEnabled {
		return false
	}
	switch class {
	case drReconcileFailureBudgetExceeded, drReconcileFailureStalled, drReconcileFailureDecodeExhausted:
	default:
		return false
	}

	now := time.Now().UTC()
	stall := s.fallbackStall
	if stall <= 0 {
		stall = drDefaultFallbackStall
	}
	lastAppliedAt := s.lastAppliedAt.Load()
	if lastAppliedAt > 0 && now.Sub(time.Unix(lastAppliedAt, 0)) < stall {
		return false
	}

	primary := s.primaryIndex.Load()
	local := s.lastAppliedIndex.Load()
	if primary <= local {
		return false
	}
	lag := primary - local
	minLag := s.fallbackMinLagEntries
	if minLag == 0 {
		minLag = uint64(2 * drStreamBufferMaxEntries)
	}
	if lag < minLag {
		return false
	}

	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	window := s.fallbackWindow
	if window <= 0 {
		window = drDefaultFallbackWindow
	}
	cutoff := now.Add(-window)
	failures := s.fallbackFailureEvents[:0]
	for _, ts := range s.fallbackFailureEvents {
		if ts.After(cutoff) {
			failures = append(failures, ts)
		}
	}
	s.fallbackFailureEvents = failures
	threshold := s.fallbackFailureThreshold
	if threshold <= 0 {
		threshold = drDefaultFallbackFailureThreshold
	}
	if len(s.fallbackFailureEvents) < threshold {
		return false
	}

	cooldown := s.fallbackCooldown
	if cooldown <= 0 {
		cooldown = drDefaultFallbackCooldown
	}
	lastFallbackAt := s.fallbackLastAt.Load()
	if lastFallbackAt > 0 && now.Sub(time.Unix(lastFallbackAt, 0)) < cooldown {
		return false
	}

	hourCutoff := now.Add(-1 * time.Hour)
	triggers := s.fallbackTriggerEvents[:0]
	for _, ts := range s.fallbackTriggerEvents {
		if ts.After(hourCutoff) {
			triggers = append(triggers, ts)
		}
	}
	s.fallbackTriggerEvents = triggers
	maxPerHour := s.fallbackMaxPerHour
	if maxPerHour <= 0 {
		maxPerHour = drDefaultFallbackMaxPerHour
	}
	if len(s.fallbackTriggerEvents) >= maxPerHour {
		return false
	}
	return true
}

func (s *drReplicationSecondary) noteFallbackTriggered(reason string) {
	now := time.Now().UTC()
	s.fallbackActive.Store(true)
	s.fallbackCount.Add(1)
	s.fallbackLastAt.Store(now.Unix())
	s.setFallbackLastReason(reason)

	s.fallbackMu.Lock()
	s.fallbackTriggerEvents = append(s.fallbackTriggerEvents, now)
	s.fallbackMu.Unlock()
}

func (s *drReplicationSecondary) performResnapshot(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "manual"
	}
	s.noteFallbackTriggered(reason)
	defer s.fallbackActive.Store(false)
	s.markReconcileActivityNow()

	rpcCtx, cancel := s.rpcContext(ctx)
	checkpoint, err := s.client.RequestCheckpoint(rpcCtx, &CheckpointRequest{RelationshipId: s.relationshipID})
	cancel()
	if err != nil {
		return fmt.Errorf("failed to request checkpoint for resnapshot: %w", err)
	}

	s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex, len(checkpoint.GetTopRanges()))
	defer s.endReconcileSession()

	full := reconciler.RangeSpan{}
	for i := range full.EndKID {
		full.EndKID[i] = 0xff
	}

	fetchTimeout := s.reconcileMaxWallTime
	if fetchTimeout <= 0 {
		fetchTimeout = drDefaultReconcileMaxWallTime
	}
	fetchCtx, fetchCancel := context.WithTimeout(ctx, fetchTimeout)
	stream, err := s.client.FetchEntries(fetchCtx, &FetchEntriesRequest{
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
		Ranges:          []*RangeSpan{rangeSpanToProto(full)},
		IncludeDeletes:  true,
	})
	if err != nil {
		fetchCancel()
		return fmt.Errorf("failed to start resnapshot fetch: %w", err)
	}
	defer fetchCancel()

	remoteKeys := make(map[string]struct{}, 1024)
	applied := 0
	for {
		batch, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("resnapshot fetch stream failed: %w", recvErr)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}
		if len(batch.GetFailedKids()) > 0 {
			return fmt.Errorf("resnapshot fetch returned failed_kids: %d", len(batch.GetFailedKids()))
		}
		if len(batch.GetEntries()) > 0 {
			entries := make([]*EntryChange, 0, len(batch.GetEntries()))
			for _, e := range batch.GetEntries() {
				entries = append(entries, cloneEntryChange(e))
			}
			if err := s.applyFetchedEntriesDeterministic(ctx, nil, entries); err != nil {
				return fmt.Errorf("resnapshot apply failed: %w", err)
			}
			applied += len(entries)
			for _, e := range entries {
				if e != nil && e.Key != "" {
					remoteKeys[e.Key] = struct{}{}
				}
			}
		}
	}

	localCheckpoint := reconciler.Checkpoint{
		ID:          checkpoint.CheckpointId,
		CommitIndex: checkpoint.CommitIndex,
	}
	localSet, err := s.scanner.Scan(ctx, s.core.barrier, localCheckpoint)
	if err != nil {
		s.scanFailures.Add(1)
		return fmt.Errorf("failed to scan local state after resnapshot fetch: %w", err)
	}

	removeKeys := make([]string, 0, 256)
	for _, key := range localSet.KIDToKey {
		if key == "" || isDRNeverReplicatePath(key) {
			continue
		}
		if _, ok := remoteKeys[key]; ok {
			continue
		}
		removeKeys = append(removeKeys, key)
	}
	if len(removeKeys) > 0 {
		if err := s.applyRemovedKeys(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, removeKeys); err != nil {
			return fmt.Errorf("resnapshot delete phase failed: %w", err)
		}
	}

	s.setLastAppliedIndex(checkpoint.CommitIndex)
	s.entriesApplied.Add(uint64(applied))
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("resnapshot fallback complete",
		"reason", reason,
		"checkpoint_id", checkpoint.CheckpointId,
		"checkpoint_index", checkpoint.CommitIndex,
		"applied_entries", applied,
		"removed_entries", len(removeKeys))
	return nil
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

	streamCtx, streamCancel := context.WithCancel(ctx)
	s.setActiveStreamCancel(streamCancel)
	defer func() {
		streamCancel()
		s.setActiveStreamCancel(nil)
	}()

	stream, err := s.client.StreamChanges(streamCtx, &StreamChangesRequest{
		RelationshipId:   s.relationshipID,
		LastAppliedIndex: s.lastAppliedIndex.Load(),
	})
	if err != nil {
		return fmt.Errorf("failed to open change stream: %w", err)
	}

	applyQueueSize := s.streamBatchMaxEntries * 4
	if applyQueueSize < 1024 {
		applyQueueSize = 1024
	}
	applyCh := make(chan *EntryChange, applyQueueSize)
	applyErrCh := make(chan error, 1)
	go func() {
		applyErrCh <- s.runStreamApplyWorker(ctx, applyCh)
	}()

	expectedNext := s.lastAppliedIndex.Load() + 1
	for {
		select {
		case <-ctx.Done():
			close(applyCh)
			<-applyErrCh
			return ctx.Err()
		case <-s.stopCh:
			close(applyCh)
			<-applyErrCh
			return nil
		case err := <-applyErrCh:
			if err != nil {
				return fmt.Errorf("stream apply worker failed: %w", err)
			}
			return nil
		default:
		}

		change, err := stream.Recv()
		if err == io.EOF {
			close(applyCh)
			if applyErr := <-applyErrCh; applyErr != nil {
				return fmt.Errorf("change stream ended with apply error: %w", applyErr)
			}
			return fmt.Errorf("change stream ended")
		}
		if err != nil {
			close(applyCh)
			if applyErr := <-applyErrCh; applyErr != nil {
				return fmt.Errorf("change stream error: %v (apply worker: %w)", err, applyErr)
			}
			return fmt.Errorf("change stream error: %w", err)
		}

		// Gap detection: if we receive an index higher than expected,
		// entries were dropped. Fall back to reconciliation.
		if change.RaftIndex > expectedNext {
			close(applyCh)
			_ = <-applyErrCh
			s.logger.Warn("gap detected in change stream, falling back to reconciliation",
				"expected_index", expectedNext,
				"received_index", change.RaftIndex,
				"gap_size", change.RaftIndex-expectedNext)
			s.streamDisconnects.Add(1)
			return fmt.Errorf("change stream gap detected: expected index %d, got %d",
				expectedNext, change.RaftIndex)
		}

		if next := change.RaftIndex + 1; next > expectedNext {
			expectedNext = next
		}

		select {
		case applyCh <- change:
		default:
			close(applyCh)
			_ = <-applyErrCh
			return fmt.Errorf("change stream apply queue full (capacity=%d); reconciliation required", cap(applyCh))
		}
	}
}

func (s *drReplicationSecondary) runStreamApplyWorker(ctx context.Context, applyCh <-chan *EntryChange) error {
	maxEntries := s.streamBatchMaxEntries
	if maxEntries <= 0 {
		maxEntries = drDefaultStreamBatchMaxEntries
	}
	maxBytes := s.streamBatchMaxBytes
	if maxBytes <= 0 {
		maxBytes = drDefaultStreamBatchMaxBytes
	}
	maxWait := s.streamBatchMaxWait
	if maxWait <= 0 {
		maxWait = drDefaultStreamBatchMaxWait
	}

	batch := make([]*EntryChange, 0, maxEntries)
	batchBytes := 0
	ticker := time.NewTicker(maxWait)
	defer ticker.Stop()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		applyStart := time.Now()
		for _, change := range batch {
			current := s.lastAppliedIndex.Load()
			if change.RaftIndex < current {
				continue
			}
			if err := s.applyStreamChange(ctx, change); err != nil {
				return fmt.Errorf("failed to apply change: %w", err)
			}
			s.setLastAppliedIndex(change.RaftIndex)
			s.entriesApplied.Add(1)
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "entries_applied"}, 1)
			metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(change.RaftIndex))
		}
		metrics.MeasureSince([]string{"replication", "dr", "secondary", "apply_latency"}, applyStart)
		batch = batch[:0]
		batchBytes = 0
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return flush()
		case <-s.stopCh:
			return flush()
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
		case change, ok := <-applyCh:
			if !ok {
				return flush()
			}
			batch = append(batch, change)
			batchBytes += int(len(change.Key) + len(change.Value) + 48)
			if len(batch) >= maxEntries || batchBytes >= maxBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
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
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
	s.reconcileQueueDepth.Store(0)
	defer func() {
		metrics.MeasureSince([]string{"replication", "dr", "secondary", "reconciliation_duration"}, startTime)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "reconciliation_count"}, 1)
	}()

	// Step 1: Request a checkpoint from the primary.
	rpcCtx, cancel := s.rpcContext(ctx)
	checkpoint, err := s.client.RequestCheckpoint(rpcCtx, &CheckpointRequest{
		RelationshipId: s.relationshipID,
	})
	cancel()
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
		s.scanFailures.Add(1)
		return fmt.Errorf("failed to scan local storage: %w", err)
	}
	s.logger.Info("local scan complete", "keys", localSet.KeyCount)

	if len(checkpoint.GetTopRanges()) == 0 {
		return fmt.Errorf("reconcile failure [checkpoint_conflict]: checkpoint %q missing top_ranges (range manifest required)", checkpoint.CheckpointId)
	}

	if err := s.runRangeReconciliation(ctx, checkpoint, localSet, startTime); err != nil {
		return err
	}
	return nil
}

type drRangeTask struct {
	span           reconciler.RangeSpan
	remote         reconciler.RangeDescriptor
	baseSplitDepth uint32
	priorityHint   int
}

type drQueuedRangeTask struct {
	id       int
	priority int
	task     drRangeTask
}

type drRangeTaskResult struct {
	id                   int
	task                 drRangeTask
	rpcBytes             uint64
	ibltCells            uint64
	fetchedEntries       []*EntryChange
	removedKeys          []string
	needsPrefixRefine    bool
	splitTasks           []drRangeTask
	decodeFailedForSplit bool
	err                  error
}

type drRangeTaskQueue []drQueuedRangeTask

func (q drRangeTaskQueue) Len() int { return len(q) }

func (q drRangeTaskQueue) Less(i, j int) bool {
	// Max-heap: larger priority first; stable-ish by task id.
	if q[i].priority == q[j].priority {
		return q[i].id < q[j].id
	}
	return q[i].priority > q[j].priority
}

func (q drRangeTaskQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *drRangeTaskQueue) Push(x interface{}) {
	*q = append(*q, x.(drQueuedRangeTask))
}

func (q *drRangeTaskQueue) Pop() interface{} {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

type drRangeBudget struct {
	start         time.Time
	rpcBytes      uint64
	rangesHandled int
	rangesSplit   int
	maxRPCBytes   uint64
	maxWallTime   time.Duration
}

func (b *drRangeBudget) check() error {
	if b.start.IsZero() {
		b.start = time.Now()
	}
	if b.maxWallTime <= 0 {
		b.maxWallTime = drDefaultReconcileMaxWallTime
	}
	if b.maxRPCBytes == 0 {
		b.maxRPCBytes = drDefaultReconcileMaxRPCBytes
	}
	if time.Since(b.start) > b.maxWallTime {
		return fmt.Errorf("budget_exceeded: reconciliation wall-time exceeded")
	}
	if b.rpcBytes > b.maxRPCBytes {
		return fmt.Errorf("budget_exceeded: reconcile RPC bytes exceeded (%d > %d)", b.rpcBytes, b.maxRPCBytes)
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
		"max_inflight_range_tasks", s.reconcileMaxInflightTasks)
	if len(checkpoint.TopRanges) > drRangeMaxTopRanges {
		return fmt.Errorf("reconcile failure [invalid_range_manifest]: top range count %d exceeds max %d", len(checkpoint.TopRanges), drRangeMaxTopRanges)
	}

	budget := &drRangeBudget{
		start:       startTime,
		maxRPCBytes: s.reconcileMaxRPCBytes,
		maxWallTime: s.reconcileMaxWallTime,
	}
	localIndex := reconciler.NewRangeMapIndex(localSet.KIDToVID, localSet.Entries)
	queue := make(drRangeTaskQueue, 0, len(checkpoint.TopRanges))
	heap.Init(&queue)
	nextTaskID := 1
	enqueueTask := func(task drRangeTask, priority int) {
		if priority < 1 {
			priority = 1
		}
		task.priorityHint = priority
		heap.Push(&queue, drQueuedRangeTask{
			id:       nextTaskID,
			priority: priority,
			task:     task,
		})
		nextTaskID++
	}
	s.reconcileRangesFailed.Store(0)
	s.reconcileRangesInflight.Store(0)
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
	s.rangeSplitCount.Store(0)
	s.reconcileRPCBytesUsed.Store(0)
	s.reconcileQueueDepth.Store(0)
	s.reconcileTasksHandled.Store(0)
	s.reconcileTaskRateMillis.Store(0)

	for _, rd := range checkpoint.TopRanges {
		remote, err := protoRangeDigestToDescriptor(rd)
		if err != nil {
			return fmt.Errorf("reconcile failure [invalid_range_manifest]: %w", err)
		}
		local := reconciler.BuildRangeDigestFromIndex(localIndex, remote.Span, drSecondaryRangeMaxIBLTCellsPerRange)
		if !local.EqualDigest(remote) {
			enqueueTask(drRangeTask{
				span:           remote.Span,
				remote:         remote,
				baseSplitDepth: remote.Span.SplitDepth,
			}, estimateRangeDiff(local, remote))
		}
	}
	if queue.Len() > drRangeMaxOutstandingTasks {
		return fmt.Errorf("reconcile failure [budget_exceeded]: mismatched range count %d exceeds cap %d", queue.Len(), drRangeMaxOutstandingTasks)
	}
	s.markReconcileActivityNow()

	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_total"}, float32(len(checkpoint.TopRanges)))
	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_mismatched"}, float32(queue.Len()))

	if queue.Len() == 0 {
		s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
		s.setLastAppliedIndex(checkpoint.CommitIndex)
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		s.logger.Info("range reconciliation: no mismatched ranges")
		return nil
	}

	maxWorkers := s.reconcileMaxInflightTasks
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	workCh := make(chan drQueuedRangeTask, maxWorkers*2)
	resultCh := make(chan drRangeTaskResult, maxWorkers*2)
	var workerWG sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for {
				select {
				case <-workerCtx.Done():
					return
				case queued, ok := <-workCh:
					if !ok {
						return
					}
					res := s.processRangeTask(workerCtx, checkpoint, localSet, localIndex, queued)
					select {
					case resultCh <- res:
					case <-workerCtx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() {
		close(workCh)
		workerWG.Wait()
	}()

	inflight := 0
	updateWorkloadMetrics := func() {
		s.reconcileRangesInflight.Store(int64(inflight))
		s.reconcileQueueDepth.Store(int64(queue.Len() + inflight))
		metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_inflight"}, float32(inflight))
	}
	updateWorkloadMetrics()
	var ibltCellsUsed uint64
	for queue.Len() > 0 || inflight > 0 {
		if stalled, since := s.isReconcileStalled(time.Now().UTC()); stalled {
			workerCancel()
			s.reconcileRangesFailed.Add(1)
			s.reconcileStalled.Add(1)
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "stalled_total"}, 1)
			return fmt.Errorf("reconcile failure [stalled]: stalled without task progress for %s", since.Round(time.Second))
		}
		for inflight < maxWorkers && queue.Len() > 0 {
			if err := budget.check(); err != nil {
				workerCancel()
				metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
				s.reconcileRangesFailed.Add(1)
				return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
			}
			task := heap.Pop(&queue).(drQueuedRangeTask)
			select {
			case workCh <- task:
				inflight++
				s.markReconcileActivityNow()
			case <-workerCtx.Done():
				s.reconcileRangesFailed.Add(1)
				return workerCtx.Err()
			}
		}
		updateWorkloadMetrics()

		var res drRangeTaskResult
		select {
		case <-ctx.Done():
			workerCancel()
			s.reconcileRangesFailed.Add(1)
			return ctx.Err()
		case res = <-resultCh:
			inflight--
			s.markReconcileActivityNow()
		}
		updateWorkloadMetrics()

		task := res.task
		if res.err != nil {
			workerCancel()
			s.reconcileRangesFailed.Add(1)
			return res.err
		}

		if res.decodeFailedForSplit {
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "range_decode_failures"}, 1)
			s.reconcileDecodeFailures.Add(1)
		}

		if err := budget.addRPC(res.rpcBytes); err != nil {
			workerCancel()
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}
		s.reconcileRPCBytesUsed.Store(budget.rpcBytes)
		metrics.SetGauge([]string{"replication", "dr", "reconcile", "rpc_bytes_used"}, float32(budget.rpcBytes))
		if budget.rpcBytes >= s.reconcileMaxRPCBytes {
			s.reconcileBudgetRemainingByte.Store(0)
		} else {
			s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes - budget.rpcBytes))
		}

		if res.ibltCells > 0 {
			ibltCellsUsed += res.ibltCells
			metrics.SetGauge([]string{"replication", "dr", "reconcile", "iblt_cells_used"}, float32(ibltCellsUsed))
		}

		budget.rangesHandled++
		s.reconcileTasksHandled.Store(uint64(budget.rangesHandled))
		elapsed := time.Since(startTime).Seconds()
		if elapsed > 0 {
			rateMillis := uint64((float64(budget.rangesHandled) / elapsed) * 1000.0)
			s.reconcileTaskRateMillis.Store(rateMillis)
		}
		if budget.rangesHandled > drRangeMaxTotalRanges {
			workerCancel()
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
			s.reconcileRangesFailed.Add(1)
			return fmt.Errorf("reconcile failure [budget_exceeded]: range task count exceeded")
		}

		if len(res.splitTasks) > 0 {
			if budget.rangesSplit+1 > drRangeMaxSessionSplits {
				workerCancel()
				metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
				s.reconcileRangesFailed.Add(1)
				return fmt.Errorf("reconcile failure [budget_exceeded]: split count exceeded (%d > %d)", budget.rangesSplit+1, drRangeMaxSessionSplits)
			}
			outstanding := queue.Len() + inflight + len(res.splitTasks)
			if outstanding > drRangeMaxOutstandingTasks {
				workerCancel()
				metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
				s.reconcileRangesFailed.Add(1)
				return fmt.Errorf("reconcile failure [budget_exceeded]: outstanding range tasks exceeded (%d > %d)", outstanding, drRangeMaxOutstandingTasks)
			}
			for _, child := range res.splitTasks {
				enqueueTask(child, child.priorityHint)
			}
			budget.rangesSplit++
			s.rangeSplitCount.Store(uint64(budget.rangesSplit))
			metrics.IncrCounter([]string{"replication", "dr", "reconcile", "ranges_split"}, 1)
			if budget.rpcBytes > (s.reconcileMaxRPCBytes*8)/10 && queue.Len() > maxWorkers*3 {
				workerCancel()
				s.reconcileRangesFailed.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "reconcile", "budget_exceeded_total"}, 1)
				return fmt.Errorf("reconcile failure [budget_exceeded]: forcing checkpoint rollover under pressure (rpc_bytes=%d queue=%d)", budget.rpcBytes, queue.Len())
			}
			updateWorkloadMetrics()
			continue
		}

		if res.needsPrefixRefine {
			if err := s.runRangePrefixRefinement(ctx, checkpoint, localSet, task.span, budget); err != nil {
				workerCancel()
				s.reconcileRangesFailed.Add(1)
				return wrapReconcileFailure(drReconcileFailureDecodeExhausted, "prefix refinement", err)
			}
			updateWorkloadMetrics()
			continue
		}

		if len(res.fetchedEntries) > 0 {
			if err := s.applyFetchedEntriesDeterministic(ctx, localSet.KIDToKey, res.fetchedEntries); err != nil {
				workerCancel()
				s.reconcileRangesFailed.Add(1)
				return wrapReconcileFailure(drReconcileFailureApplyFailed, "apply fetched entries", err)
			}
		}
		if len(res.removedKeys) > 0 {
			if err := s.applyRemovedKeys(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, res.removedKeys); err != nil {
				workerCancel()
				s.reconcileRangesFailed.Add(1)
				return wrapReconcileFailure(drReconcileFailureApplyFailed, "apply removed keys", err)
			}
		}
		updateWorkloadMetrics()
	}
	workerCancel()
	s.reconcileRangesInflight.Store(0)
	s.reconcileQueueDepth.Store(0)
	if budget.rpcBytes >= s.reconcileMaxRPCBytes {
		s.reconcileBudgetRemainingByte.Store(0)
	} else {
		s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes - budget.rpcBytes))
	}

	s.setLastAppliedIndex(checkpoint.CommitIndex)
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("range-first reconciliation complete",
		"ranges_handled", budget.rangesHandled,
		"ranges_split", budget.rangesSplit,
		"rpc_bytes", budget.rpcBytes,
		"duration", time.Since(startTime))
	return nil
}

func (s *drReplicationSecondary) processRangeTask(ctx context.Context, checkpoint *CheckpointResponse, localSet *reconciler.ReconciliationSet, localIndex *reconciler.RangeMapIndex, queued drQueuedRangeTask) drRangeTaskResult {
	task := queued.task
	result := drRangeTaskResult{
		id:   queued.id,
		task: task,
	}

	localDesc := reconciler.BuildRangeDigestFromIndex(localIndex, task.span, drSecondaryRangeMaxIBLTCellsPerRange)
	estimatedDiff := estimateRangeDiff(localDesc, task.remote)
	baseCells := clampIBLTCells(uint32(math.Ceil(float64(estimatedDiff)*1.5)), drSecondaryRangeMaxIBLTCellsPerRange)
	cellAttempts := make([]uint32, 0, 3)
	for _, candidate := range []uint32{
		baseCells,
		clampIBLTCells(baseCells*2, drSecondaryRangeMaxIBLTCellsPerRange),
		clampIBLTCells(baseCells*4, drSecondaryRangeMaxIBLTCellsPerRange),
	} {
		if len(cellAttempts) == 0 || cellAttempts[len(cellAttempts)-1] != candidate {
			cellAttempts = append(cellAttempts, candidate)
		}
	}

	for _, ibltCells := range cellAttempts {
		localIBLT := reconciler.BuildRangeIBLTFromIndex(localIndex, task.span, ibltCells)
		req := &IBLTMessage{
			CheckpointId:    checkpoint.CheckpointId,
			NumCells:        ibltCells,
			Span:            rangeSpanToProto(task.span),
			CheckpointIndex: checkpoint.CommitIndex,
		}

		var primaryIBLTMsg *IBLTMessage
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			rpcCtx, cancel := s.rpcContext(ctx)
			primaryIBLTMsg, err = s.client.ExchangeIBLT(rpcCtx, req)
			cancel()
			if err == nil {
				break
			}
			if attempt < 2 {
				s.reconcileTaskRetries.Add(1)
				time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
			}
		}
		if err != nil {
			result.err = fmt.Errorf("reconcile failure [rpc_failed]: exchange IBLT failed: %w", err)
			return result
		}
		if err := s.assertActiveCheckpoint(primaryIBLTMsg.GetCheckpointId(), primaryIBLTMsg.GetCheckpointIndex()); err != nil {
			result.err = fmt.Errorf("reconcile failure [checkpoint_conflict]: %w", err)
			return result
		}
		result.rpcBytes += uint64(len(primaryIBLTMsg.GetIbltData()) + 256)
		result.ibltCells += uint64(ibltCells)

		remoteIBLT, err := sketch.UnmarshalIBLT(primaryIBLTMsg.IbltData)
		if err != nil {
			result.err = fmt.Errorf("reconcile failure [decode_exhausted]: unmarshal IBLT: %w", err)
			return result
		}
		diff, err := remoteIBLT.Subtract(localIBLT)
		if err != nil {
			result.err = fmt.Errorf("reconcile failure [decode_exhausted]: IBLT subtract: %w", err)
			return result
		}

		added, removed, ok := diff.Decode()
		if !ok {
			continue
		}

		if len(added) > 0 {
			entries, rpcBytes, fetchErr := s.fetchEntriesForDiff(ctx, checkpoint.CheckpointId, checkpoint.CommitIndex, added)
			if fetchErr != nil {
				result.err = wrapReconcileFailure(drReconcileFailureApplyFailed, "fetch entries for diff", fetchErr)
				return result
			}
			result.fetchedEntries = entries
			result.rpcBytes += rpcBytes
		}
		if len(removed) > 0 {
			result.removedKeys = resolveRemovedKeys(localSet, removed)
		}
		return result
	}

	result.decodeFailedForSplit = true
	if task.span.SplitDepth < task.maxSplitDepth() {
		left, right, splitOK := reconciler.SplitRange(task.span)
		if splitOK {
			rpcCtx, cancel := s.rpcContext(ctx)
			digestResp, err := s.client.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
				CheckpointId:    checkpoint.CheckpointId,
				CheckpointIndex: checkpoint.CommitIndex,
				Spans: []*RangeSpan{
					rangeSpanToProto(left),
					rangeSpanToProto(right),
				},
			})
			cancel()
			if err != nil {
				result.err = fmt.Errorf("reconcile failure [rpc_failed]: exchange child range digests failed: %w", err)
				return result
			}
			result.rpcBytes += uint64(len(digestResp.GetRanges())*128 + 128)
			if len(digestResp.GetRanges()) != 2 {
				result.err = fmt.Errorf("reconcile failure [decode_exhausted]: expected 2 child range digests, got %d", len(digestResp.GetRanges()))
				return result
			}

			children := make([]drRangeTask, 0, 2)
			for _, remoteRange := range digestResp.GetRanges() {
				remoteChild, convErr := protoRangeDigestToDescriptor(remoteRange)
				if convErr != nil {
					result.err = fmt.Errorf("reconcile failure [decode_exhausted]: invalid child range digest: %w", convErr)
					return result
				}
				localChild := reconciler.BuildRangeDigestFromIndex(localIndex, remoteChild.Span, drSecondaryRangeMaxIBLTCellsPerRange)
				if localChild.EqualDigest(remoteChild) {
					continue
				}
				children = append(children, drRangeTask{
					span:           remoteChild.Span,
					remote:         remoteChild,
					baseSplitDepth: task.baseSplitDepth,
					priorityHint:   estimateRangeDiff(localChild, remoteChild),
				})
			}
			if len(children) > 0 {
				result.splitTasks = children
				return result
			}
		}
	}
	result.needsPrefixRefine = true
	return result
}

func (t drRangeTask) maxSplitDepth() uint32 {
	// Split-depth budget is relative to the top-level range depth to keep
	// adaptive splitting available even when manifests use non-zero depth.
	base := t.baseSplitDepth
	limit := base + uint32(drRangeMaxSplitDepth)
	if limit < base {
		return ^uint32(0)
	}
	return limit
}

func wrapReconcileFailure(defaultClass drReconcileFailureClass, op string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "reconcile failure [") {
		return err
	}
	class := classifyReconcileFailure(err)
	if class == drReconcileFailureUnknown {
		class = defaultClass
	}
	if op == "" {
		return fmt.Errorf("reconcile failure [%s]: %w", class, err)
	}
	return fmt.Errorf("reconcile failure [%s]: %s: %w", class, op, err)
}

func (s *drReplicationSecondary) fetchEntriesForDiff(ctx context.Context, checkpointID string, checkpointIndex uint64, entries []sketch.DiffEntry) ([]*EntryChange, uint64, error) {
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
		return nil, 0, fmt.Errorf("failed to open fetch stream: %w", err)
	}

	var rpcBytes uint64
	var out []*EntryChange
	var failedKids [][]byte
	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, rpcBytes, fmt.Errorf("fetch stream error: %w", err)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return nil, rpcBytes, fmt.Errorf("checkpoint conflict: %w", err)
		}
		rpcBytes += uint64(len(batch.Entries)*128 + len(batch.FailedKids)*32)
		for _, e := range batch.Entries {
			out = append(out, cloneEntryChange(e))
		}
		if len(batch.FailedKids) > 0 {
			failedKids = append(failedKids, batch.FailedKids...)
		}
	}

	if len(failedKids) == 0 {
		return out, rpcBytes, nil
	}

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
		return nil, rpcBytes, fmt.Errorf("failed to retry fetch for failed kids: %w", err)
	}

	failedKids = failedKids[:0]
	for {
		batch, err := retryStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, rpcBytes, fmt.Errorf("retry fetch stream error: %w", err)
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return nil, rpcBytes, fmt.Errorf("checkpoint conflict: %w", err)
		}
		rpcBytes += uint64(len(batch.Entries)*128 + len(batch.FailedKids)*32)
		for _, e := range batch.Entries {
			out = append(out, cloneEntryChange(e))
		}
		if len(batch.FailedKids) > 0 {
			failedKids = append(failedKids, batch.FailedKids...)
		}
	}
	if len(failedKids) > 0 {
		return nil, rpcBytes, fmt.Errorf("fetch failed for %d keys after retry", len(failedKids))
	}

	return out, rpcBytes, nil
}

func resolveRemovedKeys(localSet *reconciler.ReconciliationSet, removed []sketch.DiffEntry) []string {
	if len(removed) == 0 || localSet.KIDToKey == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(removed))
	keys := make([]string, 0, len(removed))
	for _, entry := range removed {
		key, ok := localSet.KIDToKey[entry.KID]
		if !ok || key == "" || isDRNeverReplicatePath(key) {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneEntryChange(in *EntryChange) *EntryChange {
	if in == nil {
		return nil
	}
	out := &EntryChange{
		OpType:    in.OpType,
		Key:       in.Key,
		SealWrap:  in.SealWrap,
		RaftIndex: in.RaftIndex,
	}
	if len(in.Value) > 0 {
		out.Value = append([]byte(nil), in.Value...)
	}
	if len(in.Kid) > 0 {
		out.Kid = append([]byte(nil), in.Kid...)
	}
	return out
}

func (s *drReplicationSecondary) applyFetchedEntriesDeterministic(ctx context.Context, kidToKey map[[32]byte]string, entries []*EntryChange) error {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i] == nil || entries[j] == nil {
			return entries[i] != nil
		}
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		if cmp := bytes.Compare(entries[i].Kid, entries[j].Kid); cmp != 0 {
			return cmp < 0
		}
		if entries[i].RaftIndex != entries[j].RaftIndex {
			return entries[i].RaftIndex < entries[j].RaftIndex
		}
		return entries[i].OpType < entries[j].OpType
	})

	failures := 0
	examples := make([]string, 0, 5)
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if err := s.applyFetchedChange(ctx, entry, kidToKey); err != nil {
			failures++
			label := entry.Key
			if label == "" && len(entry.Kid) >= 8 {
				label = fmt.Sprintf("kid:%x", entry.Kid[:8])
			}
			if len(examples) < cap(examples) {
				examples = append(examples, label)
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("failed to apply %d fetched entries (examples: %v)", failures, examples)
	}
	return nil
}

func (s *drReplicationSecondary) applyRemovedKeys(ctx context.Context, checkpointID string, checkpointIndex uint64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := s.validateDeleteSafety(checkpointID, checkpointIndex); err != nil {
		return fmt.Errorf("checkpoint conflict: %w", err)
	}
	sort.Strings(keys)
	failures := 0
	examples := make([]string, 0, 5)
	for _, key := range keys {
		if isDRNeverReplicatePath(key) {
			continue
		}
		if err := s.core.barrier.Delete(ctx, key); err != nil {
			failures++
			if len(examples) < cap(examples) {
				examples = append(examples, key)
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("reconciliation delete failed for %d entries (examples: %v)", failures, examples)
	}
	return nil
}

func (s *drReplicationSecondary) runRangePrefixRefinement(ctx context.Context, checkpoint *CheckpointResponse, localSet *reconciler.ReconciliationSet, span reconciler.RangeSpan, budget *drRangeBudget) error {
	var finalPrefix uint32
	var mismatched []uint32

	for _, p := range []uint32{8, 12, 16} {
		localPD := reconciler.BuildRangePrefixDigest(localSet, span, p)
		rpcCtx, cancel := s.rpcContext(ctx)
		resp, err := s.client.ExchangePrefixDigests(rpcCtx, &PrefixDigestRequest{
			CheckpointId:    checkpoint.CheckpointId,
			PrefixLength:    p,
			Span:            rangeSpanToProto(span),
			CheckpointIndex: checkpoint.CommitIndex,
		})
		cancel()
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
