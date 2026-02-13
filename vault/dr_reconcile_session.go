// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"fmt"
	"strings"
	"time"

	metrics "github.com/hashicorp/go-metrics/compat"
)

type drReconcileFailureClass string

const (
	drReconcileFailureBudgetExceeded     drReconcileFailureClass = "budget_exceeded"
	drReconcileFailureDecodeExhausted    drReconcileFailureClass = "decode_exhausted"
	drReconcileFailureCheckpointConflict drReconcileFailureClass = "checkpoint_conflict"
	drReconcileFailureApplyFailed        drReconcileFailureClass = "apply_failed"
	drReconcileFailureAuthRevoked        drReconcileFailureClass = "auth_revoked"
	drReconcileFailureUnknown            drReconcileFailureClass = "unknown"
)

func (s *drReplicationSecondary) beginReconcileSession(checkpointID string, checkpointIndex uint64, manifestCount int) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.activeCheckpointID = checkpointID
	s.activeCheckpointIndex = checkpointIndex
	s.sessionStart = time.Now().UTC()
	s.streamPausedAt = s.lastAppliedIndex.Load()
	s.lastRangeManifestCount = manifestCount
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
		base = 3 * time.Second
	case drReconcileFailureCheckpointConflict:
		base = 2 * time.Second
	case drReconcileFailureAuthRevoked:
		base = 5 * time.Second
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

func classifyReconcileFailure(err error) drReconcileFailureClass {
	if err == nil {
		return drReconcileFailureUnknown
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "budget_exceeded"):
		return drReconcileFailureBudgetExceeded
	case strings.Contains(msg, "decode_exhausted"):
		return drReconcileFailureDecodeExhausted
	case strings.Contains(msg, "checkpoint conflict"), strings.Contains(msg, "checkpoint tuple"):
		return drReconcileFailureCheckpointConflict
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
		return fmt.Errorf("checkpoint tuple missing")
	}
	if s.activeCheckpointID == "" || s.activeCheckpointIndex == 0 {
		return fmt.Errorf("no active reconciliation session")
	}
	if s.activeCheckpointID != checkpointID || s.activeCheckpointIndex != checkpointIndex {
		return fmt.Errorf("checkpoint tuple mismatch: active=(%s,%d) got=(%s,%d)",
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
	if s.State() != DRSecondaryReconciling {
		return fmt.Errorf("delete safety violation: secondary is not reconciling")
	}
	if s.lastAppliedIndex.Load() != pausedAt {
		return fmt.Errorf("delete safety violation: stream boundary moved (paused_at=%d current=%d)", pausedAt, s.lastAppliedIndex.Load())
	}
	return nil
}
