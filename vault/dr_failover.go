// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
)

const (
	// drPromoteQuiesceWindow is the minimum duration that lastAppliedIndex
	// must be stable before promotion proceeds.
	drPromoteQuiesceWindow = 2 * time.Second

	drPromotionDataLossEstimateBasis = "observed_primary_applied_index_gap"

	drPromotionReasonSecondaryStateNotStableStreaming = "secondary_state_not_stable_streaming"
	drPromotionReasonLastAppliedIndexChanged          = "last_applied_index_changed"
	drPromotionReasonSecondaryLagDetected             = "secondary_lag_detected"
	drPromotionReasonSecondaryRuntimeUnavailable      = "secondary_runtime_unavailable"
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

	// LastKnownPrimaryIndex is the newest primary index observed by the
	// secondary before promotion.
	LastKnownPrimaryIndex uint64

	// EstimatedDataLossEntries is the observed primary/applied index gap.
	EstimatedDataLossEntries uint64

	// PromotionClass records whether the promotion was clean or forced.
	PromotionClass DRPromotionClass

	// CleanPromotionEligible records whether the secondary had enough stable
	// streaming evidence to promote without the forced-promotion
	// acknowledgement.
	CleanPromotionEligible bool

	// ForcedPromotionRequiresAcknowledgement records whether the operator had
	// to supply accept_data_loss=true before promotion could proceed.
	ForcedPromotionRequiresAcknowledgement bool

	// ForcedPromotionReasonCodes contains stable machine-readable reason codes
	// explaining why clean promotion could not be proven.
	ForcedPromotionReasonCodes []string

	// ForcedPromotionReasonDetails contains operator-facing details matching
	// ForcedPromotionReasonCodes.
	ForcedPromotionReasonDetails []string

	// DataLossEstimateBasis explains what EstimatedDataLossEntries measures.
	DataLossEstimateBasis string

	// PromotionID is the persisted lineage record identifier.
	PromotionID string

	// PromotionRecord is the persisted failover lineage record.
	PromotionRecord *DRPromotionRecord

	// DataLossAccepted records whether the forced-promotion acknowledgement
	// was required and supplied.
	DataLossAccepted bool

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
func (c *Core) DRFailover(ctx context.Context, confirmPrimaryUnreachable bool, acceptDataLoss bool) (*DRFailoverResult, error) {
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

	promotionClass := DRPromotionForced
	var forcedReasons []drPromotionReason

	if sec := mgr.Secondary(); sec != nil {
		stateBefore := sec.State()
		indexBefore := sec.lastAppliedIndex.Load()
		primaryBefore := sec.primaryIndex.Load()

		// Quiesce barrier: wait for lastAppliedIndex to stabilize,
		// confirming no in-flight applies.
		select {
		case <-time.After(drPromoteQuiesceWindow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		stateAfter := sec.State()
		indexAfter := sec.lastAppliedIndex.Load()
		primaryAfter := sec.primaryIndex.Load()
		lastKnownPrimaryIndex := primaryBefore
		if primaryAfter > lastKnownPrimaryIndex {
			lastKnownPrimaryIndex = primaryAfter
		}

		result.LastAppliedIndex = indexAfter
		result.LastKnownPrimaryIndex = lastKnownPrimaryIndex
		if lastKnownPrimaryIndex > indexAfter {
			result.EstimatedDataLossEntries = lastKnownPrimaryIndex - indexAfter
		}

		if stateBefore != DRSecondaryStreaming || stateAfter != DRSecondaryStreaming {
			forcedReasons = append(forcedReasons, drPromotionReason{
				code:   drPromotionReasonSecondaryStateNotStableStreaming,
				detail: fmt.Sprintf("secondary state was not stable streaming (%s -> %s)", stateBefore.String(), stateAfter.String()),
			})
		}
		if indexAfter != indexBefore {
			c.logger.Warn("lastAppliedIndex changed during quiesce window; promotion may have in-flight data",
				"index_before", indexBefore,
				"index_after", indexAfter)
			forcedReasons = append(forcedReasons, drPromotionReason{
				code:   drPromotionReasonLastAppliedIndexChanged,
				detail: "lastAppliedIndex changed during the promotion quiesce window",
			})
		}
		if result.EstimatedDataLossEntries > 0 {
			forcedReasons = append(forcedReasons, drPromotionReason{
				code:   drPromotionReasonSecondaryLagDetected,
				detail: fmt.Sprintf("secondary is behind primary by approximately %d entries", result.EstimatedDataLossEntries),
			})
		}
		if len(forcedReasons) == 0 {
			promotionClass = DRPromotionClean
		}
	} else {
		forcedReasons = append(forcedReasons, drPromotionReason{
			code:   drPromotionReasonSecondaryRuntimeUnavailable,
			detail: "secondary runtime is unavailable",
		})
	}

	result.PromotionClass = promotionClass
	result.CleanPromotionEligible = promotionClass == DRPromotionClean
	result.ForcedPromotionRequiresAcknowledgement = promotionClass == DRPromotionForced
	result.ForcedPromotionReasonCodes = drPromotionReasonCodes(forcedReasons)
	result.ForcedPromotionReasonDetails = drPromotionReasonDetails(forcedReasons)
	result.DataLossEstimateBasis = drPromotionDataLossEstimateBasis

	if promotionClass == DRPromotionForced && !acceptDataLoss {
		return nil, fmt.Errorf("forced DR promotion requires accept_data_loss=true; clean promotion proof unavailable: %s", strings.Join(result.ForcedPromotionReasonDetails, "; "))
	}

	promotionID, err := uuid.GenerateUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate promotion ID: %w", err)
	}

	config := mgr.Config()
	promotionRecord := &DRPromotionRecord{
		PromotionID:                 promotionID,
		PromotedAt:                  time.Now().UTC().Unix(),
		OldPrimaryClusterID:         config.ClusterID,
		OldRelationshipID:           config.RelationshipID,
		OldSecondaryCertFingerprint: certFingerprintSHA256DER(config.SecondaryClientCert),
		LocalClusterID:              drSafeLocalClusterID(c),
		LastAppliedIndex:            result.LastAppliedIndex,
		LastKnownPrimaryIndex:       result.LastKnownPrimaryIndex,
		PromotionClass:              promotionClass,
		EstimatedDataLossEntries:    result.EstimatedDataLossEntries,
		DataLossEstimateBasis:       result.DataLossEstimateBasis,
		DataLossAccepted:            promotionClass == DRPromotionForced && acceptDataLoss,
		ForcedReasonCodes:           result.ForcedPromotionReasonCodes,
		ForcedReasonDetails:         result.ForcedPromotionReasonDetails,
	}

	result.PromotionID = promotionID
	result.PromotionRecord = promotionRecord
	result.DataLossAccepted = promotionRecord.DataLossAccepted
	if len(result.ForcedPromotionReasonDetails) > 0 {
		result.Warning = strings.Join(result.ForcedPromotionReasonDetails, "; ")
	}

	if err := mgr.PromoteSecondaryWithRecord(ctx, promotionRecord); err != nil {
		return nil, fmt.Errorf("failed to promote secondary: %w", err)
	}

	result.NewMode = DRModeDisabled
	result.Duration = time.Since(start)

	c.logger.Info("DR failover complete",
		"last_applied_index", result.LastAppliedIndex,
		"last_known_primary_index", result.LastKnownPrimaryIndex,
		"promotion_id", result.PromotionID,
		"promotion_class", result.PromotionClass,
		"duration", result.Duration)

	return result, nil
}

type drPromotionReason struct {
	code   string
	detail string
}

func drPromotionReasonCodes(reasons []drPromotionReason) []string {
	if len(reasons) == 0 {
		return nil
	}
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.code)
	}
	return codes
}

func drPromotionReasonDetails(reasons []drPromotionReason) []string {
	if len(reasons) == 0 {
		return nil
	}
	details := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		details = append(details, reason.detail)
	}
	return details
}

// DRFailoverToPrimary performs failover and immediately enables
// primary mode, allowing the cluster to accept new DR secondaries.
func (c *Core) DRFailoverToPrimary(ctx context.Context, confirmPrimaryUnreachable bool, acceptDataLoss bool) (*DRFailoverResult, error) {
	result, err := c.DRFailover(ctx, confirmPrimaryUnreachable, acceptDataLoss)
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

func drSafeLocalClusterID(c *Core) (id string) {
	defer func() {
		if recover() != nil {
			id = ""
		}
	}()
	if c == nil {
		return ""
	}
	return c.ClusterID()
}
