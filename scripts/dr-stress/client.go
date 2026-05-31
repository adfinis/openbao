package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// BaoClient is a thin, raw net/http wrapper for OpenBao API calls.
// It avoids the full SDK dependency while giving explicit control over
// transport tuning (keepalive, idle conns, timeouts, TLS).
type BaoClient struct {
	httpClient *http.Client
	addr       string // e.g. "https://127.0.0.1:8200"
	token      string
}

// NewBaoClient creates a BaoClient with a tuned http.Transport.
func NewBaoClient(node NodeConfig, cfg *Config) (*BaoClient, error) {
	tlsCfg := &tls.Config{}

	if node.CACert != "" {
		pem, err := os.ReadFile(node.CACert)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %s: %w", node.CACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid certs found in %s", node.CACert)
		}
		tlsCfg.RootCAs = pool
	}

	if node.TLSServerName != "" {
		tlsCfg.ServerName = node.TLSServerName
	}
	if node.SkipVerify {
		tlsCfg.InsecureSkipVerify = true
	}

	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.HTTPTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.HTTPTimeout + 5*time.Second, // slightly above response header timeout
	}

	addr := strings.TrimRight(node.Addr, "/")

	return &BaoClient{
		httpClient: client,
		addr:       addr,
		token:      node.Token,
	}, nil
}

// do executes an HTTP request and returns status code, response body, and error.
// The caller is responsible for interpreting the body.  On non-2xx the body
// is still returned (if available) so error details can be extracted.
func (c *BaoClient) do(ctx context.Context, method, path string, body io.Reader) (int, []byte, error) {
	url := c.addr + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", readErr)
	}

	return resp.StatusCode, respBody, nil
}

// KVPutRequest is the payload for a KV v2 PUT.
type KVPutRequest struct {
	Data map[string]interface{} `json:"data"`
}

// KVPut writes a KV v2 secret.  Returns HTTP status code and error.
func (c *BaoClient) KVPut(ctx context.Context, mount, key string, data map[string]interface{}) (int, error) {
	payload := KVPutRequest{Data: data}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal kv put: %w", err)
	}

	path := fmt.Sprintf("/v1/%s/data/%s", mount, key)
	code, _, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(body))
	return code, err
}

// KVGetResponse is the outer envelope for a KV v2 GET.
type KVGetResponse struct {
	Data   *KVGetData `json:"data"`
	Errors []string   `json:"errors"`
}

// KVGetData contains the versioned data from a KV v2 GET.
type KVGetData struct {
	Data     map[string]interface{} `json:"data"`
	Metadata map[string]interface{} `json:"metadata"`
}

// KVGet reads a KV v2 secret.  Returns HTTP status, parsed data, and error.
func (c *BaoClient) KVGet(ctx context.Context, mount, key string) (int, *KVGetResponse, error) {
	path := fmt.Sprintf("/v1/%s/data/%s", mount, key)
	code, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return code, nil, err
	}
	if len(body) == 0 {
		return code, nil, nil
	}
	var resp KVGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return code, nil, fmt.Errorf("decode kv get: %w", err)
	}
	return code, &resp, nil
}

// DRStatusResponse mirrors the relevant fields from sys/replication/dr/status.
type DRStatusResponse struct {
	Mode                    string  `json:"mode"`
	SecondaryState          string  `json:"secondary_state"`
	PrimaryIndex            int64   `json:"primary_index"`
	LastAppliedIndex        int64   `json:"last_applied_index"`
	LagEntries              int64   `json:"lag_entries"`
	LagSlopeEPS             float64 `json:"lag_slope_eps"`
	PredictedCatchupSeconds float64 `json:"predicted_catchup_seconds"`
	StreamBufferEntries     int64   `json:"stream_buffer_entries"`
	StreamBufferMaxEntries  int64   `json:"stream_buffer_max_entries"`
	StreamBufferHorizonSecs float64 `json:"stream_buffer_horizon_seconds"`
	ReconcileCount          int64   `json:"reconcile_count"`
	ConnectRetries          int64   `json:"connect_retries"`
	ConnectFailures         int64   `json:"connect_failures"`
	DRBackpressureState     string  `json:"dr_backpressure_state"`
	PrimaryWriteRateEPS     float64 `json:"primary_write_rate_eps"`
	SecondaryApplyRateEPS   float64 `json:"secondary_apply_rate_eps"`
	StreamTxnCoalescedTotal int64   `json:"stream_txn_coalesced_entries_total"`
	FallbackActive          bool    `json:"fallback_active"`
	FallbackCount           int64   `json:"fallback_count"`
	FallbackLastReason      string  `json:"fallback_last_reason"`
}

// drStatusEnvelope wraps the API response { "data": { ... } }.
type drStatusEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// DRStatus queries sys/replication/dr/status. Returns parsed status and error.
func (c *BaoClient) DRStatus(ctx context.Context) (*DRStatusResponse, int, error) {
	code, body, err := c.do(ctx, http.MethodGet, "/v1/sys/replication/dr/status", nil)
	if err != nil {
		return nil, code, err
	}
	if code != 200 {
		return nil, code, fmt.Errorf("dr status returned %d", code)
	}

	var envelope drStatusEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, code, fmt.Errorf("decode dr status envelope: %w", err)
	}

	var status DRStatusResponse
	if err := json.Unmarshal(envelope.Data, &status); err != nil {
		return nil, code, fmt.Errorf("decode dr status data: %w", err)
	}
	return &status, code, nil
}

// StepDown requests a leader stepdown on the node.
func (c *BaoClient) StepDown(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodPost, "/v1/sys/step-down", nil)
	return err
}

// secretsListEnvelope wraps the LIST response.
type secretsListEnvelope struct {
	Data struct {
		Keys []string `json:"keys"`
	} `json:"data"`
}

// SecretsList lists secrets engines at the given path.
func (c *BaoClient) SecretsList(ctx context.Context, path string) ([]string, error) {
	code, body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/sys/mounts"), nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("list mounts returned %d", code)
	}

	// The mounts response is { "<path>/": { ... }, ... }.
	var mounts map[string]json.RawMessage
	if err := json.Unmarshal(body, &mounts); err != nil {
		return nil, fmt.Errorf("decode mounts: %w", err)
	}

	// Extract just the mount paths (keys).
	var keys []string
	for k := range mounts {
		keys = append(keys, k)
	}
	return keys, nil
}

// SecretsEnable enables a secrets engine at the given path.
func (c *BaoClient) SecretsEnable(ctx context.Context, path, engineType string) error {
	payload := map[string]string{"type": engineType}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal enable: %w", err)
	}
	code, _, doErr := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/sys/mounts/%s", path), bytes.NewReader(body))
	if doErr != nil {
		return doErr
	}
	if code != 200 && code != 204 {
		return fmt.Errorf("enable %s returned %d", path, code)
	}
	return nil
}

// SysHealth queries sys/health.  Returns the HTTP status code.
func (c *BaoClient) SysHealth(ctx context.Context) (int, error) {
	code, _, err := c.do(ctx, http.MethodGet, "/v1/sys/health", nil)
	return code, err
}
