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
	"crypto/tls"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for DR streams
	"google.golang.org/grpc/status"

	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/vault/routing"
)

// errDRRedirect is returned when a DR gRPC call is redirected to the
// active leader. The controller uses LeaderAddr to reconnect.
type errDRRedirect struct {
	LeaderAddr string
}

func (e *errDRRedirect) Error() string {
	return fmt.Sprintf("DR redirect to leader at %s", e.LeaderAddr)
}

var (
	errDRSecondaryApplyStopped            = errors.New("DR secondary stream apply stopped")
	errDRIndexedBucketRepairProofMismatch = errors.New("flat accumulator indexed-bucket repair proof mismatch")
)

type drIndexedBucketRepairProofMismatchError struct {
	rangeID          uint64
	localCount       uint64
	remoteCount      uint64
	checksumMismatch bool
}

func (e *drIndexedBucketRepairProofMismatchError) Error() string {
	return fmt.Sprintf("%s: range=%d local_count=%d remote_count=%d checksum_mismatch=%t",
		errDRIndexedBucketRepairProofMismatch,
		e.rangeID,
		e.localCount,
		e.remoteCount,
		e.checksumMismatch)
}

func (e *drIndexedBucketRepairProofMismatchError) Unwrap() error {
	return errDRIndexedBucketRepairProofMismatch
}

func atomicMaxUint64(target *atomic.Uint64, value uint64) {
	for {
		current := target.Load()
		if value <= current {
			return
		}
		if target.CompareAndSwap(current, value) {
			return
		}
	}
}

func durationNanos(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d)
}

func nanosToMilliseconds(nanos uint64) float64 {
	return float64(nanos) / float64(time.Millisecond)
}

func averageUint64(total, count uint64) float64 {
	if count == 0 {
		return 0
	}
	return float64(total) / float64(count)
}

func averageNanosMilliseconds(totalNanos, count uint64) float64 {
	if count == 0 {
		return 0
	}
	return nanosToMilliseconds(totalNanos) / float64(count)
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
	normalized := strings.Trim(path, "/")
	if normalized == "" {
		return false
	}

	candidates := []string{normalized}
	if keySuffix, ok := strings.CutPrefix(normalized, namespaceBarrierPrefix); ok {
		if namespaceUUID, namespacedKey, found := strings.Cut(keySuffix, "/"); found && namespaceUUID != "" && namespacedKey != "" {
			candidates = append(candidates, namespacedKey)
		}
	}

	for _, candidate := range candidates {
		if exact[candidate] {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(candidate, prefix) {
				return true
			}
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

// isDRTransportReconnectError returns true for transport-level RPC failures
// where the secondary should hand control back to the controller loop to
// reconnect, rather than retrying reconciliation in-place.
func isDRTransportReconnectError(err error) bool {
	if err == nil {
		return false
	}

	// If this is already a structured redirect, let redirect handling take over.
	if _, ok := extractDRRedirect(err); ok {
		return false
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unsupported protocol") ||
		strings.Contains(msg, "authentication handshake failed") {
		return true
	}

	return strings.Contains(msg, "code = unavailable") &&
		(strings.Contains(msg, "connection error") ||
			strings.Contains(msg, "error while dialing") ||
			strings.Contains(msg, "tls: internal error") ||
			strings.Contains(msg, "unsupported protocol") ||
			strings.Contains(msg, "authentication handshake failed"))
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

	drDefaultReconcileMaxRPCBytes               = 128 << 20
	drDefaultReconcileMaxWallTime               = 30 * time.Minute
	drDefaultReconcileMaxInflightTasks          = 16
	drDefaultReconcileStallAbort                = 90 * time.Second
	drDefaultRPCDeadline                        = 30 * time.Second
	drDefaultStreamBatchMaxEntries              = 256
	drDefaultStreamBatchMaxBytes                = 1 << 20 // 1 MiB
	drDefaultStreamBatchMaxWait                 = 10 * time.Millisecond
	drDefaultFlatAccumulatorSnapshotMinEntries  = 4096
	drDefaultFlatAccumulatorSnapshotMinInterval = 5 * time.Second
	drDefaultReconcileApplyWorkers              = 16
	drDefaultReconcilePutBatchEntries           = 512
	drDefaultReconcilePutBatchBytes             = 2 << 20 // 2 MiB
	drDefaultReconcileTxnCommitRetries          = 8
	drDefaultReconcileTxnCommitRetryDelay       = 10 * time.Millisecond
	drDefaultReconcileOptimizerPersistTimeout   = 30 * time.Second
	drDefaultConvergenceMinRateRatio            = 0.8
	drDefaultConvergenceStall                   = 180 * time.Second

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

	// rangeAccumulator maintains a secondary-local flat projection cache for
	// fast reconciliation when it is warm and index-aligned.
	rangeAccumulator *drFlatRangeAccumulator

	// client is the gRPC client connection to the primary.
	client DRReplicationClient

	// conn is the underlying gRPC connection (for cleanup).
	conn *grpc.ClientConn

	streamMu     sync.Mutex
	streamCancel context.CancelFunc

	runtimeStateReloadMu       sync.Mutex
	runtimeStateRefreshPending atomic.Bool

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

	clientCertMu sync.RWMutex
	clientCert   *tls.Certificate

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
	entriesApplied                                    atomic.Uint64
	reconcileCount                                    atomic.Uint64
	lastReconcileAt                                   atomic.Int64 // unix timestamp
	streamDisconnects                                 atomic.Uint64
	connectRetries                                    atomic.Uint64
	connectFailures                                   atomic.Uint64
	reconcileRangesInflight                           atomic.Int64
	reconcileRangesFailed                             atomic.Int64
	reconcileBudgetRemainingByte                      atomic.Int64
	flatAccumulatorFastPathTotal                      atomic.Uint64
	flatAccumulatorEmptyRepairTotal                   atomic.Uint64
	flatAccumulatorEmptyRepairRanges                  atomic.Uint64
	flatAccumulatorIndexedRepairTotal                 atomic.Uint64
	flatAccumulatorIndexedRepairRanges                atomic.Uint64
	flatAccumulatorIndexedRepairProofMismatches       atomic.Uint64
	flatAccumulatorIndexedRepairProofMismatchRange    atomic.Uint64
	flatAccumulatorIndexedRepairProofMismatchLocal    atomic.Uint64
	flatAccumulatorIndexedRepairProofMismatchRemote   atomic.Uint64
	flatAccumulatorIndexedRepairProofMismatchChecksum atomic.Bool
	localKIDIndexBucketLoads                          atomic.Uint64
	localKIDIndexEntriesLoaded                        atomic.Uint64
	localKIDIndexLoadFailures                         atomic.Uint64
	localKIDIndexResets                               atomic.Uint64
	localKIDIndexUpdates                              atomic.Uint64
	localKIDIndexFallbackScans                        atomic.Uint64
	localKIDIndexInvalidations                        atomic.Uint64
	streamTxnCoalescedEntries                         atomic.Uint64
	streamTxnBatches                                  atomic.Uint64
	streamTxnEntries                                  atomic.Uint64
	streamTxnPhysicalEntries                          atomic.Uint64
	streamTxnMaxEntries                               atomic.Uint64
	streamTxnMaxPhysicalEntries                       atomic.Uint64
	streamTxnApplyNanos                               atomic.Uint64
	streamTxnApplyMaxNanos                            atomic.Uint64
	streamTxnCommitNanos                              atomic.Uint64
	streamTxnCommitMaxNanos                           atomic.Uint64
	streamBatchFlushMaxEntries                        atomic.Uint64
	streamBatchFlushMaxBytes                          atomic.Uint64
	streamBatchFlushMaxWait                           atomic.Uint64
	streamBatchFlushShutdown                          atomic.Uint64
	flatAccumulatorCursorWrites                       atomic.Uint64
	flatAccumulatorCursorIndex                        atomic.Uint64
	flatAccumulatorSnapshotCount                      atomic.Uint64
	flatAccumulatorSnapshotBytes                      atomic.Uint64
	flatAccumulatorSnapshotLast                       atomic.Uint64
	flatAccumulatorSnapshotNanos                      atomic.Uint64
	flatAccumulatorSnapshotMaxNs                      atomic.Uint64
	flatAccumulatorSnapshotIndex                      atomic.Uint64
	flatAccumulatorSnapshotLastAt                     atomic.Int64
	flatAccumulatorSnapshotSkipped                    atomic.Uint64
	flatAccumulatorDeltaBatches                       atomic.Uint64
	flatAccumulatorDeltaEntries                       atomic.Uint64
	flatAccumulatorDeltaOldestIndex                   atomic.Uint64
	flatAccumulatorDeltaNewestIndex                   atomic.Uint64
	flatAccumulatorDeltaReplayCount                   atomic.Uint64
	flatAccumulatorDeltaReplayBatches                 atomic.Uint64
	flatAccumulatorDeltaReplayEntries                 atomic.Uint64
	flatAccumulatorDeltaReplayFailures                atomic.Uint64
	rangeSplitCount                                   atomic.Uint64
	reconcileRPCBytesUsed                             atomic.Uint64
	reconcileQueueDepth                               atomic.Int64
	reconcileStalled                                  atomic.Uint64
	lastReconcileActivityAt                           atomic.Int64 // unix timestamp
	lastReconcileApplyAt                              atomic.Int64 // unix timestamp

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
	checkpointHighWaterMarkPersistHook func(uint64) error

	optimizerStatusMu                   sync.RWMutex
	lastLocalKIDIndexFallbackScanReason string
	lastLocalKIDIndexInvalidationReason string

	// Additional heavy-load observability counters.
	scanFailures            atomic.Uint64
	checkpointConflicts     atomic.Uint64
	reconcileRetries        atomic.Uint64
	reconcileTaskRetries    atomic.Uint64
	reconcileDecodeFailures atomic.Uint64
	reconcilePutWorkers     atomic.Int64
	reconcileDeletePhaseMS  atomic.Uint64

	// Runtime tunables.
	reconcileMaxRPCBytes               uint64
	reconcileMaxWallTime               time.Duration
	reconcileMaxInflightTasks          int
	reconcileStallAbort                time.Duration
	rpcDeadline                        time.Duration
	streamBatchMaxEntries              int
	streamBatchMaxBytes                int
	streamBatchMaxWait                 time.Duration
	flatAccumulatorSnapshotMinEntries  uint64
	flatAccumulatorSnapshotMinInterval time.Duration
	reconcileApplyWorkers              int
	reconcilePutBatchEntries           int
	reconcilePutBatchBytes             int

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
		logger:                             logger.Named("dr-secondary"),
		core:                               core,
		scanner:                            reconciler.NewScanner(config),
		rangeAccumulator:                   newDRFlatRangeAccumulator(),
		relationshipID:                     relationshipID,
		replSalt:                           replSalt,
		stopCh:                             make(chan struct{}),
		lastReconcileFailureByType:         make(map[string]uint64),
		reconcileMaxRPCBytes:               drDefaultReconcileMaxRPCBytes,
		reconcileMaxWallTime:               drDefaultReconcileMaxWallTime,
		reconcileMaxInflightTasks:          drDefaultReconcileMaxInflightTasks,
		reconcileStallAbort:                drDefaultReconcileStallAbort,
		rpcDeadline:                        drDefaultRPCDeadline,
		streamBatchMaxEntries:              drDefaultStreamBatchMaxEntries,
		streamBatchMaxBytes:                drDefaultStreamBatchMaxBytes,
		streamBatchMaxWait:                 drDefaultStreamBatchMaxWait,
		flatAccumulatorSnapshotMinEntries:  drDefaultFlatAccumulatorSnapshotMinEntries,
		flatAccumulatorSnapshotMinInterval: drDefaultFlatAccumulatorSnapshotMinInterval,
		reconcileApplyWorkers:              drDefaultReconcileApplyWorkers,
		reconcilePutBatchEntries:           drDefaultReconcilePutBatchEntries,
		reconcilePutBatchBytes:             drDefaultReconcilePutBatchBytes,
		convergenceMinRateRatio:            drDefaultConvergenceMinRateRatio,
		convergenceStall:                   drDefaultConvergenceStall,
		fallbackEnabled:                    drDefaultFallbackEnabled,
		fallbackStall:                      drDefaultFallbackStall,
		fallbackFailureThreshold:           drDefaultFallbackFailureThreshold,
		fallbackCooldown:                   drDefaultFallbackCooldown,
		fallbackMaxPerHour:                 drDefaultFallbackMaxPerHour,
		fallbackWindow:                     drDefaultFallbackWindow,
	}
	now := time.Now().UTC().Unix()
	sec.lastAppliedAt.Store(now)
	sec.lastReconcileActivityAt.Store(now)
	sec.lastReconcileApplyAt.Store(now)
	return sec
}

func (s *drReplicationSecondary) setClientCertificate(certDER, keyPEM []byte) error {
	cert, err := parseDRSecondaryClientCert(certDER, keyPEM)
	if err != nil {
		return err
	}
	s.clientCertMu.Lock()
	s.clientCert = cert
	s.clientCertMu.Unlock()
	return nil
}

func (s *drReplicationSecondary) clientCertificate() *tls.Certificate {
	s.clientCertMu.RLock()
	defer s.clientCertMu.RUnlock()
	if s.clientCert == nil {
		return nil
	}
	cert := *s.clientCert
	return &cert
}

func (s *drReplicationSecondary) ReconnectWithClientCertificate(certDER, keyPEM []byte) error {
	if err := s.setClientCertificate(certDER, keyPEM); err != nil {
		return err
	}
	s.cancelActiveStream()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if cl := s.core.getClusterListener(); cl != nil {
		cl.RemoveClient(consts.DRReplicationALPN)
	}
	return nil
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
	if s.rangeAccumulator != nil && !s.rangeAccumulator.isInitialized() {
		s.loadPersistentFlatAccumulator(ctx)
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
		clientCert:     s.clientCertificate(),
	}
	s.drClusterClient = client

	// Ensure client registration is idempotent across reconnects.
	cl.RemoveClient(consts.DRReplicationALPN)
	cl.AddClient(consts.DRReplicationALPN, client)

	dialerFunc := cl.GetContextDialerFunc(ctx, consts.DRReplicationALPN)
	opts = append(
		opts,
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
	s.prepareStateForConnect()

	s.logger.Info("connected to primary", "addr", primaryAddr)
	return nil
}

func (s *drReplicationSecondary) prepareStateForConnect() {
	switch s.State() {
	case DRSecondaryIdle:
		if s.keyringBootstrapped.Load() && s.lastAppliedIndex.Load() > 0 {
			s.setState(DRSecondaryStreaming)
			return
		}
		s.setState(DRSecondaryBootstrapping)
	case DRSecondaryBootstrapping, DRSecondaryInitialSync:
		s.setState(DRSecondaryBootstrapping)
	}
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

				// Transport-level errors should be handled by the
				// controller reconnect loop, not by local resume/reconcile
				// retries inside this Start() invocation.
				if isDRTransportReconnectError(err) {
					return fmt.Errorf("stream transport failure, reconnecting: %w", err)
				}

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

				// Transport-level errors should immediately return to the
				// controller reconnect loop; retrying reconciliation in
				// place can cause a tight reconciling loop after stepdown.
				if isDRTransportReconnectError(err) {
					return fmt.Errorf("reconciliation transport failure, reconnecting: %w", err)
				}

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

				if isDRTransportReconnectError(err) {
					return fmt.Errorf("resnapshot transport failure, reconnecting: %w", err)
				}

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

func (s *drReplicationSecondary) recordLocalKIDIndexFallbackScan(reason string) {
	if s == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	s.localKIDIndexFallbackScans.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "local_kid_index_fallback_scans_total"}, 1)
	s.optimizerStatusMu.Lock()
	s.lastLocalKIDIndexFallbackScanReason = reason
	s.optimizerStatusMu.Unlock()
}

func (s *drReplicationSecondary) recordLocalKIDIndexInvalidation(reason string) {
	if s == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	s.localKIDIndexInvalidations.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "local_kid_index_invalidations_total"}, 1)
	s.optimizerStatusMu.Lock()
	s.lastLocalKIDIndexInvalidationReason = reason
	s.optimizerStatusMu.Unlock()
}

func (s *drReplicationSecondary) localKIDIndexOptimizerStatus() (fallbackReason, invalidationReason string) {
	if s == nil {
		return "", ""
	}
	s.optimizerStatusMu.RLock()
	defer s.optimizerStatusMu.RUnlock()
	return s.lastLocalKIDIndexFallbackScanReason, s.lastLocalKIDIndexInvalidationReason
}

func (s *drReplicationSecondary) recordIndexedRepairProofMismatch(err error) {
	if s == nil {
		return
	}
	s.flatAccumulatorIndexedRepairProofMismatches.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_indexed_repair_proof_mismatches_total"}, 1)
	var mismatch *drIndexedBucketRepairProofMismatchError
	if !errors.As(err, &mismatch) || mismatch == nil {
		return
	}
	s.flatAccumulatorIndexedRepairProofMismatchRange.Store(mismatch.rangeID)
	s.flatAccumulatorIndexedRepairProofMismatchLocal.Store(mismatch.localCount)
	s.flatAccumulatorIndexedRepairProofMismatchRemote.Store(mismatch.remoteCount)
	s.flatAccumulatorIndexedRepairProofMismatchChecksum.Store(mismatch.checksumMismatch)
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
	if s.core.GetRaftBackend() != nil {
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

// reloadCoreState reloads the critical in-memory subsystems from storage.
// The storage now contains the primary's replicated mount/auth tables, but
// the live router may still reflect an older secondary view. Avoid a full
// router reset because singleton mounts such as sys/, cubbyhole/, and token/
// are process-local runtime state. Identity is the exception: its storage is
// replicated DR state, so the route must be reconciled to the primary's mount
// UUID/accessor.
func (s *drReplicationSecondary) reloadCoreState(ctx context.Context) error {
	// Purge physical cache first to ensure all reads go to storage.
	if s.core.physicalCache != nil {
		s.core.physicalCache.Purge(ctx)
	}

	// Invalidate the token store salt so it reloads from primary data.
	if s.core.tokenStore != nil {
		s.core.tokenStore.Invalidate(ctx, "token/salt")
	}

	oldMounts := cloneDRMountTable(s.core.mounts)
	oldAuth := cloneDRMountTable(s.core.auth)

	// Namespaces are replicated DR runtime state. Reload them before mount/auth
	// tables so namespace-scoped mount entries can resolve their namespace.
	s.logger.Info("reloading namespace store from storage")
	if err := s.core.setupNamespaceStore(ctx); err != nil {
		return fmt.Errorf("failed to reload namespaces: %w", err)
	}

	// Load the primary's mount table from storage. This replaces the
	// in-memory mount table but doesn't touch the router or backends.
	s.logger.Info("reloading mount table from storage")
	if err := s.core.loadMounts(ctx, false); err != nil {
		return fmt.Errorf("failed to reload mounts: %w", err)
	}
	if err := s.unmountStaleRouterEntries(ctx, oldMounts, s.core.mounts, false); err != nil {
		return fmt.Errorf("failed to unmount stale mounts: %w", err)
	}

	// Mount any new entries from the primary that aren't already
	// registered in the router (e.g., the KV engine the primary had).
	s.logger.Info("mounting new entries from primary")
	if err := s.mountNewEntries(ctx); err != nil {
		return fmt.Errorf("failed to mount new entries: %w", err)
	}

	// Reload auth backends from storage.
	s.logger.Info("reloading auth backends from storage")
	if err := s.core.loadCredentials(ctx, false); err != nil {
		return fmt.Errorf("failed to reload credentials: %w", err)
	}
	if err := s.unmountStaleRouterEntries(ctx, oldAuth, s.core.auth, true); err != nil {
		return fmt.Errorf("failed to unmount stale auth backends: %w", err)
	}
	if err := s.mountNewCredentialEntries(ctx); err != nil {
		return fmt.Errorf("failed to mount new auth backends: %w", err)
	}

	if err := s.reloadIdentityStoreArtifacts(ctx); err != nil {
		return err
	}

	s.logger.Info("core state reloaded from storage")
	return nil
}

func (s *drReplicationSecondary) reloadIdentityStoreArtifacts(ctx context.Context) error {
	if s.core.identityStore == nil {
		return nil
	}

	// Identity artifacts are stored in replicated storage but cached in the
	// identity store's per-namespace memdb. A reseeded secondary can otherwise
	// have the correct identity route and durable entries while serving stale or
	// empty in-memory identity state.
	s.logger.Info("reloading identity store artifacts from storage")
	namespaces, err := s.core.ListNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("failed to list namespaces for identity reload: %w", err)
	}
	for _, ns := range namespaces {
		if ns.ID == namespace.RootNamespaceID || s.core.identityStore.HasNamespaceView(ns) {
			continue
		}
		if err := s.core.identityStore.AddNamespaceView(s.core, ns, s.core.NamespaceView(ns)); err != nil {
			return fmt.Errorf("failed to register namespace %q to identity store: %w", ns.Path, err)
		}
	}
	if err := s.core.identityStore.ResetDB(ctx); err != nil {
		return fmt.Errorf("failed to reset identity store artifacts: %w", err)
	}
	if err := s.core.loadIdentityStoreArtifacts(ctx, true); err != nil {
		return fmt.Errorf("failed to reload identity store artifacts: %w", err)
	}

	return nil
}

func (s *drReplicationSecondary) refreshRuntimeStateAfterApply(ctx context.Context, source string, strict bool) error {
	s.runtimeStateReloadMu.Lock()
	defer s.runtimeStateReloadMu.Unlock()

	s.logger.Info("replicated runtime state changed; refreshing core state", "source", source)
	if err := s.reloadCoreState(ctx); err != nil {
		s.runtimeStateRefreshPending.Store(true)
		err = fmt.Errorf("failed to refresh replicated runtime state after %s: %w", source, err)
		if strict {
			return err
		}
		s.logger.Warn("replicated runtime state refresh deferred", "source", source, "error", err)
		return nil
	}
	s.runtimeStateRefreshPending.Store(false)
	return nil
}

func isDRRuntimeStatePath(path string) bool {
	normalized := strings.Trim(path, "/")
	if normalized == "" {
		return false
	}

	candidates := []string{normalized}
	if keySuffix, ok := strings.CutPrefix(normalized, namespaceBarrierPrefix); ok {
		if namespaceUUID, namespacedKey, found := strings.Cut(keySuffix, "/"); found && namespaceUUID != "" && namespacedKey != "" {
			candidates = append(candidates, namespacedKey)
		}
	}

	for _, candidate := range candidates {
		switch {
		case candidate == namespaceStoreSubPath || strings.HasPrefix(candidate, namespaceStoreSubPath):
			return true
		case candidate == coreMountConfigPath || strings.HasPrefix(candidate, coreMountConfigPath+"/"):
			return true
		case candidate == coreAuthConfigPath || strings.HasPrefix(candidate, coreAuthConfigPath+"/"):
			return true
		case candidate == coreAuditConfigPath || strings.HasPrefix(candidate, coreAuditConfigPath+"/"):
			return true
		}
	}
	return false
}

func cloneDRMountTable(table *routing.MountTable) *routing.MountTable {
	if table == nil {
		return nil
	}
	return table.ShallowClone()
}

func drMountEntryContext(ctx context.Context, entry *routing.MountEntry) context.Context {
	if entry != nil && entry.Namespace != nil {
		return namespace.ContextWithNamespace(ctx, entry.Namespace)
	}
	return namespace.RootContext(ctx)
}

func isDRRuntimeRouterProtectedEntry(entry *routing.MountEntry) bool {
	if entry == nil || entry.Local {
		return true
	}
	if isSingletonMountType(entry.Type) {
		return true
	}
	for _, protected := range protectedMounts {
		if strings.HasPrefix(entry.Path, protected) {
			return true
		}
	}
	return false
}

func isDRRuntimeRefreshProtectedEntry(entry *routing.MountEntry) bool {
	if entry == nil {
		return true
	}
	if entry.Type == routing.MountTypeIdentity || entry.Type == routing.MountTypeNSIdentity {
		return false
	}
	if entry.NamespaceID != "" && entry.NamespaceID != namespace.RootNamespaceID {
		return entry.Local
	}
	if entry.Namespace != nil && entry.Namespace.ID != namespace.RootNamespaceID {
		return entry.Local
	}
	return isDRRuntimeRouterProtectedEntry(entry)
}

func isSingletonMountType(mountType string) bool {
	for _, singleton := range singletonMounts {
		if mountType == singleton {
			return true
		}
	}
	return false
}

func shouldSkipDRSecondaryProtectedRouterEntry(ctx context.Context, core *Core, entry *routing.MountEntry, credential bool) bool {
	if core == nil || core.drManager == nil || core.drManager.Mode() != DRModeSecondary {
		return false
	}
	if !isDRRuntimeRouterProtectedEntry(entry) {
		return false
	}

	path := entry.Path
	if credential {
		path = routing.CredentialRoutePrefix + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return core.router.MatchingMount(drMountEntryContext(ctx, entry), path) != ""
}

func drMountAccessors(table *routing.MountTable) map[string]struct{} {
	accessors := make(map[string]struct{})
	if table == nil {
		return accessors
	}
	for _, entry := range table.Entries {
		if entry == nil || entry.Accessor == "" {
			continue
		}
		accessors[entry.Accessor] = struct{}{}
	}
	return accessors
}

func (s *drReplicationSecondary) unmountStaleRouterEntries(ctx context.Context, oldTable, newTable *routing.MountTable, credential bool) error {
	if oldTable == nil {
		return nil
	}
	current := drMountAccessors(newTable)
	for _, entry := range oldTable.Entries {
		if isDRRuntimeRefreshProtectedEntry(entry) {
			continue
		}
		if _, ok := current[entry.Accessor]; ok {
			continue
		}

		nsCtx := drMountEntryContext(ctx, entry)
		path := entry.Path
		if credential {
			path = routing.CredentialRoutePrefix + path
		}
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		if s.core.router.MatchingMount(nsCtx, path) == "" {
			continue
		}

		s.logger.Info("unmounting stale replicated router entry", "path", path, "type", entry.Type, "credential", credential)
		if err := s.core.router.Unmount(nsCtx, path); err != nil {
			return err
		}
	}
	return nil
}

// mountNewEntries iterates the loaded mount table and initializes
// backends for any mount entries not already present in the router.
// Protected local singleton routes are left untouched. Identity is remounted
// when the router still points at the secondary-local identity entry because
// identity storage is replicated DR state.
func (s *drReplicationSecondary) mountNewEntries(ctx context.Context) error {
	if s.core.mounts == nil {
		return nil
	}

	for _, entry := range s.core.mounts.Entries {
		if isDRRuntimeRefreshProtectedEntry(entry) {
			continue
		}
		nsCtx := drMountEntryContext(ctx, entry)
		if existing := s.core.router.MatchingMountEntry(nsCtx, entry.Path); existing != nil {
			if !shouldRemountDRRuntimeEntry(existing, entry) {
				continue
			}
			path := entry.Path
			if !strings.HasSuffix(path, "/") {
				path += "/"
			}
			s.logger.Info("remounting replicated router entry", "path", path, "type", entry.Type, "old_uuid", existing.UUID, "new_uuid", entry.UUID)
			if err := s.core.router.Unmount(nsCtx, path); err != nil {
				return err
			}
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
		if backend != nil {
			s.core.setCoreBackend(entry, backend, view)
		}

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

func shouldRemountDRRuntimeEntry(existing, desired *routing.MountEntry) bool {
	if existing == nil || desired == nil {
		return false
	}
	if desired.Type != routing.MountTypeIdentity && desired.Type != routing.MountTypeNSIdentity {
		return false
	}
	return existing.UUID != desired.UUID ||
		existing.Accessor != desired.Accessor ||
		existing.BackendAwareUUID != desired.BackendAwareUUID
}

// mountNewCredentialEntries initializes auth backends from the loaded auth
// table when their router entries are not present yet.
func (s *drReplicationSecondary) mountNewCredentialEntries(ctx context.Context) error {
	if s.core.auth == nil {
		return nil
	}

	var postUnsealFuncs []func()

	s.core.authLock.Lock()
	for _, entry := range s.core.auth.SortEntriesByPathDepth().Entries {
		if isDRRuntimeRouterProtectedEntry(entry) {
			continue
		}

		nsCtx := drMountEntryContext(ctx, entry)
		path := routing.CredentialRoutePrefix + entry.Path
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		if s.core.router.MatchingMount(nsCtx, path) != "" {
			continue
		}

		s.logger.Info("mounting new auth entry from primary", "path", entry.Path, "type", entry.Type)
		postUnsealFunc, err := s.core.setupCredential(nsCtx, entry)
		if err != nil {
			s.core.authLock.Unlock()
			return err
		}
		if postUnsealFunc != nil {
			postUnsealFuncs = append(postUnsealFuncs, postUnsealFunc)
		}
	}
	s.core.authLock.Unlock()

	if len(postUnsealFuncs) > 0 {
		s.core.runPostUnsealFuncs(postUnsealFuncs)
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
	streamTxnBatches := s.streamTxnBatches.Load()
	streamTxnEntries := s.streamTxnEntries.Load()
	streamTxnPhysicalEntries := s.streamTxnPhysicalEntries.Load()
	streamTxnApplyNanos := s.streamTxnApplyNanos.Load()
	streamTxnCommitNanos := s.streamTxnCommitNanos.Load()
	flatAccumulatorSnapshotCount := s.flatAccumulatorSnapshotCount.Load()
	flatAccumulatorSnapshotBytes := s.flatAccumulatorSnapshotBytes.Load()
	flatAccumulatorSnapshotNanos := s.flatAccumulatorSnapshotNanos.Load()
	localKIDIndexFallbackReason, localKIDIndexInvalidationReason := s.localKIDIndexOptimizerStatus()
	return DRSecondaryStatus{
		State:                                             s.State().String(),
		RelationshipID:                                    s.relationshipID,
		PrimaryIndex:                                      s.primaryIndex.Load(),
		LastAppliedIndex:                                  s.lastAppliedIndex.Load(),
		EntriesApplied:                                    s.entriesApplied.Load(),
		ReconcileCount:                                    s.reconcileCount.Load(),
		LastReconcileAt:                                   time.Unix(s.lastReconcileAt.Load(), 0),
		ConnectRetries:                                    s.connectRetries.Load(),
		ConnectFailures:                                   s.connectFailures.Load(),
		ReconcileRangesInflight:                           s.reconcileRangesInflight.Load(),
		ReconcileRangesFailed:                             s.reconcileRangesFailed.Load(),
		ReconcileBudgetRemainingBytes:                     uint64(remaining),
		ReconcileActiveCheckpointID:                       activeID,
		ReconcileActiveCheckpointIndex:                    activeIndex,
		ReconcileFailReasonLast:                           failReason,
		RangeManifestCount:                                rangeManifestCount,
		RangeSplitCount:                                   s.rangeSplitCount.Load(),
		ReconcileRPCBytesUsed:                             s.reconcileRPCBytesUsed.Load(),
		FlatAccumulatorFastPathTotal:                      s.flatAccumulatorFastPathTotal.Load(),
		FlatAccumulatorEmptyRepairTotal:                   s.flatAccumulatorEmptyRepairTotal.Load(),
		FlatAccumulatorEmptyRepairRanges:                  s.flatAccumulatorEmptyRepairRanges.Load(),
		FlatAccumulatorIndexedRepairTotal:                 s.flatAccumulatorIndexedRepairTotal.Load(),
		FlatAccumulatorIndexedRepairRanges:                s.flatAccumulatorIndexedRepairRanges.Load(),
		FlatAccumulatorIndexedRepairProofMismatches:       s.flatAccumulatorIndexedRepairProofMismatches.Load(),
		FlatAccumulatorIndexedRepairProofMismatchRange:    s.flatAccumulatorIndexedRepairProofMismatchRange.Load(),
		FlatAccumulatorIndexedRepairProofMismatchLocal:    s.flatAccumulatorIndexedRepairProofMismatchLocal.Load(),
		FlatAccumulatorIndexedRepairProofMismatchRemote:   s.flatAccumulatorIndexedRepairProofMismatchRemote.Load(),
		FlatAccumulatorIndexedRepairProofMismatchChecksum: s.flatAccumulatorIndexedRepairProofMismatchChecksum.Load(),
		LocalKIDIndexBucketLoadsTotal:                     s.localKIDIndexBucketLoads.Load(),
		LocalKIDIndexEntriesLoadedTotal:                   s.localKIDIndexEntriesLoaded.Load(),
		LocalKIDIndexLoadFailuresTotal:                    s.localKIDIndexLoadFailures.Load(),
		LocalKIDIndexResetsTotal:                          s.localKIDIndexResets.Load(),
		LocalKIDIndexUpdatesTotal:                         s.localKIDIndexUpdates.Load(),
		LocalKIDIndexFallbackScansTotal:                   s.localKIDIndexFallbackScans.Load(),
		LocalKIDIndexFallbackScanReasonLast:               localKIDIndexFallbackReason,
		LocalKIDIndexInvalidationsTotal:                   s.localKIDIndexInvalidations.Load(),
		LocalKIDIndexInvalidationReasonLast:               localKIDIndexInvalidationReason,
		StreamTxnCoalescedEntriesTotal:                    s.streamTxnCoalescedEntries.Load(),
		StreamTxnBatchesTotal:                             streamTxnBatches,
		StreamTxnEntriesTotal:                             streamTxnEntries,
		StreamTxnPhysicalEntriesTotal:                     streamTxnPhysicalEntries,
		StreamTxnAverageEntries:                           averageUint64(streamTxnEntries, streamTxnBatches),
		StreamTxnMaxEntries:                               s.streamTxnMaxEntries.Load(),
		StreamTxnAveragePhysicalEntries: averageUint64(
			streamTxnPhysicalEntries,
			streamTxnBatches,
		),
		StreamTxnMaxPhysicalEntries:           s.streamTxnMaxPhysicalEntries.Load(),
		StreamTxnApplyMillisecondsTotal:       nanosToMilliseconds(streamTxnApplyNanos),
		StreamTxnApplyMillisecondsAverage:     averageNanosMilliseconds(streamTxnApplyNanos, streamTxnBatches),
		StreamTxnApplyMillisecondsMax:         nanosToMilliseconds(s.streamTxnApplyMaxNanos.Load()),
		StreamTxnCommitMillisecondsTotal:      nanosToMilliseconds(streamTxnCommitNanos),
		StreamTxnCommitMillisecondsAverage:    averageNanosMilliseconds(streamTxnCommitNanos, streamTxnBatches),
		StreamTxnCommitMillisecondsMax:        nanosToMilliseconds(s.streamTxnCommitMaxNanos.Load()),
		StreamBatchFlushMaxEntriesTotal:       s.streamBatchFlushMaxEntries.Load(),
		StreamBatchFlushMaxBytesTotal:         s.streamBatchFlushMaxBytes.Load(),
		StreamBatchFlushMaxWaitTotal:          s.streamBatchFlushMaxWait.Load(),
		StreamBatchFlushShutdownTotal:         s.streamBatchFlushShutdown.Load(),
		FlatAccumulatorCursorWritesTotal:      s.flatAccumulatorCursorWrites.Load(),
		FlatAccumulatorCursorIndex:            s.flatAccumulatorCursorIndex.Load(),
		FlatAccumulatorSnapshotPersistsTotal:  flatAccumulatorSnapshotCount,
		FlatAccumulatorSnapshotIndex:          s.flatAccumulatorSnapshotIndex.Load(),
		FlatAccumulatorSnapshotBytesTotal:     flatAccumulatorSnapshotBytes,
		FlatAccumulatorSnapshotBytesAverage:   averageUint64(flatAccumulatorSnapshotBytes, flatAccumulatorSnapshotCount),
		FlatAccumulatorSnapshotBytesLast:      s.flatAccumulatorSnapshotLast.Load(),
		FlatAccumulatorSnapshotPersistMsTotal: nanosToMilliseconds(flatAccumulatorSnapshotNanos),
		FlatAccumulatorSnapshotPersistMsAverage: averageNanosMilliseconds(
			flatAccumulatorSnapshotNanos,
			flatAccumulatorSnapshotCount,
		),
		FlatAccumulatorSnapshotPersistMsMax:            nanosToMilliseconds(s.flatAccumulatorSnapshotMaxNs.Load()),
		FlatAccumulatorSnapshotSkippedTotal:            s.flatAccumulatorSnapshotSkipped.Load(),
		FlatAccumulatorDeltaBatchesTotal:               s.flatAccumulatorDeltaBatches.Load(),
		FlatAccumulatorDeltaEntriesTotal:               s.flatAccumulatorDeltaEntries.Load(),
		FlatAccumulatorDeltaOldestIndex:                s.flatAccumulatorDeltaOldestIndex.Load(),
		FlatAccumulatorDeltaNewestIndex:                s.flatAccumulatorDeltaNewestIndex.Load(),
		FlatAccumulatorDeltaReplayTotal:                s.flatAccumulatorDeltaReplayCount.Load(),
		FlatAccumulatorDeltaReplayBatchesTotal:         s.flatAccumulatorDeltaReplayBatches.Load(),
		FlatAccumulatorDeltaReplayEntriesTotal:         s.flatAccumulatorDeltaReplayEntries.Load(),
		FlatAccumulatorDeltaReplayFailuresTotal:        s.flatAccumulatorDeltaReplayFailures.Load(),
		FlatAccumulatorSnapshotMinEntries:              s.flatAccumulatorSnapshotMinEntries,
		FlatAccumulatorSnapshotMinIntervalMilliseconds: int64(s.flatAccumulatorSnapshotMinInterval / time.Millisecond),
		ScanFailuresTotal:                              s.scanFailures.Load(),
		CheckpointConflictsTotal:                       s.checkpointConflicts.Load(),
		ReconcileRetriesTotal:                          s.reconcileRetries.Load(),
		ReconcileQueueDepth:                            s.reconcileQueueDepth.Load(),
		ReconcileTaskRetriesTotal:                      s.reconcileTaskRetries.Load(),
		ReconcileDecodeFailuresTotal:                   s.reconcileDecodeFailures.Load(),
		ReconcileStalledTotal:                          s.reconcileStalled.Load(),
		ReconcileStuckSeconds:                          reconcileStuckSeconds,
		ReconcilePhase:                                 s.reconcilePhaseString(),
		LastAppliedAgeSeconds:                          lastAppliedAgeSeconds,
		ReconcileMaxRPCBytes:                           s.reconcileMaxRPCBytes,
		ReconcileMaxWallTimeSeconds:                    int64(s.reconcileMaxWallTime / time.Second),
		ReconcileMaxInflightTasks:                      s.reconcileMaxInflightTasks,
		StreamBatchMaxEntries:                          s.streamBatchMaxEntries,
		StreamBatchMaxBytes:                            s.streamBatchMaxBytes,
		StreamBatchMaxWaitMilliseconds:                 int64(s.streamBatchMaxWait / time.Millisecond),
		FallbackActive:                                 s.fallbackActive.Load(),
		FallbackCount:                                  s.fallbackCount.Load(),
		FallbackLastReason:                             s.getFallbackLastReason(),
		FallbackLastAt:                                 fallbackLastAt,
		ReconcileTaskRate:                              taskRate,
		PrimaryWriteRateEPS:                            primaryRate,
		SecondaryApplyRateEPS:                          secondaryRate,
		LagEntries:                                     lagEntries,
		LagSlopeEPS:                                    lagSlope,
		PredictedCatchupSeconds:                        predictedCatchup,
		ReconcilePutWorkersActive:                      s.reconcilePutWorkers.Load(),
		ReconcileDeletePhaseSeconds:                    float64(s.reconcileDeletePhaseMS.Load()) / 1000.0,
	}
}

// DRSecondaryStatus is a point-in-time snapshot of replication status.
type DRSecondaryStatus struct {
	State                                             string
	RelationshipID                                    string
	PrimaryIndex                                      uint64
	LastAppliedIndex                                  uint64
	EntriesApplied                                    uint64
	ReconcileCount                                    uint64
	LastReconcileAt                                   time.Time
	ConnectRetries                                    uint64
	ConnectFailures                                   uint64
	ReconcileRangesInflight                           int64
	ReconcileRangesFailed                             int64
	ReconcileBudgetRemainingBytes                     uint64
	ReconcileActiveCheckpointID                       string
	ReconcileActiveCheckpointIndex                    uint64
	ReconcileFailReasonLast                           string
	RangeManifestCount                                int
	RangeSplitCount                                   uint64
	ReconcileRPCBytesUsed                             uint64
	FlatAccumulatorFastPathTotal                      uint64
	FlatAccumulatorEmptyRepairTotal                   uint64
	FlatAccumulatorEmptyRepairRanges                  uint64
	FlatAccumulatorIndexedRepairTotal                 uint64
	FlatAccumulatorIndexedRepairRanges                uint64
	FlatAccumulatorIndexedRepairProofMismatches       uint64
	FlatAccumulatorIndexedRepairProofMismatchRange    uint64
	FlatAccumulatorIndexedRepairProofMismatchLocal    uint64
	FlatAccumulatorIndexedRepairProofMismatchRemote   uint64
	FlatAccumulatorIndexedRepairProofMismatchChecksum bool
	LocalKIDIndexBucketLoadsTotal                     uint64
	LocalKIDIndexEntriesLoadedTotal                   uint64
	LocalKIDIndexLoadFailuresTotal                    uint64
	LocalKIDIndexResetsTotal                          uint64
	LocalKIDIndexUpdatesTotal                         uint64
	LocalKIDIndexFallbackScansTotal                   uint64
	LocalKIDIndexFallbackScanReasonLast               string
	LocalKIDIndexInvalidationsTotal                   uint64
	LocalKIDIndexInvalidationReasonLast               string
	StreamTxnCoalescedEntriesTotal                    uint64
	StreamTxnBatchesTotal                             uint64
	StreamTxnEntriesTotal                             uint64
	StreamTxnPhysicalEntriesTotal                     uint64
	StreamTxnAverageEntries                           float64
	StreamTxnMaxEntries                               uint64
	StreamTxnAveragePhysicalEntries                   float64
	StreamTxnMaxPhysicalEntries                       uint64
	StreamTxnApplyMillisecondsTotal                   float64
	StreamTxnApplyMillisecondsAverage                 float64
	StreamTxnApplyMillisecondsMax                     float64
	StreamTxnCommitMillisecondsTotal                  float64
	StreamTxnCommitMillisecondsAverage                float64
	StreamTxnCommitMillisecondsMax                    float64
	StreamBatchFlushMaxEntriesTotal                   uint64
	StreamBatchFlushMaxBytesTotal                     uint64
	StreamBatchFlushMaxWaitTotal                      uint64
	StreamBatchFlushShutdownTotal                     uint64
	FlatAccumulatorCursorWritesTotal                  uint64
	FlatAccumulatorCursorIndex                        uint64
	FlatAccumulatorSnapshotPersistsTotal              uint64
	FlatAccumulatorSnapshotIndex                      uint64
	FlatAccumulatorSnapshotBytesTotal                 uint64
	FlatAccumulatorSnapshotBytesAverage               float64
	FlatAccumulatorSnapshotBytesLast                  uint64
	FlatAccumulatorSnapshotPersistMsTotal             float64
	FlatAccumulatorSnapshotPersistMsAverage           float64
	FlatAccumulatorSnapshotPersistMsMax               float64
	FlatAccumulatorSnapshotSkippedTotal               uint64
	FlatAccumulatorDeltaBatchesTotal                  uint64
	FlatAccumulatorDeltaEntriesTotal                  uint64
	FlatAccumulatorDeltaOldestIndex                   uint64
	FlatAccumulatorDeltaNewestIndex                   uint64
	FlatAccumulatorDeltaReplayTotal                   uint64
	FlatAccumulatorDeltaReplayBatchesTotal            uint64
	FlatAccumulatorDeltaReplayEntriesTotal            uint64
	FlatAccumulatorDeltaReplayFailuresTotal           uint64
	FlatAccumulatorSnapshotMinEntries                 uint64
	FlatAccumulatorSnapshotMinIntervalMilliseconds    int64
	ScanFailuresTotal                                 uint64
	CheckpointConflictsTotal                          uint64
	ReconcileRetriesTotal                             uint64
	ReconcileQueueDepth                               int64
	ReconcileTaskRetriesTotal                         uint64
	ReconcileDecodeFailuresTotal                      uint64
	ReconcileStalledTotal                             uint64
	ReconcileStuckSeconds                             int64
	ReconcilePhase                                    string
	LastAppliedAgeSeconds                             int64
	ReconcileMaxRPCBytes                              uint64
	ReconcileMaxWallTimeSeconds                       int64
	ReconcileMaxInflightTasks                         int
	StreamBatchMaxEntries                             int
	StreamBatchMaxBytes                               int
	StreamBatchMaxWaitMilliseconds                    int64
	FallbackActive                                    bool
	FallbackCount                                     uint64
	FallbackLastReason                                string
	FallbackLastAt                                    time.Time
	ReconcileTaskRate                                 float64
	PrimaryWriteRateEPS                               float64
	SecondaryApplyRateEPS                             float64
	LagEntries                                        uint64
	LagSlopeEPS                                       float64
	PredictedCatchupSeconds                           float64
	ReconcilePutWorkersActive                         int64
	ReconcileDeletePhaseSeconds                       float64
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
	if cfg.FlatAccumulatorSnapshotMinEntries > 0 {
		s.flatAccumulatorSnapshotMinEntries = cfg.FlatAccumulatorSnapshotMinEntries
	}
	if cfg.FlatAccumulatorSnapshotMinIntervalMillis > 0 {
		s.flatAccumulatorSnapshotMinInterval = time.Duration(cfg.FlatAccumulatorSnapshotMinIntervalMillis) * time.Millisecond
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

func (s *drReplicationSecondary) stopAwareContext(ctx context.Context) (context.Context, context.CancelFunc) {
	child, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-child.Done():
		}
	}()
	return child, cancel
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
				if status.Code(err) == codes.PermissionDenied {
					s.cancelActiveStream()
					if errCh != nil {
						select {
						case errCh <- fmt.Errorf("heartbeat authorization failed: %w", err):
						default:
						}
					}
					return
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
	stopCtx, stopCancel := s.stopAwareContext(ctx)
	defer stopCancel()
	ctx = stopCtx

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
	fetchSpans, err := s.fetchRemoteDigestSpans(ctx, checkpoint, full)
	if err != nil {
		return fmt.Errorf("resnapshot digest proof failed: %w", err)
	}
	accumulators := make([]drFetchedRangeAccumulator, len(fetchSpans))
	seenRemoteKIDs := make(map[[32]byte]struct{})

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
		RelationshipId:  s.relationshipID,
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
	applyPipeline, err := newDRPutApplyPipeline(fetchCtx, s, nil, markProgress, nil, false)
	if err != nil {
		return wrapReconcileFailure(drReconcileFailureApplyFailed, "prepare resnapshot apply pipeline", err)
	}
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
		if batch == nil {
			return fmt.Errorf("resnapshot fetch returned nil batch")
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
				kid, vid, err := s.kidVIDFromFetchedChange(e)
				if err != nil {
					return fmt.Errorf("resnapshot fetch proof failed: %w", err)
				}
				if _, ok := seenRemoteKIDs[kid]; ok {
					return fmt.Errorf("resnapshot fetch returned duplicate kid %x", kid)
				}
				spanIndex, err := fetchSpanIndex(fetchSpans, kid)
				if err != nil {
					return fmt.Errorf("resnapshot fetch proof failed: %w", err)
				}
				accumulators[spanIndex].add(kid, vid)
				seenRemoteKIDs[kid] = struct{}{}
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
	if err := validateFetchedRangeProofs(fetchSpans, accumulators); err != nil {
		return fmt.Errorf("resnapshot fetch proof failed: %w", err)
	}

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
		for _, key := range removeKeys {
			kid := s.scanner.ComputeKID(key)
			delete(localSet.KIDToVID, kid)
			delete(localSet.KIDToKey, kid)
			delete(localSet.Entries, kid)
		}
		markProgress()
	}

	s.setReconcilePhase(drReconcilePhaseFinalize)
	if err := s.finalizeReconcileCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("resnapshot finalize failed: %w", err)
	}
	s.persistReconcileOptimizerStateBestEffort(ctx, localSet, checkpoint.CommitIndex, "resnapshot")
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
	if s.drClusterClient != nil {
		// Use the exact client certificate fingerprint selected during the
		// mTLS handshake for this connection. This must match the fingerprint
		// observed by the primary when building wrap AAD.
		unwrapSecondaryFP = s.drClusterClient.LastClientCertFingerprint()
	}
	if localCert := s.core.localClusterParsedCert.Load(); localCert != nil {
		if unwrapSecondaryFP == "" {
			unwrapSecondaryFP = certFingerprintSHA256(localCert)
		}
	}
	if unwrapClusterID == "" {
		return fmt.Errorf("DR cluster ID unavailable for key unwrap")
	}
	if unwrapPrimaryIdentity == "" {
		return fmt.Errorf("primary DR transport CA identity unavailable for key unwrap")
	}
	if unwrapSecondaryFP == "" {
		return fmt.Errorf("secondary certificate fingerprint unavailable for key unwrap")
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
	if s.core.physicalCache != nil {
		s.core.physicalCache.Purge(ctx)
	}
	if err := s.core.setupCluster(ctx); err != nil {
		return fmt.Errorf("failed to recreate local cluster info after bootstrap purge: %w", err)
	}
	if mgr := s.core.drManager; mgr != nil {
		if err := mgr.PersistSecondaryKeyringBootstrap(ctx); err != nil {
			return fmt.Errorf("failed to persist DR keyring bootstrap state after purge: %w", err)
		}
	} else {
		return fmt.Errorf("failed to persist DR keyring bootstrap state after purge: DR manager unavailable")
	}
	if err := s.core.ensureRaftTLSKeyringForDRSecondary(ctx); err != nil {
		return fmt.Errorf("failed to ensure raft TLS keyring after bootstrap purge: %w", err)
	}
	generation, _ := s.core.beginDRKeyTransition("dr secondary bootstrap key transition")
	if s.core.invalidations != nil {
		s.core.invalidations.ensureDRKeyTransitionWorker(generation)
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
	"core/recovery-config":         true, // local auto-unseal recovery config
	"core/recovery-key":            true, // local auto-unseal recovery key material
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

type drStreamBatchFlushReason string

const (
	drStreamBatchFlushMaxEntries drStreamBatchFlushReason = "max_entries"
	drStreamBatchFlushMaxBytes   drStreamBatchFlushReason = "max_bytes"
	drStreamBatchFlushMaxWait    drStreamBatchFlushReason = "max_wait"
	drStreamBatchFlushShutdown   drStreamBatchFlushReason = "shutdown"
)

func (s *drReplicationSecondary) recordStreamBatchFlush(reason drStreamBatchFlushReason) {
	if s == nil {
		return
	}
	switch reason {
	case drStreamBatchFlushMaxEntries:
		s.streamBatchFlushMaxEntries.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_batch_flush_max_entries_total"}, 1)
	case drStreamBatchFlushMaxBytes:
		s.streamBatchFlushMaxBytes.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_batch_flush_max_bytes_total"}, 1)
	case drStreamBatchFlushMaxWait:
		s.streamBatchFlushMaxWait.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_batch_flush_max_wait_total"}, 1)
	case drStreamBatchFlushShutdown:
		s.streamBatchFlushShutdown.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_batch_flush_shutdown_total"}, 1)
	}
}

func (s *drReplicationSecondary) recordStreamTxnStats(logicalEntries, physicalEntries int, applyDuration, commitDuration time.Duration) {
	if s == nil || logicalEntries <= 0 {
		return
	}
	if physicalEntries < 0 {
		physicalEntries = 0
	}

	logical := uint64(logicalEntries)
	physical := uint64(physicalEntries)
	applyNanos := durationNanos(applyDuration)
	commitNanos := durationNanos(commitDuration)

	s.streamTxnBatches.Add(1)
	s.streamTxnEntries.Add(logical)
	s.streamTxnPhysicalEntries.Add(physical)
	atomicMaxUint64(&s.streamTxnMaxEntries, logical)
	atomicMaxUint64(&s.streamTxnMaxPhysicalEntries, physical)
	s.streamTxnApplyNanos.Add(applyNanos)
	atomicMaxUint64(&s.streamTxnApplyMaxNanos, applyNanos)
	s.streamTxnCommitNanos.Add(commitNanos)
	atomicMaxUint64(&s.streamTxnCommitMaxNanos, commitNanos)

	metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_batches_total"}, 1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_entries_total"}, float32(logicalEntries))
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_physical_entries_total"}, float32(physicalEntries))
	metrics.SetGauge([]string{"replication", "dr", "secondary", "stream_txn_max_entries"}, float32(s.streamTxnMaxEntries.Load()))
	metrics.SetGauge([]string{"replication", "dr", "secondary", "stream_txn_max_physical_entries"}, float32(s.streamTxnMaxPhysicalEntries.Load()))
	metrics.MeasureSince([]string{"replication", "dr", "secondary", "stream_txn_apply_duration"}, time.Now().Add(-applyDuration))
	metrics.MeasureSince([]string{"replication", "dr", "secondary", "stream_txn_commit_duration"}, time.Now().Add(-commitDuration))
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

	applyStopped := func() bool {
		if ctx.Err() != nil {
			return true
		}
		select {
		case <-s.stopCh:
			return true
		default:
			return false
		}
	}

	discardBatch := func() {
		batch = batch[:0]
		batchBytes = 0
	}

	// Flush consumes the current batch. It optimistically tries a transaction,
	// and falls back to sequential application on error.
	flush := func(reason drStreamBatchFlushReason) error {
		if len(batch) == 0 {
			return nil
		}
		if applyStopped() {
			discardBatch()
			return nil
		}

		flushedCount := len(batch)
		applyStart := time.Now()

		// Optimization: Try to apply as a transaction if supported
		if txnSupported {
			if err := s.applyStreamTxn(ctx, txnBackend, batch, reason == drStreamBatchFlushShutdown); err == nil {
				// Transaction succeeded
				txnLogOnce.Do(func() {
					s.logger.Info("stream apply using transactional batching",
						"batch_max_entries", maxEntries,
						"batch_max_bytes", maxBytes)
				})
				s.recordStreamBatchFlush(reason)
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_success"}, 1)
				metrics.MeasureSince([]string{"replication", "dr", "secondary", "apply_latency"}, applyStart)

				// Clear batch
				batch = batch[:0]
				batchBytes = 0
				replenishCredits(flushedCount)
				applyYield()
				return nil
			} else {
				if errors.Is(err, errDRSecondaryApplyStopped) || applyStopped() {
					discardBatch()
					return nil
				}
				// Transaction failed; log warning and fall back to sequential
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_fallback"}, 1)
				s.logger.Warn("transactional batch apply failed, falling back to sequential",
					"error", err, "batch_size", flushedCount)
			}
		}

		// Fallback: apply sequentially
		for _, change := range batch {
			if applyStopped() {
				discardBatch()
				return nil
			}
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
		s.recordStreamBatchFlush(reason)
		discardBatch()
		replenishCredits(flushedCount)
		applyYield()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			discardBatch()
			persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.persistCurrentFlatAccumulatorSnapshot(persistCtx)
			cancel()
			return ctx.Err()
		case <-s.stopCh:
			discardBatch()
			persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.persistCurrentFlatAccumulatorSnapshot(persistCtx)
			cancel()
			return nil
		case <-ticker.C:
			if err := flush(drStreamBatchFlushMaxWait); err != nil {
				return err
			}
		case incoming, ok := <-applyCh:
			if !ok {
				if err := flush(drStreamBatchFlushShutdown); err != nil {
					return err
				}
				persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.persistCurrentFlatAccumulatorSnapshot(persistCtx)
				cancel()
				return nil
			}
			for _, change := range incoming {
				batch = append(batch, change)
				// Rough estimate of memory size: key + value + overhead
				batchBytes += len(change.Key) + len(change.Value) + 48
			}
			if len(batch) >= maxEntries || batchBytes >= maxBytes {
				reason := drStreamBatchFlushMaxBytes
				if len(batch) >= maxEntries {
					reason = drStreamBatchFlushMaxEntries
				}
				if err := flush(reason); err != nil {
					return err
				}
			}
		}
	}
}

type drStreamTxnCoalescedChange struct {
	change   *EntryChange
	position int
}

func coalesceDRStreamTxnBatch(batch []*EntryChange, current uint64) ([]*EntryChange, uint64, int, int, bool, bool, bool) {
	if len(batch) == 0 {
		return nil, 0, 0, 0, false, false, false
	}

	byKey := make(map[string]int)
	coalesced := make([]drStreamTxnCoalescedChange, 0, len(batch))
	var lastIndex uint64
	var affected int
	var coalescedEntries int
	var keyringTouched bool
	var rootKeyTouched bool
	var runtimeStateTouched bool

	for i, change := range batch {
		if change == nil || change.RaftIndex < current {
			continue
		}
		if change.RaftIndex > lastIndex && change.Key == "" {
			lastIndex = change.RaftIndex
		}
		if change.Key == "" || isDRNeverReplicatePath(change.Key) {
			continue
		}
		if change.RaftIndex > lastIndex {
			lastIndex = change.RaftIndex
		}

		op := physical.Operation(change.OpType)
		switch op {
		case physical.PutOperation:
			switch change.Key {
			case "core/keyring":
				keyringTouched = true
			case "core/root-key":
				rootKeyTouched = true
			}
			if isDRRuntimeStatePath(change.Key) {
				runtimeStateTouched = true
			}
		case physical.DeleteOperation:
			if isDRRuntimeStatePath(change.Key) {
				runtimeStateTouched = true
			}
		}

		affected++
		if op != physical.PutOperation && op != physical.DeleteOperation {
			coalesced = append(coalesced, drStreamTxnCoalescedChange{change: change, position: i})
			continue
		}
		if existing, ok := byKey[change.Key]; ok {
			coalesced[existing] = drStreamTxnCoalescedChange{change: change, position: i}
			coalescedEntries++
			continue
		}
		byKey[change.Key] = len(coalesced)
		coalesced = append(coalesced, drStreamTxnCoalescedChange{change: change, position: i})
	}

	sort.SliceStable(coalesced, func(i, j int) bool {
		return coalesced[i].position < coalesced[j].position
	})

	changes := make([]*EntryChange, 0, len(coalesced))
	for _, entry := range coalesced {
		changes = append(changes, entry.change)
	}
	return changes, lastIndex, affected, coalescedEntries, keyringTouched, rootKeyTouched, runtimeStateTouched
}

// applyStreamTxn attempts to apply a batch of changes in a single transaction.
func (s *drReplicationSecondary) applyStreamTxn(ctx context.Context, backend physical.Transactional, batch []*EntryChange, forceAccumulatorSnapshot bool) error {
	if err := s.ensureStreamApplyActive(ctx); err != nil {
		return err
	}

	txn, err := backend.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer txn.Rollback(ctx)

	applyStart := time.Now()
	current := s.lastAppliedIndex.Load()
	batch, lastIndex, affected, coalescedEntries, keyringTouched, rootKeyTouched, runtimeStateTouched := coalesceDRStreamTxnBatch(batch, current)
	materializedEntries := 0
	var invalidateKeys []string
	accumulatorWarm := s.rangeAccumulator != nil && s.rangeAccumulator.isInitialized()
	var accumulatorBase [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	var accumulatorNext [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	if accumulatorWarm {
		accumulatorIndex, buckets, ok := s.rangeAccumulator.snapshot()
		if !ok || accumulatorIndex != current {
			s.logger.Warn("disabling DR flat accumulator because stream transaction is not index-aligned",
				"accumulator_index", accumulatorIndex,
				"last_applied_index", current)
			s.rangeAccumulator.invalidate()
			accumulatorWarm = false
		} else {
			accumulatorBase = buckets
			accumulatorNext = buckets
		}
	}
	accumulatorPending := make(map[string]drFlatAccumulatorPendingState)
	var accumulatorDeltas []drFlatAccumulatorDelta

	for _, change := range batch {
		if err := s.ensureStreamApplyActive(ctx); err != nil {
			return err
		}

		op := physical.Operation(change.OpType)
		if accumulatorWarm {
			delta, ok, err := s.accumulatorDeltaForChange(ctx, txn, change, "", accumulatorPending)
			if err != nil {
				s.logger.Warn("disabling DR flat accumulator after stream transaction read failure", "key", change.Key, "error", err)
				s.rangeAccumulator.invalidate()
				accumulatorWarm = false
				accumulatorDeltas = nil
				accumulatorNext = accumulatorBase
			} else if ok {
				accumulatorDeltas = append(accumulatorDeltas, delta)
			}
		}
		switch op {
		case physical.DeleteOperation:
			if err := txn.Delete(ctx, change.Key); err != nil {
				return err
			}
			materializedEntries++
			invalidateKeys = append(invalidateKeys, change.Key)
			if isDRRuntimeStatePath(change.Key) {
				runtimeStateTouched = true
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
			materializedEntries++
			invalidateKeys = append(invalidateKeys, change.Key)

			switch change.Key {
			case "core/keyring":
				keyringTouched = true
			case "core/root-key":
				rootKeyTouched = true
			}
			if isDRRuntimeStatePath(change.Key) {
				runtimeStateTouched = true
			}
		default:
			// Ignore unknown operations
		}
	}

	// If we only saw index-advance markers (no storage ops) we still
	// need to advance lastAppliedIndex so lag reporting stays accurate.
	if affected == 0 && lastIndex > 0 && lastIndex > current {
		if err := s.ensureStreamApplyActive(ctx); err != nil {
			return err
		}
		if accumulatorWarm {
			if err := s.persistFlatAccumulatorState(ctx, txn, lastIndex, accumulatorNext, forceAccumulatorSnapshot, nil); err != nil {
				return fmt.Errorf("persist flat accumulator marker: %w", err)
			}
			if err := s.advanceLocalKIDIndexMetaIfCurrent(ctx, txn, current, lastIndex); err != nil {
				return fmt.Errorf("advance local kid index marker: %w", err)
			}
			if err := txn.Commit(ctx); err != nil {
				return err
			}
			s.rangeAccumulator.replace(lastIndex, accumulatorNext)
		} else {
			if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, txn, "stream_marker_accumulator_not_warm"); err != nil {
				return fmt.Errorf("invalidate stale local kid index marker: %w", err)
			}
			if err := s.persistFlatAccumulatorCursor(ctx, txn, lastIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
				return fmt.Errorf("persist flat accumulator marker cursor: %w", err)
			}
			if err := txn.Commit(ctx); err != nil {
				return err
			}
		}
		s.setLastAppliedIndex(lastIndex)
		metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(lastIndex))
		return nil
	}

	if affected == 0 {
		return nil
	}

	if accumulatorWarm {
		if !applyFlatAccumulatorDeltasToBuckets(&accumulatorNext, accumulatorDeltas) {
			s.logger.Warn("disabling DR flat accumulator after stream transaction delta preflight failed", "last_index", lastIndex)
			s.rangeAccumulator.invalidate()
			accumulatorWarm = false
			accumulatorNext = accumulatorBase
		}
	}
	if accumulatorWarm {
		if err := s.persistFlatAccumulatorState(ctx, txn, lastIndex, accumulatorNext, forceAccumulatorSnapshot, accumulatorDeltas); err != nil {
			return fmt.Errorf("persist flat accumulator: %w", err)
		}
		if err := s.persistLocalKIDIndexChanges(ctx, txn, lastIndex, batch); err != nil {
			return fmt.Errorf("persist local kid index: %w", err)
		}
	} else if err := s.deletePersistedFlatAccumulator(ctx, txn); err != nil {
		return fmt.Errorf("delete stale flat accumulator: %w", err)
	} else if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, txn, "stream_txn_accumulator_not_warm"); err != nil {
		return fmt.Errorf("invalidate stale local kid index: %w", err)
	} else if err := s.persistFlatAccumulatorCursor(ctx, txn, lastIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
		return fmt.Errorf("persist flat accumulator cursor: %w", err)
	}

	commitStart := time.Now()
	if err := txn.Commit(ctx); err != nil {
		return err
	}
	commitDuration := time.Since(commitStart)
	applyDuration := commitStart.Sub(applyStart)
	if err := s.ensureStreamApplyActive(ctx); err != nil {
		if s.rangeAccumulator != nil {
			s.rangeAccumulator.invalidate()
		}
		return err
	}
	if accumulatorWarm {
		s.rangeAccumulator.replace(lastIndex, accumulatorNext)
	}

	if rootKeyTouched {
		if err := s.handleReplicatedKeyringUpdate(ctx, "transaction batch", "core/root-key", true); err != nil {
			if s.rangeAccumulator != nil {
				s.rangeAccumulator.invalidate()
			}
			return err
		}
	} else if keyringTouched {
		if err := s.handleReplicatedKeyringUpdate(ctx, "transaction batch", "core/keyring", false); err != nil {
			if s.rangeAccumulator != nil {
				s.rangeAccumulator.invalidate()
			}
			return err
		}
	}
	if runtimeStateTouched {
		if err := s.refreshRuntimeStateAfterApply(ctx, "stream transaction batch", false); err != nil {
			if s.rangeAccumulator != nil {
				s.rangeAccumulator.invalidate()
			}
			return err
		}
	}
	if err := s.ensureStreamApplyActive(ctx); err != nil {
		if s.rangeAccumulator != nil {
			s.rangeAccumulator.invalidate()
		}
		return err
	}
	s.invalidateAppliedStorageKeys(ctx, invalidateKeys)

	s.setLastAppliedIndex(lastIndex)
	s.entriesApplied.Add(uint64(affected))
	s.recordStreamTxnStats(affected, materializedEntries, applyDuration, commitDuration)
	if coalescedEntries > 0 {
		s.streamTxnCoalescedEntries.Add(uint64(coalescedEntries))
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "stream_txn_coalesced_entries_total"}, float32(coalescedEntries))
	}
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "entries_applied"}, float32(affected))
	metrics.SetGauge([]string{"replication", "dr", "secondary", "last_applied_index"}, float32(lastIndex))

	return nil
}

func (s *drReplicationSecondary) ensureStreamApplyActive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return errDRSecondaryApplyStopped
	}
	select {
	case <-s.stopCh:
		return errDRSecondaryApplyStopped
	default:
		return nil
	}
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
		if s.rangeAccumulator != nil {
			if _, buckets, ok := s.rangeAccumulator.snapshot(); ok {
				if err := s.persistFlatAccumulatorState(ctx, s.core.physical, change.RaftIndex, buckets, false, nil); err != nil {
					s.logger.Warn("failed to persist DR flat accumulator marker", "raft_index", change.RaftIndex, "error", err)
				} else {
					s.rangeAccumulator.replace(change.RaftIndex, buckets)
				}
			} else if err := s.persistFlatAccumulatorCursor(ctx, s.core.physical, change.RaftIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
				s.logger.Warn("failed to persist DR flat accumulator marker cursor", "raft_index", change.RaftIndex, "error", err)
			}
		}
		return nil
	}

	// Skip cluster-local paths that should never be replicated.
	if isDRNeverReplicatePath(change.Key) {
		s.logger.Debug("skipping cluster-local path in stream", "key", change.Key)
		if s.rangeAccumulator != nil {
			if _, buckets, ok := s.rangeAccumulator.snapshot(); ok {
				if err := s.persistFlatAccumulatorState(ctx, s.core.physical, change.RaftIndex, buckets, false, nil); err != nil {
					s.logger.Warn("failed to persist DR flat accumulator skip marker", "raft_index", change.RaftIndex, "key", change.Key, "error", err)
				} else {
					s.rangeAccumulator.replace(change.RaftIndex, buckets)
				}
			} else if err := s.persistFlatAccumulatorCursor(ctx, s.core.physical, change.RaftIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
				s.logger.Warn("failed to persist DR flat accumulator skip marker cursor", "raft_index", change.RaftIndex, "key", change.Key, "error", err)
			}
		}
		return nil
	}

	accumulatorWarm := s.rangeAccumulator != nil && s.rangeAccumulator.isInitialized()
	var accumulatorBase [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	var accumulatorNext [drRangeMaxTotalRanges]drFlatAccumulatorBucket
	var accumulatorDelta drFlatAccumulatorDelta
	accumulatorDeltaOK := false
	if accumulatorWarm {
		accumulatorIndex, buckets, ok := s.rangeAccumulator.snapshot()
		if !ok || accumulatorIndex > change.RaftIndex {
			s.logger.Warn("disabling DR flat accumulator because stream apply is not index-aligned",
				"accumulator_index", accumulatorIndex,
				"raft_index", change.RaftIndex)
			s.rangeAccumulator.invalidate()
			accumulatorWarm = false
		} else {
			accumulatorBase = buckets
			accumulatorNext = buckets
		}
	}
	if accumulatorWarm {
		pending := make(map[string]drFlatAccumulatorPendingState, 1)
		delta, ok, err := s.accumulatorDeltaForChange(ctx, s.core.physical, change, "", pending)
		if err != nil {
			s.logger.Warn("disabling DR flat accumulator after stream apply read failure", "key", change.Key, "error", err)
			s.rangeAccumulator.invalidate()
			accumulatorWarm = false
		} else {
			accumulatorDelta = delta
			accumulatorDeltaOK = ok
		}
	}
	if accumulatorWarm && accumulatorDeltaOK {
		if !applyFlatAccumulatorDeltasToBuckets(&accumulatorNext, []drFlatAccumulatorDelta{accumulatorDelta}) {
			s.logger.Warn("disabling DR flat accumulator after stream apply delta preflight failed", "raft_index", change.RaftIndex)
			s.rangeAccumulator.invalidate()
			accumulatorWarm = false
			accumulatorNext = accumulatorBase
		}
	}
	if err := s.deletePersistedFlatAccumulator(ctx, s.core.physical); err != nil {
		return fmt.Errorf("delete stale flat accumulator: %w", err)
	}
	if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "single_stream_apply"); err != nil {
		return fmt.Errorf("invalidate stale local kid index: %w", err)
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
			if err := s.handleReplicatedKeyringUpdate(ctx, "stream", change.Key, change.Key == "core/root-key"); err != nil {
				if s.rangeAccumulator != nil {
					s.rangeAccumulator.invalidate()
				}
				return err
			}
		}
		if isDRRuntimeStatePath(change.Key) {
			if err := s.refreshRuntimeStateAfterApply(ctx, "stream", false); err != nil {
				if s.rangeAccumulator != nil {
					s.rangeAccumulator.invalidate()
				}
				return err
			}
		}
		s.invalidateAppliedStorageKey(ctx, change.Key)
		if accumulatorWarm {
			s.rangeAccumulator.replace(change.RaftIndex, accumulatorNext)
			var deltas []drFlatAccumulatorDelta
			if accumulatorDeltaOK {
				deltas = []drFlatAccumulatorDelta{accumulatorDelta}
			}
			if err := s.persistFlatAccumulatorState(ctx, s.core.physical, change.RaftIndex, accumulatorNext, false, deltas); err != nil {
				s.logger.Warn("failed to persist DR flat accumulator after stream put", "raft_index", change.RaftIndex, "error", err)
			}
		} else if err := s.persistFlatAccumulatorCursor(ctx, s.core.physical, change.RaftIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
			s.logger.Warn("failed to persist DR flat accumulator cursor after stream put", "raft_index", change.RaftIndex, "error", err)
		}
		return nil

	case physical.DeleteOperation:
		if err := s.core.physical.Delete(ctx, change.Key); err != nil {
			return err
		}
		if isDRRuntimeStatePath(change.Key) {
			if err := s.refreshRuntimeStateAfterApply(ctx, "stream", false); err != nil {
				if s.rangeAccumulator != nil {
					s.rangeAccumulator.invalidate()
				}
				return err
			}
		}
		s.invalidateAppliedStorageKey(ctx, change.Key)
		if accumulatorWarm {
			s.rangeAccumulator.replace(change.RaftIndex, accumulatorNext)
			var deltas []drFlatAccumulatorDelta
			if accumulatorDeltaOK {
				deltas = []drFlatAccumulatorDelta{accumulatorDelta}
			}
			if err := s.persistFlatAccumulatorState(ctx, s.core.physical, change.RaftIndex, accumulatorNext, false, deltas); err != nil {
				s.logger.Warn("failed to persist DR flat accumulator after stream delete", "raft_index", change.RaftIndex, "error", err)
			}
		} else if err := s.persistFlatAccumulatorCursor(ctx, s.core.physical, change.RaftIndex, s.flatAccumulatorSnapshotIndex.Load()); err != nil {
			s.logger.Warn("failed to persist DR flat accumulator cursor after stream delete", "raft_index", change.RaftIndex, "error", err)
		}
		return nil

	default:
		s.logger.Warn("unknown operation type in change stream",
			"op_type", change.OpType, "key", change.Key)
		if s.rangeAccumulator != nil {
			s.rangeAccumulator.advanceIndex(change.RaftIndex)
		}
		return nil
	}
}

func (s *drReplicationSecondary) handleReplicatedKeyringUpdate(ctx context.Context, source, key string, strict bool) error {
	s.logger.Info("keyring/root-key update detected via "+source, "key", key)

	if err := s.core.barrier.ReloadRootKey(ctx); err != nil {
		if strict {
			return fmt.Errorf("reload root key after %s update: %w", source, err)
		}
		s.logger.Warn("failed to reload root key after "+source+" update", "key", key, "error", err)
		return nil
	}
	if err := s.core.barrier.ReloadKeyring(ctx); err != nil {
		if strict {
			return fmt.Errorf("reload keyring after %s update: %w", source, err)
		}
		s.logger.Warn("failed to reload keyring after "+source+" update", "key", key, "error", err)
		return nil
	}

	keyring, err := s.core.barrier.Keyring()
	if err != nil {
		if strict {
			return fmt.Errorf("read keyring after %s update: %w", source, err)
		}
		s.logger.Warn("failed to read keyring after "+source+" update", "key", key, "error", err)
		return nil
	}
	if err := s.core.seal.SetStoredKeys(ctx, [][]byte{keyring.RootKey()}); err != nil {
		if strict {
			return fmt.Errorf("persist rotated root key after %s update: %w", source, err)
		}
		s.logger.Error("failed to persist rotated root key in seal", "key", key, "error", err)
	}
	if s.runtimeStateRefreshPending.Load() {
		if err := s.refreshRuntimeStateAfterApply(ctx, source+" keyring retry", false); err != nil {
			return err
		}
	}
	return nil
}

func (s *drReplicationSecondary) invalidateAppliedStorageKeys(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		s.invalidateAppliedStorageKey(ctx, key)
	}
}

func (s *drReplicationSecondary) invalidateAppliedStorageKey(ctx context.Context, key string) {
	if s == nil || s.core == nil || key == "" || !isDRAppliedStorageInvalidationPath(key) {
		return
	}
	if s.core.namespaceStore == nil {
		s.logger.Debug("skipping DR applied storage invalidation because namespace store is not available", "key", key)
		return
	}

	// DR secondaries apply primary storage mutations directly below the
	// barrier. Route-backed backends therefore do not see the normal local
	// write-side invalidation path, so force the same invalidation after the
	// replicated write commits.
	s.core.invalidateSynchronous(key)
}

func isDRAppliedStorageInvalidationPath(key string) bool {
	normalized := strings.Trim(key, "/")
	if normalized == "" {
		return false
	}

	candidates := []string{normalized}
	if keySuffix, ok := strings.CutPrefix(normalized, namespaceBarrierPrefix); ok {
		if namespaceUUID, namespacedKey, found := strings.Cut(keySuffix, "/"); found && namespaceUUID != "" && namespacedKey != "" {
			candidates = append(candidates, namespacedKey)
		}
	}

	for _, candidate := range candidates {
		if isMissedMountKey(candidate) ||
			strings.HasPrefix(candidate, "sys/policy/") {
			return true
		}
	}
	return false
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
	return s.applyFetchedChangeWithKIDMapAndLocalKIDIndexMode(ctx, change, kidToKey, false)
}

func (s *drReplicationSecondary) applyFetchedChangeWithKIDMapPreservingLocalKIDIndex(ctx context.Context, change *EntryChange, kidToKey map[[32]byte]string) error {
	return s.applyFetchedChangeWithKIDMapAndLocalKIDIndexMode(ctx, change, kidToKey, true)
}

func (s *drReplicationSecondary) applyFetchedChangeWithKIDMapAndLocalKIDIndexMode(ctx context.Context, change *EntryChange, kidToKey map[[32]byte]string, preserveLocalKIDIndex bool) error {
	// Skip cluster-local paths that should never be replicated.
	if change.Key != "" && isDRNeverReplicatePath(change.Key) {
		s.logger.Debug("skipping cluster-local path in fetch", "key", change.Key)
		return nil
	}
	if err := s.deletePersistedFlatAccumulator(ctx, s.core.physical); err != nil {
		return fmt.Errorf("delete stale flat accumulator: %w", err)
	}
	if !preserveLocalKIDIndex {
		if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "fetched_change_apply"); err != nil {
			return fmt.Errorf("invalidate stale local kid index: %w", err)
		}
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
			if err := s.handleReplicatedKeyringUpdate(ctx, "fetch", change.Key, change.Key == "core/root-key"); err != nil {
				return err
			}
		}
		if isDRRuntimeStatePath(change.Key) {
			if err := s.refreshRuntimeStateAfterApply(ctx, "fetch", false); err != nil {
				return err
			}
		}
		s.invalidateAppliedStorageKey(ctx, change.Key)
		return nil

	case physical.DeleteOperation:
		if change.Key != "" {
			if err := s.core.physical.Delete(ctx, change.Key); err != nil {
				return err
			}
			if isDRRuntimeStatePath(change.Key) {
				if err := s.refreshRuntimeStateAfterApply(ctx, "fetch", false); err != nil {
					return err
				}
			}
			s.invalidateAppliedStorageKey(ctx, change.Key)
			return nil
		}
		// If Key is empty but KID is present, resolve via local KID map.
		if len(change.Kid) == 32 && kidToKey != nil {
			var kid [32]byte
			copy(kid[:], change.Kid)
			if key, ok := kidToKey[kid]; ok {
				if isDRNeverReplicatePath(key) {
					return nil
				}
				if err := s.core.physical.Delete(ctx, key); err != nil {
					return err
				}
				if isDRRuntimeStatePath(key) {
					if err := s.refreshRuntimeStateAfterApply(ctx, "fetch", false); err != nil {
						return err
					}
				}
				s.invalidateAppliedStorageKey(ctx, key)
				return nil
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

	stopCtx, stopCancel := s.stopAwareContext(ctx)
	defer stopCancel()

	// Bound the total time a single reconciliation attempt may run so
	// that a slow scan, checkpoint build, or stalled FetchEntries RPC
	// cannot hang the secondary indefinitely.
	reconcileCtx, reconcileCancel := context.WithTimeout(stopCtx, drDefaultReconcileTimeout)
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

	if done, err := s.tryFlatAccumulatorReconciliation(reconcileCtx, checkpoint, startTime); done || err != nil {
		return err
	}

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

type drFlatAccumulatorMismatch struct {
	rangeID uint64
	local   drFlatAccumulatorBucket
	remote  *RangeChecksum
}

func (s *drReplicationSecondary) tryFlatAccumulatorReconciliation(ctx context.Context, checkpoint *CheckpointResponse, startTime time.Time) (bool, error) {
	if s.rangeAccumulator == nil || checkpoint == nil || checkpoint.CommitIndex == 0 {
		return false, nil
	}
	accumulatorIndex, localBuckets, ok := s.rangeAccumulator.snapshot()
	if !ok {
		return false, nil
	}
	if accumulatorIndex > checkpoint.CommitIndex {
		return false, nil
	}

	rangesToVerify := buildCompleteRangeVerificationOrder(nil)
	mismatches := make([]drFlatAccumulatorMismatch, 0)
	const batchSize = 100
	for i := 0; i < len(rangesToVerify); i += batchSize {
		end := i + batchSize
		if end > len(rangesToVerify) {
			end = len(rangesToVerify)
		}
		batchIDs := rangesToVerify[i:end]

		rpcCtx, cancel := s.rpcContext(ctx)
		csumResp, err := s.client.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
			RelationshipId:  s.relationshipID,
			CheckpointId:    checkpoint.CheckpointId,
			CheckpointIndex: checkpoint.CommitIndex,
			RangeIds:        batchIDs,
		})
		cancel()
		if err != nil {
			return true, fmt.Errorf("reconcile failure [rpc_failed]: flat accumulator exchange range checksums: %w", err)
		}
		if csumResp == nil {
			return true, fmt.Errorf("reconcile failure [rpc_failed]: flat accumulator exchange range checksums returned nil response")
		}

		remoteChecksums := make(map[uint64]*RangeChecksum, len(csumResp.GetChecksums()))
		for _, rc := range csumResp.GetChecksums() {
			if rc == nil {
				continue
			}
			remoteChecksums[rc.GetRangeId()] = rc
		}

		for _, rangeID := range batchIDs {
			if rangeID >= uint64(len(localBuckets)) {
				return false, nil
			}
			local := localBuckets[rangeID]
			remote := remoteChecksums[rangeID]
			if remote == nil {
				if local.count != 0 {
					mismatches = append(mismatches, drFlatAccumulatorMismatch{
						rangeID: rangeID,
						local:   local,
					})
				}
				continue
			}
			if remote.GetChecksum() != local.checksum || remote.GetCount() != local.count {
				mismatches = append(mismatches, drFlatAccumulatorMismatch{
					rangeID: rangeID,
					local:   local,
					remote:  remote,
				})
			}
		}
	}

	if len(mismatches) > 0 {
		localSet := &reconciler.ReconciliationSet{
			Checkpoint: reconciler.Checkpoint{ID: checkpoint.CheckpointId, CommitIndex: accumulatorIndex},
			KIDToVID:   make(map[[32]byte][32]byte),
			KIDToKey:   make(map[[32]byte]string),
		}
		var nonEmptyRanges []uint64
		for _, mismatch := range mismatches {
			if mismatch.local.count != 0 {
				nonEmptyRanges = append(nonEmptyRanges, mismatch.rangeID)
			}
		}
		if len(nonEmptyRanges) > 0 {
			indexSet, ok, reason, err := s.loadLocalKIDIndexForRanges(ctx, s.core.physical, accumulatorIndex, nonEmptyRanges, localBuckets)
			if err != nil {
				s.localKIDIndexLoadFailures.Add(1)
				s.recordLocalKIDIndexFallbackScan(reason)
				s.logger.Warn("local KID index could not be loaded; falling back to local scan", "reason", reason, "error", err)
				if deleteErr := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "load_failed_"+reason); deleteErr != nil {
					return true, fmt.Errorf("invalidate stale local KID index after load failure: %w", deleteErr)
				}
				return false, nil
			}
			if !ok {
				s.recordLocalKIDIndexFallbackScan(reason)
				s.logger.Info("local KID index unavailable for flat accumulator repair; falling back to local scan",
					"reason", reason,
					"ranges", len(nonEmptyRanges),
					"accumulator_index", accumulatorIndex)
				return false, nil
			}
			localSet = indexSet
		}
		if err := s.runFlatAccumulatorIndexedBucketRepair(ctx, checkpoint, localBuckets, mismatches, localSet, startTime); err != nil {
			if errors.Is(err, errDRIndexedBucketRepairProofMismatch) {
				s.recordIndexedRepairProofMismatch(err)
				s.recordLocalKIDIndexFallbackScan("indexed_repair_proof_mismatch")
				s.logger.Warn("flat accumulator indexed-bucket repair could not prove completeness; falling back to full local scan",
					"checkpoint_id", checkpoint.CheckpointId,
					"checkpoint_index", checkpoint.CommitIndex,
					"error", err)
				if invalidateErr := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "indexed_repair_proof_mismatch"); invalidateErr != nil {
					return true, fmt.Errorf("invalidate stale local KID index after indexed repair proof failure: %w", invalidateErr)
				}
				if s.rangeAccumulator != nil {
					s.rangeAccumulator.invalidate()
				}
				return false, nil
			}
			return true, err
		}
		return true, nil
	}

	s.setReconcilePhase(drReconcilePhaseFinalize)
	if err := s.finalizeReconcileCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return true, fmt.Errorf("range reconciliation finalize failed: %w", err)
	}
	if accumulatorIndex != checkpoint.CommitIndex {
		s.rangeAccumulator.replace(checkpoint.CommitIndex, localBuckets)
		if err := s.persistFlatAccumulatorState(ctx, s.core.physical, checkpoint.CommitIndex, localBuckets, true, nil); err != nil {
			s.logger.Warn("failed to persist flat accumulator after phase-a match",
				"checkpoint_id", checkpoint.CheckpointId,
				"checkpoint_index", checkpoint.CommitIndex,
				"error", err)
		}
		if err := s.advanceLocalKIDIndexMetaIfCurrent(ctx, s.core.physical, accumulatorIndex, checkpoint.CommitIndex); err != nil {
			s.logger.Warn("failed to advance local KID index after phase-a match",
				"from_index", accumulatorIndex,
				"to_index", checkpoint.CommitIndex,
				"error", err)
			if invalidateErr := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "phase_a_meta_advance_failed"); invalidateErr != nil {
				s.logger.Warn("failed to invalidate local KID index after metadata advance failure", "error", invalidateErr)
			}
		}
	}
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
	s.reconcileCount.Add(1)
	s.flatAccumulatorFastPathTotal.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("range reconciliation: all ranges converged via flat accumulator",
		"ranges_checked", len(rangesToVerify),
		"accumulator_index", accumulatorIndex,
		"checkpoint_index", checkpoint.CommitIndex,
		"duration", time.Since(startTime))
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_fast_path_total"}, 1)
	return true, nil
}

func (s *drReplicationSecondary) runFlatAccumulatorIndexedBucketRepair(
	ctx context.Context,
	checkpoint *CheckpointResponse,
	baseBuckets [drRangeMaxTotalRanges]drFlatAccumulatorBucket,
	mismatches []drFlatAccumulatorMismatch,
	localSet *reconciler.ReconciliationSet,
	startTime time.Time,
) error {
	if len(mismatches) == 0 {
		return nil
	}
	if localSet == nil {
		localSet = &reconciler.ReconciliationSet{}
	}
	if localSet.KIDToVID == nil {
		localSet.KIDToVID = make(map[[32]byte][32]byte)
	}
	if localSet.KIDToKey == nil {
		localSet.KIDToKey = make(map[[32]byte]string)
	}

	s.setReconcilePhase(drReconcilePhaseRangeTasks)
	s.logger.Info("starting flat accumulator indexed-bucket repair",
		"checkpoint_id", checkpoint.CheckpointId,
		"commit_index", checkpoint.CommitIndex,
		"ranges", len(mismatches))

	budget := &drRangeBudget{
		start:       startTime,
		maxRPCBytes: s.reconcileMaxRPCBytes,
		maxWallTime: s.reconcileMaxWallTime,
	}
	localIndex := reconciler.NewRangeMapIndex(localSet.KIDToVID, nil)
	mutationTracker := newDRReconciliationSetMutationTracker(s)
	applyPipeline, err := newDRPutApplyPipeline(ctx, s, localSet.KIDToKey, s.markReconcileActivityNow, mutationTracker, true)
	if err != nil {
		return wrapReconcileFailure(drReconcileFailureApplyFailed, "prepare indexed repair apply pipeline", err)
	}
	defer func() {
		_ = applyPipeline.closeAndWait()
	}()

	if s.rangeAccumulator != nil {
		s.rangeAccumulator.invalidate()
	}
	appliedFetchedEntries := make([]*EntryChange, 0)
	pendingDeletes := make(map[string]struct{})
	for i, mismatch := range mismatches {
		if err := budget.check(); err != nil {
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}
		task := drQueuedRangeTask{
			id:       i + 1,
			priority: 1,
			task: drRangeTask{
				rangeID: mismatch.rangeID,
				span:    reconciler.SpanFromRangeID(mismatch.rangeID),
			},
		}
		result := s.processRangeTask(ctx, checkpoint, localIndex, localSet.KIDToKey, task)
		if result.err != nil {
			return result.err
		}
		if len(result.removedKeys) > 0 {
			for _, key := range result.removedKeys {
				pendingDeletes[key] = struct{}{}
			}
		}
		if err := budget.addRPC(result.rpcBytes); err != nil {
			return fmt.Errorf("reconcile failure [budget_exceeded]: %w", err)
		}
		budget.rangesHandled++
		if len(result.fetchedEntries) > 0 {
			if err := applyPipeline.submit(result.fetchedEntries); err != nil {
				return wrapReconcileFailure(drReconcileFailureApplyFailed, "queue fetched entries for apply", err)
			}
			appliedFetchedEntries = append(appliedFetchedEntries, result.fetchedEntries...)
			for _, change := range result.fetchedEntries {
				kid, _, err := s.kidVIDFromFetchedChange(change)
				if err != nil {
					return err
				}
				if rangeID := reconciler.RangeIDFromKID(kid); rangeID != mismatch.rangeID {
					return fmt.Errorf("fetched kid %x mapped to range %d outside requested range %d", kid, rangeID, mismatch.rangeID)
				}
			}
		}
	}

	s.setReconcilePhase(drReconcilePhaseApplyPuts)
	if err := applyPipeline.closeAndWait(); err != nil {
		return wrapReconcileFailure(drReconcileFailureStalled, "apply pipeline", err)
	}
	if err := mutationTracker.recordFetchedChanges(appliedFetchedEntries); err != nil {
		return wrapReconcileFailure(drReconcileFailureApplyFailed, "record indexed repair fetched entries", err)
	}
	if len(pendingDeletes) > 0 {
		s.setReconcilePhase(drReconcilePhaseApplyDeletes)
		keys := make([]string, 0, len(pendingDeletes))
		for key := range pendingDeletes {
			keys = append(keys, key)
		}
		deleteCtx, cancel := context.WithTimeout(ctx, drDefaultReconcileStallAbort)
		err := s.applyRemovedKeysPreservingLocalKIDIndex(deleteCtx, checkpoint.CheckpointId, checkpoint.CommitIndex, keys)
		cancel()
		if err != nil {
			return wrapReconcileFailure(drReconcileFailureApplyFailed, "apply indexed repair removed keys", err)
		}
		mutationTracker.recordRemovedKeys(keys)
	}
	mutationTracker.applyToSet(localSet)

	nextBuckets := baseBuckets
	repairedBuckets := drFlatAccumulatorBucketsFromSet(localSet)
	repairedRanges := make([]uint64, 0, len(mismatches))
	emptyOnly := true
	for _, mismatch := range mismatches {
		if mismatch.local.count != 0 {
			emptyOnly = false
		}
		repairedRanges = append(repairedRanges, mismatch.rangeID)
		nextBuckets[mismatch.rangeID] = repairedBuckets[mismatch.rangeID]
		remoteCount := uint64(0)
		remoteChecksum := uint64(0)
		if mismatch.remote != nil {
			remoteCount = mismatch.remote.GetCount()
			remoteChecksum = mismatch.remote.GetChecksum()
		}
		if nextBuckets[mismatch.rangeID].count != remoteCount || nextBuckets[mismatch.rangeID].checksum != remoteChecksum {
			return &drIndexedBucketRepairProofMismatchError{
				rangeID:          mismatch.rangeID,
				localCount:       nextBuckets[mismatch.rangeID].count,
				remoteCount:      remoteCount,
				checksumMismatch: nextBuckets[mismatch.rangeID].checksum != remoteChecksum,
			}
		}
	}

	s.setReconcilePhase(drReconcilePhaseFinalize)
	if err := s.finalizeReconcileCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("range reconciliation finalize failed: %w", err)
	}
	s.rangeAccumulator.replace(checkpoint.CommitIndex, nextBuckets)
	if err := s.persistFlatAccumulatorState(ctx, s.core.physical, checkpoint.CommitIndex, nextBuckets, true, nil); err != nil {
		s.logger.Warn("failed to persist flat accumulator after indexed-bucket repair",
			"checkpoint_id", checkpoint.CheckpointId,
			"checkpoint_index", checkpoint.CommitIndex,
			"error", err)
	}
	indexCtx, indexCancel := context.WithTimeout(ctx, s.reconcileOptimizerPersistTimeout())
	err = s.resetLocalKIDIndexRangesFromSet(indexCtx, s.core.physical, checkpoint.CommitIndex, localSet, repairedRanges)
	indexCancel()
	if err != nil {
		s.logger.Warn("failed to persist local KID index after indexed-bucket repair",
			"checkpoint_id", checkpoint.CheckpointId,
			"checkpoint_index", checkpoint.CommitIndex,
			"ranges", len(repairedRanges),
			"error", err)
		if invalidateErr := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "indexed_repair_persist_failed"); invalidateErr != nil {
			s.logger.Warn("failed to invalidate local KID index after indexed repair persist failure", "error", invalidateErr)
		}
	}
	s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
	s.reconcileCount.Add(1)
	s.flatAccumulatorIndexedRepairTotal.Add(1)
	s.flatAccumulatorIndexedRepairRanges.Add(uint64(len(mismatches)))
	if emptyOnly {
		s.flatAccumulatorEmptyRepairTotal.Add(1)
		s.flatAccumulatorEmptyRepairRanges.Add(uint64(len(mismatches)))
	}
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("flat accumulator indexed-bucket repair complete",
		"ranges_handled", budget.rangesHandled,
		"rpc_bytes", budget.rpcBytes,
		"duration", time.Since(startTime))
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_indexed_repair_total"}, 1)
	metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_indexed_repair_ranges_total"}, float32(len(mismatches)))
	if emptyOnly {
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_empty_repair_total"}, 1)
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "flat_accumulator_empty_repair_ranges_total"}, float32(len(mismatches)))
	}
	return nil
}

type drRangeTask struct {
	span    reconciler.RangeSpan
	rangeID uint64
}

type drRangeFetchProof struct {
	hasDigest bool

	count        uint64
	xorKeyHash   [32]byte
	xorValueHash [32]byte
}

type drFetchSpan struct {
	span  reconciler.RangeSpan
	proof drRangeFetchProof
}

type drFetchedRangeAccumulator struct {
	count        uint64
	xorKeyHash   [32]byte
	xorValueHash [32]byte
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

type drReconciliationSetPutMutation struct {
	vid   [32]byte
	key   string
	entry *physical.Entry
}

type drReconciliationSetMutationTracker struct {
	secondary *drReplicationSecondary

	mu      sync.Mutex
	puts    map[[32]byte]drReconciliationSetPutMutation
	deletes map[[32]byte]struct{}
}

func newDRReconciliationSetMutationTracker(secondary *drReplicationSecondary) *drReconciliationSetMutationTracker {
	return &drReconciliationSetMutationTracker{
		secondary: secondary,
		puts:      make(map[[32]byte]drReconciliationSetPutMutation),
		deletes:   make(map[[32]byte]struct{}),
	}
}

func (m *drReconciliationSetMutationTracker) recordFetchedChanges(changes []*EntryChange) error {
	if m == nil || m.secondary == nil {
		return nil
	}
	for _, change := range changes {
		if change == nil {
			continue
		}
		if change.Key != "" && isDRNeverReplicatePath(change.Key) {
			continue
		}
		kid, ok := m.secondary.kidFromEntryChange(change)
		if !ok {
			return fmt.Errorf("cannot update reconciliation set for change without resolvable kid")
		}

		switch physical.Operation(change.OpType) {
		case physical.PutOperation:
			vid := m.secondary.scanner.ComputeVIDWithSealWrap(change.Value, change.SealWrap)
			mutation := drReconciliationSetPutMutation{
				vid: vid,
				key: change.Key,
			}
			if change.Key != "" {
				mutation.entry = &physical.Entry{
					Key:      change.Key,
					Value:    append([]byte(nil), change.Value...),
					SealWrap: change.SealWrap,
				}
			}

			m.mu.Lock()
			delete(m.deletes, kid)
			m.puts[kid] = mutation
			m.mu.Unlock()

		case physical.DeleteOperation:
			m.mu.Lock()
			delete(m.puts, kid)
			m.deletes[kid] = struct{}{}
			m.mu.Unlock()

		default:
			return fmt.Errorf("unknown operation type in fetched change: op_type=%s key=%s", change.OpType, change.Key)
		}
	}
	return nil
}

func (m *drReconciliationSetMutationTracker) recordRemovedKeys(keys []string) {
	if m == nil || m.secondary == nil {
		return
	}
	for _, key := range keys {
		if key == "" || isDRNeverReplicatePath(key) {
			continue
		}
		kid := m.secondary.scanner.ComputeKID(key)
		m.mu.Lock()
		delete(m.puts, kid)
		m.deletes[kid] = struct{}{}
		m.mu.Unlock()
	}
}

func (m *drReconciliationSetMutationTracker) applyToSet(rs *reconciler.ReconciliationSet) {
	if m == nil || rs == nil {
		return
	}
	if rs.KIDToVID == nil {
		rs.KIDToVID = make(map[[32]byte][32]byte)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for kid := range m.deletes {
		delete(rs.KIDToVID, kid)
		if rs.KIDToKey != nil {
			delete(rs.KIDToKey, kid)
		}
		if rs.Entries != nil {
			delete(rs.Entries, kid)
		}
	}
	for kid, mutation := range m.puts {
		rs.KIDToVID[kid] = mutation.vid
		if rs.KIDToKey != nil && mutation.key != "" {
			rs.KIDToKey[kid] = mutation.key
		}
		if rs.Entries != nil && mutation.entry != nil {
			rs.Entries[kid] = mutation.entry
		}
	}
	rs.KeyCount = len(rs.KIDToVID)
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
	secondary             *drReplicationSecondary
	ctx                   context.Context
	cancel                context.CancelFunc
	kidToKey              map[[32]byte]string
	onApply               func()
	mutations             *drReconciliationSetMutationTracker
	preserveLocalKIDIndex bool
	txnBackend            physical.TransactionalBackend // nil if physical backend doesn't support transactions
	submitted             atomic.Int64                  // total entries submitted (for diagnostics)

	shards []chan *EntryChange
	wg     sync.WaitGroup

	mu     sync.Mutex
	err    error
	closed bool
}

func (s *drReplicationSecondary) clearPersistedReconcileOptimizationState(ctx context.Context, preserveLocalKIDIndex bool) error {
	if s == nil || s.core == nil || s.core.physical == nil {
		return nil
	}
	if err := s.deletePersistedFlatAccumulator(ctx, s.core.physical); err != nil {
		return fmt.Errorf("delete stale flat accumulator: %w", err)
	}
	if !preserveLocalKIDIndex {
		if err := s.invalidatePersistedLocalKIDIndexWithReason(ctx, s.core.physical, "clear_reconcile_optimizer_state"); err != nil {
			return fmt.Errorf("invalidate stale local kid index: %w", err)
		}
	}
	return nil
}

func (s *drReplicationSecondary) reconcileOptimizerPersistTimeout() time.Duration {
	timeout := s.reconcileStallAbort
	if timeout <= 0 || timeout > drDefaultReconcileOptimizerPersistTimeout {
		timeout = drDefaultReconcileOptimizerPersistTimeout
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	return timeout
}

func (s *drReplicationSecondary) persistReconcileOptimizerStateBestEffort(ctx context.Context, localSet *reconciler.ReconciliationSet, checkpointIndex uint64, reason string) {
	if s == nil || localSet == nil || checkpointIndex == 0 {
		return
	}
	timeout := s.reconcileOptimizerPersistTimeout()
	persistCtx, cancel := context.WithTimeout(ctx, timeout)
	err := s.resetAndPersistFlatAccumulatorFromSet(persistCtx, localSet, checkpointIndex)
	cancel()
	if err == nil {
		return
	}

	s.logger.Warn("failed to persist DR reconciliation optimizer state; continuing with storage checkpoint committed",
		"reason", reason,
		"checkpoint_index", checkpointIndex,
		"timeout", timeout,
		"error", err)
	if s.rangeAccumulator != nil {
		s.rangeAccumulator.invalidate()
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	if invalidateErr := s.invalidatePersistedLocalKIDIndexWithReason(cleanupCtx, s.core.physical, "optimizer_persist_failed"); invalidateErr != nil {
		s.logger.Warn("failed to invalidate local KID index after optimizer persist failure", "error", invalidateErr)
	}
}

func newDRPutApplyPipeline(ctx context.Context, secondary *drReplicationSecondary, kidToKey map[[32]byte]string, onApply func(), mutations *drReconciliationSetMutationTracker, preserveLocalKIDIndex bool) (*drPutApplyPipeline, error) {
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
	if txnBackend != nil {
		if err := secondary.clearPersistedReconcileOptimizationState(ctx, preserveLocalKIDIndex); err != nil {
			cancel()
			return nil, err
		}
	}

	p := &drPutApplyPipeline{
		secondary:             secondary,
		ctx:                   workerCtx,
		cancel:                cancel,
		kidToKey:              kidToKey,
		onApply:               onApply,
		mutations:             mutations,
		preserveLocalKIDIndex: preserveLocalKIDIndex,
		txnBackend:            txnBackend,
		shards:                make([]chan *EntryChange, workers),
	}
	for i := 0; i < workers; i++ {
		ch := make(chan *EntryChange, queueDepth/workers+1)
		p.shards[i] = ch
		p.wg.Add(1)
		go p.runWorker(ch)
	}
	return p, nil
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
				var err error
				if p.preserveLocalKIDIndex {
					err = p.secondary.applyFetchedChangeWithKIDMapPreservingLocalKIDIndex(p.ctx, change, p.kidToKey)
				} else {
					err = p.secondary.applyFetchedChange(p.ctx, change, p.kidToKey)
				}
				if err != nil {
					p.setErr(fmt.Errorf("reconcile apply worker failed: %w", err))
					return
				}
				if p.mutations != nil && drFetchedChangeAppliesToStorage(change, p.kidToKey) {
					if err := p.mutations.recordFetchedChanges([]*EntryChange{change}); err != nil {
						p.setErr(fmt.Errorf("reconcile apply worker failed: %w", err))
						return
					}
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
	var runtimeStateTouched bool

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
			if isDRRuntimeStatePath(change.Key) {
				runtimeStateTouched = true
			}
		}
	}

	// Batch-commit normal entries in a single Raft round.
	if len(normalEntries) > 0 {
		for attempt := 0; ; attempt++ {
			if err := p.ctx.Err(); err != nil {
				return err
			}
			tx, err := p.txnBackend.BeginTx(p.ctx)
			if err != nil {
				return fmt.Errorf("begin batch txn: %w", err)
			}
			appliedNormalEntries := make([]*EntryChange, 0, len(normalEntries))
			invalidateKeys := make([]string, 0, len(normalEntries))

			for _, change := range normalEntries {
				appliedKey := change.Key
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
					} else {
						continue
					}
					appliedKey = key

				default:
					_ = tx.Rollback(p.ctx)
					return fmt.Errorf("unknown operation type in fetched change: op_type=%s key=%s", change.OpType, change.Key)
				}
				if appliedKey != "" {
					invalidateKeys = append(invalidateKeys, appliedKey)
				}
				appliedNormalEntries = append(appliedNormalEntries, change)
			}

			if err := tx.Commit(p.ctx); err != nil {
				if errors.Is(err, physical.ErrTransactionCommitFailure) && attempt < drDefaultReconcileTxnCommitRetries {
					p.secondary.logger.Debug("retrying DR reconcile transaction after commit conflict",
						"entries", len(normalEntries),
						"attempt", attempt+1,
						"max_attempts", drDefaultReconcileTxnCommitRetries,
						"error", err)
					delay := time.Duration(attempt+1) * drDefaultReconcileTxnCommitRetryDelay
					timer := time.NewTimer(delay)
					select {
					case <-p.ctx.Done():
						timer.Stop()
						return p.ctx.Err()
					case <-timer.C:
					}
					continue
				}
				return fmt.Errorf("commit batch txn (%d entries): %w", len(normalEntries), err)
			}
			if runtimeStateTouched {
				if err := p.secondary.refreshRuntimeStateAfterApply(p.ctx, "fetch transaction batch", false); err != nil {
					return err
				}
			}
			p.secondary.invalidateAppliedStorageKeys(p.ctx, invalidateKeys)
			if p.mutations != nil {
				if err := p.mutations.recordFetchedChanges(appliedNormalEntries); err != nil {
					return err
				}
			}
			break
		}
	}

	// Apply keyring / root-key entries individually so that the barrier
	// reload and seal-persist side-effects run in the correct order.
	for _, change := range keyringEntries {
		var err error
		if p.preserveLocalKIDIndex {
			err = p.secondary.applyFetchedChangeWithKIDMapPreservingLocalKIDIndex(p.ctx, change, p.kidToKey)
		} else {
			err = p.secondary.applyFetchedChange(p.ctx, change, p.kidToKey)
		}
		if err != nil {
			return err
		}
		if p.mutations != nil && drFetchedChangeAppliesToStorage(change, p.kidToKey) {
			if err := p.mutations.recordFetchedChanges([]*EntryChange{change}); err != nil {
				return err
			}
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

func drFetchedChangeAppliesToStorage(change *EntryChange, kidToKey map[[32]byte]string) bool {
	if change == nil {
		return false
	}
	if change.Key != "" {
		return !isDRNeverReplicatePath(change.Key)
	}
	if physical.Operation(change.OpType) != physical.DeleteOperation || len(change.Kid) != 32 || kidToKey == nil {
		return physical.Operation(change.OpType) == physical.PutOperation
	}
	var kid [32]byte
	copy(kid[:], change.Kid)
	key, ok := kidToKey[kid]
	return ok && key != "" && !isDRNeverReplicatePath(key)
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

	// 1. Exchange Dirty Bitmap. The bitmap is a priority hint only; range
	// selection remains complete for every checkpoint reconciliation.
	dirtyHints := make([]bool, drRangeMaxTotalRanges)
	dirtyHintCount := 0
	rpcCtx, cancel := s.rpcContext(ctx)
	bitmapResp, err := s.client.ExchangeDirtyBitmap(rpcCtx, &DirtyBitmapMessage{
		RelationshipId:  s.relationshipID,
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
	})
	cancel()

	useBitmapHints := false
	if err == nil {
		if bitmapResp == nil {
			s.logger.Warn("dirty bitmap response was nil; using natural range order")
		} else if lastApplied := s.lastAppliedIndex.Load(); bitmapResp.StartIndex <= lastApplied {
			useBitmapHints = true
			s.logger.Info("using dirty bitmap priority hints", "start_index", bitmapResp.StartIndex, "last_applied", lastApplied)

			for i := 0; i < len(bitmapResp.Bitmap); i++ {
				b := bitmapResp.Bitmap[i]
				for j := 0; j < 8; j++ {
					if (b & (1 << j)) != 0 {
						rangeID := i*8 + j
						if rangeID < drRangeMaxTotalRanges && !dirtyHints[rangeID] {
							dirtyHints[rangeID] = true
							dirtyHintCount++
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

	rangesToVerify := buildCompleteRangeVerificationOrder(dirtyHints)

	s.logger.Info("ranges to verify",
		"count", len(rangesToVerify),
		"dirty_hint_count", dirtyHintCount,
		"using_bitmap_hints", useBitmapHints)

	// 2. Exchange Range Checksums (Batched)
	batchSize := 100
	for i := 0; i < len(rangesToVerify); i += batchSize {
		end := i + batchSize
		if end > len(rangesToVerify) {
			end = len(rangesToVerify)
		}
		batchIDs := rangesToVerify[i:end]

		rpcCtx, cancel := s.rpcContext(ctx)
		csumResp, err := s.client.ExchangeRangeChecksums(rpcCtx, &RangeChecksumRequest{
			RelationshipId:  s.relationshipID,
			CheckpointId:    checkpoint.CheckpointId,
			CheckpointIndex: checkpoint.CommitIndex,
			RangeIds:        batchIDs,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile failure [rpc_failed]: exchange range checksums: %w", err)
		}
		if csumResp == nil {
			return fmt.Errorf("reconcile failure [rpc_failed]: exchange range checksums returned nil response")
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
		remoteChecksums := make(map[uint64]*RangeChecksum, len(csumResp.GetChecksums()))
		for _, rc := range csumResp.GetChecksums() {
			if rc == nil {
				continue
			}
			remoteChecksums[rc.GetRangeId()] = rc
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
				if remoteRC.GetChecksum() != localSum || remoteRC.GetCount() != localCount {
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
		// All top-level ranges matched at the CRC64-XOR level (Phase A).
		// No Phase B drill-down or Phase C fetch required.
		s.setReconcilePhase(drReconcilePhaseFinalize)
		if err := s.finalizeReconcileCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
			return fmt.Errorf("range reconciliation finalize failed: %w", err)
		}
		s.persistReconcileOptimizerStateBestEffort(ctx, localSet, checkpoint.CommitIndex, "phase-a match")
		s.reconcileBudgetRemainingByte.Store(int64(s.reconcileMaxRPCBytes))
		s.reconcileCount.Add(1)
		s.lastReconcileAt.Store(time.Now().Unix())
		s.logger.Info("range reconciliation: all ranges converged at Phase A checksum",
			"ranges_checked", len(rangesToVerify),
			"dirty_hint_count", dirtyHintCount)
		return nil
	}

	s.logger.Info("ranges mismatched", "count", queue.Len())
	if s.rangeAccumulator != nil {
		s.rangeAccumulator.invalidate()
	}

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
					res := s.processRangeTask(workerCtx, checkpoint, localIndex, localSet.KIDToKey, queued)
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

	mutationTracker := newDRReconciliationSetMutationTracker(s)
	applyPipeline, err := newDRPutApplyPipeline(ctx, s, localSet.KIDToKey, s.markReconcileActivityNow, mutationTracker, false)
	if err != nil {
		workerCancel()
		return wrapReconcileFailure(drReconcileFailureApplyFailed, "prepare range repair apply pipeline", err)
	}
	defer func() {
		_ = applyPipeline.closeAndWait()
	}()
	appliedFetchedEntries := make([]*EntryChange, 0)
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
			appliedFetchedEntries = append(appliedFetchedEntries, res.fetchedEntries...)
		}
		// Range fetch streams primary-present entries; the worker also
		// reports local-only keys so deletes are applied after puts settle.
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
	if err := mutationTracker.recordFetchedChanges(appliedFetchedEntries); err != nil {
		return wrapReconcileFailure(drReconcileFailureApplyFailed, "record range repair fetched entries", err)
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
		mutationTracker.recordRemovedKeys(keys)
	}
	s.setReconcilePhase(drReconcilePhaseFinalize)

	if err := s.finalizeReconcileCheckpoint(checkpoint.CheckpointId, checkpoint.CommitIndex); err != nil {
		return fmt.Errorf("range reconciliation finalize failed: %w", err)
	}
	mutationTracker.applyToSet(localSet)
	s.persistReconcileOptimizerStateBestEffort(ctx, localSet, checkpoint.CommitIndex, "range repair")
	s.reconcileCount.Add(1)
	s.lastReconcileAt.Store(time.Now().Unix())
	s.logger.Info("range-first reconciliation complete",
		"ranges_handled", budget.rangesHandled,
		"rpc_bytes", budget.rpcBytes,
		"duration", time.Since(startTime))
	return nil
}

func buildCompleteRangeVerificationOrder(dirtyHints []bool) []uint64 {
	ranges := make([]uint64, 0, drRangeMaxTotalRanges)
	for i := 0; i < drRangeMaxTotalRanges; i++ {
		if i < len(dirtyHints) && dirtyHints[i] {
			ranges = append(ranges, uint64(i))
		}
	}
	for i := 0; i < drRangeMaxTotalRanges; i++ {
		if i >= len(dirtyHints) || !dirtyHints[i] {
			ranges = append(ranges, uint64(i))
		}
	}
	return ranges
}

func (s *drReplicationSecondary) processRangeTask(ctx context.Context, checkpoint *CheckpointResponse, localIndex *reconciler.RangeMapIndex, kidToKey map[[32]byte]string, queued drQueuedRangeTask) drRangeTaskResult {
	task := queued.task
	result := drRangeTaskResult{
		id:   queued.id,
		task: task,
	}

	// Attempt fine-grained drill-down to narrow the diff.
	fetchSpans, err := s.runRangeDrillDown(ctx, checkpoint, localIndex, task.span)
	if err != nil {
		result.err = err
		return result
	}
	if fetchSpans == nil {
		result.err = fmt.Errorf("drill-down did not return a fetch plan")
		return result
	}
	if len(fetchSpans) == 0 {
		return result
	}

	// Convert narrowed spans to proto and fetch entries.
	protoSpans := make([]*RangeSpan, len(fetchSpans))
	for i, sp := range fetchSpans {
		protoSpans[i] = rangeSpanToProto(sp.span)
	}

	stream, err := s.client.FetchEntries(ctx, &FetchEntriesRequest{
		RelationshipId:  s.relationshipID,
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
	maxRPCBytes := s.reconcileMaxRPCBytes
	if maxRPCBytes == 0 {
		maxRPCBytes = drDefaultReconcileMaxRPCBytes
	}
	remoteKIDs := make(map[[32]byte]struct{})
	seenRemoteKIDs := make(map[[32]byte]struct{})
	accumulators := make([]drFetchedRangeAccumulator, len(fetchSpans))

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			result.err = fmt.Errorf("fetch stream recv error: %w", err)
			return result
		}
		if batch == nil {
			result.err = fmt.Errorf("fetch stream returned nil batch")
			return result
		}
		if err := s.assertActiveCheckpoint(batch.GetCheckpointId(), batch.GetCheckpointIndex()); err != nil {
			result.err = fmt.Errorf("checkpoint conflict: %w", err)
			return result
		}
		if len(batch.GetFailedKids()) > 0 {
			result.err = fmt.Errorf("fetch stream returned failed_kids: %d", len(batch.GetFailedKids()))
			return result
		}
		rpcBytes += entryBatchWireBytes(batch)
		if rpcBytes > maxRPCBytes {
			result.err = fmt.Errorf("budget_exceeded: fetch stream bytes exceeded (%d > %d)", rpcBytes, maxRPCBytes)
			return result
		}
		for _, e := range batch.Entries {
			kid, vid, err := s.kidVIDFromFetchedChange(e)
			if err != nil {
				result.err = err
				return result
			}
			if _, ok := seenRemoteKIDs[kid]; ok {
				result.err = fmt.Errorf("fetch stream returned duplicate kid %x", kid)
				return result
			}
			spanIndex, err := fetchSpanIndex(fetchSpans, kid)
			if err != nil {
				result.err = err
				return result
			}
			accumulators[spanIndex].add(kid, vid)
			remoteKIDs[kid] = struct{}{}
			seenRemoteKIDs[kid] = struct{}{}
			result.fetchedEntries = append(result.fetchedEntries, cloneEntryChange(e))
		}
	}
	result.rpcBytes = rpcBytes

	if err := validateFetchedRangeProofs(fetchSpans, accumulators); err != nil {
		result.err = err
		return result
	}

	removedKeys, err := localOnlyKeysForSpans(localIndex, kidToKey, rangeSpansFromFetchSpans(fetchSpans), remoteKIDs)
	if err != nil {
		result.err = err
		return result
	}
	result.removedKeys = removedKeys

	return result
}

func (s *drReplicationSecondary) kidFromEntryChange(change *EntryChange) ([32]byte, bool) {
	var kid [32]byte
	if change == nil {
		return kid, false
	}
	if len(change.Kid) == 32 {
		copy(kid[:], change.Kid)
		return kid, true
	}
	if change.Key != "" {
		return s.scanner.ComputeKID(change.Key), true
	}
	return kid, false
}

func (s *drReplicationSecondary) kidVIDFromFetchedChange(change *EntryChange) ([32]byte, [32]byte, error) {
	var vid [32]byte
	kid, ok := s.kidFromEntryChange(change)
	if !ok {
		return kid, vid, fmt.Errorf("fetch stream returned entry without resolvable kid")
	}
	switch physical.Operation(change.OpType) {
	case physical.PutOperation:
		vid = s.scanner.ComputeVIDWithSealWrap(change.Value, change.SealWrap)
		return kid, vid, nil
	case physical.DeleteOperation:
		if change.Key == "" {
			return kid, vid, fmt.Errorf("fetch stream returned kid-only delete that cannot be range-proofed: kid=%x", kid)
		}
		_, vid = s.scanner.ComputeItemFromEntry(&physical.Entry{Key: change.Key})
		return kid, vid, nil
	default:
		return kid, vid, fmt.Errorf("unknown operation type in fetched change: op_type=%s key=%s", change.OpType, change.Key)
	}
}

func (a *drFetchedRangeAccumulator) add(kid, vid [32]byte) {
	a.count++

	kidHash := sha256.Sum256(kid[:])
	xorHash32(&a.xorKeyHash, kidHash)

	h := sha256.New()
	h.Write(kid[:])
	h.Write(vid[:])
	var elem [32]byte
	copy(elem[:], h.Sum(nil))
	xorHash32(&a.xorValueHash, elem)
}

func validateFetchedRangeProofs(fetchSpans []drFetchSpan, accumulators []drFetchedRangeAccumulator) error {
	if len(fetchSpans) != len(accumulators) {
		return fmt.Errorf("fetch proof validation invariant failed: spans=%d accumulators=%d", len(fetchSpans), len(accumulators))
	}
	for i, fetchSpan := range fetchSpans {
		acc := accumulators[i]
		proof := fetchSpan.proof
		if proof.hasDigest {
			if acc.count != proof.count || acc.xorKeyHash != proof.xorKeyHash || acc.xorValueHash != proof.xorValueHash {
				return fmt.Errorf("fetch digest proof mismatch: span_depth=%d fetched_count=%d expected_count=%d", fetchSpan.span.SplitDepth, acc.count, proof.count)
			}
			continue
		}
		return fmt.Errorf("missing fetch completeness proof for span depth=%d", fetchSpan.span.SplitDepth)
	}
	return nil
}

func fetchSpanIndex(fetchSpans []drFetchSpan, kid [32]byte) (int, error) {
	found := -1
	for i, fetchSpan := range fetchSpans {
		if !fetchSpan.span.Contains(kid) {
			continue
		}
		if found >= 0 {
			return -1, fmt.Errorf("fetch stream returned kid %x matching multiple spans", kid)
		}
		found = i
	}
	if found < 0 {
		return -1, fmt.Errorf("fetch stream returned kid %x outside requested spans", kid)
	}
	return found, nil
}

func rangeSpansFromFetchSpans(fetchSpans []drFetchSpan) []reconciler.RangeSpan {
	spans := make([]reconciler.RangeSpan, len(fetchSpans))
	for i, fetchSpan := range fetchSpans {
		spans[i] = fetchSpan.span
	}
	return spans
}

func xorHash32(dst *[32]byte, src [32]byte) {
	for i := 0; i < 32; i++ {
		dst[i] ^= src[i]
	}
}

func localOnlyKeysForSpans(localIndex *reconciler.RangeMapIndex, kidToKey map[[32]byte]string, spans []reconciler.RangeSpan, remoteKIDs map[[32]byte]struct{}) ([]string, error) {
	if localIndex == nil || len(spans) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for _, span := range spans {
		for _, kid := range localIndex.RangeKeys(span) {
			if _, ok := remoteKIDs[kid]; ok {
				continue
			}
			key, ok := kidToKey[kid]
			if !ok || key == "" {
				return nil, fmt.Errorf("local-only kid %x in reconcile span has no key mapping", kid)
			}
			if isDRNeverReplicatePath(key) {
				continue
			}
			seen[key] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
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
	return s.applyRemovedKeysWithLocalKIDIndexMode(ctx, checkpointID, checkpointIndex, keys, false)
}

func (s *drReplicationSecondary) applyRemovedKeysPreservingLocalKIDIndex(ctx context.Context, checkpointID string, checkpointIndex uint64, keys []string) error {
	return s.applyRemovedKeysWithLocalKIDIndexMode(ctx, checkpointID, checkpointIndex, keys, true)
}

func (s *drReplicationSecondary) applyRemovedKeysWithLocalKIDIndexMode(ctx context.Context, checkpointID string, checkpointIndex uint64, keys []string, preserveLocalKIDIndex bool) error {
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
	if err := s.clearPersistedReconcileOptimizationState(ctx, preserveLocalKIDIndex); err != nil {
		return err
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

			for attempt := 0; ; attempt++ {
				if err := ctx.Err(); err != nil {
					return err
				}
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
					if errors.Is(err, physical.ErrTransactionCommitFailure) && attempt < drDefaultReconcileTxnCommitRetries {
						s.logger.Debug("retrying DR reconcile delete transaction after commit conflict",
							"keys", len(batch),
							"attempt", attempt+1,
							"max_attempts", drDefaultReconcileTxnCommitRetries,
							"error", err)
						delay := time.Duration(attempt+1) * drDefaultReconcileTxnCommitRetryDelay
						timer := time.NewTimer(delay)
						select {
						case <-ctx.Done():
							timer.Stop()
							return ctx.Err()
						case <-timer.C:
						}
						continue
					}
					return fmt.Errorf("commit delete txn (%d keys): %w", len(batch), err)
				}
				break
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

func (s *drReplicationSecondary) fetchRemoteDigestSpans(ctx context.Context, checkpoint *CheckpointResponse, parentSpan reconciler.RangeSpan) ([]drFetchSpan, error) {
	rpcCtx, cancel := s.rpcContext(ctx)
	resp, err := s.client.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
		RelationshipId:  s.relationshipID,
		CheckpointId:    checkpoint.CheckpointId,
		CheckpointIndex: checkpoint.CommitIndex,
		ParentSpan:      rangeSpanToProto(parentSpan),
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("digest RPC failed for span depth=%d: %w", parentSpan.SplitDepth, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("invalid digest response: nil response for span depth=%d", parentSpan.SplitDepth)
	}
	if len(resp.GetDigests()) == 0 {
		return nil, fmt.Errorf("invalid digest response: no digests for span depth=%d", parentSpan.SplitDepth)
	}
	return fetchSpansFromDigestResponse(parentSpan, resp.GetDigests())
}

// runRangeDrillDown performs fine-grained drill-down on a mismatched
// range by recursively splitting it via ExchangeRangeDigests until the
// diff is narrowed to the smallest mismatched sub-ranges. Returns the
// list of sub-range spans that need fetching. Drill-down is mandatory for
// mismatched ranges: transport failures and malformed digest content both
// return an error because they cannot safely narrow the fetch scope.
func (s *drReplicationSecondary) runRangeDrillDown(
	ctx context.Context,
	checkpoint *CheckpointResponse,
	localIndex *reconciler.RangeMapIndex,
	parentSpan reconciler.RangeSpan,
) ([]drFetchSpan, error) {
	// Work queue of spans to drill into.
	pending := []drFetchSpan{{span: parentSpan}}
	var mismatched []drFetchSpan
	splits := 0

	for len(pending) > 0 && splits < drRangeMaxSessionSplits {
		item := pending[0]
		span := item.span
		pending = pending[1:]

		// Check depth limit.
		if span.SplitDepth >= uint32(drRangeMaxSplitDepth) {
			mismatched = append(mismatched, item)
			continue
		}

		rpcCtx, cancel := s.rpcContext(ctx)
		resp, err := s.client.ExchangeRangeDigests(rpcCtx, &RangeDigestRequest{
			RelationshipId:  s.relationshipID,
			CheckpointId:    checkpoint.CheckpointId,
			CheckpointIndex: checkpoint.CommitIndex,
			ParentSpan:      rangeSpanToProto(span),
		})
		cancel()

		if err != nil {
			return nil, fmt.Errorf("drill-down RPC failed for span depth=%d: %w", span.SplitDepth, err)
		}
		if resp == nil {
			return nil, fmt.Errorf("invalid drill-down response: nil response for span depth=%d", span.SplitDepth)
		}
		if len(resp.GetDigests()) == 0 {
			return nil, fmt.Errorf("invalid drill-down response: no digests for span depth=%d", span.SplitDepth)
		}

		children, err := fetchSpansFromDigestResponse(span, resp.GetDigests())
		if err != nil {
			return nil, fmt.Errorf("invalid drill-down response: %w", err)
		}

		for _, child := range children {
			// Compute local RangeDescriptor for this sub-range.
			localDesc := reconciler.BuildRangeDigestFromIndex(localIndex, child.span)

			// Phase B authoritative equality: compare (count, XORKeyHash,
			// XORValueHash). These are 256-bit SHA256-XOR accumulators,
			// making false-equality cryptographically negligible (~2^-256).
			if localDesc.Count == child.proof.count &&
				localDesc.XORKeyHash == child.proof.xorKeyHash &&
				localDesc.XORValueHash == child.proof.xorValueHash {
				// Sub-range matches at RangeDescriptor level -- skip.
				continue
			}

			splits++
			// Sub-range mismatched -- drill deeper or mark for fetch.
			if child.span.SplitDepth < uint32(drRangeMaxSplitDepth) && child.proof.count > 1 {
				pending = append(pending, child)
			} else {
				mismatched = append(mismatched, child)
			}
		}
	}

	// Drain any remaining pending spans as mismatched (hit split limit).
	mismatched = append(mismatched, pending...)

	if len(mismatched) == 0 {
		return mismatched, nil // Empty -- ranges converged during drill-down.
	}

	s.logger.Info("drill-down complete",
		"original_span_depth", parentSpan.SplitDepth,
		"mismatched_sub_ranges", len(mismatched),
		"total_splits", splits)
	return mismatched, nil
}

func fetchSpansFromDigestResponse(parent reconciler.RangeSpan, digests []*RangeDigest) ([]drFetchSpan, error) {
	if !parent.Valid() {
		return nil, fmt.Errorf("invalid parent span")
	}
	fetchSpans := make([]drFetchSpan, 0, len(digests))
	for _, digest := range digests {
		remoteSpan, _, err := protoToRangeSpan(digest.GetSpan())
		if err != nil {
			return nil, err
		}
		if !remoteSpan.Valid() {
			return nil, fmt.Errorf("invalid span bounds")
		}
		if bytes.Compare(remoteSpan.StartKID[:], parent.StartKID[:]) < 0 ||
			bytes.Compare(remoteSpan.EndKID[:], parent.EndKID[:]) > 0 {
			return nil, fmt.Errorf("digest span is outside parent span")
		}
		proof, err := rangeFetchProofFromDigest(digest)
		if err != nil {
			return nil, err
		}
		fetchSpans = append(fetchSpans, drFetchSpan{span: remoteSpan, proof: proof})
	}
	if len(fetchSpans) == 0 {
		return nil, fmt.Errorf("no digests")
	}

	sort.Slice(fetchSpans, func(i, j int) bool {
		if cmp := bytes.Compare(fetchSpans[i].span.StartKID[:], fetchSpans[j].span.StartKID[:]); cmp != 0 {
			return cmp < 0
		}
		return bytes.Compare(fetchSpans[i].span.EndKID[:], fetchSpans[j].span.EndKID[:]) < 0
	})

	if !bytes.Equal(fetchSpans[0].span.StartKID[:], parent.StartKID[:]) {
		return nil, fmt.Errorf("digest coverage gap at parent start")
	}

	_, _, parentCanSplit := reconciler.SplitRange(parent)
	for i, fetchSpan := range fetchSpans {
		if fetchSpan.span == parent && fetchSpan.span.SplitDepth == parent.SplitDepth {
			if parentCanSplit {
				return nil, fmt.Errorf("digest span did not split splittable parent")
			}
		} else if fetchSpan.span.SplitDepth <= parent.SplitDepth {
			return nil, fmt.Errorf("digest span did not advance split depth")
		}

		if i == 0 {
			continue
		}
		expectedStart, carry := nextKID(fetchSpans[i-1].span.EndKID)
		if carry {
			return nil, fmt.Errorf("digest coverage overlaps after terminal kid")
		}
		if !bytes.Equal(expectedStart[:], fetchSpan.span.StartKID[:]) {
			return nil, fmt.Errorf("digest coverage gap or overlap")
		}
	}

	last := fetchSpans[len(fetchSpans)-1].span
	if !bytes.Equal(last.EndKID[:], parent.EndKID[:]) {
		return nil, fmt.Errorf("digest coverage gap at parent end")
	}
	return fetchSpans, nil
}

func rangeFetchProofFromDigest(digest *RangeDigest) (drRangeFetchProof, error) {
	var proof drRangeFetchProof
	if digest == nil {
		return proof, fmt.Errorf("nil digest")
	}
	keyHash := digest.GetXorKeyHash()
	valueHash := digest.GetXorValueHash()
	if len(keyHash) != 32 {
		return proof, fmt.Errorf("invalid xor_key_hash length %d", len(keyHash))
	}
	if len(valueHash) != 32 {
		return proof, fmt.Errorf("invalid xor_value_hash length %d", len(valueHash))
	}
	proof.hasDigest = true
	proof.count = digest.GetCount()
	copy(proof.xorKeyHash[:], keyHash)
	copy(proof.xorValueHash[:], valueHash)
	return proof, nil
}

func nextKID(kid [32]byte) ([32]byte, bool) {
	out := kid
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			return out, false
		}
	}
	return out, true
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
