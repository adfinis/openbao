// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

// Package reconciler builds reconciliation sets from storage for
// disaster recovery replication. It scans the full keyspace, computes
// strata estimator, prefix digest) for set reconciliation.
package reconciler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"sync"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// tombstoneMarker is a fixed byte sequence used to derive the VID
// for deleted/tombstoned keys.
var tombstoneMarker = []byte("__openbao_tombstone__")

// Checkpoint represents a consistent point-in-time snapshot of the
// reconciliation state.
type Checkpoint struct {
	// ID is a unique identifier for this checkpoint.
	ID string

	// CommitIndex is the Raft commit index at the time of scanning.
	CommitIndex uint64

	// Term is the Raft term at the time of scanning.
	Term uint64
}

// CheckpointItemMeta is the compact checkpoint representation used by
// metadata-first reconciliation and range manifest planning.
type CheckpointItemMeta struct {
	KID [32]byte
	VID [32]byte
	Key string
}

// CheckpointArtifact is an immutable reconciliation artifact for one
// checkpoint tuple.
type CheckpointArtifact struct {
	CheckpointID  string
	CommitIndex   uint64
	Items         []CheckpointItemMeta
	RangeManifest []RangeDescriptor
}

// scanning the storage keyspace at a specific checkpoint.
type ReconciliationSet struct {
	// Checkpoint anchors this set to a specific point in time.
	Checkpoint Checkpoint

	// KeyCount is the total number of keys scanned.
	KeyCount int

	// KIDToKey maps KID -> original storage key for entry fetching.
	// Only populated on the local side (not sent over the wire).
	KIDToKey map[[32]byte]string

	// KIDToVID maps KID -> VID for all scanned entries. This allows
	// building IBLTs from a single checkpoint snapshot without rescanning.
	KIDToVID map[[32]byte][32]byte

	// Entries is an optional snapshot of scanned storage entries keyed by
	// KID. Populated when ScanConfig.BuildEntryMap is true.
	Entries map[[32]byte]*physical.Entry
}

// ScanConfig configures the reconciliation scan.
type ScanConfig struct {
	// ReplSalt is the HMAC key used to derive KIDs from storage keys.
	// Must be the same on primary and secondary.
	ReplSalt []byte

	// BuildKIDMap if true, populates ReconciliationSet.KIDToKey for
	// reverse lookups. Should be true on the side that will serve
	// entry fetches (typically the primary).
	BuildKIDMap bool

	// BuildEntryMap if true, populates ReconciliationSet.Entries with a
	// point-in-time snapshot of scanned entries keyed by KID.
	BuildEntryMap bool

	// RequireTransactionalSnapshot enforces that scans run against a
	// read-only transaction view. If no transactional view is available,
	// Scan returns an error.
	RequireTransactionalSnapshot bool

	// ExcludePaths is an optional set of storage paths to skip during
	// reconciliation scans. Paths in this set cannot be read through
	// the barrier (e.g. core/keyring, encrypted with root key) or
	// are cluster-local (e.g. core/hsm/barrier-unseal-keys).
	ExcludePaths map[string]bool

	// ExcludePathFunc, when set, is evaluated for every scanned path
	// and can be used for prefix- or pattern-based exclusions.
	ExcludePathFunc func(string) bool

	// Logger for scan progress.
	Logger log.Logger

	// ValueDomain controls how VIDs are derived.
	//
	// Plaintext mode hashes only entry bytes and matches legacy behavior.
	// Ciphertext mode hashes entry bytes plus seal-wrap flag and is used
	// for below-barrier DR reconciliation.
	ValueDomain ValueDomain
}

// ValueDomain controls VID derivation semantics.
type ValueDomain string

const (
	// ValueDomainPlaintext hashes only entry bytes.
	ValueDomainPlaintext ValueDomain = "plaintext"
	// ValueDomainCiphertext hashes entry bytes plus seal-wrap flag.
	ValueDomainCiphertext ValueDomain = "ciphertext"
)

// DefaultScanConfig returns a ScanConfig with sensible defaults.
func DefaultScanConfig(replSalt []byte) ScanConfig {
	return ScanConfig{
		ReplSalt:                     replSalt,
		BuildKIDMap:                  false,
		RequireTransactionalSnapshot: false,
		ValueDomain:                  ValueDomainPlaintext,
	}
}

// Scanner builds reconciliation sets by scanning storage.
type Scanner struct {
	config ScanConfig
	logger log.Logger
}

// NewScanner creates a new reconciliation scanner.
func NewScanner(config ScanConfig) *Scanner {
	logger := config.Logger
	if logger == nil {
		logger = log.NewNullLogger()
	}
	return &Scanner{
		config: config,
		logger: logger,
	}
}

// Scan iterates the full storage keyspace and builds a ReconciliationSet.
// The storage should ideally support transactions; ScanView handles this
// internally by wrapping in a read-only transaction if available.
func (s *Scanner) Scan(ctx context.Context, storage logical.Storage, checkpoint Checkpoint) (*ReconciliationSet, error) {
	s.logger.Info("starting reconciliation scan", "checkpoint_id", checkpoint.ID, "commit_index", checkpoint.CommitIndex)

	rs := &ReconciliationSet{
		Checkpoint: checkpoint,
		KIDToVID:   make(map[[32]byte][32]byte),
	}

	if s.config.BuildKIDMap {
		rs.KIDToKey = make(map[[32]byte]string)
	}
	if s.config.BuildEntryMap {
		rs.Entries = make(map[[32]byte]*physical.Entry)
	}

	scanStorage, rollback, err := s.beginScanSnapshot(ctx, storage)
	if err != nil {
		return nil, fmt.Errorf("reconciler: failed to begin scan snapshot: %w", err)
	}
	if rollback != nil {
		defer rollback()
	}

	var mu sync.Mutex
	err = logical.ScanViewPaginated(ctx, scanStorage, s.logger, logical.DefaultScanViewPageLimit, func(_ int, _ int, path string) (bool, error) {
		// Skip excluded paths (e.g. core/keyring which is encrypted
		// with the root key and cannot be read through the barrier).
		if s.shouldExclude(path) {
			return true, nil
		}

		// Get the entry to compute VID from value hash.
		entry, err := scanStorage.Get(ctx, path)
		if err != nil {
			return false, fmt.Errorf("failed to read entry during scan for %q: %w", path, err)
		}

		kid := s.computeKID(path)
		var vid [32]byte
		if entry == nil {
			// Tombstone / deleted entry.
			vid = s.computeTombstoneVID(path)
		} else {
			vid = s.computeVIDWithSealWrap(entry.Value, entry.SealWrap)
		}

		mu.Lock()
		rs.KIDToVID[kid] = vid

		if rs.KIDToKey != nil {
			rs.KIDToKey[kid] = path
		}
		if rs.Entries != nil && entry != nil {
			valueCopy := make([]byte, len(entry.Value))
			copy(valueCopy, entry.Value)
			rs.Entries[kid] = &physical.Entry{
				Key:      path,
				Value:    valueCopy,
				SealWrap: entry.SealWrap,
			}
		}
		rs.KeyCount++
		mu.Unlock()
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconciler: scan failed: %w", err)
	}

	s.logger.Info("reconciliation scan complete", "keys", rs.KeyCount, "checkpoint_id", checkpoint.ID)
	return rs, nil
}

// ScanPhysical iterates the full keyspace using a physical backend snapshot.
// This is used by DR below-barrier reconciliation.
func (s *Scanner) ScanPhysical(ctx context.Context, backend physical.Backend, checkpoint Checkpoint) (*ReconciliationSet, error) {
	s.logger.Info("starting physical reconciliation scan", "checkpoint_id", checkpoint.ID, "commit_index", checkpoint.CommitIndex)

	rs := &ReconciliationSet{
		Checkpoint: checkpoint,
		KIDToVID:   make(map[[32]byte][32]byte),
	}

	if s.config.BuildKIDMap {
		rs.KIDToKey = make(map[[32]byte]string)
	}
	if s.config.BuildEntryMap {
		rs.Entries = make(map[[32]byte]*physical.Entry)
	}

	scanBackend, rollback, err := s.beginScanSnapshotPhysical(ctx, backend)
	if err != nil {
		return nil, fmt.Errorf("reconciler: failed to begin physical scan snapshot: %w", err)
	}
	if rollback != nil {
		defer rollback()
	}

	var mu sync.Mutex
	err = logical.ScanViewPaginated(ctx, scanBackend, s.logger, logical.DefaultScanViewPageLimit, func(_ int, _ int, path string) (bool, error) {
		if s.shouldExclude(path) {
			return true, nil
		}

		entry, err := scanBackend.Get(ctx, path)
		if err != nil {
			return false, fmt.Errorf("failed to read physical entry during scan for %q: %w", path, err)
		}

		kid := s.computeKID(path)
		var vid [32]byte
		if entry == nil {
			vid = s.computeTombstoneVID(path)
		} else {
			vid = s.computeVIDWithSealWrap(entry.Value, entry.SealWrap)
		}

		mu.Lock()
		rs.KIDToVID[kid] = vid

		if rs.KIDToKey != nil {
			rs.KIDToKey[kid] = path
		}
		if rs.Entries != nil && entry != nil {
			valueCopy := make([]byte, len(entry.Value))
			copy(valueCopy, entry.Value)
			rs.Entries[kid] = &physical.Entry{
				Key:      path,
				Value:    valueCopy,
				SealWrap: entry.SealWrap,
			}
		}
		rs.KeyCount++
		mu.Unlock()
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconciler: physical scan failed: %w", err)
	}

	s.logger.Info("physical reconciliation scan complete", "keys", rs.KeyCount, "checkpoint_id", checkpoint.ID)
	return rs, nil
}

func (s *Scanner) beginScanSnapshot(ctx context.Context, storage logical.Storage) (logical.Storage, func(), error) {
	if txView, ok := storage.(logical.Transactional); ok {
		txn, err := txView.BeginReadOnlyTx(ctx)
		if err != nil {
			return nil, nil, err
		}
		return txn, func() {
			_ = txn.Rollback(ctx)
		}, nil
	}

	if s.config.RequireTransactionalSnapshot {
		return nil, nil, fmt.Errorf("transactional snapshot required but storage does not support read-only transactions")
	}

	return storage, nil, nil
}

func (s *Scanner) beginScanSnapshotPhysical(ctx context.Context, backend physical.Backend) (physical.Backend, func(), error) {
	if txView, ok := backend.(physical.Transactional); ok {
		txn, err := txView.BeginReadOnlyTx(ctx)
		if err != nil {
			return nil, nil, err
		}
		return txn, func() {
			_ = txn.Rollback(ctx)
		}, nil
	}

	if s.config.RequireTransactionalSnapshot {
		return nil, nil, fmt.Errorf("transactional snapshot required but physical backend does not support read-only transactions")
	}

	return backend, nil, nil
}

func (s *Scanner) shouldExclude(path string) bool {
	if s.config.ExcludePaths[path] {
		return true
	}
	if s.config.ExcludePathFunc != nil && s.config.ExcludePathFunc(path) {
		return true
	}
	return false
}

// ComputeKID computes the KID for a storage key. Exported for use
// in entry fetch resolution.
func (s *Scanner) ComputeKID(key string) [32]byte {
	return s.computeKID(key)
}

// ComputeVID computes the VID for an entry value. Exported for use
// in change stream processing.
func (s *Scanner) ComputeVID(value []byte) [32]byte {
	return s.computeVIDWithSealWrap(value, false)
}

// ComputeVIDWithSealWrap computes the VID for entry bytes and SealWrap flag.
func (s *Scanner) ComputeVIDWithSealWrap(value []byte, sealWrap bool) [32]byte {
	return s.computeVIDWithSealWrap(value, sealWrap)
}

// ComputeItemFromEntry computes the (KID, VID) pair for a storage entry.
func (s *Scanner) ComputeItemFromEntry(entry *physical.Entry) (kid, vid [32]byte) {
	kid = s.computeKID(entry.Key)
	if entry.Value == nil {
		vid = s.computeTombstoneVID(entry.Key)
	} else {
		vid = s.computeVIDWithSealWrap(entry.Value, entry.SealWrap)
	}
	return
}

// --- Internal helpers ---

// computeKID derives KID = HMAC-SHA256(replSalt, key).
func (s *Scanner) computeKID(key string) [32]byte {
	mac := hmac.New(sha256.New, s.config.ReplSalt)
	mac.Write([]byte(key))
	var kid [32]byte
	copy(kid[:], mac.Sum(nil))
	return kid
}

// computeVIDWithSealWrap derives VID from value and (optionally) SealWrap.
func (s *Scanner) computeVIDWithSealWrap(value []byte, sealWrap bool) [32]byte {
	switch s.config.ValueDomain {
	case ValueDomainCiphertext:
		h := sha256.New()
		h.Write(value)
		if sealWrap {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		var vid [32]byte
		copy(vid[:], h.Sum(nil))
		return vid
	default:
		return sha256.Sum256(value)
	}
}

// computeTombstoneVID derives VID for a deleted key.
func (s *Scanner) computeTombstoneVID(key string) [32]byte {
	h := sha256.New()
	h.Write(tombstoneMarker)
	h.Write([]byte(key))
	var vid [32]byte
	copy(vid[:], h.Sum(nil))
	return vid
}
