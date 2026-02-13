// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-uuid"
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
	drBootstrapTokenTTL           = 15 * time.Minute
	drBootstrapMaxFailedAttempts  = 5
	drBootstrapLockoutDuration    = 30 * time.Minute
	drRelationshipCleanupInterval = 24 * time.Hour

	// Persist relationship heartbeat writes at most once per interval.
	drLastSeenPersistInterval = 30 * time.Second
)

// DRMode represents the DR replication role of this cluster.
type DRMode string

const (
	DRModeDisabled  DRMode = "disabled"
	DRModePrimary   DRMode = "primary"
	DRModeSecondary DRMode = "secondary"
)

// DRConfig is the persistent DR replication configuration.
type DRConfig struct {
	// Mode is the DR role: "disabled", "primary", or "secondary".
	Mode DRMode `json:"mode"`

	// ClusterID is a unique identifier for this DR cluster pair.
	ClusterID string `json:"cluster_id"`

	// ReplSalt is the shared HMAC key for KID derivation.
	ReplSalt []byte `json:"repl_salt"`

	// PrimaryAddr is the gRPC address of the primary (set on secondary).
	PrimaryAddr string `json:"primary_addr,omitempty"`

	// RelationshipID is the active relationship ID on secondary nodes.
	RelationshipID string `json:"relationship_id,omitempty"`

	// PrimaryCACert is the primary's TLS CA certificate (DER-encoded).
	// Persisted so the secondary can re-establish mTLS after a restart.
	PrimaryCACert []byte `json:"primary_ca_cert,omitempty"`
}

// DRActivationToken contains the information a secondary needs to
// establish a DR relationship with the primary.
type DRActivationToken struct {
	// ClusterID identifies the DR cluster pair.
	ClusterID string `json:"cluster_id"`

	// RelationshipID identifies the specific DR relationship.
	RelationshipID string `json:"relationship_id"`

	// PrimaryAddr is the primary's cluster gRPC address.
	PrimaryAddr string `json:"primary_addr"`

	// PrimaryAPIAddr is the primary's HTTP API address (for the
	// secondary to register its cert before mTLS connection).
	PrimaryAPIAddr string `json:"primary_api_addr,omitempty"`

	// ReplSalt is the shared HMAC key for KID derivation.
	ReplSalt []byte `json:"repl_salt"`

	// CACert is the primary's TLS CA certificate (DER).
	CACert []byte `json:"ca_cert,omitempty"`

	// PrimaryAPICACert is the CA certificate used to validate the
	// primary API endpoint during secondary registration. If empty,
	// CACert is used.
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

type DRRelationship struct {
	RelationshipID string              `json:"relationship_id"`
	State          DRRelationshipState `json:"state"`

	// SecondaryCertFingerprint is SHA-256 over the secondary leaf cert DER.
	SecondaryCertFingerprint string `json:"secondary_cert_fingerprint,omitempty"`

	// SecondaryCACert is the secondary cluster CA/leaf cert bytes used for mTLS trust.
	SecondaryCACert []byte `json:"secondary_ca_cert,omitempty"`

	// BootstrapToken is one-time secret used for cert registration.
	BootstrapToken string `json:"bootstrap_token,omitempty"`

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

	// primary is non-nil when this cluster is a DR primary.
	primary *drReplicationPrimary

	// handler is the cluster handler for the primary side. Stored here
	// so the registration endpoint and LoadConfig can add trusted certs.
	handler *drReplicationClusterHandler

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
	return &drRelationshipManager{
		core:            core,
		logger:          logger.Named("dr-manager"),
		config:          &DRConfig{Mode: DRModeDisabled},
		lastSeenWriteAt: make(map[string]time.Time),
		latestSeenAt:    make(map[string]int64),
	}
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
	m.config = &config

	// Restore replication state based on persisted config.
	switch config.Mode {
	case DRModePrimary:
		m.logger.Info("restoring DR primary mode from config", "cluster_id", config.ClusterID)
		m.primary = NewDRReplicationPrimary(m.core, config.ReplSalt, m.logger)

		// Re-wire the change stream hook.
		if csb, ok := m.core.underlyingPhysical.(physical.ChangeStreamBackend); ok {
			csb.HookChangeStream(m.primary.OnChange)
		}

		// Re-register the cluster handler.
		m.handler = newDRReplicationClusterHandler(m.core, m.primary, m.logger)
		registerDRHandler(m.core, m.handler)

		// Restore trusted secondary certs from storage.
		if err := m.restoreRelationshipCerts(ctx); err != nil {
			m.logger.Warn("failed to restore secondary certs", "error", err)
		}

		m.core.replicationState.Store(uint32(consts.ReplicationDRPrimary))

	case DRModeSecondary:
		m.logger.Info("restoring DR secondary mode from config",
			"cluster_id", config.ClusterID,
			"primary_addr", config.PrimaryAddr)

		if config.RelationshipID == "" {
			return fmt.Errorf("invalid DR secondary config: missing relationship_id")
		}

		m.secondary = newDRReplicationSecondary(
			m.core,
			config.ReplSalt,
			config.RelationshipID,
			m.logger,
		)

		// Restore the primary's CA cert so mTLS works after restart.
		if len(config.PrimaryCACert) > 0 {
			m.secondary.primaryCACert = config.PrimaryCACert
		}

		m.core.replicationState.Store(uint32(consts.ReplicationDRSecondary))

		if config.PrimaryAddr != "" {
			m.startSecondaryControllerLocked()
		}
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

	m.config = &DRConfig{
		Mode:      DRModePrimary,
		ClusterID: clusterID,
		ReplSalt:  replSalt,
	}

	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	// Initialize the primary-side gRPC server.
	m.primary = NewDRReplicationPrimary(m.core, replSalt, m.logger)

	// Wire up the change stream hook.
	if csb, ok := m.core.underlyingPhysical.(physical.ChangeStreamBackend); ok {
		csb.HookChangeStream(m.primary.OnChange)
	}

	// Register the DR handler on the cluster listener for mTLS-secured gRPC.
	m.handler = newDRReplicationClusterHandler(m.core, m.primary, m.logger)
	registerDRHandler(m.core, m.handler)

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

	m.config = &DRConfig{Mode: DRModeDisabled}
	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	// Unregister the DR handler from the cluster listener.
	unregisterDRHandler(m.core)

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
	if token.RelationshipID == "" {
		return fmt.Errorf("activation token missing relationship_id")
	}

	m.config = &DRConfig{
		Mode:           DRModeSecondary,
		ClusterID:      token.ClusterID,
		RelationshipID: token.RelationshipID,
		ReplSalt:       token.ReplSalt,
		PrimaryAddr:    token.PrimaryAddr,
		PrimaryCACert:  token.CACert,
	}

	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	// Initialize the secondary.
	m.secondary = newDRReplicationSecondary(
		m.core,
		token.ReplSalt,
		token.RelationshipID,
		m.logger,
	)
	m.secondary.primaryCACert = token.CACert

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
		"primary_addr", token.PrimaryAddr)
	return nil
}

// DisableSecondary disables DR secondary mode.
func (m *drRelationshipManager) DisableSecondary(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}

	if m.secondary != nil {
		if m.secondaryLoopCancel != nil {
			m.secondaryLoopCancel()
			m.secondaryLoopCancel = nil
		}
		m.secondary.Stop()
		m.secondary = nil
	}

	m.config = &DRConfig{Mode: DRModeDisabled}
	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))

	m.logger.Info("DR secondary mode disabled")
	return nil
}

// PromoteSecondary promotes this DR secondary to standalone primary.
func (m *drRelationshipManager) PromoteSecondary(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}

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

	// Transition to disabled (standalone) mode. The operator can
	// re-enable as a primary if they want to accept new secondaries.
	m.config.Mode = DRModeDisabled
	if err := m.saveConfig(ctx); err != nil {
		return err
	}

	m.core.replicationState.Store(uint32(consts.ReplicationDRDisabled))

	m.logger.Info("DR secondary promoted to standalone")
	return nil
}

// drSecondaryAllowedPaths are the API paths that can perform writes
// even when the cluster is a DR secondary. All other write operations
// are rejected with a read-only error.
var drSecondaryAllowedPaths = []string{
	"sys/replication/dr/secondary/promote",
	"sys/replication/dr/secondary/disable",
	"sys/replication/dr/status",
	"sys/seal",
	"sys/step-down",
}

// isDRSecondaryAllowedPath returns true if the given request path is
// allowed to perform write operations on a DR secondary. Uses suffix
// matching so that namespace-prefixed paths (e.g. "ns1/sys/seal") are
// also correctly matched.
func isDRSecondaryAllowedPath(path string) bool {
	for _, allowed := range drSecondaryAllowedPaths {
		if path == allowed {
			return true
		}
		// Namespace-aware: check if path ends with "/"+allowed
		// (e.g. "ns1/sys/replication/dr/secondary/promote")
		if strings.HasSuffix(path, "/"+allowed) {
			return true
		}
	}
	return false
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

// Handler returns the cluster handler (nil if not primary).
func (m *drRelationshipManager) Handler() *drReplicationClusterHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.handler
}
