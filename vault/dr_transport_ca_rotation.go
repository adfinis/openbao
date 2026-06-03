// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
)

type DRTransportCARotationResult struct {
	OperationID   string
	ActiveKeyID   string
	StagedKeyID   string
	PreviousKeyID string
	StartedAt     int64
	ActivatedAt   int64
	RetiredAt     int64
	TrustBundle   *DRTransportCATrustBundle
}

func (m *drRelationshipManager) transportCABundleLocked() (*drTransportCABundle, *drTransportCA, error) {
	bundle, err := loadDRTransportCABundle(m.core)
	if err != nil {
		return nil, nil, err
	}
	if bundle == nil {
		return nil, nil, fmt.Errorf("DR transport CA has not been generated")
	}
	active, err := parseDRTransportCA(bundle.CertDER, bundle.KeyPEM)
	if err != nil {
		return nil, nil, err
	}
	return bundle, active, nil
}

func trustedDERFromTransportCABundle(bundle *drTransportCABundle) [][]byte {
	if bundle == nil {
		return nil
	}
	trusted := normalizeDRPrimaryCACerts(bundle.CertDER, nil)
	if len(bundle.PendingCertDER) > 0 {
		trusted = normalizeDRPrimaryCACerts(bundle.CertDER, append(trusted, bundle.PendingCertDER))
	}
	if len(bundle.PreviousCertDER) > 0 {
		trusted = normalizeDRPrimaryCACerts(bundle.CertDER, append(trusted, bundle.PreviousCertDER))
	}
	return trusted
}

func (m *drRelationshipManager) transportCATrustBundleLocked(bundle *drTransportCABundle, signer *drTransportCA) (*DRTransportCATrustBundle, error) {
	if m.config == nil || m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	if bundle == nil || signer == nil {
		return nil, fmt.Errorf("DR transport CA is not initialized")
	}
	tb := &DRTransportCATrustBundle{
		Version:        drTransportCATrustBundleVersion,
		ClusterID:      m.config.ClusterID,
		OperationID:    bundle.PendingOperationID,
		IssuedAt:       time.Now().UTC().Unix(),
		ActiveCACert:   append([]byte(nil), bundle.CertDER...),
		TrustedCACerts: trustedDERFromTransportCABundle(bundle),
		StagedCACert:   append([]byte(nil), bundle.PendingCertDER...),
		PreviousCACert: append([]byte(nil), bundle.PreviousCertDER...),
	}
	if err := signDRTransportCATrustBundle(tb, signer); err != nil {
		return nil, err
	}
	return tb, nil
}

func (m *drRelationshipManager) PrimaryTransportCATrustBundle(ctx context.Context) (*DRTransportCATrustBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	bundle, active, err := m.transportCABundleLocked()
	if err != nil {
		return nil, err
	}
	return m.transportCATrustBundleLocked(bundle, active)
}

func (m *drRelationshipManager) StagePrimaryTransportCARotation(ctx context.Context) (*DRTransportCARotationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	bundle, active, err := m.transportCABundleLocked()
	if err != nil {
		return nil, err
	}

	if len(bundle.PendingCertDER) == 0 {
		material, err := generateDRTransportCAMaterial()
		if err != nil {
			return nil, err
		}
		operationID, err := uuid.GenerateUUID()
		if err != nil {
			return nil, fmt.Errorf("failed to generate DR transport CA rotation operation ID: %w", err)
		}
		bundle.PendingCertPEM = material.certPEM
		bundle.PendingKeyPEM = material.keyPEM
		bundle.PendingCertDER = material.ca.certDER
		bundle.PendingCreatedAt = material.createdAt
		bundle.PendingOperationID = operationID
		bundle.PendingRotationTime = time.Now().UTC().Unix()
		if err := persistDRTransportCABundle(m.core, bundle); err != nil {
			return nil, err
		}
	}

	tb, err := m.transportCATrustBundleLocked(bundle, active)
	if err != nil {
		return nil, err
	}
	stagedCert, err := x509.ParseCertificate(bundle.PendingCertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse staged DR transport CA: %w", err)
	}
	return &DRTransportCARotationResult{
		OperationID: bundle.PendingOperationID,
		ActiveKeyID: active.spkiHash(),
		StagedKeyID: drTransportCAKeyID(stagedCert),
		StartedAt:   bundle.PendingRotationTime,
		TrustBundle: tb,
	}, nil
}

func (m *drRelationshipManager) ActivatePrimaryTransportCARotation(ctx context.Context, operationID string) (*DRTransportCARotationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	bundle, active, err := m.transportCABundleLocked()
	if err != nil {
		return nil, err
	}
	if bundle.PendingOperationID == "" || len(bundle.PendingCertDER) == 0 || len(bundle.PendingKeyPEM) == 0 {
		return nil, fmt.Errorf("no pending DR transport CA rotation")
	}
	if operationID == "" {
		return nil, fmt.Errorf("operation_id is required")
	}
	if !strings.EqualFold(operationID, bundle.PendingOperationID) {
		return nil, fmt.Errorf("pending DR transport CA rotation operation mismatch")
	}

	oldKeyID := active.spkiHash()
	oldCertDER := append([]byte(nil), bundle.CertDER...)
	newCA, err := parseDRTransportCA(bundle.PendingCertDER, bundle.PendingKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid pending DR transport CA: %w", err)
	}

	activatedAt := time.Now().UTC().Unix()
	bundle.CertPEM = append([]byte(nil), bundle.PendingCertPEM...)
	bundle.KeyPEM = append([]byte(nil), bundle.PendingKeyPEM...)
	bundle.CertDER = append([]byte(nil), bundle.PendingCertDER...)
	bundle.CreatedAt = bundle.PendingCreatedAt
	bundle.PreviousCertDER = oldCertDER
	bundle.PreviousKeyID = oldKeyID
	bundle.ActivatedAt = activatedAt
	bundle.PendingCertPEM = nil
	bundle.PendingKeyPEM = nil
	bundle.PendingCertDER = nil
	bundle.PendingCreatedAt = time.Time{}
	bundle.PendingOperationID = ""
	bundle.PendingRotationTime = 0
	if err := persistDRTransportCABundle(m.core, bundle); err != nil {
		return nil, err
	}

	m.transportCA = newCA
	if m.handler != nil {
		if err := m.handler.SetTransportCA(newCA); err != nil {
			if errors.Is(err, errDRTransportLeafKeyUnavailable) {
				m.logger.Debug("deferring DR transport leaf cert mint after CA activation until local cluster key is available")
			} else {
				return nil, fmt.Errorf("failed to mint DR transport leaf from activated CA: %w", err)
			}
		}
	}

	tb, err := m.transportCATrustBundleLocked(bundle, newCA)
	if err != nil {
		return nil, err
	}
	return &DRTransportCARotationResult{
		OperationID:   operationID,
		ActiveKeyID:   newCA.spkiHash(),
		PreviousKeyID: oldKeyID,
		ActivatedAt:   activatedAt,
		TrustBundle:   tb,
	}, nil
}

func (m *drRelationshipManager) RetirePreviousPrimaryTransportCA(ctx context.Context) (*DRTransportCARotationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}
	bundle, active, err := m.transportCABundleLocked()
	if err != nil {
		return nil, err
	}
	previousKeyID := bundle.PreviousKeyID
	bundle.PreviousCertDER = nil
	bundle.PreviousKeyID = ""
	if err := persistDRTransportCABundle(m.core, bundle); err != nil {
		return nil, err
	}
	tb, err := m.transportCATrustBundleLocked(bundle, active)
	if err != nil {
		return nil, err
	}
	return &DRTransportCARotationResult{
		ActiveKeyID:   active.spkiHash(),
		PreviousKeyID: previousKeyID,
		RetiredAt:     time.Now().UTC().Unix(),
		TrustBundle:   tb,
	}, nil
}

func (m *drRelationshipManager) AcceptPrimaryTransportCATrustBundle(ctx context.Context, bundle *DRTransportCATrustBundle) (*DRTransportCARotationResult, error) {
	if bundle == nil {
		return nil, fmt.Errorf("DR transport CA trust bundle is required")
	}
	if _, _, err := validateDRTransportCATrustBundleCerts(bundle); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil || m.config.Mode != DRModeSecondary {
		return nil, fmt.Errorf("not in DR secondary mode")
	}
	if bundle.ClusterID != m.config.ClusterID {
		return nil, fmt.Errorf("DR transport CA trust bundle cluster mismatch")
	}

	currentActive, currentTrusted, err := parseDRTransportCATrustAnchors(m.config.PrimaryCACert, m.config.PrimaryCACerts)
	if err != nil {
		return nil, fmt.Errorf("invalid current primary CA trust set: %w", err)
	}
	if err := verifyDRTransportCATrustBundleSignature(bundle, currentTrusted); err != nil {
		return nil, err
	}
	activeCert, trustedCerts, err := validateDRTransportCATrustBundleCerts(bundle)
	if err != nil {
		return nil, err
	}
	if currentActive != nil &&
		bytes.Equal(currentActive.Raw, activeCert.Raw) &&
		len(bundle.PreviousCACert) > 0 &&
		!drTransportCATrustSetContainsDER(currentTrusted, bundle.PreviousCACert) {
		return nil, fmt.Errorf("stale DR transport CA trust bundle would revive a retired previous CA")
	}

	trustedDER := make([][]byte, 0, len(trustedCerts))
	for _, cert := range trustedCerts {
		trustedDER = append(trustedDER, append([]byte(nil), cert.Raw...))
	}
	previous := *m.config
	m.config.PrimaryCACert = append([]byte(nil), activeCert.Raw...)
	m.config.PrimaryCACerts = normalizeDRPrimaryCACerts(activeCert.Raw, trustedDER)
	if err := m.saveConfig(ctx); err != nil {
		*m.config = previous
		return nil, err
	}
	if m.secondary != nil {
		if err := m.secondary.setPrimaryCATrustDER(m.config.PrimaryCACert, m.config.PrimaryCACerts); err != nil {
			*m.config = previous
			_ = m.saveConfig(ctx)
			return nil, err
		}
	}

	return &DRTransportCARotationResult{
		OperationID: bundle.OperationID,
		ActiveKeyID: drTransportCAKeyID(activeCert),
		StagedKeyID: func() string {
			if len(bundle.StagedCACert) == 0 {
				return ""
			}
			cert, err := x509.ParseCertificate(bundle.StagedCACert)
			if err != nil {
				return ""
			}
			return drTransportCAKeyID(cert)
		}(),
		PreviousKeyID: func() string {
			if len(bundle.PreviousCACert) == 0 {
				return ""
			}
			cert, err := x509.ParseCertificate(bundle.PreviousCACert)
			if err != nil {
				return ""
			}
			return drTransportCAKeyID(cert)
		}(),
	}, nil
}

func drTransportCATrustSetContainsDER(certs []*x509.Certificate, der []byte) bool {
	if len(der) == 0 {
		return false
	}
	for _, cert := range certs {
		if cert != nil && bytes.Equal(cert.Raw, der) {
			return true
		}
	}
	return false
}
