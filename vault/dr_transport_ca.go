// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
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
}

// drTransportCA holds the parsed DR transport CA material.
type drTransportCA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey
}

// spkiHash returns the SHA-256 hash of the CA's SubjectPublicKeyInfo,
// suitable for inclusion in bootstrap AAD as the primary identity.
func (ca *drTransportCA) spkiHash() string {
	spki, err := x509.MarshalPKIXPublicKey(ca.cert.PublicKey)
	if err != nil {
		// Should not happen for a valid ECDSA key.
		return ""
	}
	h := sha256.Sum256(spki)
	return hex.EncodeToString(h[:])
}

// generateDRTransportCA creates a new P-256 ECDSA CA key pair for DR
// transport and persists it to barrier storage.
func generateDRTransportCA(core *Core) (*drTransportCA, error) {
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

	bundle := &drTransportCABundle{
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		CertDER:   certDER,
		CreatedAt: now,
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

	return &drTransportCA{cert: cert, certDER: certDER, key: key}, nil
}

// loadDRTransportCA loads the DR transport CA from barrier storage.
// Returns nil, nil if no CA has been generated yet.
func loadDRTransportCA(core *Core) (*drTransportCA, error) {
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

	block, _ := pem.Decode(bundle.KeyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode CA private key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA private key: %w", err)
	}

	cert, err := x509.ParseCertificate(bundle.CertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	return &drTransportCA{cert: cert, certDER: bundle.CertDER, key: key}, nil
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
