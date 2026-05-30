// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
)

const (
	drCredentialRotationInitiate = "initiate"
	drCredentialRotationConfirm  = "confirm"
)

type DRSecondaryCredentialRotationResult struct {
	RelationshipID      string
	OperationID         string
	OldFingerprint      string
	NewFingerprint      string
	PendingResumed      bool
	RotationStartedAt   int64
	RotationCompletedAt int64
}

func drCredentialRotationPayload(kind, relationshipID, operationID string, issuedAt int64, certDER []byte) []byte {
	certHash := sha256.Sum256(certDER)
	return []byte(fmt.Sprintf(
		"openbao-dr-secondary-credential-rotation-v1\nkind:%s\nrelationship_id:%s\noperation_id:%s\nissued_at:%d\nsecondary_cert_sha256:%x\n",
		kind,
		relationshipID,
		operationID,
		issuedAt,
		certHash[:],
	))
}

func signDRCredentialRotationPayload(cert *tls.Certificate, kind, relationshipID, operationID string, issuedAt int64, certDER []byte) ([]byte, error) {
	if cert == nil {
		return nil, fmt.Errorf("missing DR secondary client certificate")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key == nil {
		return nil, fmt.Errorf("DR secondary client key must be ECDSA")
	}
	payload := drCredentialRotationPayload(kind, relationshipID, operationID, issuedAt, certDER)
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, key, digest[:])
}

func drCredentialRotationRequestBody(relationshipID, operationID string, issuedAt int64, certDER, signature []byte) map[string]interface{} {
	return map[string]interface{}{
		"relationship_id":   relationshipID,
		"operation_id":      operationID,
		"issued_at":         issuedAt,
		"secondary_ca_cert": base64.StdEncoding.EncodeToString(certDER),
		"signature":         base64.StdEncoding.EncodeToString(signature),
	}
}

func verifyDRCredentialRotationSignature(cert *x509.Certificate, kind, relationshipID, operationID string, issuedAt int64, certDER, signature []byte) error {
	if cert == nil {
		return fmt.Errorf("missing DR secondary client certificate")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub == nil {
		return fmt.Errorf("DR secondary client certificate public key must be ECDSA")
	}
	payload := drCredentialRotationPayload(kind, relationshipID, operationID, issuedAt, certDER)
	digest := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(pub, digest[:], signature) {
		return fmt.Errorf("credential rotation signature verification failed")
	}
	return nil
}

var parseDRCredentialRotationCertificate = parseAndValidateDRCredentialRotationCertificate

func validateDRCredentialRotationRequestShape(relationshipID, operationID string, issuedAt int64, secondaryCACert, signature []byte, now time.Time) error {
	if relationshipID == "" {
		return fmt.Errorf("relationship_id is required")
	}
	if _, err := uuid.ParseUUID(relationshipID); err != nil {
		return fmt.Errorf("relationship_id must be a valid UUID")
	}
	if operationID == "" {
		return fmt.Errorf("operation_id is required")
	}
	if _, err := uuid.ParseUUID(operationID); err != nil {
		return fmt.Errorf("operation_id must be a valid UUID")
	}
	if issuedAt <= 0 {
		return fmt.Errorf("issued_at is required")
	}
	issued := time.Unix(issuedAt, 0).UTC()
	if issued.Before(now.Add(-drCredentialRotationMaxSkew)) || issued.After(now.Add(drCredentialRotationMaxSkew)) {
		return fmt.Errorf("credential rotation request timestamp is outside the allowed skew")
	}
	if len(secondaryCACert) == 0 {
		return fmt.Errorf("secondary_ca_cert is required")
	}
	if len(secondaryCACert) > drBootstrapMaxCertDERBytes {
		return fmt.Errorf("secondary_ca_cert exceeds maximum DER size %d", drBootstrapMaxCertDERBytes)
	}
	if len(signature) == 0 {
		return fmt.Errorf("signature is required")
	}
	if len(signature) > drCredentialRotationMaxSigLen {
		return fmt.Errorf("signature exceeds maximum size %d", drCredentialRotationMaxSigLen)
	}
	return nil
}

func parseAndValidateDRCredentialRotationCertificate(secondaryCACert []byte, now time.Time) (*x509.Certificate, error) {
	cert, err := x509.ParseCertificate(secondaryCACert)
	if err != nil {
		return nil, fmt.Errorf("invalid secondary CA certificate: %w", err)
	}
	if err := validateDRSecondaryClientCert(cert, now); err != nil {
		return nil, fmt.Errorf("invalid secondary CA certificate: %w", err)
	}
	return cert, nil
}

func pendingCredentialRotationExpired(rel *DRRelationship, now time.Time) bool {
	if rel == nil || rel.PendingSecondaryCertFingerprint == "" {
		return false
	}
	if rel.PendingRotationStartedAt == 0 {
		return true
	}
	return time.Unix(rel.PendingRotationStartedAt, 0).Add(drCredentialRotationPendingTTL).Before(now)
}

func clearPendingCredentialRotation(rel *DRRelationship) {
	if rel == nil {
		return
	}
	rel.PendingSecondaryCertFingerprint = ""
	rel.PendingSecondaryCACert = nil
	rel.PendingRotationOperationID = ""
	rel.PendingRotationStartedAt = 0
}

func localPendingSecondaryCredentialRotationExpired(config DRConfig, now time.Time) bool {
	if len(config.PendingSecondaryClientCert) == 0 && len(config.PendingSecondaryClientKeyPEM) == 0 {
		return false
	}
	if config.PendingSecondaryRotationStartedAt == 0 {
		return true
	}
	return time.Unix(config.PendingSecondaryRotationStartedAt, 0).Add(drCredentialRotationPendingTTL).Before(now)
}

func clearLocalPendingSecondaryCredentialRotation(config *DRConfig) {
	if config == nil {
		return
	}
	config.PendingSecondaryClientCert = nil
	config.PendingSecondaryClientKeyPEM = nil
	config.PendingSecondaryRotationOperation = ""
	config.PendingSecondaryRotationStartedAt = 0
}

func (m *drRelationshipManager) clearExpiredLocalPendingSecondaryCredentialRotation(ctx context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || !localPendingSecondaryCredentialRotationExpired(*m.config, now) {
		return nil
	}
	clearLocalPendingSecondaryCredentialRotation(m.config)
	return m.saveConfig(ctx)
}

func (m *drRelationshipManager) secondaryCredentialRotationSnapshot() (DRConfig, *tls.Certificate, *tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.config == nil || m.config.Mode != DRModeSecondary {
		return DRConfig{}, nil, nil, fmt.Errorf("not in DR secondary mode")
	}
	config := *m.config
	if config.RelationshipID == "" {
		return DRConfig{}, nil, nil, fmt.Errorf("DR secondary config missing relationship_id")
	}
	if config.PrimaryAPIAddr == "" {
		return DRConfig{}, nil, nil, fmt.Errorf("DR secondary config missing primary API address; replace the relationship from the current authority")
	}
	if primaryAPIAddrRequiresTLS(config.PrimaryAPIAddr) && len(config.PrimaryAPICACert) == 0 {
		return DRConfig{}, nil, nil, fmt.Errorf("DR secondary config missing primary API CA certificate; replace the relationship from the current authority")
	}
	if len(config.SecondaryClientCert) == 0 || len(config.SecondaryClientKeyPEM) == 0 {
		return DRConfig{}, nil, nil, fmt.Errorf("DR secondary config missing current client credential")
	}
	currentCert, err := parseDRSecondaryClientCert(config.SecondaryClientCert, config.SecondaryClientKeyPEM)
	if err != nil {
		return DRConfig{}, nil, nil, fmt.Errorf("invalid current DR secondary client credential: %w", err)
	}
	var pendingCert *tls.Certificate
	if len(config.PendingSecondaryClientCert) > 0 || len(config.PendingSecondaryClientKeyPEM) > 0 {
		pendingCert, err = parseDRSecondaryClientCert(config.PendingSecondaryClientCert, config.PendingSecondaryClientKeyPEM)
		if err != nil {
			return DRConfig{}, nil, nil, fmt.Errorf("invalid pending DR secondary client credential: %w", err)
		}
	}
	return config, currentCert, pendingCert, nil
}

func (m *drRelationshipManager) persistPendingSecondaryCredentialRotation(ctx context.Context, relationshipID, oldFingerprint, operationID string, startedAt int64, certDER, keyPEM []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}
	if m.config.RelationshipID != relationshipID {
		return fmt.Errorf("DR secondary relationship changed during credential rotation")
	}
	if got := certFingerprintSHA256DER(m.config.SecondaryClientCert); !strings.EqualFold(got, oldFingerprint) {
		return fmt.Errorf("DR secondary current credential changed during credential rotation")
	}
	if m.config.PendingSecondaryRotationOperation != "" && m.config.PendingSecondaryRotationOperation != operationID {
		return fmt.Errorf("DR secondary already has a pending credential rotation")
	}

	oldConfig := *m.config
	m.config.PendingSecondaryClientCert = append([]byte(nil), certDER...)
	m.config.PendingSecondaryClientKeyPEM = append([]byte(nil), keyPEM...)
	m.config.PendingSecondaryRotationOperation = operationID
	m.config.PendingSecondaryRotationStartedAt = startedAt
	if err := m.saveConfig(ctx); err != nil {
		*m.config = oldConfig
		return err
	}
	return nil
}

func (m *drRelationshipManager) finalizeLocalSecondaryCredentialRotation(ctx context.Context, relationshipID, operationID, oldFingerprint, newFingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModeSecondary {
		return fmt.Errorf("not in DR secondary mode")
	}
	if m.config.RelationshipID != relationshipID {
		return fmt.Errorf("DR secondary relationship changed during credential rotation")
	}
	if m.config.PendingSecondaryRotationOperation != operationID {
		return fmt.Errorf("DR secondary pending credential rotation operation mismatch")
	}
	if got := certFingerprintSHA256DER(m.config.SecondaryClientCert); !strings.EqualFold(got, oldFingerprint) {
		return fmt.Errorf("DR secondary current credential changed during credential rotation")
	}
	if got := certFingerprintSHA256DER(m.config.PendingSecondaryClientCert); !strings.EqualFold(got, newFingerprint) {
		return fmt.Errorf("DR secondary pending credential changed during credential rotation")
	}

	newCertDER := append([]byte(nil), m.config.PendingSecondaryClientCert...)
	newKeyPEM := append([]byte(nil), m.config.PendingSecondaryClientKeyPEM...)
	oldConfig := *m.config
	m.config.SecondaryClientCert = newCertDER
	m.config.SecondaryClientKeyPEM = newKeyPEM
	clearLocalPendingSecondaryCredentialRotation(m.config)
	if err := m.saveConfig(ctx); err != nil {
		*m.config = oldConfig
		return err
	}
	if m.secondary != nil {
		if err := m.secondary.ReconnectWithClientCertificate(newCertDER, newKeyPEM); err != nil {
			return err
		}
	}
	return nil
}

func (m *drRelationshipManager) RotateSecondaryCredential(ctx context.Context) (*DRSecondaryCredentialRotationResult, error) {
	now := time.Now().UTC()
	config, currentCert, pendingCert, err := m.secondaryCredentialRotationSnapshot()
	if err != nil {
		return nil, err
	}
	if localPendingSecondaryCredentialRotationExpired(config, now) {
		if err := m.clearExpiredLocalPendingSecondaryCredentialRotation(ctx, now); err != nil {
			return nil, fmt.Errorf("failed to clear expired local pending credential rotation: %w", err)
		}
		return nil, fmt.Errorf("local pending credential rotation expired; retry rotation")
	}

	apiConfig := drPrimaryAPIClientConfigFromDRConfig(config)
	relationshipID := config.RelationshipID
	oldFP := certFingerprintSHA256(currentCert.Leaf)
	var (
		newCertDER     []byte
		newKeyPEM      []byte
		newCert        *tls.Certificate
		operationID    string
		startedAt      int64
		pendingResumed bool
	)

	if pendingCert != nil {
		newCert = pendingCert
		newCertDER = append([]byte(nil), config.PendingSecondaryClientCert...)
		newKeyPEM = append([]byte(nil), config.PendingSecondaryClientKeyPEM...)
		operationID = config.PendingSecondaryRotationOperation
		startedAt = config.PendingSecondaryRotationStartedAt
		pendingResumed = true
	} else {
		newCertDER, newKeyPEM, err = generateDRSecondaryClientCert()
		if err != nil {
			return nil, fmt.Errorf("failed to generate new DR secondary client credential: %w", err)
		}
		newCert, err = parseDRSecondaryClientCert(newCertDER, newKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to parse new DR secondary client credential: %w", err)
		}
		operationID, err = uuid.GenerateUUID()
		if err != nil {
			return nil, fmt.Errorf("failed to generate credential rotation operation ID: %w", err)
		}
		startedAt = now.Unix()
		initSig, err := signDRCredentialRotationPayload(currentCert, drCredentialRotationInitiate, relationshipID, operationID, startedAt, newCertDER)
		if err != nil {
			return nil, fmt.Errorf("failed to sign credential rotation stage request: %w", err)
		}
		if err := postDRPrimaryAPIJSON(ctx, apiConfig, "replication/dr/primary/rotate-secondary-certificate", drCredentialRotationRequestBody(relationshipID, operationID, startedAt, newCertDER, initSig)); err != nil {
			return nil, fmt.Errorf("failed to stage credential rotation on primary: %w", err)
		}
		if err := m.persistPendingSecondaryCredentialRotation(ctx, relationshipID, oldFP, operationID, startedAt, newCertDER, newKeyPEM); err != nil {
			return nil, fmt.Errorf("failed to persist local pending credential rotation: %w", err)
		}
	}

	newFP := certFingerprintSHA256(newCert.Leaf)
	confirmIssuedAt := time.Now().UTC().Unix()
	confirmSig, err := signDRCredentialRotationPayload(newCert, drCredentialRotationConfirm, relationshipID, operationID, confirmIssuedAt, newCertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to sign credential rotation confirm request: %w", err)
	}
	if err := postDRPrimaryAPIJSON(ctx, apiConfig, "replication/dr/primary/confirm-secondary-certificate", drCredentialRotationRequestBody(relationshipID, operationID, confirmIssuedAt, newCertDER, confirmSig)); err != nil {
		return nil, fmt.Errorf("failed to confirm credential rotation on primary: %w", err)
	}
	if err := m.finalizeLocalSecondaryCredentialRotation(ctx, relationshipID, operationID, oldFP, newFP); err != nil {
		return nil, fmt.Errorf("failed to finalize local credential rotation: %w", err)
	}

	return &DRSecondaryCredentialRotationResult{
		RelationshipID:      relationshipID,
		OperationID:         operationID,
		OldFingerprint:      oldFP,
		NewFingerprint:      newFP,
		PendingResumed:      pendingResumed,
		RotationStartedAt:   startedAt,
		RotationCompletedAt: time.Now().UTC().Unix(),
	}, nil
}

// InitiateSecondaryCredentialRotation stages a new secondary relationship
// certificate. The request must be signed by the current relationship
// credential, so this path is usable only while the existing credential is
// still healthy.
func (m *drRelationshipManager) InitiateSecondaryCredentialRotation(ctx context.Context, relationshipID, operationID string, issuedAt int64, secondaryCACert, signature []byte, sourceIP string) (*DRRelationship, error) {
	now := time.Now().UTC()
	if err := validateDRCredentialRotationRequestShape(relationshipID, operationID, issuedAt, secondaryCACert, signature, now); err != nil {
		return nil, err
	}
	requestFP := certFingerprintSHA256RawDER(secondaryCACert)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	if m.handler == nil {
		return nil, fmt.Errorf("DR handler not initialized")
	}

	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if rel.State != DRRelationshipStateActive {
		return nil, fmt.Errorf("relationship %q must be active for credential rotation", relationshipID)
	}
	if rel.SecondaryCertFingerprint == "" || len(rel.SecondaryCACert) == 0 {
		return nil, fmt.Errorf("relationship %q has no current credential", relationshipID)
	}
	if rel.PendingSecondaryCertFingerprint != "" {
		if pendingCredentialRotationExpired(rel, now) {
			clearPendingCredentialRotation(rel)
			if err := m.saveRelationship(ctx, rel); err != nil {
				return nil, fmt.Errorf("failed to clear expired pending credential rotation: %w", err)
			}
			m.handler.RemoveTrustedRelationship(relationshipID)
			if currentCert, parseErr := x509.ParseCertificate(rel.SecondaryCACert); parseErr == nil {
				m.handler.AddTrustedCert(relationshipID, currentCert)
			}
		} else {
			if rel.PendingRotationOperationID == operationID && strings.EqualFold(rel.PendingSecondaryCertFingerprint, requestFP) {
				pendingCert, err := parseDRCredentialRotationCertificate(rel.PendingSecondaryCACert, now)
				if err != nil {
					return nil, err
				}
				m.handler.AddTrustedCert(relationshipID, pendingCert)
				return rel, nil
			}
			return nil, fmt.Errorf("relationship %q already has pending credential rotation", relationshipID)
		}
	}
	currentCert, err := x509.ParseCertificate(rel.SecondaryCACert)
	if err != nil {
		return nil, fmt.Errorf("stored relationship certificate is invalid: %w", err)
	}
	if err := verifyDRCredentialRotationSignature(currentCert, drCredentialRotationInitiate, relationshipID, operationID, issuedAt, secondaryCACert, signature); err != nil {
		return nil, err
	}
	newCert, err := parseDRCredentialRotationCertificate(secondaryCACert, now)
	if err != nil {
		return nil, err
	}
	newFP := certFingerprintSHA256(newCert)
	if isStalePostPromotionFingerprint(m.config.Promotion, newFP) {
		return nil, fmt.Errorf("certificate fingerprint belongs to stale pre-promotion DR lineage")
	}
	if existingRel, fpErr := m.findRelationshipByFingerprint(ctx, newFP); fpErr != nil {
		return nil, fpErr
	} else if existingRel != nil {
		return nil, fmt.Errorf("certificate fingerprint already bound to relationship %q", existingRel.RelationshipID)
	}

	rel.PendingSecondaryCACert = append([]byte(nil), secondaryCACert...)
	rel.PendingSecondaryCertFingerprint = newFP
	rel.PendingRotationOperationID = operationID
	rel.PendingRotationStartedAt = now.Unix()
	rel.LastSeenAt = now.Unix()
	rel.LastError = ""
	if rel.CredentialGeneration == 0 {
		rel.CredentialGeneration = 1
	}
	if err := m.saveRelationship(ctx, rel); err != nil {
		return nil, fmt.Errorf("failed to persist pending credential rotation: %w", err)
	}

	m.handler.AddTrustedCert(relationshipID, newCert)
	m.latestSeenAt[relationshipID] = rel.LastSeenAt
	m.lastSeenWriteAt[relationshipID] = now
	m.logger.Info("staged DR secondary credential rotation",
		"relationship_id", relationshipID,
		"operation_id", operationID,
		"credential_generation", rel.CredentialGeneration,
		"source_ip", sourceIP)
	return rel, nil
}

// ConfirmSecondaryCredentialRotation finalizes a staged certificate rotation.
// The request must be signed by the pending credential, proving the secondary
// has installed the new private key before the primary drops trust in the old
// certificate.
func (m *drRelationshipManager) ConfirmSecondaryCredentialRotation(ctx context.Context, relationshipID, operationID string, issuedAt int64, secondaryCACert, signature []byte, sourceIP string) (*DRRelationship, error) {
	now := time.Now().UTC()
	if err := validateDRCredentialRotationRequestShape(relationshipID, operationID, issuedAt, secondaryCACert, signature, now); err != nil {
		return nil, err
	}
	requestFP := certFingerprintSHA256RawDER(secondaryCACert)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	if m.handler == nil {
		return nil, fmt.Errorf("DR handler not initialized")
	}

	rel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	if rel.State != DRRelationshipStateActive {
		return nil, fmt.Errorf("relationship %q must be active for credential rotation", relationshipID)
	}
	if rel.PendingSecondaryCertFingerprint == "" || len(rel.PendingSecondaryCACert) == 0 {
		if strings.EqualFold(rel.SecondaryCertFingerprint, requestFP) {
			currentCert, err := parseDRCredentialRotationCertificate(rel.SecondaryCACert, now)
			if err != nil {
				return nil, err
			}
			if err := verifyDRCredentialRotationSignature(currentCert, drCredentialRotationConfirm, relationshipID, operationID, issuedAt, secondaryCACert, signature); err != nil {
				return nil, err
			}
			return rel, nil
		}
		return nil, fmt.Errorf("relationship %q has no pending credential rotation", relationshipID)
	}
	if pendingCredentialRotationExpired(rel, now) {
		clearPendingCredentialRotation(rel)
		if err := m.saveRelationship(ctx, rel); err != nil {
			return nil, fmt.Errorf("failed to clear expired pending credential rotation: %w", err)
		}
		m.handler.RemoveTrustedRelationship(relationshipID)
		if currentCert, parseErr := x509.ParseCertificate(rel.SecondaryCACert); parseErr == nil {
			m.handler.AddTrustedCert(relationshipID, currentCert)
		}
		return nil, fmt.Errorf("pending credential rotation expired")
	}
	if rel.PendingRotationOperationID != operationID {
		return nil, fmt.Errorf("credential rotation operation_id mismatch")
	}
	if !strings.EqualFold(rel.PendingSecondaryCertFingerprint, requestFP) {
		return nil, fmt.Errorf("pending credential fingerprint mismatch")
	}
	newCert, err := parseDRCredentialRotationCertificate(rel.PendingSecondaryCACert, now)
	if err != nil {
		return nil, err
	}
	newFP := certFingerprintSHA256(newCert)
	if !strings.EqualFold(rel.PendingSecondaryCertFingerprint, newFP) {
		return nil, fmt.Errorf("pending credential fingerprint mismatch")
	}
	if err := verifyDRCredentialRotationSignature(newCert, drCredentialRotationConfirm, relationshipID, operationID, issuedAt, secondaryCACert, signature); err != nil {
		return nil, err
	}

	oldFP := rel.SecondaryCertFingerprint
	newCertDER := append([]byte(nil), rel.PendingSecondaryCACert...)
	rel.PreviousSecondaryCertFingerprint = oldFP
	rel.SecondaryCertFingerprint = newFP
	rel.SecondaryCACert = newCertDER
	rel.PendingSecondaryCertFingerprint = ""
	rel.PendingSecondaryCACert = nil
	rel.PendingRotationOperationID = ""
	rel.PendingRotationStartedAt = 0
	if rel.CredentialGeneration == 0 {
		rel.CredentialGeneration = 1
	}
	rel.CredentialGeneration++
	rel.RotatedAt = now.Unix()
	rel.LastSeenAt = rel.RotatedAt
	rel.LastError = ""
	if err := m.saveRelationship(ctx, rel); err != nil {
		return nil, fmt.Errorf("failed to persist credential rotation: %w", err)
	}

	m.handler.RemoveTrustedRelationship(relationshipID)
	m.handler.AddTrustedCert(relationshipID, newCert)
	if m.primary != nil {
		m.primary.TerminateRelationshipStreams(relationshipID)
	}
	m.latestSeenAt[relationshipID] = rel.LastSeenAt
	m.lastSeenWriteAt[relationshipID] = now
	m.logger.Info("finalized DR secondary credential rotation",
		"relationship_id", relationshipID,
		"operation_id", operationID,
		"credential_generation", rel.CredentialGeneration,
		"source_ip", sourceIP)
	return rel, nil
}
