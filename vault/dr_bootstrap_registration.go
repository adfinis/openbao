// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
	hashiraft "github.com/hashicorp/raft"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	drRegistrationDialTimeout           = 5 * time.Second
	drRegistrationTLSHandshakeTimeout   = 5 * time.Second
	drRegistrationResponseHeaderTimeout = 10 * time.Second
	drRegistrationRequestTimeout        = 15 * time.Second
	drBootstrapTokenHashDomain          = "openbao-dr-bootstrap-token-v1"
)

type drPrimaryAPIClientConfig struct {
	APIAddr    string
	CACert     []byte
	ServerName string
}

func drPrimaryAPIClientConfigFromActivationToken(token *DRActivationToken) drPrimaryAPIClientConfig {
	if token == nil {
		return drPrimaryAPIClientConfig{}
	}
	caBytes := token.PrimaryAPICACert
	if len(caBytes) == 0 {
		caBytes = token.DRTransportCACert
	}
	serverName := token.PrimaryAPIServerName
	if serverName == "" {
		serverName = deriveServerName(token.PrimaryAPIAddr)
	}
	return drPrimaryAPIClientConfig{
		APIAddr:    normalizePrimaryAPIAddr(token.PrimaryAPIAddr),
		CACert:     caBytes,
		ServerName: serverName,
	}
}

func drPrimaryAPIClientConfigFromDRConfig(config DRConfig) drPrimaryAPIClientConfig {
	serverName := config.PrimaryAPIServerName
	if serverName == "" {
		serverName = deriveServerName(config.PrimaryAPIAddr)
	}
	return drPrimaryAPIClientConfig{
		APIAddr:    normalizePrimaryAPIAddr(config.PrimaryAPIAddr),
		CACert:     config.PrimaryAPICACert,
		ServerName: serverName,
	}
}

func primaryAPIAddrRequiresTLS(apiAddr string) bool {
	apiAddr = normalizePrimaryAPIAddr(apiAddr)
	if apiAddr == "" {
		return false
	}
	u, err := url.Parse(apiAddr)
	if err != nil {
		return true
	}
	return strings.EqualFold(u.Scheme, "https")
}

func newDRPrimaryAPIHTTPClient(config drPrimaryAPIClientConfig) (*http.Client, error) {
	config.APIAddr = normalizePrimaryAPIAddr(config.APIAddr)
	if config.APIAddr == "" {
		return nil, fmt.Errorf("missing primary API address")
	}

	u, err := url.Parse(config.APIAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid primary API address: %w", err)
	}

	var tlsConfig *tls.Config
	switch strings.ToLower(u.Scheme) {
	case "http":
	case "https":
		if len(config.CACert) == 0 {
			return nil, fmt.Errorf("missing primary API CA certificate")
		}
		primaryCert, err := x509.ParseCertificate(config.CACert)
		if err != nil {
			return nil, fmt.Errorf("invalid primary API CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(primaryCert)

		serverName := config.ServerName
		if serverName == "" {
			serverName = deriveServerName(config.APIAddr)
		}
		if serverName == "" {
			return nil, fmt.Errorf("failed to determine primary API TLS server name")
		}

		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
			ServerName: serverName,
		}
	default:
		return nil, fmt.Errorf("unsupported primary API scheme %q", u.Scheme)
	}

	return &http.Client{
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
	}, nil
}

func postDRPrimaryAPIJSON(ctx context.Context, config drPrimaryAPIClientConfig, path string, body map[string]interface{}) error {
	config.APIAddr = normalizePrimaryAPIAddr(config.APIAddr)
	client, err := newDRPrimaryAPIHTTPClient(config)
	if err != nil {
		return err
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal primary API request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/sys/%s", strings.TrimRight(config.APIAddr, "/"), strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create primary API request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("primary API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg != "" {
			return fmt.Errorf("primary API request returned status %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("primary API request returned status %d", resp.StatusCode)
	}
	return nil
}

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
		RelationshipID:     relationshipID,
		State:              DRRelationshipStatePending,
		BootstrapTokenHash: hashDRBootstrapToken(relationshipID, bootstrapToken),
		CreatedAt:          now.Unix(),
		ExpiresAt:          now.Add(drBootstrapTokenTTL).Unix(),
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
	clusterAddr := normalizePrimaryAddr(m.core.ClusterAddr())
	primaryAddrs := normalizePrimaryAddrs([]string{clusterAddr})

	// Add voter peer addresses from the raft configuration when available.
	if rb := m.core.GetRaftBackend(); rb != nil {
		peers, peersErr := rb.Peers(ctx)
		if peersErr != nil {
			m.logger.Warn("failed to enumerate raft peers for DR token", "error", peersErr)
		} else {
			peerAddrs := make([]string, 0, len(peers)+1)
			peerAddrs = append(peerAddrs, clusterAddr)
			for _, peer := range peers {
				if peer.Suffrage != int(hashiraft.Voter) {
					continue
				}
				peerAddrs = append(peerAddrs, peer.Address)
			}
			primaryAddrs = normalizePrimaryAddrs(peerAddrs)
		}
	}

	if len(primaryAddrs) == 0 && clusterAddr != "" {
		primaryAddrs = []string{clusterAddr}
	}
	primaryAddr := ""
	if len(primaryAddrs) > 0 {
		primaryAddr = primaryAddrs[0]
	}

	token := &DRActivationToken{
		ClusterID:      m.config.ClusterID,
		RelationshipID: relationshipID,
		PrimaryAddr:    primaryAddr,
		PrimaryAddrs:   primaryAddrs,
		PrimaryAPIAddr: m.core.redirectAddr,
		ReplSalt:       m.config.ReplSalt,
		BootstrapToken: bootstrapToken,
	}

	// Include the DR transport CA cert so the secondary can verify
	// the primary's TLS identity. The transport CA is the sole trust
	// anchor for cross-cluster mTLS.
	if m.transportCA != nil {
		token.DRTransportCACert = m.transportCA.certDER
	}
	// For API registration HTTPS verification, use the API listener chain CA
	// where available; fall back to cluster CA if we cannot determine it.
	if apiCA := m.primaryAPICACertFromConfig(); len(apiCA) > 0 {
		token.PrimaryAPICACert = apiCA
	}
	if len(token.PrimaryAPICACert) == 0 {
		token.PrimaryAPICACert = token.DRTransportCACert
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

func validateDRBootstrapRegistrationInput(relationshipID, bootstrapToken string, secondaryCACert []byte) error {
	if relationshipID == "" {
		return fmt.Errorf("relationship_id is required")
	}
	if _, err := uuid.ParseUUID(relationshipID); err != nil {
		return fmt.Errorf("relationship_id must be a valid UUID")
	}
	if bootstrapToken == "" {
		return fmt.Errorf("bootstrap_token is required")
	}
	if _, err := uuid.ParseUUID(bootstrapToken); err != nil {
		return fmt.Errorf("bootstrap_token must be a valid UUID")
	}
	if len(secondaryCACert) == 0 {
		return fmt.Errorf("secondary_ca_cert is required")
	}
	if len(secondaryCACert) > drBootstrapMaxCertDERBytes {
		return fmt.Errorf("secondary_ca_cert exceeds maximum DER size %d", drBootstrapMaxCertDERBytes)
	}
	return nil
}

func hashDRBootstrapToken(relationshipID, bootstrapToken string) string {
	sum := sha256.Sum256([]byte(drBootstrapTokenHashDomain + "\x00" + relationshipID + "\x00" + bootstrapToken))
	return hex.EncodeToString(sum[:])
}

func bootstrapTokenMatches(rel *DRRelationship, bootstrapToken string) bool {
	if rel == nil {
		return false
	}
	if rel.BootstrapTokenHash != "" {
		got := hashDRBootstrapToken(rel.RelationshipID, bootstrapToken)
		return subtle.ConstantTimeCompare([]byte(got), []byte(rel.BootstrapTokenHash)) == 1
	}
	if rel.BootstrapToken != "" {
		return subtle.ConstantTimeCompare([]byte(rel.BootstrapToken), []byte(bootstrapToken)) == 1
	}
	return false
}

func (m *drRelationshipManager) recordBootstrapFailureLocked(ctx context.Context, rel *DRRelationship, reason string, now time.Time, sourceIP string, terminal bool) {
	if rel == nil {
		return
	}
	rel.FailedAttempts++
	rel.LastError = reason
	rel.LastFailedAt = now.Unix()
	rel.LastFailedFromIP = sourceIP
	if rel.State == DRRelationshipStatePending && (terminal || rel.FailedAttempts >= drBootstrapMaxFailedAttempts) {
		rel.State = DRRelationshipStateRevoked
		rel.BootstrapToken = ""
		rel.BootstrapTokenHash = ""
		rel.ExpiresAt = 0
		rel.RevokedAt = now.Unix()
		rel.LockedUntil = now.Add(drBootstrapLockoutDuration).Unix()
		m.logger.Warn("DR bootstrap registration reached terminal failure",
			"relationship_id", rel.RelationshipID,
			"failed_attempts", rel.FailedAttempts,
			"locked_until", rel.LockedUntil,
			"terminal", true)
	}
	if err := m.saveRelationship(ctx, rel); err != nil {
		m.logger.Warn("failed to persist DR bootstrap failure",
			"relationship_id", rel.RelationshipID,
			"error", err)
	}
	m.logger.Warn("DR bootstrap registration failed",
		"relationship_id", rel.RelationshipID,
		"failed_attempts", rel.FailedAttempts,
		"reason", reason,
		"source_ip", sourceIP)
}

// ValidateBootstrapAndStoreCert validates a bootstrap token and stores the
// secondary's DR client certificate trust anchor. Returns an error if the token
// is invalid or already consumed.
func (m *drRelationshipManager) ValidateBootstrapAndStoreCert(ctx context.Context, relationshipID string, bootstrapToken string, secondaryCACert []byte) error {
	return m.ValidateBootstrapAndStoreCertWithSourceIP(ctx, relationshipID, bootstrapToken, secondaryCACert, "")
}

// ValidateBootstrapAndStoreCertWithSourceIP validates bootstrap registration
// and records the source IP of successful registration.
func (m *drRelationshipManager) ValidateBootstrapAndStoreCertWithSourceIP(ctx context.Context, relationshipID string, bootstrapToken string, secondaryCACert []byte, sourceIP string) error {
	if err := validateDRBootstrapRegistrationInput(relationshipID, bootstrapToken, secondaryCACert); err != nil {
		return err
	}

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
		m.logger.Warn("DR bootstrap registration rejected for non-pending relationship",
			"relationship_id", matchedRel.RelationshipID,
			"state", matchedRel.State,
			"source_ip", sourceIP)
		return fmt.Errorf("invalid or already-used bootstrap token")
	}

	if matchedRel.ExpiresAt > 0 && matchedRel.ExpiresAt <= nowUnix {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "bootstrap token expired", now, sourceIP, true)
		return fmt.Errorf("bootstrap token expired")
	}

	if matchedRel.LockedUntil > nowUnix {
		return fmt.Errorf("bootstrap registration locked until %d", matchedRel.LockedUntil)
	}

	if matchedRel.RegisteredFromIP != "" && sourceIP != "" && matchedRel.RegisteredFromIP != sourceIP {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "source IP mismatch", now, sourceIP, false)
		return fmt.Errorf("registration source IP mismatch")
	}

	if !bootstrapTokenMatches(matchedRel, bootstrapToken) {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "bootstrap token mismatch", now, sourceIP, false)
		return fmt.Errorf("invalid or already-used bootstrap token")
	}

	// Parse and validate the secondary's DR client certificate trust anchor.
	cert, err := x509.ParseCertificate(secondaryCACert)
	if err != nil {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "invalid secondary certificate", now, sourceIP, false)
		return fmt.Errorf("invalid secondary CA certificate: %w", err)
	}
	if err := validateDRSecondaryClientCert(cert, now); err != nil {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "invalid secondary certificate", now, sourceIP, false)
		return fmt.Errorf("invalid secondary CA certificate: %w", err)
	}

	// Enforce fingerprint uniqueness: a secondary cert fingerprint can be
	// bound to at most one relationship lineage. Reusing a revoked
	// relationship credential with a fresh bootstrap token would revive stale
	// credential material, so revoked relationships are included in the scan.
	newFP := certFingerprintSHA256(cert)
	if isStalePostPromotionFingerprint(m.config.Promotion, newFP) {
		m.recordBootstrapFailureLocked(ctx, matchedRel, "stale pre-promotion certificate fingerprint", now, sourceIP, true)
		return fmt.Errorf("certificate fingerprint belongs to stale pre-promotion DR lineage")
	}
	if existingRel, fpErr := m.findRelationshipByFingerprint(ctx, newFP); fpErr == nil && existingRel != nil {
		if existingRel.RelationshipID != matchedRel.RelationshipID {
			m.recordBootstrapFailureLocked(ctx, matchedRel, "duplicate fingerprint", now, sourceIP, false)
			return fmt.Errorf("certificate fingerprint already bound to relationship %q", existingRel.RelationshipID)
		}
	}

	// Update relationship: store cert, clear bootstrap token, and mark registered.
	matchedRel.SecondaryCACert = secondaryCACert
	matchedRel.SecondaryCertFingerprint = newFP
	matchedRel.CredentialGeneration = 1
	matchedRel.BootstrapToken = ""
	matchedRel.BootstrapTokenHash = ""
	matchedRel.State = DRRelationshipStateRegistered
	matchedRel.LastSeenAt = nowUnix
	matchedRel.RegisteredAt = nowUnix
	matchedRel.FailedAttempts = 0
	matchedRel.LockedUntil = 0
	matchedRel.ExpiresAt = 0
	matchedRel.LastError = ""
	matchedRel.LastFailedAt = 0
	matchedRel.LastFailedFromIP = ""
	matchedRel.RegisteredFromIP = sourceIP

	if err := m.saveRelationship(ctx, matchedRel); err != nil {
		return fmt.Errorf("failed to persist relationship certificate: %w", err)
	}

	// Add the cert to the handler's trusted pool only after the relationship
	// record is durably updated. Otherwise a failed write could leave an
	// in-memory trust entry without matching persisted authorization state.
	m.handler.AddTrustedCert(matchedRel.RelationshipID, cert)

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

	if m.config == nil || len(m.config.SecondaryClientCert) == 0 {
		return fmt.Errorf("missing DR secondary client certificate for registration")
	}
	secondaryCertDER := m.config.SecondaryClientCert

	// Build the request body. The cert is base64-encoded DER as expected
	// by the registration endpoint.
	body := map[string]interface{}{
		"relationship_id":   token.RelationshipID,
		"bootstrap_token":   token.BootstrapToken,
		"secondary_ca_cert": base64.StdEncoding.EncodeToString(secondaryCertDER),
	}
	if err := postDRPrimaryAPIJSON(ctx, drPrimaryAPIClientConfigFromActivationToken(token), "replication/dr/primary/register-secondary", body); err != nil {
		return fmt.Errorf("registration request failed: %w", err)
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
