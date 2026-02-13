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
	drWrapNonceSize            = 12
	drWrapKeySize              = 32
)

type drWrapAAD struct {
	RelationshipID string `json:"relationship_id"`
	Version        uint32 `json:"version"`
	ClientNonceHex string `json:"client_nonce_hex"`
}

func buildDRWrapAAD(relationshipID string, clientNonce []byte) ([]byte, error) {
	aad := drWrapAAD{
		RelationshipID: relationshipID,
		Version:        drWrappedRootKeyAADVersion,
		ClientNonceHex: hex.EncodeToString(clientNonce),
	}
	return json.Marshal(aad)
}

func wrapRootKeyForSecondary(rootKey []byte, relationshipID string, clientPubBytes []byte, clientNonce []byte) (wrappedRootKey []byte, serverPub []byte, wrapNonce []byte, aadVersion uint32, err error) {
	curve := ecdh.X25519()

	clientPub, err := curve.NewPublicKey(clientPubBytes)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("invalid client ephemeral public key: %w", err)
	}

	serverPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to generate server ephemeral key: %w", err)
	}

	sharedSecret, err := serverPriv.ECDH(clientPub)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	wrapNonce = make([]byte, drWrapNonceSize)
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to generate wrap nonce: %w", err)
	}

	wrapKey, err := deriveDRWrapKey(sharedSecret, clientNonce, wrapNonce)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to create AEAD: %w", err)
	}

	aad, err := buildDRWrapAAD(relationshipID, clientNonce)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to build wrap AAD: %w", err)
	}

	wrappedRootKey = aead.Seal(nil, wrapNonce, rootKey, aad)
	serverPub = serverPriv.PublicKey().Bytes()
	return wrappedRootKey, serverPub, wrapNonce, drWrappedRootKeyAADVersion, nil
}

func unwrapRootKeyFromPrimary(wrappedRootKey []byte, relationshipID string, serverPubBytes []byte, clientPriv *ecdh.PrivateKey, clientNonce []byte, wrapNonce []byte, aadVersion uint32) ([]byte, error) {
	if aadVersion != drWrappedRootKeyAADVersion {
		return nil, fmt.Errorf("unsupported wrapped root key AAD version: %d", aadVersion)
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

	wrapKey, err := deriveDRWrapKey(sharedSecret, clientNonce, wrapNonce)
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

	aad, err := buildDRWrapAAD(relationshipID, clientNonce)
	if err != nil {
		return nil, fmt.Errorf("failed to build wrap AAD: %w", err)
	}

	rootKey, err := aead.Open(nil, wrapNonce, wrappedRootKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to unwrap root key: %w", err)
	}
	return rootKey, nil
}

func deriveDRWrapKey(sharedSecret []byte, clientNonce []byte, wrapNonce []byte) ([]byte, error) {
	// Bind key derivation to both the request nonce and the per-response
	// wrap nonce so each bootstrap response gets a unique key stream.
	salt := make([]byte, 0, len(clientNonce)+len(wrapNonce))
	salt = append(salt, clientNonce...)
	salt = append(salt, wrapNonce...)

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
