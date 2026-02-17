// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for DR streams
	"google.golang.org/grpc/status"

	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// errDRRedirect is returned when a DR gRPC call is redirected to the
// active leader. The controller uses LeaderAddr to reconnect.
type errDRRedirect struct {
	LeaderAddr string
}

func (e *errDRRedirect) Error() string {
	return fmt.Sprintf("DR redirect to leader at %s", e.LeaderAddr)
}

// extractDRRedirect checks whether a gRPC error contains a
// DRRedirectDetail and returns the leader address if so.
func extractDRRedirect(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return "", false
	}
	for _, detail := range st.Details() {
		if rd, ok := detail.(*DRRedirectDetail); ok && rd.LeaderClusterAddr != "" {
			return rd.LeaderClusterAddr, true
		}
	}
	return "", false
}

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

// isReconciliationRequired returns true when a stream error indicates the
// primary cannot satisfy catch-up from the buffer/journal and a full
// reconciliation is needed.
func isReconciliationRequired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "reconciliation required") ||
		strings.Contains(msg, "buffer too old") ||
		strings.Contains(msg, "journal too old")
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

type drReconcilePhase uint32

const (
	drReconcilePhaseIdle drReconcilePhase = iota
	drReconcilePhaseRangeTasks
	drReconcilePhaseApplyPuts
	drReconcilePhaseApplyDeletes
	drReconcilePhaseFinalize
	drReconcilePhaseResnapshot
)

func (p drReconcilePhase) String() string {
	switch p {
	case drReconcilePhaseRangeTasks:
		return "range_tasks"
	case drReconcilePhaseApplyPuts:
		return "apply_puts"
	case drReconcilePhaseApplyDeletes:
		return "apply_deletes"
	case drReconcilePhaseFinalize:
		return "finalize"
	case drReconcilePhaseResnapshot:
		return "resnapshot"
	default:
		return "idle"
	}
}

const (
	drRangeTargetKeysPerRange  = 12000
	drRangeTargetValueBytes    = 8 << 20 // 8 MiB
	drRangeMaxTopRanges        = 256
	drRangeMaxTotalRanges      = 1024
	drRangeMaxOutstandingTasks = 384
	drRangeMaxSessionSplits    = 192
	drRangeMaxSplitDepth       = 6

	drDefaultReconcileMaxRPCBytes      = 128 << 20
	drDefaultReconcileMaxWallTime      = 30 * time.Minute
	drDefaultReconcileMaxInflightTasks = 16
	drDefaultReconcileStallAbort       = 90 * time.Second
	drDefaultRPCDeadline               = 30 * time.Second
	drDefaultStreamBatchMaxEntries     = 256
	drDefaultStreamBatchMaxBytes       = 1 << 20 // 1 MiB
	drDefaultStreamBatchMaxWait        = 10 * time.Millisecond
	drDefaultReconcileApplyWorkers     = 16
	drDefaultReconcilePutBatchEntries  = 512
	drDefaultReconcilePutBatchBytes    = 2 << 20 // 2 MiB
	drDefaultConvergenceMinRateRatio   = 0.8
	drDefaultConvergenceStall          = 180 * time.Second

	drDefaultFallbackEnabled          = true
	drDefaultFallbackStall            = 180 * time.Second
	drDefaultFallbackFailureThreshold = 3
	drDefaultFallbackWindow           = 10 * time.Minute
	drDefaultFallbackCooldown         = 10 * time.Minute
	drDefaultFallbackMaxPerHour       = 2

	drMaxStreamResumeAttempts = 3
	drDefaultReconcileTimeout = 10 * time.Minute

	// drDefaultCheckpointRPCTimeout is the timeout for the
	// RequestCheckpoint RPC specifically. This is longer than the
	// general drDefaultRPCDeadline because the primary must build a
	// checkpoint artifact (full storage scan + KID/VID index), which
	// scales with the number of keys and can take minutes for large
	// datasets (100K+ entries).
	drDefaultCheckpointRPCTimeout = 5 * time.Minute

	// drDefaultApplyYieldDuration is a short pause inserted after each
	// successful batch commit in the DR apply path (both streaming and
	// reconciliation).  This prevents the apply worker from monopolising
	// the secondary's Raft subsystem and starving heartbeat processing,
	// which can otherwise cause the secondary cluster to lose quorum
	// under sustained write load.
	drDefaultApplyYieldDuration = 1 * time.Millisecond
)

// drHeartbeatInterval controls DR heartbeat cadence. Kept as a package var
// so tests can shorten it for deterministic execution time.
var drHeartbeatInterval = 2 * time.Second

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

	// trustedPrimaryCerts holds primary cluster TLS certificates
	// keyed by SHA-256 fingerprint. Seeded from the activation
	// token cert and updated from heartbeat responses. Entries
	// older than trustedCertTTL are pruned during heartbeat ticks.
	trustedPrimaryCertsMu sync.RWMutex
	trustedPrimaryCerts   map[string]*trustedPrimaryCert

	// drClusterClient is the cluster client registered with the
	// cluster listener. Kept so the heartbeat loop can call
	// addTrustedCert / pruneTrustedCerts directly.
	drClusterClient *drReplicationClusterClient

	// lastKnownLeaderAddr caches the active leader's cluster address,
	// updated on each heartbeat. Used for redirect-based reconnection
	// after stepdowns.
	lastKnownLeaderAddr atomic.Pointer[string]

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
	lastReconcileApplyAt         atomic.Int64 // unix timestamp

	sessionMu                          sync.RWMutex
	activeCheckpointID                 string
	activeCheckpointIndex              uint64
	highestCommittedCheckpointIndex    uint64 // monotonic high-water mark
	highestCommittedCheckpointIndexSet bool   // true once loaded/initialized
	sessionStart                       time.Time
	streamPausedAt                     uint64
	lastReconcileFailReason            string
	lastRangeManifestCount             int
	lastReconcileFailureByType         map[string]uint64

	// Additional heavy-load observability counters.
	scanFailures            atomic.Uint64
	checkpointConflicts     atomic.Uint64
	reconcileRetries        atomic.Uint64
	reconcileTaskRetries    atomic.Uint64
	reconcileDecodeFailures atomic.Uint64
	reconcilePutWorkers     atomic.Int64
	reconcileDeletePhaseMS  atomic.Uint64

	// Runtime tunables.
	reconcileMaxRPCBytes      uint64
	reconcileMaxWallTime      time.Duration
	reconcileMaxInflightTasks int
	reconcileStallAbort       time.Duration
	rpcDeadline               time.Duration
	streamBatchMaxEntries     int
	streamBatchMaxBytes       int
	streamBatchMaxWait        time.Duration
	reconcileApplyWorkers     int
	reconcilePutBatchEntries  int
	reconcilePutBatchBytes    int

	rateMu                  sync.Mutex
	rateLastSampleAt        time.Time
	rateLastPrimaryIndex    uint64
	rateLastAppliedIndex    uint64
	rateLastLag             int64
	primaryWriteRateEPS     float64
	secondaryApplyRateEPS   float64
	lagSlopeEPS             float64
	predictedCatchupSeconds float64
	lagGrowthStartAt        time.Time
	convergenceMinRateRatio float64
	convergenceStall        time.Duration

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
	reconcilePhase          atomic.Uint32

	// streamResumeAttempts tracks consecutive resume attempts after
	// a non-redirect stream disconnect.  Reset on successful streaming
	// or after falling back to reconciliation.
	streamResumeAttempts int

	// applyYieldDuration is the pause inserted after each successful
	// Raft commit in the apply path to give the secondary's Raft
	// heartbeat goroutines time to run.
	applyYieldDuration time.Duration
}

// newDRReplicationSecondary creates a new secondary replication manager.
func newDRReplicationSecondary(core *Core, replSalt []byte, relationshipID string, logger log.Logger) *drReplicationSecondary {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true // Secondary needs reverse KID->key mapping for deletes
	config.RequireTransactionalSnapshot = true
	config.ValueDomain = reconciler.ValueDomainCiphertext
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
		reconcileApplyWorkers:      drDefaultReconcileApplyWorkers,
		reconcilePutBatchEntries:   drDefaultReconcilePutBatchEntries,
		reconcilePutBatchBytes:     drDefaultReconcilePutBatchBytes,
		convergenceMinRateRatio:    drDefaultConvergenceMinRateRatio,
		convergenceStall:           drDefaultConvergenceStall,
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
	sec.lastReconcileApplyAt.Store(now)
	return sec
}

// Connect establishes the gRPC connection to the primary.
// The connection is strictly mTLS-only via DRReplicationALPN and fails
// closed if transport credentials cannot be established.
func (s *drReplicationSecondary) Connect(ctx context.Context, primaryAddr string, opts ...grpc.DialOption) error {
	s.logger.Info("connecting to primary", "addr", primaryAddr)

	// Load the monotonic checkpoint high-water mark from barrier storage
	// on first connect (or after restart).
	if !s.highestCommittedCheckpointIndexSet {
		s.loadCheckpointHighWaterMark()
	}

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

	// Lazily initialize the trust pool (persists across reconnects).
	s.trustedPrimaryCertsMu.Lock()
	if s.trustedPrimaryCerts == nil {
		s.trustedPrimaryCerts = make(map[string]*trustedPrimaryCert)
	}
	initTrustedPool(s.trustedPrimaryCerts, parsedCert)
	s.trustedPrimaryCertsMu.Unlock()

	client := &drReplicationClusterClient{
		core:           s.core,
		primaryCACert:  parsedCert,
		logger:         s.logger,
		trustedCertsMu: &s.trustedPrimaryCertsMu,
		trustedCerts:   s.trustedPrimaryCerts,
	}
	s.drClusterClient = client

	// Ensure client registration is idempotent across reconnects.
	cl.RemoveClient(consts.DRReplicationALPN)
	cl.AddClient(consts.DRReplicationALPN, client)

	dialerFunc := cl.GetContextDialerFunc(ctx, consts.DRReplicationALPN)
	opts = append(opts,
		grpc.WithContextDialer(dialerFunc),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")),
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
	heartbeatErrCh := make(chan error, 1)
	go s.runHeartbeatLoop(heartbeatCtx, heartbeatErrCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		case err := <-heartbeatErrCh:
			if err != nil {
				// Cancel any in-flight stream so Start can return and the
				// controller reconnect loop can establish a fresh transport.
				s.cancelActiveStream()
				return fmt.Errorf("heartbeat trust validation failed: %w", err)
			}
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

				// Redirect during initial reconciliation -- return to
				// controller to reconnect to the actual leader.
				if class == drReconcileFailureRedirect {
					if addr, ok := extractDRRedirect(err); ok {
						s.logger.Info("initial reconciliation redirected to new leader", "addr", addr)
						return &errDRRedirect{LeaderAddr: addr}
					}
					return fmt.Errorf("initial reconciliation hit standby, reconnecting: %w", err)
				}

				s.reconcileRetries.Add(1)
				if !s.shouldRetryReconcile(class) {
					// Retry cap exhausted -- return to the controller
					// so it can re-establish the gRPC connection. This
					// handles the case where the secondary is connected
					// to a node that keeps failing (e.g. wrong node
					// after a stepdown) and needs a fresh Connect().
					return fmt.Errorf("initial reconciliation retry cap exhausted (class=%s): %w", class, err)
				}
				time.Sleep(s.nextReconcileRetryDelay(class))
				continue
			}
			s.markReconcileSuccess()
			s.sessionMu.RLock()
			commitIdx := s.activeCheckpointIndex
			s.sessionMu.RUnlock()
			if commitIdx > 0 {
				s.commitCheckpointIndex(commitIdx)
			}

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

			s.streamResumeAttempts = 0
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

				// Redirect: must reconnect via controller to reach the new leader.
				if addr, ok := extractDRRedirect(err); ok {
					s.logger.Info("stream returned redirect to new leader", "addr", addr)
					s.streamResumeAttempts = 0
					s.setState(DRSecondaryReconciling)
					return &errDRRedirect{LeaderAddr: addr}
				}

				// If the primary explicitly says reconciliation is needed
				// (buffer/journal too old to catch up), go directly to
				// reconciling without wasting resume attempts.
				if isReconciliationRequired(err) {
					s.logger.Info("primary requires reconciliation", "error", err)
					s.streamResumeAttempts = 0
					s.setState(DRSecondaryReconciling)
					continue
				}

				// Transient error: attempt stream resume.  The primary's
				// StreamChanges handler has built-in catch-up replay from
				// the buffer/journal keyed on lastAppliedIndex, so a
				// simple reconnect often avoids a full reconciliation.
				s.streamResumeAttempts++
				if s.streamResumeAttempts > drMaxStreamResumeAttempts {
					s.logger.Warn("stream resume attempts exhausted, falling back to reconciliation",
						"attempts", s.streamResumeAttempts)
					s.streamResumeAttempts = 0
					s.setState(DRSecondaryReconciling)
					continue
				}

				s.logger.Info("attempting stream resume",
					"attempt", s.streamResumeAttempts,
					"last_applied", s.lastAppliedIndex.Load())
				time.Sleep(500 * time.Millisecond)
				continue // Re-enter Streaming case -> runStream again
			}
			// Stream ended cleanly (EOF). Reset resume counter.
			s.streamResumeAttempts = 0

		case DRSecondaryReconciling:
			if requested, reason := s.consumeResnapshotRequest(); requested {
				s.setState(DRSecondaryResnapshotting)
				s.setFallbackLastReason(reason)
				continue
			}
			if err := s.runReconciliation(ctx); err != nil {
				s.logger.Error("reconciliation failed", "error", err)
				class := s.markReconcileFailure(err)

				// If the reconciler hit a standby (redirect), abort
				// immediately and return to the controller so it can
				// reconnect to the actual leader.
				if class == drReconcileFailureRedirect {
					if addr, ok := extractDRRedirect(err); ok {
						s.logger.Info("reconciliation redirected to new leader", "addr", addr)
						return &errDRRedirect{LeaderAddr: addr}
					}
					// Redirect classified but address not parseable;
					// fall through to generic reconnect.
					return fmt.Errorf("reconciliation hit standby, reconnecting: %w", err)
				}

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
					// Retry cap exhausted -- return to the controller
					// so it can re-establish the gRPC connection. This
					// breaks the internal retry loop when the secondary
					// is stuck talking to the wrong node.
					return fmt.Errorf("reconciliation retry cap exhausted (class=%s): %w", class, err)
				}
				time.Sleep(s.nextReconcileRetryDelay(class))
				continue
			}
			s.markReconcileSuccess()
			s.sessionMu.RLock()
			reconcileCommitIdx := s.activeCheckpointIndex
			s.sessionMu.RUnlock()
			if reconcileCommitIdx > 0 {
				s.commitCheckpointIndex(reconcileCommitIdx)
			}
			s.streamResumeAttempts = 0
			s.setState(DRSecondaryStreaming)

		case DRSecondaryResnapshotting:
			s.setReconcilePhase(drReconcilePhaseResnapshot)
			reason := s.getFallbackLastReason()
			if reason == "" {
				reason = "manual"
			}
			if err := s.performResnapshot(ctx, reason); err != nil {
				s.logger.Error("resnapshot fallback failed", "error", err)
				class := s.markReconcileFailure(err)

				// Redirect during resnapshot -- return to controller.
				if class == drReconcileFailureRedirect {
					if addr, ok := extractDRRedirect(err); ok {
						s.logger.Info("resnapshot redirected to new leader", "addr", addr)
						return &errDRRedirect{LeaderAddr: addr}
					}
					return fmt.Errorf("resnapshot hit standby, reconnecting: %w", err)
				}

				s.reconcileRetries.Add(1)
				time.Sleep(s.nextReconcileRetryDelay(class))
				s.setState(DRSecondaryReconciling)
				continue
			}
			s.markReconcileSuccess()
			s.sessionMu.RLock()
			resnapshotCommitIdx := s.activeCheckpointIndex
			s.sessionMu.RUnlock()
			if resnapshotCommitIdx > 0 {
				s.commitCheckpointIndex(resnapshotCommitIdx)
			}
			s.streamResumeAttempts = 0
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

	// Regenerate the Raft TLS keyring (if using Raft storage).
	//
	// During DR replication the primary's barrier key (core/keyring,
	// core/root-key) replaces the secondary's original key. However,
	// the secondary's own core/raft/tls entry (in the
	// drNeverReplicatePrefixes exclusion list) was written with the
	// OLD barrier key and is now unreadable. Without a valid TLS
	// keyring Raft nodes cannot establish peer connections, leader
	// election fails, and the promoted cluster is permanently stuck.
	//
	// Force-recreate the keyring: delete the orphaned entry and
	// generate a fresh one encrypted with the current (primary's)
	// barrier key, then apply it to the running Raft backend.
	if s.core.getRaftBackend() != nil {
		if _, err := s.core.raftForceRecreateTLSKeyring(ctx); err != nil {
			s.logger.Error("failed to regenerate raft TLS keyring during promotion", "error", err)
			return fmt.Errorf("failed to regenerate raft TLS keyring: %w", err)
		}
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
	if state != DRSecondaryReconciling && state != DRSecondaryResnapshotting {
		s.setReconcilePhase(drReconcilePhaseIdle)
	}
}

func (s *drReplicationSecondary) setLastAppliedIndex(index uint64) {
	s.lastAppliedIndex.Store(index)
	s.lastAppliedAt.Store(time.Now().UTC().Unix())
}

func (s *drReplicationSecondary) setReconcilePhase(phase drReconcilePhase) {
	s.reconcilePhase.Store(uint32(phase))
}

func (s *drReplicationSecondary) reconcilePhaseString() string {
	return drReconcilePhase(s.reconcilePhase.Load()).String()
}

func (s *drReplicationSecondary) markReconcileActivityNow() {
	s.lastReconcileActivityAt.Store(time.Now().UTC().Unix())
}

func (s *drReplicationSecondary) markReconcileApplyNow() {
	now := time.Now().UTC().Unix()
	s.lastReconcileApplyAt.Store(now)
	s.lastReconcileActivityAt.Store(now)
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
	lastApply := s.lastReconcileApplyAt.Load()
	if lastApply > lastActivity {
		lastActivity = lastApply
	}
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
	if state := s.State(); state == DRSecondaryReconciling || state == DRSecondaryResnapshotting {
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
	primaryRate, secondaryRate, lagSlope, predictedCatchup, lagEntries := s.rateSnapshot()
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
		ReconcilePhase:                 s.reconcilePhaseString(),
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
		PrimaryWriteRateEPS:            primaryRate,
		SecondaryApplyRateEPS:          secondaryRate,
		LagEntries:                     lagEntries,
		LagSlopeEPS:                    lagSlope,
		PredictedCatchupSeconds:        predictedCatchup,
		ReconcilePutWorkersActive:      s.reconcilePutWorkers.Load(),
		ReconcileDeletePhaseSeconds:    float64(s.reconcileDeletePhaseMS.Load()) / 1000.0,
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
	ReconcilePhase                 string
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
	PrimaryWriteRateEPS            float64
	SecondaryApplyRateEPS          float64
	LagEntries                     uint64
	LagSlopeEPS                    float64
	PredictedCatchupSeconds        float64
	ReconcilePutWorkersActive      int64
	ReconcileDeletePhaseSeconds    float64
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
	if cfg.ReconcileApplyWorkers > 0 {
		s.reconcileApplyWorkers = cfg.ReconcileApplyWorkers
	}
	if cfg.ReconcilePutBatchMaxEntries > 0 {
		s.reconcilePutBatchEntries = cfg.ReconcilePutBatchMaxEntries
	}
	if cfg.ReconcilePutBatchMaxBytes > 0 {
		s.reconcilePutBatchBytes = cfg.ReconcilePutBatchMaxBytes
	}
	if cfg.ConvergenceMinRateRatio > 0 {
		s.convergenceMinRateRatio = cfg.ConvergenceMinRateRatio
	}
	if cfg.ConvergenceStallSeconds > 0 {
		s.convergenceStall = time.Duration(cfg.ConvergenceStallSeconds) * time.Second
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

func (s *drReplicationSecondary) runHeartbeatLoop(ctx context.Context, errCh chan<- error) {
	ticker := time.NewTicker(drHeartbeatInterval)
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
			// If the heartbeat returns a redirect, cache the leader address
			// so the controller can use it for reconnection.
			if err != nil {
				if addr, ok := extractDRRedirect(err); ok {
					s.lastKnownLeaderAddr.Store(&addr)
				}
			}
			continue
		}
		s.primaryIndex.Store(resp.PrimaryIndex)
		s.updateConvergenceTelemetry(resp.PrimaryIndex, s.lastAppliedIndex.Load(), time.Now().UTC())

		// Cache the leader's cluster address for redirect-based reconnection.
		if addr := resp.GetLeaderClusterAddr(); addr != "" {
			s.lastKnownLeaderAddr.Store(&addr)
		}

		// Verify and add the active leader's cluster certificate to the
		// CA-scoped trust pool and prune expired entries.
		if cert := resp.GetActiveClusterCert(); len(cert) > 0 {
			if cl := s.drClusterClient; cl != nil {
				if err := cl.addTrustedCert(cert); err != nil {
					wrappedErr := fmt.Errorf("heartbeat certificate rejected by trust pool: %w", err)
					s.logger.Error("heartbeat certificate rejected by trust pool; forcing reconnect", "error", wrappedErr)
					// Cancel any active stream immediately so the
					// reconnect controller can take over.
					s.cancelActiveStream()
					if errCh != nil {
						select {
						case errCh <- wrappedErr:
						default:
						}
					}
					return
				}
				cl.pruneTrustedCerts()
			}
		}
	}
}

func (s *drReplicationSecondary) updateConvergenceTelemetry(primaryIndex uint64, appliedIndex uint64, now time.Time) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	if s.rateLastSampleAt.IsZero() {
		s.rateLastSampleAt = now
		s.rateLastPrimaryIndex = primaryIndex
		s.rateLastAppliedIndex = appliedIndex
		s.rateLastLag = int64(primaryIndex) - int64(appliedIndex)
		return
	}

	dt := now.Sub(s.rateLastSampleAt).Seconds()
	if dt <= 0 {
		return
	}

	dp := int64(primaryIndex) - int64(s.rateLastPrimaryIndex)
	if dp < 0 {
		dp = 0
	}
	da := int64(appliedIndex) - int64(s.rateLastAppliedIndex)
	if da < 0 {
		da = 0
	}

	primaryRate := float64(dp) / dt
	secondaryRate := float64(da) / dt
	lag := int64(primaryIndex) - int64(appliedIndex)
	lagSlope := float64(lag-s.rateLastLag) / dt

	if s.primaryWriteRateEPS == 0 {
		s.primaryWriteRateEPS = primaryRate
	} else {
		s.primaryWriteRateEPS = (s.primaryWriteRateEPS * 0.7) + (primaryRate * 0.3)
	}
	if s.secondaryApplyRateEPS == 0 {
		s.secondaryApplyRateEPS = secondaryRate
	} else {
		s.secondaryApplyRateEPS = (s.secondaryApplyRateEPS * 0.7) + (secondaryRate * 0.3)
	}
	if s.lagSlopeEPS == 0 {
		s.lagSlopeEPS = lagSlope
	} else {
		s.lagSlopeEPS = (s.lagSlopeEPS * 0.7) + (lagSlope * 0.3)
	}

	if lag > 0 && s.secondaryApplyRateEPS > s.primaryWriteRateEPS {
		s.predictedCatchupSeconds = float64(lag) / (s.secondaryApplyRateEPS - s.primaryWriteRateEPS)
	} else {
		s.predictedCatchupSeconds = -1
	}

	if s.lagSlopeEPS > 0 {
		if s.lagGrowthStartAt.IsZero() {
			s.lagGrowthStartAt = now
		}
	} else {
		s.lagGrowthStartAt = time.Time{}
	}

	s.rateLastSampleAt = now
	s.rateLastPrimaryIndex = primaryIndex
	s.rateLastAppliedIndex = appliedIndex
	s.rateLastLag = lag
}

func (s *drReplicationSecondary) rateSnapshot() (primaryRate float64, secondaryRate float64, lagSlope float64, predictedCatchup float64, lagEntries uint64) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	primaryRate = s.primaryWriteRateEPS
	secondaryRate = s.secondaryApplyRateEPS
	lagSlope = s.lagSlopeEPS
	predictedCatchup = s.predictedCatchupSeconds
	primary := s.primaryIndex.Load()
	applied := s.lastAppliedIndex.Load()
	if primary > applied {
		lagEntries = primary - applied
	}
	return
}

func (s *drReplicationSecondary) convergenceFallbackEligible(now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	lag := uint64(0)
	primary := s.primaryIndex.Load()
	applied := s.lastAppliedIndex.Load()
	if primary > applied {
		lag = primary - applied
	}
	minLag := s.fallbackMinLagEntries
	if minLag == 0 {
		minLag = uint64(2 * drStreamBufferMaxEntries)
	}
	if lag < minLag {
		return false
	}
	if s.lagGrowthStartAt.IsZero() || now.Sub(s.lagGrowthStartAt) < s.convergenceStall {
		return false
	}
	if s.primaryWriteRateEPS <= 0 {
		return false
	}
	ratio := s.secondaryApplyRateEPS / s.primaryWriteRateEPS
	return ratio < s.convergenceMinRateRatio
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

func (s *drReplicationSecondary) hardStallFallbackEligible(now time.Time) bool {
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
	stall := s.reconcileStallAbort
	if stall <= 0 {
		stall = s.fallbackStall
	}
	if stall <= 0 {
		stall = drDefaultFallbackStall
	}
	lastActivity := s.lastReconcileActivityAt.Load()
	if lastActivity <= 0 {
		return false
	}
	return now.Sub(time.Unix(lastActivity, 0)) >= stall
}

func (s *drReplicationSecondary) fallbackRateAllowed(now time.Time) bool {
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()

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
	return len(s.fallbackTriggerEvents) < maxPerHour
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
	if class == drReconcileFailureStalled &&
		(s.convergenceFallbackEligible(now) || s.hardStallFallbackEligible(now)) {
		return s.fallbackRateAllowed(now)
	}
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
	return s.fallbackRateAllowed(now)
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
	s.reconcileQueueDepth.Store(0)
	s.reconcileRangesInflight.Store(0)
	s.reconcileRPCBytesUsed.Store(0)
	s.reconcileTaskRateMillis.Store(0)
	s.reconcileDeletePhaseMS.Store(0)
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))

	checkpointCtx, checkpointCancel := context.WithTimeout(ctx, drDefaultCheckpointRPCTimeout)
	checkpoint, err := s.client.RequestCheckpoint(checkpointCtx, &CheckpointRequest{RelationshipId: s.relationshipID})
	checkpointCancel()
	if err != nil {
		return fmt.Errorf("failed to request checkpoint for resnapshot: %w", err)
	}

	if err := s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("failed to begin reconcile session: %w", err)
	}
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
	defer fetchCancel()

	stallAfter := s.reconcileStallAbort
	if stallAfter <= 0 {
		stallAfter = 2 * time.Minute
	}
	var stallTriggered atomic.Bool
	progressAt := atomic.Int64{}
	markProgress := func() {
		now := time.Now().UTC()
		progressAt.Store(now.Unix())
		s.markReconcileActivityNow()
	}
	markProgress()

	watchdogStop := make(chan struct{})
	defer close(watchdogStop)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogStop:
				return
			case <-fetchCtx.Done():
				return
			case <-ticker.C:
				last := progressAt.Load()
				if last <= 0 {
					continue
				}
				since := time.Since(time.Unix(last, 0))
				if since >= stallAfter {
					if stallTriggered.CompareAndSwap(false, true) {
						s.logger.Warn("resnapshot stalled, aborting session",
							"stall_for", since.Round(time.Second),
							"checkpoint_id", checkpoint.CheckpointId,
							"checkpoint_index", checkpoint.CommitIndex)
						fetchCancel()
					}
					return
				}
			}
		}
	}()

	stream, err := s.client.FetchEntries(fetchCtx, &FetchEntriesRequest{
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
		Ranges:          []*RangeSpan{rangeSpanToProto(full)},
		IncludeDeletes:  true,
	})
	if err != nil {
		if stallTriggered.Load() || errors.Is(fetchCtx.Err(), context.Canceled) {
			return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot fetch start", fmt.Errorf("no resnapshot progress for %s", stallAfter.Round(time.Second)))
		}
		return fmt.Errorf("failed to start resnapshot fetch: %w", err)
	}
	markProgress()

	remoteKeys := make(map[string]struct{}, 1024)
	applied := 0
	applyPipeline := newDRPutApplyPipeline(fetchCtx, s, nil, markProgress)
	pipelineClosed := false
	defer func() {
		if !pipelineClosed {
			_ = applyPipeline.closeAndWait()
		}
	}()
	for {
		batch, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if stallTriggered.Load() || errors.Is(fetchCtx.Err(), context.Canceled) {
				return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot fetch stream", fmt.Errorf("no resnapshot progress for %s", stallAfter.Round(time.Second)))
			}
			if errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
				return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot fetch stream", fmt.Errorf("resnapshot exceeded max wall time %s", fetchTimeout.Round(time.Second)))
			}
			return fmt.Errorf("resnapshot fetch stream failed: %w", recvErr)
		}
		markProgress()
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			return fmt.Errorf("checkpoint conflict: %w", err)
		}
		if len(batch.GetFailedKids()) > 0 {
			return fmt.Errorf("resnapshot fetch returned failed_kids: %d", len(batch.GetFailedKids()))
		}
		if len(batch.GetEntries()) > 0 {
			s.reconcileRPCBytesUsed.Add(uint64(len(batch.GetEntries())) * 128)
			entries := make([]*EntryChange, 0, len(batch.GetEntries()))
			for _, e := range batch.GetEntries() {
				entries = append(entries, cloneEntryChange(e))
			}
			if err := applyPipeline.submit(entries); err != nil {
				if stallTriggered.Load() || errors.Is(fetchCtx.Err(), context.Canceled) {
					return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot apply queue", fmt.Errorf("no resnapshot progress for %s", stallAfter.Round(time.Second)))
				}
				return fmt.Errorf("resnapshot apply queue failed: %w", err)
			}
			markProgress()
			applied += len(entries)
			for _, e := range entries {
				if e != nil && e.Key != "" {
					remoteKeys[e.Key] = struct{}{}
				}
			}
		}
	}
	s.setReconcilePhase(drReconcilePhaseApplyPuts)
	if err := applyPipeline.closeAndWait(); err != nil {
		if stallTriggered.Load() || errors.Is(fetchCtx.Err(), context.Canceled) {
			return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot apply", fmt.Errorf("no resnapshot progress for %s", stallAfter.Round(time.Second)))
		}
		return fmt.Errorf("resnapshot apply failed: %w", err)
	}
	pipelineClosed = true
	markProgress()

	localCheckpoint := reconciler.Checkpoint{
		ID:          checkpoint.CheckpointId,
		CommitIndex: checkpoint.CommitIndex,
	}
	localSet, err := s.scanner.ScanPhysical(fetchCtx, s.core.physical, localCheckpoint)
	if err != nil {
		if stallTriggered.Load() || errors.Is(fetchCtx.Err(), context.Canceled) {
			return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot local scan", fmt.Errorf("no resnapshot progress for %s", stallAfter.Round(time.Second)))
		}
		s.scanFailures.Add(1)
		return fmt.Errorf("failed to scan local state after resnapshot fetch: %w", err)
	}
	markProgress()

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
		s.setReconcilePhase(drReconcilePhaseApplyDeletes)
		deleteCtx, deleteCancel := context.WithTimeout(fetchCtx, stallAfter)
		err := s.applyRemovedKeys(deleteCtx, checkpoint.CheckpointId, checkpoint.CommitIndex, removeKeys)
		deleteCancel()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(deleteCtx.Err(), context.DeadlineExceeded) {
			return wrapReconcileFailure(drReconcileFailureStalled, "resnapshot delete phase", fmt.Errorf("delete phase exceeded %s", stallAfter.Round(time.Second)))
		}
		if err != nil {
			return fmt.Errorf("resnapshot delete phase failed: %w", err)
		}
		markProgress()
	}

	s.setReconcilePhase(drReconcilePhaseFinalize)
	s.setLastAppliedIndex(checkpoint.CommitIndex)
	s.entriesApplied.Add(uint64(applied))
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.reconcileQueueDepth.Store(0)
	s.reconcileRangesInflight.Store(0)
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

	// Derive the same AAD binding parameters used by the primary:
	// - clusterID from the DR config
	// - secondary's own cert fingerprint
	// - primary identity from the DR transport CA SPKI hash
	unwrapClusterID := ""
	unwrapPrimaryIdentity := ""
	if mgr := s.core.drManager; mgr != nil {
		cfg := mgr.Config()
		unwrapClusterID = cfg.ClusterID
	}
	if len(s.primaryCACert) > 0 {
		if caCert, parseErr := x509.ParseCertificate(s.primaryCACert); parseErr == nil {
			// Compute SPKI hash to match what the primary used.
			if spki, spkiErr := x509.MarshalPKIXPublicKey(caCert.PublicKey); spkiErr == nil {
				h := sha256.Sum256(spki)
				unwrapPrimaryIdentity = hex.EncodeToString(h[:])
			}
		}
	}
	unwrapSecondaryFP := ""
	if localCert := s.core.localClusterParsedCert.Load(); localCert != nil {
		unwrapSecondaryFP = certFingerprintSHA256(localCert)
	}

	rootKey, err := unwrapRootKeyFromPrimary(
		resp.WrappedRootKey,
		s.relationshipID,
		unwrapClusterID,
		unwrapSecondaryFP,
		unwrapPrimaryIdentity,
		resp.ServerEphemeralPubkey,
		clientPriv,
		clientNonce,
		resp.ServerNonce,
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
//
// The stream is bidirectional: the secondary sends an init message
// followed by periodic WindowUpdate credits after each batch flush;
// the primary sends EntryChange messages gated by those credits.
func (s *drReplicationSecondary) runStream(ctx context.Context) error {
	s.logger.Info("starting change stream",
		"last_applied_index", s.lastAppliedIndex.Load())

	streamCtx, streamCancel := context.WithCancel(ctx)
	s.setActiveStreamCancel(streamCancel)
	defer func() {
		streamCancel()
		s.setActiveStreamCancel(nil)
	}()

	stream, err := s.client.StreamChanges(streamCtx)
	if err != nil {
		return fmt.Errorf("failed to open change stream: %w", err)
	}

	applyQueueEntries := s.streamBatchMaxEntries * 4
	if applyQueueEntries < 1024 {
		applyQueueEntries = 1024
	}
	// Size the batch channel by the number of gRPC batches, not
	// individual entries.  Each batch can carry up to 64 entries
	// (drStreamSendBatchMaxEntries), so the channel depth is the
	// total capacity divided by the expected batch size.
	applyQueueBatches := applyQueueEntries / drStreamSendBatchMaxEntries
	if applyQueueBatches < 64 {
		applyQueueBatches = 64
	}

	// Send the init message with the initial credit window.
	if err := stream.Send(&StreamChangesUpstream{
		Msg: &StreamChangesUpstream_Init{
			Init: &StreamChangesRequest{
				RelationshipId:   s.relationshipID,
				LastAppliedIndex: s.lastAppliedIndex.Load(),
				InitialWindow:    uint64(applyQueueEntries),
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send stream init: %w", err)
	}

	applyCh := make(chan []*EntryChange, applyQueueBatches)
	creditReplenishCh := make(chan uint64, 64)
	applyErrCh := make(chan error, 1)
	go func() {
		applyErrCh <- s.runStreamApplyWorker(ctx, applyCh, creditReplenishCh)
	}()

	// Credit-sender goroutine: reads replenishment counts from the
	// apply worker and sends WindowUpdate messages to the primary.
	go func() {
		for {
			select {
			case <-streamCtx.Done():
				return
			case credits, ok := <-creditReplenishCh:
				if !ok {
					return
				}
				if credits == 0 {
					continue
				}
				_ = stream.Send(&StreamChangesUpstream{
					Msg: &StreamChangesUpstream_WindowUpdate{
						WindowUpdate: &WindowUpdate{
							Credits: credits,
						},
					},
				})
			}
		}
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

		batch, err := stream.Recv()
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

		// Validate monotonic index ordering and filter stale entries,
		// then deliver the entire batch slice in one channel send to
		// reduce per-entry channel overhead and let the apply worker
		// form optimal transaction batches.
		entries := batch.GetEntries()
		filtered := make([]*EntryChange, 0, len(entries))
		for _, change := range entries {
			// Monotonic index check: indices must never go backwards.
			// Gaps (change.RaftIndex > expectedNext) are expected and
			// normal because the primary filters out non-replicable
			// entries (core/dr-replication/*, core/raft/*, etc.) before
			// sending.  The Raft indices of filtered entries are simply
			// skipped, creating benign gaps.  Real data-loss scenarios
			// (ring buffer overflow, subscriber lag) are handled on the
			// primary side by cancelling the subscriber, which tears
			// down the stream.
			//
			// Equal indices are valid: Raft transactions can commit
			// multiple operations under the same log index.
			if change.RaftIndex > 0 && change.RaftIndex < expectedNext-1 {
				s.logger.Warn("received out-of-order raft index on change stream",
					"expected_at_least", expectedNext,
					"received_index", change.RaftIndex)
				continue
			}

			if next := change.RaftIndex + 1; next > expectedNext {
				expectedNext = next
			}
			filtered = append(filtered, change)
		}

		if len(filtered) > 0 {
			// Block until the apply worker consumes the batch. This
			// exerts natural backpressure on the primary via the credit
			// system: while we're blocked here, no WindowUpdate is sent,
			// so the primary stops sending new entries.  Only tear down
			// the stream if we've been blocked longer than the stall
			// timeout (indicating a genuine apply failure, not a
			// transient slow-down).
			stallTimeout := s.reconcileStallAbort
			if stallTimeout <= 0 {
				stallTimeout = drDefaultReconcileStallAbort
			}
			timer := time.NewTimer(stallTimeout)
			select {
			case applyCh <- filtered:
				timer.Stop()
			case <-timer.C:
				close(applyCh)
				_ = <-applyErrCh
				return fmt.Errorf("change stream apply stalled for %s; reconciliation required", stallTimeout)
			case <-ctx.Done():
				timer.Stop()
				close(applyCh)
				_ = <-applyErrCh
				return ctx.Err()
			case <-s.stopCh:
				timer.Stop()
				close(applyCh)
				_ = <-applyErrCh
				return nil
			}
		}
	}
}

// runStreamApplyWorker consumes entries from the apply channel and persists them
// to storage. It attempts to batch updates into transactions for performance.
// After each successful flush it sends the number of flushed entries to
// creditReplenishCh so the credit-sender goroutine can issue a WindowUpdate
// to the primary.
func (s *drReplicationSecondary) runStreamApplyWorker(ctx context.Context, applyCh <-chan []*EntryChange, creditReplenishCh chan<- uint64) error {
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

	// pendingCredits accumulates credit counts that could not be sent
	// to the credit-sender goroutine because creditReplenishCh was full.
	// On each flush the total (new + pending) is sent, ensuring credits
	// are never silently dropped.
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
			// Channel full -- credits are accumulated in pendingCredits
			// and will be sent with the next successful send.
		}
	}

	// Probe transaction support once. If the backend doesn't support
	// transactions we skip the attempt entirely on every flush.
	txnBackend, txnSupported := s.core.physical.(physical.Transactional)
	var txnLogOnce sync.Once

	// yieldDuration is a short pause after each Raft commit that gives
	// the secondary's Raft heartbeat goroutines time to run, preventing
	// quorum loss under sustained apply pressure.
	yieldDuration := s.applyYieldDuration
	if yieldDuration <= 0 {
		yieldDuration = drDefaultApplyYieldDuration
	}

	applyYield := func() {
		runtime.Gosched()
		time.Sleep(yieldDuration)
	}

	// Flush consumes the current batch. It optimistically tries a transaction,
	// and falls back to sequential application on error.
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		flushedCount := len(batch)
		applyStart := time.Now()

		// Optimization: Try to apply as a transaction if supported
		if txnSupported {
			if err := s.applyStreamTxn(ctx, txnBackend, batch); err == nil {
				// Transaction succeeded
				txnLogOnce.Do(func() {
					s.logger.Info("stream apply using transactional batching",
						"batch_max_entries", maxEntries,
						"batch_max_bytes", maxBytes)
				})
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_success"}, 1)
				metrics.MeasureSince([]string{"replication", "dr", "secondary", "apply_latency"}, applyStart)

				// Clear batch
				batch = batch[:0]
				batchBytes = 0
				replenishCredits(flushedCount)
				applyYield()
				return nil
			} else {
				// Transaction failed; log warning and fall back to sequential
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_fallback"}, 1)
				s.logger.Warn("transactional batch apply failed, falling back to sequential",
					"error", err, "batch_size", flushedCount)
			}
		}

		// Fallback: apply sequentially
		for _, change := range batch {
			current := s.lastAppliedIndex.Load()
			if change.RaftIndex < current {
				continue
			}
			if err := s.applyStreamChange(ctx, change); err != nil {
				return fmt.Errorf("failed to apply change sequentially: %w", err)
			}
			s.setLastAppliedIndex(change.RaftIndex)
			s.entriesApplied.Add(1)
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "entries_applied"}, 1)
			metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(change.RaftIndex))
		}

		metrics.MeasureSince([]string{"replication", "dr", "secondary", "apply_latency"}, applyStart)
		batch = batch[:0]
		batchBytes = 0
		replenishCredits(flushedCount)
		applyYield()
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
		case incoming, ok := <-applyCh:
			if !ok {
				return flush()
			}
			for _, change := range incoming {
				batch = append(batch, change)
				// Rough estimate of memory size: key + value + overhead
				batchBytes += len(change.Key) + len(change.Value) + 48
			}
			if len(batch) >= maxEntries || batchBytes >= maxBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
}

// applyStreamTxn attempts to apply a batch of changes in a single transaction.
func (s *drReplicationSecondary) applyStreamTxn(ctx context.Context, backend physical.Transactional, batch []*EntryChange) error {
	txn, err := backend.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer txn.Rollback(ctx)

	current := s.lastAppliedIndex.Load()
	affected := 0
	var lastIndex uint64
	var keyRotationDetected bool

	for _, change := range batch {
		if change.RaftIndex < current {
			continue
		}

		// Index-advance marker: empty key means the primary filtered
		// all entries in a Raft batch. Track the index so
		// lastAppliedIndex advances, but don't touch storage.
		if change.Key == "" {
			if change.RaftIndex > lastIndex {
				lastIndex = change.RaftIndex
			}
			continue
		}

		if isDRNeverReplicatePath(change.Key) {
			continue
		}

		op := physical.Operation(change.OpType)
		switch op {
		case physical.DeleteOperation:
			if err := txn.Delete(ctx, change.Key); err != nil {
				return err
			}
		case physical.PutOperation:
			entry := &physical.Entry{
				Key:      change.Key,
				Value:    change.Value,
				SealWrap: change.SealWrap,
			}
			if err := txn.Put(ctx, entry); err != nil {
				return err
			}

			// Check for special keys that trigger post-commit actions
			if change.Key == "core/keyring" || change.Key == "core/root-key" {
				keyRotationDetected = true
			}
		default:
			// Ignore unknown operations
		}

		affected++
		lastIndex = change.RaftIndex
	}

	// If we only saw index-advance markers (no storage ops) we still
	// need to advance lastAppliedIndex so lag reporting stays accurate.
	if affected == 0 && lastIndex > 0 && lastIndex > current {
		s.setLastAppliedIndex(lastIndex)
		metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(lastIndex))
		return nil
	}

	if affected == 0 {
		return nil
	}

	if err := txn.Commit(ctx); err != nil {
		return err
	}

	// Update state
	s.setLastAppliedIndex(lastIndex)
	s.entriesApplied.Add(uint64(affected))
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "entries_applied"}, float32(affected))
	metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(lastIndex))

	// Handle side effects (key rotation) AFTER commit
	if keyRotationDetected {
		// We call applyStreamChange for the specific keys again?
		// No, we should just invoke the reload logic directly.
		// Since the data is now COMMITTED, re-reading it from storage in Reload logic works.

		// Logic copied from applyStreamChange:
		s.logger.Info("keyring/root-key update detected via transaction batch")

		if err := s.core.barrier.ReloadRootKey(ctx); err != nil {
			s.logger.Warn("failed to reload root key after batch update", "error", err)
		}
		if err := s.core.barrier.ReloadKeyring(ctx); err != nil {
			// We don't have the specific key easily here without re-iterating, but logging generic is fine
			s.logger.Warn("failed to reload keyring after batch update", "error", err)
		}

		// Persist updated root key under secondary's seal for restart survival.
		if keyring, err := s.core.barrier.Keyring(); err == nil {
			if err := s.core.seal.SetStoredKeys(ctx, [][]byte{keyring.RootKey()}); err != nil {
				s.logger.Error("failed to persist rotated root key in seal", "error", err)
			}
		}
	}

	return nil
}

// applyStreamChange applies a single entry change from the FSM change
// stream to local physical storage. Stream values are already
// barrier-encrypted (they come from the primary's Raft log), so they
// must be written directly to the physical backend to avoid
// double-encryption.
func (s *drReplicationSecondary) applyStreamChange(ctx context.Context, change *EntryChange) error {
	// Index-advance marker: the primary sends entries with an empty key
	// when a Raft batch contained only non-replicable paths. There is
	// nothing to apply to storage; the caller advances lastAppliedIndex
	// using the marker's RaftIndex so lag reporting stays accurate.
	if change.Key == "" {
		return nil
	}

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
// FetchEntries (reconciliation). Reconciliation runs below the barrier,
// so fetched values are ciphertext bytes and must be written directly to
// physical storage.
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
		entry := &physical.Entry{
			Key:      change.Key,
			Value:    change.Value,
			SealWrap: change.SealWrap,
		}
		if err := s.core.physical.Put(ctx, entry); err != nil {
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
			return s.core.physical.Delete(ctx, change.Key)
		}
		// If Key is empty but KID is present, resolve via local KID map.
		if len(change.Kid) == 32 && kidToKey != nil {
			var kid [32]byte
			copy(kid[:], change.Kid)
			if key, ok := kidToKey[kid]; ok {
				if isDRNeverReplicatePath(key) {
					return nil
				}
				return s.core.physical.Delete(ctx, key)
			}
			s.logger.Warn("delete with KID but key not found in local map",
				"kid_prefix", fmt.Sprintf("%x", change.Kid[:8]))
		}
		return nil

	default:
		return fmt.Errorf("unknown operation type in fetched change: op_type=%s key=%s", change.OpType, change.Key)
	}
}

// --- Reconciliation mode ---

// runReconciliation performs the full IBLT/prefix digest reconciliation
// protocol against the primary.
func (s *drReplicationSecondary) runReconciliation(ctx context.Context) error {
	s.logger.Info("starting reconciliation")
	startTime := time.Now()

	// Bound the total time a single reconciliation attempt may run so
	// that a slow scan, checkpoint build, or stalled FetchEntries RPC
	// cannot hang the secondary indefinitely.
	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, drDefaultReconcileTimeout)
	defer reconcileCancel()

	s.setReconcilePhase(drReconcilePhaseRangeTasks)
	s.reconcileRPCBytesUsed.Store(0)
	s.rangeSplitCount.Store(0)
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
	s.reconcileQueueDepth.Store(0)
	defer func() {
		s.setReconcilePhase(drReconcilePhaseIdle)
		metrics.MeasureSince([]string{"replication", "dr", "secondary", "reconciliation_duration"}, startTime)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "reconciliation_count"}, 1)
	}()

	// Step 1: Request a checkpoint from the primary.
	// Use a dedicated longer timeout: the primary may need to build the
	// checkpoint artifact from scratch (full storage scan + KID/VID
	// index), which scales linearly with key count.
	checkpointCtx, checkpointCancel := context.WithTimeout(reconcileCtx, drDefaultCheckpointRPCTimeout)
	checkpoint, err := s.client.RequestCheckpoint(checkpointCtx, &CheckpointRequest{
		RelationshipId: s.relationshipID,
	})
	checkpointCancel()
	if err != nil {
		return fmt.Errorf("failed to request checkpoint: %w", err)
	}
	s.logger.Info("checkpoint established",
		"checkpoint_id", checkpoint.CheckpointId,
		"primary_commit_index", checkpoint.CommitIndex)
	if err := s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("failed to begin reconcile session: %w", err)
	}
	defer s.endReconcileSession()

	// Step 2: Build local reconciliation set.
	localCheckpoint := reconciler.Checkpoint{
		ID:          checkpoint.CheckpointId,
		CommitIndex: checkpoint.CommitIndex,
	}
	localSet, err := s.scanner.Scan(reconcileCtx, s.core.barrier, localCheckpoint)
	if err != nil {
		s.scanFailures.Add(1)
		return fmt.Errorf("failed to scan local storage: %w", err)
	}
	s.logger.Info("local scan complete", "keys", localSet.KeyCount)

	if err := s.runRangeReconciliation(reconcileCtx, checkpoint, localSet, startTime); err != nil {
		return err
	}
	return nil
}

type drRangeTask struct {
	span    reconciler.RangeSpan
	rangeID uint64
}

type drQueuedRangeTask struct {
	id       int
	priority int
	task     drRangeTask
}

type drRangeTaskResult struct {
	id             int
	task           drRangeTask
	rpcBytes       uint64
	fetchedEntries []*EntryChange
	removedKeys    []string
	err            error
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

type drPutApplyPipeline struct {
	secondary  *drReplicationSecondary
	ctx        context.Context
	cancel     context.CancelFunc
	kidToKey   map[[32]byte]string
	onApply    func()
	txnBackend physical.TransactionalBackend // nil if physical backend doesn't support transactions
	submitted  atomic.Int64                  // total entries submitted (for diagnostics)

	shards []chan *EntryChange
	wg     sync.WaitGroup

	mu     sync.Mutex
	err    error
	closed bool
}

func newDRPutApplyPipeline(ctx context.Context, secondary *drReplicationSecondary, kidToKey map[[32]byte]string, onApply func()) *drPutApplyPipeline {
	workers := secondary.reconcileApplyWorkers
	if workers <= 0 {
		workers = 1
	}
	queueDepth := workers * 512
	if queueDepth < 1024 {
		queueDepth = 1024
	}
	workerCtx, cancel := context.WithCancel(ctx)

	// Probe for TransactionalBackend support to enable batch commits.
	var txnBackend physical.TransactionalBackend
	if tb, ok := secondary.core.physical.(physical.TransactionalBackend); ok {
		txnBackend = tb
	}

	p := &drPutApplyPipeline{
		secondary:  secondary,
		ctx:        workerCtx,
		cancel:     cancel,
		kidToKey:   kidToKey,
		onApply:    onApply,
		txnBackend: txnBackend,
		shards:     make([]chan *EntryChange, workers),
	}
	for i := 0; i < workers; i++ {
		ch := make(chan *EntryChange, queueDepth/workers+1)
		p.shards[i] = ch
		p.wg.Add(1)
		go p.runWorker(ch)
	}
	return p
}

func (p *drPutApplyPipeline) runWorker(ch <-chan *EntryChange) {
	defer p.wg.Done()
	p.secondary.reconcilePutWorkers.Add(1)
	defer p.secondary.reconcilePutWorkers.Add(-1)

	maxEntries := p.secondary.reconcilePutBatchEntries
	if maxEntries <= 0 {
		maxEntries = drDefaultReconcilePutBatchEntries
	}
	maxBytes := p.secondary.reconcilePutBatchBytes
	if maxBytes <= 0 {
		maxBytes = drDefaultReconcilePutBatchBytes
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	// Yield duration to let the secondary's Raft heartbeat goroutines
	// run between reconciliation batch commits.
	yieldDuration := p.secondary.applyYieldDuration
	if yieldDuration <= 0 {
		yieldDuration = drDefaultApplyYieldDuration
	}

	batch := make([]*EntryChange, 0, maxEntries)
	batchBytes := 0

	flush := func() {
		if len(batch) == 0 {
			return
		}

		// Coalesce updates by key to reduce work.
		coalesced := make([]*EntryChange, 0, len(batch))
		putPosByKey := make(map[string]int, len(batch))
		for _, change := range batch {
			if change == nil {
				continue
			}
			if change.OpType == string(physical.PutOperation) && change.Key != "" {
				if pos, ok := putPosByKey[change.Key]; ok {
					coalesced[pos] = change
					continue
				}
				putPosByKey[change.Key] = len(coalesced)
			}
			coalesced = append(coalesced, change)
		}

		if p.txnBackend != nil {
			// Batch-apply using a single Raft transaction instead of
			// individual consensus rounds per entry. This turns N Raft
			// commits into 1, dramatically reducing apply time during
			// reconciliation of large key sets.
			if err := p.applyBatchTxn(coalesced); err != nil {
				p.setErr(fmt.Errorf("reconcile apply worker failed: %w", err))
				batch = batch[:0]
				batchBytes = 0
				return
			}
		} else {
			// Fallback: apply sequentially when transactions unsupported.
			for _, change := range coalesced {
				if err := p.secondary.applyFetchedChange(p.ctx, change, p.kidToKey); err != nil {
					p.setErr(fmt.Errorf("reconcile apply worker failed: %w", err))
					return
				}
			}
		}

		p.secondary.markReconcileApplyNow()
		if p.onApply != nil {
			p.onApply()
		}
		batch = batch[:0]
		batchBytes = 0

		// Yield to Raft heartbeat goroutines after each commit.
		runtime.Gosched()
		time.Sleep(yieldDuration)
	}

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			flush()
		case change, ok := <-ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, change)
			batchBytes += len(change.Key) + len(change.Value) + 48
			if len(batch) >= maxEntries || batchBytes >= maxBytes {
				flush()
			}
		}
	}
}

// applyBatchTxn applies a batch of coalesced changes in a single Raft
// transaction.  Entries that require special post-commit handling (the
// barrier keyring and root key) are extracted and applied individually
// after the transaction commits so that the reload + seal-persist logic
// in applyFetchedChange fires correctly.
func (p *drPutApplyPipeline) applyBatchTxn(entries []*EntryChange) error {
	// Partition entries: "normal" go into the transaction, "keyring"
	// entries need the special reload path in applyFetchedChange.
	var keyringEntries []*EntryChange
	normalEntries := make([]*EntryChange, 0, len(entries))

	for _, change := range entries {
		if change == nil {
			continue
		}
		if change.Key != "" && isDRNeverReplicatePath(change.Key) {
			continue
		}
		if change.Key == "core/keyring" || change.Key == "core/root-key" {
			keyringEntries = append(keyringEntries, change)
		} else {
			normalEntries = append(normalEntries, change)
		}
	}

	// Batch-commit normal entries in a single Raft round.
	if len(normalEntries) > 0 {
		tx, err := p.txnBackend.BeginTx(p.ctx)
		if err != nil {
			return fmt.Errorf("begin batch txn: %w", err)
		}

		for _, change := range normalEntries {
			switch physical.Operation(change.OpType) {
			case physical.PutOperation:
				if err := tx.Put(p.ctx, &physical.Entry{
					Key:      change.Key,
					Value:    change.Value,
					SealWrap: change.SealWrap,
				}); err != nil {
					_ = tx.Rollback(p.ctx)
					return fmt.Errorf("batch txn put %q: %w", change.Key, err)
				}

			case physical.DeleteOperation:
				key := change.Key
				if key == "" && len(change.Kid) == 32 && p.kidToKey != nil {
					var kid [32]byte
					copy(kid[:], change.Kid)
					if k, ok := p.kidToKey[kid]; ok {
						if isDRNeverReplicatePath(k) {
							continue
						}
						key = k
					}
				}
				if key != "" {
					if err := tx.Delete(p.ctx, key); err != nil {
						_ = tx.Rollback(p.ctx)
						return fmt.Errorf("batch txn delete %q: %w", key, err)
					}
				}

			default:
				_ = tx.Rollback(p.ctx)
				return fmt.Errorf("unknown operation type in fetched change: op_type=%s key=%s", change.OpType, change.Key)
			}
		}

		if err := tx.Commit(p.ctx); err != nil {
			return fmt.Errorf("commit batch txn (%d entries): %w", len(normalEntries), err)
		}
	}

	// Apply keyring / root-key entries individually so that the barrier
	// reload and seal-persist side-effects run in the correct order.
	for _, change := range keyringEntries {
		if err := p.secondary.applyFetchedChange(p.ctx, change, p.kidToKey); err != nil {
			return err
		}
	}

	return nil
}

func (p *drPutApplyPipeline) submit(entries []*EntryChange) error {
	if len(entries) == 0 {
		return p.Err()
	}
	var count int64
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		shard := hashEntryShard(entry, len(p.shards))
		select {
		case <-p.ctx.Done():
			return p.Err()
		case p.shards[shard] <- entry:
			count++
		}
	}
	p.submitted.Add(count)
	return p.Err()
}

func (p *drPutApplyPipeline) closeAndWait() error {
	p.mu.Lock()
	if p.closed {
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.closed = true
	chans := make([]chan *EntryChange, len(p.shards))
	copy(chans, p.shards)
	p.mu.Unlock()

	for _, ch := range chans {
		close(ch)
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	timeout := p.secondary.reconcileStallAbort
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	submitted := p.submitted.Load()
	select {
	case <-done:
	case <-time.After(timeout):
		p.setErr(fmt.Errorf("apply phase exceeded %s (%d entries submitted, txn=%v)",
			timeout.Round(time.Second), submitted, p.txnBackend != nil))
		p.cancel()
		return p.Err()
	}
	p.cancel()
	return p.Err()
}

func (p *drPutApplyPipeline) setErr(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	p.err = err
	p.cancel()
}

func (p *drPutApplyPipeline) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func hashEntryShard(change *EntryChange, shards int) int {
	if shards <= 1 {
		return 0
	}
	h := fnv.New32a()
	if change.Key != "" {
		_, _ = h.Write([]byte(change.Key))
	} else if len(change.Kid) > 0 {
		_, _ = h.Write(change.Kid)
	}
	return int(h.Sum32() % uint32(shards))
}

func (s *drReplicationSecondary) runRangeReconciliation(ctx context.Context, checkpoint *CheckpointResponse, localSet *reconciler.ReconciliationSet, startTime time.Time) error {
	ownedSession := false
	if err := s.assertActiveCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		if beginErr := s.beginReconcileSession(checkpoint.CheckpointId, checkpoint.CommitIndex); beginErr != nil {
			return fmt.Errorf("failed to begin reconcile session: %w", beginErr)
		}
		ownedSession = true
	}
	if ownedSession {
		defer s.endReconcileSession()
	}
	s.setReconcilePhase(drReconcilePhaseRangeTasks)
	s.logger.Info("starting range-first reconciliation",
		"checkpoint_id", checkpoint.CheckpointId,
		"commit_index", checkpoint.CommitIndex)

	budget := &drRangeBudget{
		start:       startTime,
		maxRPCBytes: s.reconcileMaxRPCBytes,
		maxWallTime: s.reconcileMaxWallTime,
	}
	localIndex := reconciler.NewRangeMapIndex(localSet.KIDToVID, localSet.Entries)
	queue := make(drRangeTaskQueue, 0, drRangeMaxTotalRanges)
	heap.Init(&queue)
	nextTaskID := 1
	enqueueTask := func(task drRangeTask) {
		priority := 1 // Default priority
		heap.Push(&queue, drQueuedRangeTask{
			id:       nextTaskID,
			priority: priority,
			task:     task,
		})
		nextTaskID++
	}

	// 1. Exchange Dirty Bitmap
	var dirtyRanges []uint64
	rpcCtx, cancel := s.rpcContext(ctx)
	bitmapResp, err := s.client.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId:  s.relationshipID,
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
	})
	cancel()

	useBitmap := false
	if err == nil {
		// Verify if bitmap is usable (covers all missing changes)
		lastApplied := s.lastAppliedIndex.Load()
		if bitmapResp.StartIndex <= lastApplied {
			useBitmap = true
			s.logger.Info("using dirty bitmap optimization", "start_index", bitmapResp.StartIndex, "last_applied", lastApplied)

			// Parse bitmap
			for i := 0; i < len(bitmapResp.Bitmap); i++ {
				b := bitmapResp.Bitmap[i]
				for j := 0; j < 8; j++ {
					if (b & (1 << j)) != 0 {
						rangeID := uint64(i*8 + j)
						if rangeID < uint64(drRangeMaxTotalRanges) {
							dirtyRanges = append(dirtyRanges, rangeID)
						}
					}
				}
			}
		} else {
			s.logger.Warn("dirty bitmap not applicable (gap in tracking)", "start_index", bitmapResp.StartIndex, "last_applied", lastApplied)
		}
	} else {
		s.logger.Warn("failed to exchange dirty bitmap", "error", err)
	}

	if !useBitmap {
		// Fallback: check all ranges
		dirtyRanges = make([]uint64, drRangeMaxTotalRanges)
		for i := 0; i < drRangeMaxTotalRanges; i++ {
			dirtyRanges[i] = uint64(i)
		}
	}

	s.logger.Info("ranges to verify", "count", len(dirtyRanges), "using_bitmap", useBitmap)

	// 2. Exchange Range Checksums (Batched)
	batchSize := 100
	for i := 0; i < len(dirtyRanges); i += batchSize {
		end := i + batchSize
		if end > len(dirtyRanges) {
			end = len(dirtyRanges)
		}
		batchIDs := dirtyRanges[i:end]

		rpcCtx, cancel := s.rpcContext(ctx)
		csumResp, err := s.client.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
			CheckpointId:    checkpoint.CheckpointId,
			CheckpointIndex: checkpoint.CommitIndex,
			RangeIds:        batchIDs,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile failure [rpc_failed]: exchange range checksums: %w", err)
		}
		if err := budget.addRPC(uint64(len(batchIDs) * 24)); err != nil { // Approximate size
			return err
		}

		// Phase A: compare CRC64-XOR coarse checksums.
		//
		// A match (checksum + count equal) treats the range as converged
		// and skips Phase B drill-down. This is a probabilistic decision:
		// false-equality is bounded by ~2^-64 per range (CRC64 component;
		// count must also match). Across 1024 ranges, the probability of
		// any undetected divergence per reconciliation cycle is ~2^-54,
		// which is negligible for the non-Byzantine DR threat model.
		//
		// A mismatch always enters Phase B, where the authoritative
		// RangeDescriptor (256-bit SHA256-XOR pair + count) narrows the
		// diff to the smallest mismatched sub-ranges.
		remoteChecksums := make(map[uint64]*RangeChecksum, len(csumResp.Checksums))
		for _, rc := range csumResp.Checksums {
			remoteChecksums[rc.RangeId] = rc
		}

		for _, rangeID := range batchIDs {
			remoteRC, ok := remoteChecksums[rangeID]
			localSum, localCount := reconciler.ComputeRangeChecksum(localIndex, rangeID)

			mismatch := false
			if !ok {
				// Remote didn't send checksum? Implies empty or error. Assume 0 if empty.
				if localCount != 0 {
					mismatch = true
				}
			} else {
				if remoteRC.Checksum != localSum || remoteRC.Count != localCount {
					mismatch = true
				}
			}

			if mismatch {
				enqueueTask(drRangeTask{
					rangeID: rangeID,
					span:    reconciler.SpanFromRangeID(rangeID),
				})
			}
		}
	}

	metrics.SetGauge([]string{"replication", "dr", "reconcile", "ranges_mismatched"}, float32(queue.Len()))

	if queue.Len() == 0 {
		// All dirty ranges matched at the CRC64-XOR level (Phase A).
		// No Phase B drill-down or Phase C fetch required.
		s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
		s.setLastAppliedIndex(checkpoint.CommitIndex)
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		s.logger.Info("range reconciliation: all ranges converged at Phase A checksum",
			"dirty_ranges_checked", len(dirtyRanges))
		return nil
	}

	s.logger.Info("ranges mismatched", "count", queue.Len())

	// 3. Process Mismatches (FetchEntries)
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
					res := s.processRangeTask(workerCtx, checkpoint, localIndex, queued)
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

	applyPipeline := newDRPutApplyPipeline(ctx, s, localSet.KIDToKey, s.markReconcileActivityNow)
	defer func() {
		_ = applyPipeline.closeAndWait()
	}()
	pendingDeletes := make(map[string]struct{})

	for queue.Len() > 0 || inflight > 0 {
		if stalled, since := s.isReconcileStalled(time.Now().UTC()); stalled {
			workerCancel()
			return fmt.Errorf("reconcile failure [stalled]: stalled without task progress for %s", since.Round(time.Second))
		}

		for inflight < maxWorkers && queue.Len() > 0 {
			if err := budget.check(); err != nil {
				workerCancel()
				return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
			}
			task := heap.Pop(&queue).(drQueuedRangeTask)
			select {
			case workCh <- task:
				inflight++
				s.markReconcileActivityNow()
			case <-workerCtx.Done():
				return workerCtx.Err()
			}
		}

		var res drRangeTaskResult
		select {
		case <-ctx.Done():
			workerCancel()
			return ctx.Err()
		case res = <-resultCh:
			inflight--
			s.markReconcileActivityNow()
		}

		if res.err != nil {
			workerCancel()
			return res.err
		}

		if err := budget.addRPC(res.rpcBytes); err != nil {
			workerCancel()
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}

		budget.rangesHandled++

		if len(res.fetchedEntries) > 0 {
			if err := applyPipeline.submit(res.fetchedEntries); err != nil {
				workerCancel()
				return wrapReconcileFailure(drReconcileFailureApplyFailed, "queue fetched entries for apply", err)
			}
		}
		// Removed keys handling?
		// FetchEntries response (EntryBatch) can contain indications of removal if we compare?
		// Actually, FetchEntries with `ranges` streams ALL entries in range.
		// So simple logic:
		// 1. Get all remote entries for range.
		// 2. Local entries in range that are NOT in remote list are DELETES.
		// 3. Remote entries are PUTS.
		// Wait, `processRangeTask` needs to determine DELETES too!
		// `FetchEntries` only gives me what exists on Primary.
		// I need to difference with Local.
		// I will update `processRangeTask` to do this diff.
		if len(res.removedKeys) > 0 {
			for _, key := range res.removedKeys {
				pendingDeletes[key] = struct{}{}
			}
		}
	}
	workerCancel()
	s.setReconcilePhase(drReconcilePhaseApplyPuts)
	if err := applyPipeline.closeAndWait(); err != nil {
		return wrapReconcileFailure(drReconcileFailureStalled, "apply pipeline", err)
	}
	if len(pendingDeletes) > 0 {
		s.setReconcilePhase(drReconcilePhaseApplyDeletes)
		keys := make([]string, 0, len(pendingDeletes))
		for key := range pendingDeletes {
			keys = append(keys, key)
		}
		deleteCtx, cancel := context.WithTimeout(ctx, drDefaultReconcileStallAbort)
		err := s.applyRemovedKeys(deleteCtx, checkpoint.CheckpointId, checkpoint.CommitIndex, keys)
		cancel()
		if err != nil {
			return wrapReconcileFailure(drReconcileFailureApplyFailed, "apply removed keys", err)
		}
	}
	s.setReconcilePhase(drReconcilePhaseFinalize)

	s.setLastAppliedIndex(checkpoint.CommitIndex)
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("range-first reconciliation complete",
		"ranges_handled", budget.rangesHandled,
		"rpc_bytes", budget.rpcBytes,
		"duration", time.Since(startTime))
	return nil
}

func (s *drReplicationSecondary) processRangeTask(ctx context.Context, checkpoint *CheckpointResponse, localIndex *reconciler.RangeMapIndex, queued drQueuedRangeTask) drRangeTaskResult {
	task := queued.task
	result := drRangeTaskResult{
		id:   queued.id,
		task: task,
	}

	// Attempt fine-grained drill-down to narrow the diff.
	fetchSpans := s.runRangeDrillDown(ctx, checkpoint, localIndex, task.span)
	if fetchSpans == nil {
		// Drill-down failed or unsupported -- fall back to full range.
		fetchSpans = []reconciler.RangeSpan{task.span}
	}

	// Convert narrowed spans to proto and fetch entries.
	protoSpans := make([]*RangeSpan, len(fetchSpans))
	for i, sp := range fetchSpans {
		protoSpans[i] = rangeSpanToProto(sp)
	}

	stream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
		Ranges:          protoSpans,
		IncludeDeletes:  true,
	})
	if err != nil {
		result.err = fmt.Errorf("fetch stream failed: %w", err)
		return result
	}

	var rpcBytes uint64

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			result.err = fmt.Errorf("fetch stream recv error: %w", err)
			return result
		}
		rpcBytes += uint64(len(batch.Entries) * 128) // Estimate
		for _, e := range batch.Entries {
			result.fetchedEntries = append(result.fetchedEntries, cloneEntryChange(e))
		}
	}
	result.rpcBytes = rpcBytes

	return result
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

	// Filter out non-replicable paths.
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		if !isDRNeverReplicatePath(key) {
			filtered = append(filtered, key)
		}
	}
	if len(filtered) == 0 {
		return nil
	}

	// Batch deletes using transactions when supported, mirroring the
	// batch PUT path in applyBatchTxn.
	if txnBackend, ok := s.core.physical.(physical.TransactionalBackend); ok {
		batchSize := s.reconcilePutBatchEntries
		if batchSize <= 0 {
			batchSize = drDefaultReconcilePutBatchEntries
		}
		for i := 0; i < len(filtered); i += batchSize {
			end := i + batchSize
			if end > len(filtered) {
				end = len(filtered)
			}
			batch := filtered[i:end]

			tx, err := txnBackend.BeginTx(ctx)
			if err != nil {
				return fmt.Errorf("begin delete txn: %w", err)
			}
			for _, key := range batch {
				if err := tx.Delete(ctx, key); err != nil {
					_ = tx.Rollback(ctx)
					return fmt.Errorf("batch txn delete %q: %w", key, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit delete txn (%d keys): %w", len(batch), err)
			}
		}
		return nil
	}

	// Fallback: sequential deletes.
	failures := 0
	examples := make([]string, 0, 5)
	for _, key := range filtered {
		if err := s.core.physical.Delete(ctx, key); err != nil {
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

func rangeSpanToProto(span reconciler.RangeSpan) *RangeSpan {
	return &RangeSpan{
		StartKid:   span.StartKID[:],
		EndKid:     span.EndKID[:],
		SplitDepth: span.SplitDepth,
	}
}

// runRangeDrillDown performs fine-grained drill-down on a mismatched
// range by recursively splitting it via ExchangeRangeDigests until the
// diff is narrowed to the smallest mismatched sub-ranges. Returns the
// list of sub-range spans that need fetching. If drill-down fails or
// the RPC is unimplemented, it returns nil to signal the caller should
// fall back to fetching the entire range.
func (s *drReplicationSecondary) runRangeDrillDown(
	ctx context.Context,
	checkpoint *CheckpointResponse,
	localIndex *reconciler.RangeMapIndex,
	parentSpan reconciler.RangeSpan,
) []reconciler.RangeSpan {
	// Work queue of spans to drill into.
	pending := []reconciler.RangeSpan{parentSpan}
	var mismatched []reconciler.RangeSpan
	splits := 0

	for len(pending) > 0 && splits < drRangeMaxSessionSplits {
		span := pending[0]
		pending = pending[1:]

		// Check depth limit.
		if span.SplitDepth >= uint32(drRangeMaxSplitDepth) {
			mismatched = append(mismatched, span)
			continue
		}

		rpcCtx, cancel := s.rpcContext(ctx)
		resp, err := s.client.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
			CheckpointId:    checkpoint.CheckpointId,
			CheckpointIndex: checkpoint.CommitIndex,
			ParentSpan:      rangeSpanToProto(span),
		})
		cancel()

		if err != nil {
			// If the primary doesn't support drill-down (old version)
			// or any RPC failure, fall back to full-range fetch.
			s.logger.Warn("drill-down RPC failed, falling back to full range fetch",
				"error", err, "split_depth", span.SplitDepth)
			return nil
		}

		for _, digest := range resp.Digests {
			remoteSpan, _, err := protoToRangeSpan(digest.Span)
			if err != nil {
				s.logger.Warn("invalid drill-down span from primary", "error", err)
				return nil
			}

			// Compute local RangeDescriptor for this sub-range.
			localDesc := reconciler.BuildRangeDigestFromIndex(localIndex, remoteSpan)

			// Phase B authoritative equality: compare (count, XORKeyHash,
			// XORValueHash). These are 256-bit SHA256-XOR accumulators,
			// making false-equality cryptographically negligible (~2^-256).
			if localDesc.Count == digest.Count &&
				localDesc.XORKeyHash == bytesToHash32(digest.XorKeyHash) &&
				localDesc.XORValueHash == bytesToHash32(digest.XorValueHash) {
				// Sub-range matches at RangeDescriptor level -- skip.
				continue
			}

			splits++
			// Sub-range mismatched -- drill deeper or mark for fetch.
			if remoteSpan.SplitDepth < uint32(drRangeMaxSplitDepth) && digest.Count > 1 {
				pending = append(pending, remoteSpan)
			} else {
				mismatched = append(mismatched, remoteSpan)
			}
		}
	}

	// Drain any remaining pending spans as mismatched (hit split limit).
	mismatched = append(mismatched, pending...)

	if len(mismatched) == 0 {
		return mismatched // Empty -- ranges converged during drill-down.
	}

	s.logger.Info("drill-down complete",
		"original_span_depth", parentSpan.SplitDepth,
		"mismatched_sub_ranges", len(mismatched),
		"total_splits", splits)
	return mismatched
}

// protoToRangeSpan converts a proto RangeSpan to a reconciler.RangeSpan.
func protoToRangeSpan(span *RangeSpan) (reconciler.RangeSpan, bool, error) {
	if span == nil {
		return reconciler.RangeSpan{}, false, fmt.Errorf("nil span")
	}
	if len(span.StartKid) != 32 || len(span.EndKid) != 32 {
		return reconciler.RangeSpan{}, false, fmt.Errorf("invalid span KID length")
	}
	var s reconciler.RangeSpan
	copy(s.StartKID[:], span.StartKid)
	copy(s.EndKID[:], span.EndKid)
	s.SplitDepth = span.SplitDepth
	return s, true, nil
}

// bytesToHash32 converts a byte slice to a [32]byte array.
func bytesToHash32(b []byte) [32]byte {
	var h [32]byte
	if len(b) == 32 {
		copy(h[:], b)
	}
	return h
}
