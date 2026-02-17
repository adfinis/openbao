// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"time"
)

const (
	// drPromoteQuiesceWindow is the minimum duration that lastAppliedIndex
	// must be stable before promotion proceeds.
	drPromoteQuiesceWindow = 2 * time.Second
)

// DRFailoverResult contains the outcome of a DR failover operation.
type DRFailoverResult struct {
	// OldMode is the mode before failover.
	OldMode DRMode

	// NewMode is the mode after failover.
	NewMode DRMode

	// LastAppliedIndex is the last Raft index applied from the primary
	// before failover.
	LastAppliedIndex uint64

	// Duration is how long the failover took.
	Duration time.Duration

	// Warning contains any non-fatal warnings (e.g., data lag).
	Warning string
}

// DRFailover performs a full disaster recovery failover.
//
// This promotes the current DR secondary to a standalone cluster that can
// serve requests. The operation:
//  1. Verifies the operator confirmed primary is unreachable
//  2. Stops the replication stream from the primary
//  3. Waits for lastAppliedIndex to stabilize (quiesce barrier)
//  4. Transitions replication state to disabled
//  5. Persists the new configuration
//
// After failover, the operator can optionally re-enable as primary to
// accept new DR secondaries.
func (c *Core) DRFailover(ctx context.Context, confirmPrimaryUnreachable bool) (*DRFailoverResult, error) {
	mgr := c.drManager
	if mgr == nil {
		return nil, fmt.Errorf("DR replication not initialized")
	}

	if mgr.Mode() != DRModeSecondary {
		return nil, fmt.Errorf("cannot failover: not in DR secondary mode (current mode: %s)", mgr.Mode())
	}

	if !confirmPrimaryUnreachable {
		return nil, fmt.Errorf("promotion requires confirm_primary_unreachable=true to proceed; " +
			"this confirms the operator has verified the primary cluster is unreachable")
	}

	start := time.Now()
	result := &DRFailoverResult{
		OldMode: DRModeSecondary,
	}

	c.logger.Info("DR failover initiated")

	if sec := mgr.Secondary(); sec != nil {
		result.LastAppliedIndex = sec.lastAppliedIndex.Load()
		if sec.State() == DRSecondaryReconciling {
			result.Warning = "failover during active reconciliation; some data may not be fully synchronized"
			c.logger.Warn("failover initiated during reconciliation")
		}

		// Quiesce barrier: wait for lastAppliedIndex to stabilize,
		// confirming no in-flight applies.
		indexBefore := sec.lastAppliedIndex.Load()
		time.Sleep(drPromoteQuiesceWindow)
		indexAfter := sec.lastAppliedIndex.Load()
		if indexAfter != indexBefore {
			c.logger.Warn("lastAppliedIndex changed during quiesce window; promotion may have in-flight data",
				"index_before", indexBefore,
				"index_after", indexAfter)
			if result.Warning != "" {
				result.Warning += "; "
			}
			result.Warning += fmt.Sprintf("lastAppliedIndex changed during quiesce window (%d -> %d)", indexBefore, indexAfter)
		}
		result.LastAppliedIndex = sec.lastAppliedIndex.Load()
	}

	if err := mgr.PromoteSecondary(ctx); err != nil {
		return nil, fmt.Errorf("failed to promote secondary: %w", err)
	}

	result.NewMode = DRModeDisabled
	result.Duration = time.Since(start)

	c.logger.Info("DR failover complete",
		"last_applied_index", result.LastAppliedIndex,
		"duration", result.Duration)

	return result, nil
}

// DRFailoverToPrimary performs failover and immediately enables
// primary mode, allowing the cluster to accept new DR secondaries.
func (c *Core) DRFailoverToPrimary(ctx context.Context, confirmPrimaryUnreachable bool) (*DRFailoverResult, error) {
	result, err := c.DRFailover(ctx, confirmPrimaryUnreachable)
	if err != nil {
		return nil, err
	}

	mgr := c.drManager
	if err := mgr.EnablePrimary(ctx); err != nil {
		return nil, fmt.Errorf("failed to enable primary after failover: %w", err)
	}

	result.NewMode = DRModePrimary
	c.logger.Info("DR failover to primary complete",
		"cluster_id", mgr.Config().ClusterID)

	return result, nil
}
