// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/openbao/openbao/physical/replication/sketch"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drCheckpointGlobalBudgetBytes          = 1024 << 20 // 1 GiB
	drCheckpointPerRelationshipBudgetBytes = 256 << 20  // 256 MiB
	drCheckpointMaxPerRelationship         = 8
	drCheckpointTTL                        = 30 * time.Minute
	drRangeMaxIBLTCellsPerRange            = 32768
	drStreamBufferMaxEntries               = 10000
	drStreamBufferMaxBytes                 = 64 << 20 // 64 MiB
	drCheckpointThrottleBufferPct          = 85
	drCheckpointLaggingActiveWindow        = 10 * time.Second
	drCheckpointForceBuildInterval         = 15 * time.Second
)

// drReplicationPrimary implements the DRReplicationServer gRPC interface
// on the primary side. It provides:
//   - Change streaming (normal-mode replication)
//   - IBLT/strata/prefix digest reconciliation (recovery mode)
//   - Entry fetching for divergent keys
type drReplicationPrimary struct {
	UnimplementedDRReplicationServer

	logger  log.Logger
	scanner *reconciler.Scanner
	core    *Core

	// subscribers tracks active change stream subscribers.
	mu          sync.RWMutex
	subscribers map[string]*changeStreamSubscriber

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
	// streamLaggingSubscribers counts forced disconnects due to subscriber lag.
	streamLaggingSubscribers atomic.Uint64
	// streamLaggingSubscribersActive tracks recent lagging pressure in a short window.
	streamLaggingSubscribersActive atomic.Uint64
	streamLaggingLastEventUnix     atomic.Int64

	checkpointMu                    sync.RWMutex
	checkpoints                     map[string]*drCheckpointCacheEntry
	checkpointTTL                   time.Duration
	maxCheckpoints                  int
	checkpointBytes                 uint64
	checkpointMetaBytes             uint64
	checkpointValueBytes            uint64
	checkpointManifestBytes         uint64
	checkpointDerivedBytes          uint64
	checkpointEvictions             atomic.Uint64
	checkpointAdmissionFailures     atomic.Uint64
	checkpointThrottleTotal         atomic.Uint64
	checkpointGlobalBudget          uint64
	checkpointPerRelationshipBudget uint64
	maxCheckpointsPerRelationship   int
	rangePlanConfig                 reconciler.RangePlanConfig
	// checkpointBuildInFlight de-amplifies parallel checkpoint requests
	// for the same relationship.
	checkpointBuildInFlight        map[string]*drCheckpointBuildResult
	latestCheckpointByRelationship map[string]string
	// checkpointLastForcedBuildByRelationship prevents permanent reconcile
	// starvation under sustained stream pressure by allowing occasional builds.
	checkpointLastForcedBuildByRelationship map[string]time.Time

	revokedStreamsTerminated      atomic.Uint64
	checkpointThrottleBypassTotal atomic.Uint64

	writeRateMu            sync.Mutex
	writeRateWindowStart   time.Time
	writeRateWindowEntries uint64
	writeRateEPS           float64
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
	derivedBytes     uint64
	prefixDigest     *sketch.PrefixDigest
	kidToKey         map[[32]byte]string
	kidToVID         map[[32]byte][32]byte
	topRanges        []reconciler.RangeDescriptor
	rangePlanVersion uint32
}

// changeStreamSubscriber represents a connected secondary that is
// receiving change stream updates.
type changeStreamSubscriber struct {
	id             string
	relationshipID string
	ch             chan physical.ChangeStreamEntry
	cancel         context.CancelFunc
}

// NewDRReplicationPrimary creates a new DR replication gRPC server for
// the primary side.
func NewDRReplicationPrimary(core *Core, replSalt []byte, logger log.Logger) *drReplicationPrimary {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	config := reconciler.DefaultScanConfig(replSalt)
	config.BuildKIDMap = true // Primary needs reverse KID -> key mapping
	// Use metadata-first checkpoints to avoid retaining full values in cache.
	config.BuildEntryMap = false
	config.RequireTransactionalSnapshot = true
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

	return &drReplicationPrimary{
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
		latestCheckpointByRelationship:          make(map[string]string),
		checkpointLastForcedBuildByRelationship: make(map[string]time.Time),
	}
}

// OnChange is called by the FSM change stream hook when storage
// mutations are applied. It distributes changes to all subscribers
// and appends to the ring buffer.
func (s *drReplicationPrimary) OnChange(entries []physical.ChangeStreamEntry) {
	// Filter out cluster-local keys that are never replicated.
	replicableEntries := make([]physical.ChangeStreamEntry, 0, len(entries))
	for _, e := range entries {
		if isDRNeverReplicatePath(e.Key) {
			continue
		}
		replicableEntries = append(replicableEntries, e)
	}
	if len(replicableEntries) == 0 {
		return
	}

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
// The primary streams storage mutations to the secondary in real time.
func (s *drReplicationPrimary) StreamChanges(req *StreamChangesRequest, stream grpc.ServerStreamingServer[EntryChange]) error {
	if err := s.authorizeRelationship(stream.Context(), req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return err
	}

	subID, err := uuid.GenerateUUID()
	if err != nil {
		return fmt.Errorf("failed to generate subscriber ID: %w", err)
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	sub := &changeStreamSubscriber{
		id:             subID,
		relationshipID: req.RelationshipId,
		ch:             make(chan physical.ChangeStreamEntry, s.bufMaxSize),
		cancel:         cancel,
	}

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
		"last_applied_index", req.LastAppliedIndex)

	// Check if the buffer can satisfy catch-up, and send buffered changes.
	//
	// Resume is inclusive on last_applied_index because EntryChange is emitted
	// per storage operation while the cursor is a Raft index. A reconnect may
	// occur after applying one operation at index N but before applying the
	// remaining operations at the same index.
	s.bufMu.RLock()
	if len(s.changeBuffer) > 0 {
		oldestIdx := s.changeBuffer[0].RaftIndex
		resumeFrom := req.LastAppliedIndex
		if resumeFrom < oldestIdx {
			s.bufMu.RUnlock()
			s.logger.Warn("buffer cannot satisfy catch-up, secondary needs reconciliation",
				"requested_from", resumeFrom,
				"oldest_buffered", oldestIdx)
			return status.Errorf(codes.FailedPrecondition, "buffer too old: secondary missing from %d (inclusive), oldest buffered %d; reconciliation required",
				resumeFrom, oldestIdx)
		}
	}
	for _, e := range s.changeBuffer {
		if e.RaftIndex >= req.LastAppliedIndex {
			if err := stream.Send(entryChangeFromPhysical(e)); err != nil {
				s.bufMu.RUnlock()
				return err
			}
		}
	}
	s.bufMu.RUnlock()

	// Stream live changes.
	authTicker := time.NewTicker(5 * time.Second)
	defer authTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-authTicker.C:
			if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
				return err
			}
		case entry := <-sub.ch:
			if err := stream.Send(entryChangeFromPhysical(entry)); err != nil {
				return err
			}
		}
	}
}

// RequestCheckpoint implements DRReplicationServer.RequestCheckpoint.
func (s *drReplicationPrimary) RequestCheckpoint(ctx context.Context, req *CheckpointRequest) (*CheckpointResponse, error) {
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return nil, err
	}

	// Get current Raft commit index from the underlying backend.
	var commitIndex uint64
	if rb, ok := s.core.underlyingPhysical.(*raft.RaftBackend); ok {
		commitIndex = rb.AppliedIndex()
	}

	for {
		if resp, ok := s.reuseCheckpointResponse(req.RelationshipId, commitIndex); ok {
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

		wait, owner := s.claimCheckpointBuild(req.RelationshipId)
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
		return resp, err
	}
}

// ExchangeIBLT implements DRReplicationServer.ExchangeIBLT.
func (s *drReplicationPrimary) ExchangeIBLT(ctx context.Context, req *IBLTMessage) (*IBLTMessage, error) {
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}
	if err := s.authorizeCheckpoint(ctx, cp); err != nil {
		return nil, err
	}

	s.logger.Info("building IBLT from checkpoint cache",
		"checkpoint_id", req.CheckpointId,
		"requested_cells", req.NumCells)

	numCells := req.NumCells
	if numCells < sketch.DefaultHashCount {
		numCells = sketch.DefaultHashCount
	}
	if numCells > drRangeMaxIBLTCellsPerRange {
		numCells = drRangeMaxIBLTCellsPerRange
	}

	span, hasSpan, err := parseRangeSpan(req.GetSpan())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid range span: %v", err)
	}
	iblt := sketch.NewIBLT(numCells, sketch.DefaultHashCount)
	if hasSpan {
		iblt = reconciler.BuildRangeIBLTFromMap(cp.kidToVID, span, numCells)
	} else {
		for kid, vid := range cp.kidToVID {
			iblt.Insert(kid, vid)
		}
	}

	return &IBLTMessage{
		CheckpointId:    req.CheckpointId,
		NumCells:        iblt.NumCells(),
		IbltData:        iblt.Marshal(),
		Span:            req.GetSpan(),
		CheckpointIndex: cp.checkpoint.CommitIndex,
	}, nil
}

// ExchangeRangeDigests implements DRReplicationServer.ExchangeRangeDigests.
// It returns digest metadata for explicit spans from an immutable checkpoint.
func (s *drReplicationPrimary) ExchangeRangeDigests(ctx context.Context, req *RangeDigestRequest) (*RangeDigestResponse, error) {
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}
	if err := s.authorizeCheckpoint(ctx, cp); err != nil {
		return nil, err
	}
	if len(req.GetSpans()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one span is required")
	}

	spans, err := parseRangeSpans(req.GetSpans())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid spans: %v", err)
	}

	resp := &RangeDigestResponse{
		Ranges: make([]*RangeDigest, 0, len(spans)),
	}
	for _, span := range spans {
		desc := reconciler.BuildRangeDigestFromMap(cp.kidToVID, nil, span, drRangeMaxIBLTCellsPerRange)
		resp.Ranges = append(resp.Ranges, rangeDescriptorToProto(desc))
	}

	return resp, nil
}

// ExchangePrefixDigests implements DRReplicationServer.ExchangePrefixDigests.
func (s *drReplicationPrimary) ExchangePrefixDigests(ctx context.Context, req *PrefixDigestRequest) (*PrefixDigestResponse, error) {
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}
	if err := s.authorizeCheckpoint(ctx, cp); err != nil {
		return nil, err
	}

	s.logger.Info("building prefix digests from checkpoint cache",
		"checkpoint_id", req.CheckpointId,
		"prefix_length", req.PrefixLength)
	span, hasSpan, err := parseRangeSpan(req.GetSpan())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid range span: %v", err)
	}

	resp := &PrefixDigestResponse{
		PrefixLength: req.PrefixLength,
	}

	prefixLen := req.PrefixLength
	if prefixLen == 0 {
		if cp.prefixDigest != nil {
			prefixLen = cp.prefixDigest.PrefixLen()
		} else {
			prefixLen = 8
		}
	}
	resp.PrefixLength = prefixLen

	var pd *sketch.PrefixDigest
	switch {
	case hasSpan:
		pd = reconciler.BuildRangePrefixDigestFromMap(cp.kidToVID, span, prefixLen)
	case cp.prefixDigest != nil && cp.prefixDigest.PrefixLen() == prefixLen:
		pd = cp.prefixDigest
	default:
		pd = reconciler.BuildRangePrefixDigestFromMap(cp.kidToVID, fullRangeSpan(), prefixLen)
	}

	numBuckets := pd.NumBuckets()
	if len(req.BucketIndices) > 0 {
		// Only return requested buckets.
		for _, idx := range req.BucketIndices {
			if idx < numBuckets {
				b := pd.Bucket(idx)
				resp.Buckets = append(resp.Buckets, bucketToProto(b))
			}
		}
	} else {
		// Return all buckets.
		for i := uint32(0); i < numBuckets; i++ {
			b := pd.Bucket(i)
			resp.Buckets = append(resp.Buckets, bucketToProto(b))
		}
	}

	return resp, nil
}

// FetchEntries implements DRReplicationServer.FetchEntries.
// The secondary requests specific entries by KID and/or by bucket
// indices; the primary looks them up and streams them back.
func (s *drReplicationPrimary) FetchEntries(req *FetchEntriesRequest, stream grpc.ServerStreamingServer[EntryBatch]) error {
	cp, err := s.getCheckpoint(req.CheckpointId)
	if err != nil {
		return status.Errorf(codes.NotFound, "checkpoint not found: %v", err)
	}
	if err := validateCheckpointTuple(req.CheckpointId, req.GetCheckpointIndex(), cp); err != nil {
		return status.Errorf(codes.FailedPrecondition, "invalid checkpoint tuple: %v", err)
	}
	if err := s.authorizeCheckpoint(stream.Context(), cp); err != nil {
		return err
	}

	s.logger.Info("fetching entries",
		"checkpoint_id", req.CheckpointId,
		"num_kids", len(req.Kids),
		"num_items", len(req.Items),
		"bucket_count", len(req.BucketIndices),
		"range_count", len(req.Ranges))
	if req.GetCheckpointIndex() > 0 &&
		len(req.GetItems()) == 0 &&
		len(req.GetKids()) > 0 &&
		len(req.GetBucketIndices()) == 0 &&
		len(req.GetRanges()) == 0 {
		s.logger.Debug("fetch request missing explicit expected_vid items; falling back to checkpoint-only provenance",
			"checkpoint_id", req.CheckpointId,
			"kids", len(req.Kids))
	}

	var batch EntryBatch
	batch.CheckpointId = cp.checkpoint.ID
	batch.CheckpointIndex = cp.checkpoint.CommitIndex
	sent := make(map[[32]byte]bool)
	rangeSpans, err := parseRangeSpans(req.GetRanges())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid ranges: %v", err)
	}
	bucketSet := make(map[uint32]bool, len(req.BucketIndices))
	for _, idx := range req.BucketIndices {
		bucketSet[idx] = true
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
		change, err := s.readCheckpointEntryChange(stream.Context(), cp, kid, expectedVID, req.IncludeDeletes)
		if err != nil {
			if status.Code(err) == codes.FailedPrecondition {
				return err
			}
			batch.FailedKids = append(batch.FailedKids, kid[:])
			return nil
		}
		if change == nil {
			return nil
		}
		batch.Entries = append(batch.Entries, change)
		if len(batch.Entries) >= 100 {
			if err := stream.Send(&batch); err != nil {
				return err
			}
			batch.Entries = batch.Entries[:0]
			batch.FailedKids = batch.FailedKids[:0]
			batch.CheckpointId = cp.checkpoint.ID
			batch.CheckpointIndex = cp.checkpoint.CommitIndex
		}
		return nil
	}

	// Range and/or bucket-based fetching. If both are set, entries must satisfy both predicates.
	if len(rangeSpans) > 0 || (len(bucketSet) > 0 && req.BucketPrefixLength > 0) {
		for kid := range cp.kidToVID {
			if len(rangeSpans) > 0 && !kidInAnyRange(kid, rangeSpans) {
				continue
			}
			if len(bucketSet) > 0 && req.BucketPrefixLength > 0 {
				bucketIdx := sketch.ParentBucket(kid, req.BucketPrefixLength)
				if !bucketSet[bucketIdx] {
					continue
				}
			}
			sent[kid] = true
			if err := emitFromCheckpoint(kid, nil); err != nil {
				return err
			}
		}
	}

	// Provenance-safe point fetches from items.
	for kid, expectedVID := range expectedVIDByKID {
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
	if len(batch.Entries) > 0 || len(batch.FailedKids) > 0 {
		if err := stream.Send(&batch); err != nil {
			return err
		}
	}

	return nil
}

// Heartbeat implements DRReplicationServer.Heartbeat.
func (s *drReplicationPrimary) Heartbeat(ctx context.Context, req *DRHeartbeatRequest) (*DRHeartbeatResponse, error) {
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	if mgr := s.core.drManager; mgr != nil {
		mgr.MarkRelationshipSeen(req.RelationshipId)
	}

	var primaryIndex, primaryTerm uint64
	if rb, ok := s.core.underlyingPhysical.(*raft.RaftBackend); ok {
		primaryIndex = rb.AppliedIndex()
		primaryTerm = rb.Term()
	}

	return &DRHeartbeatResponse{
		PrimaryIndex:     primaryIndex,
		PrimaryTerm:      primaryTerm,
		ReplicationState: uint32(s.core.ReplicationState()),
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
	if err := s.authorizeRelationship(ctx, req.RelationshipId, DRRelationshipStateRegistered, DRRelationshipStateActive); err != nil {
		return nil, err
	}
	if len(req.ClientEphemeralPubkey) == 0 {
		return nil, status.Error(codes.InvalidArgument, "client_ephemeral_pubkey is required")
	}
	if len(req.ClientNonce) != drBootstrapNonceSize {
		return nil, status.Errorf(codes.InvalidArgument, "client_nonce must be %d bytes", drBootstrapNonceSize)
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
	wrappedRootKey, serverPub, wrapNonce, aadVersion, err := wrapRootKeyForSecondary(
		keyring.RootKey(),
		req.RelationshipId,
		req.ClientEphemeralPubkey,
		req.ClientNonce,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to wrap root key: %v", err)
	}

	resp := &SyncKeyringResponse{
		WrappedRootKey:        wrappedRootKey,
		ServerEphemeralPubkey: serverPub,
		WrapNonce:             wrapNonce,
		WrapAadVersion:        aadVersion,
	}
	if keyringEntry != nil {
		resp.KeyringEntry = keyringEntry.Value
	}
	if rootKeyEntry != nil {
		resp.RootKeyEntry = rootKeyEntry.Value
	}

	if mgr := s.core.drManager; mgr != nil {
		mgr.MarkRelationshipActive(req.RelationshipId)
	}

	s.logger.Info("keyring synced to secondary")
	return resp, nil
}

// --- Helpers ---

func (s *drReplicationPrimary) claimCheckpointBuild(relationshipID string) (*drCheckpointBuildResult, bool) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	if in, ok := s.checkpointBuildInFlight[relationshipID]; ok {
		return in, false
	}
	in := &drCheckpointBuildResult{done: make(chan struct{})}
	s.checkpointBuildInFlight[relationshipID] = in
	return in, true
}

func (s *drReplicationPrimary) finishCheckpointBuild(relationshipID string, in *drCheckpointBuildResult, resp *CheckpointResponse, err error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	in.resp = resp
	in.err = err
	close(in.done)
	delete(s.checkpointBuildInFlight, relationshipID)
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
		TopRanges:        rangeDescriptorsToProto(cp.topRanges),
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

	rs, err := s.scanner.Scan(ctx, s.core.barrier, checkpoint)
	if err != nil {
		s.scanFailures.Add(1)
		metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "scan_failures"}, 1)
		return nil, status.Errorf(codes.Internal, "failed to build checkpoint snapshot: %v", err)
	}
	topRanges, err := reconciler.BuildRangeManifest(rs, s.rangePlanConfig)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to build range manifest: %v", err)
	}

	entry := &drCheckpointCacheEntry{
		checkpoint:       checkpoint,
		relationshipID:   relationshipID,
		createdAt:        time.Now().UTC(),
		prefixDigest:     rs.PrefixDigest,
		kidToKey:         rs.KIDToKey,
		kidToVID:         rs.KIDToVID,
		topRanges:        topRanges,
		rangePlanVersion: reconciler.RangePlanVersion,
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
		TopRanges:        rangeDescriptorsToProto(topRanges),
		RangePlanVersion: reconciler.RangePlanVersion,
	}, nil
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

func bucketToProto(b *sketch.Bucket) *BucketDigestProto {
	return &BucketDigestProto{
		Index:        b.Index,
		Count:        b.Count,
		XorKeyHash:   b.XORKeyHash[:],
		XorValueHash: b.XORValueHash[:],
	}
}

func rangeDescriptorsToProto(desc []reconciler.RangeDescriptor) []*RangeDigest {
	if len(desc) == 0 {
		return nil
	}
	out := make([]*RangeDigest, 0, len(desc))
	for _, d := range desc {
		out = append(out, rangeDescriptorToProto(d))
	}
	return out
}

func rangeDescriptorToProto(d reconciler.RangeDescriptor) *RangeDigest {
	return &RangeDigest{
		Span: &RangeSpan{
			StartKid:   d.Span.StartKID[:],
			EndKid:     d.Span.EndKID[:],
			SplitDepth: d.Span.SplitDepth,
		},
		Count:              d.Count,
		XorKeyHash:         d.XORKeyHash[:],
		XorValueHash:       d.XORValueHash[:],
		SuggestedIbltCells: d.SuggestedIBLTCells,
		ApproxValueBytes:   d.ApproxValueBytes,
	}
}

func parseRangeSpan(span *RangeSpan) (reconciler.RangeSpan, bool, error) {
	if span == nil {
		return reconciler.RangeSpan{}, false, nil
	}
	if len(span.StartKid) != 32 || len(span.EndKid) != 32 {
		return reconciler.RangeSpan{}, true, fmt.Errorf("start_kid/end_kid must be 32 bytes")
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
		rs, _, err := parseRangeSpan(s)
		if err != nil {
			return nil, err
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

func fullRangeSpan() reconciler.RangeSpan {
	var span reconciler.RangeSpan
	for i := range span.EndKID {
		span.EndKID[i] = 0xff
	}
	return span
}

func estimateCheckpointBytes(entry *drCheckpointCacheEntry) uint64 {
	if entry == nil {
		return 0
	}
	// Approximate cache footprint by component so status can expose pressure
	// sources under sustained load.
	var metaBytes uint64
	var valueBytes uint64
	var manifestBytes uint64
	var derivedBytes uint64

	if entry.prefixDigest != nil {
		derivedBytes += uint64(entry.prefixDigest.NumBuckets()) * (8 + 32 + 32 + 16)
	}
	metaBytes += uint64(len(entry.kidToKey)) * (32 + 64)
	metaBytes += uint64(len(entry.kidToVID)) * (32 + 32 + 16)
	for _, key := range entry.kidToKey {
		metaBytes += uint64(len(key))
	}
	manifestBytes += uint64(len(entry.topRanges)) * (32 + 32 + 32 + 32 + 8 + 8 + 16)
	// Per-map and object overhead margin.
	derivedBytes += uint64(len(entry.kidToKey)+len(entry.kidToVID)+len(entry.topRanges)) * 32

	entry.metaBytes = metaBytes
	entry.valueBytes = valueBytes
	entry.manifestBytes = manifestBytes
	entry.derivedBytes = derivedBytes

	total := metaBytes + valueBytes + manifestBytes + derivedBytes
	return total
}

func (s *drReplicationPrimary) setCheckpointCacheGaugesLocked() {
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "cache_bytes"}, float32(s.checkpointBytes))
	metrics.SetGauge([]string{"replication", "dr", "checkpoint", "cache_items"}, float32(len(s.checkpoints)))
}

func (s *drReplicationPrimary) evictCheckpointLocked(id string) {
	cp, ok := s.checkpoints[id]
	if !ok {
		return
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
	if cp.manifestBytes <= s.checkpointManifestBytes {
		s.checkpointManifestBytes -= cp.manifestBytes
	} else {
		s.checkpointManifestBytes = 0
	}
	if cp.derivedBytes <= s.checkpointDerivedBytes {
		s.checkpointDerivedBytes -= cp.derivedBytes
	} else {
		s.checkpointDerivedBytes = 0
	}
	s.checkpointEvictions.Add(1)
	metrics.IncrCounter([]string{"replication", "dr", "checkpoint", "cache_evictions"}, 1)
	if cp != nil {
		if latest, ok := s.latestCheckpointByRelationship[cp.relationshipID]; ok && latest == id {
			delete(s.latestCheckpointByRelationship, cp.relationshipID)
		}
	}
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
		if oldestID == "" || cp.createdAt.Before(oldestTime) {
			oldestID = id
			oldestTime = cp.createdAt
		}
	}
	if oldestID == "" {
		return false
	}
	s.evictCheckpointLocked(oldestID)
	return true
}

func (s *drReplicationPrimary) evictOldestGlobalLocked() bool {
	var oldestID string
	var oldestTime time.Time
	for id, cp := range s.checkpoints {
		if oldestID == "" || cp.createdAt.Before(oldestTime) {
			oldestID = id
			oldestTime = cp.createdAt
		}
	}
	if oldestID == "" {
		return false
	}
	s.evictCheckpointLocked(oldestID)
	return true
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
		if old.manifestBytes <= s.checkpointManifestBytes {
			s.checkpointManifestBytes -= old.manifestBytes
		} else {
			s.checkpointManifestBytes = 0
		}
		if old.derivedBytes <= s.checkpointDerivedBytes {
			s.checkpointDerivedBytes -= old.derivedBytes
		} else {
			s.checkpointDerivedBytes = 0
		}
	}
	s.checkpoints[entry.checkpoint.ID] = entry
	s.checkpointBytes += entry.estimatedBytes
	s.checkpointMetaBytes += entry.metaBytes
	s.checkpointValueBytes += entry.valueBytes
	s.checkpointManifestBytes += entry.manifestBytes
	s.checkpointDerivedBytes += entry.derivedBytes
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
		s.evictCheckpointLocked(id)
		return nil, fmt.Errorf("checkpoint %q expired", id)
	}
	return entry, nil
}

func (s *drReplicationPrimary) pruneCheckpointsLocked(now time.Time) {
	for id, cp := range s.checkpoints {
		if now.Sub(cp.createdAt) > s.checkpointTTL {
			s.evictCheckpointLocked(id)
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

func (s *drReplicationPrimary) RevokeRelationship(relationshipID string) {
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
		s.revokedStreamsTerminated.Add(terminated)
		metrics.IncrCounter([]string{"replication", "dr", "stream", "revoked_streams_terminated"}, float32(terminated))
	}
}

func (s *drReplicationPrimary) revokedStreamsTerminatedCount() uint64 {
	return s.revokedStreamsTerminated.Load()
}

func (s *drReplicationPrimary) authorizeRelationship(ctx context.Context, relationshipID string, states ...DRRelationshipState) error {
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

	if _, err := mgr.ValidateRelationshipAccess(relationshipID, fingerprint, states...); err != nil {
		return status.Errorf(codes.PermissionDenied, "relationship authorization failed: %v", err)
	}
	return nil
}

func (s *drReplicationPrimary) authorizeCheckpoint(ctx context.Context, cp *drCheckpointCacheEntry) error {
	return s.authorizeRelationship(ctx, cp.relationshipID, DRRelationshipStateRegistered, DRRelationshipStateActive)
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
	cpVID, ok := cp.kidToVID[kid]
	if !ok {
		if includeDeletes {
			return &EntryChange{
				OpType: string(physical.DeleteOperation),
				Kid:    kid[:],
			}, nil
		}
		return nil, nil
	}
	if expectedVID != nil && *expectedVID != cpVID {
		return nil, status.Error(codes.FailedPrecondition, "checkpoint conflict: expected_vid does not match checkpoint artifact")
	}

	key, ok := cp.kidToKey[kid]
	if !ok || key == "" {
		return nil, status.Error(codes.FailedPrecondition, "checkpoint conflict: missing KID->key mapping")
	}

	entry, err := s.core.barrier.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to read barrier entry %q: %w", key, err)
	}

	var currentVID [32]byte
	if entry == nil {
		_, currentVID = s.scanner.ComputeItemFromEntry(&physical.Entry{Key: key})
	} else {
		currentVID = s.scanner.ComputeVID(entry.Value)
	}

	if currentVID != cpVID {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpoint conflict: storage changed for key %q", key)
	}

	if entry == nil {
		if includeDeletes {
			return &EntryChange{
				OpType: string(physical.DeleteOperation),
				Key:    key,
				Kid:    kid[:],
			}, nil
		}
		return nil, nil
	}

	return &EntryChange{
		OpType:   string(physical.PutOperation),
		Key:      key,
		Value:    entry.Value,
		SealWrap: entry.SealWrap,
	}, nil
}

// Verify drReplicationPrimary implements the gRPC server interface.
var _ DRReplicationServer = (*drReplicationPrimary)(nil)
