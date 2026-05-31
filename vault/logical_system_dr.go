// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// drReplicationPaths returns the API paths for DR replication management.
func (b *SystemBackend) drReplicationPaths() []*framework.Path {
	return []*framework.Path{
		// --- Status ---
		{
			Pattern: "replication/dr/status$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr",
				OperationVerb:   "status",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleDRStatus,
					Summary:  "Returns the DR replication status of the cluster.",
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"mode": {
									Type:     framework.TypeString,
									Required: true,
								},
								"cluster_id": {
									Type: framework.TypeString,
								},
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Check the DR replication status of this cluster",
			HelpDescription: "Returns the current DR replication mode and status.",
		},

		// --- Enable Primary ---
		{
			Pattern: "replication/dr/primary/enable$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "enable",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryEnable,
					Summary:                   "Enable DR replication primary mode on this cluster.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusNoContent: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Enable DR primary mode",
			HelpDescription: "Enables this cluster as a DR replication primary. A DR secondary can then be configured to replicate from this cluster.",
		},

		// --- Disable Primary ---
		{
			Pattern: "replication/dr/primary/disable$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "disable",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryDisable,
					Summary:                   "Disable DR replication primary mode.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusNoContent: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Disable DR primary mode",
			HelpDescription: "Disables DR replication primary mode on this cluster.",
		},

		// --- Generate Activation Token ---
		{
			Pattern: "replication/dr/primary/secondary-token$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "generate-secondary-token",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryGenerateToken,
					Summary:                   "Generate a DR activation token for a secondary cluster.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"token": {
									Type:     framework.TypeString,
									Required: true,
									DisplayAttrs: &framework.DisplayAttributes{
										Sensitive: true,
									},
								},
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Generate DR secondary activation token",
			HelpDescription: "Generates a token that a secondary cluster uses to establish a DR replication relationship with this primary.",
		},

		// --- Enable Secondary ---
		{
			Pattern: "replication/dr/secondary/enable$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "enable",
			},

			Fields: map[string]*framework.FieldSchema{
				"token": {
					Type:        framework.TypeString,
					Description: "The DR activation token from the primary cluster.",
					Required:    true,
					DisplayAttrs: &framework.DisplayAttributes{
						Sensitive: true,
					},
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryEnable,
					Summary:                   "Enable DR replication secondary mode using an activation token.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusNoContent: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Enable DR secondary mode",
			HelpDescription: "Enables this cluster as a DR replication secondary, connecting it to the primary specified in the activation token.",
		},

		// --- Disable Secondary ---
		{
			Pattern: "replication/dr/secondary/disable$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "disable",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryDisable,
					Summary:                   "Disable DR replication secondary mode.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusNoContent: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Disable DR secondary mode",
			HelpDescription: "Stops DR replication and removes this cluster's secondary role.",
		},

		// --- Rotate Secondary Credential ---
		{
			Pattern: "replication/dr/secondary/rotate-certificate$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "rotate-certificate",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryRotateCertificate,
					Summary:                   "Rotate this DR secondary's relationship certificate.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Rotate DR secondary credential",
			HelpDescription: "Generates a new local DR secondary client certificate, stages and confirms it with the primary, persists it locally, and reconnects using the new credential.",
		},

		// --- Register Secondary (unauthenticated, bootstrap token is the auth) ---
		{
			Pattern: "replication/dr/primary/register-secondary$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "register-secondary",
			},

			Fields: map[string]*framework.FieldSchema{
				"relationship_id": {
					Type:        framework.TypeString,
					Description: "The relationship ID from the activation token.",
					Required:    true,
				},
				"bootstrap_token": {
					Type:        framework.TypeString,
					Description: "The one-time bootstrap token from the activation token.",
					Required:    true,
					DisplayAttrs: &framework.DisplayAttributes{
						Sensitive: true,
					},
				},
				"secondary_ca_cert": {
					Type:        framework.TypeString,
					Description: "The secondary's DR client certificate trust anchor (base64-encoded DER).",
					Required:    true,
					DisplayAttrs: &framework.DisplayAttributes{
						Sensitive: true,
					},
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryRegisterSecondary,
					Summary:                   "Register a DR secondary's CA certificate with the primary.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusNoContent: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Register a DR secondary's certificate",
			HelpDescription: "Registers a DR secondary's client certificate trust anchor so the primary can trust it for DR mTLS connections. Authenticated via a one-time bootstrap token.",
		},

		// --- Rotate Secondary Certificate (unauthenticated, signed by relationship credential) ---
		{
			Pattern: "replication/dr/primary/rotate-secondary-certificate$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "rotate-secondary-certificate",
			},

			Fields: drCredentialRotationFieldSchemas(),

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryRotateSecondaryCertificate,
					Summary:                   "Stage a DR secondary certificate rotation.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Stage a DR secondary credential rotation",
			HelpDescription: "Stages a new DR secondary client certificate for an active relationship. Authenticated by a signature from the current relationship credential.",
		},

		// --- Confirm Secondary Certificate Rotation (unauthenticated, signed by pending credential) ---
		{
			Pattern: "replication/dr/primary/confirm-secondary-certificate$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "confirm-secondary-certificate",
			},

			Fields: drCredentialRotationFieldSchemas(),

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryConfirmSecondaryCertificate,
					Summary:                   "Finalize a DR secondary certificate rotation.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
						}},
					},
				},
			},

			HelpSynopsis:    "Finalize a DR secondary credential rotation",
			HelpDescription: "Finalizes a staged DR secondary client certificate rotation. Authenticated by a signature from the pending relationship credential.",
		},

		// --- List Relationships ---
		{
			Pattern: "replication/dr/primary/relationships$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "list-relationships",
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleDRPrimaryListRelationships,
					Summary:  "List DR relationships on the primary.",
				},
			},
			HelpSynopsis:    "List DR relationships",
			HelpDescription: "Returns all known DR relationship records for the primary cluster.",
		},

		// --- Relationship Status ---
		{
			Pattern: "replication/dr/primary/relationships/" + framework.GenericNameRegex("id") + "/status$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "relationship-status",
			},
			Fields: map[string]*framework.FieldSchema{
				"id": {
					Type:        framework.TypeString,
					Description: "Relationship ID.",
					Required:    true,
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleDRPrimaryRelationshipStatus,
					Summary:  "Read status of one DR relationship.",
				},
			},
			HelpSynopsis:    "DR relationship status",
			HelpDescription: "Returns status for the specified DR relationship.",
		},

		// --- Relationship Revoke ---
		{
			Pattern: "replication/dr/primary/relationships/" + framework.GenericNameRegex("id") + "/revoke$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary",
				OperationVerb:   "relationship-revoke",
			},
			Fields: map[string]*framework.FieldSchema{
				"id": {
					Type:        framework.TypeString,
					Description: "Relationship ID.",
					Required:    true,
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.handleDRPrimaryRelationshipRevoke,
					Summary:  "Revoke one DR relationship.",
				},
			},
			HelpSynopsis:    "Revoke DR relationship",
			HelpDescription: "Revokes the specified DR relationship and removes trust for its certificate.",
		},

		// --- Promote Secondary ---
		{
			Pattern: "replication/dr/secondary/promote$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "promote",
			},

			Fields: map[string]*framework.FieldSchema{
				"confirm_primary_unreachable": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that the primary cluster is unreachable. Required for promotion to proceed.",
				},
				"accept_data_loss": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Explicit acknowledgement required when promotion cannot prove the secondary is fully caught up.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.handleDRSecondaryPromote,
					Summary:  "Promote the DR secondary to a standalone primary.",
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"message": {
									Type: framework.TypeString,
								},
								"promotion_id": {
									Type: framework.TypeString,
								},
								"promotion_class": {
									Type: framework.TypeString,
								},
								"clean_promotion_eligible": {
									Type: framework.TypeBool,
								},
								"clean_promotion_proof_available": {
									Type: framework.TypeBool,
								},
								"forced_promotion_requires_acknowledgement": {
									Type: framework.TypeBool,
								},
								"forced_promotion_reason_codes": {
									Type: framework.TypeStringSlice,
								},
								"forced_promotion_reason_details": {
									Type: framework.TypeStringSlice,
								},
								"data_loss_accepted": {
									Type: framework.TypeBool,
								},
								"last_applied_index": {
									Type: framework.TypeInt,
								},
								"last_known_primary_index": {
									Type: framework.TypeInt,
								},
								"estimated_data_loss_entries": {
									Type: framework.TypeInt,
								},
								"estimated_data_loss_entries_basis": {
									Type: framework.TypeString,
								},
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Promote DR secondary",
			HelpDescription: "Promotes this DR secondary to a standalone primary. Clean promotion requires confirm_primary_unreachable=true. Forced promotion also requires accept_data_loss=true when the secondary is lagging, reconciling, or otherwise cannot prove it is fully caught up.",
		},

		// --- Secondary resnapshot trigger ---
		{
			Pattern: "replication/dr/secondary/resnapshot$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "resnapshot",
			},
			Fields: map[string]*framework.FieldSchema{
				"confirm_rollback_ok": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that resetting the checkpoint high-water mark is acceptable. Required because resnapshot allows the secondary to accept older checkpoint indices.",
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryResnapshot,
					Summary:                   "Trigger a hard-cutover DR resnapshot on the secondary.",
					ForwardPerformanceStandby: true,
				},
			},
			HelpSynopsis:    "Trigger DR secondary resnapshot",
			HelpDescription: "Requests a protocol-scoped full-copy resnapshot from the primary checkpoint. Requires confirm_rollback_ok=true.",
		},

		// --- DR tuning ---
		{
			Pattern: "replication/dr/tuning$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr",
				OperationVerb:   "tuning",
			},
			Fields: map[string]*framework.FieldSchema{
				"checkpoint_ttl_seconds": {
					Type:        framework.TypeInt,
					Description: "Checkpoint cache TTL in seconds.",
				},
				"checkpoint_global_budget_bytes": {
					Type:        framework.TypeInt,
					Description: "Global checkpoint cache budget in bytes.",
				},
				"checkpoint_per_relationship_budget_bytes": {
					Type:        framework.TypeInt,
					Description: "Per-relationship checkpoint cache budget in bytes.",
				},
				"stream_buffer_max_entries": {
					Type:        framework.TypeInt,
					Description: "Primary stream buffer max entries.",
				},
				"stream_buffer_max_bytes": {
					Type:        framework.TypeInt,
					Description: "Primary stream buffer max bytes.",
				},
				"reconcile_max_rpc_bytes": {
					Type:        framework.TypeInt,
					Description: "Secondary reconcile RPC byte budget.",
				},
				"reconcile_max_wall_time_seconds": {
					Type:        framework.TypeInt,
					Description: "Secondary reconcile wall-time budget in seconds.",
				},
				"reconcile_max_inflight_tasks": {
					Type:        framework.TypeInt,
					Description: "Secondary max in-flight range tasks.",
				},
				"stream_batch_max_entries": {
					Type:        framework.TypeInt,
					Description: "Secondary stream apply batch max entries.",
				},
				"stream_batch_max_bytes": {
					Type:        framework.TypeInt,
					Description: "Secondary stream apply batch max bytes.",
				},
				"stream_batch_max_wait_milliseconds": {
					Type:        framework.TypeInt,
					Description: "Secondary stream apply batch max wait in milliseconds.",
				},
				"stream_journal_enabled": {
					Type:        framework.TypeBool,
					Description: "Enable persistent primary stream journal replay.",
				},
				"stream_journal_max_bytes": {
					Type:        framework.TypeInt,
					Description: "Primary stream journal max retained bytes.",
				},
				"stream_journal_segment_bytes": {
					Type:        framework.TypeInt,
					Description: "Primary stream journal segment size in bytes.",
				},
				"stream_journal_retention_seconds": {
					Type:        framework.TypeInt,
					Description: "Primary stream journal retention window in seconds.",
				},
				"reconcile_apply_workers": {
					Type:        framework.TypeInt,
					Description: "Secondary reconcile apply worker count.",
				},
				"reconcile_put_batch_max_entries": {
					Type:        framework.TypeInt,
					Description: "Secondary reconcile put batching max entries.",
				},
				"reconcile_put_batch_max_bytes": {
					Type:        framework.TypeInt,
					Description: "Secondary reconcile put batching max bytes.",
				},
				"convergence_min_rate_ratio": {
					Type:        framework.TypeFloat,
					Description: "Minimum secondary_apply_rate_eps / primary_write_rate_eps before convergence fallback.",
				},
				"convergence_stall_seconds": {
					Type:        framework.TypeInt,
					Description: "Lag-growth duration before convergence fallback can trigger.",
				},
				"fallback_enabled": {
					Type:        framework.TypeBool,
					Description: "Enable automatic resnapshot fallback under sustained lag pressure.",
				},
				"fallback_stall_seconds": {
					Type:        framework.TypeInt,
					Description: "Stall duration required before fallback can trigger.",
				},
				"fallback_failure_threshold": {
					Type:        framework.TypeInt,
					Description: "Failure count threshold in the fallback rolling window.",
				},
				"fallback_min_lag_entries": {
					Type:        framework.TypeInt,
					Description: "Minimum lag entries required before fallback can trigger.",
				},
				"fallback_cooldown_seconds": {
					Type:        framework.TypeInt,
					Description: "Cooldown between fallback runs in seconds.",
				},
				"fallback_max_per_hour": {
					Type:        framework.TypeInt,
					Description: "Maximum fallback runs per hour.",
				},
				"checkpoint_artifact_enabled": {
					Type:        framework.TypeBool,
					Description: "Enable immutable disk-backed checkpoint artifacts for DR fetches.",
				},
				"checkpoint_artifact_global_budget_bytes": {
					Type:        framework.TypeInt,
					Description: "Global checkpoint artifact budget in bytes.",
				},
				"checkpoint_artifact_per_relationship_budget_bytes": {
					Type:        framework.TypeInt,
					Description: "Per-relationship checkpoint artifact budget in bytes.",
				},
				"checkpoint_artifact_ttl_seconds": {
					Type:        framework.TypeInt,
					Description: "Checkpoint artifact TTL in seconds.",
				},
				"checkpoint_artifact_segment_bytes": {
					Type:        framework.TypeInt,
					Description: "Checkpoint artifact segment size in bytes.",
				},
				"dr_backpressure_enabled": {
					Type:        framework.TypeBool,
					Description: "Enable DR-aware bounded write backpressure on primary.",
				},
				"dr_backpressure_degraded_ratio": {
					Type:        framework.TypeFloat,
					Description: "Apply-rate ratio threshold for degraded backpressure state.",
				},
				"dr_backpressure_critical_ratio": {
					Type:        framework.TypeFloat,
					Description: "Apply-rate ratio threshold for critical backpressure state.",
				},
				"dr_backpressure_min_lag_entries": {
					Type:        framework.TypeInt,
					Description: "Minimum lag entries before backpressure can engage.",
				},
				"dr_backpressure_horizon_seconds": {
					Type:        framework.TypeInt,
					Description: "Stream horizon threshold used for critical backpressure state.",
				},
				"dr_backpressure_degraded_min_qps": {
					Type:        framework.TypeInt,
					Description: "Minimum admitted QPS in degraded backpressure state.",
				},
				"dr_backpressure_critical_min_qps": {
					Type:        framework.TypeInt,
					Description: "Minimum admitted QPS in critical backpressure state.",
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleDRTuningRead,
					Summary:  "Read DR runtime tuning values.",
				},
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRTuningWrite,
					Summary:                   "Update DR runtime tuning values.",
					ForwardPerformanceStandby: true,
				},
			},
			HelpSynopsis:    "Read/update DR tuning",
			HelpDescription: "Reads and updates persisted DR runtime tuning values.",
		},
	}
}

// --- Handler implementations ---

func (b *SystemBackend) handleDRStatus(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	config := mgr.Config()
	data := map[string]interface{}{
		"mode":       string(config.Mode),
		"cluster_id": config.ClusterID,
	}
	if record := config.Promotion; record != nil {
		data["last_promotion_id"] = record.PromotionID
		data["last_promotion_class"] = string(record.PromotionClass)
		data["last_promotion_time"] = record.PromotedAt
		data["last_promotion_local_cluster_id"] = record.LocalClusterID
		data["last_promotion_last_applied_index"] = record.LastAppliedIndex
		data["last_promotion_last_known_primary_index"] = record.LastKnownPrimaryIndex
		data["last_promotion_estimated_data_loss_entries"] = record.EstimatedDataLossEntries
		data["last_promotion_estimated_data_loss_entries_basis"] = record.DataLossEstimateBasis
		data["last_promotion_data_loss_accepted"] = record.DataLossAccepted
		data["last_promotion_clean_promotion_eligible"] = record.PromotionClass == DRPromotionClean
		data["last_promotion_clean_promotion_proof_available"] = record.PromotionClass == DRPromotionClean
		data["last_promotion_forced_promotion_requires_acknowledgement"] = record.PromotionClass == DRPromotionForced
		if len(record.ForcedReasonCodes) > 0 {
			data["last_promotion_forced_reason_codes"] = append([]string(nil), record.ForcedReasonCodes...)
		}
		if len(record.ForcedReasonDetails) > 0 {
			data["last_promotion_forced_reason_details"] = append([]string(nil), record.ForcedReasonDetails...)
		}
	}

	// If secondary, include secondary-specific status.
	if sec := mgr.Secondary(); sec != nil {
		status := sec.Status()
		data["secondary_state"] = status.State
		data["primary_index"] = status.PrimaryIndex
		data["last_applied_index"] = status.LastAppliedIndex
		data["entries_applied"] = status.EntriesApplied
		data["reconcile_count"] = status.ReconcileCount
		data["last_reconcile_at"] = status.LastReconcileAt
		data["connect_retries"] = status.ConnectRetries
		data["connect_failures"] = status.ConnectFailures
		data["reconcile_ranges_inflight"] = status.ReconcileRangesInflight
		data["reconcile_ranges_failed"] = status.ReconcileRangesFailed
		data["reconcile_budget_remaining_bytes"] = status.ReconcileBudgetRemainingBytes
		data["reconcile_active_checkpoint_id"] = status.ReconcileActiveCheckpointID
		data["reconcile_active_checkpoint_index"] = status.ReconcileActiveCheckpointIndex
		data["reconcile_fail_reason_last"] = status.ReconcileFailReasonLast
		data["range_manifest_count"] = status.RangeManifestCount
		data["range_split_count"] = status.RangeSplitCount
		data["reconcile_rpc_bytes_used"] = status.ReconcileRPCBytesUsed
		data["flat_accumulator_fast_path_total"] = status.FlatAccumulatorFastPathTotal
		data["scan_failures_total"] = status.ScanFailuresTotal
		data["checkpoint_conflicts_total"] = status.CheckpointConflictsTotal
		data["reconcile_retries_total"] = status.ReconcileRetriesTotal
		data["reconcile_queue_depth"] = status.ReconcileQueueDepth
		data["reconcile_task_retries_total"] = status.ReconcileTaskRetriesTotal
		data["reconcile_decode_failures_total"] = status.ReconcileDecodeFailuresTotal
		data["reconcile_stalled_total"] = status.ReconcileStalledTotal
		data["reconcile_stuck_seconds"] = status.ReconcileStuckSeconds
		data["reconcile_phase"] = status.ReconcilePhase
		data["last_applied_age_seconds"] = status.LastAppliedAgeSeconds
		data["reconcile_max_rpc_bytes"] = status.ReconcileMaxRPCBytes
		data["reconcile_max_wall_time_seconds"] = status.ReconcileMaxWallTimeSeconds
		data["reconcile_max_inflight_tasks"] = status.ReconcileMaxInflightTasks
		data["stream_batch_max_entries"] = status.StreamBatchMaxEntries
		data["stream_batch_max_bytes"] = status.StreamBatchMaxBytes
		data["stream_batch_max_wait_milliseconds"] = status.StreamBatchMaxWaitMilliseconds
		data["fallback_active"] = status.FallbackActive
		data["fallback_count"] = status.FallbackCount
		data["fallback_last_reason"] = status.FallbackLastReason
		data["fallback_last_at"] = status.FallbackLastAt
		data["reconcile_task_rate"] = status.ReconcileTaskRate
		data["secondary_apply_rate_eps"] = status.SecondaryApplyRateEPS
		data["lag_entries"] = status.LagEntries
		data["lag_slope_eps"] = status.LagSlopeEPS
		data["predicted_catchup_seconds"] = status.PredictedCatchupSeconds
		data["reconcile_put_workers_active"] = status.ReconcilePutWorkersActive
		data["reconcile_delete_phase_seconds"] = status.ReconcileDeletePhaseSeconds
		data["primary_write_rate_eps"] = status.PrimaryWriteRateEPS
	}

	if primary := mgr.Primary(); primary != nil {
		cacheBytes, cacheItems, cacheEvictions, cacheMetaBytes, cacheValueBytes, cacheAdmissionFailures := primary.checkpointCacheStats()
		streamEntries, streamBytes := primary.streamBufferStats()
		checkpointTTLSeconds, checkpointGlobalBudget, checkpointPerRelationshipBudget, streamBufferMaxEntries, streamBufferMaxBytes := primary.tuningSnapshot()
		data["checkpoint_cache_bytes"] = cacheBytes
		data["checkpoint_cache_items"] = cacheItems
		data["checkpoint_cache_evictions"] = cacheEvictions
		data["checkpoint_meta_bytes"] = cacheMetaBytes
		data["checkpoint_value_bytes"] = cacheValueBytes
		data["checkpoint_admission_failures"] = cacheAdmissionFailures
		data["checkpoint_throttle_total"] = primary.checkpointThrottleCount()
		data["checkpoint_throttle_bypass_total"] = primary.checkpointThrottleBypassCount()
		checkpointBuildInFlight, checkpointBuildMaxInFlight, checkpointBuildAdmissionFailures := primary.checkpointBuildStats()
		data["checkpoint_build_inflight"] = checkpointBuildInFlight
		data["checkpoint_build_max_inflight"] = checkpointBuildMaxInFlight
		data["checkpoint_build_admission_failures"] = checkpointBuildAdmissionFailures
		data["revoked_streams_terminated"] = primary.revokedStreamsTerminatedCount()
		data["stream_buffer_entries"] = streamEntries
		data["stream_buffer_bytes"] = streamBytes
		data["stream_lagging_subscribers_total"] = primary.laggingSubscribersCount()
		data["stream_lagging_subscribers_active"] = primary.laggingSubscribersActiveCount()
		data["stream_subscribers_active"] = primary.subscriberCount()
		data["scan_failures_total"] = primary.scanFailuresCount()
		data["checkpoint_ttl_seconds"] = checkpointTTLSeconds
		data["checkpoint_global_budget_bytes"] = checkpointGlobalBudget
		data["checkpoint_per_relationship_budget_bytes"] = checkpointPerRelationshipBudget
		data["stream_buffer_max_entries"] = streamBufferMaxEntries
		data["stream_buffer_max_bytes"] = streamBufferMaxBytes
		data["stream_buffer_horizon_seconds"] = primary.streamBufferHorizonSeconds()
		data["primary_write_rate_eps"] = primary.writeRate()
		journalBytes, journalSegments, journalOldest := primary.streamJournalSnapshot()
		data["stream_journal_bytes"] = journalBytes
		data["stream_journal_segments"] = journalSegments
		data["stream_journal_oldest_index"] = journalOldest
		replayAttempts, replaySuccess, replayTooOld := primary.streamJournalReplayStats()
		data["journal_replay_attempts_total"] = replayAttempts
		data["journal_replay_success_total"] = replaySuccess
		data["journal_range_too_old_total"] = replayTooOld
		artifactBytes, artifactItems, artifactEvictions, artifactBuildSeconds, artifactStorageDrift := primary.checkpointArtifactStats()
		data["checkpoint_artifact_bytes"] = artifactBytes
		data["checkpoint_artifact_items"] = artifactItems
		data["checkpoint_artifact_evictions"] = artifactEvictions
		data["checkpoint_artifact_build_seconds"] = artifactBuildSeconds
		data["checkpoint_conflicts_storage_drift_total"] = artifactStorageDrift
		backpressureState, backpressureRejected, backpressureCap := primary.backpressureStatus()
		data["dr_backpressure_state"] = backpressureState
		data["dr_backpressure_rejections_total"] = backpressureRejected
		data["dr_backpressure_effective_qps_cap"] = backpressureCap
	}

	if config.Mode == DRModePrimary {
		rels, err := mgr.ListRelationships(ctx)
		if err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
		counts := map[string]int{}
		for _, rel := range rels {
			counts[string(rel.State)]++
		}
		data["relationship_count_by_state"] = counts
	}

	return &logical.Response{
		Data: data,
	}, nil
}

func (b *SystemBackend) handleDRPrimaryEnable(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	if err := mgr.EnablePrimary(ctx); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return nil, nil
}

func (b *SystemBackend) handleDRPrimaryDisable(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	if err := mgr.DisablePrimary(ctx); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return nil, nil
}

func (b *SystemBackend) handleDRPrimaryGenerateToken(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	token, err := mgr.GenerateActivationToken(ctx)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal activation token: %w", err)
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"token": string(tokenJSON),
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryEnable(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	tokenRaw, ok := d.GetOk("token")
	if !ok {
		return logical.ErrorResponse("token is required"), nil
	}
	tokenStr, ok := tokenRaw.(string)
	if !ok || tokenStr == "" {
		return logical.ErrorResponse("token must be a non-empty string"), nil
	}
	if len(tokenStr) > drActivationTokenMaxBytes {
		return logical.ErrorResponse("activation token exceeds maximum size %d", drActivationTokenMaxBytes), nil
	}

	var token DRActivationToken
	if err := json.Unmarshal([]byte(tokenStr), &token); err != nil {
		return logical.ErrorResponse("invalid activation token: %s", err.Error()), nil
	}

	// Validate required token fields.
	if token.ClusterID == "" {
		return logical.ErrorResponse("activation token missing cluster_id"), nil
	}
	if token.RelationshipID == "" {
		return logical.ErrorResponse("activation token missing relationship_id"), nil
	}
	token.PrimaryAddrs = normalizePrimaryAddrs(token.PrimaryAddrs)
	if len(token.PrimaryAddrs) == 0 {
		return logical.ErrorResponse("activation token missing primary_addrs"), nil
	}
	token.PrimaryAddr = token.PrimaryAddrs[0]
	if len(token.ReplSalt) != drReplSaltLen {
		return logical.ErrorResponse("activation token has invalid repl_salt (expected %d bytes, got %d)", drReplSaltLen, len(token.ReplSalt)), nil
	}

	if err := mgr.EnableSecondary(ctx, &token); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryDisable(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	if err := mgr.DisableSecondary(ctx); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryRotateCertificate(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}
	result, err := mgr.RotateSecondaryCredential(ctx)
	if err != nil {
		return logical.ErrorResponse("credential rotation failed: %s", err.Error()), nil
	}
	b.Core.logger.Info("DR secondary credential rotated via API",
		"relationship_id", result.RelationshipID,
		"operation_id", result.OperationID,
		"pending_resumed", result.PendingResumed,
		"source_ip", sourceIPFromRequest(req))
	return &logical.Response{Data: map[string]interface{}{
		"relationship_id":       result.RelationshipID,
		"operation_id":          result.OperationID,
		"old_fingerprint":       result.OldFingerprint,
		"new_fingerprint":       result.NewFingerprint,
		"pending_resumed":       result.PendingResumed,
		"rotation_started_at":   result.RotationStartedAt,
		"rotation_completed_at": result.RotationCompletedAt,
	}}, nil
}

func (b *SystemBackend) handleDRPrimaryRegisterSecondary(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	relationshipIDRaw, ok := d.GetOk("relationship_id")
	if !ok {
		return logical.ErrorResponse("relationship_id is required"), nil
	}
	relationshipID, ok := relationshipIDRaw.(string)
	if !ok || relationshipID == "" {
		return logical.ErrorResponse("relationship_id must be a non-empty string"), nil
	}

	bootstrapTokenRaw, ok := d.GetOk("bootstrap_token")
	if !ok {
		return logical.ErrorResponse("bootstrap_token is required"), nil
	}
	bootstrapToken, ok := bootstrapTokenRaw.(string)
	if !ok || bootstrapToken == "" {
		return logical.ErrorResponse("bootstrap_token must be a non-empty string"), nil
	}

	certRaw, ok := d.GetOk("secondary_ca_cert")
	if !ok {
		return logical.ErrorResponse("secondary_ca_cert is required"), nil
	}
	certStr, ok := certRaw.(string)
	if !ok || certStr == "" {
		return logical.ErrorResponse("secondary_ca_cert must be a non-empty string"), nil
	}
	if len(certStr) > base64.StdEncoding.EncodedLen(drBootstrapMaxCertDERBytes) {
		return logical.ErrorResponse("secondary_ca_cert exceeds maximum DER size %d", drBootstrapMaxCertDERBytes), nil
	}

	// Decode the base64-encoded DER certificate.
	certDER, err := base64.StdEncoding.DecodeString(certStr)
	if err != nil {
		return logical.ErrorResponse("secondary_ca_cert is not valid base64: %s", err.Error()), nil
	}

	mgr := b.Core.drManager
	if mgr == nil {
		return b.drBootstrapRegistrationFailure(req, relationshipID, fmt.Errorf("DR replication not initialized")), nil
	}

	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, relationshipID, bootstrapToken, certDER, sourceIPFromRequest(req)); err != nil {
		return b.drBootstrapRegistrationFailure(req, relationshipID, err), nil
	}

	return nil, nil
}

func drCredentialRotationFieldSchemas() map[string]*framework.FieldSchema {
	return map[string]*framework.FieldSchema{
		"relationship_id": {
			Type:        framework.TypeString,
			Description: "The active DR relationship ID.",
			Required:    true,
		},
		"operation_id": {
			Type:        framework.TypeString,
			Description: "Client-generated UUID binding the two-phase rotation attempt.",
			Required:    true,
		},
		"issued_at": {
			Type:        framework.TypeInt,
			Description: "Unix timestamp covered by the rotation signature.",
			Required:    true,
		},
		"secondary_ca_cert": {
			Type:        framework.TypeString,
			Description: "The new secondary DR client certificate trust anchor (base64-encoded DER).",
			Required:    true,
			DisplayAttrs: &framework.DisplayAttributes{
				Sensitive: true,
			},
		},
		"signature": {
			Type:        framework.TypeString,
			Description: "Base64-encoded ECDSA signature over the credential-rotation request.",
			Required:    true,
			DisplayAttrs: &framework.DisplayAttributes{
				Sensitive: true,
			},
		},
	}
}

func parseDRCredentialRotationRequest(d *framework.FieldData) (relationshipID, operationID string, issuedAt int64, certDER, signature []byte, resp *logical.Response) {
	rawRelationshipID, ok := d.GetOk("relationship_id")
	if !ok {
		resp = logical.ErrorResponse("relationship_id is required")
		return
	}
	relationshipID, ok = rawRelationshipID.(string)
	if !ok || relationshipID == "" {
		resp = logical.ErrorResponse("relationship_id must be a non-empty string")
		return
	}

	rawOperationID, ok := d.GetOk("operation_id")
	if !ok {
		resp = logical.ErrorResponse("operation_id is required")
		return
	}
	operationID, ok = rawOperationID.(string)
	if !ok || operationID == "" {
		resp = logical.ErrorResponse("operation_id must be a non-empty string")
		return
	}

	rawIssuedAt, ok := d.GetOk("issued_at")
	if !ok {
		resp = logical.ErrorResponse("issued_at is required")
		return
	}
	switch v := rawIssuedAt.(type) {
	case int:
		issuedAt = int64(v)
	case int64:
		issuedAt = v
	case int32:
		issuedAt = int64(v)
	case uint64:
		if v <= uint64(^uint(0)>>1) {
			issuedAt = int64(v)
		}
	case float64:
		issuedAt = int64(v)
	}
	if issuedAt <= 0 {
		resp = logical.ErrorResponse("issued_at must be a positive Unix timestamp")
		return
	}

	certRaw, ok := d.GetOk("secondary_ca_cert")
	if !ok {
		resp = logical.ErrorResponse("secondary_ca_cert is required")
		return
	}
	certStr, ok := certRaw.(string)
	if !ok || certStr == "" {
		resp = logical.ErrorResponse("secondary_ca_cert must be a non-empty string")
		return
	}
	if len(certStr) > base64.StdEncoding.EncodedLen(drBootstrapMaxCertDERBytes) {
		resp = logical.ErrorResponse("secondary_ca_cert exceeds maximum DER size %d", drBootstrapMaxCertDERBytes)
		return
	}
	certDER, err := base64.StdEncoding.DecodeString(certStr)
	if err != nil {
		resp = logical.ErrorResponse("secondary_ca_cert is not valid base64: %s", err.Error())
		return
	}

	signatureRaw, ok := d.GetOk("signature")
	if !ok {
		resp = logical.ErrorResponse("signature is required")
		return
	}
	signatureStr, ok := signatureRaw.(string)
	if !ok || signatureStr == "" {
		resp = logical.ErrorResponse("signature must be a non-empty string")
		return
	}
	if len(signatureStr) > base64.StdEncoding.EncodedLen(drCredentialRotationMaxSigLen) {
		resp = logical.ErrorResponse("signature exceeds maximum size %d", drCredentialRotationMaxSigLen)
		return
	}
	signature, err = base64.StdEncoding.DecodeString(signatureStr)
	if err != nil {
		resp = logical.ErrorResponse("signature is not valid base64: %s", err.Error())
		return
	}
	return
}

func credentialRotationResponse(rel *DRRelationship) *logical.Response {
	return &logical.Response{Data: map[string]interface{}{
		"relationship_id":                     rel.RelationshipID,
		"credential_generation":               rel.CredentialGeneration,
		"secondary_cert_fingerprint":          rel.SecondaryCertFingerprint,
		"pending_secondary_cert_fingerprint":  rel.PendingSecondaryCertFingerprint,
		"previous_secondary_cert_fingerprint": rel.PreviousSecondaryCertFingerprint,
		"pending_rotation_operation_id":       rel.PendingRotationOperationID,
		"pending_rotation_started_at":         rel.PendingRotationStartedAt,
		"rotated_at":                          rel.RotatedAt,
	}}
}

func (b *SystemBackend) handleDRPrimaryRotateSecondaryCertificate(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	relationshipID, operationID, issuedAt, certDER, signature, resp := parseDRCredentialRotationRequest(d)
	if resp != nil {
		return resp, nil
	}

	mgr := b.Core.drManager
	if mgr == nil {
		return b.drCredentialRotationFailure(req, relationshipID, operationID, fmt.Errorf("DR replication not initialized")), nil
	}
	rel, err := mgr.InitiateSecondaryCredentialRotation(ctx, relationshipID, operationID, issuedAt, certDER, signature, sourceIPFromRequest(req))
	if err != nil {
		return b.drCredentialRotationFailure(req, relationshipID, operationID, err), nil
	}
	return credentialRotationResponse(rel), nil
}

func (b *SystemBackend) handleDRPrimaryConfirmSecondaryCertificate(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	relationshipID, operationID, issuedAt, certDER, signature, resp := parseDRCredentialRotationRequest(d)
	if resp != nil {
		return resp, nil
	}

	mgr := b.Core.drManager
	if mgr == nil {
		return b.drCredentialRotationFailure(req, relationshipID, operationID, fmt.Errorf("DR replication not initialized")), nil
	}
	rel, err := mgr.ConfirmSecondaryCredentialRotation(ctx, relationshipID, operationID, issuedAt, certDER, signature, sourceIPFromRequest(req))
	if err != nil {
		return b.drCredentialRotationFailure(req, relationshipID, operationID, err), nil
	}
	return credentialRotationResponse(rel), nil
}

func (b *SystemBackend) drBootstrapRegistrationFailure(req *logical.Request, relationshipID string, err error) *logical.Response {
	b.Core.logger.Warn("DR bootstrap registration rejected",
		"relationship_id", relationshipID,
		"source_ip", sourceIPFromRequest(req),
		"error", err)
	return logical.ErrorResponse("registration failed")
}

func (b *SystemBackend) drCredentialRotationFailure(req *logical.Request, relationshipID, operationID string, err error) *logical.Response {
	b.Core.logger.Warn("DR secondary credential rotation rejected",
		"relationship_id", relationshipID,
		"operation_id", operationID,
		"source_ip", sourceIPFromRequest(req),
		"error", err)
	return logical.ErrorResponse("credential rotation failed")
}

func sourceIPFromRequest(req *logical.Request) string {
	if req == nil || req.Connection == nil || req.Connection.RemoteAddr == "" {
		return ""
	}
	addr := req.Connection.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func drRelationshipResponseData(rel *DRRelationship) map[string]interface{} {
	return map[string]interface{}{
		"relationship_id":             rel.RelationshipID,
		"state":                       string(rel.State),
		"secondary_cert_fingerprint":  rel.SecondaryCertFingerprint,
		"credential_generation":       rel.CredentialGeneration,
		"rotated_at":                  rel.RotatedAt,
		"pending_rotation":            rel.PendingSecondaryCertFingerprint != "",
		"pending_rotation_started_at": rel.PendingRotationStartedAt,
		"created_at":                  rel.CreatedAt,
		"last_seen_at":                rel.LastSeenAt,
		"expires_at":                  rel.ExpiresAt,
		"failed_attempts":             rel.FailedAttempts,
		"locked_until":                rel.LockedUntil,
		"registered_from_ip":          rel.RegisteredFromIP,
		"registered_at":               rel.RegisteredAt,
		"last_failed_at":              rel.LastFailedAt,
		"last_failed_from_ip":         rel.LastFailedFromIP,
		"revoked_at":                  rel.RevokedAt,
		"last_error":                  rel.LastError,
	}
}

func (b *SystemBackend) handleDRPrimaryListRelationships(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	rels, err := mgr.ListRelationships(ctx)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	resp := make([]map[string]interface{}, 0, len(rels))
	for _, rel := range rels {
		resp = append(resp, drRelationshipResponseData(rel))
	}

	return &logical.Response{
		Data: map[string]interface{}{"relationships": resp},
	}, nil
}

func (b *SystemBackend) handleDRPrimaryRelationshipStatus(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	idRaw, ok := d.GetOk("id")
	if !ok {
		return logical.ErrorResponse("relationship id is required"), nil
	}
	id, _ := idRaw.(string)
	rel, err := mgr.GetRelationship(ctx, id)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return &logical.Response{
		Data: drRelationshipResponseData(rel),
	}, nil
}

func (b *SystemBackend) handleDRPrimaryRelationshipRevoke(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	idRaw, ok := d.GetOk("id")
	if !ok {
		return logical.ErrorResponse("relationship id is required"), nil
	}
	id, _ := idRaw.(string)
	if id == "" {
		return logical.ErrorResponse("relationship id is required"), nil
	}

	if err := mgr.RevokeRelationship(ctx, id); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	b.Core.logger.Info("DR relationship revoked via API",
		"relationship_id", id,
		"source_ip", sourceIPFromRequest(req))
	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryResnapshot(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	confirmRollback := d.Get("confirm_rollback_ok").(bool)
	if !confirmRollback {
		return logical.ErrorResponse(
			"resnapshot resets the checkpoint high-water mark, which allows the secondary to accept older checkpoint indices; " +
				"set confirm_rollback_ok=true to proceed",
		), nil
	}

	// Log the HWM reset for audit trail.
	oldHWM := uint64(0)
	if sec := mgr.Secondary(); sec != nil {
		sec.sessionMu.RLock()
		oldHWM = sec.highestCommittedCheckpointIndex
		sec.sessionMu.RUnlock()
	}
	b.Core.logger.Info("DR resnapshot: checkpoint high-water mark reset",
		"old_hwm", oldHWM,
		"new_hwm", 0,
		"reason", "manual-api",
		"source_ip", sourceIPFromRequest(req))

	if err := mgr.RequestSecondaryResnapshot("manual-api"); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":  "DR secondary resnapshot requested",
			"old_hwm":  oldHWM,
			"reset_to": 0,
		},
	}, nil
}

func (b *SystemBackend) handleDRTuningRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}
	cfg := mgr.Config()
	return &logical.Response{
		Data: map[string]interface{}{
			"checkpoint_ttl_seconds":                            cfg.CheckpointTTLSeconds,
			"checkpoint_global_budget_bytes":                    cfg.CheckpointGlobalBudgetBytes,
			"checkpoint_per_relationship_budget_bytes":          cfg.CheckpointPerRelBudgetBytes,
			"stream_buffer_max_entries":                         cfg.StreamBufferMaxEntries,
			"stream_buffer_max_bytes":                           cfg.StreamBufferMaxBytes,
			"reconcile_max_rpc_bytes":                           cfg.ReconcileMaxRPCBytes,
			"reconcile_max_wall_time_seconds":                   cfg.ReconcileMaxWallTimeSeconds,
			"reconcile_max_inflight_tasks":                      cfg.ReconcileMaxInflightTasks,
			"stream_batch_max_entries":                          cfg.StreamBatchMaxEntries,
			"stream_batch_max_bytes":                            cfg.StreamBatchMaxBytes,
			"stream_batch_max_wait_milliseconds":                cfg.StreamBatchMaxWaitMillis,
			"stream_journal_enabled":                            cfg.StreamJournalEnabled,
			"stream_journal_max_bytes":                          cfg.StreamJournalMaxBytes,
			"stream_journal_segment_bytes":                      cfg.StreamJournalSegmentBytes,
			"stream_journal_retention_seconds":                  cfg.StreamJournalRetentionSecs,
			"reconcile_apply_workers":                           cfg.ReconcileApplyWorkers,
			"reconcile_put_batch_max_entries":                   cfg.ReconcilePutBatchMaxEntries,
			"reconcile_put_batch_max_bytes":                     cfg.ReconcilePutBatchMaxBytes,
			"convergence_min_rate_ratio":                        cfg.ConvergenceMinRateRatio,
			"convergence_stall_seconds":                         cfg.ConvergenceStallSeconds,
			"fallback_enabled":                                  cfg.FallbackEnabled,
			"fallback_stall_seconds":                            cfg.FallbackStallSeconds,
			"fallback_failure_threshold":                        cfg.FallbackFailureThreshold,
			"fallback_min_lag_entries":                          cfg.FallbackMinLagEntries,
			"fallback_cooldown_seconds":                         cfg.FallbackCooldownSeconds,
			"fallback_max_per_hour":                             cfg.FallbackMaxPerHour,
			"checkpoint_artifact_enabled":                       cfg.CheckpointArtifactEnabled,
			"checkpoint_artifact_global_budget_bytes":           cfg.CheckpointArtifactGlobalBudgetBytes,
			"checkpoint_artifact_per_relationship_budget_bytes": cfg.CheckpointArtifactPerRelBudgetBytes,
			"checkpoint_artifact_ttl_seconds":                   cfg.CheckpointArtifactTTLSeconds,
			"checkpoint_artifact_segment_bytes":                 cfg.CheckpointArtifactSegmentBytes,
			"dr_backpressure_enabled":                           cfg.DRBackpressureEnabled,
			"dr_backpressure_degraded_ratio":                    cfg.DRBackpressureDegradedRatio,
			"dr_backpressure_critical_ratio":                    cfg.DRBackpressureCriticalRatio,
			"dr_backpressure_min_lag_entries":                   cfg.DRBackpressureMinLagEntries,
			"dr_backpressure_horizon_seconds":                   cfg.DRBackpressureHorizonSeconds,
			"dr_backpressure_degraded_min_qps":                  cfg.DRBackpressureDegradedMinQPS,
			"dr_backpressure_critical_min_qps":                  cfg.DRBackpressureCriticalMinQPS,
		},
	}, nil
}

func (b *SystemBackend) handleDRTuningWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}
	getPositiveInt := func(name string) (int, bool, error) {
		raw, ok, err := d.GetOkErr(name)
		if err != nil || !ok {
			return 0, ok, err
		}
		value, ok := raw.(int)
		if !ok {
			return 0, false, fmt.Errorf("%s must be an integer", name)
		}
		if value <= 0 {
			return 0, false, fmt.Errorf("%s must be greater than zero", name)
		}
		return value, true, nil
	}
	setInt := func(name string, dest *int) error {
		value, ok, err := getPositiveInt(name)
		if err != nil || !ok {
			return err
		}
		*dest = value
		return nil
	}
	setInt64 := func(name string, dest *int64) error {
		value, ok, err := getPositiveInt(name)
		if err != nil || !ok {
			return err
		}
		*dest = int64(value)
		return nil
	}
	setUint64 := func(name string, dest *uint64) error {
		value, ok, err := getPositiveInt(name)
		if err != nil || !ok {
			return err
		}
		*dest = uint64(value)
		return nil
	}
	setRatio := func(name string, dest *float64) error {
		raw, ok, err := d.GetOkErr(name)
		if err != nil || !ok {
			return err
		}
		value, ok := raw.(float64)
		if !ok {
			return fmt.Errorf("%s must be a number", name)
		}
		if value <= 0 || value > 1 {
			return fmt.Errorf("%s must be > 0 and <= 1", name)
		}
		*dest = value
		return nil
	}
	setBool := func(name string, dest *bool) error {
		raw, ok, err := d.GetOkErr(name)
		if err != nil || !ok {
			return err
		}
		value, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("%s must be a boolean", name)
		}
		*dest = value
		return nil
	}
	if err := mgr.UpdateTuning(ctx, func(cfg *DRConfig) error {
		for _, set := range []func() error{
			func() error { return setInt64("checkpoint_ttl_seconds", &cfg.CheckpointTTLSeconds) },
			func() error { return setUint64("checkpoint_global_budget_bytes", &cfg.CheckpointGlobalBudgetBytes) },
			func() error {
				return setUint64("checkpoint_per_relationship_budget_bytes", &cfg.CheckpointPerRelBudgetBytes)
			},
			func() error { return setInt("stream_buffer_max_entries", &cfg.StreamBufferMaxEntries) },
			func() error { return setUint64("stream_buffer_max_bytes", &cfg.StreamBufferMaxBytes) },
			func() error { return setUint64("reconcile_max_rpc_bytes", &cfg.ReconcileMaxRPCBytes) },
			func() error { return setInt64("reconcile_max_wall_time_seconds", &cfg.ReconcileMaxWallTimeSeconds) },
			func() error { return setInt("reconcile_max_inflight_tasks", &cfg.ReconcileMaxInflightTasks) },
			func() error { return setInt("stream_batch_max_entries", &cfg.StreamBatchMaxEntries) },
			func() error { return setInt("stream_batch_max_bytes", &cfg.StreamBatchMaxBytes) },
			func() error { return setInt64("stream_batch_max_wait_milliseconds", &cfg.StreamBatchMaxWaitMillis) },
			func() error { return setBool("stream_journal_enabled", &cfg.StreamJournalEnabled) },
			func() error { return setUint64("stream_journal_max_bytes", &cfg.StreamJournalMaxBytes) },
			func() error { return setUint64("stream_journal_segment_bytes", &cfg.StreamJournalSegmentBytes) },
			func() error { return setInt64("stream_journal_retention_seconds", &cfg.StreamJournalRetentionSecs) },
			func() error { return setInt("reconcile_apply_workers", &cfg.ReconcileApplyWorkers) },
			func() error { return setInt("reconcile_put_batch_max_entries", &cfg.ReconcilePutBatchMaxEntries) },
			func() error { return setInt("reconcile_put_batch_max_bytes", &cfg.ReconcilePutBatchMaxBytes) },
			func() error { return setRatio("convergence_min_rate_ratio", &cfg.ConvergenceMinRateRatio) },
			func() error { return setInt64("convergence_stall_seconds", &cfg.ConvergenceStallSeconds) },
			func() error { return setBool("fallback_enabled", &cfg.FallbackEnabled) },
			func() error { return setInt64("fallback_stall_seconds", &cfg.FallbackStallSeconds) },
			func() error { return setInt("fallback_failure_threshold", &cfg.FallbackFailureThreshold) },
			func() error { return setUint64("fallback_min_lag_entries", &cfg.FallbackMinLagEntries) },
			func() error { return setInt64("fallback_cooldown_seconds", &cfg.FallbackCooldownSeconds) },
			func() error { return setInt("fallback_max_per_hour", &cfg.FallbackMaxPerHour) },
			func() error { return setBool("checkpoint_artifact_enabled", &cfg.CheckpointArtifactEnabled) },
			func() error {
				return setUint64("checkpoint_artifact_global_budget_bytes", &cfg.CheckpointArtifactGlobalBudgetBytes)
			},
			func() error {
				return setUint64("checkpoint_artifact_per_relationship_budget_bytes", &cfg.CheckpointArtifactPerRelBudgetBytes)
			},
			func() error { return setInt64("checkpoint_artifact_ttl_seconds", &cfg.CheckpointArtifactTTLSeconds) },
			func() error {
				return setUint64("checkpoint_artifact_segment_bytes", &cfg.CheckpointArtifactSegmentBytes)
			},
			func() error { return setBool("dr_backpressure_enabled", &cfg.DRBackpressureEnabled) },
			func() error { return setRatio("dr_backpressure_degraded_ratio", &cfg.DRBackpressureDegradedRatio) },
			func() error { return setRatio("dr_backpressure_critical_ratio", &cfg.DRBackpressureCriticalRatio) },
			func() error { return setUint64("dr_backpressure_min_lag_entries", &cfg.DRBackpressureMinLagEntries) },
			func() error { return setInt64("dr_backpressure_horizon_seconds", &cfg.DRBackpressureHorizonSeconds) },
			func() error { return setInt64("dr_backpressure_degraded_min_qps", &cfg.DRBackpressureDegradedMinQPS) },
			func() error { return setInt64("dr_backpressure_critical_min_qps", &cfg.DRBackpressureCriticalMinQPS) },
		} {
			if err := set(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	b.Core.logger.Info("DR tuning updated via API", "source_ip", sourceIPFromRequest(req))
	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryPromote(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	confirmPrimaryUnreachable := d.Get("confirm_primary_unreachable").(bool)
	acceptDataLoss := d.Get("accept_data_loss").(bool)

	result, err := b.Core.DRFailover(ctx, confirmPrimaryUnreachable, acceptDataLoss)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	data := map[string]interface{}{
		"message":                                   "DR secondary promoted to standalone primary",
		"promotion_id":                              result.PromotionID,
		"promotion_class":                           string(result.PromotionClass),
		"clean_promotion_eligible":                  result.CleanPromotionEligible,
		"clean_promotion_proof_available":           result.CleanPromotionEligible,
		"forced_promotion_requires_acknowledgement": result.ForcedPromotionRequiresAcknowledgement,
		"forced_promotion_reason_codes":             append([]string(nil), result.ForcedPromotionReasonCodes...),
		"forced_promotion_reason_details":           append([]string(nil), result.ForcedPromotionReasonDetails...),
		"data_loss_accepted":                        result.DataLossAccepted,
		"last_applied_index":                        result.LastAppliedIndex,
		"last_known_primary_index":                  result.LastKnownPrimaryIndex,
		"estimated_data_loss_entries":               result.EstimatedDataLossEntries,
		"estimated_data_loss_entries_basis":         result.DataLossEstimateBasis,
		"duration":                                  result.Duration.String(),
	}
	if record := result.PromotionRecord; record != nil {
		data["promoted_at"] = record.PromotedAt
		data["local_cluster_id"] = record.LocalClusterID
		data["old_primary_cluster_id"] = record.OldPrimaryClusterID
		data["old_relationship_id"] = record.OldRelationshipID
		data["old_secondary_cert_fingerprint"] = record.OldSecondaryCertFingerprint
	}
	if result.Warning != "" {
		data["warning"] = result.Warning
	}
	b.Core.logger.Info("DR secondary promoted via API",
		"promotion_id", result.PromotionID,
		"promotion_class", result.PromotionClass,
		"data_loss_accepted", result.DataLossAccepted,
		"source_ip", sourceIPFromRequest(req))

	return &logical.Response{Data: data}, nil
}
