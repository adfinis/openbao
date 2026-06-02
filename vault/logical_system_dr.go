// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

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

		// --- Secondary Checkpoint Verification ---
		{
			Pattern: "replication/dr/secondary/verify-checkpoint$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary",
				OperationVerb:   "verify-checkpoint",
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleDRSecondaryVerifyCheckpoint,
					Summary:  "Verify DR secondary storage convergence against a primary checkpoint without serving replicated data.",
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"pass": {
									Type:     framework.TypeBool,
									Required: true,
								},
								"reason": {
									Type: framework.TypeString,
								},
								"state": {
									Type: framework.TypeString,
								},
								"relationship_id": {
									Type: framework.TypeString,
								},
								"checkpoint_id": {
									Type: framework.TypeString,
								},
								"checkpoint_index": {
									Type: framework.TypeInt,
								},
								"accumulator_index": {
									Type: framework.TypeInt,
								},
								"matched_ranges": {
									Type: framework.TypeInt,
								},
								"mismatched_ranges": {
									Type: framework.TypeInt,
								},
								"missing_ranges": {
									Type: framework.TypeInt,
								},
								"physical_scan_used": {
									Type: framework.TypeBool,
								},
								"optimizer_reseeded": {
									Type: framework.TypeBool,
								},
								"mismatches": {
									Type: framework.TypeSlice,
								},
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Verify DR secondary checkpoint convergence",
			HelpDescription: "Requests an immutable primary checkpoint and compares all top-level range checksums with the secondary's local flat accumulator. This is a DR control-plane verification endpoint and does not expose replicated secret values.",
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

		// --- Primary Pre-Seed Manifest ---
		{
			Pattern: "replication/dr/primary/preseed/manifest$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary-preseed",
				OperationVerb:   "generate-manifest",
			},

			Fields: map[string]*framework.FieldSchema{
				"relationship_id": {
					Type:        framework.TypeString,
					Description: "The pending, registered, or active DR relationship ID this seed bundle is bound to.",
					Required:    true,
				},
				"bundle_integrity_sha256": {
					Type:        framework.TypeString,
					Description: "Hex-encoded SHA-256 digest of the out-of-band seed bundle.",
					Required:    true,
				},
				"ttl_seconds": {
					Type:        framework.TypeInt,
					Default:     int((24 * time.Hour).Seconds()),
					Description: "Manifest validity window in seconds. Set to 0 for no expiry.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryPreSeedManifest,
					Summary:                   "Generate a relationship-bound DR pre-seed manifest.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Generate DR pre-seed manifest",
			HelpDescription: "Cuts a primary checkpoint and returns a DR pre-seed manifest that binds an out-of-band seed bundle to the relationship, range/checksum versions, local-only scrub metadata, and checkpoint identity.",
		},

		// --- Primary Pre-Seed Export ---
		{
			Pattern: "replication/dr/primary/preseed/export$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary-preseed",
				OperationVerb:   "export",
			},

			Fields: map[string]*framework.FieldSchema{
				"relationship_id": {
					Type:        framework.TypeString,
					Description: "The pending, registered, or active DR relationship ID this seed bundle is bound to.",
					Required:    true,
				},
				"ttl_seconds": {
					Type:        framework.TypeInt,
					Default:     int((24 * time.Hour).Seconds()),
					Description: "Manifest validity window in seconds. Set to 0 for no expiry.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryPreSeedExport,
					Summary:                   "Export a relationship-bound DR pre-seed bundle.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"bundle": {
									Type:     framework.TypeString,
									Required: true,
									DisplayAttrs: &framework.DisplayAttributes{
										Sensitive: true,
									},
								},
								"manifest": {
									Type: framework.TypeString,
								},
								"relationship_id": {
									Type: framework.TypeString,
								},
								"checkpoint_id": {
									Type: framework.TypeString,
								},
								"checkpoint_index": {
									Type: framework.TypeInt,
								},
								"entry_count": {
									Type: framework.TypeInt,
								},
								"bundle_integrity_sha256": {
									Type: framework.TypeString,
								},
								"expires_at_unix": {
									Type: framework.TypeInt,
								},
							},
						}},
					},
				},
			},

			HelpSynopsis:    "Export DR pre-seed bundle",
			HelpDescription: "Cuts a primary checkpoint and returns a JSON DR pre-seed bundle containing replicated below-barrier storage entries plus a relationship-bound manifest. The bundle is intended for operator transfer into a disabled secondary.",
		},

		// --- Primary Segmented Pre-Seed Export Plan ---
		{
			Pattern: "replication/dr/primary/preseed/export-plan$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary-preseed",
				OperationVerb:   "export-plan",
			},

			Fields: map[string]*framework.FieldSchema{
				"relationship_id": {
					Type:        framework.TypeString,
					Description: "The pending, registered, or active DR relationship ID this segmented seed bundle is bound to.",
					Required:    true,
				},
				"ttl_seconds": {
					Type:        framework.TypeInt,
					Default:     int((24 * time.Hour).Seconds()),
					Description: "Manifest validity window in seconds. Set to 0 for no expiry.",
				},
				"segment_max_bytes": {
					Type:        framework.TypeInt,
					Default:     drPreSeedDefaultSegmentBytes,
					Description: "Maximum canonical segment payload size in bytes.",
				},
				"async": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Return a pollable pre-seed export plan job instead of waiting for checkpoint artifact planning to finish.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryPreSeedExportPlan,
					Summary:                   "Create a segmented DR pre-seed export plan.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Create segmented DR pre-seed export plan",
			HelpDescription: "Cuts a primary checkpoint and returns a relationship-bound manifest with deterministic segment descriptors for chunked operator transfer.",
		},

		// --- Primary Segmented Pre-Seed Export Plan Status ---
		{
			Pattern: "replication/dr/primary/preseed/export-plan-status$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary-preseed",
				OperationVerb:   "export-plan-status",
			},

			Fields: map[string]*framework.FieldSchema{
				"plan_id": {
					Type:        framework.TypeString,
					Description: "The async pre-seed export plan ID returned by export-plan async=true.",
					Required:    true,
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryPreSeedExportPlanStatus,
					Summary:                   "Read a segmented DR pre-seed export plan job.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Read segmented DR pre-seed export plan status",
			HelpDescription: "Returns the status of an async segmented DR pre-seed export plan and the manifest once planning has completed.",
		},

		// --- Primary Segmented Pre-Seed Export Segment ---
		{
			Pattern: "replication/dr/primary/preseed/export-segment$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-primary-preseed",
				OperationVerb:   "export-segment",
			},

			Fields: map[string]*framework.FieldSchema{
				"manifest": {
					Type:        framework.TypeString,
					Description: "The JSON segmented DR pre-seed manifest returned by export-plan.",
					Required:    true,
				},
				"segment_index": {
					Type:        framework.TypeInt,
					Description: "Zero-based segment index to export.",
					Required:    true,
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRPrimaryPreSeedExportSegment,
					Summary:                   "Export one DR pre-seed segment.",
					ForwardPerformanceStandby: true,
					Responses: map[int][]framework.Response{
						http.StatusOK: {{
							Description: "OK",
							Fields: map[string]*framework.FieldSchema{
								"segment": {
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

			HelpSynopsis:    "Export DR pre-seed segment",
			HelpDescription: "Exports one checkpoint-bound DR pre-seed segment identified by a segmented pre-seed manifest and segment index.",
		},

		// --- Secondary Pre-Seed Accept ---
		{
			Pattern: "replication/dr/secondary/preseed/accept$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary-preseed",
				OperationVerb:   "accept",
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
				"manifest": {
					Type:        framework.TypeString,
					Description: "The JSON DR pre-seed manifest generated by the primary.",
					Required:    true,
				},
				"confirm_storage_restored": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that the seed bundle has already been restored into this disabled secondary cluster.",
				},
				"confirm_local_only_scrubbed": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that local-only paths from the seed manifest have been scrubbed or preserved with local secondary values.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryPreSeedAccept,
					Summary:                   "Accept a restored DR pre-seed manifest before enabling secondary mode.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Accept DR pre-seed manifest",
			HelpDescription: "Validates and records a restored DR pre-seed manifest. The accepted checkpoint baseline is applied when the same activation token is used to enable secondary mode.",
		},

		// --- Secondary Pre-Seed Import ---
		{
			Pattern: "replication/dr/secondary/preseed/import$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary-preseed",
				OperationVerb:   "import",
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
				"bundle": {
					Type:        framework.TypeString,
					Description: "The JSON DR pre-seed bundle generated by the primary.",
					Required:    true,
					DisplayAttrs: &framework.DisplayAttributes{
						Sensitive: true,
					},
				},
				"confirm_replace_replicated_storage": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that importing this bundle may replace the disabled secondary's replicated storage plane while preserving cluster-local paths.",
				},
				"enable_secondary": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Enable DR secondary mode in the same authenticated request after accepting the imported pre-seed bundle.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryPreSeedImport,
					Summary:                   "Import a DR pre-seed bundle before enabling secondary mode.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Import DR pre-seed bundle",
			HelpDescription: "Validates a DR pre-seed bundle, replaces the disabled secondary's replicated storage plane with the bundle entries, preserves cluster-local paths, and records the checkpoint baseline for secondary enable.",
		},

		// --- Secondary Segmented Pre-Seed Import Begin ---
		{
			Pattern: "replication/dr/secondary/preseed/import-begin$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary-preseed",
				OperationVerb:   "import-begin",
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
				"manifest": {
					Type:        framework.TypeString,
					Description: "The JSON segmented DR pre-seed manifest generated by the primary.",
					Required:    true,
				},
				"confirm_replace_replicated_storage": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that completing this import may replace the disabled secondary's replicated storage plane while preserving cluster-local paths.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryPreSeedImportBegin,
					Summary:                   "Start a segmented DR pre-seed import.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Begin segmented DR pre-seed import",
			HelpDescription: "Validates a segmented DR pre-seed manifest and records durable import staging before any replicated storage replacement happens.",
		},

		// --- Secondary Segmented Pre-Seed Import Segment ---
		{
			Pattern: "replication/dr/secondary/preseed/import-segment$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary-preseed",
				OperationVerb:   "import-segment",
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
				"segment": {
					Type:        framework.TypeString,
					Description: "The JSON DR pre-seed segment generated by the primary.",
					Required:    true,
					DisplayAttrs: &framework.DisplayAttributes{
						Sensitive: true,
					},
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryPreSeedImportSegment,
					Summary:                   "Stage one segmented DR pre-seed segment.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Import DR pre-seed segment",
			HelpDescription: "Validates and durably stages one segmented DR pre-seed segment. Re-sending the same segment is idempotent.",
		},

		// --- Secondary Segmented Pre-Seed Import Complete ---
		{
			Pattern: "replication/dr/secondary/preseed/import-complete$",

			DisplayAttrs: &framework.DisplayAttributes{
				OperationPrefix: "replication-dr-secondary-preseed",
				OperationVerb:   "import-complete",
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
				"confirm_replace_replicated_storage": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Operator confirmation that completing this import may replace the disabled secondary's replicated storage plane while preserving cluster-local paths.",
				},
				"enable_secondary": {
					Type:        framework.TypeBool,
					Default:     false,
					Description: "Enable DR secondary mode in the same authenticated request after accepting the staged pre-seed segments.",
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback:                  b.handleDRSecondaryPreSeedImportComplete,
					Summary:                   "Complete a segmented DR pre-seed import.",
					ForwardPerformanceStandby: true,
				},
			},

			HelpSynopsis:    "Complete segmented DR pre-seed import",
			HelpDescription: "Assembles staged DR pre-seed segments, validates the full bundle, replaces replicated storage, and records the checkpoint baseline for secondary enable.",
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
				"reconcile_max_range_drilldown_rpcs": {
					Type:        framework.TypeInt,
					Description: "Secondary max ExchangeRangeDigests RPCs per mismatched top-level range before falling back to coarser proof-backed fetch spans.",
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
				"flat_accumulator_snapshot_min_entries": {
					Type:        framework.TypeInt,
					Description: "Minimum applied entry delta before persisting a full flat-accumulator snapshot.",
				},
				"flat_accumulator_snapshot_min_interval_milliseconds": {
					Type:        framework.TypeInt,
					Description: "Minimum elapsed time before persisting a full flat-accumulator snapshot.",
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
		data["reconcile_budget_exhausted_total"] = status.ReconcileBudgetExhaustedTotal
		data["reconcile_budget_exhausted_phase_last"] = status.ReconcileBudgetExhaustedPhaseLast
		data["reconcile_budget_exhausted_reason_last"] = status.ReconcileBudgetExhaustedReasonLast
		data["reconcile_budget_exhausted_rpc_bytes_last"] = status.ReconcileBudgetExhaustedRPCBytesLast
		data["reconcile_budget_exhausted_max_rpc_bytes_last"] = status.ReconcileBudgetExhaustedMaxRPCBytesLast
		data["reconcile_budget_exhausted_rpc_calls_last"] = status.ReconcileBudgetExhaustedRPCCallsLast
		data["reconcile_budget_exhausted_entries_last"] = status.ReconcileBudgetExhaustedEntriesLast
		data["reconcile_budget_exhausted_ranges_handled_last"] = status.ReconcileBudgetExhaustedRangesHandledLast
		data["reconcile_budget_exhausted_ranges_split_last"] = status.ReconcileBudgetExhaustedRangesSplitLast
		data["reconcile_budget_exhausted_retries_last"] = status.ReconcileBudgetExhaustedRetriesLast
		data["flat_accumulator_fast_path_total"] = status.FlatAccumulatorFastPathTotal
		data["flat_accumulator_empty_repair_total"] = status.FlatAccumulatorEmptyRepairTotal
		data["flat_accumulator_empty_repair_ranges_total"] = status.FlatAccumulatorEmptyRepairRanges
		data["flat_accumulator_indexed_repair_total"] = status.FlatAccumulatorIndexedRepairTotal
		data["flat_accumulator_indexed_repair_ranges_total"] = status.FlatAccumulatorIndexedRepairRanges
		data["flat_accumulator_indexed_repair_full_bucket_fallback_total"] = status.FlatAccumulatorIndexedRepairFullBucketFallbacks
		data["flat_accumulator_indexed_repair_full_bucket_ranges_total"] = status.FlatAccumulatorIndexedRepairFullBucketRanges
		data["flat_accumulator_indexed_repair_proof_mismatches_total"] = status.FlatAccumulatorIndexedRepairProofMismatches
		data["flat_accumulator_indexed_repair_proof_mismatch_range_last"] = status.FlatAccumulatorIndexedRepairProofMismatchRange
		data["flat_accumulator_indexed_repair_proof_mismatch_local_count_last"] = status.FlatAccumulatorIndexedRepairProofMismatchLocal
		data["flat_accumulator_indexed_repair_proof_mismatch_remote_count_last"] = status.FlatAccumulatorIndexedRepairProofMismatchRemote
		data["flat_accumulator_indexed_repair_proof_mismatch_checksum_last"] = status.FlatAccumulatorIndexedRepairProofMismatchChecksum
		data["local_kid_index_bucket_loads_total"] = status.LocalKIDIndexBucketLoadsTotal
		data["local_kid_index_entries_loaded_total"] = status.LocalKIDIndexEntriesLoadedTotal
		data["local_kid_index_load_failures_total"] = status.LocalKIDIndexLoadFailuresTotal
		data["local_kid_index_resets_total"] = status.LocalKIDIndexResetsTotal
		data["local_kid_index_updates_total"] = status.LocalKIDIndexUpdatesTotal
		data["local_kid_index_fallback_scans_total"] = status.LocalKIDIndexFallbackScansTotal
		data["local_kid_index_fallback_scan_reason_last"] = status.LocalKIDIndexFallbackScanReasonLast
		data["local_kid_index_invalidations_total"] = status.LocalKIDIndexInvalidationsTotal
		data["local_kid_index_invalidation_reason_last"] = status.LocalKIDIndexInvalidationReasonLast
		data["range_drilldown_rpc_total"] = status.RangeDrillDownRPCTotal
		data["range_drilldown_coarse_fetch_total"] = status.RangeDrillDownCoarseFetchTotal
		data["range_drilldown_coarse_fetch_ranges_total"] = status.RangeDrillDownCoarseFetchRangesTotal
		data["stream_txn_coalesced_entries_total"] = status.StreamTxnCoalescedEntriesTotal
		data["stream_txn_batches_total"] = status.StreamTxnBatchesTotal
		data["stream_txn_entries_total"] = status.StreamTxnEntriesTotal
		data["stream_txn_physical_entries_total"] = status.StreamTxnPhysicalEntriesTotal
		data["stream_txn_average_entries"] = status.StreamTxnAverageEntries
		data["stream_txn_max_entries"] = status.StreamTxnMaxEntries
		data["stream_txn_average_physical_entries"] = status.StreamTxnAveragePhysicalEntries
		data["stream_txn_max_physical_entries"] = status.StreamTxnMaxPhysicalEntries
		data["stream_txn_apply_milliseconds_total"] = status.StreamTxnApplyMillisecondsTotal
		data["stream_txn_apply_milliseconds_average"] = status.StreamTxnApplyMillisecondsAverage
		data["stream_txn_apply_milliseconds_max"] = status.StreamTxnApplyMillisecondsMax
		data["stream_txn_commit_milliseconds_total"] = status.StreamTxnCommitMillisecondsTotal
		data["stream_txn_commit_milliseconds_average"] = status.StreamTxnCommitMillisecondsAverage
		data["stream_txn_commit_milliseconds_max"] = status.StreamTxnCommitMillisecondsMax
		data["stream_batch_flush_max_entries_total"] = status.StreamBatchFlushMaxEntriesTotal
		data["stream_batch_flush_max_bytes_total"] = status.StreamBatchFlushMaxBytesTotal
		data["stream_batch_flush_max_wait_total"] = status.StreamBatchFlushMaxWaitTotal
		data["stream_batch_flush_shutdown_total"] = status.StreamBatchFlushShutdownTotal
		data["stream_batch_adaptive_adjustments_total"] = status.StreamBatchAdaptiveAdjustmentsTotal
		data["stream_batch_adaptive_level"] = status.StreamBatchAdaptiveLevel
		data["stream_batch_effective_max_entries"] = status.StreamBatchEffectiveMaxEntries
		data["stream_batch_effective_max_wait_milliseconds"] = status.StreamBatchEffectiveMaxWaitMillis
		data["flat_accumulator_cursor_writes_total"] = status.FlatAccumulatorCursorWritesTotal
		data["flat_accumulator_cursor_index"] = status.FlatAccumulatorCursorIndex
		data["flat_accumulator_snapshot_persists_total"] = status.FlatAccumulatorSnapshotPersistsTotal
		data["flat_accumulator_snapshot_index"] = status.FlatAccumulatorSnapshotIndex
		data["flat_accumulator_snapshot_bytes_total"] = status.FlatAccumulatorSnapshotBytesTotal
		data["flat_accumulator_snapshot_bytes_average"] = status.FlatAccumulatorSnapshotBytesAverage
		data["flat_accumulator_snapshot_bytes_last"] = status.FlatAccumulatorSnapshotBytesLast
		data["flat_accumulator_snapshot_persist_milliseconds_total"] = status.FlatAccumulatorSnapshotPersistMsTotal
		data["flat_accumulator_snapshot_persist_milliseconds_average"] = status.FlatAccumulatorSnapshotPersistMsAverage
		data["flat_accumulator_snapshot_persist_milliseconds_max"] = status.FlatAccumulatorSnapshotPersistMsMax
		data["flat_accumulator_snapshot_skipped_total"] = status.FlatAccumulatorSnapshotSkippedTotal
		data["flat_accumulator_delta_batches_total"] = status.FlatAccumulatorDeltaBatchesTotal
		data["flat_accumulator_delta_entries_total"] = status.FlatAccumulatorDeltaEntriesTotal
		data["flat_accumulator_delta_oldest_index"] = status.FlatAccumulatorDeltaOldestIndex
		data["flat_accumulator_delta_newest_index"] = status.FlatAccumulatorDeltaNewestIndex
		data["flat_accumulator_delta_replay_total"] = status.FlatAccumulatorDeltaReplayTotal
		data["flat_accumulator_delta_replay_batches_total"] = status.FlatAccumulatorDeltaReplayBatchesTotal
		data["flat_accumulator_delta_replay_entries_total"] = status.FlatAccumulatorDeltaReplayEntriesTotal
		data["flat_accumulator_delta_replay_failures_total"] = status.FlatAccumulatorDeltaReplayFailuresTotal
		data["flat_accumulator_snapshot_min_entries"] = status.FlatAccumulatorSnapshotMinEntries
		data["flat_accumulator_snapshot_min_interval_milliseconds"] = status.FlatAccumulatorSnapshotMinIntervalMilliseconds
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
		data["reconcile_max_range_drilldown_rpcs"] = status.ReconcileMaxRangeDrillDownRPCs
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
		rangeChecksumRequests, rangeChecksumRejections, rangeDigestRequests, rangeDigestRejections, fetchRequests, fetchRequestRejections, fetchResponseBudgetRejections := primary.checkpointRPCPressureStats()
		data["range_checksum_requests_total"] = rangeChecksumRequests
		data["range_checksum_rejections_total"] = rangeChecksumRejections
		data["range_digest_requests_total"] = rangeDigestRequests
		data["range_digest_rejections_total"] = rangeDigestRejections
		data["fetch_requests_total"] = fetchRequests
		data["fetch_request_rejections_total"] = fetchRequestRejections
		data["fetch_response_budget_rejections_total"] = fetchResponseBudgetRejections
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

func (b *SystemBackend) handleDRSecondaryVerifyCheckpoint(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil || !secondaryLocalControlTokenMatches(mgr.Config(), req.ClientToken) {
		return nil, logical.ErrPermissionDenied
	}
	sec := mgr.Secondary()
	if sec == nil {
		return logical.ErrorResponse("cluster is not running as a DR secondary"), nil
	}

	result, err := sec.VerifyCheckpoint(ctx)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	data := map[string]interface{}{
		"pass":               result.Pass,
		"reason":             result.Reason,
		"state":              result.State,
		"relationship_id":    result.RelationshipID,
		"checkpoint_id":      result.CheckpointID,
		"checkpoint_index":   result.CheckpointIndex,
		"accumulator_index":  result.AccumulatorIndex,
		"range_count":        result.RangeCount,
		"matched_ranges":     result.MatchedRanges,
		"mismatched_ranges":  result.MismatchedRanges,
		"missing_ranges":     result.MissingRanges,
		"physical_scan_used": result.PhysicalScanUsed,
		"optimizer_reseeded": result.OptimizerReseeded,
	}

	if len(result.Mismatches) > 0 {
		mismatches := make([]map[string]interface{}, 0, len(result.Mismatches))
		for _, mismatch := range result.Mismatches {
			mismatches = append(mismatches, map[string]interface{}{
				"range_id":          mismatch.RangeID,
				"local_count":       mismatch.LocalCount,
				"remote_count":      mismatch.RemoteCount,
				"local_checksum":    mismatch.LocalChecksum,
				"remote_checksum":   mismatch.RemoteChecksum,
				"missing_remote":    mismatch.MissingRemote,
				"checksum_mismatch": mismatch.ChecksumMismatch,
				"count_mismatch":    mismatch.CountMismatch,
			})
		}
		data["mismatches"] = mismatches
	}

	return &logical.Response{Data: data}, nil
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

func (b *SystemBackend) handleDRPrimaryPreSeedManifest(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	relationshipIDRaw, ok := d.GetOk("relationship_id")
	if !ok {
		return logical.ErrorResponse("relationship_id is required"), nil
	}
	relationshipID, _ := relationshipIDRaw.(string)
	relationshipID = strings.TrimSpace(relationshipID)
	if relationshipID == "" {
		return logical.ErrorResponse("relationship_id is required"), nil
	}

	bundleHashRaw, ok := d.GetOk("bundle_integrity_sha256")
	if !ok {
		return logical.ErrorResponse("bundle_integrity_sha256 is required"), nil
	}
	bundleHashText, _ := bundleHashRaw.(string)
	bundleHash, err := hex.DecodeString(strings.TrimSpace(bundleHashText))
	if err != nil || len(bundleHash) != sha256.Size {
		return logical.ErrorResponse("bundle_integrity_sha256 must be a hex-encoded SHA-256 digest"), nil
	}

	ttlSeconds := d.Get("ttl_seconds").(int)
	if ttlSeconds < 0 {
		return logical.ErrorResponse("ttl_seconds must be >= 0"), nil
	}
	manifest, err := mgr.GeneratePreSeedManifest(ctx, relationshipID, bundleHash, time.Duration(ttlSeconds)*time.Second)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DR pre-seed manifest: %w", err)
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"manifest":         string(manifestJSON),
			"relationship_id":  manifest.RelationshipID,
			"checkpoint_id":    manifest.CheckpointID,
			"checkpoint_index": manifest.CheckpointIndex,
			"expires_at_unix":  manifest.ExpiresAtUnix,
		},
	}, nil
}

func (b *SystemBackend) handleDRPrimaryPreSeedExport(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	relationshipIDRaw, ok := d.GetOk("relationship_id")
	if !ok {
		return logical.ErrorResponse("relationship_id is required"), nil
	}
	relationshipID, _ := relationshipIDRaw.(string)
	relationshipID = strings.TrimSpace(relationshipID)
	if relationshipID == "" {
		return logical.ErrorResponse("relationship_id is required"), nil
	}

	ttlSeconds := d.Get("ttl_seconds").(int)
	if ttlSeconds < 0 {
		return logical.ErrorResponse("ttl_seconds must be >= 0"), nil
	}
	bundle, err := mgr.GeneratePreSeedBundle(ctx, relationshipID, time.Duration(ttlSeconds)*time.Second)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DR pre-seed bundle: %w", err)
	}
	manifestJSON, err := json.Marshal(bundle.Manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DR pre-seed manifest: %w", err)
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"bundle":                  string(bundleJSON),
			"manifest":                string(manifestJSON),
			"relationship_id":         bundle.Manifest.RelationshipID,
			"checkpoint_id":           bundle.Manifest.CheckpointID,
			"checkpoint_index":        bundle.Manifest.CheckpointIndex,
			"entry_count":             bundle.EntryCount,
			"bundle_integrity_sha256": hex.EncodeToString(bundle.Manifest.BundleIntegritySHA256),
			"expires_at_unix":         bundle.Manifest.ExpiresAtUnix,
		},
	}, nil
}

func (b *SystemBackend) handleDRPrimaryPreSeedExportPlan(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	relationshipIDRaw, ok := d.GetOk("relationship_id")
	if !ok {
		return logical.ErrorResponse("relationship_id is required"), nil
	}
	relationshipID, _ := relationshipIDRaw.(string)
	relationshipID = strings.TrimSpace(relationshipID)
	if relationshipID == "" {
		return logical.ErrorResponse("relationship_id is required"), nil
	}

	ttlSeconds := d.Get("ttl_seconds").(int)
	if ttlSeconds < 0 {
		return logical.ErrorResponse("ttl_seconds must be >= 0"), nil
	}
	segmentMaxBytes := d.Get("segment_max_bytes").(int)
	if segmentMaxBytes <= 0 {
		return logical.ErrorResponse("segment_max_bytes must be > 0"), nil
	}
	if async := d.Get("async").(bool); async {
		status, err := mgr.StartSegmentedPreSeedManifestPlan(ctx, relationshipID, segmentMaxBytes, time.Duration(ttlSeconds)*time.Second)
		if err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
		return drPreSeedExportPlanStatusResponse(status)
	}
	manifest, entryCount, err := mgr.GenerateSegmentedPreSeedManifest(ctx, relationshipID, segmentMaxBytes, time.Duration(ttlSeconds)*time.Second)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return drPreSeedExportPlanStatusResponse(&DRPreSeedExportPlanStatus{
		State:      drPreSeedExportPlanStateComplete,
		Manifest:   manifest,
		EntryCount: entryCount,
	})
}

func (b *SystemBackend) handleDRPrimaryPreSeedExportPlanStatus(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	planIDRaw, ok := d.GetOk("plan_id")
	if !ok {
		return logical.ErrorResponse("plan_id is required"), nil
	}
	planID, _ := planIDRaw.(string)
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return logical.ErrorResponse("plan_id is required"), nil
	}

	status, err := mgr.GetSegmentedPreSeedManifestPlan(planID)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return drPreSeedExportPlanStatusResponse(status)
}

func drPreSeedExportPlanStatusResponse(status *DRPreSeedExportPlanStatus) (*logical.Response, error) {
	if status == nil {
		return nil, fmt.Errorf("DR pre-seed export plan status is nil")
	}
	data := map[string]interface{}{
		"plan_id":           status.PlanID,
		"state":             status.State,
		"entry_count":       status.EntryCount,
		"started_at_unix":   status.StartedAtUnix,
		"completed_at_unix": status.CompletedAtUnix,
	}
	if status.Error != "" {
		data["error"] = status.Error
	}
	if status.Manifest != nil {
		manifestJSON, err := json.Marshal(status.Manifest)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal DR pre-seed manifest: %w", err)
		}
		data["manifest"] = string(manifestJSON)
		data["relationship_id"] = status.Manifest.RelationshipID
		data["checkpoint_id"] = status.Manifest.CheckpointID
		data["checkpoint_index"] = status.Manifest.CheckpointIndex
		data["segment_count"] = len(status.Manifest.BundleSegments)
		data["bundle_integrity_sha256"] = hex.EncodeToString(status.Manifest.BundleIntegritySHA256)
		data["expires_at_unix"] = status.Manifest.ExpiresAtUnix
	}
	return &logical.Response{Data: data}, nil
}

func (b *SystemBackend) handleDRPrimaryPreSeedExportSegment(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	mgr := b.Core.drManager
	if mgr == nil {
		return logical.ErrorResponse("DR replication not initialized"), nil
	}

	manifestRaw, ok := d.GetOk("manifest")
	if !ok {
		return logical.ErrorResponse("manifest is required"), nil
	}
	manifestStr, ok := manifestRaw.(string)
	if !ok || manifestStr == "" {
		return logical.ErrorResponse("manifest must be a non-empty string"), nil
	}
	var manifest DRPreSeedManifest
	if err := json.Unmarshal([]byte(manifestStr), &manifest); err != nil {
		return logical.ErrorResponse("invalid pre-seed manifest: %s", err.Error()), nil
	}
	segmentIndex := d.Get("segment_index").(int)
	if segmentIndex < 0 {
		return logical.ErrorResponse("segment_index must be >= 0"), nil
	}
	segment, err := mgr.GeneratePreSeedSegment(ctx, &manifest, segmentIndex)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	segmentJSON, err := json.Marshal(segment)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DR pre-seed segment: %w", err)
	}
	descriptor := manifest.BundleSegments[segmentIndex]
	return &logical.Response{
		Data: map[string]interface{}{
			"segment":       string(segmentJSON),
			"segment_index": segment.SegmentIndex,
			"entry_count":   segment.EntryCount,
			"byte_count":    descriptor.ByteCount,
			"sha256":        hex.EncodeToString(descriptor.SHA256),
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryPreSeedAccept(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
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

	manifestRaw, ok := d.GetOk("manifest")
	if !ok {
		return logical.ErrorResponse("manifest is required"), nil
	}
	manifestStr, ok := manifestRaw.(string)
	if !ok || manifestStr == "" {
		return logical.ErrorResponse("manifest must be a non-empty string"), nil
	}
	var manifest DRPreSeedManifest
	if err := json.Unmarshal([]byte(manifestStr), &manifest); err != nil {
		return logical.ErrorResponse("invalid pre-seed manifest: %s", err.Error()), nil
	}

	confirmStorageRestored := d.Get("confirm_storage_restored").(bool)
	confirmLocalOnlyScrubbed := d.Get("confirm_local_only_scrubbed").(bool)
	if err := mgr.AcceptPreSeedManifest(ctx, &manifest, &token, time.Now().UTC(), confirmStorageRestored, confirmLocalOnlyScrubbed); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":          "DR pre-seed manifest accepted",
			"relationship_id":  manifest.RelationshipID,
			"checkpoint_id":    manifest.CheckpointID,
			"checkpoint_index": manifest.CheckpointIndex,
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryPreSeedImport(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
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

	bundleRaw, ok := d.GetOk("bundle")
	if !ok {
		return logical.ErrorResponse("bundle is required"), nil
	}
	bundleStr, ok := bundleRaw.(string)
	if !ok || bundleStr == "" {
		return logical.ErrorResponse("bundle must be a non-empty string"), nil
	}
	if len(bundleStr) > drPreSeedBundleMaxBytes {
		return logical.ErrorResponse("pre-seed bundle exceeds maximum size %d", drPreSeedBundleMaxBytes), nil
	}
	var bundle DRPreSeedBundle
	if err := json.Unmarshal([]byte(bundleStr), &bundle); err != nil {
		return logical.ErrorResponse("invalid pre-seed bundle: %s", err.Error()), nil
	}

	confirmReplace := d.Get("confirm_replace_replicated_storage").(bool)
	if err := mgr.ImportPreSeedBundle(ctx, &bundle, &token, time.Now().UTC(), confirmReplace); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	enabled := false
	if d.Get("enable_secondary").(bool) {
		if err := mgr.EnableSecondary(ctx, &token, req.ClientToken); err != nil {
			return logical.ErrorResponse("pre-seed bundle imported but secondary enable failed: %s", err.Error()), nil
		}
		enabled = true
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":          "DR pre-seed bundle imported",
			"relationship_id":  bundle.Manifest.RelationshipID,
			"checkpoint_id":    bundle.Manifest.CheckpointID,
			"checkpoint_index": bundle.Manifest.CheckpointIndex,
			"entry_count":      bundle.EntryCount,
			"enabled":          enabled,
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryPreSeedImportBegin(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
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

	manifestRaw, ok := d.GetOk("manifest")
	if !ok {
		return logical.ErrorResponse("manifest is required"), nil
	}
	manifestStr, ok := manifestRaw.(string)
	if !ok || manifestStr == "" {
		return logical.ErrorResponse("manifest must be a non-empty string"), nil
	}
	var manifest DRPreSeedManifest
	if err := json.Unmarshal([]byte(manifestStr), &manifest); err != nil {
		return logical.ErrorResponse("invalid pre-seed manifest: %s", err.Error()), nil
	}

	confirmReplace := d.Get("confirm_replace_replicated_storage").(bool)
	if err := mgr.BeginPreSeedSegmentImport(ctx, &manifest, &token, time.Now().UTC(), confirmReplace); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":          "DR pre-seed segment import started",
			"relationship_id":  manifest.RelationshipID,
			"checkpoint_id":    manifest.CheckpointID,
			"checkpoint_index": manifest.CheckpointIndex,
			"segment_count":    len(manifest.BundleSegments),
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryPreSeedImportSegment(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
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

	segmentRaw, ok := d.GetOk("segment")
	if !ok {
		return logical.ErrorResponse("segment is required"), nil
	}
	segmentStr, ok := segmentRaw.(string)
	if !ok || segmentStr == "" {
		return logical.ErrorResponse("segment must be a non-empty string"), nil
	}
	if len(segmentStr) > drPreSeedBundleMaxBytes {
		return logical.ErrorResponse("pre-seed segment exceeds maximum size %d", drPreSeedBundleMaxBytes), nil
	}
	var segment DRPreSeedSegment
	if err := json.Unmarshal([]byte(segmentStr), &segment); err != nil {
		return logical.ErrorResponse("invalid pre-seed segment: %s", err.Error()), nil
	}
	received, expected, err := mgr.ImportPreSeedSegment(ctx, &segment, &token, time.Now().UTC())
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":           "DR pre-seed segment staged",
			"relationship_id":   segment.Manifest.RelationshipID,
			"checkpoint_id":     segment.Manifest.CheckpointID,
			"checkpoint_index":  segment.Manifest.CheckpointIndex,
			"segment_index":     segment.SegmentIndex,
			"received_segments": received,
			"expected_segments": expected,
		},
	}, nil
}

func (b *SystemBackend) handleDRSecondaryPreSeedImportComplete(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
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

	confirmReplace := d.Get("confirm_replace_replicated_storage").(bool)
	if err := mgr.CompletePreSeedSegmentImport(ctx, &token, time.Now().UTC(), confirmReplace); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	enabled := false
	if d.Get("enable_secondary").(bool) {
		if err := mgr.EnableSecondary(ctx, &token, req.ClientToken); err != nil {
			return logical.ErrorResponse("pre-seed segment import completed but secondary enable failed: %s", err.Error()), nil
		}
		enabled = true
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"message":         "DR pre-seed segment import completed",
			"relationship_id": token.RelationshipID,
			"enabled":         enabled,
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

	if err := mgr.EnableSecondary(ctx, &token, req.ClientToken); err != nil {
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
			"checkpoint_ttl_seconds":                              cfg.CheckpointTTLSeconds,
			"checkpoint_global_budget_bytes":                      cfg.CheckpointGlobalBudgetBytes,
			"checkpoint_per_relationship_budget_bytes":            cfg.CheckpointPerRelBudgetBytes,
			"stream_buffer_max_entries":                           cfg.StreamBufferMaxEntries,
			"stream_buffer_max_bytes":                             cfg.StreamBufferMaxBytes,
			"reconcile_max_rpc_bytes":                             cfg.ReconcileMaxRPCBytes,
			"reconcile_max_wall_time_seconds":                     cfg.ReconcileMaxWallTimeSeconds,
			"reconcile_max_inflight_tasks":                        cfg.ReconcileMaxInflightTasks,
			"reconcile_max_range_drilldown_rpcs":                  cfg.ReconcileMaxRangeDrillDownRPCs,
			"stream_batch_max_entries":                            cfg.StreamBatchMaxEntries,
			"stream_batch_max_bytes":                              cfg.StreamBatchMaxBytes,
			"stream_batch_max_wait_milliseconds":                  cfg.StreamBatchMaxWaitMillis,
			"flat_accumulator_snapshot_min_entries":               cfg.FlatAccumulatorSnapshotMinEntries,
			"flat_accumulator_snapshot_min_interval_milliseconds": cfg.FlatAccumulatorSnapshotMinIntervalMillis,
			"stream_journal_enabled":                              cfg.StreamJournalEnabled,
			"stream_journal_max_bytes":                            cfg.StreamJournalMaxBytes,
			"stream_journal_segment_bytes":                        cfg.StreamJournalSegmentBytes,
			"stream_journal_retention_seconds":                    cfg.StreamJournalRetentionSecs,
			"reconcile_apply_workers":                             cfg.ReconcileApplyWorkers,
			"reconcile_put_batch_max_entries":                     cfg.ReconcilePutBatchMaxEntries,
			"reconcile_put_batch_max_bytes":                       cfg.ReconcilePutBatchMaxBytes,
			"convergence_min_rate_ratio":                          cfg.ConvergenceMinRateRatio,
			"convergence_stall_seconds":                           cfg.ConvergenceStallSeconds,
			"fallback_enabled":                                    cfg.FallbackEnabled,
			"fallback_stall_seconds":                              cfg.FallbackStallSeconds,
			"fallback_failure_threshold":                          cfg.FallbackFailureThreshold,
			"fallback_min_lag_entries":                            cfg.FallbackMinLagEntries,
			"fallback_cooldown_seconds":                           cfg.FallbackCooldownSeconds,
			"fallback_max_per_hour":                               cfg.FallbackMaxPerHour,
			"checkpoint_artifact_enabled":                         cfg.CheckpointArtifactEnabled,
			"checkpoint_artifact_global_budget_bytes":             cfg.CheckpointArtifactGlobalBudgetBytes,
			"checkpoint_artifact_per_relationship_budget_bytes":   cfg.CheckpointArtifactPerRelBudgetBytes,
			"checkpoint_artifact_ttl_seconds":                     cfg.CheckpointArtifactTTLSeconds,
			"checkpoint_artifact_segment_bytes":                   cfg.CheckpointArtifactSegmentBytes,
			"dr_backpressure_enabled":                             cfg.DRBackpressureEnabled,
			"dr_backpressure_degraded_ratio":                      cfg.DRBackpressureDegradedRatio,
			"dr_backpressure_critical_ratio":                      cfg.DRBackpressureCriticalRatio,
			"dr_backpressure_min_lag_entries":                     cfg.DRBackpressureMinLagEntries,
			"dr_backpressure_horizon_seconds":                     cfg.DRBackpressureHorizonSeconds,
			"dr_backpressure_degraded_min_qps":                    cfg.DRBackpressureDegradedMinQPS,
			"dr_backpressure_critical_min_qps":                    cfg.DRBackpressureCriticalMinQPS,
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
			func() error { return setInt("reconcile_max_range_drilldown_rpcs", &cfg.ReconcileMaxRangeDrillDownRPCs) },
			func() error { return setInt("stream_batch_max_entries", &cfg.StreamBatchMaxEntries) },
			func() error { return setInt("stream_batch_max_bytes", &cfg.StreamBatchMaxBytes) },
			func() error { return setInt64("stream_batch_max_wait_milliseconds", &cfg.StreamBatchMaxWaitMillis) },
			func() error {
				return setUint64("flat_accumulator_snapshot_min_entries", &cfg.FlatAccumulatorSnapshotMinEntries)
			},
			func() error {
				return setInt64("flat_accumulator_snapshot_min_interval_milliseconds", &cfg.FlatAccumulatorSnapshotMinIntervalMillis)
			},
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
