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
	"sync"
	"time"
)

// BaoClient is a thin, raw net/http wrapper for OpenBao API calls.
// It avoids the full SDK dependency while giving explicit control over
// transport tuning (keepalive, idle conns, timeouts, TLS).
type BaoClient struct {
	httpClient *http.Client
	addr       string // e.g. "https://127.0.0.1:8200"
	addrs      []string
	token      string
	activeMu   sync.RWMutex
	activeAddr string
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

	addrs := parseAddrs(node.Addr)
	addr := ""
	if len(addrs) > 0 {
		addr = addrs[0]
	}

	return &BaoClient{
		httpClient: client,
		addr:       addr,
		addrs:      addrs,
		token:      node.Token,
	}, nil
}

func parseAddrs(raw string) []string {
	var addrs []string
	for _, part := range strings.Split(raw, ",") {
		addr := strings.TrimRight(strings.TrimSpace(part), "/")
		if addr != "" {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// do executes an HTTP request and returns status code, response body, and error.
// The caller is responsible for interpreting the body.  On non-2xx the body
// is still returned (if available) so error details can be extracted.
func (c *BaoClient) do(ctx context.Context, method, path string, body io.Reader) (int, []byte, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return 0, nil, fmt.Errorf("read request body: %w", err)
		}
	}
	bodyReader := func() io.Reader {
		if body == nil {
			return nil
		}
		return bytes.NewReader(bodyBytes)
	}

	if len(c.addrs) <= 1 {
		return c.doAt(ctx, c.addr, method, path, bodyReader())
	}

	addr := c.cachedActiveAddr()
	if addr == "" {
		if resolved, err := c.resolveActiveAddr(ctx); err == nil && resolved != "" {
			addr = resolved
		} else {
			addr = c.addr
		}
	}

	code, resp, err := c.doAt(ctx, addr, method, path, bodyReader())
	if err == nil {
		return code, resp, nil
	}

	c.clearActiveAddr()
	if resolved, resolveErr := c.resolveActiveAddr(ctx); resolveErr == nil && resolved != "" && resolved != addr {
		return c.doAt(ctx, resolved, method, path, bodyReader())
	}
	return code, resp, err
}

func (c *BaoClient) doAt(ctx context.Context, addr, method, path string, body io.Reader) (int, []byte, error) {
	url := addr + path
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

func (c *BaoClient) cachedActiveAddr() string {
	c.activeMu.RLock()
	defer c.activeMu.RUnlock()
	return c.activeAddr
}

func (c *BaoClient) setActiveAddr(addr string) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	c.activeAddr = addr
}

func (c *BaoClient) clearActiveAddr() {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	c.activeAddr = ""
}

type leaderEnvelope struct {
	Data struct {
		IsSelf bool `json:"is_self"`
	} `json:"data"`
}

func (c *BaoClient) resolveActiveAddr(ctx context.Context) (string, error) {
	var lastErr error
	for _, addr := range c.addrs {
		code, body, err := c.doAt(ctx, addr, http.MethodGet, "/v1/sys/leader", nil)
		if err != nil {
			lastErr = err
			continue
		}
		if code != http.StatusOK {
			lastErr = fmt.Errorf("leader lookup on %s returned %d", addr, code)
			continue
		}
		var envelope leaderEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			lastErr = fmt.Errorf("decode leader response from %s: %w", addr, err)
			continue
		}
		if envelope.Data.IsSelf {
			c.setActiveAddr(addr)
			return addr, nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no active node found")
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
	Mode                            string  `json:"mode"`
	SecondaryState                  string  `json:"secondary_state"`
	PrimaryIndex                    int64   `json:"primary_index"`
	LastAppliedIndex                int64   `json:"last_applied_index"`
	LagEntries                      int64   `json:"lag_entries"`
	LagSlopeEPS                     float64 `json:"lag_slope_eps"`
	PredictedCatchupSeconds         float64 `json:"predicted_catchup_seconds"`
	StreamBufferEntries             int64   `json:"stream_buffer_entries"`
	StreamBufferMaxEntries          int64   `json:"stream_buffer_max_entries"`
	StreamBufferHorizonSecs         float64 `json:"stream_buffer_horizon_seconds"`
	ReconcileCount                  int64   `json:"reconcile_count"`
	ReconcileRetriesTotal           int64   `json:"reconcile_retries_total"`
	ReconcilePhase                  string  `json:"reconcile_phase"`
	ReconcileFailReasonLast         string  `json:"reconcile_fail_reason_last"`
	ReconcileActiveCheckpoint       string  `json:"reconcile_active_checkpoint_id"`
	ReconcileActiveIndex            int64   `json:"reconcile_active_checkpoint_index"`
	ReconcileStuckSeconds           int64   `json:"reconcile_stuck_seconds"`
	ReconcileQueueDepth             int64   `json:"reconcile_queue_depth"`
	ReconcileRangesInflight         int64   `json:"reconcile_ranges_inflight"`
	ReconcileRangesFailed           int64   `json:"reconcile_ranges_failed"`
	ReconcilePutWorkersActive       int64   `json:"reconcile_put_workers_active"`
	ReconcileTaskRetriesTotal       int64   `json:"reconcile_task_retries_total"`
	ConnectRetries                  int64   `json:"connect_retries"`
	ConnectFailures                 int64   `json:"connect_failures"`
	DRBackpressureState             string  `json:"dr_backpressure_state"`
	PrimaryWriteRateEPS             float64 `json:"primary_write_rate_eps"`
	SecondaryApplyRateEPS           float64 `json:"secondary_apply_rate_eps"`
	StreamTxnCoalescedTotal         int64   `json:"stream_txn_coalesced_entries_total"`
	StreamTxnBatchesTotal           int64   `json:"stream_txn_batches_total"`
	StreamTxnEntriesTotal           int64   `json:"stream_txn_entries_total"`
	StreamTxnPhysicalTotal          int64   `json:"stream_txn_physical_entries_total"`
	StreamTxnAvgEntries             float64 `json:"stream_txn_average_entries"`
	StreamTxnMaxEntries             int64   `json:"stream_txn_max_entries"`
	StreamTxnAvgPhysical            float64 `json:"stream_txn_average_physical_entries"`
	StreamTxnMaxPhysical            int64   `json:"stream_txn_max_physical_entries"`
	StreamTxnApplyTotalMS           float64 `json:"stream_txn_apply_milliseconds_total"`
	StreamTxnApplyAvgMS             float64 `json:"stream_txn_apply_milliseconds_average"`
	StreamTxnApplyMaxMS             float64 `json:"stream_txn_apply_milliseconds_max"`
	StreamTxnCommitTotalMS          float64 `json:"stream_txn_commit_milliseconds_total"`
	StreamTxnCommitAvgMS            float64 `json:"stream_txn_commit_milliseconds_average"`
	StreamTxnCommitMaxMS            float64 `json:"stream_txn_commit_milliseconds_max"`
	StreamFlushMaxEntries           int64   `json:"stream_batch_flush_max_entries_total"`
	StreamFlushMaxBytes             int64   `json:"stream_batch_flush_max_bytes_total"`
	StreamFlushMaxWait              int64   `json:"stream_batch_flush_max_wait_total"`
	StreamFlushShutdown             int64   `json:"stream_batch_flush_shutdown_total"`
	StreamBatchAdaptiveAdjustments  int64   `json:"stream_batch_adaptive_adjustments_total"`
	StreamBatchAdaptiveLevel        int64   `json:"stream_batch_adaptive_level"`
	StreamBatchEffectiveMaxEntries  int64   `json:"stream_batch_effective_max_entries"`
	StreamBatchEffectiveMaxWaitMS   int64   `json:"stream_batch_effective_max_wait_milliseconds"`
	FlatAccCursorWrites             int64   `json:"flat_accumulator_cursor_writes_total"`
	FlatAccCursorIndex              int64   `json:"flat_accumulator_cursor_index"`
	FlatAccSnapshotsTotal           int64   `json:"flat_accumulator_snapshot_persists_total"`
	FlatAccSnapshotIndex            int64   `json:"flat_accumulator_snapshot_index"`
	FlatAccSnapshotBytes            int64   `json:"flat_accumulator_snapshot_bytes_total"`
	FlatAccSnapshotAvgBytes         float64 `json:"flat_accumulator_snapshot_bytes_average"`
	FlatAccSnapshotLastBytes        int64   `json:"flat_accumulator_snapshot_bytes_last"`
	FlatAccPersistTotalMS           float64 `json:"flat_accumulator_snapshot_persist_milliseconds_total"`
	FlatAccPersistAvgMS             float64 `json:"flat_accumulator_snapshot_persist_milliseconds_average"`
	FlatAccPersistMaxMS             float64 `json:"flat_accumulator_snapshot_persist_milliseconds_max"`
	FlatAccSnapshotSkipped          int64   `json:"flat_accumulator_snapshot_skipped_total"`
	FlatAccFastPathTotal            int64   `json:"flat_accumulator_fast_path_total"`
	FlatAccEmptyRepairTotal         int64   `json:"flat_accumulator_empty_repair_total"`
	FlatAccEmptyRepairRanges        int64   `json:"flat_accumulator_empty_repair_ranges_total"`
	FlatAccIndexedRepairTotal       int64   `json:"flat_accumulator_indexed_repair_total"`
	FlatAccIndexedRepairRanges      int64   `json:"flat_accumulator_indexed_repair_ranges_total"`
	FlatAccIndexedProofMismatches   int64   `json:"flat_accumulator_indexed_repair_proof_mismatches_total"`
	FlatAccIndexedProofRangeLast    int64   `json:"flat_accumulator_indexed_repair_proof_mismatch_range_last"`
	FlatAccIndexedProofLocalLast    int64   `json:"flat_accumulator_indexed_repair_proof_mismatch_local_count_last"`
	FlatAccIndexedProofRemoteLast   int64   `json:"flat_accumulator_indexed_repair_proof_mismatch_remote_count_last"`
	FlatAccIndexedProofChecksumLast bool    `json:"flat_accumulator_indexed_repair_proof_mismatch_checksum_last"`
	FlatAccIndexedFullBucketTotal   int64   `json:"flat_accumulator_indexed_repair_full_bucket_fallback_total"`
	FlatAccIndexedFullBucketRanges  int64   `json:"flat_accumulator_indexed_repair_full_bucket_ranges_total"`
	LocalKIDIndexBucketLoads        int64   `json:"local_kid_index_bucket_loads_total"`
	LocalKIDIndexEntriesLoaded      int64   `json:"local_kid_index_entries_loaded_total"`
	LocalKIDIndexLoadFailures       int64   `json:"local_kid_index_load_failures_total"`
	LocalKIDIndexResets             int64   `json:"local_kid_index_resets_total"`
	LocalKIDIndexUpdates            int64   `json:"local_kid_index_updates_total"`
	LocalKIDIndexFallbackScans      int64   `json:"local_kid_index_fallback_scans_total"`
	LocalKIDIndexFallbackReason     string  `json:"local_kid_index_fallback_scan_reason_last"`
	LocalKIDIndexInvalidations      int64   `json:"local_kid_index_invalidations_total"`
	LocalKIDIndexInvalidationReason string  `json:"local_kid_index_invalidation_reason_last"`
	FallbackActive                  bool    `json:"fallback_active"`
	FallbackCount                   int64   `json:"fallback_count"`
	FallbackLastReason              string  `json:"fallback_last_reason"`
	CheckpointStorageDrift          int64   `json:"checkpoint_conflicts_storage_drift_total"`
}

// drStatusEnvelope wraps the API response { "data": { ... } }.
type drStatusEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// DRStatus queries sys/replication/dr/status. Returns parsed status and error.
func (c *BaoClient) DRStatus(ctx context.Context) (*DRStatusResponse, int, error) {
	if len(c.addrs) <= 1 {
		return c.drStatusAt(ctx, c.addr)
	}

	var best *DRStatusResponse
	var bestCode int
	var bestScore int
	var lastErr error
	var lastCode int
	for _, addr := range c.addrs {
		status, code, err := c.drStatusAt(ctx, addr)
		if err != nil {
			lastErr = err
			lastCode = code
			continue
		}
		score := drStatusScore(status)
		if best == nil || score > bestScore {
			best = status
			bestCode = code
			bestScore = score
		}
	}
	if best != nil {
		return best, bestCode, nil
	}
	if lastErr != nil {
		return nil, lastCode, lastErr
	}
	return nil, 0, fmt.Errorf("no addresses configured")
}

func (c *BaoClient) drStatusAt(ctx context.Context, addr string) (*DRStatusResponse, int, error) {
	code, body, err := c.doAt(ctx, addr, http.MethodGet, "/v1/sys/replication/dr/status", nil)
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

func drStatusScore(status *DRStatusResponse) int {
	if status == nil {
		return 0
	}
	score := 0
	if status.Mode != "" {
		score++
	}
	if status.Mode == "primary" {
		score += 10
	}
	if status.Mode == "secondary" {
		score += 10
	}
	if status.SecondaryState == "streaming" {
		score += 10
	}
	if status.PrimaryIndex > 0 {
		score += 5
	}
	if status.PrimaryIndex > 0 && status.LastAppliedIndex >= status.PrimaryIndex && status.LagEntries == 0 {
		score += 5
	}
	return score
}

// StepDown requests a leader stepdown on the node.
func (c *BaoClient) StepDown(ctx context.Context) (int, error) {
	code, _, err := c.do(ctx, http.MethodPost, "/v1/sys/step-down", nil)
	c.clearActiveAddr()
	if err != nil {
		return code, err
	}
	if code < 200 || code >= 300 {
		return code, fmt.Errorf("step-down returned %d", code)
	}
	return code, nil
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
