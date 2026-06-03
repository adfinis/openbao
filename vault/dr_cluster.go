// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/vault/cluster"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for DR gRPC server
	"google.golang.org/grpc/keepalive"
)

// drPeerFingerprintContextKey stores the authenticated peer certificate
// fingerprint for DR RPC authz fallback when gRPC AuthInfo is not populated.
type drPeerFingerprintContextKey struct{}

// drPeerFingerprintByRemoteAddr tracks authenticated DR peer fingerprints by
// remote address for RPC authz fallback when gRPC AuthInfo is unavailable.
var drPeerFingerprintByRemoteAddr sync.Map

var errDRTransportLeafKeyUnavailable = errors.New("no local cluster private key available")

// --- DR Replication Cluster Handler (primary side) ---

// drReplicationClusterHandler implements the cluster.Handler interface for
// serving DR replication gRPC connections over mTLS on the cluster port.
type drReplicationClusterHandler struct {
	core       *Core
	grpcServer *grpc.Server
	fws        *http2.Server
	logger     log.Logger
	stopCh     chan struct{}

	// drLeafCertMu protects drLeafCertDER / drLeafParsedCert / drLeafPrivateKey.
	drLeafCertMu     sync.RWMutex
	drTransportCA    *drTransportCA
	drLeafCertDER    []byte // DER-encoded leaf cert signed by DR transport CA
	drLeafParsedCert *x509.Certificate
	drLeafPrivateKey *ecdsa.PrivateKey // snapshot of the key used when minting the leaf

	// certMu protects trustedSecondaryCerts.
	certMu sync.RWMutex
	// trustedSecondaryCerts holds per-relationship trusted certificates.
	trustedSecondaryCerts map[string][]*x509.Certificate
}

// newDRReplicationClusterHandler creates and registers the DR gRPC handler
// on the cluster listener.
const (
	// drGRPCMaxMessageSize bounds the maximum gRPC message size for the
	// DR replication service. This limits decompression cost for gzip
	// messages and prevents unbounded memory allocation.
	drGRPCMaxMessageSize = 16 * 1024 * 1024 // 16 MiB
)

func newDRReplicationClusterHandler(core *Core, drServer *drReplicationPrimary, logger log.Logger) *drReplicationClusterHandler {
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time: 2 * core.clusterHeartbeatInterval,
		}),
		grpc.MaxRecvMsgSize(drGRPCMaxMessageSize),
		grpc.MaxSendMsgSize(drGRPCMaxMessageSize),
	)
	RegisterDRReplicationServer(grpcServer, drServer)

	return &drReplicationClusterHandler{
		core:                  core,
		grpcServer:            grpcServer,
		fws:                   &http2.Server{},
		logger:                logger.Named("dr-cluster-handler"),
		stopCh:                make(chan struct{}),
		trustedSecondaryCerts: make(map[string][]*x509.Certificate),
	}
}

// SetTransportCA mints a short-lived leaf certificate signed by the DR
// transport CA using this node's cluster key pair. The leaf is presented
// to secondaries in the TLS handshake so they can verify it chains to
// the CA from the activation token.
func (h *drReplicationClusterHandler) SetTransportCA(ca *drTransportCA) error {
	if ca == nil {
		return errors.New("no DR transport CA available")
	}

	h.drLeafCertMu.Lock()
	h.drTransportCA = ca
	h.drLeafCertMu.Unlock()

	privKey := h.core.localClusterPrivateKey.Load()
	if privKey == nil {
		return errDRTransportLeafKeyUnavailable
	}

	clusterAddr := h.core.ClusterAddr()

	leafDER, err := signDRTransportLeafCert(ca, &privKey.PublicKey, clusterAddr)
	if err != nil {
		return fmt.Errorf("failed to mint DR leaf certificate: %w", err)
	}

	parsed, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return fmt.Errorf("failed to parse minted DR leaf certificate: %w", err)
	}

	h.drLeafCertMu.Lock()
	h.drLeafCertDER = leafDER
	h.drLeafParsedCert = parsed
	h.drLeafPrivateKey = privKey // snapshot: must match the public key in the leaf cert
	h.drLeafCertMu.Unlock()

	h.logger.Info("minted DR transport leaf certificate",
		"cn", parsed.Subject.CommonName,
		"not_after", parsed.NotAfter,
		"fingerprint", drCertFingerprint(leafDER))

	return nil
}

func (h *drReplicationClusterHandler) hasDRLeafCert() bool {
	h.drLeafCertMu.RLock()
	defer h.drLeafCertMu.RUnlock()
	return len(h.drLeafCertDER) != 0 && h.drLeafParsedCert != nil && h.drLeafPrivateKey != nil
}

func (h *drReplicationClusterHandler) ensureDRLeafCert() error {
	h.drLeafCertMu.RLock()
	hasLeaf := len(h.drLeafCertDER) != 0 && h.drLeafParsedCert != nil && h.drLeafPrivateKey != nil
	ca := h.drTransportCA
	h.drLeafCertMu.RUnlock()

	if hasLeaf {
		return nil
	}
	if ca == nil {
		return errors.New("no DR transport CA available")
	}
	return h.SetTransportCA(ca)
}

func (h *drReplicationClusterHandler) currentTransportCA() *drTransportCA {
	h.drLeafCertMu.RLock()
	defer h.drLeafCertMu.RUnlock()
	return h.drTransportCA
}

// ActiveDRLeafCertDER returns a copy of the currently minted DR transport
// leaf certificate DER bytes. Returns nil when no DR leaf is available.
func (h *drReplicationClusterHandler) ActiveDRLeafCertDER() []byte {
	if h == nil {
		return nil
	}

	_ = h.ensureDRLeafCert()

	h.drLeafCertMu.RLock()
	defer h.drLeafCertMu.RUnlock()

	if len(h.drLeafCertDER) == 0 {
		return nil
	}
	out := make([]byte, len(h.drLeafCertDER))
	copy(out, h.drLeafCertDER)
	return out
}

// ServerLookup returns the DR transport leaf certificate for incoming
// connections. DR transport identity is always rooted in the DR transport CA;
// if a leaf is unavailable, fail closed instead of serving the node's
// self-signed cluster cert.
func (h *drReplicationClusterHandler) ServerLookup(ctx context.Context, clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.drLeafCertMu.RLock()
	leafDER := h.drLeafCertDER
	leafParsed := h.drLeafParsedCert
	leafKey := h.drLeafPrivateKey
	h.drLeafCertMu.RUnlock()

	if leafDER == nil || leafParsed == nil || leafKey == nil {
		_ = h.ensureDRLeafCert()
		h.drLeafCertMu.RLock()
		leafDER = h.drLeafCertDER
		leafParsed = h.drLeafParsedCert
		leafKey = h.drLeafPrivateKey
		h.drLeafCertMu.RUnlock()
	}

	if leafDER != nil && leafParsed != nil && leafKey != nil {
		return &tls.Certificate{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey, // use the snapshot taken at mint time
			Leaf:        leafParsed,
		}, nil
	}

	return nil, errors.New("dr replication connection but DR transport leaf cert is unavailable")
}

// startLeafRenewal launches a background goroutine that re-mints the DR
// leaf certificate at half the leaf validity interval. The goroutine
// stops when the handler's stopCh is closed.
func (h *drReplicationClusterHandler) startLeafRenewal(ca *drTransportCA) {
	h.drLeafCertMu.Lock()
	h.drTransportCA = ca
	h.drLeafCertMu.Unlock()

	renewInterval := drTransportLeafValidity / 2
	retryTicker := time.NewTicker(time.Second)
	go func() {
		renewTicker := time.NewTicker(renewInterval)
		defer retryTicker.Stop()
		defer renewTicker.Stop()
		for {
			select {
			case <-retryTicker.C:
				if h.hasDRLeafCert() {
					continue
				}
				currentCA := h.currentTransportCA()
				if currentCA == nil {
					currentCA = ca
				}
				if err := h.SetTransportCA(currentCA); err != nil {
					h.logger.Debug("DR transport leaf cert not available yet", "error", err)
				}
			case <-renewTicker.C:
				currentCA := h.currentTransportCA()
				if currentCA == nil {
					currentCA = ca
				}
				if err := h.SetTransportCA(currentCA); err != nil {
					h.logger.Error("failed to renew DR transport leaf cert", "error", err)
				} else {
					h.logger.Info("renewed DR transport leaf certificate")
				}
			case <-h.stopCh:
				return
			}
		}
	}()
}

// CALookup returns the CA certificates for verifying client connections.
// Includes the primary's own cert plus any registered secondary certs.
func (h *drReplicationClusterHandler) CALookup(ctx context.Context) ([]*x509.Certificate, error) {
	parsedCert := h.core.localClusterParsedCert.Load()
	if parsedCert == nil {
		return nil, errors.New("dr replication CA lookup but no local cert")
	}
	h.certMu.RLock()
	defer h.certMu.RUnlock()
	total := 1
	for _, relCerts := range h.trustedSecondaryCerts {
		total += len(relCerts)
	}
	certs := make([]*x509.Certificate, 0, total)
	certs = append(certs, parsedCert)
	for _, relCerts := range h.trustedSecondaryCerts {
		certs = append(certs, relCerts...)
	}
	return certs, nil
}

// AddTrustedCert adds a secondary's CA certificate to the trusted pool.
// This allows the secondary to establish mTLS connections to this primary.
func (h *drReplicationClusterHandler) AddTrustedCert(relationshipID string, cert *x509.Certificate) {
	h.certMu.Lock()
	defer h.certMu.Unlock()

	// Don't add if it matches the primary's own cert.
	if primaryCert := h.core.localClusterParsedCert.Load(); primaryCert != nil {
		if bytes.Equal(primaryCert.Raw, cert.Raw) {
			return
		}
	}

	// Avoid duplicates by checking raw bytes.
	for _, existing := range h.trustedSecondaryCerts[relationshipID] {
		if bytes.Equal(existing.Raw, cert.Raw) {
			return
		}
	}

	h.trustedSecondaryCerts[relationshipID] = append(h.trustedSecondaryCerts[relationshipID], cert)
	h.logger.Info("added trusted secondary cert",
		"relationship_id", relationshipID,
		"subject", cert.Subject.CommonName,
		"issuer", cert.Issuer.CommonName)
}

func (h *drReplicationClusterHandler) RemoveTrustedRelationship(relationshipID string) {
	h.certMu.Lock()
	defer h.certMu.Unlock()

	delete(h.trustedSecondaryCerts, relationshipID)
	h.logger.Info("removed trusted relationship certificates", "relationship_id", relationshipID)
}

// Handoff accepts a TLS connection and serves it via the gRPC server.
func (h *drReplicationClusterHandler) Handoff(ctx context.Context, shutdownWg *sync.WaitGroup, closeCh chan struct{}, tlsConn *tls.Conn) error {
	h.logger.Debug("got DR replication connection")

	peerFingerprint := ""
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		peerFingerprint = certFingerprintSHA256(state.PeerCertificates[0])
	}
	remoteAddr := tlsConn.RemoteAddr().String()
	if peerFingerprint != "" {
		drPeerFingerprintByRemoteAddr.Store(remoteAddr, peerFingerprint)
	}

	shutdownWg.Add(2)
	quitCh := make(chan struct{})

	go func() {
		select {
		case <-quitCh:
		case <-closeCh:
		case <-h.stopCh:
		}
		tlsConn.Close()
		if remoteAddr != "" {
			drPeerFingerprintByRemoteAddr.Delete(remoteAddr)
		}
		shutdownWg.Done()
	}()

	go func() {
		h.fws.ServeConn(tlsConn, &http2.ServeConnOpts{
			Handler: h.grpcServer,
			BaseConfig: &http.Server{
				ErrorLog: h.logger.StandardLogger(nil),
				ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
					if peerFingerprint == "" {
						return ctx
					}
					return context.WithValue(ctx, drPeerFingerprintContextKey{}, peerFingerprint)
				},
			},
		})
		close(quitCh)
		shutdownWg.Done()
	}()

	return nil
}

// Stop shuts down the DR gRPC server, allowing a brief drain period
// for in-flight RPCs to complete.
func (h *drReplicationClusterHandler) Stop() error {
	// Give existing RPCs time to drain, matching the request forwarding
	// handler pattern.
	time.Sleep(cluster.ListenerAcceptDeadline)
	select {
	case <-h.stopCh:
	default:
		close(h.stopCh)
	}
	h.grpcServer.Stop()
	return nil
}

// --- DR Replication Cluster Client (secondary side) ---

// trustedPrimaryCert holds a cached primary cluster certificate along
// with a timestamp of when it was last seen in a heartbeat response.
// Certificates whose lastSeen exceeds trustedCertTTL are pruned.
type trustedPrimaryCert struct {
	derBytes   []byte
	lastSeen   time.Time
	caIdentity string
}

const trustedCertTTL = 10 * time.Minute

// drCertFingerprint computes the SHA-256 hex fingerprint of a
// DER-encoded certificate.
func drCertFingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// drReplicationClusterClient implements the cluster.Client interface for
// connecting to the primary's DR replication gRPC service over mTLS.
type drReplicationClusterClient struct {
	core *Core
	caMu sync.RWMutex
	// primaryCACert is the active primary DR transport CA certificate from
	// the activation token. It is retained for the cluster.Client interface
	// and as a fallback before the TLS verifier records the matched CA.
	primaryCACert *x509.Certificate

	// primaryCACerts is the ordered primary DR transport CA trust set. The
	// active CA is first; later entries are staged/previous CAs accepted
	// during transport CA rotation.
	primaryCACerts []*x509.Certificate
	logger         log.Logger

	// trustedCertsMu and trustedCerts are owned by the
	// drReplicationSecondary and shared via pointer so the pool
	// persists across reconnects.
	trustedCertsMu *sync.RWMutex
	trustedCerts   map[string]*trustedPrimaryCert

	clientCert *tls.Certificate

	clientCertMu sync.RWMutex
	clientCertFP string

	serverIdentityMu sync.RWMutex
	serverCAIdentity string
}

// ClientLookup returns the client TLS certificate for outgoing connections.
func (c *drReplicationClusterClient) ClientLookup(ctx context.Context, requestInfo *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if c.clientCert == nil || c.clientCert.Leaf == nil {
		return nil, nil
	}
	for _, subj := range requestInfo.AcceptableCAs {
		if bytes.Equal(subj, c.clientCert.Leaf.RawSubject) || bytes.Equal(subj, c.clientCert.Leaf.RawIssuer) {
			c.setLastClientCertFingerprint(certFingerprintSHA256(c.clientCert.Leaf))
			cert := *c.clientCert
			return &cert, nil
		}
	}
	return nil, nil
}

func (c *drReplicationClusterClient) setLastClientCertFingerprint(fp string) {
	if c == nil || fp == "" {
		return
	}
	c.clientCertMu.Lock()
	c.clientCertFP = fp
	c.clientCertMu.Unlock()
}

func (c *drReplicationClusterClient) LastClientCertFingerprint() string {
	if c == nil {
		return ""
	}
	c.clientCertMu.RLock()
	fp := c.clientCertFP
	c.clientCertMu.RUnlock()
	return fp
}

func (c *drReplicationClusterClient) setLastPrimaryCAIdentity(identity string) {
	if c == nil || identity == "" {
		return
	}
	c.serverIdentityMu.Lock()
	c.serverCAIdentity = identity
	c.serverIdentityMu.Unlock()
}

func (c *drReplicationClusterClient) LastPrimaryCAIdentity() string {
	if c == nil {
		return ""
	}
	c.serverIdentityMu.RLock()
	identity := c.serverCAIdentity
	c.serverIdentityMu.RUnlock()
	return identity
}

// ServerName returns empty because after a leadership change the
// secondary may reach any primary node, each with a unique CN.
func (c *drReplicationClusterClient) ServerName() string {
	return ""
}

// CACert returns the primary's CA certificate from the activation
// token. Retained for the initial CA pool (first connection to the
// original leader).
func (c *drReplicationClusterClient) CACert(ctx context.Context) *x509.Certificate {
	c.caMu.RLock()
	defer c.caMu.RUnlock()
	if c.primaryCACert != nil {
		return c.primaryCACert
	}
	if len(c.primaryCACerts) == 0 {
		return nil
	}
	return c.primaryCACerts[0]
}

// VerifyPeerCertificate returns a callback that checks the primary's
// server certificate against a dynamically maintained CA-scoped trust pool.
//
// Known certs (from heartbeats) are verified immediately. Unknown
// certs are verified against the DR transport CA from the activation
// token. If the certificate chains to the CA, it is accepted and
// added to the pool. If it does not chain, the connection is rejected.
func (c *drReplicationClusterClient) VerifyPeerCertificate() func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("dr: server presented no certificate")
		}

		fp := drCertFingerprint(rawCerts[0])

		c.trustedCertsMu.RLock()
		entry, known := c.trustedCerts[fp]
		entryCAIdentity := ""
		if known {
			entryCAIdentity = entry.caIdentity
		}
		c.trustedCertsMu.RUnlock()

		if known {
			caIdentity := entryCAIdentity
			if caIdentity == "" || !c.primaryTrustAnchorAllowsIdentity(caIdentity) {
				matchedCA, err := c.verifyPrimaryCertCA(rawCerts[0])
				if err != nil {
					c.trustedCertsMu.Lock()
					delete(c.trustedCerts, fp)
					c.trustedCertsMu.Unlock()
					return fmt.Errorf("dr: cached server certificate no longer chains to a trusted DR transport CA: %w", err)
				}
				caIdentity = drTransportCAKeyID(matchedCA)
			}

			// Refresh lastSeen and the matched CA identity for the known cert.
			c.trustedCertsMu.Lock()
			entry.lastSeen = time.Now()
			entry.caIdentity = caIdentity
			c.trustedCertsMu.Unlock()
			c.setLastPrimaryCAIdentity(caIdentity)
			return nil
		}

		// Unknown cert: verify it chains to one of the configured DR
		// transport CAs. During CA rotation both the current and staged
		// anchors can be trusted, but an unknown CA still fails closed.
		matchedCA, err := c.verifyPrimaryCertCA(rawCerts[0])
		if err != nil {
			cn := "(unknown)"
			if cert, parseErr := x509.ParseCertificate(rawCerts[0]); parseErr == nil {
				cn = cert.Subject.CommonName
			}
			c.logger.Warn("rejected primary certificate: does not chain to DR transport CA",
				"fingerprint", fp,
				"cn", cn,
				"error", err)
			return fmt.Errorf("dr: server certificate does not chain to DR transport CA: %w", err)
		}

		// Certificate chains to the CA -- accept and add to pool.
		caIdentity := drTransportCAKeyID(matchedCA)
		c.trustedCertsMu.Lock()
		c.trustedCerts[fp] = &trustedPrimaryCert{
			derBytes:   append([]byte(nil), rawCerts[0]...),
			lastSeen:   time.Now(),
			caIdentity: caIdentity,
		}
		c.trustedCertsMu.Unlock()
		c.setLastPrimaryCAIdentity(caIdentity)

		cn := "(unknown)"
		if cert, err := x509.ParseCertificate(rawCerts[0]); err == nil {
			cn = cert.Subject.CommonName
		}
		c.logger.Info("accepted primary certificate verified against DR transport CA",
			"fingerprint", fp,
			"ca_key_id", caIdentity,
			"cn", cn)
		return nil
	}
}

// addTrustedCert adds or refreshes a DER-encoded certificate in the trust
// pool. Called from the heartbeat loop. The certificate must chain to a
// configured DR transport CA or it is rejected.
func (c *drReplicationClusterClient) addTrustedCert(der []byte) error {
	fp := drCertFingerprint(der)

	c.trustedCertsMu.RLock()
	entry, known := c.trustedCerts[fp]
	entryCAIdentity := ""
	if known {
		entryCAIdentity = entry.caIdentity
	}
	c.trustedCertsMu.RUnlock()

	if known {
		caIdentity := entryCAIdentity
		if caIdentity == "" || !c.primaryTrustAnchorAllowsIdentity(caIdentity) {
			matchedCA, err := c.verifyPrimaryCertCA(der)
			if err != nil {
				c.trustedCertsMu.Lock()
				delete(c.trustedCerts, fp)
				c.trustedCertsMu.Unlock()
				return fmt.Errorf("cached heartbeat certificate no longer chains to a trusted DR transport CA: %w", err)
			}
			caIdentity = drTransportCAKeyID(matchedCA)
		}

		c.trustedCertsMu.Lock()
		entry.lastSeen = time.Now()
		entry.caIdentity = caIdentity
		c.trustedCertsMu.Unlock()
		c.setLastPrimaryCAIdentity(caIdentity)
		return nil
	}

	// Verify chain to a configured DR transport CA before adding.
	matchedCA, err := c.verifyPrimaryCertCA(der)
	if err != nil {
		c.logger.Warn("rejected heartbeat certificate: does not chain to DR transport CA",
			"fingerprint", fp,
			"error", err)
		return fmt.Errorf("heartbeat certificate does not chain to DR transport CA: %w", err)
	}

	caIdentity := drTransportCAKeyID(matchedCA)
	c.trustedCertsMu.Lock()
	c.trustedCerts[fp] = &trustedPrimaryCert{
		derBytes:   append([]byte(nil), der...),
		lastSeen:   time.Now(),
		caIdentity: caIdentity,
	}
	c.trustedCertsMu.Unlock()
	c.setLastPrimaryCAIdentity(caIdentity)
	return nil
}

func (c *drReplicationClusterClient) verifyPrimaryCertCA(der []byte) (*x509.Certificate, error) {
	cas := c.primaryTrustAnchors()
	if len(cas) == 0 {
		return nil, errors.New("no DR transport CA configured")
	}
	return verifyCertChainToAnyCA(der, cas)
}

func (c *drReplicationClusterClient) primaryTrustAnchorAllowsIdentity(identity string) bool {
	if c == nil || identity == "" {
		return false
	}
	for _, ca := range c.primaryTrustAnchors() {
		if drTransportCAKeyID(ca) == identity {
			return true
		}
	}
	return false
}

func (c *drReplicationClusterClient) primaryTrustAnchors() []*x509.Certificate {
	if c == nil {
		return nil
	}
	c.caMu.RLock()
	defer c.caMu.RUnlock()
	out := make([]*x509.Certificate, 0, len(c.primaryCACerts)+1)
	seen := make(map[string]struct{}, len(c.primaryCACerts)+1)
	add := func(cert *x509.Certificate) {
		if cert == nil {
			return
		}
		fp := certFingerprintSHA256(cert)
		if _, ok := seen[fp]; ok {
			return
		}
		seen[fp] = struct{}{}
		out = append(out, cert)
	}
	add(c.primaryCACert)
	for _, ca := range c.primaryCACerts {
		add(ca)
	}
	return out
}

func (c *drReplicationClusterClient) updatePrimaryTrustAnchors(active *x509.Certificate, trusted []*x509.Certificate) {
	if c == nil {
		return
	}
	c.caMu.Lock()
	c.primaryCACert = active
	c.primaryCACerts = append([]*x509.Certificate(nil), trusted...)
	c.caMu.Unlock()
}

func parseDRPrimaryCACerts(active []byte, trusted [][]byte) (*x509.Certificate, []*x509.Certificate, error) {
	trusted = normalizeDRPrimaryCACerts(active, trusted)
	if len(trusted) == 0 {
		return nil, nil, errors.New("no DR transport CA configured")
	}

	activeDER := active
	if len(activeDER) == 0 {
		activeDER = trusted[0]
	}

	var activeCert *x509.Certificate
	certs := make([]*x509.Certificate, 0, len(trusted))
	for i, der := range trusted {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse primary CA cert %d: %w", i, err)
		}
		if activeCert == nil && bytes.Equal(der, activeDER) {
			activeCert = cert
		}
		certs = append(certs, cert)
	}
	if activeCert == nil {
		activeCert = certs[0]
	}
	return activeCert, certs, nil
}

// pruneTrustedCerts removes entries whose lastSeen exceeds the TTL.
func (c *drReplicationClusterClient) pruneTrustedCerts() {
	now := time.Now()
	c.trustedCertsMu.Lock()
	defer c.trustedCertsMu.Unlock()
	for fp, entry := range c.trustedCerts {
		if now.Sub(entry.lastSeen) > trustedCertTTL {
			c.logger.Debug("pruning expired primary certificate from trust pool",
				"fingerprint", fp)
			delete(c.trustedCerts, fp)
		}
	}
}

// initTrustedPool seeds the trust pool with the activation token
// certificate if not already present.
func initTrustedPool(pool map[string]*trustedPrimaryCert, caCert *x509.Certificate) {
	if caCert == nil {
		return
	}
	fp := drCertFingerprint(caCert.Raw)
	if _, ok := pool[fp]; !ok {
		pool[fp] = &trustedPrimaryCert{
			derBytes:   append([]byte(nil), caCert.Raw...),
			lastSeen:   time.Now(),
			caIdentity: drTransportCAKeyID(caCert),
		}
	}
}

func initTrustedPoolFromCAs(pool map[string]*trustedPrimaryCert, caCerts []*x509.Certificate) {
	for _, caCert := range caCerts {
		initTrustedPool(pool, caCert)
	}
}

// formatTrustedPoolFingerprints returns a short string listing all
// fingerprints in the pool for diagnostic logging.
func formatTrustedPoolFingerprints(pool map[string]*trustedPrimaryCert) string {
	fps := make([]string, 0, len(pool))
	for fp := range pool {
		if len(fp) > 12 {
			fps = append(fps, fp[:12])
		} else {
			fps = append(fps, fp)
		}
	}
	return fmt.Sprintf("%v", fps)
}

// registerDRHandler registers the DR replication handler on the cluster listener.
func registerDRHandler(core *Core, handler *drReplicationClusterHandler) {
	cl := core.getClusterListener()
	if cl != nil {
		cl.AddHandler(consts.DRReplicationALPN, handler)
	}
}

// unregisterDRHandler removes the DR replication handler from the cluster listener.
func unregisterDRHandler(core *Core) {
	cl := core.getClusterListener()
	if cl != nil {
		cl.StopHandler(consts.DRReplicationALPN)
	}
}
