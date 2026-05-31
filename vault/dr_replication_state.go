// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-uuid"
	raft "github.com/openbao/openbao/physical/raft"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	// drConfigPath is the storage path for DR configuration.
	drConfigPath = "core/dr-replication/config"

	// drRelationshipsPath is the prefix for DR relationship metadata.
	drRelationshipsPath = "core/dr-replication/relationships/"

	// drReplSaltLen is the length of the replication HMAC salt in bytes.
	drReplSaltLen = 32

	// Bootstrap token policy defaults.
	drBootstrapTokenTTL            = 15 * time.Minute
	drBootstrapMaxFailedAttempts   = 5
	drBootstrapLockoutDuration     = 30 * time.Minute
	drBootstrapMaxCertDERBytes     = 64 * 1024
	drActivationTokenMaxBytes      = 256 * 1024
	drCredentialRotationMaxSkew    = 5 * time.Minute
	drCredentialRotationPendingTTL = 30 * time.Minute
	drCredentialRotationMaxSigLen  = 8 * 1024
	drRelationshipCleanupInterval  = 24 * time.Hour

	// Persist relationship heartbeat writes at most once per interval.
	drLastSeenPersistInterval = 30 * time.Second
)

const (
	drMaxTuningDurationSeconds = int64(1<<63-1) / int64(time.Second)
	drMaxTuningDurationMillis  = int64(1<<63-1) / int64(time.Millisecond)
)

// DRMode represents the DR replication role of this cluster.
type DRMode string

const (
	DRModeDisabled  DRMode = "disabled"
	DRModePrimary   DRMode = "primary"
	DRModeSecondary DRMode = "secondary"
)

// DRPromotionClass records whether a DR promotion was fully caught up or
// required an explicit data-loss acknowledgement.
type DRPromotionClass string

const (
	DRPromotionClean  DRPromotionClass = "clean"
	DRPromotionForced DRPromotionClass = "forced"
)

// DRPromotionRecord is persisted after a secondary is promoted so stale
// upstream relationship state cannot be mistaken for a resumable stream.
type DRPromotionRecord struct {
	PromotionID                 string           `json:"promotion_id"`
	PromotedAt                  int64            `json:"promoted_at"`
	OldPrimaryClusterID         string           `json:"old_primary_cluster_id,omitempty"`
	OldRelationshipID           string           `json:"old_relationship_id,omitempty"`
	OldSecondaryCertFingerprint string           `json:"old_secondary_cert_fingerprint,omitempty"`
	StalePrimaryClusterIDs      []string         `json:"stale_primary_cluster_ids,omitempty"`
	StaleRelationshipIDs        []string         `json:"stale_relationship_ids,omitempty"`
	StaleSecondaryFingerprints  []string         `json:"stale_secondary_fingerprints,omitempty"`
	LocalClusterID              string           `json:"local_cluster_id,omitempty"`
	LastAppliedIndex            uint64           `json:"last_applied_index"`
	LastKnownPrimaryIndex       uint64           `json:"last_known_primary_index"`
	PromotionClass              DRPromotionClass `json:"promotion_class"`
	EstimatedDataLossEntries    uint64           `json:"estimated_data_loss_entries,omitempty"`
	DataLossEstimateBasis       string           `json:"data_loss_estimate_basis,omitempty"`
	DataLossAccepted            bool             `json:"data_loss_accepted,omitempty"`
	ForcedReasonCodes           []string         `json:"forced_reason_codes,omitempty"`
	ForcedReasonDetails         []string         `json:"forced_reason_details,omitempty"`
}

// DRConfig is the persistent DR replication configuration.
type DRConfig struct {
	// Mode is the DR role: "disabled", "primary", or "secondary".
	Mode DRMode `json:"mode"`

	// ClusterID is a unique identifier for this DR cluster pair.
	ClusterID string `json:"cluster_id"`

	// ReplSalt is the shared HMAC key for KID derivation.
	ReplSalt []byte `json:"repl_salt"`

	// PrimaryAddr mirrors the first entry in PrimaryAddrs for convenience.
	PrimaryAddr string `json:"primary_addr,omitempty"`

	// PrimaryAddrs is the ordered candidate set of primary cluster gRPC
	// addresses used by secondary reconnect logic.
	PrimaryAddrs []string `json:"primary_addrs,omitempty"`

	// RelationshipID is the active relationship ID on secondary nodes.
	RelationshipID string `json:"relationship_id,omitempty"`

	// SecondaryKeyringBootstrapped records that the secondary has already
	// adopted the primary root key/keyring. This makes HA active restore skip
	// the one-time SyncKeyring bootstrap for an already-active relationship.
	SecondaryKeyringBootstrapped bool `json:"secondary_keyring_bootstrapped,omitempty"`

	// PrimaryCACert is the primary's DR transport CA certificate (DER-encoded).
	// Persisted so the secondary can re-establish mTLS after a restart.
	// This is the sole trust anchor for verifying primary identity.
	PrimaryCACert []byte `json:"primary_ca_cert,omitempty"`

	// PrimaryAPIAddr, PrimaryAPICACert, and PrimaryAPIServerName describe the
	// primary HTTP API endpoint used for bootstrap registration and later
	// first-class secondary credential rotation.
	PrimaryAPIAddr       string `json:"primary_api_addr,omitempty"`
	PrimaryAPICACert     []byte `json:"primary_api_ca_cert,omitempty"`
	PrimaryAPIServerName string `json:"primary_api_server_name,omitempty"`

	// SecondaryClientCert and SecondaryClientKeyPEM are the secondary's stable
	// DR client certificate material. The certificate is registered with the
	// primary and the private key remains barrier-encrypted in local DR config.
	SecondaryClientCert   []byte `json:"secondary_client_cert,omitempty"`
	SecondaryClientKeyPEM []byte `json:"secondary_client_key_pem,omitempty"`

	// PendingSecondaryClientCert and PendingSecondaryClientKeyPEM are local
	// durable state for an in-progress first-class secondary credential rotation.
	// The current credential remains active until the primary confirms the
	// pending credential.
	PendingSecondaryClientCert        []byte `json:"pending_secondary_client_cert,omitempty"`
	PendingSecondaryClientKeyPEM      []byte `json:"pending_secondary_client_key_pem,omitempty"`
	PendingSecondaryRotationOperation string `json:"pending_secondary_rotation_operation,omitempty"`
	PendingSecondaryRotationStartedAt int64  `json:"pending_secondary_rotation_started_at,omitempty"`

	// Promotion records the most recent secondary promotion lineage.
	Promotion *DRPromotionRecord `json:"promotion,omitempty"`

	// ReconcileIntegrityMode controls Phase A match behavior.
	// "performance" (default): Phase A match skips to next range.
	// "strict": Phase A match requires top-level RangeDescriptor confirmation.
	ReconcileIntegrityMode string `json:"reconcile_integrity_mode,omitempty"`

	// Optional DR runtime tuning knobs. Zero values mean "use defaults".
	CheckpointTTLSeconds                     int64   `json:"checkpoint_ttl_seconds,omitempty"`
	CheckpointGlobalBudgetBytes              uint64  `json:"checkpoint_global_budget_bytes,omitempty"`
	CheckpointPerRelBudgetBytes              uint64  `json:"checkpoint_per_relationship_budget_bytes,omitempty"`
	StreamBufferMaxEntries                   int     `json:"stream_buffer_max_entries,omitempty"`
	StreamBufferMaxBytes                     uint64  `json:"stream_buffer_max_bytes,omitempty"`
	ReconcileMaxRPCBytes                     uint64  `json:"reconcile_max_rpc_bytes,omitempty"`
	ReconcileMaxWallTimeSeconds              int64   `json:"reconcile_max_wall_time_seconds,omitempty"`
	ReconcileMaxInflightTasks                int     `json:"reconcile_max_inflight_tasks,omitempty"`
	StreamBatchMaxEntries                    int     `json:"stream_batch_max_entries,omitempty"`
	StreamBatchMaxBytes                      int     `json:"stream_batch_max_bytes,omitempty"`
	StreamBatchMaxWaitMillis                 int64   `json:"stream_batch_max_wait_milliseconds,omitempty"`
	FlatAccumulatorSnapshotMinEntries        uint64  `json:"flat_accumulator_snapshot_min_entries,omitempty"`
	FlatAccumulatorSnapshotMinIntervalMillis int64   `json:"flat_accumulator_snapshot_min_interval_milliseconds,omitempty"`
	StreamJournalEnabled                     bool    `json:"stream_journal_enabled,omitempty"`
	StreamJournalMaxBytes                    uint64  `json:"stream_journal_max_bytes,omitempty"`
	StreamJournalSegmentBytes                uint64  `json:"stream_journal_segment_bytes,omitempty"`
	StreamJournalRetentionSecs               int64   `json:"stream_journal_retention_seconds,omitempty"`
	ReconcileApplyWorkers                    int     `json:"reconcile_apply_workers,omitempty"`
	ReconcilePutBatchMaxEntries              int     `json:"reconcile_put_batch_max_entries,omitempty"`
	ReconcilePutBatchMaxBytes                int     `json:"reconcile_put_batch_max_bytes,omitempty"`
	ConvergenceMinRateRatio                  float64 `json:"convergence_min_rate_ratio,omitempty"`
	ConvergenceStallSeconds                  int64   `json:"convergence_stall_seconds,omitempty"`
	FallbackEnabled                          bool    `json:"fallback_enabled,omitempty"`
	FallbackStallSeconds                     int64   `json:"fallback_stall_seconds,omitempty"`
	FallbackFailureThreshold                 int     `json:"fallback_failure_threshold,omitempty"`
	FallbackMinLagEntries                    uint64  `json:"fallback_min_lag_entries,omitempty"`
	FallbackCooldownSeconds                  int64   `json:"fallback_cooldown_seconds,omitempty"`
	FallbackMaxPerHour                       int     `json:"fallback_max_per_hour,omitempty"`

	CheckpointArtifactEnabled           bool    `json:"checkpoint_artifact_enabled,omitempty"`
	CheckpointArtifactGlobalBudgetBytes uint64  `json:"checkpoint_artifact_global_budget_bytes,omitempty"`
	CheckpointArtifactPerRelBudgetBytes uint64  `json:"checkpoint_artifact_per_relationship_budget_bytes,omitempty"`
	CheckpointArtifactTTLSeconds        int64   `json:"checkpoint_artifact_ttl_seconds,omitempty"`
	CheckpointArtifactSegmentBytes      uint64  `json:"checkpoint_artifact_segment_bytes,omitempty"`
	DRBackpressureEnabled               bool    `json:"dr_backpressure_enabled,omitempty"`
	DRBackpressureDegradedRatio         float64 `json:"dr_backpressure_degraded_ratio,omitempty"`
	DRBackpressureCriticalRatio         float64 `json:"dr_backpressure_critical_ratio,omitempty"`
	DRBackpressureMinLagEntries         uint64  `json:"dr_backpressure_min_lag_entries,omitempty"`
	DRBackpressureHorizonSeconds        int64   `json:"dr_backpressure_horizon_seconds,omitempty"`
	DRBackpressureDegradedMinQPS        int64   `json:"dr_backpressure_degraded_min_qps,omitempty"`
	DRBackpressureCriticalMinQPS        int64   `json:"dr_backpressure_critical_min_qps,omitempty"`
}

// DRActivationToken contains the information a secondary needs to
// establish a DR relationship with the primary.
type DRActivationToken struct {
	// ClusterID identifies the DR cluster pair.
	ClusterID string `json:"cluster_id"`

	// RelationshipID identifies the specific DR relationship.
	RelationshipID string `json:"relationship_id"`

	// PrimaryAddr mirrors the first entry in PrimaryAddrs for convenience.
	PrimaryAddr string `json:"primary_addr"`

	// PrimaryAddrs is the ordered candidate set of primary cluster gRPC
	// addresses used for reconnect/failover without a load balancer.
	PrimaryAddrs []string `json:"primary_addrs,omitempty"`

	// PrimaryAPIAddr is the primary's HTTP API address (for the
	// secondary to register its cert before mTLS connection).
	PrimaryAPIAddr string `json:"primary_api_addr,omitempty"`

	// ReplSalt is the shared HMAC key for KID derivation.
	ReplSalt []byte `json:"repl_salt"`

	// DRTransportCACert is the primary's DR transport CA certificate (DER).
	// This is the sole trust anchor for verifying primary identity over
	// the DR gRPC transport.
	DRTransportCACert []byte `json:"dr_transport_ca_cert,omitempty"`

	// PrimaryAPICACert is the CA certificate used to validate the
	// primary API endpoint during secondary registration. If empty,
	// DRTransportCACert is used.
	PrimaryAPICACert []byte `json:"primary_api_ca_cert,omitempty"`

	// PrimaryAPIServerName overrides the TLS ServerName for primary
	// API registration requests.
	PrimaryAPIServerName string `json:"primary_api_server_name,omitempty"`

	// BootstrapToken is a single-use token that authenticates the
	// secondary's cert registration with the primary's HTTP API.
	BootstrapToken string `json:"bootstrap_token,omitempty"`
}

type DRRelationshipState string

const (
	DRRelationshipStatePending    DRRelationshipState = "pending"
	DRRelationshipStateRegistered DRRelationshipState = "registered"
	DRRelationshipStateActive     DRRelationshipState = "active"
	DRRelationshipStateRevoked    DRRelationshipState = "revoked"
)

func normalizePrimaryAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "http://") {
		addr = "https://" + strings.TrimPrefix(addr, "http://")
	}
	if !strings.HasPrefix(addr, "https://") {
		addr = "https://" + addr
	}
	return addr
}

func normalizePrimaryAPIAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "https://" + addr
}

func normalizePrimaryAddrs(primaryAddrs []string) []string {
	out := make([]string, 0, len(primaryAddrs))
	seen := make(map[string]struct{}, len(primaryAddrs))
	add := func(addr string) {
		addr = normalizePrimaryAddr(addr)
		if addr == "" {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}

	for _, addr := range primaryAddrs {
		add(addr)
	}
	return out
}

func isStalePostPromotionActivationToken(promotion *DRPromotionRecord, token *DRActivationToken) bool {
	if promotion == nil || token == nil {
		return false
	}
	return isStalePostPromotionLineage(promotion, token.ClusterID, token.RelationshipID)
}

func isStalePostPromotionLineage(promotion *DRPromotionRecord, clusterID, relationshipID string) bool {
	if promotion == nil {
		return false
	}
	if promotion.OldPrimaryClusterID != "" && clusterID == promotion.OldPrimaryClusterID {
		return true
	}
	for _, staleClusterID := range promotion.StalePrimaryClusterIDs {
		if staleClusterID != "" && clusterID == staleClusterID {
			return true
		}
	}
	if promotion.OldRelationshipID != "" && relationshipID == promotion.OldRelationshipID {
		return true
	}
	for _, staleRelationshipID := range promotion.StaleRelationshipIDs {
		if staleRelationshipID != "" && relationshipID == staleRelationshipID {
			return true
		}
	}
	return false
}

func isStalePostPromotionFingerprint(promotion *DRPromotionRecord, fingerprint string) bool {
	if promotion == nil || fingerprint == "" {
		return false
	}
	if promotion.OldSecondaryCertFingerprint != "" && strings.EqualFold(fingerprint, promotion.OldSecondaryCertFingerprint) {
		return true
	}
	for _, staleFingerprint := range promotion.StaleSecondaryFingerprints {
		if staleFingerprint != "" && strings.EqualFold(fingerprint, staleFingerprint) {
			return true
		}
	}
	return false
}

func appendUniquePromotionStrings(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	out := make([]string, 0, len(dst)+len(values))
	for _, value := range dst {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func inheritPromotionLineage(dst *DRPromotionRecord, previous *DRPromotionRecord) {
	if dst == nil || previous == nil {
		return
	}
	dst.StalePrimaryClusterIDs = appendUniquePromotionStrings(dst.StalePrimaryClusterIDs, previous.OldPrimaryClusterID)
	dst.StalePrimaryClusterIDs = appendUniquePromotionStrings(dst.StalePrimaryClusterIDs, previous.StalePrimaryClusterIDs...)
	dst.StaleRelationshipIDs = appendUniquePromotionStrings(dst.StaleRelationshipIDs, previous.OldRelationshipID)
	dst.StaleRelationshipIDs = appendUniquePromotionStrings(dst.StaleRelationshipIDs, previous.StaleRelationshipIDs...)
	dst.StaleSecondaryFingerprints = appendUniquePromotionStrings(dst.StaleSecondaryFingerprints, previous.OldSecondaryCertFingerprint)
	dst.StaleSecondaryFingerprints = appendUniquePromotionStrings(dst.StaleSecondaryFingerprints, previous.StaleSecondaryFingerprints...)
}

func disabledConfigPreservingPromotion(promotion *DRPromotionRecord) *DRConfig {
	return &DRConfig{
		Mode:      DRModeDisabled,
		Promotion: promotion,
	}
}

type DRRelationship struct {
	RelationshipID string              `json:"relationship_id"`
	State          DRRelationshipState `json:"state"`

	// SecondaryCertFingerprint is SHA-256 over the secondary leaf cert DER.
	SecondaryCertFingerprint string `json:"secondary_cert_fingerprint,omitempty"`

	// SecondaryCACert is the secondary cluster CA/leaf cert bytes used for mTLS trust.
	SecondaryCACert []byte `json:"secondary_ca_cert,omitempty"`

	// PreviousSecondaryCertFingerprint is retained after first-class credential
	// rotation so stale credentials cannot be reused in another relationship.
	PreviousSecondaryCertFingerprint string `json:"previous_secondary_cert_fingerprint,omitempty"`

	// PendingSecondaryCertFingerprint and PendingSecondaryCACert represent a
	// staged first-class credential rotation. While pending, both the current and
	// pending certs are trusted so the secondary can switch certificates and
	// confirm with the new credential.
	PendingSecondaryCertFingerprint string `json:"pending_secondary_cert_fingerprint,omitempty"`
	PendingSecondaryCACert          []byte `json:"pending_secondary_ca_cert,omitempty"`
	PendingRotationOperationID      string `json:"pending_rotation_operation_id,omitempty"`
	PendingRotationStartedAt        int64  `json:"pending_rotation_started_at,omitempty"`

	// CredentialGeneration starts at 1 after bootstrap registration and
	// increments after each finalized first-class rotation.
	CredentialGeneration uint64 `json:"credential_generation,omitempty"`
	RotatedAt            int64  `json:"rotated_at,omitempty"`

	// BootstrapToken is retained only to decode legacy pending records that
	// stored the one-time secret directly. New records store BootstrapTokenHash.
	BootstrapToken string `json:"bootstrap_token,omitempty"`

	// BootstrapTokenHash is a verifier for the one-time cert-registration token.
	// The bearer token itself is returned only in the activation token and is
	// not persisted in relationship state.
	BootstrapTokenHash string `json:"bootstrap_token_hash,omitempty"`

	CreatedAt  int64 `json:"created_at"`
	LastSeenAt int64 `json:"last_seen_at"`

	// ExpiresAt is bootstrap token expiry for pending relationships.
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// FailedAttempts tracks failed bootstrap registration attempts.
	FailedAttempts int `json:"failed_attempts,omitempty"`

	// LockedUntil is a lockout timestamp after too many failed attempts.
	LockedUntil int64 `json:"locked_until,omitempty"`

	// RegisteredFromIP is the source IP that successfully registered the cert.
	RegisteredFromIP string `json:"registered_from_ip,omitempty"`

	// RegisteredAt records the successful bootstrap registration timestamp.
	RegisteredAt int64 `json:"registered_at,omitempty"`

	// LastFailedAt and LastFailedFromIP record the latest failed bootstrap
	// registration attempt for audit/status surfaces.
	LastFailedAt     int64  `json:"last_failed_at,omitempty"`
	LastFailedFromIP string `json:"last_failed_from_ip,omitempty"`

	// RevokedAt is the revoke timestamp, if revoked.
	RevokedAt int64 `json:"revoked_at,omitempty"`

	// LastError stores the most recent bootstrap/authz failure reason.
	LastError string `json:"last_error,omitempty"`
}

// drRelationshipManager manages the DR replication lifecycle: enabling/
// disabling primary/secondary mode, generating activation tokens, and
// coordinating the primary/secondary components.
type drRelationshipManager struct {
	core   *Core
	logger log.Logger

	mu     sync.RWMutex
	config *DRConfig

	// dispatcher is the FSM change stream hook that maintains the
	// stream journal on ALL primary-cluster Raft nodes and delegates
	// to the primary when this node is the active leader. It is
	// created once and outlives individual primary instances.
	dispatcher *drChangeStreamDispatcher

	// primary is non-nil when this cluster is a DR primary.
	primary *drReplicationPrimary

	// handler is the cluster handler for the primary side. Stored here
	// so the registration endpoint and LoadConfig can add trusted certs.
	handler *drReplicationClusterHandler

	// transportCA is the DR transport CA used to sign per-leader
	// certificates and embedded in activation tokens. Non-nil when
	// primary mode is enabled.
	transportCA *drTransportCA

	// secondary is non-nil when this cluster is a DR secondary.
	secondary *drReplicationSecondary

	secondaryLoopCancel context.CancelFunc

	// lastSeenWriteAt throttles persistence of LastSeenAt.
	lastSeenWriteAt map[string]time.Time
	// latestSeenAt keeps an in-memory timestamp for near-real-time status.
	latestSeenAt map[string]int64

	lastPendingCleanupAt time.Time
}

// newDRRelationshipManager creates a new relationship manager.
func newDRRelationshipManager(core *Core, logger log.Logger) *drRelationshipManager {
	if logger == nil {
		logger = log.NewNullLogger()
	}

	namedLogger := logger.Named("dr-manager")

	// Determine a persistent journal directory. If the underlying
	// physical backend is Raft, use a subdirectory of its data path
	// so journal segments survive restarts and leader elections.
	// For non-Raft backends (e.g. in-memory test backends), create an
	// isolated temp directory to avoid cross-test contamination.
	var journalDir string
	if rb, ok := core.underlyingPhysical.(*raft.RaftBackend); ok {
		journalDir = rb.JournalDir()
	} else {
		tmp, err := os.MkdirTemp("", "openbao-dr-journal-*")
		if err != nil {
			namedLogger.Warn("failed to create temp journal dir", "error", err)
		} else {
			journalDir = tmp
		}
	}

	// Create the shared stream journal and dispatcher. The dispatcher
	// is registered as the FSM change stream hook so it runs on ALL
	// nodes in the primary cluster (leaders and followers). This
	// ensures the journal is populated before a node becomes leader.
	journal := newDRStreamJournal(namedLogger, journalDir)
	if err := journal.configure(true, drDefaultStreamJournalMaxBytes, drDefaultStreamJournalSegmentBytes, drDefaultStreamJournalRetention); err != nil {
		namedLogger.Warn("failed to initialize shared DR stream journal", "error", err)
	}

	dispatcher := &drChangeStreamDispatcher{
		journal: journal,
		logger:  namedLogger,
	}

	// Register the dispatcher's OnChange as the FSM hook immediately.
	if csb, ok := core.underlyingPhysical.(physical.ChangeStreamBackend); ok {
		csb.HookChangeStream(dispatcher.OnChange)
	}

	return &drRelationshipManager{
		core:            core,
		logger:          namedLogger,
		config:          &DRConfig{Mode: DRModeDisabled},
		dispatcher:      dispatcher,
		lastSeenWriteAt: make(map[string]time.Time),
		latestSeenAt:    make(map[string]int64),
	}
}

func applyDRConfigDefaults(cfg *DRConfig) {
	if cfg == nil {
		return
	}
	if cfg.FallbackStallSeconds <= 0 {
		cfg.FallbackStallSeconds = int64(drDefaultFallbackStall / time.Second)
	}
	if cfg.FallbackFailureThreshold <= 0 {
		cfg.FallbackFailureThreshold = drDefaultFallbackFailureThreshold
	}
	if cfg.FallbackCooldownSeconds <= 0 {
		cfg.FallbackCooldownSeconds = int64(drDefaultFallbackCooldown / time.Second)
	}
	if cfg.FallbackMaxPerHour <= 0 {
		cfg.FallbackMaxPerHour = drDefaultFallbackMaxPerHour
	}
	if cfg.Mode != DRModeDisabled && !cfg.FallbackEnabled {
		cfg.FallbackEnabled = drDefaultFallbackEnabled
	}
	if cfg.StreamJournalMaxBytes == 0 {
		cfg.StreamJournalMaxBytes = drDefaultStreamJournalMaxBytes
	}
	if cfg.StreamJournalSegmentBytes == 0 {
		cfg.StreamJournalSegmentBytes = drDefaultStreamJournalSegmentBytes
	}
	if cfg.StreamJournalRetentionSecs == 0 {
		cfg.StreamJournalRetentionSecs = int64(drDefaultStreamJournalRetention / time.Second)
	}
	if cfg.Mode != DRModeDisabled &&
		!cfg.StreamJournalEnabled &&
		cfg.StreamJournalMaxBytes == drDefaultStreamJournalMaxBytes &&
		cfg.StreamJournalSegmentBytes == drDefaultStreamJournalSegmentBytes {
		cfg.StreamJournalEnabled = true
	}
	if cfg.PrimaryAPIAddr != "" {
		cfg.PrimaryAPIAddr = normalizePrimaryAPIAddr(cfg.PrimaryAPIAddr)
		if len(cfg.PrimaryAPICACert) == 0 {
			cfg.PrimaryAPICACert = cfg.PrimaryCACert
		}
		if cfg.PrimaryAPIServerName == "" {
			cfg.PrimaryAPIServerName = deriveServerName(cfg.PrimaryAPIAddr)
		}
	}
	if cfg.ReconcileApplyWorkers <= 0 {
		cfg.ReconcileApplyWorkers = drDefaultReconcileApplyWorkers
	}
	if cfg.ReconcilePutBatchMaxEntries <= 0 {
		cfg.ReconcilePutBatchMaxEntries = drDefaultReconcilePutBatchEntries
	}
	if cfg.ReconcilePutBatchMaxBytes <= 0 {
		cfg.ReconcilePutBatchMaxBytes = drDefaultReconcilePutBatchBytes
	}
	if cfg.ConvergenceMinRateRatio <= 0 {
		cfg.ConvergenceMinRateRatio = drDefaultConvergenceMinRateRatio
	}
	if cfg.ConvergenceStallSeconds <= 0 {
		cfg.ConvergenceStallSeconds = int64(drDefaultConvergenceStall / time.Second)
	}
	if cfg.FlatAccumulatorSnapshotMinEntries == 0 {
		cfg.FlatAccumulatorSnapshotMinEntries = drDefaultFlatAccumulatorSnapshotMinEntries
	}
	if cfg.FlatAccumulatorSnapshotMinIntervalMillis == 0 {
		cfg.FlatAccumulatorSnapshotMinIntervalMillis = int64(drDefaultFlatAccumulatorSnapshotMinInterval / time.Millisecond)
	}
	if cfg.Mode != DRModeDisabled && !cfg.CheckpointArtifactEnabled {
		cfg.CheckpointArtifactEnabled = true
	}
	if cfg.CheckpointArtifactGlobalBudgetBytes == 0 {
		cfg.CheckpointArtifactGlobalBudgetBytes = drCheckpointArtifactDefaultGlobalBudget
	}
	if cfg.CheckpointArtifactPerRelBudgetBytes == 0 {
		cfg.CheckpointArtifactPerRelBudgetBytes = drCheckpointArtifactDefaultPerRelBudget
	}
	if cfg.CheckpointArtifactTTLSeconds == 0 {
		cfg.CheckpointArtifactTTLSeconds = int64(drCheckpointArtifactDefaultTTL / time.Second)
	}
	if cfg.CheckpointArtifactSegmentBytes == 0 {
		cfg.CheckpointArtifactSegmentBytes = drCheckpointArtifactDefaultSegmentBytes
	}
	if cfg.Mode != DRModeDisabled && !cfg.DRBackpressureEnabled {
		cfg.DRBackpressureEnabled = drBackpressureDefaultEnabled
	}
	if cfg.DRBackpressureDegradedRatio <= 0 {
		cfg.DRBackpressureDegradedRatio = drBackpressureDefaultDegradedRatio
	}
	if cfg.DRBackpressureCriticalRatio <= 0 {
		cfg.DRBackpressureCriticalRatio = drBackpressureDefaultCriticalRatio
	}
	if cfg.DRBackpressureHorizonSeconds <= 0 {
		cfg.DRBackpressureHorizonSeconds = drBackpressureDefaultHorizonSeconds
	}
	if cfg.DRBackpressureDegradedMinQPS <= 0 {
		cfg.DRBackpressureDegradedMinQPS = drBackpressureDefaultDegradedMinQPS
	}
	if cfg.DRBackpressureCriticalMinQPS <= 0 {
		cfg.DRBackpressureCriticalMinQPS = drBackpressureDefaultCriticalMinQPS
	}
}

func validateDRTuningConfig(cfg *DRConfig) error {
	if cfg == nil {
		return nil
	}

	positiveInt := func(name string, value int) error {
		if value < 0 {
			return fmt.Errorf("%s must be zero or greater", name)
		}
		return nil
	}
	positiveInt64 := func(name string, value int64) error {
		if value < 0 {
			return fmt.Errorf("%s must be zero or greater", name)
		}
		return nil
	}
	seconds := func(name string, value int64) error {
		if err := positiveInt64(name, value); err != nil {
			return err
		}
		if value > drMaxTuningDurationSeconds {
			return fmt.Errorf("%s is too large", name)
		}
		return nil
	}
	millis := func(name string, value int64) error {
		if err := positiveInt64(name, value); err != nil {
			return err
		}
		if value > drMaxTuningDurationMillis {
			return fmt.Errorf("%s is too large", name)
		}
		return nil
	}
	ratio := func(name string, value float64) error {
		if value < 0 {
			return fmt.Errorf("%s must be zero or greater", name)
		}
		if value > 1 {
			return fmt.Errorf("%s must be <= 1", name)
		}
		return nil
	}

	for name, value := range map[string]int{
		"stream_buffer_max_entries":       cfg.StreamBufferMaxEntries,
		"reconcile_max_inflight_tasks":    cfg.ReconcileMaxInflightTasks,
		"stream_batch_max_entries":        cfg.StreamBatchMaxEntries,
		"stream_batch_max_bytes":          cfg.StreamBatchMaxBytes,
		"reconcile_apply_workers":         cfg.ReconcileApplyWorkers,
		"reconcile_put_batch_max_entries": cfg.ReconcilePutBatchMaxEntries,
		"reconcile_put_batch_max_bytes":   cfg.ReconcilePutBatchMaxBytes,
		"fallback_failure_threshold":      cfg.FallbackFailureThreshold,
		"fallback_max_per_hour":           cfg.FallbackMaxPerHour,
	} {
		if err := positiveInt(name, value); err != nil {
			return err
		}
	}

	for name, value := range map[string]int64{
		"checkpoint_ttl_seconds":           cfg.CheckpointTTLSeconds,
		"reconcile_max_wall_time_seconds":  cfg.ReconcileMaxWallTimeSeconds,
		"stream_journal_retention_seconds": cfg.StreamJournalRetentionSecs,
		"convergence_stall_seconds":        cfg.ConvergenceStallSeconds,
		"fallback_stall_seconds":           cfg.FallbackStallSeconds,
		"fallback_cooldown_seconds":        cfg.FallbackCooldownSeconds,
		"checkpoint_artifact_ttl_seconds":  cfg.CheckpointArtifactTTLSeconds,
		"dr_backpressure_horizon_seconds":  cfg.DRBackpressureHorizonSeconds,
	} {
		if err := seconds(name, value); err != nil {
			return err
		}
	}
	if err := millis("stream_batch_max_wait_milliseconds", cfg.StreamBatchMaxWaitMillis); err != nil {
		return err
	}
	if err := millis("flat_accumulator_snapshot_min_interval_milliseconds", cfg.FlatAccumulatorSnapshotMinIntervalMillis); err != nil {
		return err
	}

	for name, value := range map[string]int64{
		"dr_backpressure_degraded_min_qps": cfg.DRBackpressureDegradedMinQPS,
		"dr_backpressure_critical_min_qps": cfg.DRBackpressureCriticalMinQPS,
	} {
		if err := positiveInt64(name, value); err != nil {
			return err
		}
	}

	for name, value := range map[string]float64{
		"convergence_min_rate_ratio":     cfg.ConvergenceMinRateRatio,
		"dr_backpressure_degraded_ratio": cfg.DRBackpressureDegradedRatio,
		"dr_backpressure_critical_ratio": cfg.DRBackpressureCriticalRatio,
	} {
		if err := ratio(name, value); err != nil {
			return err
		}
	}

	if cfg.CheckpointGlobalBudgetBytes > 0 && cfg.CheckpointPerRelBudgetBytes > cfg.CheckpointGlobalBudgetBytes {
		return fmt.Errorf("checkpoint_per_relationship_budget_bytes must be <= checkpoint_global_budget_bytes")
	}
	if cfg.StreamJournalMaxBytes > 0 && cfg.StreamJournalSegmentBytes > cfg.StreamJournalMaxBytes {
		return fmt.Errorf("stream_journal_segment_bytes must be <= stream_journal_max_bytes")
	}
	if cfg.CheckpointArtifactGlobalBudgetBytes > 0 && cfg.CheckpointArtifactPerRelBudgetBytes > cfg.CheckpointArtifactGlobalBudgetBytes {
		return fmt.Errorf("checkpoint_artifact_per_relationship_budget_bytes must be <= checkpoint_artifact_global_budget_bytes")
	}
	if cfg.CheckpointArtifactGlobalBudgetBytes > 0 && cfg.CheckpointArtifactSegmentBytes > cfg.CheckpointArtifactGlobalBudgetBytes {
		return fmt.Errorf("checkpoint_artifact_segment_bytes must be <= checkpoint_artifact_global_budget_bytes")
	}
	if cfg.DRBackpressureDegradedRatio > 0 && cfg.DRBackpressureCriticalRatio > 0 && cfg.DRBackpressureCriticalRatio > cfg.DRBackpressureDegradedRatio {
		return fmt.Errorf("dr_backpressure_critical_ratio must be <= dr_backpressure_degraded_ratio")
	}
	if cfg.DRBackpressureDegradedMinQPS > 0 && cfg.DRBackpressureCriticalMinQPS > cfg.DRBackpressureDegradedMinQPS {
		return fmt.Errorf("dr_backpressure_critical_min_qps must be <= dr_backpressure_degraded_min_qps")
	}

	return nil
}

// LoadConfig loads the DR configuration from storage and restores
// the primary or secondary replication state if previously enabled.
func (m *drRelationshipManager) LoadConfig(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, err := m.core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read DR config: %w", err)
	}
	if entry == nil {
		m.config = &DRConfig{Mode: DRModeDisabled}
		return nil
	}

	var config DRConfig
	if err := json.Unmarshal(entry.Value, &config); err != nil {
		return fmt.Errorf("failed to unmarshal DR config: %w", err)
	}
	applyDRConfigDefaults(&config)
	if config.Mode == DRModeSecondary && isStalePostPromotionLineage(config.Promotion, config.ClusterID, config.RelationshipID) {
		m.logger.Warn("rejecting stale post-promotion DR secondary config during restore",
			"cluster_id", config.ClusterID,
			"relationship_id", config.RelationshipID)
		m.config = disabledConfigPreservingPromotion(config.Promotion)
		m.stopSecondaryRuntimeLocked()
		m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))
		if err := m.saveConfig(ctx); err != nil {
			return fmt.Errorf("failed to disable stale post-promotion DR secondary config: %w", err)
		}
		return nil
	}
	m.config = &config

	// Restore replication state based on persisted config.
	switch config.Mode {
	case DRModePrimary:
		m.logger.Info("restoring DR primary mode from config", "cluster_id", config.ClusterID)

		// Load the DR transport CA from barrier storage so the handler can
		// mint a CA-signed leaf cert. Without this, new leaders after a
		// stepdown cannot present a valid DR transport identity.
		transportCA, err := loadDRTransportCA(m.core)
		if err != nil {
			m.logger.Error("failed to load DR transport CA during config restore", "error", err)
		} else if transportCA != nil {
			m.transportCA = transportCA
		} else {
			m.logger.Warn("no DR transport CA found in storage; DR secondaries may reject connections")
		}

		m.primary = NewDRReplicationPrimary(m.core, config.ReplSalt, m.logger, m.dispatcherJournal())
		m.applyPrimaryTunablesLocked()

		// Restore persisted dirty bitmap so post-restart reconciliation
		// knows which ranges were modified before the crash.
		if err := m.primary.loadDirtyBitmap(ctx); err != nil {
			m.logger.Warn("failed to load persisted dirty bitmap", "error", err)
		}

		// Attach the primary to the dispatcher so the FSM hook
		// now also drives ring buffer, subscriber fan-out, etc.
		if m.dispatcher != nil {
			m.dispatcher.setPrimary(m.primary)
		}

		// Seed indexApplied with the current Raft applied index so the
		// checkpoint fence does not stall waiting for entries that were
		// committed before the change stream hook was re-registered.
		if rb, ok := m.core.underlyingPhysical.(*raft.RaftBackend); ok {
			m.primary.SeedAppliedIndex(rb.AppliedIndex())
		}

		// Proactively warm the checkpoint index in the background so the
		// first RequestCheckpoint does not pay the full-scan cost.
		m.primary.WarmIndex(m.core.activeContext.Load())

		// Re-register the cluster handler with a CA-signed leaf cert.
		m.handler = newDRReplicationClusterHandler(m.core, m.primary, m.logger)
		if m.transportCA != nil {
			if err := m.handler.SetTransportCA(m.transportCA); err != nil {
				if errors.Is(err, errDRTransportLeafKeyUnavailable) {
					m.logger.Debug("deferring DR transport leaf cert mint until local cluster key is available")
				} else {
					m.logger.Error("failed to mint DR transport leaf cert on restore", "error", err)
				}
			}
			m.handler.startLeafRenewal(m.transportCA)
		}
		registerDRHandler(m.core, m.handler)

		// Restore trusted secondary certs from storage.
		if err := m.restoreRelationshipCerts(ctx); err != nil {
			m.logger.Warn("failed to restore secondary certs", "error", err)
		}

		m.core.replicationState.Store(uint32(consts.ReplicationDRPrimary))

	case DRModeSecondary:
		normalizedPrimaryAddrs := normalizePrimaryAddrs(config.PrimaryAddrs)
		if len(normalizedPrimaryAddrs) == 0 {
			return fmt.Errorf("invalid DR secondary config: missing primary_addrs")
		}
		config.PrimaryAddrs = normalizedPrimaryAddrs
		config.PrimaryAddr = normalizedPrimaryAddrs[0]

		m.logger.Info("restoring DR secondary mode from config",
			"cluster_id", config.ClusterID,
			"primary_addr", config.PrimaryAddr,
			"primary_addrs", config.PrimaryAddrs)

		if config.RelationshipID == "" {
			return fmt.Errorf("invalid DR secondary config: missing relationship_id")
		}

		m.secondary = newDRReplicationSecondary(
			m.core,
			config.ReplSalt,
			config.RelationshipID,
			m.logger,
		)
		if config.SecondaryKeyringBootstrapped {
			m.secondary.keyringBootstrapped.Store(true)
		}
		m.applySecondaryTunablesLocked()

		// Restore the primary's CA cert so mTLS works after restart.
		if len(config.PrimaryCACert) > 0 {
			m.secondary.primaryCACert = config.PrimaryCACert
		}
		if len(config.SecondaryClientCert) > 0 || len(config.SecondaryClientKeyPEM) > 0 {
			if err := m.secondary.setClientCertificate(config.SecondaryClientCert, config.SecondaryClientKeyPEM); err != nil {
				return fmt.Errorf("invalid DR secondary client certificate: %w", err)
			}
		}

		m.core.replicationState.Store(uint32(consts.ReplicationDRSecondary))

		if len(config.PrimaryAddrs) > 0 {
			m.startSecondaryControllerLocked()
		}
	}

	return nil
}

// Teardown cleanly shuts down DR replication state when the node steps
// down (preSeal). It unregisters the cluster handler (stopping the leaf
// renewal goroutine and gRPC server), cancels the secondary controller
// loop, and clears in-memory state so the next postUnseal/LoadConfig
// cycle starts fresh. The persisted config is NOT touched -- only the
// runtime state is reset.
func (m *drRelationshipManager) Teardown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.handler != nil {
		unregisterDRHandler(m.core) // calls handler.Stop() via StopHandler
		m.handler = nil
	}

	if m.secondaryLoopCancel != nil {
		m.secondaryLoopCancel()
		m.secondaryLoopCancel = nil
	}

	// Mirror DisablePrimary teardown behavior on stepdown so this node stops
	// active-only primary processing when no longer leader.
	if m.dispatcher != nil {
		m.dispatcher.clearPrimary()
	}
	if m.primary != nil && m.primary.tombstoneGC != nil {
		m.primary.tombstoneGC.Stop()
	}

	m.primary = nil
	m.secondary = nil
	m.transportCA = nil
}

// dispatcherJournal returns the shared stream journal from the
// dispatcher, or nil if no dispatcher is configured.
func (m *drRelationshipManager) dispatcherJournal() *drStreamJournal {
	if m.dispatcher != nil {
		return m.dispatcher.journal
	}
	return nil
}

// saveConfig persists the DR configuration to storage.
func (m *drRelationshipManager) saveConfig(ctx context.Context) error {
	data, err := json.Marshal(m.config)
	if err != nil {
		return fmt.Errorf("failed to marshal DR config: %w", err)
	}

	entry := &logical.StorageEntry{
		Key:   drConfigPath,
		Value: data,
	}
	return m.core.barrier.Put(ctx, entry)
}

// PersistConfigSnapshot re-persist the in-memory DR configuration.
// This is used by DR secondary bootstrap after root-key swap/purge to ensure
// the persisted config is encrypted under the current barrier key.
func (m *drRelationshipManager) PersistConfigSnapshot(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config == nil {
		return fmt.Errorf("DR config not initialized")
	}

	return m.saveConfig(ctx)
}

// PersistSecondaryKeyringBootstrap marks the secondary keyring bootstrap as
// durable and re-persists DR config under the current barrier key.
func (m *drRelationshipManager) PersistSecondaryKeyringBootstrap(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config == nil {
		return fmt.Errorf("DR config not initialized")
	}
	if m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}

	m.config.SecondaryKeyringBootstrapped = true
	if m.secondary != nil {
		m.secondary.keyringBootstrapped.Store(true)
	}

	return m.saveConfig(ctx)
}

// RefreshConfigFromStorage reloads the persisted DR config into in-memory
// manager state without changing runtime primary/secondary processes.
// It is used by standby invalidation handling to pick up mode transitions
// promptly during DR bootstrap key swaps.
func (m *drRelationshipManager) RefreshConfigFromStorage(ctx context.Context) (DRMode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, err := m.core.barrier.Get(ctx, drConfigPath)
	if err != nil {
		return m.config.Mode, fmt.Errorf("failed to read DR config: %w", err)
	}
	if entry == nil {
		m.config = &DRConfig{Mode: DRModeDisabled}
		return DRModeDisabled, nil
	}

	var config DRConfig
	if err := json.Unmarshal(entry.Value, &config); err != nil {
		return m.config.Mode, fmt.Errorf("failed to unmarshal DR config: %w", err)
	}
	applyDRConfigDefaults(&config)
	if config.Mode == DRModeSecondary && isStalePostPromotionLineage(config.Promotion, config.ClusterID, config.RelationshipID) {
		m.logger.Warn("rejecting stale post-promotion DR secondary config during refresh",
			"cluster_id", config.ClusterID,
			"relationship_id", config.RelationshipID)
		m.config = disabledConfigPreservingPromotion(config.Promotion)
		m.stopSecondaryRuntimeLocked()
		m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))
		if err := m.saveConfig(ctx); err != nil {
			return m.config.Mode, fmt.Errorf("failed to disable stale post-promotion DR secondary config: %w", err)
		}
		return DRModeDisabled, nil
	}

	if config.Mode == DRModeSecondary {
		normalized := normalizePrimaryAddrs(config.PrimaryAddrs)
		if len(normalized) == 0 && config.PrimaryAddr != "" {
			normalized = normalizePrimaryAddrs([]string{config.PrimaryAddr})
		}
		config.PrimaryAddrs = normalized
		if len(config.PrimaryAddrs) > 0 {
			config.PrimaryAddr = config.PrimaryAddrs[0]
		} else {
			config.PrimaryAddr = ""
		}
	}

	m.config = &config
	if config.Mode == DRModeSecondary && config.SecondaryKeyringBootstrapped && m.secondary != nil {
		m.secondary.keyringBootstrapped.Store(true)
	}
	return config.Mode, nil
}

// EnablePrimary enables DR primary mode on this cluster.
func (m *drRelationshipManager) EnablePrimary(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeDisabled {
		return fmt.Errorf("DR replication already enabled as %s", m.config.Mode)
	}

	clusterID, err := uuid.GenerateUUID()
	if err != nil {
		return fmt.Errorf("failed to generate cluster ID: %w", err)
	}

	replSalt := make([]byte, drReplSaltLen)
	if _, err := rand.Read(replSalt); err != nil {
		return fmt.Errorf("failed to generate replication salt: %w", err)
	}

	oldConfig := m.config
	newConfig := &DRConfig{
		Mode:      DRModePrimary,
		ClusterID: clusterID,
		ReplSalt:  replSalt,
		Promotion: oldConfig.Promotion,

		FallbackEnabled:                     drDefaultFallbackEnabled,
		FallbackStallSeconds:                int64(drDefaultFallbackStall / time.Second),
		FallbackFailureThreshold:            drDefaultFallbackFailureThreshold,
		FallbackCooldownSeconds:             int64(drDefaultFallbackCooldown / time.Second),
		FallbackMaxPerHour:                  drDefaultFallbackMaxPerHour,
		CheckpointArtifactEnabled:           true,
		CheckpointArtifactGlobalBudgetBytes: drCheckpointArtifactDefaultGlobalBudget,
		CheckpointArtifactPerRelBudgetBytes: drCheckpointArtifactDefaultPerRelBudget,
		CheckpointArtifactTTLSeconds:        int64(drCheckpointArtifactDefaultTTL / time.Second),
		CheckpointArtifactSegmentBytes:      drCheckpointArtifactDefaultSegmentBytes,
		DRBackpressureEnabled:               true,
		DRBackpressureDegradedRatio:         drBackpressureDefaultDegradedRatio,
		DRBackpressureCriticalRatio:         drBackpressureDefaultCriticalRatio,
		DRBackpressureHorizonSeconds:        drBackpressureDefaultHorizonSeconds,
		DRBackpressureDegradedMinQPS:        drBackpressureDefaultDegradedMinQPS,
		DRBackpressureCriticalMinQPS:        drBackpressureDefaultCriticalMinQPS,
	}

	// Generate or load the DR transport CA. This CA signs per-leader
	// certificates and is embedded in activation tokens as the sole
	// trust anchor for cross-cluster mTLS.
	transportCA, err := loadDRTransportCA(m.core)
	if err != nil {
		return fmt.Errorf("failed to load DR transport CA: %w", err)
	}
	if transportCA == nil {
		transportCA, err = generateDRTransportCA(m.core)
		if err != nil {
			return fmt.Errorf("failed to generate DR transport CA: %w", err)
		}
		m.logger.Info("generated new DR transport CA",
			"spki_hash", transportCA.spkiHash())
	}

	m.config = newConfig
	if err := m.saveConfig(ctx); err != nil {
		m.config = oldConfig
		return err
	}
	m.transportCA = transportCA

	// Initialize the primary-side gRPC server.
	m.primary = NewDRReplicationPrimary(m.core, replSalt, m.logger, m.dispatcherJournal())
	m.applyPrimaryTunablesLocked()

	// Attach the primary to the dispatcher so the FSM hook
	// now also drives ring buffer, subscriber fan-out, etc.
	if m.dispatcher != nil {
		m.dispatcher.setPrimary(m.primary)
	}

	// Seed indexApplied with the current Raft applied index so the
	// checkpoint fence does not stall waiting for entries that were
	// committed before the change stream hook was registered.
	if rb, ok := m.core.underlyingPhysical.(*raft.RaftBackend); ok {
		m.primary.SeedAppliedIndex(rb.AppliedIndex())
	}

	// Proactively warm the checkpoint index in the background so the
	// first RequestCheckpoint does not pay the full-scan cost.
	m.primary.WarmIndex(ctx)

	// Register the DR handler on the cluster listener for mTLS-secured gRPC.
	m.handler = newDRReplicationClusterHandler(m.core, m.primary, m.logger)
	if err := m.handler.SetTransportCA(m.transportCA); err != nil {
		if errors.Is(err, errDRTransportLeafKeyUnavailable) {
			m.logger.Warn("deferring DR transport leaf cert mint until local cluster key is available")
		} else {
			m.logger.Error("failed to mint DR transport leaf cert", "error", err)
		}
		// Non-fatal: the handler will retry and will not serve the
		// self-signed fallback cert while a DR transport CA is configured.
	}
	registerDRHandler(m.core, m.handler)

	// Start background leaf cert renewal.
	m.handler.startLeafRenewal(m.transportCA)

	m.core.replicationState.Store(uint32(consts.ReplicationDRPrimary))

	m.logger.Info("DR primary mode enabled", "cluster_id", clusterID)
	return nil
}

// DisablePrimary disables DR primary mode.
func (m *drRelationshipManager) DisablePrimary(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return fmt.Errorf("not in DR primary mode")
	}

	oldConfig := m.config
	m.config = disabledConfigPreservingPromotion(oldConfig.Promotion)
	if err := m.saveConfig(ctx); err != nil {
		m.config = oldConfig
		return err
	}

	// Unregister the DR handler from the cluster listener.
	unregisterDRHandler(m.core)

	// Detach the primary from the dispatcher; the journal continues
	// accumulating on this node so a future leader promotion is fast.
	if m.dispatcher != nil {
		m.dispatcher.clearPrimary()
	}

	// Stop the tombstone GC before releasing the primary.
	if m.primary != nil && m.primary.tombstoneGC != nil {
		m.primary.tombstoneGC.Stop()
	}

	m.primary = nil
	m.handler = nil
	m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))

	m.logger.Info("DR primary mode disabled")
	return nil
}

// EnableSecondary enables DR secondary mode using an activation token.
func (m *drRelationshipManager) EnableSecondary(ctx context.Context, token *DRActivationToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeDisabled {
		return fmt.Errorf("DR replication already enabled as %s", m.config.Mode)
	}
	if token == nil {
		return fmt.Errorf("activation token is required")
	}
	if token.ClusterID == "" {
		return fmt.Errorf("activation token missing cluster_id")
	}
	if token.RelationshipID == "" {
		return fmt.Errorf("activation token missing relationship_id")
	}
	if isStalePostPromotionActivationToken(m.config.Promotion, token) {
		return fmt.Errorf("activation token references a stale pre-promotion DR lineage; create a new relationship from the promoted authority")
	}
	normalizedPrimaryAddrs := normalizePrimaryAddrs(token.PrimaryAddrs)
	if len(normalizedPrimaryAddrs) == 0 {
		return fmt.Errorf("activation token missing primary_addrs")
	}
	primaryAddr := normalizedPrimaryAddrs[0]

	secondaryClientCert, secondaryClientKeyPEM, err := generateDRSecondaryClientCert()
	if err != nil {
		return fmt.Errorf("failed to generate DR secondary client certificate: %w", err)
	}

	oldConfig := m.config
	m.config = &DRConfig{
		Mode:                  DRModeSecondary,
		ClusterID:             token.ClusterID,
		RelationshipID:        token.RelationshipID,
		ReplSalt:              token.ReplSalt,
		PrimaryAddr:           primaryAddr,
		PrimaryAddrs:          normalizedPrimaryAddrs,
		PrimaryCACert:         token.DRTransportCACert,
		PrimaryAPIAddr:        normalizePrimaryAPIAddr(token.PrimaryAPIAddr),
		PrimaryAPICACert:      token.PrimaryAPICACert,
		PrimaryAPIServerName:  token.PrimaryAPIServerName,
		SecondaryClientCert:   secondaryClientCert,
		SecondaryClientKeyPEM: secondaryClientKeyPEM,
		Promotion:             oldConfig.Promotion,

		FallbackEnabled:          drDefaultFallbackEnabled,
		FallbackStallSeconds:     int64(drDefaultFallbackStall / time.Second),
		FallbackFailureThreshold: drDefaultFallbackFailureThreshold,
		FallbackCooldownSeconds:  int64(drDefaultFallbackCooldown / time.Second),
		FallbackMaxPerHour:       drDefaultFallbackMaxPerHour,
	}
	if len(m.config.PrimaryAPICACert) == 0 {
		m.config.PrimaryAPICACert = token.DRTransportCACert
	}
	if m.config.PrimaryAPIServerName == "" {
		m.config.PrimaryAPIServerName = deriveServerName(m.config.PrimaryAPIAddr)
	}

	if err := m.saveConfig(ctx); err != nil {
		m.config = oldConfig
		return err
	}
	if err := m.clearSecondaryCheckpointCursor(ctx); err != nil {
		m.config = oldConfig
		_ = m.saveConfig(ctx)
		return err
	}

	// Initialize the secondary.
	m.secondary = newDRReplicationSecondary(
		m.core,
		token.ReplSalt,
		token.RelationshipID,
		m.logger,
	)
	m.applySecondaryTunablesLocked()
	m.secondary.primaryCACert = token.DRTransportCACert
	if err := m.secondary.setClientCertificate(secondaryClientCert, secondaryClientKeyPEM); err != nil {
		m.config = oldConfig
		return fmt.Errorf("failed to configure DR secondary client certificate: %w", err)
	}

	m.core.replicationState.Store(uint32(consts.ReplicationDRSecondary))

	// Register the secondary's cert with the primary via the HTTP API
	// before attempting the mTLS gRPC connection. This ensures the
	// primary trusts our cluster cert.
	if token.BootstrapToken != "" {
		if err := m.registerWithPrimary(ctx, token); err != nil {
			m.logger.Warn("failed to register with primary; mTLS connection may fail",
				"error", err)
			// Continue anyway -- the operator can re-register manually if needed.
		}
	}

	// Always start the reconnect/controller loop. It retries connect/start
	// with bounded exponential backoff until stopped or promoted.
	m.startSecondaryControllerLocked()

	m.logger.Info("DR secondary mode enabled",
		"cluster_id", token.ClusterID,
		"primary_addr", primaryAddr,
		"primary_addrs", normalizedPrimaryAddrs)
	return nil
}

// DisableSecondary disables DR secondary mode.
func (m *drRelationshipManager) DisableSecondary(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}

	oldConfig := m.config
	m.config = disabledConfigPreservingPromotion(oldConfig.Promotion)
	if err := m.saveConfig(ctx); err != nil {
		m.config = oldConfig
		return err
	}
	if err := m.clearSecondaryCheckpointCursor(ctx); err != nil {
		m.config = oldConfig
		_ = m.saveConfig(ctx)
		return err
	}

	m.stopSecondaryRuntimeLocked()

	m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))

	m.logger.Info("DR secondary mode disabled")
	return nil
}

func (m *drRelationshipManager) clearSecondaryCheckpointCursor(ctx context.Context) error {
	if m == nil || m.core == nil || m.core.barrier == nil {
		return nil
	}
	if err := m.core.barrier.Delete(ctx, drCheckpointHWMPath); err != nil {
		return fmt.Errorf("failed to clear DR secondary checkpoint cursor: %w", err)
	}
	if m.core.physical != nil {
		if err := m.core.physical.Delete(ctx, drFlatAccumulatorStoragePath); err != nil {
			return fmt.Errorf("failed to clear DR secondary flat accumulator: %w", err)
		}
		if err := m.core.physical.Delete(ctx, drFlatAccumulatorCursorStoragePath); err != nil {
			return fmt.Errorf("failed to clear DR secondary flat accumulator cursor: %w", err)
		}
		keys, err := m.core.physical.List(ctx, drFlatAccumulatorDeltaStoragePath)
		if err != nil {
			return fmt.Errorf("failed to list DR secondary flat accumulator deltas: %w", err)
		}
		for _, key := range keys {
			if err := m.core.physical.Delete(ctx, drFlatAccumulatorDeltaStoragePath+key); err != nil {
				return fmt.Errorf("failed to clear DR secondary flat accumulator delta: %w", err)
			}
		}
		if err := deletePersistedLocalKIDIndex(ctx, m.core.physical); err != nil {
			return fmt.Errorf("failed to clear DR secondary local KID index: %w", err)
		}
	}
	return nil
}

// PromoteSecondary promotes this DR secondary to standalone mode.
func (m *drRelationshipManager) PromoteSecondary(ctx context.Context) error {
	return m.promoteSecondary(ctx, nil)
}

// PromoteSecondaryWithRecord promotes this DR secondary to standalone mode and
// persists the promotion lineage record.
func (m *drRelationshipManager) PromoteSecondaryWithRecord(ctx context.Context, promotion *DRPromotionRecord) error {
	return m.promoteSecondary(ctx, promotion)
}

func (m *drRelationshipManager) promoteSecondary(ctx context.Context, promotion *DRPromotionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}

	now := time.Now().UTC()
	if promotion == nil {
		promotionID, err := uuid.GenerateUUID()
		if err != nil {
			return fmt.Errorf("failed to generate promotion ID: %w", err)
		}
		promotion = &DRPromotionRecord{
			PromotionID:    promotionID,
			PromotedAt:     now.Unix(),
			PromotionClass: DRPromotionForced,
		}
	}
	promotionCopy := *promotion
	if promotionCopy.PromotionID == "" {
		promotionID, err := uuid.GenerateUUID()
		if err != nil {
			return fmt.Errorf("failed to generate promotion ID: %w", err)
		}
		promotionCopy.PromotionID = promotionID
	}
	if promotionCopy.PromotedAt == 0 {
		promotionCopy.PromotedAt = now.Unix()
	}
	if promotionCopy.PromotionClass == "" {
		promotionCopy.PromotionClass = DRPromotionForced
	}
	if promotionCopy.OldPrimaryClusterID == "" {
		promotionCopy.OldPrimaryClusterID = m.config.ClusterID
	}
	if promotionCopy.OldRelationshipID == "" {
		promotionCopy.OldRelationshipID = m.config.RelationshipID
	}
	if promotionCopy.OldSecondaryCertFingerprint == "" {
		promotionCopy.OldSecondaryCertFingerprint = certFingerprintSHA256DER(m.config.SecondaryClientCert)
	}
	inheritPromotionLineage(&promotionCopy, m.config.Promotion)
	promotion = &promotionCopy

	if m.secondary != nil {
		if m.secondaryLoopCancel != nil {
			m.secondaryLoopCancel()
			m.secondaryLoopCancel = nil
		}
		if err := m.secondary.Promote(); err != nil {
			return fmt.Errorf("failed to promote secondary: %w", err)
		}
		m.secondary = nil
	}

	// Transition to disabled (standalone) mode and clear stale upstream
	// relationship material. A promoted cluster must not resume or merge with
	// the old primary relationship after this safety boundary.
	m.config = &DRConfig{
		Mode:      DRModeDisabled,
		Promotion: promotion,
	}
	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))

	m.logger.Info("DR secondary promoted to standalone")
	return nil
}

func (m *drRelationshipManager) stopSecondaryRuntimeLocked() {
	if m.secondary == nil {
		return
	}
	if m.secondaryLoopCancel != nil {
		m.secondaryLoopCancel()
		m.secondaryLoopCancel = nil
	}
	m.secondary.Stop()
	m.secondary = nil
}

// drSecondaryAllowedPaths are the API paths that can perform writes
// even when the cluster is a DR secondary. All other write operations
// are rejected with a read-only error.
var drSecondaryAllowedPaths = []string{
	"sys/replication/dr/secondary/promote",
	"sys/replication/dr/secondary/disable",
	"sys/replication/dr/secondary/rotate-certificate",
	"sys/replication/dr/secondary/resnapshot",
	"sys/replication/dr/tuning",
	"sys/replication/dr/status",
	"sys/seal",
	"sys/step-down",
}

// isDRSecondaryAllowedPath returns true if the given canonical request path
// is allowed to perform write operations on a DR secondary.
func isDRSecondaryAllowedPath(path string) bool {
	path = strings.Trim(path, "/")
	for _, allowed := range drSecondaryAllowedPaths {
		if path == allowed {
			return true
		}
	}
	return false
}

func (m *drRelationshipManager) applyPrimaryTunablesLocked() {
	if m.primary == nil || m.config == nil {
		return
	}
	m.primary.applyRuntimeTuning(m.config)
}

func (m *drRelationshipManager) applySecondaryTunablesLocked() {
	if m.secondary == nil || m.config == nil {
		return
	}
	m.secondary.applyRuntimeTuning(m.config)
}

// Mode returns the current DR mode.
func (m *drRelationshipManager) Mode() DRMode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Mode
}

// Config returns a copy of the current DR configuration.
func (m *drRelationshipManager) Config() DRConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return *m.config
}

// Primary returns the primary replication server (nil if not primary).
func (m *drRelationshipManager) Primary() *drReplicationPrimary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.primary
}

// Secondary returns the secondary replication client (nil if not secondary).
func (m *drRelationshipManager) Secondary() *drReplicationSecondary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.secondary
}

// UpdateTuning updates persisted DR tuning values and applies them at runtime.
func (m *drRelationshipManager) UpdateTuning(ctx context.Context, apply func(cfg *DRConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil {
		return fmt.Errorf("DR config not initialized")
	}

	previous := *m.config
	next := previous
	if err := apply(&next); err != nil {
		return err
	}
	if err := validateDRTuningConfig(&next); err != nil {
		return err
	}
	*m.config = next
	if err := m.saveConfig(ctx); err != nil {
		*m.config = previous
		return err
	}
	m.applyPrimaryTunablesLocked()
	m.applySecondaryTunablesLocked()
	return nil
}

// RequestSecondaryResnapshot asks the active secondary controller to perform a
// hard-cutover resnapshot on its next reconcile cycle.
func (m *drRelationshipManager) RequestSecondaryResnapshot(reason string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.config == nil || m.config.Mode != DRModeSecondary || m.secondary == nil {
		return fmt.Errorf("not in DR secondary mode")
	}
	// Reset the monotonic checkpoint high-water mark so the resnapshot
	// can accept any checkpoint_index from the primary.
	m.secondary.resetCheckpointHighWaterMark()
	m.secondary.RequestResnapshot(reason)
	return nil
}

// Handler returns the cluster handler (nil if not primary).
func (m *drRelationshipManager) Handler() *drReplicationClusterHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.handler
}
