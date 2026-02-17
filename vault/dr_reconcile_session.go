// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"fmt"
	"strings"
	"time"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/logical"
)

type drReconcileFailureClass string

const (
	drReconcileFailureBudgetExceeded     drReconcileFailureClass = "budget_exceeded"
	drReconcileFailureStalled            drReconcileFailureClass = "stalled"
	drReconcileFailureDecodeExhausted    drReconcileFailureClass = "decode_exhausted"
	drReconcileFailureCheckpointTuple    drReconcileFailureClass = "checkpoint_tuple_mismatch"
	drReconcileFailureCheckpointArtifact drReconcileFailureClass = "checkpoint_artifact_missing"
	drReconcileFailureCheckpointProof    drReconcileFailureClass = "checkpoint_provenance_mismatch"
	drReconcileFailureApplyFailed        drReconcileFailureClass = "apply_failed"
	drReconcileFailureAuthRevoked        drReconcileFailureClass = "auth_revoked"
	drReconcileFailureRedirect           drReconcileFailureClass = "redirect"
	drReconcileFailureUnknown            drReconcileFailureClass = "unknown"
)

const (
	drReconcileRetryCapBudgetExceeded     = uint64(8)
	drReconcileRetryCapStalled            = uint64(8)
	drReconcileRetryCapDecodeExhausted    = uint64(8)
	drReconcileRetryCapCheckpointTuple    = uint64(10)
	drReconcileRetryCapCheckpointArtifact = uint64(10)
	drReconcileRetryCapCheckpointProof    = uint64(10)
	drReconcileRetryCapApplyFailed        = uint64(6)
	drReconcileRetryCapAuthRevoked        = uint64(4)
	drReconcileRetryCapUnknown            = uint64(6)
)

func (s *drReplicationSecondary) beginReconcileSession(checkpointID string, checkpointIndex uint64) error {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	// Enforce monotonic checkpoint index to prevent rollback.
	if s.highestCommittedCheckpointIndexSet && checkpointIndex < s.highestCommittedCheckpointIndex {
		return fmt.Errorf("checkpoint_index %d is lower than highest committed %d; "+
			"use sys/replication/dr/secondary/resnapshot to force reset",
			checkpointIndex, s.highestCommittedCheckpointIndex)
	}

	s.activeCheckpointID = checkpointID
	s.activeCheckpointIndex = checkpointIndex
	s.sessionStart = time.Now().UTC()
	s.streamPausedAt = s.lastAppliedIndex.Load()
	s.lastRangeManifestCount = 0
	s.lastReconcileActivityAt.Store(s.sessionStart.Unix())
	return nil
}

// commitCheckpointIndex advances the monotonic checkpoint high-water mark
// and persists it to barrier storage. Called on successful reconciliation.
func (s *drReplicationSecondary) commitCheckpointIndex(checkpointIndex uint64) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if checkpointIndex > s.highestCommittedCheckpointIndex {
		s.highestCommittedCheckpointIndex = checkpointIndex
		s.highestCommittedCheckpointIndexSet = true

		// Best-effort persist; failure is logged but does not block reconciliation.
		if err := s.persistCheckpointHighWaterMark(checkpointIndex); err != nil {
			s.logger.Warn("failed to persist checkpoint high-water mark",
				"checkpoint_index", checkpointIndex,
				"error", err)
		}
	}
}

// resetCheckpointHighWaterMark clears the monotonic checkpoint constraint.
// Called by operator-initiated resnapshot to allow recovery from catastrophic divergence.
func (s *drReplicationSecondary) resetCheckpointHighWaterMark() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	s.highestCommittedCheckpointIndex = 0
	s.highestCommittedCheckpointIndexSet = false
	_ = s.persistCheckpointHighWaterMark(0)
	s.logger.Info("checkpoint high-water mark reset by operator resnapshot")
}

const drCheckpointHWMPath = "core/dr-replication/checkpoint-hwm"

func (s *drReplicationSecondary) persistCheckpointHighWaterMark(index uint64) error {
	data := fmt.Sprintf("%d", index)
	entry := &logical.StorageEntry{
		Key:   drCheckpointHWMPath,
		Value: []byte(data),
	}
	return s.core.barrier.Put(s.core.activeContext, entry)
}

func (s *drReplicationSecondary) loadCheckpointHighWaterMark() {
	entry, err := s.core.barrier.Get(s.core.activeContext, drCheckpointHWMPath)
	if err != nil || entry == nil {
		return
	}
	var hwm uint64
	if _, err := fmt.Sscanf(string(entry.Value), "%d", &hwm); err == nil && hwm > 0 {
		s.sessionMu.Lock()
		s.highestCommittedCheckpointIndex = hwm
		s.highestCommittedCheckpointIndexSet = true
		s.sessionMu.Unlock()
		s.logger.Info("loaded checkpoint high-water mark", "checkpoint_index", hwm)
	}
}

func (s *drReplicationSecondary) endReconcileSession() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.activeCheckpointID = ""
	s.activeCheckpointIndex = 0
	s.sessionStart = time.Time{}
	s.streamPausedAt = 0
	s.lastRangeManifestCount = 0
}

func (s *drReplicationSecondary) markReconcileSuccess() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.lastReconcileFailReason = ""
	s.lastReconcileFailureByType = make(map[string]uint64)
}

func (s *drReplicationSecondary) markReconcileFailure(err error) drReconcileFailureClass {
	class := classifyReconcileFailure(err)
	classKey := string(class)

	switch class {
	case drReconcileFailureCheckpointTuple, drReconcileFailureCheckpointArtifact, drReconcileFailureCheckpointProof:
		s.checkpointConflicts.Add(1)
	case drReconcileFailureDecodeExhausted:
		s.reconcileDecodeFailures.Add(1)
	}

	s.sessionMu.Lock()
	s.lastReconcileFailReason = classKey
	if s.lastReconcileFailureByType == nil {
		s.lastReconcileFailureByType = make(map[string]uint64)
	}
	s.lastReconcileFailureByType[classKey]++
	s.sessionMu.Unlock()

	metrics.IncrCounter([]string{"replication", "dr", "secondary", "reconcile_failures", classKey}, 1)
	return class
}

func (s *drReplicationSecondary) nextReconcileRetryDelay(class drReconcileFailureClass) time.Duration {
	classKey := string(class)
	s.sessionMu.Lock()
	attempt := s.lastReconcileFailureByType[classKey]
	s.sessionMu.Unlock()
	if attempt == 0 {
		attempt = 1
	}

	base := time.Second
	switch class {
	case drReconcileFailureBudgetExceeded:
		// Force faster checkpoint rollover attempts under pressure.
		base = time.Second
	case drReconcileFailureStalled:
		base = 1500 * time.Millisecond
	case drReconcileFailureCheckpointTuple, drReconcileFailureCheckpointArtifact, drReconcileFailureCheckpointProof:
		base = 2 * time.Second
	case drReconcileFailureAuthRevoked:
		base = 5 * time.Second
	}

	if class == drReconcileFailureBudgetExceeded {
		if attempt > 4 {
			attempt = 4
		}
		backoff := base * time.Duration(attempt)
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
		jitter := time.Duration(randIntn(int(backoff / 4)))
		return backoff + jitter
	}

	if attempt > 6 {
		attempt = 6
	}
	backoff := base * time.Duration(1<<(attempt-1))
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	jitter := time.Duration(randIntn(int(backoff / 5)))
	return backoff + jitter
}

func (s *drReplicationSecondary) shouldRetryReconcile(class drReconcileFailureClass) bool {
	// Never retry a redirect -- the controller must reconnect to the new leader.
	if class == drReconcileFailureRedirect {
		return false
	}
	classKey := string(class)
	s.sessionMu.RLock()
	attempt := s.lastReconcileFailureByType[classKey]
	s.sessionMu.RUnlock()
	if attempt == 0 {
		return true
	}
	return attempt <= s.reconcileRetryCap(class)
}

func (s *drReplicationSecondary) reconcileRetryCap(class drReconcileFailureClass) uint64 {
	switch class {
	case drReconcileFailureBudgetExceeded:
		return drReconcileRetryCapBudgetExceeded
	case drReconcileFailureStalled:
		return drReconcileRetryCapStalled
	case drReconcileFailureDecodeExhausted:
		return drReconcileRetryCapDecodeExhausted
	case drReconcileFailureCheckpointTuple:
		return drReconcileRetryCapCheckpointTuple
	case drReconcileFailureCheckpointArtifact:
		return drReconcileRetryCapCheckpointArtifact
	case drReconcileFailureCheckpointProof:
		return drReconcileRetryCapCheckpointProof
	case drReconcileFailureApplyFailed:
		return drReconcileRetryCapApplyFailed
	case drReconcileFailureAuthRevoked:
		return drReconcileRetryCapAuthRevoked
	default:
		return drReconcileRetryCapUnknown
	}
}

func (s *drReplicationSecondary) retryCapCooldown(class drReconcileFailureClass) time.Duration {
	switch class {
	case drReconcileFailureCheckpointTuple, drReconcileFailureCheckpointArtifact, drReconcileFailureCheckpointProof:
		return 15 * time.Second
	case drReconcileFailureStalled:
		return 15 * time.Second
	case drReconcileFailureBudgetExceeded:
		return 20 * time.Second
	default:
		return 10 * time.Second
	}
}

func classifyReconcileFailure(err error) drReconcileFailureClass {
	if err == nil {
		return drReconcileFailureUnknown
	}
	// Check for DR redirect first -- this is a structured gRPC error, not
	// a string match. If the reconciler hit a standby node, we must abort
	// immediately and reconnect to the leader.
	if _, ok := extractDRRedirect(err); ok {
		return drReconcileFailureRedirect
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "reconcile failure [stalled]"), strings.Contains(msg, "stalled without task progress"):
		return drReconcileFailureStalled
	case strings.Contains(msg, "budget_exceeded"):
		return drReconcileFailureBudgetExceeded
	case strings.Contains(msg, "stream pressure"), strings.Contains(msg, "primary overloaded"):
		return drReconcileFailureBudgetExceeded
	case strings.Contains(msg, "decode_exhausted"):
		return drReconcileFailureDecodeExhausted
	case strings.Contains(msg, "checkpoint_tuple_mismatch"), strings.Contains(msg, "checkpoint tuple mismatch"):
		return drReconcileFailureCheckpointTuple
	case strings.Contains(msg, "checkpoint_artifact_missing"), strings.Contains(msg, "checkpoint artifact missing"):
		return drReconcileFailureCheckpointArtifact
	case strings.Contains(msg, "checkpoint_provenance_mismatch"), strings.Contains(msg, "expected_vid does not match checkpoint artifact"):
		return drReconcileFailureCheckpointProof
	case strings.Contains(msg, "apply_failed"), strings.Contains(msg, "failed to apply"), strings.Contains(msg, "delete failed"):
		return drReconcileFailureApplyFailed
	case strings.Contains(msg, "permissiondenied"), strings.Contains(msg, "revoked"), strings.Contains(msg, "authorization failed"):
		return drReconcileFailureAuthRevoked
	default:
		return drReconcileFailureUnknown
	}
}

func (s *drReplicationSecondary) assertActiveCheckpoint(checkpointID string, checkpointIndex uint64) error {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	if checkpointID == "" || checkpointIndex == 0 {
		return fmt.Errorf("checkpoint_tuple_mismatch: checkpoint tuple missing")
	}
	if s.activeCheckpointID == "" || s.activeCheckpointIndex == 0 {
		return fmt.Errorf("checkpoint_tuple_mismatch: no active reconciliation session")
	}
	if s.activeCheckpointID != checkpointID || s.activeCheckpointIndex != checkpointIndex {
		return fmt.Errorf("checkpoint_tuple_mismatch: active=(%s,%d) got=(%s,%d)",
			s.activeCheckpointID, s.activeCheckpointIndex, checkpointID, checkpointIndex)
	}
	return nil
}

func (s *drReplicationSecondary) validateDeleteSafety(checkpointID string, checkpointIndex uint64) error {
	if err := s.assertActiveCheckpoint(checkpointID, checkpointIndex); err != nil {
		return err
	}
	s.sessionMu.RLock()
	pausedAt := s.streamPausedAt
	s.sessionMu.RUnlock()
	state := s.State()
	if state != DRSecondaryReconciling && state != DRSecondaryResnapshotting {
		return fmt.Errorf("delete safety violation: secondary is not reconciling")
	}
	if s.lastAppliedIndex.Load() != pausedAt {
		return fmt.Errorf("delete safety violation: stream boundary moved (paused_at=%d current=%d)", pausedAt, s.lastAppliedIndex.Load())
	}
	return nil
}
