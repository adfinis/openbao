// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func newTestDRTransportCACert(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}

	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dr-test-ca"},
		NotBefore:             now.Add(-2 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("failed to parse CA certificate: %v", err)
	}

	return caCert, caKey
}

func newLeafSignedByTestCA(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, notBefore, notAfter time.Time) []byte {
	t.Helper()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate leaf key: %v", err)
	}

	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(notBefore.UnixNano()),
		Subject: pkix.Name{
			CommonName: "openbao-dr-transport-leaf",
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create leaf certificate: %v", err)
	}

	return leafDER
}

func TestVerifyCertChainToCA_AcceptsCurrentlyValidLeaf(t *testing.T) {
	caCert, caKey := newTestDRTransportCACert(t)
	now := time.Now().UTC()
	leafDER := newLeafSignedByTestCA(t, caCert, caKey, now.Add(-30*time.Minute), now.Add(30*time.Minute))

	if err := verifyCertChainToCA(leafDER, caCert); err != nil {
		t.Fatalf("expected currently valid leaf to verify, got: %v", err)
	}
}

func TestVerifyCertChainToCA_RejectsExpiredLeaf(t *testing.T) {
	caCert, caKey := newTestDRTransportCACert(t)
	now := time.Now().UTC()
	leafDER := newLeafSignedByTestCA(t, caCert, caKey, now.Add(-3*time.Hour), now.Add(-2*time.Hour))

	if err := verifyCertChainToCA(leafDER, caCert); err == nil {
		t.Fatal("expected expired leaf verification to fail")
	}
}

func TestVerifyCertChainToCA_RejectsNotYetValidLeaf(t *testing.T) {
	caCert, caKey := newTestDRTransportCACert(t)
	now := time.Now().UTC()
	leafDER := newLeafSignedByTestCA(t, caCert, caKey, now.Add(2*time.Hour), now.Add(3*time.Hour))

	if err := verifyCertChainToCA(leafDER, caCert); err == nil {
		t.Fatal("expected not-yet-valid leaf verification to fail")
	}
}
