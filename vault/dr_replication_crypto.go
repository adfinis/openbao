// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	drWrappedRootKeyAADVersion = uint32(1)
	drBootstrapNonceSize       = 32
	drServerNonceSize          = 32
	drGCMIVSize                = 12
	drWrapKeySize              = 32
)

// drWrapAAD defines the Additional Authenticated Data used in the bootstrap
// ECDH + AEAD key wrapping. All fields are bound into the AES-GCM AAD to
// prevent cross-cluster replay, unknown-key-share, and confused-deputy attacks.
type drWrapAAD struct {
	// RelationshipID binds the wrapped material to a specific DR relationship.
	RelationshipID string `json:"relationship_id"`

	// ClusterID binds to the primary cluster, preventing cross-cluster replay.
	ClusterID string `json:"cluster_id"`

	// SecondaryCertFP is the SHA-256 fingerprint of the secondary's cluster
	// certificate, binding to the secondary's identity.
	SecondaryCertFP string `json:"secondary_cert_fp"`

	// PrimaryIdentity is the SHA-256 SPKI hash of the DR transport CA public
	// key, binding to the primary's long-lived identity.
	PrimaryIdentity string `json:"primary_identity"`

	// Version is the protocol version for forward compatibility.
	Version uint32 `json:"version"`

	// ClientNonceHex is the hex-encoded client nonce, ensuring freshness.
	ClientNonceHex string `json:"client_nonce_hex"`
}

// buildDRWrapAAD constructs the AAD for bootstrap key wrapping. All
// parameters are required to produce a valid AAD that binds the wrapped
// material to the specific cluster, relationship, and peer identities.
func buildDRWrapAAD(relationshipID, clusterID, secondaryCertFP, primaryIdentity string, clientNonce []byte) ([]byte, error) {
	if relationshipID == "" {
		return nil, fmt.Errorf("relationship_id is required")
	}
	if clusterID == "" {
		return nil, fmt.Errorf("cluster_id is required")
	}
	if secondaryCertFP == "" {
		return nil, fmt.Errorf("secondary certificate fingerprint is required")
	}
	if primaryIdentity == "" {
		return nil, fmt.Errorf("primary identity is required")
	}
	if len(clientNonce) != drBootstrapNonceSize {
		return nil, fmt.Errorf("client nonce must be %d bytes", drBootstrapNonceSize)
	}

	aad := drWrapAAD{
		RelationshipID:  relationshipID,
		ClusterID:       clusterID,
		SecondaryCertFP: secondaryCertFP,
		PrimaryIdentity: primaryIdentity,
		Version:         drWrappedRootKeyAADVersion,
		ClientNonceHex:  hex.EncodeToString(clientNonce),
	}
	return json.Marshal(aad)
}

func wrapRootKeyForSecondary(rootKey []byte, relationshipID, clusterID, secondaryCertFP, primaryIdentity string, clientPubBytes []byte, clientNonce []byte) (wrappedRootKey []byte, serverPub []byte, serverNonce []byte, gcmIV []byte, aadVersion uint32, err error) {
	if len(rootKey) == 0 {
		return nil, nil, nil, nil, 0, fmt.Errorf("root key is required")
	}
	curve := ecdh.X25519()

	clientPub, err := curve.NewPublicKey(clientPubBytes)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("invalid client ephemeral public key: %w", err)
	}

	serverPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to generate server ephemeral key: %w", err)
	}

	sharedSecret, err := serverPriv.ECDH(clientPub)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	// Generate separate server nonce (for HKDF salt) and GCM IV.
	// These serve distinct roles: the server nonce contributes to key
	// derivation entropy, while the GCM IV is the AES-GCM initialization
	// vector. Keeping them separate avoids "same bytes, two roles" concerns.
	serverNonce = make([]byte, drServerNonceSize)
	if _, err := rand.Read(serverNonce); err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to generate server nonce: %w", err)
	}

	gcmIV = make([]byte, drGCMIVSize)
	if _, err := rand.Read(gcmIV); err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to generate GCM IV: %w", err)
	}

	wrapKey, err := deriveDRWrapKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to create AEAD: %w", err)
	}

	aad, err := buildDRWrapAAD(relationshipID, clusterID, secondaryCertFP, primaryIdentity, clientNonce)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("failed to build wrap AAD: %w", err)
	}

	wrappedRootKey = aead.Seal(nil, gcmIV, rootKey, aad)
	serverPub = serverPriv.PublicKey().Bytes()
	return wrappedRootKey, serverPub, serverNonce, gcmIV, drWrappedRootKeyAADVersion, nil
}

func unwrapRootKeyFromPrimary(wrappedRootKey []byte, relationshipID, clusterID, secondaryCertFP, primaryIdentity string, serverPubBytes []byte, clientPriv *ecdh.PrivateKey, clientNonce []byte, serverNonce []byte, gcmIV []byte, aadVersion uint32) ([]byte, error) {
	if aadVersion != drWrappedRootKeyAADVersion {
		return nil, fmt.Errorf("unsupported wrapped root key AAD version: %d", aadVersion)
	}
	if len(wrappedRootKey) == 0 {
		return nil, fmt.Errorf("wrapped root key is required")
	}
	if clientPriv == nil {
		return nil, fmt.Errorf("client ephemeral private key is required")
	}
	if len(serverNonce) != drServerNonceSize {
		return nil, fmt.Errorf("server nonce must be %d bytes", drServerNonceSize)
	}
	if len(gcmIV) != drGCMIVSize {
		return nil, fmt.Errorf("GCM IV must be %d bytes", drGCMIVSize)
	}

	curve := ecdh.X25519()
	serverPub, err := curve.NewPublicKey(serverPubBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid server ephemeral public key: %w", err)
	}

	sharedSecret, err := clientPriv.ECDH(serverPub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	wrapKey, err := deriveDRWrapKey(sharedSecret, clientNonce, serverNonce)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create AEAD: %w", err)
	}

	aad, err := buildDRWrapAAD(relationshipID, clusterID, secondaryCertFP, primaryIdentity, clientNonce)
	if err != nil {
		return nil, fmt.Errorf("failed to build wrap AAD: %w", err)
	}

	rootKey, err := aead.Open(nil, gcmIV, wrappedRootKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to unwrap root key: %w", err)
	}
	return rootKey, nil
}

func deriveDRWrapKey(sharedSecret []byte, clientNonce []byte, serverNonce []byte) ([]byte, error) {
	// HKDF salt = client_nonce || server_nonce. Both are 32-byte random
	// values generated independently by each party. This ensures each
	// bootstrap handshake derives a unique wrap key.
	salt := make([]byte, 0, len(clientNonce)+len(serverNonce))
	salt = append(salt, clientNonce...)
	salt = append(salt, serverNonce...)

	reader := hkdf.New(sha256.New, sharedSecret, salt, []byte("openbao-dr-root-key-wrap-v1"))
	key := make([]byte, drWrapKeySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("failed to derive wrap key: %w", err)
	}
	return key, nil
}

func certFingerprintSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

var wrapRootKeyForDRSync = wrapRootKeyForSecondary

func certFingerprintSHA256RawDER(certDER []byte) string {
	if len(certDER) == 0 {
		return ""
	}
	sum := sha256.Sum256(certDER)
	return hex.EncodeToString(sum[:])
}

func certFingerprintSHA256DER(certDER []byte) string {
	if len(certDER) == 0 {
		return ""
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return ""
	}
	return certFingerprintSHA256(cert)
}
