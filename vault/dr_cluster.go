// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/vault/cluster"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
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

	// certMu protects trustedSecondaryCerts.
	certMu sync.RWMutex
	// trustedSecondaryCerts holds per-relationship trusted certificates.
	trustedSecondaryCerts map[string][]*x509.Certificate
}

// newDRReplicationClusterHandler creates and registers the DR gRPC handler
// on the cluster listener.
func newDRReplicationClusterHandler(core *Core, drServer *drReplicationPrimary, logger log.Logger) *drReplicationClusterHandler {
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time: 2 * core.clusterHeartbeatInterval,
		}),
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

// ServerLookup returns the cluster TLS certificate for incoming connections.
func (h *drReplicationClusterHandler) ServerLookup(ctx context.Context, clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
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

// drReplicationClusterClient implements the cluster.Client interface for
// connecting to the primary's DR replication gRPC service over mTLS.
type drReplicationClusterClient struct {
	core *Core
	// primaryCACert is the primary's CA certificate from the activation token.
	primaryCACert *x509.Certificate
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
			return &tls.Certificate{
				Certificate: [][]byte{localCert},
				PrivateKey:  c.core.localClusterPrivateKey.Load(),
				Leaf:        c.core.localClusterParsedCert.Load(),
			}, nil
		}
		// Also match against the local cert's issuer (same-cluster case).
		if bytes.Equal(subj, parsedCert.RawIssuer) {
			return &tls.Certificate{
				Certificate: [][]byte{localCert},
				PrivateKey:  c.core.localClusterPrivateKey.Load(),
				Leaf:        c.core.localClusterParsedCert.Load(),
			}, nil
		}
	}

	return nil, nil
}

// ServerName returns the expected server name for TLS verification.
func (c *drReplicationClusterClient) ServerName() string {
	if c.primaryCACert != nil {
		return c.primaryCACert.Subject.CommonName
	}
	return ""
}

// CACert returns the primary's CA certificate for TLS verification.
func (c *drReplicationClusterClient) CACert(ctx context.Context) *x509.Certificate {
	return c.primaryCACert
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
