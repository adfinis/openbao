// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
)

// restoreRelationshipCerts loads persisted relationship certs and restores
// them into the cluster handler trust store.
func (m *drRelationshipManager) restoreRelationshipCerts(ctx context.Context) error {
	if m.handler == nil {
		return nil
	}

	keys, err := m.core.barrier.List(ctx, drRelationshipsPath)
	if err != nil {
		return fmt.Errorf("failed to list relationship entries: %w", err)
	}

	for _, key := range keys {
		entry, err := m.core.barrier.Get(ctx, drRelationshipsPath+key)
		if err != nil {
			m.logger.Warn("failed to read relationship entry", "key", key, "error", err)
			continue
		}
		if entry == nil {
			continue
		}

		var rel DRRelationship
		if err := json.Unmarshal(entry.Value, &rel); err != nil {
			m.logger.Warn("failed to unmarshal relationship info", "key", key, "error", err)
			continue
		}

		if rel.State == DRRelationshipStateRevoked || len(rel.SecondaryCACert) == 0 {
			continue
		}

		cert, err := x509.ParseCertificate(rel.SecondaryCACert)
		if err != nil {
			m.logger.Warn("failed to parse relationship cert", "relationship_id", rel.RelationshipID, "error", err)
			continue
		}

		m.handler.AddTrustedCert(rel.RelationshipID, cert)
		m.logger.Info("restored trusted relationship cert", "relationship_id", rel.RelationshipID)
	}

	return nil
}

func (m *drRelationshipManager) relationshipKey(id string) string {
	return drRelationshipsPath + id
}

func (m *drRelationshipManager) normalizeRelationshipLocked(rel *DRRelationship) (bool, []string) {
	changed := false
	var warnings []string
	now := time.Now().UTC().Unix()

	if rel.RelationshipID == "" {
		changed = true
		warnings = append(warnings, "missing relationship_id")
	}

	if rel.State == "" {
		rel.State = DRRelationshipStatePending
		changed = true
		warnings = append(warnings, "missing state")
	}

	if rel.CreatedAt == 0 {
		if rel.LastSeenAt > 0 {
			rel.CreatedAt = rel.LastSeenAt
		} else {
			rel.CreatedAt = now
		}
		changed = true
		warnings = append(warnings, "missing created_at")
	}

	// Backfill pending expiry semantics for legacy records.
	if rel.State == DRRelationshipStatePending && rel.ExpiresAt == 0 {
		if rel.CreatedAt > 0 {
			rel.ExpiresAt = rel.CreatedAt + int64(drBootstrapTokenTTL/time.Second)
		} else {
			// Legacy record with no creation timestamp: mark immediately expired.
			rel.ExpiresAt = now - 1
		}
		changed = true
		warnings = append(warnings, "missing expires_at")
	}

	if rel.State == DRRelationshipStateRevoked && rel.RevokedAt == 0 {
		rel.RevokedAt = now
		changed = true
	}

	if rel.LastSeenAt > 0 {
		if seen, ok := m.latestSeenAt[rel.RelationshipID]; ok && seen > rel.LastSeenAt {
			rel.LastSeenAt = seen
		}
	}

	return changed, warnings
}

func (m *drRelationshipManager) maybeCleanupExpiredPendingRelationshipsLocked(ctx context.Context, now time.Time) {
	if !m.lastPendingCleanupAt.IsZero() && now.Sub(m.lastPendingCleanupAt) < drRelationshipCleanupInterval {
		return
	}
	m.lastPendingCleanupAt = now

	keys, err := m.core.barrier.List(ctx, drRelationshipsPath)
	if err != nil {
		m.logger.Warn("failed to run pending relationship cleanup list", "error", err)
		return
	}

	removed := 0
	nowUnix := now.Unix()
	for _, key := range keys {
		entry, err := m.core.barrier.Get(ctx, drRelationshipsPath+key)
		if err != nil || entry == nil {
			continue
		}
		var rel DRRelationship
		if err := json.Unmarshal(entry.Value, &rel); err != nil {
			continue
		}
		if rel.RelationshipID == "" {
			rel.RelationshipID = key
		}
		changed, _ := m.normalizeRelationshipLocked(&rel)
		if changed {
			if err := m.saveRelationship(ctx, &rel); err != nil {
				m.logger.Warn("failed to persist normalized relationship during cleanup", "relationship_id", rel.RelationshipID, "error", err)
			}
		}
		if rel.State == DRRelationshipStatePending && rel.ExpiresAt > 0 && rel.ExpiresAt <= nowUnix {
			if err := m.core.barrier.Delete(ctx, drRelationshipsPath+key); err != nil {
				m.logger.Warn("failed to delete expired pending relationship", "relationship_id", rel.RelationshipID, "error", err)
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		m.logger.Info("cleaned up expired pending DR relationships", "count", removed)
	}
}

func (m *drRelationshipManager) loadRelationship(ctx context.Context, relationshipID string) (*DRRelationship, error) {
	entry, err := m.core.barrier.Get(ctx, m.relationshipKey(relationshipID))
	if err != nil {
		return nil, fmt.Errorf("failed to read relationship %q: %w", relationshipID, err)
	}
	if entry == nil {
		return nil, fmt.Errorf("relationship %q not found", relationshipID)
	}

	var rel DRRelationship
	if err := json.Unmarshal(entry.Value, &rel); err != nil {
		return nil, fmt.Errorf("failed to decode relationship %q: %w", relationshipID, err)
	}
	if rel.RelationshipID == "" {
		rel.RelationshipID = relationshipID
	}
	changed, warnings := m.normalizeRelationshipLocked(&rel)
	for _, warning := range warnings {
		m.logger.Warn("detected legacy/invalid relationship record during load",
			"relationship_id", relationshipID,
			"warning", warning)
	}
	if changed {
		if err := m.saveRelationship(ctx, &rel); err != nil {
			m.logger.Warn("failed to persist normalized relationship record", "relationship_id", relationshipID, "error", err)
		}
	}
	return &rel, nil
}

func (m *drRelationshipManager) saveRelationship(ctx context.Context, rel *DRRelationship) error {
	relBytes, err := json.Marshal(rel)
	if err != nil {
		return fmt.Errorf("failed to marshal relationship %q: %w", rel.RelationshipID, err)
	}
	return m.core.barrier.Put(ctx, &logical.StorageEntry{
		Key:   m.relationshipKey(rel.RelationshipID),
		Value: relBytes,
	})
}

func (m *drRelationshipManager) ListRelationships(ctx context.Context) ([]*DRRelationship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}

	m.maybeCleanupExpiredPendingRelationshipsLocked(ctx, time.Now().UTC())

	keys, err := m.core.barrier.List(ctx, drRelationshipsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list relationships: %w", err)
	}

	relationships := make([]*DRRelationship, 0, len(keys))
	for _, key := range keys {
		rel, err := m.loadRelationship(ctx, key)
		if err != nil {
			m.logger.Warn("skipping unreadable relationship record", "relationship_id", key, "error", err)
			continue
		}
		relationships = append(relationships, rel)
	}
	return relationships, nil
}

func (m *drRelationshipManager) GetRelationship(ctx context.Context, relationshipID string) (*DRRelationship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	m.maybeCleanupExpiredPendingRelationshipsLocked(ctx, time.Now().UTC())
	return m.loadRelationship(ctx, relationshipID)
}
