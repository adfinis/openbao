// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/physical/replication/reconciler"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drCheckpointArtifactDefaultGlobalBudget  = uint64(8 << 30)  // 8 GiB
	drCheckpointArtifactDefaultPerRelBudget  = uint64(2 << 30)  // 2 GiB
	drCheckpointArtifactDefaultSegmentBytes  = uint64(64 << 20) // 64 MiB
	drCheckpointArtifactDefaultTTL           = 30 * time.Minute
	drCheckpointArtifactBuildFenceTimeout    = 10 * time.Second
	drCheckpointArtifactFencePollInterval    = 10 * time.Millisecond
	drCheckpointArtifactStaleHeartbeatWindow = 30 * time.Second
)

type drCheckpointArtifactRecord struct {
	KID       [32]byte
	VID       [32]byte
	Key       string
	SealWrap  bool
	Tombstone bool
	ValueRef  string
}

type drCheckpointArtifact struct {
	CheckpointID    string
	CheckpointIndex uint64
	RelationshipID  string
	CreatedAt       time.Time
	Path            string
	Bytes           uint64
	Records         map[[32]byte]drCheckpointArtifactRecord
}

type drCheckpointArtifactStore struct {
	logger log.Logger
	dir    string

	mu              sync.RWMutex
	enabled         bool
	ttl             time.Duration
	globalBudget    uint64
	perRelBudget    uint64
	segmentBytes    uint64
	artifacts       map[string]*drCheckpointArtifact
	activeRefs      map[string]int
	totalBytes      uint64
	storageDriftHit atomic.Uint64
	evictions       atomic.Uint64
	lastBuildMS     atomic.Uint64
}

func newDRCheckpointArtifactStore(logger log.Logger, dir string) *drCheckpointArtifactStore {
	if logger == nil {
		logger = log.NewNullLogger()
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "openbao-dr-checkpoint-artifacts")
	}
	return &drCheckpointArtifactStore{
		logger:       logger.Named("dr-checkpoint-artifacts"),
		dir:          dir,
		enabled:      true,
		ttl:          drCheckpointArtifactDefaultTTL,
		globalBudget: drCheckpointArtifactDefaultGlobalBudget,
		perRelBudget: drCheckpointArtifactDefaultPerRelBudget,
		segmentBytes: drCheckpointArtifactDefaultSegmentBytes,
		artifacts:    make(map[string]*drCheckpointArtifact),
		activeRefs:   make(map[string]int),
	}
}

func (s *drCheckpointArtifactStore) configure(enabled bool, ttl time.Duration, globalBudget uint64, perRelBudget uint64, segmentBytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.enabled = enabled
	if ttl > 0 {
		s.ttl = ttl
	}
	if globalBudget > 0 {
		s.globalBudget = globalBudget
	}
	if perRelBudget > 0 {
		s.perRelBudget = perRelBudget
	}
	if segmentBytes > 0 {
		s.segmentBytes = segmentBytes
	}

	if !s.enabled {
		s.evictAllLocked()
	}
}

func (s *drCheckpointArtifactStore) stats() (bytes uint64, items int, evictions uint64, buildSeconds float64, storageDriftConflicts uint64) {
	s.mu.RLock()
	bytes = s.totalBytes
	items = len(s.artifacts)
	s.mu.RUnlock()
	return bytes, items, s.evictions.Load(), float64(s.lastBuildMS.Load()) / 1000.0, s.storageDriftHit.Load()
}

func (s *drCheckpointArtifactStore) getRecord(checkpointID string, kid [32]byte) (drCheckpointArtifactRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	art, ok := s.artifacts[checkpointID]
	if !ok {
		return drCheckpointArtifactRecord{}, false, fmt.Errorf("checkpoint artifact missing")
	}
	if s.ttl > 0 && s.activeRefs[checkpointID] == 0 && time.Since(art.CreatedAt) > s.ttl {
		s.evictLocked(checkpointID)
		return drCheckpointArtifactRecord{}, false, fmt.Errorf("checkpoint artifact expired")
	}
	rec, ok := art.Records[kid]
	return rec, ok, nil
}

func (s *drCheckpointArtifactStore) readValue(checkpointID, valueRef string) ([]byte, error) {
	s.mu.RLock()
	art, ok := s.artifacts[checkpointID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("checkpoint artifact missing")
	}
	if valueRef == "" {
		return nil, nil
	}
	path := filepath.Join(art.Path, "blobs", valueRef+".bin")
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint artifact value: %w", err)
	}
	return value, nil
}

func (s *drCheckpointArtifactStore) retain(checkpointID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.artifacts[checkpointID]; !ok {
		return false
	}
	s.activeRefs[checkpointID]++
	return true
}

func (s *drCheckpointArtifactStore) release(checkpointID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if refs := s.activeRefs[checkpointID]; refs <= 1 {
		delete(s.activeRefs, checkpointID)
	} else {
		s.activeRefs[checkpointID] = refs - 1
	}
}

func (s *drCheckpointArtifactStore) delete(checkpointID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(checkpointID)
}

func (s *drCheckpointArtifactStore) evictAllLocked() {
	for id := range s.artifacts {
		s.evictLocked(id)
	}
	s.totalBytes = 0
}

func (s *drCheckpointArtifactStore) relationshipBytesLocked(relationshipID string) uint64 {
	var total uint64
	for _, art := range s.artifacts {
		if art.RelationshipID == relationshipID {
			total += art.Bytes
		}
	}
	return total
}

func (s *drCheckpointArtifactStore) evictOldestGlobalLocked() bool {
	var oldest *drCheckpointArtifact
	for _, art := range s.artifacts {
		if s.activeRefs[art.CheckpointID] > 0 {
			continue
		}
		if oldest == nil || art.CreatedAt.Before(oldest.CreatedAt) {
			oldest = art
		}
	}
	if oldest == nil {
		return false
	}
	s.evictLocked(oldest.CheckpointID)
	return true
}

func (s *drCheckpointArtifactStore) evictOldestRelationshipLocked(relationshipID string) bool {
	var oldest *drCheckpointArtifact
	for _, art := range s.artifacts {
		if art.RelationshipID != relationshipID {
			continue
		}
		if s.activeRefs[art.CheckpointID] > 0 {
			continue
		}
		if oldest == nil || art.CreatedAt.Before(oldest.CreatedAt) {
			oldest = art
		}
	}
	if oldest == nil {
		return false
	}
	s.evictLocked(oldest.CheckpointID)
	return true
}

func (s *drCheckpointArtifactStore) evictLocked(checkpointID string) {
	if s.activeRefs[checkpointID] > 0 {
		return
	}
	art, ok := s.artifacts[checkpointID]
	if !ok {
		return
	}
	delete(s.artifacts, checkpointID)
	if art.Bytes <= s.totalBytes {
		s.totalBytes -= art.Bytes
	} else {
		s.totalBytes = 0
	}
	s.evictions.Add(1)
	if art.Path != "" {
		_ = os.RemoveAll(art.Path)
	}
}

func (s *drCheckpointArtifactStore) putArtifact(art *drCheckpointArtifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		if art.Path != "" {
			_ = os.RemoveAll(art.Path)
		}
		return nil
	}
	if art == nil {
		return fmt.Errorf("nil checkpoint artifact")
	}
	if art.Bytes > s.perRelBudget {
		if art.Path != "" {
			_ = os.RemoveAll(art.Path)
		}
		return fmt.Errorf("checkpoint artifact exceeds per-relationship budget")
	}
	if art.Bytes > s.globalBudget {
		if art.Path != "" {
			_ = os.RemoveAll(art.Path)
		}
		return fmt.Errorf("checkpoint artifact exceeds global budget")
	}

	for s.relationshipBytesLocked(art.RelationshipID)+art.Bytes > s.perRelBudget {
		if !s.evictOldestRelationshipLocked(art.RelationshipID) {
			break
		}
	}
	for s.totalBytes+art.Bytes > s.globalBudget {
		if !s.evictOldestGlobalLocked() {
			break
		}
	}
	if s.relationshipBytesLocked(art.RelationshipID)+art.Bytes > s.perRelBudget {
		if art.Path != "" {
			_ = os.RemoveAll(art.Path)
		}
		return fmt.Errorf("checkpoint artifact per-relationship budget exhausted")
	}
	if s.totalBytes+art.Bytes > s.globalBudget {
		if art.Path != "" {
			_ = os.RemoveAll(art.Path)
		}
		return fmt.Errorf("checkpoint artifact global budget exhausted")
	}

	s.artifacts[art.CheckpointID] = art
	s.totalBytes += art.Bytes
	return nil
}

func (s *drCheckpointArtifactStore) waitForIndexFence(idx *atomic.Uint64, target uint64, timeout time.Duration) error {
	if target == 0 || idx == nil {
		return nil
	}
	if idx.Load() >= target {
		return nil
	}
	if timeout <= 0 {
		timeout = drCheckpointArtifactBuildFenceTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if idx.Load() >= target {
			return nil
		}
		time.Sleep(drCheckpointArtifactFencePollInterval)
	}
	return fmt.Errorf("checkpoint index fence timeout: have %d need %d", idx.Load(), target)
}

func (s *drCheckpointArtifactStore) build(
	ctx context.Context,
	cp *drCheckpointCacheEntry,
	scanner *reconciler.Scanner,
	backend physical.Backend,
	rangeCfg reconciler.RangePlanConfig,
) error {
	if cp == nil || scanner == nil || backend == nil {
		return fmt.Errorf("invalid checkpoint artifact build input")
	}
	start := time.Now()
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("create checkpoint artifact dir: %w", err)
	}
	tmpPath := filepath.Join(s.dir, cp.checkpoint.ID+".tmp")
	finalPath := filepath.Join(s.dir, cp.checkpoint.ID)
	_ = os.RemoveAll(tmpPath)
	_ = os.RemoveAll(finalPath)
	if err := os.MkdirAll(filepath.Join(tmpPath, "blobs"), 0o750); err != nil {
		return fmt.Errorf("create checkpoint artifact path: %w", err)
	}

	kids := make([][32]byte, 0, len(cp.kidToKey))
	for kid := range cp.kidToKey {
		kids = append(kids, kid)
	}
	sort.Slice(kids, func(i, j int) bool {
		return string(kids[i][:]) < string(kids[j][:])
	})

	records := make(map[[32]byte]drCheckpointArtifactRecord, len(kids))
	kidToVID := make(map[[32]byte][32]byte, len(kids))
	var bytesTotal uint64
	blobSeen := make(map[string]struct{})

	for _, kid := range kids {
		select {
		case <-ctx.Done():
			_ = os.RemoveAll(tmpPath)
			return ctx.Err()
		default:
		}

		key := cp.kidToKey[kid]
		if key == "" {
			continue
		}

		entry, err := backend.Get(ctx, key)
		if err != nil {
			_ = os.RemoveAll(tmpPath)
			return fmt.Errorf("build checkpoint artifact read %q: %w", key, err)
		}

		rec := drCheckpointArtifactRecord{KID: kid, Key: key}
		if entry == nil {
			_, rec.VID = scanner.ComputeItemFromEntry(&physical.Entry{Key: key})
			rec.Tombstone = true
			records[kid] = rec
			kidToVID[kid] = rec.VID
			bytesTotal += uint64(len(key) + 96)
			continue
		}

		rec.SealWrap = entry.SealWrap
		rec.VID = scanner.ComputeVIDWithSealWrap(entry.Value, entry.SealWrap)

		h := sha256.New()
		h.Write(entry.Value)
		if entry.SealWrap {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		ref := hex.EncodeToString(h.Sum(nil))
		rec.ValueRef = ref

		if _, ok := blobSeen[ref]; !ok {
			blobPath := filepath.Join(tmpPath, "blobs", ref+".bin")
			if err := os.WriteFile(blobPath, entry.Value, 0o600); err != nil {
				_ = os.RemoveAll(tmpPath)
				return fmt.Errorf("write checkpoint artifact blob: %w", err)
			}
			blobSeen[ref] = struct{}{}
			bytesTotal += uint64(len(entry.Value))
		}

		records[kid] = rec
		kidToVID[kid] = rec.VID
		bytesTotal += uint64(len(key) + len(ref) + 128)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.RemoveAll(tmpPath)
		return fmt.Errorf("finalize checkpoint artifact: %w", err)
	}

	cp.kidToVID = kidToVID

	art := &drCheckpointArtifact{
		CheckpointID:    cp.checkpoint.ID,
		CheckpointIndex: cp.checkpoint.CommitIndex,
		RelationshipID:  cp.relationshipID,
		CreatedAt:       time.Now().UTC(),
		Path:            finalPath,
		Bytes:           bytesTotal,
		Records:         records,
	}
	if err := s.putArtifact(art); err != nil {
		_ = os.RemoveAll(finalPath)
		return err
	}

	s.lastBuildMS.Store(uint64(time.Since(start) / time.Millisecond))
	return nil
}
