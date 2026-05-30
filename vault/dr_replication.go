// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc64"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/hashicorp/go-uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/openbao/openbao/physical/raft"
	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drCheckpointGlobalBudgetBytes          = 1024 << 20 // 1 GiB
	drCheckpointPerRelationshipBudgetBytes = 256 << 20  // 256 MiB
	drCheckpointMaxPerRelationship         = 8
	drCheckpointTTL                        = 30 * time.Minute
	drRangeMaxIBLTCellsPerRange            = 32768
	drStreamBufferMaxEntries               = 50000
	drStreamBufferMaxBytes                 = 256 << 20 // 256 MiB
	drCheckpointThrottleBufferPct          = 85
	drCheckpointLaggingActiveWindow        = 10 * time.Second
	drCheckpointForceBuildInterval         = 15 * time.Second
	drCheckpointIndexFullScanInterval      = 4 * time.Hour
	drCheckpointBuildMaxInFlight           = 1

	drDirtyBitmapSize            = 1024
	drDirtyBitmapBytes           = drDirtyBitmapSize / 8
	drDirtyBitmapStoragePath     = "core/dr-replication/dirty-bitmap"
	drDirtyBitmapPersistInterval = 5 * time.Second

	drDefaultInitialWindow = 4096             // default credit window if secondary omits it
	drCreditWaitTimeout    = 30 * time.Second // how long primary waits for credits before cancelling subscriber

	drStreamSendBatchMaxEntries = 64      // max entries per EntryBatch on the stream
	drStreamSendBatchMaxBytes   = 1 << 20 // 1 MiB max payload per stream batch
	drFetchRequestMaxSelectors  = 16384   // max point selectors (kids + items) per FetchEntries request
	drFetchSendBatchMaxEntries  = 100     // max entries per FetchEntries response batch
	drFetchSendBatchMaxBytes    = 8 << 20 // 8 MiB max approximate FetchEntries response batch payload

	drBackpressureDefaultEnabled        = true
	drBackpressureDefaultDegradedRatio  = 0.80
	drBackpressureDefaultCriticalRatio  = 0.50
	drBackpressureDefaultHorizonSeconds = int64(180)
	drBackpressureDefaultDegradedMinQPS = int64(50)
	drBackpressureDefaultCriticalMinQPS = int64(10)
)

type drBackpressureState uint32

const (
	drBackpressureHealthy drBackpressureState = iota
	drBackpressureDegraded
	drBackpressureCritical
)

func (s drBackpressureState) String() string {
	switch s {
	case drBackpressureDegraded:
		return "degraded"
	case drBackpressureCritical:
		return "critical"
	default:
		return "healthy"
	}
}

type drSecondaryPressureSample struct {
	lastSeen     time.Time
	lastApplied  uint64
	lastPrimary  uint64
	applyRateEPS float64
	lagEntries   uint64
}

// drChangeStreamDispatcher is a two-layer change stream hook that ensures
// the stream journal is maintained on ALL primary-cluster Raft nodes (leader
// and followers alike). The base layer filters non-replicable paths and
// appends entries to the on-disk journal. When the node is the active
// primary leader an optional second layer delegates to the primary's
// processing logic (ring buffer, subscriber fan-out, dirty bitmap, index).
//
// This design guarantees that after a leader election the new leader already
// has a populated journal covering its follower period, eliminating the need
// for expensive reconciliation when secondaries reconnect.
type drChangeStreamDispatcher struct {
	mu      sync.RWMutex
	journal *drStreamJournal
	primary *drReplicationPrimary // nil when not the active primary leader
	logger  log.Logger
}

// OnChange is the FSM change stream callback. It runs on every Raft node.
func (d *drChangeStreamDispatcher) OnChange(entries []physical.ChangeStreamEntry) {
	// 1. Filter non-replicable paths (always, on all nodes).
	replicable := filterDRReplicableEntries(entries)

	// 2. Append to journal (always, on all nodes).
	if d.journal != nil && len(replicable) > 0 {
		if err := d.journal.append(replicable); err != nil {
			d.logger.Warn("journal append failed", "error", err)
		}
	}

	// 3. Delegate to primary-specific processing (only when active leader).
	d.mu.RLock()
	primary := d.primary
	d.mu.RUnlock()
	if primary != nil {
		primary.onChangePrimaryPath(entries, replicable)
	}
}

// setPrimary attaches the primary processing layer. Must be called when
// this node becomes the active primary leader.
func (d *drChangeStreamDispatcher) setPrimary(p *drReplicationPrimary) {
	d.mu.Lock()
	d.primary = p
	d.mu.Unlock()
}

// clearPrimary detaches the primary processing layer. Must be called when
// this node steps down from primary leadership.
func (d *drChangeStreamDispatcher) clearPrimary() {
	d.mu.Lock()
	d.primary = nil
	d.mu.Unlock()
}

// filterDRReplicableEntries returns a subset of entries that excludes
// cluster-local keys which are never replicated (e.g. core/raft/*,
// core/dr-replication/*).
func filterDRReplicableEntries(entries []physical.ChangeStreamEntry) []physical.ChangeStreamEntry {
	replicable := make([]physical.ChangeStreamEntry, 0, len(entries))
	for _, e := range entries {
		if isDRNeverReplicatePath(e.Key) {
			continue
		}
		replicable = append(replicable, e)
	}
	return replicable
}

// drReplicationPrimary implements the DRReplicationServer gRPC interface
// on the primary side. It provides:
//   - Change streaming (normal-mode replication)
//   - Dirty-bitmap + ordered hash-stream reconciliation (recovery mode)
//   - Fine-grained range drill-down for efficient diff narrowing
//   - Entry fetching for divergent keys
type drReplicationPrimary struct {
	UnimplementedDRReplicationServer

	logger  log.Logger
	scanner *reconciler.Scanner
	core    *Core

	// subscribers tracks active change stream subscribers.
	mu          sync.RWMutex
	subscribers map[string]*changeStreamSubscriber

	// syncKeyringMu keeps bootstrap keyring sync single-shot per registered
	// relationship by serializing authorize-wrap-activate.
	syncKeyringMu sync.Mutex

	// changeBuffer is a ring buffer of recent changes for stream
	// catch-up. Protected by bufMu.
	bufMu        sync.RWMutex
	changeBuffer []physical.ChangeStreamEntry
	bufMaxSize   int
	bufMaxBytes  uint64
	bufBytes     uint64

	// metrics
	entriesDropped atomic.Uint64
	scanFailures   atomic.Uint64
	// heartbeatMissingDRLeafWarned suppresses repeated warning spam when
	// heartbeats cannot include the active DR transport leaf certificate.
	heartbeatMissingDRLeafWarned atomic.Bool
	// streamLaggingSubscribers counts forced disconnects due to subscriber lag.
	streamLaggingSubscribers atomic.Uint64
	// streamLaggingSubscribersActive tracks recent lagging pressure in a short window.
	streamLaggingSubscribersActive atomic.Uint64
	streamLaggingLastEventUnix     atomic.Int64

	checkpointMu                     sync.RWMutex
	checkpoints                      map[string]*drCheckpointCacheEntry
	checkpointTTL                    time.Duration
	maxCheckpoints                   int
	checkpointBytes                  uint64
	checkpointMetaBytes              uint64
	checkpointValueBytes             uint64
	checkpointManifestBytes          uint64
	checkpointDerivedBytes           uint64
	checkpointEvictions              atomic.Uint64
	checkpointAdmissionFailures      atomic.Uint64
	checkpointThrottleTotal          atomic.Uint64
	checkpointBuildAdmissionFailures atomic.Uint64
	checkpointGlobalBudget           uint64
	checkpointPerRelationshipBudget  uint64
	maxCheckpointsPerRelationship    int
	rangePlanConfig                  reconciler.RangePlanConfig
	// checkpointBuildInFlight de-amplifies parallel checkpoint requests
	// for the same relationship. checkpointBuildMaxInFlight bounds
	// concurrent full checkpoint builders across relationships.
	checkpointBuildInFlight        map[string]*drCheckpointBuildResult
	checkpointBuildMaxInFlight     int
	latestCheckpointByRelationship map[string]string
	activeCheckpointRefs           map[string]int
	// checkpointLastForcedBuildByRelationship prevents permanent reconcile
	// starvation under sustained stream pressure by allowing occasional builds.
	checkpointLastForcedBuildByRelationship map[string]time.Time

	revokedStreamsTerminated      atomic.Uint64
	checkpointThrottleBypassTotal atomic.Uint64

	writeRateMu            sync.Mutex
	writeRateWindowStart   time.Time
	writeRateWindowEntries uint64
	writeRateEPS           float64

	indexMu            sync.RWMutex
	indexInitialized   bool
	indexLastFullScan  time.Time
	indexKIDToVID      map[[32]byte][32]byte
	indexKIDToKey      map[[32]byte]string
	indexResyncSkipped atomic.Uint64
	indexApplied       atomic.Uint64
	// indexReplicable tracks the highest RaftIndex of any entry that
	// passed the isDRNeverReplicatePath filter and was pushed to
	// subscribers. Used by the Heartbeat RPC so the secondary can
	// compute lag against the replicable watermark instead of the
	// raw Raft applied index, avoiding phantom lag from non-replicated
	// internal entries (core/raft/*, core/dr-replication/*, etc.).
	indexReplicable atomic.Uint64

	streamJournal *drStreamJournal

	checkpointArtifacts *drCheckpointArtifactStore
	tombstoneGC         *drTombstoneGC

	// Journal replay health counters.
	journalReplayAttempts atomic.Uint64
	journalReplaySuccess  atomic.Uint64
	journalRangeTooOld    atomic.Uint64

	pressureMu             sync.Mutex
	secondaryPressure      map[string]*drSecondaryPressureSample
	backpressureEnabled    bool
	backpressureState      atomic.Uint32
	backpressureDegraded   float64
	backpressureCritical   float64
	backpressureMinLag     uint64
	backpressureHorizonSec int64
	backpressureMinQPSDeg  int64
	backpressureMinQPSCrit int64
	backpressureWindowSec  int64
	backpressureCount      int64
	backpressureCapQPS     atomic.Int64
	backpressureRejected   atomic.Uint64

	// dirty tracking
	dirtyMapMu          sync.RWMutex
	dirtyMap            []byte
	dirtyMapStart       uint64
	dirtyMapLastPersist time.Time

	// creditWaitTimeout is how long the primary waits for credit
	// replenishment before cancelling a subscriber. Defaults to
	// drCreditWaitTimeout; overridable for tests.
	creditWaitTimeout time.Duration
}

type drCheckpointBuildResult struct {
	done chan struct{}
	resp *CheckpointResponse
	err  error
}

type drCheckpointCacheEntry struct {
	checkpoint       reconciler.Checkpoint
	relationshipID   string
	createdAt        time.Time
	estimatedBytes   uint64
	metaBytes        uint64
	valueBytes       uint64
	manifestBytes    uint64
	kidToKey         map[[32]byte]string
	kidToVID         map[[32]byte][32]byte
	rangePlanVersion uint32
}

// changeStreamSubscriber represents a connected secondary that is
// receiving change stream updates.
type changeStreamSubscriber struct {
	id             string
	relationshipID string
	ch             chan physical.ChangeStreamEntry
	cancel         context.CancelFunc

	// Credit-based flow control. The secondary sends an initial window
	// and periodically replenishes credits via WindowUpdate messages.
	// The primary decrements credits on each send; when exhausted it
	// blocks until credits arrive or drCreditWaitTimeout fires.
	credits  atomic.Int64
	creditCh chan struct{} // signaled (non-blocking) when credits arrive
}

// NewDRReplicationPrimary creates a new DR replication gRPC server for
// the primary side.  The journal parameter is the shared stream journal
// maintained by the drChangeStreamDispatcher; if nil a local journal is
// created (useful for tests).
func NewDRReplicationPrimary(core *Core, replSalt []byte, logger log.Logger, journal *drStreamJournal) *drReplicationPrimary {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true // Primary needs reverse KID -> key mapping
	// Use metadata-first checkpoints to avoid retaining full values in cache.
	config.BuildEntryMap = false
	config.RequireTransactionalSnapshot = true
	config.ValueDomain = reconciler.ValueDomainCiphertext
	config.Logger = logger.Named("reconciler")
	config.ExcludePaths = map[string]bool{
		"core/keyring":                 true,
		"core/hsm/barrier-unseal-keys": true,
		"core/seal-config":             true,
		"core/local-mounts":            true,
		"core/local-auth":              true,
		"core/local-audit":             true,
	}
	// Keep reconciliation path filtering symmetric between primary and
	// secondary so set comparisons are stable.
	config.ExcludePathFunc = isDRReconcileExcludedPath

	primary := &drReplicationPrimary{
		logger:                                  logger.Named("dr-replication"),
		scanner:                                 reconciler.NewScanner(config),
		core:                                    core,
		subscribers:                             make(map[string]*changeStreamSubscriber),
		changeBuffer:                            make([]physical.ChangeStreamEntry, 0, drStreamBufferMaxEntries),
		bufMaxSize:                              drStreamBufferMaxEntries,
		bufMaxBytes:                             drStreamBufferMaxBytes,
		checkpoints:                             make(map[string]*drCheckpointCacheEntry),
		checkpointTTL:                           drCheckpointTTL,
		maxCheckpoints:                          16,
		checkpointGlobalBudget:                  drCheckpointGlobalBudgetBytes,
		checkpointPerRelationshipBudget:         drCheckpointPerRelationshipBudgetBytes,
		maxCheckpointsPerRelationship:           drCheckpointMaxPerRelationship,
		rangePlanConfig:                         reconciler.DefaultRangePlanConfig(),
		checkpointBuildInFlight:                 make(map[string]*drCheckpointBuildResult),
		checkpointBuildMaxInFlight:              drCheckpointBuildMaxInFlight,
		latestCheckpointByRelationship:          make(map[string]string),
		activeCheckpointRefs:                    make(map[string]int),
		checkpointLastForcedBuildByRelationship: make(map[string]time.Time),
		indexKIDToVID:                           make(map[[32]byte][32]byte),
		indexKIDToKey:                           make(map[[32]byte]string),
		streamJournal:                           journal,
		checkpointArtifacts:                     newDRCheckpointArtifactStore(logger, ""),
		secondaryPressure:                       make(map[string]*drSecondaryPressureSample),
		backpressureEnabled:                     drBackpressureDefaultEnabled,
		backpressureDegraded:                    drBackpressureDefaultDegradedRatio,
		backpressureCritical:                    drBackpressureDefaultCriticalRatio,
		backpressureHorizonSec:                  drBackpressureDefaultHorizonSeconds,
		backpressureMinQPSDeg:                   drBackpressureDefaultDegradedMinQPS,
		backpressureMinQPSCrit:                  drBackpressureDefaultCriticalMinQPS,
		dirtyMap:                                make([]byte, drDirtyBitmapBytes),
		creditWaitTimeout:                       drCreditWaitTimeout,
	}
	// If no shared journal was provided (e.g. unit tests), create a local one.
	if primary.streamJournal == nil {
		primary.streamJournal = newDRStreamJournal(logger, "")
		if err := primary.streamJournal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
			primary.logger.Warn("failed to initialize DR stream journal", "error", err)
		}
	}
	primary.checkpointArtifacts.configure(true, drCheckpointArtifactDefaultTTL, drCheckpointArtifactDefaultGlobalBudget, drCheckpointArtifactDefaultPerRelBudget, drCheckpointArtifactDefaultSegmentBytes)
	primary.tombstoneGC = newDRTombstoneGC(primary, logger)
	primary.tombstoneGC.Start()
	return primary
}

// SeedAppliedIndex sets the baseline applied index so the checkpoint
// fence does not wait for Raft entries that were committed before the
// change stream hook was registered.  It must be called exactly once
// after the hook is wired and before any checkpoint requests are served.
// The value is only stored when it advances the current watermark (i.e.
// it never moves the counter backwards).
func (s *drReplicationPrimary) SeedAppliedIndex(idx uint64) {
	if idx == 0 {
		return
	}
	for {
		cur := s.indexApplied.Load()
		if cur >= idx {
			return
		}
		if s.indexApplied.CompareAndSwap(cur, idx) {
			s.logger.Info("seeded indexApplied from Raft applied index", "index", idx)
			return
		}
	}
}

// OnChange is a convenience wrapper that filters entries and delegates
// to onChangePrimaryPath. It exists so tests and callers that don't use
// the dispatcher can still call primary.OnChange(entries) directly.
func (s *drReplicationPrimary) OnChange(entries []physical.ChangeStreamEntry) {
	replicable := filterDRReplicableEntries(entries)

	// When called directly (not via dispatcher), also append to journal.
	if s.streamJournal != nil && len(replicable) > 0 {
		if err := s.streamJournal.append(replicable); err != nil {
			s.logger.Warn("failed to append stream journal entries", "error", err)
		}
	}

	s.onChangePrimaryPath(entries, replicable)
}

// onChangePrimaryPath performs primary-specific processing: index
// tracking, dirty bitmap updates, ring buffer append, write-rate
// tracking, and subscriber fan-out. Journal append is NOT done here
// because the drChangeStreamDispatcher has already written to the
// journal before calling this method.
func (s *drReplicationPrimary) onChangePrimaryPath(entries []physical.ChangeStreamEntry, replicableEntries []physical.ChangeStreamEntry) {
	// Always advance the applied-index watermark from the raw (unfiltered)
	// entries so the checkpoint fence sees every Raft index we have
	// observed, including entries for non-replicated paths (e.g.
	// core/dr-replication/*, core/raft/*).  Without this, batches that
	// contain only excluded paths would leave indexApplied behind the
	// true Raft applied index and cause the fence to time out.
	if len(entries) > 0 {
		last := entries[len(entries)-1].RaftIndex
		if last > 0 {
			s.indexApplied.Store(last)
		}
	}

	if len(replicableEntries) == 0 {
		// All entries in this batch were non-replicable. Inject an
		// index-advance marker into each subscriber so the secondary
		// can advance its lastAppliedIndex and avoid phantom lag.
		// The marker uses an empty key which the secondary recognises
		// as a noop (no storage write, just index advancement).
		if len(entries) > 0 {
			marker := physical.ChangeStreamEntry{
				RaftIndex: entries[len(entries)-1].RaftIndex,
				// Empty Key signals an index-advance marker.
			}
			s.mu.RLock()
			for _, sub := range s.subscribers {
				select {
				case sub.ch <- marker:
				default:
					// Channel full; subscriber will catch up via
					// normal reconnect/reconcile. Don't cancel here
					// for a lightweight marker.
				}
			}
			s.mu.RUnlock()
		}
		return
	}

	// Track the highest replicable Raft index for accurate lag
	// reporting in the Heartbeat RPC.
	lastReplicable := replicableEntries[len(replicableEntries)-1].RaftIndex
	if lastReplicable > 0 {
		s.indexReplicable.Store(lastReplicable)
	}

	// Mark dirty ranges.
	s.dirtyMapMu.Lock()
	if s.dirtyMapStart == 0 && len(replicableEntries) > 0 {
		s.dirtyMapStart = replicableEntries[0].RaftIndex
	}
	for _, e := range replicableEntries {
		kid := s.scanner.ComputeKID(e.Key)
		// RangeID = first 10 bits of KID
		rangeID := (uint64(kid[0]) << 2) | (uint64(kid[1]) >> 6)
		byteIdx := rangeID / 8
		bitIdx := rangeID % 8
		s.dirtyMap[byteIdx] |= (1 << bitIdx)
	}
	s.maybePersistDirtyBitmap()
	s.dirtyMapMu.Unlock()

	// Keep metadata index in sync with committed changes.
	s.updateIndexFromChanges(replicableEntries)

	// Append to ring buffer.
	now := time.Now().UTC()
	s.bufMu.Lock()
	for _, e := range replicableEntries {
		s.changeBuffer = append(s.changeBuffer, e)
		s.bufBytes += entryChangeBytes(e)
	}
	for len(s.changeBuffer) > 0 && (len(s.changeBuffer) > s.bufMaxSize || s.bufBytes > s.bufMaxBytes) {
		dropped := s.changeBuffer[0]
		s.changeBuffer = s.changeBuffer[1:]
		dropBytes := entryChangeBytes(dropped)
		if dropBytes <= s.bufBytes {
			s.bufBytes -= dropBytes
		} else {
			s.bufBytes = 0
		}
	}
	metrics.SetGauge([]string{"replication", "dr", "stream", "entries_buffered"}, float32(len(s.changeBuffer)))
	metrics.SetGauge([]string{"replication", "dr", "stream", "buffer_bytes"}, float32(s.bufBytes))
	s.bufMu.Unlock()

	// Update a rolling write-rate estimate used to expose stream horizon.
	s.writeRateMu.Lock()
	if s.writeRateWindowStart.IsZero() {
		s.writeRateWindowStart = now
	}
	s.writeRateWindowEntries += uint64(len(replicableEntries))
	window := now.Sub(s.writeRateWindowStart)
	if window >= 5*time.Second {
		instant := float64(s.writeRateWindowEntries) / window.Seconds()
		if s.writeRateEPS <= 0 {
			s.writeRateEPS = instant
		} else {
			// Light smoothing to keep status stable while responsive.
			s.writeRateEPS = (s.writeRateEPS * 0.6) + (instant * 0.4)
		}
		s.writeRateWindowStart = now
		s.writeRateWindowEntries = 0
	}
	s.writeRateMu.Unlock()

	// Fan out to subscribers with backpressure.
	s.mu.RLock()
	metrics.SetGauge([]string{"replication", "dr", "stream", "subscribers"}, float32(len(s.subscribers)))
	var laggingSubs []string
	for _, sub := range s.subscribers {
		for _, e := range replicableEntries {
			select {
			case sub.ch <- e:
				// Sent successfully.
			default:
				// Channel full. Rather than silently dropping, cancel
				// the subscriber so it reconnects and reconciles.
				s.logger.Warn("subscriber lagging, forcing reconnection",
					"subscriber", sub.id)
				sub.cancel()
				laggingSubs = append(laggingSubs, sub.id)
				s.entriesDropped.Add(1)
				s.streamLaggingSubscribers.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "stream", "entries_dropped"}, 1)
				metrics.IncrCounter([]string{"replication", "dr", "stream", "lagging_subscribers"}, 1)
				break
			}
		}
	}
	s.mu.RUnlock()

	// Remove lagging subscribers outside the RLock.
	if len(laggingSubs) > 0 {
		now := time.Now().UTC()
		s.mu.Lock()
		for _, id := range laggingSubs {
			delete(s.subscribers, id)
		}
		s.mu.Unlock()
		s.streamLaggingSubscribersActive.Store(uint64(len(laggingSubs)))
		s.streamLaggingLastEventUnix.Store(now.Unix())
	}
}

// StreamChanges implements DRReplicationServer.StreamChanges.
// requireActiveNode checks whether this node is the active leader.
// If it is, it returns nil. If it is a standby, it looks up the
// current leader's cluster address and returns a gRPC Unavailable
// status with a DRRedirectDetail so the secondary can reconnect to
// the actual leader. This allows secondaries to discover the new
// leader after a stepdown without requiring a load balancer.
func (s *drReplicationPrimary) requireActiveNode() error {
	if !s.core.standby.Load() {
		return nil // we are the active node
	}
	// Look up the current leader's cluster address.
	_, _, clusterAddr, err := s.core.Leader()
	if err != nil || clusterAddr == "" {
		return status.Errorf(codes.Unavailable, "node is standby and leader address is unknown")
	}
	st, _ := status.New(codes.Unavailable, "node is standby; use leader at "+clusterAddr).
		WithDetails(&DRRedirectDetail{LeaderClusterAddr: clusterAddr})
	return st.Err()
}

// The primary streams storage mutations to the secondary in real time
// as EntryBatch messages (batched for throughput). The RPC is
// bidirectional: the secondary sends an init message followed by
// periodic WindowUpdate credits; the primary sends batched entries
// gated by available credits.
func (s *drReplicationPrimary) StreamChanges(stream grpc.BidiStreamingServer[StreamChangesUpstream, EntryBatch]) error {
	if err := s.requireActiveNode(); err != nil {
		return err
	}

	// --- Handshake: first upstream message must be init ---
	initMsg, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive init message: %v", err)
	}
	req := initMsg.GetInit()
	if req == nil {
		return status.Error(codes.InvalidArgument, "first StreamChangesUpstream message must be init")
	}

	if err := s.authorizeRelationship(stream.Context(), req.RelationshipId, DRRelationshipStateActive); err != nil {
		return err
	}

	initialWindow, err := s.validateInitialStreamWindow(req.InitialWindow)
	if err != nil {
		return err
	}
	maxWindowCredits := s.maxStreamWindowCredits()

	subID, err := uuid.GenerateUUID()
	if err != nil {
		return fmt.Errorf("failed to generate subscriber ID: %w", err)
	}

	// Derive the subscriber context from BOTH the gRPC stream context and
	// the core's active context. The stream must terminate when either:
	//   (a) the gRPC connection breaks (stream.Context() cancelled), or
	//   (b) this node loses leadership (activeContext cancelled on stepdown/seal).
	// Without (b), a stepdown leaves the gRPC stream open but idle: the
	// primary stops pushing entries (OnChange runs on the new leader) while
	// the secondary blocks on Recv() and never detects the disconnect.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// Monitor the active context; cancel the subscriber if leadership is lost.
	go func() {
		select {
		case <-ctx.Done():
			// Stream already closing -- nothing to do.
		case <-s.core.activeContext.Load().Done():
			cancel()
		}
	}()

	sub := &changeStreamSubscriber{
		id:             subID,
		relationshipID: req.RelationshipId,
		ch:             make(chan physical.ChangeStreamEntry, s.bufMaxSize),
		cancel:         cancel,
		creditCh:       make(chan struct{}, 1),
	}
	sub.credits.Store(initialWindow)

	s.mu.Lock()
	s.subscribers[subID] = sub
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subscribers, subID)
		s.mu.Unlock()
	}()

	s.logger.Info("change stream subscriber connected",
		"subscriber", subID,
		"relationship", req.RelationshipId,
		"last_applied_index", req.LastAppliedIndex,
		"initial_window", initialWindow)

	// --- Credit-reader goroutine ---
	// Reads subsequent upstream messages (WindowUpdate) and replenishes
	// the subscriber's credit counter.
	var creditErr atomic.Value
	go func() {
		for {
			upstream, err := stream.Recv()
			if err != nil {
				// Stream closed or error -- cancel the subscriber context
				// so the send loop exits.
				cancel()
				return
			}
			if wu := upstream.GetWindowUpdate(); wu != nil && wu.Credits > 0 {
				if err := addStreamWindowCredits(sub, wu.Credits, maxWindowCredits); err != nil {
					creditErr.Store(err)
					cancel()
					return
				}
				// Non-blocking signal to wake up a waiting sender.
				select {
				case sub.creditCh <- struct{}{}:
				default:
				}
			}
		}
	}()

	// --- Catch-up replay ---
	// Check if buffer/journal can satisfy catch-up, and send catch-up changes.
	//
	// Resume is inclusive on last_applied_index because EntryChange is emitted
	// per storage operation while the cursor is a Raft index. A reconnect may
	// occur after applying one operation at index N but before applying the
	// remaining operations at the same index.
	resumeFrom := req.LastAppliedIndex
	s.bufMu.RLock()
	bufferSnapshot := append([]physical.ChangeStreamEntry(nil), s.changeBuffer...)
	s.bufMu.RUnlock()

	// catchupSent tracks how many entries are sent during catch-up replay
	// so the credit counter can be adjusted before entering the live loop.
	var catchupSent int64

	// sendBatch is a helper that sends an accumulated batch as a single
	// EntryBatch message. It is used by both catch-up replay and live
	// streaming to reduce per-message gRPC overhead.
	sendBatch := func(batch []*EntryChange) error {
		if len(batch) == 0 {
			return nil
		}
		return stream.Send(&EntryBatch{Entries: batch})
	}

	// sendCatchupBatch sends a batch and tracks the entry count for
	// credit adjustment after catch-up completes.
	sendCatchupBatch := func(batch []*EntryChange) error {
		if len(batch) == 0 {
			return nil
		}
		catchupSent += int64(len(batch))
		return stream.Send(&EntryBatch{Entries: batch})
	}

	bufferStart := resumeFrom
	if len(bufferSnapshot) > 0 {
		oldestIdx := bufferSnapshot[0].RaftIndex
		if bufferStart < oldestIdx {
			if s.streamJournal != nil {
				s.journalReplayAttempts.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "stream", "journal_replay_attempts_total"}, 1)
				// Batch journal replay entries for throughput.
				journalBatch := make([]*EntryChange, 0, drStreamSendBatchMaxEntries)
				if err := s.streamJournal.replayRange(bufferStart, oldestIdx, func(e physical.ChangeStreamEntry) error {
					journalBatch = append(journalBatch, entryChangeFromPhysical(e))
					if len(journalBatch) >= drStreamSendBatchMaxEntries {
						if err := sendCatchupBatch(journalBatch); err != nil {
							return err
						}
						journalBatch = journalBatch[:0]
					}
					return nil
				}); err != nil {
					if errors.Is(err, errDRStreamJournalRangeTooOld) {
						s.journalRangeTooOld.Add(1)
						metrics.IncrCounter([]string{"replication", "dr", "stream", "journal_range_too_old_total"}, 1)
						s.logger.Warn("journal cannot satisfy catch-up, secondary needs reconciliation",
							"requested_from", bufferStart,
							"oldest_buffered", oldestIdx)
						return status.Errorf(codes.FailedPrecondition, "journal too old: secondary missing from %d (inclusive), oldest buffered %d; reconciliation required",
							bufferStart, oldestIdx)
					}
					return status.Errorf(codes.Internal, "journal catch-up failed: %v", err)
				}
				// Flush remaining journal entries.
				if err := sendCatchupBatch(journalBatch); err != nil {
					return err
				}
				s.journalReplaySuccess.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "stream", "journal_replay_success_total"}, 1)
			} else {
				s.logger.Warn("buffer cannot satisfy catch-up, secondary needs reconciliation",
					"requested_from", bufferStart,
					"oldest_buffered", oldestIdx)
				return status.Errorf(codes.FailedPrecondition, "buffer too old: secondary missing from %d (inclusive), oldest buffered %d; reconciliation required",
					bufferStart, oldestIdx)
			}
			bufferStart = oldestIdx
		}
	}
	// Batch buffer catch-up entries for throughput.
	bufCatchupBatch := make([]*EntryChange, 0, drStreamSendBatchMaxEntries)
	for _, e := range bufferSnapshot {
		if e.RaftIndex >= bufferStart {
			bufCatchupBatch = append(bufCatchupBatch, entryChangeFromPhysical(e))
			if len(bufCatchupBatch) >= drStreamSendBatchMaxEntries {
				if err := sendCatchupBatch(bufCatchupBatch); err != nil {
					return err
				}
				bufCatchupBatch = bufCatchupBatch[:0]
			}
		}
	}
	if err := sendCatchupBatch(bufCatchupBatch); err != nil {
		return err
	}

	// Debit the credit counter for entries sent during catch-up so the
	// live loop has an accurate view of how many entries the secondary
	// can still accept. Without this adjustment the counter is inflated:
	// the secondary will send WindowUpdate credits for the catch-up
	// entries it processes, but the primary never decremented for them.
	if catchupSent > 0 {
		sub.credits.Add(-catchupSent)
		s.logger.Debug("adjusted credits after catch-up replay",
			"subscriber", sub.id,
			"catchup_sent", catchupSent,
			"credits_remaining", sub.credits.Load())
	}

	// --- Live stream with credit-gated batched sends ---
	authTicker := time.NewTicker(5 * time.Second)
	defer authTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err, ok := loadStreamCreditError(&creditErr); ok {
				return err
			}
			return ctx.Err()
		case <-authTicker.C:
			if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
				return err
			}
		case entry := <-sub.ch:
			// Wait for at least one credit for the first entry.
			if err := s.waitForCredit(ctx, sub); err != nil {
				return err
			}
			// Build a batch: first entry is already credit-gated above.
			batch := []*EntryChange{entryChangeFromPhysical(entry)}
			// Drain additional entries while credits and channel permit.
		drainLoop:
			for len(batch) < drStreamSendBatchMaxEntries {
				// Try to acquire another credit optimistically.
				if sub.credits.Add(-1) < 0 {
					sub.credits.Add(1) // undo
					break drainLoop
				}
				// Credit acquired -- try to drain one entry.
				select {
				case e := <-sub.ch:
					batch = append(batch, entryChangeFromPhysical(e))
				default:
					sub.credits.Add(1) // refund -- no entry queued
					break drainLoop
				}
			}
			if err := sendBatch(batch); err != nil {
				return err
			}
		}
	}
}

func (s *drReplicationPrimary) maxStreamWindowCredits() int64 {
	max := s.bufMaxSize
	if max <= 0 {
		max = drStreamBufferMaxEntries
	}
	return int64(max)
}

func (s *drReplicationPrimary) validateInitialStreamWindow(requested uint64) (int64, error) {
	max := s.maxStreamWindowCredits()
	if requested == 0 {
		if drDefaultInitialWindow > max {
			return max, nil
		}
		return drDefaultInitialWindow, nil
	}
	if requested > uint64(max) {
		return 0, status.Errorf(codes.ResourceExhausted, "initial_window %d exceeds maximum %d", requested, max)
	}
	return int64(requested), nil
}

func addStreamWindowCredits(sub *changeStreamSubscriber, credits uint64, max int64) error {
	if credits == 0 {
		return nil
	}
	if max <= 0 {
		return status.Error(codes.FailedPrecondition, "stream window maximum is not configured")
	}
	if credits > uint64(max) {
		return status.Errorf(codes.ResourceExhausted, "window_update credits %d exceeds maximum %d", credits, max)
	}
	add := int64(credits)
	for {
		current := sub.credits.Load()
		if current > max {
			return status.Errorf(codes.ResourceExhausted, "stream credits %d exceed maximum %d", current, max)
		}
		if add > max-current {
			return status.Errorf(codes.ResourceExhausted, "stream credits would exceed maximum %d", max)
		}
		if sub.credits.CompareAndSwap(current, current+add) {
			return nil
		}
	}
}

func loadStreamCreditError(v *atomic.Value) (error, bool) {
	if v == nil {
		return nil, false
	}
	raw := v.Load()
	if raw == nil {
		return nil, false
	}
	err, ok := raw.(error)
	return err, ok
}

func validateRangeChecksumRequest(req *RangeChecksumRequest) error {
	rangeCount := len(req.GetRangeIds())
	if rangeCount > drRangeMaxTotalRanges {
		return status.Errorf(codes.ResourceExhausted,
			"range checksum request has %d ranges; maximum is %d",
			rangeCount, drRangeMaxTotalRanges)
	}
	for _, rangeID := range req.GetRangeIds() {
		if rangeID >= uint64(drRangeMaxTotalRanges) {
			return status.Errorf(codes.InvalidArgument,
				"range_id %d exceeds maximum %d",
				rangeID, drRangeMaxTotalRanges-1)
		}
	}
	return nil
}

func validateFetchEntriesRequest(req *FetchEntriesRequest) error {
	selectorCount := len(req.GetKids()) + len(req.GetItems())
	if selectorCount > drFetchRequestMaxSelectors {
		return status.Errorf(codes.ResourceExhausted,
			"fetch request has %d point selectors; maximum is %d",
			selectorCount, drFetchRequestMaxSelectors)
	}
	rangeCount := len(req.GetRanges())
	if rangeCount > drRangeMaxTotalRanges {
		return status.Errorf(codes.ResourceExhausted,
			"fetch request has %d ranges; maximum is %d",
			rangeCount, drRangeMaxTotalRanges)
	}
	return nil
}

// waitForCredit blocks until the subscriber has at least one credit
// available, or until the credit wait timeout / context cancellation.
// Returns nil when a credit was successfully consumed.
func (s *drReplicationPrimary) waitForCredit(ctx context.Context, sub *changeStreamSubscriber) error {
	// Fast path: credit available.
	if sub.credits.Add(-1) >= 0 {
		return nil
	}
	// Undo the speculative decrement.
	sub.credits.Add(1)

	metrics.IncrCounter([]string{"replication", "dr", "stream", "credit_wait_total"}, 1)
	s.logger.Debug("subscriber out of credits, waiting for replenishment",
		"subscriber", sub.id)

	timeout := s.creditWaitTimeout
	if timeout <= 0 {
		timeout = drCreditWaitTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			s.logger.Warn("subscriber credit wait timeout, cancelling",
				"subscriber", sub.id,
				"timeout", timeout)
			return status.Errorf(codes.ResourceExhausted,
				"subscriber %s: credit replenishment timeout after %v", sub.id, timeout)
		case <-sub.creditCh:
			// Credits may have arrived. Try to consume one.
			if sub.credits.Add(-1) >= 0 {
				return nil
			}
			sub.credits.Add(1)
			// Spurious wake or credits consumed by catch-up; loop.
		}
	}
}

// RequestCheckpoint implements DRReplicationServer.RequestCheckpoint.
func (s *drReplicationPrimary) RequestCheckpoint(ctx context.Context, req *CheckpointRequest) (*CheckpointResponse, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}

	// Get current Raft commit index from the underlying backend.
	var commitIndex uint64
	if rb, ok := s.core.underlyingPhysical.(*raft.RaftBackend); ok {
		commitIndex = rb.AppliedIndex()
	}

	for {
		if resp, ok := s.reuseCheckpointResponse(req.RelationshipId, commitIndex); ok {
			if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
				return nil, err
			}
			return resp, nil
		}
		if throttle, reason := s.shouldThrottleCheckpointBuild(); throttle {
			if s.allowForcedCheckpointBuild(req.RelationshipId) {
				s.checkpointThrottleBypassTotal.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "throttle_bypass"}, 1)
				s.logger.Warn("overriding checkpoint throttle to preserve reconcile progress",
					"relationship_id", req.RelationshipId,
					"reason", reason)
			} else {
				s.checkpointThrottleTotal.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "throttled"}, 1)
				s.logger.Warn("throttling checkpoint build due to stream pressure",
					"relationship_id", req.RelationshipId,
					"reason", reason)
				return nil, status.Errorf(codes.FailedPrecondition, "budget_exceeded: primary stream pressure (%s); retry", reason)
			}
		}

		wait, owner, err := s.claimCheckpointBuild(req.RelationshipId)
		if err != nil {
			return nil, err
		}
		if !owner {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait.done:
				if wait.err != nil {
					return nil, wait.err
				}
				// Try reuse after the in-flight build completes.
				continue
			}
		}

		resp, err := s.buildAndCacheCheckpoint(ctx, req.RelationshipId, commitIndex)
		s.finishCheckpointBuild(req.RelationshipId, wait, resp, err)
		if err != nil {
			return nil, err
		}
		if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// ExchangeDirtyBitmap implements DRReplicationServer.ExchangeDirtyBitmap.
func (s *drReplicationPrimary) ExchangeDirtyBitmap(ctx context.Context, req *DirtyBitmapMessage) (*DirtyBitmapMessage, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	s.dirtyMapMu.RLock()
	defer s.dirtyMapMu.RUnlock()

	bitmap := make([]byte, len(s.dirtyMap))

	if s.dirtyMapStart == 0 {
		// Pessimistic safety: the dirty bitmap has not been tracking
		// since startup (fresh start or post-restart). Return an
		// all-dirty bitmap so the secondary checks every range. This
		// prevents silent data loss where a restart wipes the in-memory
		// bitmap and the secondary incorrectly concludes nothing changed.
		for i := range bitmap {
			bitmap[i] = 0xFF
		}
		s.logger.Warn("dirty bitmap not initialized (post-restart or fresh start), returning all-dirty")
	} else {
		copy(bitmap, s.dirtyMap)
	}

	resp := &DirtyBitmapMessage{
		RelationshipId:  req.RelationshipId,
		CheckpointId:    req.CheckpointId,
		CheckpointIndex: req.CheckpointIndex,
		StartIndex:      s.dirtyMapStart,
		Bitmap:          bitmap,
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	return resp, nil
}

// loadDirtyBitmap restores the dirty bitmap from physical storage after
// a restart. If no persisted bitmap is found, the bitmap remains in its
// default state (dirtyMapStart == 0) which causes ExchangeDirtyBitmap
// to return an all-dirty bitmap for safety.
func (s *drReplicationPrimary) loadDirtyBitmap(ctx context.Context) error {
	entry, err := s.core.physical.Get(ctx, drDirtyBitmapStoragePath)
	if err != nil {
		return fmt.Errorf("failed to read dirty bitmap from storage: %w", err)
	}
	if entry == nil || len(entry.Value) < 8 {
		s.logger.Info("no persisted dirty bitmap found, will default to all-dirty on next exchange")
		return nil
	}

	s.dirtyMapMu.Lock()
	defer s.dirtyMapMu.Unlock()

	startIndex := binary.BigEndian.Uint64(entry.Value[:8])
	bitmapData := entry.Value[8:]

	if len(bitmapData) != drDirtyBitmapBytes {
		s.logger.Warn("persisted dirty bitmap has unexpected size, ignoring",
			"expected", drDirtyBitmapBytes, "got", len(bitmapData))
		return nil
	}

	s.dirtyMapStart = startIndex
	copy(s.dirtyMap, bitmapData)
	s.logger.Info("loaded persisted dirty bitmap", "start_index", startIndex)
	return nil
}

// persistDirtyBitmap saves the current dirty bitmap to physical storage
// synchronously.  Caller must hold dirtyMapMu at least for reading.
//
// WARNING: Do NOT call this from within OnChange (the Raft FSM
// applyBatch callback).  Use persistDirtyBitmapAsync instead.
// A synchronous physical.Put from inside the FSM callback would submit
// a new Raft log and block waiting for the FSM to apply it, but the
// FSM cannot proceed because it is still inside the current
// applyBatch -- causing a re-entrant deadlock.
func (s *drReplicationPrimary) persistDirtyBitmap() {
	if s.dirtyMapStart == 0 {
		return // Nothing meaningful to persist yet.
	}

	buf := make([]byte, 8+drDirtyBitmapBytes)
	binary.BigEndian.PutUint64(buf[:8], s.dirtyMapStart)
	copy(buf[8:], s.dirtyMap)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.core.physical.Put(ctx, &physical.Entry{
		Key:   drDirtyBitmapStoragePath,
		Value: buf,
	}); err != nil {
		s.logger.Warn("failed to persist dirty bitmap", "error", err)
		return
	}
	s.dirtyMapLastPersist = time.Now()
}

// persistDirtyBitmapAsync snapshots the dirty bitmap under the caller's
// lock and writes it to physical storage in a background goroutine.
// This is safe to call from within the FSM applyBatch path (OnChange).
// Caller must hold dirtyMapMu at least for reading.
func (s *drReplicationPrimary) persistDirtyBitmapAsync() {
	if s.dirtyMapStart == 0 {
		return // Nothing meaningful to persist yet.
	}

	// Snapshot the bitmap while the lock is held so the goroutine
	// doesn't race with future OnChange updates.
	buf := make([]byte, 8+drDirtyBitmapBytes)
	binary.BigEndian.PutUint64(buf[:8], s.dirtyMapStart)
	copy(buf[8:], s.dirtyMap)

	// Mark the persist time immediately to prevent duplicate goroutines
	// from being launched before the write completes.
	s.dirtyMapLastPersist = time.Now()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := s.core.physical.Put(ctx, &physical.Entry{
			Key:   drDirtyBitmapStoragePath,
			Value: buf,
		}); err != nil {
			s.logger.Warn("failed to persist dirty bitmap", "error", err)
		}
	}()
}

// maybePersistDirtyBitmap schedules an async bitmap persist if enough
// time has elapsed since the last one. Must be called with dirtyMapMu held.
//
// This is called from OnChange (inside the FSM applyBatch path) so it
// MUST use the async variant to avoid a re-entrant Raft write deadlock.
func (s *drReplicationPrimary) maybePersistDirtyBitmap() {
	if time.Since(s.dirtyMapLastPersist) >= drDirtyBitmapPersistInterval {
		s.persistDirtyBitmapAsync()
	}
}

// ExchangeRangeChecksums implements DRReplicationServer.ExchangeRangeChecksums.
func (s *drReplicationPrimary) ExchangeRangeChecksums(ctx context.Context, req *RangeChecksumRequest) (*RangeChecksumResponse, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "range checksum request is required")
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	if err := validateRangeChecksumRequest(req); err != nil {
		return nil, err
	}
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointRelationship(req.RelationshipId, cp); err != nil {
		return nil, err
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}

	// Calculate checksums for requested ranges.
	checksums := make(map[uint64]*RangeChecksum)
	for _, rid := range req.RangeIds {
		checksums[rid] = &RangeChecksum{RangeId: rid}
	}
	allRanges := len(req.RangeIds) == 0

	crcTable := crc64.MakeTable(crc64.ISO)

	for kid, vid := range cp.kidToVID {
		rangeID := (uint64(kid[0]) << 2) | (uint64(kid[1]) >> 6)

		var rc *RangeChecksum
		if allRanges {
			var ok bool
			rc, ok = checksums[rangeID]
			if !ok {
				rc = &RangeChecksum{RangeId: rangeID}
				checksums[rangeID] = rc
			}
		} else {
			var ok bool
			rc, ok = checksums[rangeID]
			if !ok {
				continue
			}
		}

		rc.Count++
		// Simple XOR of CRC64(KID) ^ CRC64(VID)
		kSum := crc64.Checksum(kid[:], crcTable)
		vSum := crc64.Checksum(vid[:], crcTable)
		rc.Checksum ^= (kSum ^ vSum)
	}

	resp := &RangeChecksumResponse{
		Checksums: make([]*RangeChecksum, 0, len(checksums)),
	}
	for _, rc := range checksums {
		resp.Checksums = append(resp.Checksums, rc)
	}

	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	return resp, nil
}

// ExchangeRangeDigests implements DRReplicationServer.ExchangeRangeDigests.
// It performs fine-grained drill-down on a mismatched range by splitting
// it in half and returning sub-range digests for each child.
func (s *drReplicationPrimary) ExchangeRangeDigests(ctx context.Context, req *RangeDigestRequest) (*RangeDigestResponse, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "range digest request is required")
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointRelationship(req.RelationshipId, cp); err != nil {
		return nil, err
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}

	parentSpan := req.GetParentSpan()
	if parentSpan == nil || len(parentSpan.StartKid) != 32 || len(parentSpan.EndKid) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "parent_span must have valid 32-byte KID boundaries")
	}
	if parentSpan.SplitDepth > uint32(drRangeMaxSplitDepth) {
		return nil, status.Errorf(codes.InvalidArgument, "parent_span split depth %d exceeds maximum %d", parentSpan.SplitDepth, drRangeMaxSplitDepth)
	}

	var start, end [32]byte
	copy(start[:], parentSpan.StartKid)
	copy(end[:], parentSpan.EndKid)
	parent := reconciler.RangeSpan{
		StartKID:   start,
		EndKID:     end,
		SplitDepth: parentSpan.SplitDepth,
	}
	if !parent.Valid() {
		return nil, status.Error(codes.InvalidArgument, "parent_span start must be <= end")
	}

	// Split the parent range into two children.
	left, right, ok := reconciler.SplitRange(parent)
	if !ok {
		// Cannot split further -- return a single digest for the parent.
		idx := reconciler.NewRangeMapIndex(cp.kidToVID, nil)
		desc := reconciler.BuildRangeDigestFromIndex(idx, parent)
		resp := &RangeDigestResponse{
			Digests: []*RangeDigest{rangeDescriptorToProto(desc)},
		}
		if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
			return nil, err
		}
		return resp, nil
	}

	idx := reconciler.NewRangeMapIndex(cp.kidToVID, nil)
	leftDesc := reconciler.BuildRangeDigestFromIndex(idx, left)
	rightDesc := reconciler.BuildRangeDigestFromIndex(idx, right)

	resp := &RangeDigestResponse{
		Digests: []*RangeDigest{
			rangeDescriptorToProto(leftDesc),
			rangeDescriptorToProto(rightDesc),
		},
	}
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	return resp, nil
}

// rangeDescriptorToProto converts a reconciler.RangeDescriptor to the proto RangeDigest.
func rangeDescriptorToProto(desc reconciler.RangeDescriptor) *RangeDigest {
	return &RangeDigest{
		Span: &RangeSpan{
			StartKid:   desc.Span.StartKID[:],
			EndKid:     desc.Span.EndKID[:],
			SplitDepth: desc.Span.SplitDepth,
		},
		Count:            desc.Count,
		XorKeyHash:       desc.XORKeyHash[:],
		XorValueHash:     desc.XORValueHash[:],
		ApproxValueBytes: desc.ApproxValueBytes,
	}
}

// FetchEntries implements DRReplicationServer.FetchEntries.
// The secondary requests specific entries by KID and/or by bucket
// indices; the primary looks them up and streams them back.
func (s *drReplicationPrimary) FetchEntries(req *FetchEntriesRequest, stream grpc.ServerStreamingServer[EntryBatch]) error {
	if err := s.requireActiveNode(); err != nil {
		return err
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "fetch request is required")
	}
	if err := s.authorizeRelationship(stream.Context(), req.RelationshipId, DRRelationshipStateActive); err != nil {
		return err
	}
	if err := validateFetchEntriesRequest(req); err != nil {
		return err
	}

	// Derive a context from both the gRPC stream and the core's active
	// context. The stream must terminate when either:
	//   (a) the gRPC connection breaks (stream.Context() cancelled), or
	//   (b) this node loses leadership (activeContext cancelled on stepdown).
	// Without (b), a stepdown during a FetchEntries call leaves the
	// handler iterating over checkpoint data while the secondary's
	// stream.Recv() blocks indefinitely, stalling reconciliation.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.core.activeContext.Load().Done():
			cancel()
		}
	}()

	cp, releaseCheckpoint, err := s.getCheckpointLease(req.CheckpointId)
	if err != nil {
		return status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	defer releaseCheckpoint()
	if err := validateCheckpointRelationship(req.RelationshipId, cp); err != nil {
		return err
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}
	authorizeFetchContinuation := func() error {
		return s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateActive)
	}

	s.logger.Info("fetching entries",
		"checkpoint_id", req.CheckpointId,
		"num_kids", len(req.Kids),
		"num_items", len(req.Items),
		"range_count", len(req.Ranges))
	if req.GetCheckpointIndex() > 0 &&
		len(req.GetItems()) == 0 &&
		len(req.GetKids()) > 0 &&
		len(req.GetRanges()) == 0 {
		s.logger.Debug("fetch request missing explicit expected_vid items; falling back to checkpoint-only provenance",
			"checkpoint_id", req.CheckpointId,
			"kids", len(req.Kids))
	}

	var batch EntryBatch
	var batchBytes uint64
	resetFetchBatch := func() {
		batch = EntryBatch{
			CheckpointId:    cp.checkpoint.ID,
			CheckpointIndex: cp.checkpoint.CommitIndex,
		}
		batchBytes = entryBatchBaseWireBytes(&batch)
	}
	resetFetchBatch()
	flushFetchBatch := func() error {
		if len(batch.Entries) == 0 && len(batch.FailedKids) == 0 {
			return nil
		}
		if err := authorizeFetchContinuation(); err != nil {
			return err
		}
		if err := stream.Send(&batch); err != nil {
			return err
		}
		resetFetchBatch()
		return nil
	}
	appendFailedKID := func(kid [32]byte) error {
		const failedKIDBytes = uint64(32 + 8)
		if batchBytes+failedKIDBytes > drFetchSendBatchMaxBytes && (len(batch.Entries) > 0 || len(batch.FailedKids) > 0) {
			if err := flushFetchBatch(); err != nil {
				return err
			}
		}
		batch.FailedKids = append(batch.FailedKids, kid[:])
		batchBytes += failedKIDBytes
		if len(batch.Entries)+len(batch.FailedKids) >= drFetchSendBatchMaxEntries || batchBytes >= drFetchSendBatchMaxBytes {
			return flushFetchBatch()
		}
		return nil
	}
	appendFetchEntry := func(change *EntryChange) error {
		if change == nil {
			return nil
		}
		changeBytes := entryChangeWireBytes(change)
		singleEntryBatchBytes := entryBatchBaseWireBytes(&batch) + changeBytes
		if singleEntryBatchBytes > drFetchSendBatchMaxBytes {
			return status.Errorf(codes.ResourceExhausted,
				"fetch entry batch payload %d exceeds maximum batch bytes %d",
				singleEntryBatchBytes, drFetchSendBatchMaxBytes)
		}
		if batchBytes+changeBytes > drFetchSendBatchMaxBytes && (len(batch.Entries) > 0 || len(batch.FailedKids) > 0) {
			if err := flushFetchBatch(); err != nil {
				return err
			}
		}
		batch.Entries = append(batch.Entries, change)
		batchBytes += changeBytes
		if len(batch.Entries) >= drFetchSendBatchMaxEntries || batchBytes >= drFetchSendBatchMaxBytes {
			return flushFetchBatch()
		}
		return nil
	}
	sent := make(map[[32]byte]bool)
	rangeSpans, err := parseRangeSpans(req.GetRanges())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid ranges: %v", err)
	}

	expectedVIDByKID := make(map[[32]byte][32]byte, len(req.Items))
	for _, item := range req.Items {
		if len(item.Kid) != 32 || len(item.ExpectedVid) != 32 {
			return status.Error(codes.InvalidArgument, "fetch items must include 32-byte kid and expected_vid")
		}
		var kid [32]byte
		var vid [32]byte
		copy(kid[:], item.Kid)
		copy(vid[:], item.ExpectedVid)
		expectedVIDByKID[kid] = vid
	}

	emitFromCheckpoint := func(kid [32]byte, expectedVID *[32]byte) error {
		change, err := s.readCheckpointEntryChange(ctx, cp, kid, expectedVID, req.IncludeDeletes)
		if err != nil {
			if status.Code(err) == codes.FailedPrecondition {
				return err
			}
			return appendFailedKID(kid)
		}
		return appendFetchEntry(change)
	}

	// Range-based fetching with parallel artifact reads.
	if len(rangeSpans) > 0 {
		// Collect matching KIDs first (cheap in-memory filter).
		var matchingKIDs [][32]byte
		for kid := range cp.kidToVID {
			if kidInAnyRange(kid, rangeSpans) {
				matchingKIDs = append(matchingKIDs, kid)
				sent[kid] = true
			}
		}

		if len(matchingKIDs) > 0 {
			const fetchWorkers = 4

			type fetchResult struct {
				change *EntryChange
				err    error
			}

			kidCh := make(chan [32]byte, fetchWorkers*2)
			resultCh := make(chan fetchResult, fetchWorkers*2)

			// Spawn parallel reader workers.
			var wg sync.WaitGroup
			for w := 0; w < fetchWorkers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for kid := range kidCh {
						change, err := s.readCheckpointEntryChange(ctx, cp, kid, nil, req.IncludeDeletes)
						resultCh <- fetchResult{change: change, err: err}
					}
				}()
			}

			// Feed KIDs to workers in a separate goroutine.
			go func() {
				for _, kid := range matchingKIDs {
					select {
					case kidCh <- kid:
					case <-ctx.Done():
						break
					}
				}
				close(kidCh)
				wg.Wait()
				close(resultCh)
			}()

			// Collect results and stream them.
			for res := range resultCh {
				if ctx.Err() != nil {
					return status.Errorf(codes.Canceled, "fetch aborted: %v", ctx.Err())
				}
				if res.err != nil {
					if status.Code(res.err) == codes.FailedPrecondition {
						return res.err
					}
					// Non-fatal: record as failed KID in the batch.
					continue
				}
				if res.change == nil {
					continue
				}
				if err := appendFetchEntry(res.change); err != nil {
					return err
				}
			}
		}
	}

	// Provenance-safe point fetches from items.
	for kid, expectedVID := range expectedVIDByKID {
		if ctx.Err() != nil {
			return status.Errorf(codes.Canceled, "fetch aborted: %v", ctx.Err())
		}
		if sent[kid] {
			continue
		}
		sent[kid] = true
		expected := expectedVID
		if err := emitFromCheckpoint(kid, &expected); err != nil {
			return err
		}
	}

	// Look up each specifically requested KID.
	for _, kidBytes := range req.Kids {
		if ctx.Err() != nil {
			return status.Errorf(codes.Canceled, "fetch aborted: %v", ctx.Err())
		}
		if len(kidBytes) != 32 {
			continue
		}
		var kid [32]byte
		copy(kid[:], kidBytes)
		if sent[kid] {
			continue
		}
		sent[kid] = true
		if err := emitFromCheckpoint(kid, nil); err != nil {
			return err
		}
	}

	// Send remaining entries.
	if err := flushFetchBatch(); err != nil {
		return err
	}

	return nil
}

// Heartbeat implements DRReplicationServer.Heartbeat.
func (s *drReplicationPrimary) Heartbeat(ctx context.Context, req *DRHeartbeatRequest) (*DRHeartbeatResponse, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if err := s.authorizeRelationshipNoActivate(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	if mgr := s.core.drManager; mgr != nil {
		mgr.MarkRelationshipSeen(req.RelationshipId)
	}

	var raftApplied, primaryTerm uint64
	if rb, ok := s.core.underlyingPhysical.(*raft.RaftBackend); ok {
		raftApplied = rb.AppliedIndex()
		primaryTerm = rb.Term()
	}

	// Use backpressure calculations against the raw Raft applied index
	// so throttling reflects actual write load.
	s.recordSecondaryPressure(req.RelationshipId, raftApplied, req.GetAppliedIndex(), time.Now().UTC())

	// Return the replicable index as the primary_index so the
	// secondary computes lag only against indices it can actually
	// reach. Fall back to the raw applied index when no replicable
	// entries have been observed yet (cold start).
	reportedIndex := s.indexReplicable.Load()
	if reportedIndex == 0 {
		reportedIndex = raftApplied
	}

	// Include this node's cluster address so the secondary always knows
	// where the current leader is, enabling redirect-based reconnection
	// after stepdowns without requiring a load balancer.
	var leaderClusterAddr string
	if _, _, addr, err := s.core.Leader(); err == nil {
		leaderClusterAddr = addr
	}

	// Include the active node's DR transport leaf certificate so the
	// secondary can build a dynamic trust pool for server certificate
	// verification after leadership changes.
	var activeClusterCert []byte
	if mgr := s.core.drManager; mgr != nil {
		if handler := mgr.Handler(); handler != nil {
			activeClusterCert = handler.ActiveDRLeafCertDER()
		}
	}
	if len(activeClusterCert) == 0 {
		if s.heartbeatMissingDRLeafWarned.CompareAndSwap(false, true) {
			s.logger.Warn("no DR transport leaf certificate available for heartbeat; active_cluster_cert omitted")
		}
	} else {
		s.heartbeatMissingDRLeafWarned.Store(false)
	}

	if err := s.authorizeRelationshipNoActivate(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	return &DRHeartbeatResponse{
		PrimaryIndex:      reportedIndex,
		PrimaryTerm:       primaryTerm,
		ReplicationState:  uint32(s.core.ReplicationState()),
		LeaderClusterAddr: leaderClusterAddr,
		ActiveClusterCert: activeClusterCert,
	}, nil
}

// OldestBufferedIndex returns the oldest Raft index in the change buffer.
// Returns 0 if the buffer is empty.
func (s *drReplicationPrimary) OldestBufferedIndex() uint64 {
	s.bufMu.RLock()
	defer s.bufMu.RUnlock()
	if len(s.changeBuffer) == 0 {
		return 0
	}
	return s.changeBuffer[0].RaftIndex
}

// SyncKeyring implements DRReplicationServer.SyncKeyring.
// It reads the encrypted keyring and root key from physical storage,
// extracts the plaintext root key from the barrier, and sends
// everything to the secondary for bootstrap.
func (s *drReplicationPrimary) SyncKeyring(ctx context.Context, req *SyncKeyringRequest) (*SyncKeyringResponse, error) {
	if err := s.requireActiveNode(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "sync keyring request is required")
	}
	if len(req.ClientEphemeralPubkey) != 32 {
		return nil, status.Error(codes.InvalidArgument, "client_ephemeral_pubkey must be a 32-byte X25519 public key")
	}
	if len(req.ClientNonce) != drBootstrapNonceSize {
		return nil, status.Errorf(codes.InvalidArgument, "client_nonce must be %d bytes", drBootstrapNonceSize)
	}

	s.syncKeyringMu.Lock()
	defer s.syncKeyringMu.Unlock()

	if err := s.authorizeRelationshipNoActivate(ctx, req.RelationshipId, DRRelationshipStateRegistered); err != nil {
		return nil, err
	}

	s.logger.Info("syncing keyring to secondary", "relationship_id", req.RelationshipId)

	// Read the encrypted keyring blob from physical storage.
	keyringEntry, err := s.core.physical.Get(ctx, "core/keyring")
	if err != nil {
		return nil, fmt.Errorf("failed to read keyring: %w", err)
	}

	// Read the encrypted root key entry.
	rootKeyEntry, err := s.core.physical.Get(ctx, "core/root-key")
	if err != nil {
		return nil, fmt.Errorf("failed to read root key: %w", err)
	}

	// Extract the plaintext root key from the barrier's in-memory keyring
	// and wrap it to the secondary's ephemeral key.
	keyring, err := s.core.barrier.Keyring()
	if err != nil {
		return nil, fmt.Errorf("failed to get barrier keyring: %w", err)
	}
	// Derive AAD binding parameters for the wrap:
	// - clusterID from the DR config
	// - secondaryCertFP from the mTLS peer identity
	// - primaryIdentity from the DR transport CA SPKI hash
	mgr := s.core.drManager
	wrapClusterID := ""
	wrapPrimaryIdentity := ""
	if mgr != nil {
		cfg := mgr.Config()
		wrapClusterID = cfg.ClusterID
		if mgr.transportCA != nil {
			wrapPrimaryIdentity = mgr.transportCA.spkiHash()
		}
	}
	if wrapClusterID == "" {
		return nil, status.Error(codes.FailedPrecondition, "DR cluster ID unavailable for key wrap")
	}
	if wrapPrimaryIdentity == "" {
		return nil, status.Error(codes.FailedPrecondition, "DR transport CA identity unavailable for key wrap")
	}
	wrapSecondaryFP, err := peerCertFingerprintFromContext(ctx)
	if err != nil || wrapSecondaryFP == "" {
		return nil, status.Errorf(codes.PermissionDenied, "failed to derive secondary certificate fingerprint: %v", err)
	}

	wrappedRootKey, serverPub, srvNonce, gcmIV, aadVersion, err := wrapRootKeyForDRSync(
		keyring.RootKey(),
		req.RelationshipId,
		wrapClusterID,
		wrapSecondaryFP,
		wrapPrimaryIdentity,
		req.ClientEphemeralPubkey,
		req.ClientNonce,
	)
	if err != nil {
		if strings.Contains(err.Error(), "invalid client ephemeral public key") ||
			strings.Contains(err.Error(), "failed to derive shared secret") {
			return nil, status.Errorf(codes.InvalidArgument, "invalid client_ephemeral_pubkey: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "failed to wrap root key: %v", err)
	}

	resp := &SyncKeyringResponse{
		WrappedRootKey:        wrappedRootKey,
		ServerEphemeralPubkey: serverPub,
		ServerNonce:           srvNonce,
		WrapNonce:             gcmIV,
		WrapAadVersion:        aadVersion,
	}
	if keyringEntry != nil {
		resp.KeyringEntry = keyringEntry.Value
	}
	if rootKeyEntry != nil {
		resp.RootKeyEntry = rootKeyEntry.Value
	}

	if mgr := s.core.drManager; mgr != nil {
		if err := mgr.MarkRelationshipActive(req.RelationshipId); err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "relationship activation failed: %v", err)
		}
	}

	s.logger.Info("keyring synced to secondary")
	return resp, nil
}

// --- Helpers ---

func (s *drReplicationPrimary) claimCheckpointBuild(relationshipID string) (*drCheckpointBuildResult, bool, error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	if in, ok := s.checkpointBuildInFlight[relationshipID]; ok {
		return in, false, nil
	}
	maxInFlight := s.checkpointBuildMaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = drCheckpointBuildMaxInFlight
	}
	if len(s.checkpointBuildInFlight) >= maxInFlight {
		s.checkpointBuildAdmissionFailures.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "build_admission_failures"}, 1)
		return nil, false, status.Errorf(codes.ResourceExhausted,
			"budget_exceeded: checkpoint build concurrency exceeded (%d >= %d); retry",
			len(s.checkpointBuildInFlight), maxInFlight)
	}
	in := &drCheckpointBuildResult{done: make(chan struct{})}
	s.checkpointBuildInFlight[relationshipID] = in
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "build_inflight"}, float32(len(s.checkpointBuildInFlight)))
	return in, true, nil
}

func (s *drReplicationPrimary) finishCheckpointBuild(relationshipID string, in *drCheckpointBuildResult, resp *CheckpointResponse, err error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	in.resp = resp
	in.err = err
	close(in.done)
	delete(s.checkpointBuildInFlight, relationshipID)
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "build_inflight"}, float32(len(s.checkpointBuildInFlight)))
}

func (s *drReplicationPrimary) reuseCheckpointResponse(relationshipID string, commitIndex uint64) (*CheckpointResponse, bool) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()

	now := time.Now().UTC()
	s.pruneCheckpointsLocked(now)

	latestID := s.latestCheckpointByRelationship[relationshipID]
	if latestID == "" {
		return nil, false
	}
	cp, ok := s.checkpoints[latestID]
	if !ok || cp.relationshipID != relationshipID {
		return nil, false
	}
	if cp.checkpoint.CommitIndex != commitIndex {
		return nil, false
	}

	return &CheckpointResponse{
		CheckpointId:     cp.checkpoint.ID,
		CommitIndex:      cp.checkpoint.CommitIndex,
		RangePlanVersion: cp.rangePlanVersion,
	}, true
}

func (s *drReplicationPrimary) buildAndCacheCheckpoint(ctx context.Context, relationshipID string, commitIndex uint64) (*CheckpointResponse, error) {
	checkpointID, err := uuid.GenerateUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	checkpoint := reconciler.Checkpoint{
		ID:          checkpointID,
		CommitIndex: commitIndex,
	}

	rs, err := s.buildCheckpointSet(ctx, checkpoint)
	if err != nil {
		return nil, err
	}
	if rs != nil && rs.Checkpoint.CommitIndex > 0 {
		checkpoint.CommitIndex = rs.Checkpoint.CommitIndex
	}

	entry := &drCheckpointCacheEntry{
		checkpoint:       checkpoint,
		relationshipID:   relationshipID,
		createdAt:        time.Now().UTC(),
		kidToKey:         rs.KIDToKey,
		kidToVID:         rs.KIDToVID,
		rangePlanVersion: reconciler.RangePlanVersion,
	}
	if s.checkpointArtifacts != nil && s.checkpointArtifacts.enabled {
		if err := s.checkpointArtifacts.waitForIndexFence(&s.indexApplied, checkpoint.CommitIndex, drCheckpointArtifactBuildFenceTimeout); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "checkpoint index fence failed: %v", err)
		}
		if err := s.checkpointArtifacts.build(ctx, entry, s.scanner, s.core.physical, s.rangePlanConfig); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "checkpoint artifact build failed: %v", err)
		}
	}

	if err := s.cacheCheckpoint(entry); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint cache pressure: %v", err)
	}

	s.checkpointMu.Lock()
	s.latestCheckpointByRelationship[relationshipID] = checkpointID
	s.checkpointMu.Unlock()

	s.logger.Info("checkpoint created",
		"checkpoint_id", checkpointID,
		"commit_index", commitIndex,
		"relationship_id", relationshipID,
		"keys", rs.KeyCount)

	return &CheckpointResponse{
		CheckpointId:     checkpointID,
		CommitIndex:      commitIndex,
		RangePlanVersion: reconciler.RangePlanVersion,
	}, nil
}

func (s *drReplicationPrimary) buildCheckpointSet(ctx context.Context, checkpoint reconciler.Checkpoint) (*reconciler.ReconciliationSet, error) {
	if err := s.checkpointArtifacts.waitForIndexFence(&s.indexApplied, checkpoint.CommitIndex, drCheckpointArtifactBuildFenceTimeout); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint index fence failed: %v", err)
	}

	if rs, ok := s.snapshotIndexCheckpointSet(checkpoint.CommitIndex); ok {
		return rs, nil
	}

	rs, err := s.scanner.ScanPhysical(ctx, s.core.physical, checkpoint)
	if err != nil {
		s.scanFailures.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "scan_failures"}, 1)
		return nil, status.Errorf(codes.Internal, "failed to build checkpoint snapshot: %v", err)
	}
	s.resetIndexFromSet(rs)
	s.indexApplied.Store(checkpoint.CommitIndex)
	return rs, nil
}

func (s *drReplicationPrimary) snapshotIndexCheckpointSet(commitIndex uint64) (*reconciler.ReconciliationSet, bool) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	if !s.indexInitialized {
		return nil, false
	}
	if commitIndex > 0 && s.indexApplied.Load() < commitIndex {
		return nil, false
	}
	if !s.indexLastFullScan.IsZero() && time.Since(s.indexLastFullScan) > drCheckpointIndexFullScanInterval {
		return nil, false
	}
	kidToVID := make(map[[32]byte][32]byte, len(s.indexKIDToVID))
	for kid, vid := range s.indexKIDToVID {
		kidToVID[kid] = vid
	}
	kidToKey := make(map[[32]byte]string, len(s.indexKIDToKey))
	for kid, key := range s.indexKIDToKey {
		kidToKey[kid] = key
	}
	rs := &reconciler.ReconciliationSet{
		Checkpoint: reconciler.Checkpoint{
			CommitIndex: s.indexApplied.Load(),
		},
		KIDToVID: kidToVID,
		KIDToKey: kidToKey,
		KeyCount: len(kidToVID),
	}
	return rs, true
}

func (s *drReplicationPrimary) resetIndexFromSet(rs *reconciler.ReconciliationSet) {
	if rs == nil {
		return
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	s.indexKIDToVID = make(map[[32]byte][32]byte, len(rs.KIDToVID))
	for kid, vid := range rs.KIDToVID {
		s.indexKIDToVID[kid] = vid
	}
	s.indexKIDToKey = make(map[[32]byte]string, len(rs.KIDToKey))
	for kid, key := range rs.KIDToKey {
		s.indexKIDToKey[kid] = key
	}
	s.indexInitialized = true
	s.indexLastFullScan = time.Now().UTC()
}

// WarmIndexFromJournal replays all available stream journal entries to
// build the in-memory KID/VID index. This is dramatically faster than
// a full storage scan because it only performs sequential reads on the
// local journal segments. After a leader election the journal is already
// populated (maintained by the drChangeStreamDispatcher on all nodes),
// so the new leader can warm its index almost instantly.
//
// Returns the number of entries replayed, or an error if the journal is
// unavailable.
func (s *drReplicationPrimary) WarmIndexFromJournal(journal *drStreamJournal) (int, error) {
	if journal == nil {
		return 0, fmt.Errorf("no journal available")
	}

	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	var count int
	var lastIdx uint64
	err := journal.replayAll(func(e physical.ChangeStreamEntry) error {
		kid := s.scanner.ComputeKID(e.Key)
		switch e.OpType {
		case physical.PutOperation:
			s.indexKIDToVID[kid] = s.scanner.ComputeVIDWithSealWrap(e.Value, e.SealWrap)
			s.indexKIDToKey[kid] = e.Key
		case physical.DeleteOperation:
			delete(s.indexKIDToVID, kid)
			delete(s.indexKIDToKey, kid)
		}
		if e.RaftIndex > lastIdx {
			lastIdx = e.RaftIndex
		}
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}

	s.indexInitialized = true
	s.indexLastFullScan = time.Now().UTC()
	if lastIdx > 0 {
		s.indexApplied.Store(lastIdx)
	}
	return count, nil
}

// WarmIndex proactively builds the in-memory KID/VID index. It first
// attempts a fast path by replaying the local stream journal (sequential
// disk reads). If that succeeds the index is immediately usable. A
// background full physical scan is scheduled only when the journal is
// unavailable or empty. This ensures that after a leader election the
// new leader can serve checkpoints almost immediately.
func (s *drReplicationPrimary) WarmIndex(ctx context.Context) {
	s.indexMu.RLock()
	initialized := s.indexInitialized
	s.indexMu.RUnlock()
	if initialized {
		return
	}

	// Fast path: try journal-based warmup first.
	if s.streamJournal != nil {
		start := time.Now()
		count, err := s.WarmIndexFromJournal(s.streamJournal)
		if err == nil && count > 0 {
			s.logger.Info("checkpoint index warmed from journal",
				"keys", len(s.indexKIDToVID),
				"journal_entries_replayed", count,
				"duration", time.Since(start).Round(time.Millisecond))
			metrics.SetGauge([]string{"replication", "dr", "checkpoint", "index_keys"}, float32(len(s.indexKIDToVID)))
			return
		}
		if err != nil {
			s.logger.Debug("journal-based index warmup unavailable, falling back to full scan", "error", err)
		}
	}

	// Slow path: full physical scan in background.
	go func() {
		start := time.Now()
		s.logger.Info("warming checkpoint index via full scan in background")

		idx := s.indexApplied.Load()
		checkpoint := reconciler.Checkpoint{CommitIndex: idx}
		rs, err := s.scanner.ScanPhysical(ctx, s.core.physical, checkpoint)
		if err != nil {
			if ctx.Err() != nil {
				return // node stepped down; expected
			}
			s.logger.Warn("background index warm-up failed", "error", err)
			return
		}
		s.resetIndexFromSet(rs)
		s.logger.Info("checkpoint index warmed via full scan", "keys", rs.KeyCount, "duration", time.Since(start).Round(time.Millisecond))
		metrics.SetGauge([]string{"replication", "dr", "checkpoint", "index_keys"}, float32(rs.KeyCount))
	}()
}

func (s *drReplicationPrimary) applyRuntimeTuning(cfg *DRConfig) {
	if cfg == nil {
		return
	}

	s.checkpointMu.Lock()
	if cfg.CheckpointTTLSeconds > 0 {
		s.checkpointTTL = time.Duration(cfg.CheckpointTTLSeconds) * time.Second
	}
	if cfg.CheckpointGlobalBudgetBytes > 0 {
		s.checkpointGlobalBudget = cfg.CheckpointGlobalBudgetBytes
	}
	if cfg.CheckpointPerRelBudgetBytes > 0 {
		s.checkpointPerRelationshipBudget = cfg.CheckpointPerRelBudgetBytes
	}
	s.checkpointMu.Unlock()

	s.bufMu.Lock()
	if cfg.StreamBufferMaxEntries > 0 {
		s.bufMaxSize = cfg.StreamBufferMaxEntries
	}
	if cfg.StreamBufferMaxBytes > 0 {
		s.bufMaxBytes = cfg.StreamBufferMaxBytes
	}
	// Re-enforce bounds after runtime update.
	for len(s.changeBuffer) > 0 && (len(s.changeBuffer) > s.bufMaxSize || s.bufBytes > s.bufMaxBytes) {
		dropped := s.changeBuffer[0]
		s.changeBuffer = s.changeBuffer[1:]
		dropBytes := entryChangeBytes(dropped)
		if dropBytes <= s.bufBytes {
			s.bufBytes -= dropBytes
		} else {
			s.bufBytes = 0
		}
	}
	s.bufMu.Unlock()

	if s.streamJournal != nil {
		applyJournal := cfg.StreamJournalEnabled ||
			cfg.StreamJournalMaxBytes > 0 ||
			cfg.StreamJournalSegmentBytes > 0 ||
			cfg.StreamJournalRetentionSecs > 0
		if applyJournal {
			retention := time.Duration(cfg.StreamJournalRetentionSecs) * time.Second
			if err := s.streamJournal.configure(cfg.StreamJournalEnabled, cfg.StreamJournalMaxBytes, cfg.StreamJournalSegmentBytes, retention); err != nil {
				s.logger.Warn("failed to apply stream journal tuning", "error", err)
			}
		}
	}

	if s.checkpointArtifacts != nil {
		artifactEnabled := cfg.CheckpointArtifactEnabled
		if !artifactEnabled &&
			cfg.CheckpointArtifactGlobalBudgetBytes == 0 &&
			cfg.CheckpointArtifactPerRelBudgetBytes == 0 &&
			cfg.CheckpointArtifactTTLSeconds == 0 &&
			cfg.CheckpointArtifactSegmentBytes == 0 {
			artifactEnabled = true
		}
		ttl := s.checkpointTTL
		if cfg.CheckpointArtifactTTLSeconds > 0 {
			ttl = time.Duration(cfg.CheckpointArtifactTTLSeconds) * time.Second
		}
		global := cfg.CheckpointArtifactGlobalBudgetBytes
		if global == 0 {
			global = drCheckpointArtifactDefaultGlobalBudget
		}
		perRel := cfg.CheckpointArtifactPerRelBudgetBytes
		if perRel == 0 {
			perRel = drCheckpointArtifactDefaultPerRelBudget
		}
		seg := cfg.CheckpointArtifactSegmentBytes
		if seg == 0 {
			seg = drCheckpointArtifactDefaultSegmentBytes
		}
		s.checkpointArtifacts.configure(artifactEnabled, ttl, global, perRel, seg)
	}

	if cfg.DRBackpressureEnabled || cfg.DRBackpressureDegradedRatio > 0 || cfg.DRBackpressureCriticalRatio > 0 ||
		cfg.DRBackpressureMinLagEntries > 0 || cfg.DRBackpressureHorizonSeconds > 0 ||
		cfg.DRBackpressureDegradedMinQPS > 0 || cfg.DRBackpressureCriticalMinQPS > 0 {
		s.backpressureEnabled = cfg.DRBackpressureEnabled
		if cfg.DRBackpressureDegradedRatio > 0 {
			s.backpressureDegraded = cfg.DRBackpressureDegradedRatio
		}
		if cfg.DRBackpressureCriticalRatio > 0 {
			s.backpressureCritical = cfg.DRBackpressureCriticalRatio
		}
		if cfg.DRBackpressureMinLagEntries > 0 {
			s.backpressureMinLag = cfg.DRBackpressureMinLagEntries
		}
		if cfg.DRBackpressureHorizonSeconds > 0 {
			s.backpressureHorizonSec = cfg.DRBackpressureHorizonSeconds
		}
		if cfg.DRBackpressureDegradedMinQPS > 0 {
			s.backpressureMinQPSDeg = cfg.DRBackpressureDegradedMinQPS
		}
		if cfg.DRBackpressureCriticalMinQPS > 0 {
			s.backpressureMinQPSCrit = cfg.DRBackpressureCriticalMinQPS
		}
	}
}

func entryChangeBytes(e physical.ChangeStreamEntry) uint64 {
	return uint64(len(e.Key) + len(e.Value) + 48)
}

func entryChangeFromPhysical(e physical.ChangeStreamEntry) *EntryChange {
	return &EntryChange{
		OpType:    string(e.OpType),
		Key:       e.Key,
		Value:     e.Value,
		SealWrap:  e.SealWrap,
		RaftIndex: e.RaftIndex,
	}
}

func entryChangeWireBytes(e *EntryChange) uint64 {
	if e == nil {
		return 0
	}
	return uint64(len(e.OpType)+len(e.Key)+len(e.Value)+len(e.Kid)) + 64
}

func entryBatchBaseWireBytes(b *EntryBatch) uint64 {
	if b == nil {
		return 0
	}
	return uint64(len(b.CheckpointId)) + 32
}

func entryBatchWireBytes(b *EntryBatch) uint64 {
	if b == nil {
		return 0
	}
	n := entryBatchBaseWireBytes(b)
	for _, e := range b.GetEntries() {
		n += entryChangeWireBytes(e)
	}
	n += uint64(len(b.GetFailedKids())) * (32 + 8)
	return n
}

func (s *drReplicationPrimary) updateIndexFromChanges(entries []physical.ChangeStreamEntry) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for _, e := range entries {
		kid := s.scanner.ComputeKID(e.Key)
		switch e.OpType {
		case physical.PutOperation:
			s.indexKIDToVID[kid] = s.scanner.ComputeVIDWithSealWrap(e.Value, e.SealWrap)
			s.indexKIDToKey[kid] = e.Key
		case physical.DeleteOperation:
			delete(s.indexKIDToVID, kid)
			delete(s.indexKIDToKey, kid)
		}
	}
}

func parseRangeSpan(span *RangeSpan) (reconciler.RangeSpan, bool, error) {
	if span == nil {
		return reconciler.RangeSpan{}, false, fmt.Errorf("nil range span")
	}
	if len(span.StartKid) != 32 || len(span.EndKid) != 32 {
		return reconciler.RangeSpan{}, true, fmt.Errorf("start_kid/end_kid must be 32 bytes")
	}
	if span.SplitDepth > uint32(drRangeMaxSplitDepth) {
		return reconciler.RangeSpan{}, true, fmt.Errorf("range split depth %d exceeds maximum %d", span.SplitDepth, drRangeMaxSplitDepth)
	}
	var rs reconciler.RangeSpan
	copy(rs.StartKID[:], span.StartKid)
	copy(rs.EndKID[:], span.EndKid)
	rs.SplitDepth = span.SplitDepth
	if !rs.Valid() {
		return reconciler.RangeSpan{}, true, fmt.Errorf("range start must be <= end")
	}
	return rs, true, nil
}

func parseRangeSpans(spans []*RangeSpan) ([]reconciler.RangeSpan, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	out := make([]reconciler.RangeSpan, 0, len(spans))
	for _, s := range spans {
		rs, ok, err := parseRangeSpan(s)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("nil range span")
		}
		out = append(out, rs)
	}
	return out, nil
}

func kidInAnyRange(kid [32]byte, spans []reconciler.RangeSpan) bool {
	for _, span := range spans {
		if span.Contains(kid) {
			return true
		}
	}
	return false
}

func estimateCheckpointBytes(entry *drCheckpointCacheEntry) uint64 {
	if entry == nil {
		return 0
	}
	// sources under sustained load.
	var metaBytes uint64
	var valueBytes uint64

	metaBytes += uint64(len(entry.kidToKey)) * (32 + 64)
	metaBytes += uint64(len(entry.kidToVID)) * (32 + 32 + 16)
	for _, key := range entry.kidToKey {
		metaBytes += uint64(len(key))
	}
	entry.metaBytes = metaBytes
	entry.valueBytes = valueBytes

	total := metaBytes + valueBytes
	return total
}

func (s *drReplicationPrimary) setCheckpointCacheGaugesLocked() {
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "cache_bytes"}, float32(s.checkpointBytes))
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "cache_items"}, float32(len(s.checkpoints)))
}

func (s *drReplicationPrimary) evictCheckpointLocked(id string) bool {
	if s.activeCheckpointRefs[id] > 0 {
		return false
	}
	cp, ok := s.checkpoints[id]
	if !ok {
		return false
	}
	if s.checkpointArtifacts != nil {
		s.checkpointArtifacts.delete(id)
	}
	delete(s.checkpoints, id)
	if cp.estimatedBytes <= s.checkpointBytes {
		s.checkpointBytes -= cp.estimatedBytes
	} else {
		s.checkpointBytes = 0
	}
	if cp.metaBytes <= s.checkpointMetaBytes {
		s.checkpointMetaBytes -= cp.metaBytes
	} else {
		s.checkpointMetaBytes = 0
	}
	if cp.valueBytes <= s.checkpointValueBytes {
		s.checkpointValueBytes -= cp.valueBytes
	} else {
		s.checkpointValueBytes = 0
	}

	s.checkpointEvictions.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "cache_evictions"}, 1)
	if cp != nil {
		if latest, ok := s.latestCheckpointByRelationship[cp.relationshipID]; ok && latest == id {
			delete(s.latestCheckpointByRelationship, cp.relationshipID)
		}
	}
	return true
}

func (s *drReplicationPrimary) relationshipBytesLocked(relationshipID string) uint64 {
	var total uint64
	for _, cp := range s.checkpoints {
		if cp.relationshipID == relationshipID {
			total += cp.estimatedBytes
		}
	}
	return total
}

func (s *drReplicationPrimary) countRelationshipCheckpointsLocked(relationshipID string) int {
	count := 0
	for _, cp := range s.checkpoints {
		if cp.relationshipID == relationshipID {
			count++
		}
	}
	return count
}

func (s *drReplicationPrimary) evictOldestInRelationshipLocked(relationshipID string) bool {
	var oldestID string
	var oldestTime time.Time
	for id, cp := range s.checkpoints {
		if cp.relationshipID != relationshipID {
			continue
		}
		if s.activeCheckpointRefs[id] > 0 {
			continue
		}
		if oldestID == "" || cp.createdAt.Before(oldestTime) {
			oldestID = id
			oldestTime = cp.createdAt
		}
	}
	if oldestID == "" {
		return false
	}
	return s.evictCheckpointLocked(oldestID)
}

func (s *drReplicationPrimary) evictOldestGlobalLocked() bool {
	var oldestID string
	var oldestTime time.Time
	for id, cp := range s.checkpoints {
		if s.activeCheckpointRefs[id] > 0 {
			continue
		}
		if oldestID == "" || cp.createdAt.Before(oldestTime) {
			oldestID = id
			oldestTime = cp.createdAt
		}
	}
	if oldestID == "" {
		return false
	}
	return s.evictCheckpointLocked(oldestID)
}

func (s *drReplicationPrimary) cacheCheckpoint(entry *drCheckpointCacheEntry) error {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()

	entry.estimatedBytes = estimateCheckpointBytes(entry)
	now := time.Now().UTC()
	s.pruneCheckpointsLocked(now)

	if entry.estimatedBytes > s.checkpointPerRelationshipBudget {
		s.checkpointAdmissionFailures.Add(1)
		s.setCheckpointCacheGaugesLocked()
		return fmt.Errorf("size_exceeded: checkpoint size %d exceeds per-relationship budget %d", entry.estimatedBytes, s.checkpointPerRelationshipBudget)
	}
	if entry.estimatedBytes > s.checkpointGlobalBudget {
		s.checkpointAdmissionFailures.Add(1)
		s.setCheckpointCacheGaugesLocked()
		return fmt.Errorf("size_exceeded: checkpoint size %d exceeds global budget %d", entry.estimatedBytes, s.checkpointGlobalBudget)
	}

	for s.countRelationshipCheckpointsLocked(entry.relationshipID) >= s.maxCheckpointsPerRelationship {
		if !s.evictOldestInRelationshipLocked(entry.relationshipID) {
			break
		}
	}

	for s.relationshipBytesLocked(entry.relationshipID)+entry.estimatedBytes > s.checkpointPerRelationshipBudget {
		if !s.evictOldestInRelationshipLocked(entry.relationshipID) {
			break
		}
	}

	for s.checkpointBytes+entry.estimatedBytes > s.checkpointGlobalBudget {
		if !s.evictOldestGlobalLocked() {
			break
		}
	}

	if s.relationshipBytesLocked(entry.relationshipID)+entry.estimatedBytes > s.checkpointPerRelationshipBudget {
		s.checkpointAdmissionFailures.Add(1)
		s.setCheckpointCacheGaugesLocked()
		return fmt.Errorf("per_rel_budget_exhausted: per-relationship checkpoint budget exhausted")
	}
	if s.checkpointBytes+entry.estimatedBytes > s.checkpointGlobalBudget {
		s.checkpointAdmissionFailures.Add(1)
		s.setCheckpointCacheGaugesLocked()
		return fmt.Errorf("global_budget_exhausted: global checkpoint cache budget exhausted")
	}

	if len(s.checkpoints) >= s.maxCheckpoints {
		_ = s.evictOldestGlobalLocked()
	}

	if old, ok := s.checkpoints[entry.checkpoint.ID]; ok {
		if old.estimatedBytes <= s.checkpointBytes {
			s.checkpointBytes -= old.estimatedBytes
		} else {
			s.checkpointBytes = 0
		}
		if old.metaBytes <= s.checkpointMetaBytes {
			s.checkpointMetaBytes -= old.metaBytes
		} else {
			s.checkpointMetaBytes = 0
		}
		if old.valueBytes <= s.checkpointValueBytes {
			s.checkpointValueBytes -= old.valueBytes
		} else {
			s.checkpointValueBytes = 0
		}
	}
	s.checkpoints[entry.checkpoint.ID] = entry
	s.checkpointBytes += entry.estimatedBytes
	s.checkpointMetaBytes += entry.metaBytes
	s.checkpointValueBytes += entry.valueBytes
	s.setCheckpointCacheGaugesLocked()
	return nil
}

func (s *drReplicationPrimary) getCheckpoint(id string) (*drCheckpointCacheEntry, error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()

	now := time.Now().UTC()
	s.pruneCheckpointsLocked(now)

	entry, ok := s.checkpoints[id]
	if !ok {
		return nil, fmt.Errorf("unknown checkpoint id %q", id)
	}
	if now.Sub(entry.createdAt) > s.checkpointTTL {
		_ = s.evictCheckpointLocked(id)
		return nil, fmt.Errorf("checkpoint %q expired", id)
	}
	return entry, nil
}

func (s *drReplicationPrimary) getCheckpointLease(id string) (*drCheckpointCacheEntry, func(), error) {
	s.checkpointMu.Lock()
	now := time.Now().UTC()
	s.pruneCheckpointsLocked(now)

	entry, ok := s.checkpoints[id]
	if !ok {
		s.checkpointMu.Unlock()
		return nil, nil, fmt.Errorf("unknown checkpoint id %q", id)
	}
	if now.Sub(entry.createdAt) > s.checkpointTTL {
		_ = s.evictCheckpointLocked(id)
		s.checkpointMu.Unlock()
		return nil, nil, fmt.Errorf("checkpoint %q expired", id)
	}
	s.activeCheckpointRefs[id]++
	retainedArtifact := false
	if s.checkpointArtifacts != nil {
		retainedArtifact = s.checkpointArtifacts.retain(id)
	}
	s.checkpointMu.Unlock()

	release := func() {
		if retainedArtifact && s.checkpointArtifacts != nil {
			s.checkpointArtifacts.release(id)
		}
		s.checkpointMu.Lock()
		if refs := s.activeCheckpointRefs[id]; refs <= 1 {
			delete(s.activeCheckpointRefs, id)
		} else {
			s.activeCheckpointRefs[id] = refs - 1
		}
		s.checkpointMu.Unlock()
	}
	return entry, release, nil
}

func (s *drReplicationPrimary) pruneCheckpointsLocked(now time.Time) {
	for id, cp := range s.checkpoints {
		if s.activeCheckpointRefs[id] > 0 {
			continue
		}
		if now.Sub(cp.createdAt) > s.checkpointTTL {
			_ = s.evictCheckpointLocked(id)
		}
	}
	s.setCheckpointCacheGaugesLocked()
}

func (s *drReplicationPrimary) checkpointCacheStats() (bytes uint64, items int, evictions uint64, metaBytes uint64, valueBytes uint64, admissionFailures uint64) {
	s.checkpointMu.RLock()
	defer s.checkpointMu.RUnlock()
	return s.checkpointBytes, len(s.checkpoints), s.checkpointEvictions.Load(), s.checkpointMetaBytes, s.checkpointValueBytes, s.checkpointAdmissionFailures.Load()
}

func (s *drReplicationPrimary) streamBufferStats() (entries int, bytes uint64) {
	s.bufMu.RLock()
	defer s.bufMu.RUnlock()
	return len(s.changeBuffer), s.bufBytes
}

func (s *drReplicationPrimary) streamBufferLimits() (entries int, bytes uint64) {
	s.bufMu.RLock()
	defer s.bufMu.RUnlock()
	return s.bufMaxSize, s.bufMaxBytes
}

func (s *drReplicationPrimary) streamBufferHorizonSeconds() int64 {
	entries, _ := s.streamBufferStats()
	if entries <= 0 {
		return 0
	}
	s.writeRateMu.Lock()
	eps := s.writeRateEPS
	s.writeRateMu.Unlock()
	if eps <= 0 {
		return 0
	}
	seconds := float64(entries) / eps
	if seconds < 0 {
		return 0
	}
	return int64(seconds)
}

func (s *drReplicationPrimary) shouldThrottleCheckpointBuild() (bool, string) {
	entries, bytes := s.streamBufferStats()
	maxEntries, maxBytes := s.streamBufferLimits()
	laggingActive := s.laggingSubscribersActiveCount()
	activeSubscribers := s.subscriberCount()
	if maxEntries <= 0 || maxBytes == 0 {
		return false, ""
	}
	entryPct := (entries * 100) / maxEntries
	bytePct := (bytes * 100) / maxBytes
	if (entryPct >= drCheckpointThrottleBufferPct || bytePct >= drCheckpointThrottleBufferPct) &&
		laggingActive > 0 && activeSubscribers > 0 {
		return true, fmt.Sprintf("buffer=%d%% bytes=%d%% lagging_active=%d subscribers=%d",
			entryPct, bytePct, laggingActive, activeSubscribers)
	}
	return false, ""
}

func (s *drReplicationPrimary) scanFailuresCount() uint64 {
	return s.scanFailures.Load()
}

func (s *drReplicationPrimary) laggingSubscribersCount() uint64 {
	return s.streamLaggingSubscribers.Load()
}

func (s *drReplicationPrimary) laggingSubscribersActiveCount() uint64 {
	active := s.streamLaggingSubscribersActive.Load()
	if active == 0 {
		return 0
	}
	lastUnix := s.streamLaggingLastEventUnix.Load()
	if lastUnix <= 0 {
		return 0
	}
	if time.Since(time.Unix(lastUnix, 0)) > drCheckpointLaggingActiveWindow {
		s.streamLaggingSubscribersActive.Store(0)
		return 0
	}
	return active
}

func (s *drReplicationPrimary) subscriberCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subscribers)
}

func (s *drReplicationPrimary) allowForcedCheckpointBuild(relationshipID string) bool {
	if relationshipID == "" {
		return false
	}
	now := time.Now().UTC()
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	last := s.checkpointLastForcedBuildByRelationship[relationshipID]
	if !last.IsZero() && now.Sub(last) < drCheckpointForceBuildInterval {
		return false
	}
	s.checkpointLastForcedBuildByRelationship[relationshipID] = now
	return true
}

func (s *drReplicationPrimary) checkpointThrottleCount() uint64 {
	return s.checkpointThrottleTotal.Load()
}

func (s *drReplicationPrimary) checkpointThrottleBypassCount() uint64 {
	return s.checkpointThrottleBypassTotal.Load()
}

func (s *drReplicationPrimary) checkpointBuildStats() (inFlight int, maxInFlight int, admissionFailures uint64) {
	s.checkpointMu.RLock()
	inFlight = len(s.checkpointBuildInFlight)
	maxInFlight = s.checkpointBuildMaxInFlight
	s.checkpointMu.RUnlock()
	if maxInFlight <= 0 {
		maxInFlight = drCheckpointBuildMaxInFlight
	}
	return inFlight, maxInFlight, s.checkpointBuildAdmissionFailures.Load()
}

func (s *drReplicationPrimary) tuningSnapshot() (checkpointTTLSeconds int64, checkpointGlobalBudget uint64, checkpointPerRelationshipBudget uint64, streamBufferMaxEntries int, streamBufferMaxBytes uint64) {
	s.checkpointMu.RLock()
	checkpointTTLSeconds = int64(s.checkpointTTL / time.Second)
	checkpointGlobalBudget = s.checkpointGlobalBudget
	checkpointPerRelationshipBudget = s.checkpointPerRelationshipBudget
	s.checkpointMu.RUnlock()

	s.bufMu.RLock()
	streamBufferMaxEntries = s.bufMaxSize
	streamBufferMaxBytes = s.bufMaxBytes
	s.bufMu.RUnlock()

	return
}

func (s *drReplicationPrimary) writeRate() float64 {
	s.writeRateMu.Lock()
	defer s.writeRateMu.Unlock()
	return s.writeRateEPS
}

func (s *drReplicationPrimary) streamJournalSnapshot() (bytes uint64, segments int, oldestIndex uint64) {
	if s.streamJournal == nil {
		return 0, 0, 0
	}
	bytes, segments, oldestIndex, _ = s.streamJournal.stats()
	return
}

func (s *drReplicationPrimary) streamJournalReplayStats() (attempts, success, tooOld uint64) {
	return s.journalReplayAttempts.Load(), s.journalReplaySuccess.Load(), s.journalRangeTooOld.Load()
}

func (s *drReplicationPrimary) checkpointArtifactStats() (bytes uint64, items int, evictions uint64, buildSeconds float64, storageDriftConflicts uint64) {
	if s.checkpointArtifacts == nil {
		return 0, 0, 0, 0, 0
	}
	return s.checkpointArtifacts.stats()
}

func (s *drReplicationPrimary) recordSecondaryPressure(relationshipID string, primaryIndex uint64, appliedIndex uint64, now time.Time) {
	if relationshipID == "" {
		return
	}
	s.pressureMu.Lock()
	defer s.pressureMu.Unlock()

	sample, ok := s.secondaryPressure[relationshipID]
	if !ok {
		sample = &drSecondaryPressureSample{}
		s.secondaryPressure[relationshipID] = sample
	}
	if !sample.lastSeen.IsZero() {
		dt := now.Sub(sample.lastSeen).Seconds()
		if dt > 0 {
			var delta int64
			if appliedIndex >= sample.lastApplied {
				delta = int64(appliedIndex - sample.lastApplied)
			}
			rate := float64(delta) / dt
			if sample.applyRateEPS == 0 {
				sample.applyRateEPS = rate
			} else {
				sample.applyRateEPS = (sample.applyRateEPS * 0.7) + (rate * 0.3)
			}
		}
	}

	sample.lastSeen = now
	sample.lastApplied = appliedIndex
	sample.lastPrimary = primaryIndex
	if primaryIndex > appliedIndex {
		sample.lagEntries = primaryIndex - appliedIndex
	} else {
		sample.lagEntries = 0
	}
}

func (s *drReplicationPrimary) pressureSnapshot(now time.Time) (drBackpressureState, int64, float64, float64, uint64, int64) {
	if !s.backpressureEnabled {
		return drBackpressureHealthy, 0, 0, 0, 0, 0
	}
	p := s.writeRate()
	if p <= 0 {
		return drBackpressureHealthy, 0, p, 0, 0, 0
	}

	s.pressureMu.Lock()
	defer s.pressureMu.Unlock()

	minApply := -1.0
	var maxLag uint64
	active := 0
	for rel, sample := range s.secondaryPressure {
		if sample == nil {
			delete(s.secondaryPressure, rel)
			continue
		}
		if now.Sub(sample.lastSeen) > drCheckpointArtifactStaleHeartbeatWindow {
			delete(s.secondaryPressure, rel)
			continue
		}
		active++
		if minApply < 0 || sample.applyRateEPS < minApply {
			minApply = sample.applyRateEPS
		}
		if sample.lagEntries > maxLag {
			maxLag = sample.lagEntries
		}
	}
	if active == 0 {
		s.backpressureState.Store(uint32(drBackpressureHealthy))
		s.backpressureCapQPS.Store(0)
		return drBackpressureHealthy, 0, p, 0, 0, s.streamBufferHorizonSeconds()
	}
	if minApply < 0 {
		minApply = 0
	}

	minLag := s.backpressureMinLag
	if minLag == 0 {
		s.bufMu.RLock()
		minLag = uint64(2 * s.bufMaxSize)
		s.bufMu.RUnlock()
	}
	horizon := s.streamBufferHorizonSeconds()
	state := drBackpressureHealthy
	ratio := 1.0
	if p > 0 {
		ratio = minApply / p
	}
	if ratio < s.backpressureDegraded && maxLag >= minLag {
		state = drBackpressureDegraded
	}
	if ratio < s.backpressureCritical && maxLag >= (2*minLag) && horizon > 0 && horizon <= s.backpressureHorizonSec {
		state = drBackpressureCritical
	}

	var capQPS int64
	switch state {
	case drBackpressureCritical:
		capQPS = int64(minApply * 0.6)
		if capQPS < s.backpressureMinQPSCrit {
			capQPS = s.backpressureMinQPSCrit
		}
	case drBackpressureDegraded:
		capQPS = int64(minApply * 0.9)
		if capQPS < s.backpressureMinQPSDeg {
			capQPS = s.backpressureMinQPSDeg
		}
	default:
		capQPS = 0
	}
	s.backpressureState.Store(uint32(state))
	s.backpressureCapQPS.Store(capQPS)
	return state, capQPS, p, minApply, maxLag, horizon
}

func (s *drReplicationPrimary) backpressureStatus() (state string, rejections uint64, capQPS int64) {
	return drBackpressureState(s.backpressureState.Load()).String(), s.backpressureRejected.Load(), s.backpressureCapQPS.Load()
}

func (s *drReplicationPrimary) allowWriteRequest(path string) (bool, string) {
	now := time.Now().UTC()
	state, capQPS, p, a, lag, horizon := s.pressureSnapshot(now)
	if state == drBackpressureHealthy || capQPS <= 0 {
		return true, ""
	}

	sec := now.Unix()
	s.pressureMu.Lock()
	if s.backpressureWindowSec != sec {
		s.backpressureWindowSec = sec
		s.backpressureCount = 0
	}
	allowed := s.backpressureCount < capQPS
	if allowed {
		s.backpressureCount++
	}
	s.pressureMu.Unlock()
	if allowed {
		return true, ""
	}

	s.backpressureRejected.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "backpressure", "rejections_total"}, 1)
	return false, fmt.Sprintf(
		"dr backpressure (%s): path=%s primary_write_eps=%.3f secondary_apply_eps=%.3f lag_entries=%d horizon_seconds=%d qps_cap=%d",
		state.String(), path, p, a, lag, horizon, capQPS,
	)
}

func isDRBackpressureExemptPath(path string) bool {
	path = strings.Trim(path, "/")
	if path == "" {
		return true
	}
	if path == "sys/health" || path == "sys/seal-status" ||
		strings.HasPrefix(path, "sys/replication/dr/") {
		return true
	}
	return false
}

func (s *drReplicationPrimary) RevokeRelationship(relationshipID string) {
	s.terminateRelationshipStreams(relationshipID, true)
}

func (s *drReplicationPrimary) TerminateRelationshipStreams(relationshipID string) {
	s.terminateRelationshipStreams(relationshipID, false)
}

func (s *drReplicationPrimary) terminateRelationshipStreams(relationshipID string, revoked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	terminated := uint64(0)
	for id, sub := range s.subscribers {
		if sub.relationshipID != relationshipID {
			continue
		}
		sub.cancel()
		delete(s.subscribers, id)
		terminated++
	}
	if terminated > 0 {
		if revoked {
			s.revokedStreamsTerminated.Add(terminated)
			metrics.IncrCounter([]string{"replication", "dr", "stream", "revoked_streams_terminated"}, float32(terminated))
		} else {
			metrics.IncrCounter([]string{"replication", "dr", "stream", "credential_rotation_streams_terminated"}, float32(terminated))
		}
	}
}

func (s *drReplicationPrimary) revokedStreamsTerminatedCount() uint64 {
	return s.revokedStreamsTerminated.Load()
}

func (s *drReplicationPrimary) authorizeRelationship(ctx context.Context, relationshipID string, states ...DRRelationshipState) error {
	return s.authorizeRelationshipWithActivation(ctx, relationshipID, true, states...)
}

func (s *drReplicationPrimary) authorizeRelationshipNoActivate(ctx context.Context, relationshipID string, states ...DRRelationshipState) error {
	return s.authorizeRelationshipWithActivation(ctx, relationshipID, false, states...)
}

func (s *drReplicationPrimary) authorizeRelationshipWithActivation(ctx context.Context, relationshipID string, activateRegistered bool, states ...DRRelationshipState) error {
	if relationshipID == "" {
		return status.Error(codes.InvalidArgument, "relationship_id is required")
	}

	mgr := s.core.drManager
	if mgr == nil {
		return status.Error(codes.FailedPrecondition, "DR relationship manager not initialized")
	}

	fingerprint, err := peerCertFingerprintFromContext(ctx)
	if err != nil {
		return status.Errorf(codes.PermissionDenied, "failed to verify peer identity: %v", err)
	}

	var validateErr error
	if activateRegistered {
		_, validateErr = mgr.ValidateRelationshipAccess(relationshipID, fingerprint, states...)
	} else {
		_, validateErr = mgr.ValidateRelationshipAccessNoActivate(relationshipID, fingerprint, states...)
	}
	if validateErr != nil {
		return status.Errorf(codes.PermissionDenied, "relationship authorization failed: %v", validateErr)
	}
	return nil
}

func peerCertFingerprintFromContext(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if ok && p != nil {
		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if ok {
			if len(tlsInfo.State.PeerCertificates) == 0 {
				return "", errors.New("peer did not present a certificate")
			}
			return certFingerprintSHA256(tlsInfo.State.PeerCertificates[0]), nil
		}
	}

	// Cluster-listener DR RPCs may not carry gRPC TLSInfo when the transport
	// TLS is terminated by the cluster hook. Fall back to the server-derived
	// fingerprint injected in the connection context.
	if fp, ok := ctx.Value(drPeerFingerprintContextKey{}).(string); ok && fp != "" {
		return fp, nil
	}
	if ok && p != nil && p.Addr != nil {
		if fp, ok := drPeerFingerprintByRemoteAddr.Load(p.Addr.String()); ok {
			if s, ok := fp.(string); ok && s != "" {
				return s, nil
			}
		}
	}

	return "", errors.New("peer is not using TLS")
}

func validateCheckpointRelationship(relationshipID string, cp *drCheckpointCacheEntry) error {
	if cp == nil {
		return status.Error(codes.NotFound, "checkpoint not found")
	}
	if cp.relationshipID == "" || cp.relationshipID != relationshipID {
		return status.Error(codes.PermissionDenied, "checkpoint relationship authorization failed")
	}
	return nil
}

func validateCheckpointTuple(checkpointID string, checkpointIndex uint64, cp *drCheckpointCacheEntry) error {
	if cp == nil {
		return fmt.Errorf("checkpoint is nil")
	}
	if checkpointID == "" {
		return fmt.Errorf("checkpoint_id is required")
	}
	if checkpointID != cp.checkpoint.ID {
		return fmt.Errorf("checkpoint_id mismatch")
	}
	if checkpointIndex == 0 && cp.checkpoint.CommitIndex != 0 {
		return fmt.Errorf("checkpoint_index is required")
	}
	if checkpointIndex != cp.checkpoint.CommitIndex {
		return fmt.Errorf("checkpoint_index mismatch: got %d want %d", checkpointIndex, cp.checkpoint.CommitIndex)
	}
	return nil
}

func (s *drReplicationPrimary) readCheckpointEntryChange(ctx context.Context, cp *drCheckpointCacheEntry, kid [32]byte, expectedVID *[32]byte, includeDeletes bool) (*EntryChange, error) {
	_ = ctx
	cpVID, ok := cp.kidToVID[kid]
	if !ok {
		if includeDeletes {
			return &EntryChange{OpType: string(physical.DeleteOperation), Kid: kid[:]}, nil
		}
		return nil, nil
	}
	if expectedVID != nil && *expectedVID != cpVID {
		return nil, status.Error(codes.FailedPrecondition, "checkpoint_provenance_mismatch: expected_vid does not match checkpoint artifact")
	}
	if s.checkpointArtifacts == nil {
		return nil, status.Error(codes.FailedPrecondition, "checkpoint_artifact_missing: checkpoint artifacts are not enabled")
	}

	rec, found, err := s.checkpointArtifacts.getRecord(cp.checkpoint.ID, kid)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint_artifact_missing: %v", err)
	}
	if !found {
		if includeDeletes {
			return &EntryChange{OpType: string(physical.DeleteOperation), Kid: kid[:]}, nil
		}
		return nil, nil
	}
	if rec.VID != cpVID {
		return nil, status.Error(codes.FailedPrecondition, "checkpoint_provenance_mismatch: artifact VID does not match checkpoint metadata")
	}
	if rec.Tombstone {
		if includeDeletes {
			return &EntryChange{
				OpType: string(physical.DeleteOperation),
				Key:    rec.Key,
				Kid:    kid[:],
			}, nil
		}
		return nil, nil
	}

	value, err := s.checkpointArtifacts.readValue(cp.checkpoint.ID, rec.ValueRef)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint_artifact_missing: %v", err)
	}
	return &EntryChange{
		OpType:   string(physical.PutOperation),
		Key:      rec.Key,
		Value:    value,
		SealWrap: rec.SealWrap,
		Kid:      kid[:],
	}, nil
}

// Verify drReplicationPrimary implements the gRPC server interface.
var _ DRReplicationServer = (*drReplicationPrimary)(nil)
