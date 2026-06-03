// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	// drTransportCAPath is the barrier storage path for the DR transport CA
	// key pair. This CA is independent of the barrier seal and recovery keys.
	drTransportCAPath = "core/dr-replication/transport-ca"

	// drTransportCAValidityYears is the validity period for the DR transport CA.
	drTransportCAValidityYears = 10

	// drTransportLeafValidity is the validity period for per-node DR
	// transport leaf certificates signed by the CA.
	drTransportLeafValidity = 72 * time.Hour

	// drSecondaryClientCertValidityYears is the validity period for the
	// secondary's self-signed relationship credential. It is intentionally
	// long-lived; rotation is modeled as a new DR relationship or an explicit
	// future certificate-rotation API, not silent bootstrap-token reuse.
	drSecondaryClientCertValidityYears = 10

	drCertClockSkew = 5 * time.Minute

	drTransportCATrustBundleVersion   = 1
	drTransportCATrustBundleAlgorithm = "ecdsa-sha256-dr-transport-ca-trust-v1"
)

// drTransportCABundle is the serialized form of the DR transport CA stored
// in barrier storage.
type drTransportCABundle struct {
	// CertPEM is the PEM-encoded CA certificate.
	CertPEM []byte `json:"cert_pem"`

	// KeyPEM is the PEM-encoded ECDSA private key.
	KeyPEM []byte `json:"key_pem"`

	// CertDER is the raw DER-encoded CA certificate for direct use in
	// activation tokens and trust pool seeding.
	CertDER []byte `json:"cert_der"`

	// CreatedAt records when the CA was generated.
	CreatedAt time.Time `json:"created_at"`

	// Pending fields hold a staged replacement CA during operator-driven DR
	// transport CA rotation. The pending CA is not used to mint leaves until
	// activation.
	PendingCertPEM      []byte    `json:"pending_cert_pem,omitempty"`
	PendingKeyPEM       []byte    `json:"pending_key_pem,omitempty"`
	PendingCertDER      []byte    `json:"pending_cert_der,omitempty"`
	PendingCreatedAt    time.Time `json:"pending_created_at,omitempty"`
	PendingOperationID  string    `json:"pending_operation_id,omitempty"`
	PendingRotationTime int64     `json:"pending_rotation_time,omitempty"`

	// PreviousCertDER retains the prior active public CA after activation so
	// operators can observe and later retire the overlap window. The previous
	// private key is not retained.
	PreviousCertDER []byte `json:"previous_cert_der,omitempty"`
	PreviousKeyID   string `json:"previous_key_id,omitempty"`
	ActivatedAt     int64  `json:"activated_at,omitempty"`
}

// drTransportCA holds the parsed DR transport CA material.
type drTransportCA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey
}

type drTransportCAMaterial struct {
	ca        *drTransportCA
	certPEM   []byte
	keyPEM    []byte
	createdAt time.Time
}

// DRTransportCATrustBundle is the operator-transferable public trust update
// for primary DR transport CA rotation. It is signed by a currently trusted
// primary DR transport CA and contains only public CA certificates.
type DRTransportCATrustBundle struct {
	Version            int      `json:"version"`
	ClusterID          string   `json:"cluster_id"`
	OperationID        string   `json:"operation_id,omitempty"`
	IssuedAt           int64    `json:"issued_at"`
	ActiveCACert       []byte   `json:"active_ca_cert"`
	TrustedCACerts     [][]byte `json:"trusted_ca_certs,omitempty"`
	StagedCACert       []byte   `json:"staged_ca_cert,omitempty"`
	PreviousCACert     []byte   `json:"previous_ca_cert,omitempty"`
	SignatureAlgorithm string   `json:"signature_algorithm"`
	SignerKeyID        string   `json:"signer_key_id"`
	Signature          []byte   `json:"signature"`
}

// spkiHash returns the SHA-256 hash of the CA's SubjectPublicKeyInfo,
// suitable for inclusion in bootstrap AAD as the primary identity.
func (ca *drTransportCA) spkiHash() string {
	return drTransportCAKeyID(ca.cert)
}

func generateDRTransportCAMaterial() (*drTransportCAMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate DR transport CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "openbao-dr-transport-ca",
			Organization: []string{"OpenBao DR"},
		},
		NotBefore:             now.Add(-5 * time.Minute), // clock skew buffer
		NotAfter:              now.AddDate(drTransportCAValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
		MaxPathLenZero:        false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("failed to create DR transport CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse generated CA certificate: %w", err)
	}

	// Serialize to PEM for storage.
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CA private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	return &drTransportCAMaterial{
		ca:        &drTransportCA{cert: cert, certDER: certDER, key: key},
		certPEM:   certPEM,
		keyPEM:    keyPEM,
		createdAt: now,
	}, nil
}

// generateDRTransportCA creates a new P-256 ECDSA CA key pair for DR
// transport and persists it to barrier storage.
func generateDRTransportCA(core *Core) (*drTransportCA, error) {
	material, err := generateDRTransportCAMaterial()
	if err != nil {
		return nil, err
	}

	bundle := &drTransportCABundle{
		CertPEM:   material.certPEM,
		KeyPEM:    material.keyPEM,
		CertDER:   material.ca.certDER,
		CreatedAt: material.createdAt,
	}

	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CA bundle: %w", err)
	}

	entry := &logical.StorageEntry{
		Key:   drTransportCAPath,
		Value: bundleBytes,
	}
	if err := core.barrier.Put(core.activeContext.Load(), entry); err != nil {
		return nil, fmt.Errorf("failed to persist DR transport CA: %w", err)
	}

	return material.ca, nil
}

func loadDRTransportCABundle(core *Core) (*drTransportCABundle, error) {
	entry, err := core.barrier.Get(core.activeContext.Load(), drTransportCAPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load DR transport CA: %w", err)
	}
	if entry == nil {
		return nil, nil
	}

	var bundle drTransportCABundle
	if err := json.Unmarshal(entry.Value, &bundle); err != nil {
		return nil, fmt.Errorf("failed to unmarshal DR transport CA bundle: %w", err)
	}
	return &bundle, nil
}

func persistDRTransportCABundle(core *Core, bundle *drTransportCABundle) error {
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("failed to marshal CA bundle: %w", err)
	}
	entry := &logical.StorageEntry{
		Key:   drTransportCAPath,
		Value: bundleBytes,
	}
	if err := core.barrier.Put(core.activeContext.Load(), entry); err != nil {
		return fmt.Errorf("failed to persist DR transport CA: %w", err)
	}
	return nil
}

func parseDRTransportCA(certDER, keyPEM []byte) (*drTransportCA, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode CA private key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA private key: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return nil, fmt.Errorf("DR transport CA certificate must be a CA")
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("DR transport CA certificate public key must be ECDSA")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("DR transport CA certificate public key does not match private key")
	}

	return &drTransportCA{cert: cert, certDER: certDER, key: key}, nil
}

func drTransportCAKeyID(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(spki)
	return hex.EncodeToString(h[:])
}

// loadDRTransportCA loads the DR transport CA from barrier storage.
// Returns nil, nil if no CA has been generated yet.
func loadDRTransportCA(core *Core) (*drTransportCA, error) {
	bundle, err := loadDRTransportCABundle(core)
	if err != nil {
		return nil, err
	}
	if bundle == nil {
		return nil, nil
	}
	return parseDRTransportCA(bundle.CertDER, bundle.KeyPEM)
}

func generateDRSecondaryClientCert() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate DR secondary client key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate DR secondary client serial number: %w", err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "openbao-dr-secondary-client",
			Organization: []string{"OpenBao DR"},
		},
		NotBefore: now.Add(-5 * time.Minute),
		NotAfter:  now.AddDate(drSecondaryClientCertValidityYears, 0, 0),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create DR secondary client certificate: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal DR secondary client key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certDER, keyPEM, nil
}

func validateDRSecondaryClientCert(cert *x509.Certificate, now time.Time) error {
	if cert == nil {
		return fmt.Errorf("missing DR secondary client certificate")
	}
	now = now.UTC()
	if now.Add(drCertClockSkew).Before(cert.NotBefore) {
		return fmt.Errorf("DR secondary client certificate is not valid before %s", cert.NotBefore.UTC())
	}
	if !now.Before(cert.NotAfter) {
		return fmt.Errorf("DR secondary client certificate expired at %s", cert.NotAfter.UTC())
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return fmt.Errorf("DR secondary client certificate must be a self-signed CA trust anchor")
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return fmt.Errorf("DR secondary client certificate must allow digital signatures")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("DR secondary client certificate must allow certificate signing")
	}
	if len(cert.ExtKeyUsage) > 0 {
		allowed := false
		for _, usage := range cert.ExtKeyUsage {
			if usage == x509.ExtKeyUsageClientAuth || usage == x509.ExtKeyUsageAny {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("DR secondary client certificate must allow client authentication")
		}
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return fmt.Errorf("DR secondary client certificate must be self-signed: %w", err)
	}
	return nil
}

func parseDRSecondaryClientCert(certDER, keyPEM []byte) (*tls.Certificate, error) {
	if len(certDER) == 0 {
		return nil, fmt.Errorf("missing DR secondary client certificate")
	}
	if len(keyPEM) == 0 {
		return nil, fmt.Errorf("missing DR secondary client key")
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode DR secondary client key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DR secondary client key: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DR secondary client certificate: %w", err)
	}
	if err := validateDRSecondaryClientCert(cert, time.Now().UTC()); err != nil {
		return nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("DR secondary client certificate public key does not match private key")
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
		Leaf:        cert,
	}, nil
}

// signDRTransportLeafCert creates a short-lived leaf certificate signed by
// the DR transport CA. The leaf contains the node's cluster address as a
// DNS SAN and is suitable for use as the server certificate on DR gRPC
// connections.
func signDRTransportLeafCert(ca *drTransportCA, leafPubKey interface{}, clusterAddr string) ([]byte, error) {
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate leaf serial number: %w", err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "openbao-dr-transport-leaf",
			Organization: []string{"OpenBao DR"},
		},
		NotBefore: now.Add(-1 * time.Minute),
		NotAfter:  now.Add(drTransportLeafValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}

	if clusterAddr != "" {
		template.DNSNames = []string{clusterAddr}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, leafPubKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("failed to sign DR transport leaf certificate: %w", err)
	}

	return leafDER, nil
}

// verifyCertChainToCA checks whether a DER-encoded certificate was signed
// by the given CA certificate. Returns nil if the chain is valid.
func verifyCertChainToCA(certDER []byte, caCert *x509.Certificate) error {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	opts := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		// Validate certificate lifetime at current time.
		CurrentTime: time.Now().UTC(),
	}

	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("certificate does not chain to DR transport CA: %w", err)
	}
	return nil
}

func verifyCertChainToAnyCA(certDER []byte, caCerts []*x509.Certificate) (*x509.Certificate, error) {
	if len(caCerts) == 0 {
		return nil, fmt.Errorf("no DR transport CA configured")
	}

	var lastErr error
	for _, caCert := range caCerts {
		if caCert == nil {
			continue
		}
		if err := verifyCertChainToCA(certDER, caCert); err == nil {
			return caCert, nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no valid DR transport CA configured")
}

type drTransportCATrustBundlePayload struct {
	Version            int      `json:"version"`
	ClusterID          string   `json:"cluster_id"`
	OperationID        string   `json:"operation_id,omitempty"`
	IssuedAt           int64    `json:"issued_at"`
	ActiveCACert       []byte   `json:"active_ca_cert"`
	TrustedCACerts     [][]byte `json:"trusted_ca_certs,omitempty"`
	StagedCACert       []byte   `json:"staged_ca_cert,omitempty"`
	PreviousCACert     []byte   `json:"previous_ca_cert,omitempty"`
	SignatureAlgorithm string   `json:"signature_algorithm"`
	SignerKeyID        string   `json:"signer_key_id"`
}

func drTransportCATrustBundlePayloadBytes(bundle *DRTransportCATrustBundle) ([]byte, error) {
	if bundle == nil {
		return nil, fmt.Errorf("DR transport CA trust bundle is required")
	}
	payload := drTransportCATrustBundlePayload{
		Version:            bundle.Version,
		ClusterID:          bundle.ClusterID,
		OperationID:        bundle.OperationID,
		IssuedAt:           bundle.IssuedAt,
		ActiveCACert:       bundle.ActiveCACert,
		TrustedCACerts:     bundle.TrustedCACerts,
		StagedCACert:       bundle.StagedCACert,
		PreviousCACert:     bundle.PreviousCACert,
		SignatureAlgorithm: bundle.SignatureAlgorithm,
		SignerKeyID:        bundle.SignerKeyID,
	}
	return json.Marshal(payload)
}

func signDRTransportCATrustBundle(bundle *DRTransportCATrustBundle, signer *drTransportCA) error {
	if bundle == nil {
		return fmt.Errorf("DR transport CA trust bundle is required")
	}
	if signer == nil || signer.cert == nil || signer.key == nil {
		return fmt.Errorf("DR transport CA signer is required")
	}
	bundle.SignatureAlgorithm = drTransportCATrustBundleAlgorithm
	bundle.SignerKeyID = signer.spkiHash()
	payload, err := drTransportCATrustBundlePayloadBytes(bundle)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, signer.key, digest[:])
	if err != nil {
		return fmt.Errorf("failed to sign DR transport CA trust bundle: %w", err)
	}
	bundle.Signature = sig
	return nil
}

func parseDRTransportCATrustAnchors(active []byte, trusted [][]byte) (*x509.Certificate, []*x509.Certificate, error) {
	return parseDRPrimaryCACerts(active, trusted)
}

func verifyDRTransportCATrustBundleSignature(bundle *DRTransportCATrustBundle, trusted []*x509.Certificate) error {
	if bundle == nil {
		return fmt.Errorf("DR transport CA trust bundle is required")
	}
	if bundle.Version != drTransportCATrustBundleVersion {
		return fmt.Errorf("unsupported DR transport CA trust bundle version %d", bundle.Version)
	}
	if bundle.SignatureAlgorithm != drTransportCATrustBundleAlgorithm {
		return fmt.Errorf("unsupported DR transport CA trust bundle signature algorithm %q", bundle.SignatureAlgorithm)
	}
	if bundle.SignerKeyID == "" {
		return fmt.Errorf("DR transport CA trust bundle signer key ID is required")
	}
	if len(bundle.Signature) == 0 {
		return fmt.Errorf("DR transport CA trust bundle signature is required")
	}
	payload, err := drTransportCATrustBundlePayloadBytes(bundle)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	for _, cert := range trusted {
		if cert == nil || drTransportCAKeyID(cert) != bundle.SignerKeyID {
			continue
		}
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || pub == nil {
			return fmt.Errorf("DR transport CA signer public key must be ECDSA")
		}
		if ecdsa.VerifyASN1(pub, digest[:], bundle.Signature) {
			return nil
		}
		return fmt.Errorf("DR transport CA trust bundle signature verification failed")
	}
	return fmt.Errorf("DR transport CA trust bundle signer is not trusted")
}

func validateDRTransportCATrustBundleCerts(bundle *DRTransportCATrustBundle) (active *x509.Certificate, trusted []*x509.Certificate, err error) {
	if bundle == nil {
		return nil, nil, fmt.Errorf("DR transport CA trust bundle is required")
	}
	if len(bundle.ActiveCACert) == 0 {
		return nil, nil, fmt.Errorf("DR transport CA trust bundle missing active CA")
	}
	trustedDER := normalizeDRPrimaryCACerts(bundle.ActiveCACert, bundle.TrustedCACerts)
	if len(bundle.StagedCACert) > 0 {
		trustedDER = normalizeDRPrimaryCACerts(bundle.ActiveCACert, append(trustedDER, bundle.StagedCACert))
	}
	if len(bundle.PreviousCACert) > 0 {
		trustedDER = normalizeDRPrimaryCACerts(bundle.ActiveCACert, append(trustedDER, bundle.PreviousCACert))
	}
	active, trusted, err = parseDRPrimaryCACerts(bundle.ActiveCACert, trustedDER)
	if err != nil {
		return nil, nil, err
	}
	for _, cert := range trusted {
		if !cert.BasicConstraintsValid || !cert.IsCA {
			return nil, nil, fmt.Errorf("DR transport CA trust bundle contains non-CA certificate")
		}
		if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
			return nil, nil, fmt.Errorf("DR transport CA trust bundle contains non-ECDSA certificate")
		}
	}
	return active, trusted, nil
}
