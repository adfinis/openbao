// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"strings"
	"testing"
)

func TestDRRootKeyWrapAADBindingRejectsMismatches(t *testing.T) {
	clientPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)
	rootKey := randomBytesForTest(t, 32)

	wrappedRootKey, serverPub, serverNonce, gcmIV, aadVersion, err := wrapRootKeyForSecondary(
		rootKey,
		"rel-a",
		"cluster-a",
		"secondary-fp-a",
		"primary-identity-a",
		clientPriv.PublicKey().Bytes(),
		clientNonce,
	)
	if err != nil {
		t.Fatal(err)
	}

	unwrapped, err := unwrapRootKeyFromPrimary(
		wrappedRootKey,
		"rel-a",
		"cluster-a",
		"secondary-fp-a",
		"primary-identity-a",
		serverPub,
		clientPriv,
		clientNonce,
		serverNonce,
		gcmIV,
		aadVersion,
	)
	if err != nil {
		t.Fatalf("expected baseline unwrap to succeed: %v", err)
	}
	if !bytes.Equal(unwrapped, rootKey) {
		t.Fatal("baseline unwrap returned the wrong root key")
	}

	for _, tc := range []struct {
		name             string
		relationshipID   string
		clusterID        string
		secondaryCertFP  string
		primaryIdentity  string
		clientNonce      []byte
		serverNonce      []byte
		gcmIV            []byte
		wrappedRootKey   []byte
		wrapAADVersion   uint32
		clientPrivateKey *ecdh.PrivateKey
	}{
		{
			name:             "relationship",
			relationshipID:   "rel-b",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "cluster",
			relationshipID:   "rel-a",
			clusterID:        "cluster-b",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "secondary fingerprint",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-b",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "primary identity",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-b",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "client nonce",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      flipFirstByte(clientNonce),
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "server nonce",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      flipFirstByte(serverNonce),
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "GCM IV",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            flipFirstByte(gcmIV),
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "wrapped root key",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   flipFirstByte(wrappedRootKey),
			wrapAADVersion:   aadVersion,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "AAD version",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion + 1,
			clientPrivateKey: clientPriv,
		},
		{
			name:             "client private key",
			relationshipID:   "rel-a",
			clusterID:        "cluster-a",
			secondaryCertFP:  "secondary-fp-a",
			primaryIdentity:  "primary-identity-a",
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrappedRootKey:   wrappedRootKey,
			wrapAADVersion:   aadVersion,
			clientPrivateKey: mustGenerateX25519KeyForTest(t),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unwrapRootKeyFromPrimary(
				tc.wrappedRootKey,
				tc.relationshipID,
				tc.clusterID,
				tc.secondaryCertFP,
				tc.primaryIdentity,
				serverPub,
				tc.clientPrivateKey,
				tc.clientNonce,
				tc.serverNonce,
				tc.gcmIV,
				tc.wrapAADVersion,
			)
			if err == nil {
				t.Fatalf("expected unwrap mismatch to fail, got root key %x", got)
			}
		})
	}
}

func TestDRRootKeyWrapRequiresCompleteInputs(t *testing.T) {
	clientPriv := mustGenerateX25519KeyForTest(t)
	rootKey := randomBytesForTest(t, 32)
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)

	for _, tc := range []struct {
		name            string
		rootKey         []byte
		relationshipID  string
		clusterID       string
		secondaryCertFP string
		primaryIdentity string
		clientPub       []byte
		clientNonce     []byte
		wantErr         string
	}{
		{
			name:            "missing root key",
			relationshipID:  "rel-a",
			clusterID:       "cluster-a",
			secondaryCertFP: "secondary-fp-a",
			primaryIdentity: "primary-identity-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce,
			wantErr:         "root key",
		},
		{
			name:            "missing relationship",
			rootKey:         rootKey,
			clusterID:       "cluster-a",
			secondaryCertFP: "secondary-fp-a",
			primaryIdentity: "primary-identity-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce,
			wantErr:         "relationship_id",
		},
		{
			name:            "missing cluster",
			rootKey:         rootKey,
			relationshipID:  "rel-a",
			secondaryCertFP: "secondary-fp-a",
			primaryIdentity: "primary-identity-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce,
			wantErr:         "cluster_id",
		},
		{
			name:            "missing secondary fingerprint",
			rootKey:         rootKey,
			relationshipID:  "rel-a",
			clusterID:       "cluster-a",
			primaryIdentity: "primary-identity-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce,
			wantErr:         "secondary certificate fingerprint",
		},
		{
			name:            "missing primary identity",
			rootKey:         rootKey,
			relationshipID:  "rel-a",
			clusterID:       "cluster-a",
			secondaryCertFP: "secondary-fp-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce,
			wantErr:         "primary identity",
		},
		{
			name:            "short client nonce",
			rootKey:         rootKey,
			relationshipID:  "rel-a",
			clusterID:       "cluster-a",
			secondaryCertFP: "secondary-fp-a",
			primaryIdentity: "primary-identity-a",
			clientPub:       clientPriv.PublicKey().Bytes(),
			clientNonce:     clientNonce[:drBootstrapNonceSize-1],
			wantErr:         "client nonce",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, err := wrapRootKeyForSecondary(
				tc.rootKey,
				tc.relationshipID,
				tc.clusterID,
				tc.secondaryCertFP,
				tc.primaryIdentity,
				tc.clientPub,
				tc.clientNonce,
			)
			if err == nil {
				t.Fatal("expected wrap input validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDRRootKeyWrapProducesFreshEnvelopeForRepeatedClientNonce(t *testing.T) {
	clientPriv := mustGenerateX25519KeyForTest(t)
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)
	rootKey := randomBytesForTest(t, 32)

	wrapped1, serverPub1, serverNonce1, gcmIV1, aadVersion1, err := wrapRootKeyForSecondary(
		rootKey,
		"rel-a",
		"cluster-a",
		"secondary-fp-a",
		"primary-identity-a",
		clientPriv.PublicKey().Bytes(),
		clientNonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped2, serverPub2, serverNonce2, gcmIV2, aadVersion2, err := wrapRootKeyForSecondary(
		rootKey,
		"rel-a",
		"cluster-a",
		"secondary-fp-a",
		"primary-identity-a",
		clientPriv.PublicKey().Bytes(),
		clientNonce,
	)
	if err != nil {
		t.Fatal(err)
	}

	if aadVersion1 != drWrappedRootKeyAADVersion || aadVersion2 != drWrappedRootKeyAADVersion {
		t.Fatalf("unexpected AAD versions: %d %d", aadVersion1, aadVersion2)
	}
	if bytes.Equal(serverPub1, serverPub2) {
		t.Fatal("expected fresh server ephemeral public keys")
	}
	if bytes.Equal(serverNonce1, serverNonce2) {
		t.Fatal("expected fresh server nonces")
	}
	if bytes.Equal(gcmIV1, gcmIV2) {
		t.Fatal("expected fresh GCM IVs")
	}
	if bytes.Equal(wrapped1, wrapped2) {
		t.Fatal("expected fresh wrapped root-key ciphertext")
	}
}

func TestDRRootKeyUnwrapRejectsMalformedEnvelope(t *testing.T) {
	clientPriv := mustGenerateX25519KeyForTest(t)
	clientNonce := randomBytesForTest(t, drBootstrapNonceSize)
	rootKey := randomBytesForTest(t, 32)

	wrappedRootKey, serverPub, serverNonce, gcmIV, aadVersion, err := wrapRootKeyForSecondary(
		rootKey,
		"rel-a",
		"cluster-a",
		"secondary-fp-a",
		"primary-identity-a",
		clientPriv.PublicKey().Bytes(),
		clientNonce,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name             string
		wrappedRootKey   []byte
		clientPrivateKey *ecdh.PrivateKey
		clientNonce      []byte
		serverNonce      []byte
		gcmIV            []byte
		wrapAADVersion   uint32
		wantErr          string
	}{
		{
			name:             "missing wrapped root key",
			clientPrivateKey: clientPriv,
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrapAADVersion:   aadVersion,
			wantErr:          "wrapped root key",
		},
		{
			name:           "missing private key",
			wrappedRootKey: wrappedRootKey,
			clientNonce:    clientNonce,
			serverNonce:    serverNonce,
			gcmIV:          gcmIV,
			wrapAADVersion: aadVersion,
			wantErr:        "private key",
		},
		{
			name:             "short client nonce",
			wrappedRootKey:   wrappedRootKey,
			clientPrivateKey: clientPriv,
			clientNonce:      clientNonce[:drBootstrapNonceSize-1],
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrapAADVersion:   aadVersion,
			wantErr:          "client nonce",
		},
		{
			name:             "short server nonce",
			wrappedRootKey:   wrappedRootKey,
			clientPrivateKey: clientPriv,
			clientNonce:      clientNonce,
			serverNonce:      serverNonce[:drServerNonceSize-1],
			gcmIV:            gcmIV,
			wrapAADVersion:   aadVersion,
			wantErr:          "server nonce",
		},
		{
			name:             "short GCM IV",
			wrappedRootKey:   wrappedRootKey,
			clientPrivateKey: clientPriv,
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV[:drGCMIVSize-1],
			wrapAADVersion:   aadVersion,
			wantErr:          "GCM IV",
		},
		{
			name:             "bad AAD version",
			wrappedRootKey:   wrappedRootKey,
			clientPrivateKey: clientPriv,
			clientNonce:      clientNonce,
			serverNonce:      serverNonce,
			gcmIV:            gcmIV,
			wrapAADVersion:   aadVersion + 1,
			wantErr:          "AAD version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unwrapRootKeyFromPrimary(
				tc.wrappedRootKey,
				"rel-a",
				"cluster-a",
				"secondary-fp-a",
				"primary-identity-a",
				serverPub,
				tc.clientPrivateKey,
				tc.clientNonce,
				tc.serverNonce,
				tc.gcmIV,
				tc.wrapAADVersion,
			)
			if err == nil {
				t.Fatal("expected unwrap validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func randomBytesForTest(t *testing.T, size int) []byte {
	t.Helper()

	out := make([]byte, size)
	if _, err := rand.Read(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func flipFirstByte(in []byte) []byte {
	out := append([]byte(nil), in...)
	out[0] ^= 0xff
	return out
}

func mustGenerateX25519KeyForTest(t *testing.T) *ecdh.PrivateKey {
	t.Helper()

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}
