// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	drRegistrationDialTimeout           = 5 * time.Second
	drRegistrationTLSHandshakeTimeout   = 5 * time.Second
	drRegistrationResponseHeaderTimeout = 10 * time.Second
	drRegistrationRequestTimeout        = 15 * time.Second
)

// GenerateActivationToken creates a token that a secondary uses to
// establish the DR relationship. Each token includes a single-use
// bootstrap token for the secondary's cert registration step.
func (m *drRelationshipManager) GenerateActivationToken(ctx context.Context) (*DRActivationToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return nil, fmt.Errorf("not in DR primary mode")
	}

	// Generate a single-use bootstrap token for the secondary cert
	// registration handshake.
	bootstrapToken, err := uuid.GenerateUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate bootstrap token: %w", err)
	}

	relationshipID, err := uuid.GenerateUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate relationship ID: %w", err)
	}

	now := time.Now().UTC()
	relationship := &DRRelationship{
		RelationshipID: relationshipID,
		State:          DRRelationshipStatePending,
		BootstrapToken: bootstrapToken,
		CreatedAt:      now.Unix(),
		ExpiresAt:      now.Add(drBootstrapTokenTTL).Unix(),
	}
	relBytes, err := json.Marshal(relationship)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal relationship info: %w", err)
	}
	storageEntry := &logical.StorageEntry{
		Key:   drRelationshipsPath + relationshipID,
		Value: relBytes,
	}
	if err := m.core.barrier.Put(ctx, storageEntry); err != nil {
		return nil, fmt.Errorf("failed to store pending relationship: %w", err)
	}

	// Get the cluster address for the primary.
	clusterAddr := m.core.ClusterAddr()

	token := &DRActivationToken{
		ClusterID:      m.config.ClusterID,
		RelationshipID: relationshipID,
		PrimaryAddr:    clusterAddr,
		PrimaryAPIAddr: m.core.redirectAddr,
		ReplSalt:       m.config.ReplSalt,
		BootstrapToken: bootstrapToken,
	}

	// Include the cluster CA cert (DER) so the secondary
	// can verify the primary's TLS identity.
	if parsedCert := m.core.localClusterParsedCert.Load(); parsedCert != nil {
		token.CACert = parsedCert.Raw
	}
	// For API registration HTTPS verification, use the API listener chain CA
	// where available; fall back to cluster CA if we cannot determine it.
	if apiCA := m.primaryAPICACertFromConfig(); len(apiCA) > 0 {
		token.PrimaryAPICACert = apiCA
	}
	if len(token.PrimaryAPICACert) == 0 {
		token.PrimaryAPICACert = token.CACert
	}

	if token.PrimaryAPIServerName == "" {
		token.PrimaryAPIServerName = deriveServerName(token.PrimaryAPIAddr)
	}

	m.logger.Info("generated DR activation token",
		"cluster_id", m.config.ClusterID,
		"relationship_id", relationshipID,
		"expires_at", relationship.ExpiresAt)
	return token, nil
}

func (m *drRelationshipManager) primaryAPICACertFromConfig() []byte {
	conf := m.core.rawConfig.Load()
	if conf == nil || len(conf.Listeners) == 0 {
		return nil
	}

	for _, ln := range conf.Listeners {
		if ln == nil || ln.TLSCertFile == "" {
			continue
		}

		pemData, err := os.ReadFile(ln.TLSCertFile)
		if err != nil {
			continue
		}

		var certs []*x509.Certificate
		rest := pemData
		for len(rest) > 0 {
			block, next := pem.Decode(rest)
			rest = next
			if block == nil || block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			certs = append(certs, cert)
		}
		if len(certs) == 0 {
			continue
		}

		// Prefer an explicit CA cert from the chain; otherwise use the last cert.
		for i := len(certs) - 1; i >= 0; i-- {
			if certs[i].IsCA {
				return certs[i].Raw
			}
		}
		return certs[len(certs)-1].Raw
	}

	return nil
}

func (m *drRelationshipManager) recordBootstrapFailureLocked(ctx context.Context, rel *DRRelationship, reason string, now time.Time) {
	rel.FailedAttempts++
	rel.LastError = reason
	if rel.FailedAttempts >= drBootstrapMaxFailedAttempts {
		rel.LockedUntil = now.Add(drBootstrapLockoutDuration).Unix()
		m.logger.Warn("DR bootstrap registration locked due to repeated failures",
			"relationship_id", rel.RelationshipID,
			"failed_attempts", rel.FailedAttempts,
			"locked_until", rel.LockedUntil)
	}
	if err := m.saveRelationship(ctx, rel); err != nil {
		m.logger.Warn("failed to persist DR bootstrap failure",
			"relationship_id", rel.RelationshipID,
			"error", err)
	}
	m.logger.Warn("DR bootstrap registration failed",
		"relationship_id", rel.RelationshipID,
		"failed_attempts", rel.FailedAttempts,
		"reason", reason)
}

// ValidateBootstrapAndStoreCert validates a bootstrap token and stores the
// secondary's CA certificate. Returns an error if the token is invalid or
// already consumed.
func (m *drRelationshipManager) ValidateBootstrapAndStoreCert(ctx context.Context, relationshipID string, bootstrapToken string, secondaryCACert []byte) error {
	return m.ValidateBootstrapAndStoreCertWithSourceIP(ctx, relationshipID, bootstrapToken, secondaryCACert, "")
}

// ValidateBootstrapAndStoreCertWithSourceIP validates bootstrap registration
// and records the source IP of successful registration.
func (m *drRelationshipManager) ValidateBootstrapAndStoreCertWithSourceIP(ctx context.Context, relationshipID string, bootstrapToken string, secondaryCACert []byte, sourceIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.Mode != DRModePrimary {
		return fmt.Errorf("not in DR primary mode")
	}

	if m.handler == nil {
		return fmt.Errorf("DR handler not initialized")
	}

	matchedRel, err := m.loadRelationship(ctx, relationshipID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nowUnix := now.Unix()

	if matchedRel.State != DRRelationshipStatePending {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "relationship is not pending", now)
		return fmt.Errorf("invalid or already-used bootstrap token")
	}

	if matchedRel.ExpiresAt > 0 && matchedRel.ExpiresAt <= nowUnix {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "bootstrap token expired", now)
		return fmt.Errorf("bootstrap token expired")
	}

	if matchedRel.LockedUntil > nowUnix {
		return fmt.Errorf("bootstrap registration locked until %d", matchedRel.LockedUntil)
	}

	if matchedRel.RegisteredFromIP != "" && sourceIP != "" && matchedRel.RegisteredFromIP != sourceIP {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "source IP mismatch", now)
		return fmt.Errorf("registration source IP mismatch")
	}

	if matchedRel.BootstrapToken != bootstrapToken {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "bootstrap token mismatch", now)
		return fmt.Errorf("invalid or already-used bootstrap token")
	}

	// Parse and validate the secondary's CA cert.
	cert, err := x509.ParseCertificate(secondaryCACert)
	if err != nil {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "invalid secondary certificate", now)
		return fmt.Errorf("invalid secondary CA certificate: %w", err)
	}

	// Add the cert to the handler's trusted pool.
	m.handler.AddTrustedCert(matchedRel.RelationshipID, cert)

	// Update relationship: store cert, clear bootstrap token, and mark registered.
	matchedRel.SecondaryCACert = secondaryCACert
	matchedRel.SecondaryCertFingerprint = certFingerprintSHA256(cert)
	matchedRel.BootstrapToken = ""
	matchedRel.State = DRRelationshipStateRegistered
	matchedRel.LastSeenAt = nowUnix
	matchedRel.FailedAttempts = 0
	matchedRel.LockedUntil = 0
	matchedRel.ExpiresAt = 0
	matchedRel.LastError = ""
	matchedRel.RegisteredFromIP = sourceIP

	if err := m.saveRelationship(ctx, matchedRel); err != nil {
		return fmt.Errorf("failed to persist relationship certificate: %w", err)
	}
	m.latestSeenAt[matchedRel.RelationshipID] = nowUnix
	m.lastSeenWriteAt[matchedRel.RelationshipID] = now

	m.logger.Info("registered secondary certificate via bootstrap token",
		"relationship_id", matchedRel.RelationshipID,
		"fingerprint", matchedRel.SecondaryCertFingerprint,
		"source_ip", sourceIP)
	return nil
}

// registerWithPrimary sends the secondary's cluster CA cert to the primary
// via the HTTP API registration endpoint, authenticated with the bootstrap
// token from the activation token.
func (m *drRelationshipManager) registerWithPrimary(ctx context.Context, token *DRActivationToken) error {
	if token.RelationshipID == "" {
		return fmt.Errorf("activation token missing relationship_id")
	}
	if token.BootstrapToken == "" {
		m.logger.Warn("no bootstrap token in activation token; skipping registration")
		return nil
	}

	apiAddr := token.PrimaryAPIAddr
	if apiAddr == "" {
		m.logger.Warn("no primary API address in activation token; skipping registration")
		return nil
	}

	// Get the secondary's own cluster CA cert (DER).
	localCert := m.core.localClusterParsedCert.Load()
	if localCert == nil {
		return fmt.Errorf("no local cluster certificate available for registration")
	}

	// Build the request body. The cert is base64-encoded DER as expected
	// by the registration endpoint.
	body := map[string]interface{}{
		"relationship_id":   token.RelationshipID,
		"bootstrap_token":   token.BootstrapToken,
		"secondary_ca_cert": base64.StdEncoding.EncodeToString(localCert.Raw),
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal registration request: %w", err)
	}

	// Build TLS config for the registration HTTP call. Prefer the
	// API-specific CA if present; otherwise fall back to cluster CA.
	caBytes := token.PrimaryAPICACert
	if len(caBytes) == 0 {
		caBytes = token.CACert
	}
	if len(caBytes) == 0 {
		return fmt.Errorf("missing primary API CA certificate in activation token")
	}
	primaryCert, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return fmt.Errorf("invalid primary API CA certificate in activation token: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(primaryCert)

	serverName := token.PrimaryAPIServerName
	if serverName == "" {
		serverName = deriveServerName(apiAddr)
	}
	if serverName == "" {
		return fmt.Errorf("failed to determine primary API TLS server name from activation token")
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: serverName,
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: drRegistrationDialTimeout,
			}).DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   drRegistrationTLSHandshakeTimeout,
			ResponseHeaderTimeout: drRegistrationResponseHeaderTimeout,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		Timeout: drRegistrationRequestTimeout,
	}

	url := fmt.Sprintf("%s/v1/sys/replication/dr/primary/register-secondary", apiAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("registration request returned status %d", resp.StatusCode)
	}

	m.logger.Info("successfully registered with primary")
	return nil
}

func deriveServerName(apiAddr string) string {
	if apiAddr == "" {
		return ""
	}

	addr := apiAddr
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}

	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	host := u.Host
	if host == "" {
		return ""
	}

	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
