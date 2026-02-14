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
				},
				"secondary_ca_cert": {
					Type:        framework.TypeString,
					Description: "The secondary's cluster CA certificate (base64-encoded DER).",
					Required:    true,
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
			HelpDescription: "Registers a DR secondary's cluster CA certificate so the primary can trust it for mTLS connections. Authenticated via a one-time bootstrap token.",
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
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Promote DR secondary",
			HelpDescription: "Promotes this DR secondary to a standalone primary. This is the disaster recovery failover operation.",
		},

		// --- Secondary resnapshot trigger ---
		{
			Pattern: "replication/dr/secondary/resnapshot$",
			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "resnapshot",
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryResnapshot,
					Summary:                   "Trigger a hard-cutover DR resnapshot on the secondary.",
					ForwardPerformanceStandby: true,
				},
			},
			HelpSynopsis:    "Trigger DR secondary resnapshot",
			HelpDescription: "Requests a protocol-scoped full-copy resnapshot from the primary checkpoint.",
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
		data["scan_failures_total"] = status.ScanFailuresTotal
		data["checkpoint_conflicts_total"] = status.CheckpointConflictsTotal
		data["reconcile_retries_total"] = status.ReconcileRetriesTotal
		data["reconcile_queue_depth"] = status.ReconcileQueueDepth
		data["reconcile_task_retries_total"] = status.ReconcileTaskRetriesTotal
		data["reconcile_decode_failures_total"] = status.ReconcileDecodeFailuresTotal
		data["reconcile_stalled_total"] = status.ReconcileStalledTotal
		data["reconcile_stuck_seconds"] = status.ReconcileStuckSeconds
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
	if token.PrimaryAddr == "" {
		return logical.ErrorResponse("activation token missing primary_addr"), nil
	}
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

func (b *SystemBackend) handleDRPrimaryRegisterSecondary(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

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

	// Decode the base64-encoded DER certificate.
	certDER, err := base64.StdEncoding.DecodeString(certStr)
	if err != nil {
		return logical.ErrorResponse("secondary_ca_cert is not valid base64: %s", err.Error()), nil
	}

	if err := mgr.ValidateBootstrapAndStoreCertWithSourceIP(ctx, relationshipID, bootstrapToken, certDER, sourceIPFromRequest(req)); err != nil {
		return logical.ErrorResponse("registration failed: %s", err.Error()), nil
	}

	return nil, nil
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
		resp = append(resp, map[string]interface{}{
			"relationship_id":            rel.RelationshipID,
			"state":                      string(rel.State),
			"secondary_cert_fingerprint": rel.SecondaryCertFingerprint,
			"created_at":                 rel.CreatedAt,
			"last_seen_at":               rel.LastSeenAt,
			"expires_at":                 rel.ExpiresAt,
			"failed_attempts":            rel.FailedAttempts,
			"locked_until":               rel.LockedUntil,
			"registered_from_ip":         rel.RegisteredFromIP,
			"revoked_at":                 rel.RevokedAt,
			"last_error":                 rel.LastError,
		})
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
		Data: map[string]interface{}{
			"relationship_id":            rel.RelationshipID,
			"state":                      string(rel.State),
			"secondary_cert_fingerprint": rel.SecondaryCertFingerprint,
			"created_at":                 rel.CreatedAt,
			"last_seen_at":               rel.LastSeenAt,
			"expires_at":                 rel.ExpiresAt,
			"failed_attempts":            rel.FailedAttempts,
			"locked_until":               rel.LockedUntil,
			"registered_from_ip":         rel.RegisteredFromIP,
			"revoked_at":                 rel.RevokedAt,
			"last_error":                 rel.LastError,
		},
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
	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryResnapshot(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}
	if err := mgr.RequestSecondaryResnapshot("manual-api"); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message": "DR secondary resnapshot requested",
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
			"checkpoint_ttl_seconds":                   cfg.CheckpointTTLSeconds,
			"checkpoint_global_budget_bytes":           cfg.CheckpointGlobalBudgetBytes,
			"checkpoint_per_relationship_budget_bytes": cfg.CheckpointPerRelBudgetBytes,
			"stream_buffer_max_entries":                cfg.StreamBufferMaxEntries,
			"stream_buffer_max_bytes":                  cfg.StreamBufferMaxBytes,
			"reconcile_max_rpc_bytes":                  cfg.ReconcileMaxRPCBytes,
			"reconcile_max_wall_time_seconds":          cfg.ReconcileMaxWallTimeSeconds,
			"reconcile_max_inflight_tasks":             cfg.ReconcileMaxInflightTasks,
			"fallback_enabled":                         cfg.FallbackEnabled,
			"fallback_stall_seconds":                   cfg.FallbackStallSeconds,
			"fallback_failure_threshold":               cfg.FallbackFailureThreshold,
			"fallback_min_lag_entries":                 cfg.FallbackMinLagEntries,
			"fallback_cooldown_seconds":                cfg.FallbackCooldownSeconds,
			"fallback_max_per_hour":                    cfg.FallbackMaxPerHour,
		},
	}, nil
}

func (b *SystemBackend) handleDRTuningWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}
	if err := mgr.UpdateTuning(ctx, func(cfg *DRConfig) error {
		if raw, ok := d.GetOk("checkpoint_ttl_seconds"); ok {
			cfg.CheckpointTTLSeconds = int64(raw.(int))
		}
		if raw, ok := d.GetOk("checkpoint_global_budget_bytes"); ok {
			cfg.CheckpointGlobalBudgetBytes = uint64(raw.(int))
		}
		if raw, ok := d.GetOk("checkpoint_per_relationship_budget_bytes"); ok {
			cfg.CheckpointPerRelBudgetBytes = uint64(raw.(int))
		}
		if raw, ok := d.GetOk("stream_buffer_max_entries"); ok {
			cfg.StreamBufferMaxEntries = raw.(int)
		}
		if raw, ok := d.GetOk("stream_buffer_max_bytes"); ok {
			cfg.StreamBufferMaxBytes = uint64(raw.(int))
		}
		if raw, ok := d.GetOk("reconcile_max_rpc_bytes"); ok {
			cfg.ReconcileMaxRPCBytes = uint64(raw.(int))
		}
		if raw, ok := d.GetOk("reconcile_max_wall_time_seconds"); ok {
			cfg.ReconcileMaxWallTimeSeconds = int64(raw.(int))
		}
		if raw, ok := d.GetOk("reconcile_max_inflight_tasks"); ok {
			cfg.ReconcileMaxInflightTasks = raw.(int)
		}
		if raw, ok := d.GetOk("fallback_enabled"); ok {
			cfg.FallbackEnabled = raw.(bool)
		}
		if raw, ok := d.GetOk("fallback_stall_seconds"); ok {
			cfg.FallbackStallSeconds = int64(raw.(int))
		}
		if raw, ok := d.GetOk("fallback_failure_threshold"); ok {
			cfg.FallbackFailureThreshold = raw.(int)
		}
		if raw, ok := d.GetOk("fallback_min_lag_entries"); ok {
			cfg.FallbackMinLagEntries = uint64(raw.(int))
		}
		if raw, ok := d.GetOk("fallback_cooldown_seconds"); ok {
			cfg.FallbackCooldownSeconds = int64(raw.(int))
		}
		if raw, ok := d.GetOk("fallback_max_per_hour"); ok {
			cfg.FallbackMaxPerHour = raw.(int)
		}
		return nil
	}); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return nil, nil
}

func (b *SystemBackend) handleDRSecondaryPromote(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	if err := mgr.PromoteSecondary(ctx); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"message": "DR secondary promoted to standalone primary",
		},
	}, nil
}
