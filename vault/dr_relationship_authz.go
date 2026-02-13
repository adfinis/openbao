// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (m *drRelationshipManager) RevokeRelationship(ctx context.Context, relationshipID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return fmt.Errorf("not in DR primary mode")
	}

	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return err
	}
	if rel.State == DRRelationshipStateRevoked {
		return nil
	}

	rel.State = DRRelationshipStateRevoked
	rel.BootstrapToken = ""
	rel.LastSeenAt = time.Now().UTC().Unix()
	rel.RevokedAt = rel.LastSeenAt
	rel.LastError = ""
	if err := m.saveRelationship(ctx, rel); err != nil {
		return fmt.Errorf("failed to persist revoked relationship: %w", err)
	}
	m.latestSeenAt[relationshipID] = rel.LastSeenAt
	m.lastSeenWriteAt[relationshipID] = time.Now().UTC()

	if m.handler != nil {
		m.handler.RemoveTrustedRelationship(relationshipID)
	}
	if m.primary != nil {
		m.primary.RevokeRelationship(relationshipID)
	}
	m.logger.Info("revoked DR relationship", "relationship_id", relationshipID)
	return nil
}

func (m *drRelationshipManager) ValidateRelationshipAccess(relationshipID, fingerprint string, allowedStates ...DRRelationshipState) (*DRRelationship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}

	ctx := context.Background()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}

	if rel.State == DRRelationshipStateRevoked {
		return nil, fmt.Errorf("relationship %q is revoked", relationshipID)
	}

	if len(allowedStates) > 0 {
		allowed := false
		for _, st := range allowedStates {
			if rel.State == st {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("relationship %q is in state %q", relationshipID, rel.State)
		}
	}

	if rel.SecondaryCertFingerprint == "" {
		return nil, fmt.Errorf("relationship %q has no registered certificate", relationshipID)
	}
	if !strings.EqualFold(rel.SecondaryCertFingerprint, fingerprint) {
		return nil, fmt.Errorf("certificate fingerprint mismatch for relationship %q", relationshipID)
	}

	nowTime := time.Now().UTC()
	now := nowTime.Unix()
	updated := false
	m.latestSeenAt[relationshipID] = now
	if rel.LastSeenAt != now && (m.lastSeenWriteAt[relationshipID].IsZero() || nowTime.Sub(m.lastSeenWriteAt[relationshipID]) >= drLastSeenPersistInterval) {
		rel.LastSeenAt = now
		m.lastSeenWriteAt[relationshipID] = nowTime
		updated = true
	}
	if rel.State == DRRelationshipStateRegistered {
		rel.State = DRRelationshipStateActive
		updated = true
	}
	if updated {
		if err := m.saveRelationship(ctx, rel); err != nil {
			m.logger.Warn("failed to persist relationship access update", "relationship_id", relationshipID, "error", err)
		}
	}

	return rel, nil
}

func (m *drRelationshipManager) MarkRelationshipSeen(relationshipID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	nowUnix := now.Unix()
	m.latestSeenAt[relationshipID] = nowUnix
	lastWrite := m.lastSeenWriteAt[relationshipID]
	if !lastWrite.IsZero() && now.Sub(lastWrite) < drLastSeenPersistInterval {
		return
	}

	ctx := context.Background()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return
	}
	rel.LastSeenAt = nowUnix
	if err := m.saveRelationship(ctx, rel); err != nil {
		m.logger.Warn("failed to persist relationship last_seen", "relationship_id", relationshipID, "error", err)
		return
	}
	m.lastSeenWriteAt[relationshipID] = now
}

func (m *drRelationshipManager) MarkRelationshipActive(relationshipID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	nowUnix := now.Unix()
	m.latestSeenAt[relationshipID] = nowUnix

	ctx := context.Background()
	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return
	}
	if rel.State == DRRelationshipStateRegistered {
		rel.State = DRRelationshipStateActive
	}
	rel.LastSeenAt = nowUnix
	rel.LastError = ""
	if err := m.saveRelationship(ctx, rel); err != nil {
		m.logger.Warn("failed to persist relationship state", "relationship_id", relationshipID, "error", err)
		return
	}
	m.lastSeenWriteAt[relationshipID] = now
}
