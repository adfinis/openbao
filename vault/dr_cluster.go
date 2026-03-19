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
	privKey := h.core.localClusterPrivateKey.Load()
	if privKey == nil {
		return errors.New("no local cluster private key available")
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

// ActiveDRLeafCertDER returns a copy of the currently minted DR transport
// leaf certificate DER bytes. Returns nil when no DR leaf is available.
func (h *drReplicationClusterHandler) ActiveDRLeafCertDER() []byte {
	if h == nil {
		return nil
	}

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
// connections. If a CA-signed leaf has been minted, it is presented
// together with the transport CA cert as the chain. Falls back to the
// node's self-signed cluster cert only if no DR leaf has been minted yet.
func (h *drReplicationClusterHandler) ServerLookup(ctx context.Context, clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.drLeafCertMu.RLock()
	leafDER := h.drLeafCertDER
	leafParsed := h.drLeafParsedCert
	leafKey := h.drLeafPrivateKey
	h.drLeafCertMu.RUnlock()

	if leafDER != nil && leafParsed != nil && leafKey != nil {
		return &tls.Certificate{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey, // use the snapshot taken at mint time
			Leaf:        leafParsed,
		}, nil
	}

	// Fallback: no DR leaf cert minted yet, use self-signed cluster cert.
	h.logger.Warn("no DR transport leaf cert available, falling back to cluster cert")
	currCert := h.core.localClusterCert.Load()
	if currCert == nil {
		return nil, errors.New("dr replication connection but no local cert")
	}
	certBytes := *currCert
	if len(certBytes) == 0 {
		return nil, errors.New("dr replication connection but empty local cert")
	}

	localCert := make([]byte, len(certBytes))
	copy(localCert, certBytes)

	return &tls.Certificate{
		Certificate: [][]byte{localCert},
		PrivateKey:  h.core.localClusterPrivateKey.Load(),
		Leaf:        h.core.localClusterParsedCert.Load(),
	}, nil
}

// startLeafRenewal launches a background goroutine that re-mints the DR
// leaf certificate at half the leaf validity interval. The goroutine
// stops when the handler's stopCh is closed.
func (h *drReplicationClusterHandler) startLeafRenewal(ca *drTransportCA) {
	renewInterval := drTransportLeafValidity / 2
	go func() {
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := h.SetTransportCA(ca); err != nil {
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
	derBytes []byte
	lastSeen time.Time
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
	// primaryCACert is the primary's DR transport CA certificate from
	// the activation token. This is the sole trust anchor used to
	// verify primary identity.
	primaryCACert *x509.Certificate
	logger        log.Logger

	// trustedCertsMu and trustedCerts are owned by the
	// drReplicationSecondary and shared via pointer so the pool
	// persists across reconnects.
	trustedCertsMu *sync.RWMutex
	trustedCerts   map[string]*trustedPrimaryCert

	clientCertMu sync.RWMutex
	clientCertFP string
}

// ClientLookup returns the client TLS certificate for outgoing connections.
func (c *drReplicationClusterClient) ClientLookup(ctx context.Context, requestInfo *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	parsedCert := c.core.localClusterParsedCert.Load()
	if parsedCert == nil {
		return nil, nil
	}
	currCert := c.core.localClusterCert.Load()
	if currCert == nil {
		return nil, nil
	}
	certBytes := *currCert
	if len(certBytes) == 0 {
		return nil, nil
	}

	localCert := make([]byte, len(certBytes))
	copy(localCert, certBytes)

	for _, subj := range requestInfo.AcceptableCAs {
		// Match against the primary's CA certificate RawSubject for
		// cross-cluster mTLS. The primary's cert is the CA from the
		// activation token, so we compare the server's acceptable CA
		// with the primary's raw subject.
		if c.primaryCACert != nil && bytes.Equal(subj, c.primaryCACert.RawSubject) {
			c.setLastClientCertFingerprint(certFingerprintSHA256(parsedCert))
			return &tls.Certificate{
				Certificate: [][]byte{localCert},
				PrivateKey:  c.core.localClusterPrivateKey.Load(),
				Leaf:        c.core.localClusterParsedCert.Load(),
			}, nil
		}
		// Also match against the local cert's issuer (same-cluster case).
		if bytes.Equal(subj, parsedCert.RawIssuer) {
			c.setLastClientCertFingerprint(certFingerprintSHA256(parsedCert))
			return &tls.Certificate{
				Certificate: [][]byte{localCert},
				PrivateKey:  c.core.localClusterPrivateKey.Load(),
				Leaf:        c.core.localClusterParsedCert.Load(),
			}, nil
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

// ServerName returns empty because after a leadership change the
// secondary may reach any primary node, each with a unique CN.
func (c *drReplicationClusterClient) ServerName() string {
	return ""
}

// CACert returns the primary's CA certificate from the activation
// token. Retained for the initial CA pool (first connection to the
// original leader).
func (c *drReplicationClusterClient) CACert(ctx context.Context) *x509.Certificate {
	return c.primaryCACert
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
		c.trustedCertsMu.RUnlock()

		if known {
			// Refresh lastSeen for the known cert.
			c.trustedCertsMu.Lock()
			entry.lastSeen = time.Now()
			c.trustedCertsMu.Unlock()
			return nil
		}

		// Unknown cert: verify it chains to the DR transport CA.
		if c.primaryCACert == nil {
			return errors.New("dr: no DR transport CA configured; cannot verify server certificate")
		}

		if err := verifyCertChainToCA(rawCerts[0], c.primaryCACert); err != nil {
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
		c.trustedCertsMu.Lock()
		c.trustedCerts[fp] = &trustedPrimaryCert{
			derBytes: append([]byte(nil), rawCerts[0]...),
			lastSeen: time.Now(),
		}
		c.trustedCertsMu.Unlock()

		cn := "(unknown)"
		if cert, err := x509.ParseCertificate(rawCerts[0]); err == nil {
			cn = cert.Subject.CommonName
		}
		c.logger.Info("accepted primary certificate verified against DR transport CA",
			"fingerprint", fp,
			"cn", cn)
		return nil
	}
}

// addTrustedCert adds or refreshes a DER-encoded certificate in the
// trust pool. Called from the heartbeat loop. The certificate must
// chain to the DR transport CA or it is rejected.
func (c *drReplicationClusterClient) addTrustedCert(der []byte) error {
	fp := drCertFingerprint(der)

	c.trustedCertsMu.RLock()
	entry, known := c.trustedCerts[fp]
	c.trustedCertsMu.RUnlock()

	if known {
		c.trustedCertsMu.Lock()
		entry.lastSeen = time.Now()
		c.trustedCertsMu.Unlock()
		return nil
	}

	// Verify chain to DR transport CA before adding.
	if c.primaryCACert != nil {
		if err := verifyCertChainToCA(der, c.primaryCACert); err != nil {
			c.logger.Warn("rejected heartbeat certificate: does not chain to DR transport CA",
				"fingerprint", fp,
				"error", err)
			return fmt.Errorf("heartbeat certificate does not chain to DR transport CA: %w", err)
		}
	}

	c.trustedCertsMu.Lock()
	c.trustedCerts[fp] = &trustedPrimaryCert{
		derBytes: append([]byte(nil), der...),
		lastSeen: time.Now(),
	}
	c.trustedCertsMu.Unlock()
	return nil
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
			derBytes: append([]byte(nil), caCert.Raw...),
			lastSeen: time.Now(),
		}
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
