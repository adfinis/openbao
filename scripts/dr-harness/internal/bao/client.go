package bao

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
	"strings"
	"sync"
	"time"
)

type Node struct {
	Addrs       []string
	Token       string
	HTTPTimeout time.Duration
}

type Client struct {
	httpClient *http.Client
	addrs      []string
	token      string

	activeMu   sync.RWMutex
	activeAddr string
}

func NewClient(node Node) (*Client, error) {
	if node.HTTPTimeout <= 0 {
		node.HTTPTimeout = 60 * time.Second
	}
	if len(node.Addrs) == 0 {
		return nil, fmt.Errorf("no addresses configured")
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: x509.NewCertPool(),
		},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: node.HTTPTimeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   node.HTTPTimeout + 5*time.Second,
		},
		addrs: normalizeAddrs(node.Addrs),
		token: node.Token,
	}, nil
}

func normalizeAddrs(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimRight(strings.TrimSpace(addr), "/")
		if addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

func (c *Client) Addrs() []string {
	return append([]string(nil), c.addrs...)
}

func (c *Client) ActiveAddr(ctx context.Context) (string, error) {
	if cached := c.cachedActiveAddr(); cached != "" {
		return cached, nil
	}
	return c.ResolveActiveAddr(ctx)
}

func (c *Client) ResolveActiveAddr(ctx context.Context) (string, error) {
	var lastErr error
	for _, addr := range c.addrs {
		code, body, err := c.doAt(ctx, addr, http.MethodGet, "/v1/sys/leader", nil)
		if err != nil {
			lastErr = err
			continue
		}
		if code != http.StatusOK {
			lastErr = fmt.Errorf("%s sys/leader returned %d: %s", addr, code, summarizeBody(body))
			continue
		}
		var leader struct {
			IsSelf bool `json:"is_self"`
			Data   struct {
				IsSelf bool `json:"is_self"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &leader); err != nil {
			lastErr = fmt.Errorf("decode %s sys/leader: %w", addr, err)
			continue
		}
		if leader.IsSelf || leader.Data.IsSelf {
			c.setActiveAddr(addr)
			return addr, nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no active node among %s", strings.Join(c.addrs, ","))
}

func (c *Client) cachedActiveAddr() string {
	c.activeMu.RLock()
	defer c.activeMu.RUnlock()
	return c.activeAddr
}

func (c *Client) setActiveAddr(addr string) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	c.activeAddr = addr
}

func (c *Client) ClearActiveAddr() {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	c.activeAddr = ""
}

func (c *Client) Read(ctx context.Context, path string, data any) ([]byte, error) {
	return c.request(ctx, http.MethodGet, path, nil, data)
}

func (c *Client) Write(ctx context.Context, path string, payload any, data any) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	return c.request(ctx, http.MethodPost, path, payload, data)
}

func (c *Client) request(ctx context.Context, method, path string, payload any, data any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal request for %s: %w", path, err)
		}
		body = bytes.NewReader(raw)
	}

	code, resp, err := c.do(ctx, method, apiPath(path), body)
	if err != nil {
		return resp, err
	}
	if code < 200 || code >= 300 {
		return resp, fmt.Errorf("%s %s returned %d: %s", method, path, code, summarizeBody(resp))
	}
	if data == nil {
		return resp, nil
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []string        `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return resp, fmt.Errorf("decode response envelope for %s: %w", path, err)
	}
	if len(envelope.Errors) > 0 {
		return resp, fmt.Errorf("%s %s returned errors: %s", method, path, strings.Join(envelope.Errors, "; "))
	}
	if len(envelope.Data) == 0 {
		return resp, nil
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return resp, fmt.Errorf("decode response data for %s: %w", path, err)
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (int, []byte, error) {
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

	if len(c.addrs) == 1 {
		return c.doAt(ctx, c.addrs[0], method, path, bodyReader())
	}

	addr := c.cachedActiveAddr()
	if addr == "" {
		resolved, err := c.ResolveActiveAddr(ctx)
		if err == nil {
			addr = resolved
		} else {
			addr = c.addrs[0]
		}
	}

	code, resp, err := c.doAt(ctx, addr, method, path, bodyReader())
	if err == nil {
		return code, resp, nil
	}
	c.ClearActiveAddr()
	resolved, resolveErr := c.ResolveActiveAddr(ctx)
	if resolveErr == nil && resolved != "" && resolved != addr {
		return c.doAt(ctx, resolved, method, path, bodyReader())
	}
	return code, resp, err
}

func (c *Client) doAt(ctx context.Context, addr, method, path string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, addr+path, body)
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

func apiPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if strings.HasPrefix(path, "v1/") {
		return "/" + path
	}
	return "/v1/" + path
}

func summarizeBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Errors) > 0 {
		return strings.Join(envelope.Errors, "; ")
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

type KVPutRequest struct {
	Data map[string]any `json:"data"`
}

func (c *Client) KVPut(ctx context.Context, mount, key string, data map[string]any) error {
	_, err := c.Write(ctx, fmt.Sprintf("%s/data/%s", strings.Trim(mount, "/"), strings.TrimPrefix(key, "/")), KVPutRequest{Data: data}, nil)
	return err
}

func (c *Client) KVExists(ctx context.Context, mount, key string) (bool, error) {
	code, body, err := c.do(ctx, http.MethodGet, apiPath(fmt.Sprintf("%s/data/%s", strings.Trim(mount, "/"), strings.TrimPrefix(key, "/"))), nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("kv get returned %d: %s", code, summarizeBody(body))
	}
}

func (c *Client) EnsureKVV2Mount(ctx context.Context, mount string) error {
	mount = strings.Trim(mount, "/")
	payload := map[string]any{"type": "kv-v2"}
	code, body, err := c.do(ctx, http.MethodPost, apiPath("sys/mounts/"+mount), mustJSON(payload))
	if err != nil {
		return err
	}
	if code == http.StatusOK || code == http.StatusNoContent {
		return nil
	}
	if code == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "path is already in use") {
		return nil
	}
	return fmt.Errorf("enable kv mount returned %d: %s", code, summarizeBody(body))
}

func mustJSON(v any) io.Reader {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(raw)
}

type ActivationToken struct {
	RelationshipID string `json:"relationship_id"`
}

func (c *Client) NewSecondaryToken(ctx context.Context) (string, ActivationToken, error) {
	var data struct {
		Token string `json:"token"`
	}
	if _, err := c.Write(ctx, "sys/replication/dr/primary/secondary-token", nil, &data); err != nil {
		return "", ActivationToken{}, err
	}
	if data.Token == "" {
		return "", ActivationToken{}, fmt.Errorf("secondary-token response did not include token")
	}
	var token ActivationToken
	if err := json.Unmarshal([]byte(data.Token), &token); err != nil {
		return "", ActivationToken{}, fmt.Errorf("decode activation token: %w", err)
	}
	if token.RelationshipID == "" {
		return "", ActivationToken{}, fmt.Errorf("activation token did not include relationship_id")
	}
	return data.Token, token, nil
}

func (c *Client) DisableSecondary(ctx context.Context) error {
	_, err := c.Write(ctx, "sys/replication/dr/secondary/disable", nil, nil)
	return err
}

func (c *Client) EnableSecondary(ctx context.Context, token string) error {
	_, err := c.Write(ctx, "sys/replication/dr/secondary/enable", map[string]any{"token": token}, nil)
	return err
}

func (c *Client) StepDown(ctx context.Context) error {
	_, err := c.Write(ctx, "sys/step-down", nil, nil)
	c.ClearActiveAddr()
	return err
}

type DRStatus struct {
	Mode                                      string  `json:"mode"`
	SecondaryState                            string  `json:"secondary_state"`
	PrimaryIndex                              uint64  `json:"primary_index"`
	LastAppliedIndex                          uint64  `json:"last_applied_index"`
	LagEntries                                int64   `json:"lag_entries"`
	ReconcileCount                            int64   `json:"reconcile_count"`
	ReconcilePhase                            string  `json:"reconcile_phase"`
	ScanFailuresTotal                         int64   `json:"scan_failures_total"`
	FlatAccumulatorIndexedRepair              int64   `json:"flat_accumulator_indexed_repair_total"`
	LocalKIDIndexFallbackScans                int64   `json:"local_kid_index_fallback_scans_total"`
	StreamTxnBatchesTotal                     int64   `json:"stream_txn_batches_total"`
	StreamTxnEntriesTotal                     int64   `json:"stream_txn_entries_total"`
	StreamTxnCommitAverageMS                  float64 `json:"stream_txn_commit_milliseconds_average"`
	StreamTxnCommitMaxMS                      float64 `json:"stream_txn_commit_milliseconds_max"`
	FlatAccumulatorCursorIndex                uint64  `json:"flat_accumulator_cursor_index"`
	FlatAccumulatorSnapshotIndex              uint64  `json:"flat_accumulator_snapshot_index"`
	FlatAccumulatorCursorWrites               int64   `json:"flat_accumulator_cursor_writes_total"`
	FlatAccumulatorSnapshotWrites             int64   `json:"flat_accumulator_snapshot_persists_total"`
	JournalRangeTooOldTotal                   int64   `json:"journal_range_too_old_total"`
	FlatAccumulatorFastPathTotal              int64   `json:"flat_accumulator_fast_path_total"`
	FlatAccumulatorFullBucket                 int64   `json:"flat_accumulator_indexed_repair_full_bucket_fallback_total"`
	FlatAccumulatorProofMismatch              int64   `json:"flat_accumulator_indexed_repair_proof_mismatches_total"`
	LocalKIDIndexBucketLoads                  int64   `json:"local_kid_index_bucket_loads_total"`
	LocalKIDIndexEntriesLoaded                int64   `json:"local_kid_index_entries_loaded_total"`
	LocalKIDIndexLoadFailures                 int64   `json:"local_kid_index_load_failures_total"`
	RangeDrillDownRPCsTotal                   int64   `json:"range_drilldown_rpc_total"`
	RangeDrillDownCoarseFetchTotal            int64   `json:"range_drilldown_coarse_fetch_total"`
	RangeDrillDownCoarseFetchRangesTotal      int64   `json:"range_drilldown_coarse_fetch_ranges_total"`
	ReconcileBudgetRemainingBytes             uint64  `json:"reconcile_budget_remaining_bytes"`
	ReconcileBudgetExhaustedTotal             int64   `json:"reconcile_budget_exhausted_total"`
	ReconcileBudgetExhaustedPhaseLast         string  `json:"reconcile_budget_exhausted_phase_last"`
	ReconcileBudgetExhaustedReasonLast        string  `json:"reconcile_budget_exhausted_reason_last"`
	ReconcileBudgetExhaustedRPCBytesLast      uint64  `json:"reconcile_budget_exhausted_rpc_bytes_last"`
	ReconcileBudgetExhaustedMaxRPCBytesLast   uint64  `json:"reconcile_budget_exhausted_max_rpc_bytes_last"`
	ReconcileBudgetExhaustedRPCCallsLast      int64   `json:"reconcile_budget_exhausted_rpc_calls_last"`
	ReconcileBudgetExhaustedEntriesLast       int64   `json:"reconcile_budget_exhausted_entries_last"`
	ReconcileBudgetExhaustedRangesHandledLast int64   `json:"reconcile_budget_exhausted_ranges_handled_last"`
	ReconcileBudgetExhaustedRangesSplitLast   int64   `json:"reconcile_budget_exhausted_ranges_split_last"`
	ReconcileBudgetExhaustedRetriesLast       int64   `json:"reconcile_budget_exhausted_retries_last"`
	RangeChecksumRequestsTotal                int64   `json:"range_checksum_requests_total"`
	RangeChecksumRejectionsTotal              int64   `json:"range_checksum_rejections_total"`
	RangeDigestRequestsTotal                  int64   `json:"range_digest_requests_total"`
	RangeDigestRejectionsTotal                int64   `json:"range_digest_rejections_total"`
	FetchRequestsTotal                        int64   `json:"fetch_requests_total"`
	FetchRequestRejectionsTotal               int64   `json:"fetch_request_rejections_total"`
	FetchResponseBudgetRejectionsTotal        int64   `json:"fetch_response_budget_rejections_total"`
	ReconcileMaxRangeDrillDownRPCs            int     `json:"reconcile_max_range_drilldown_rpcs"`
}

func (c *Client) DRStatus(ctx context.Context) (*DRStatus, []byte, error) {
	raw, status, err := c.bestDRStatus(ctx)
	return status, raw, err
}

func (c *Client) ActiveDRStatus(ctx context.Context) (*DRStatus, []byte, error) {
	addr, err := c.ActiveAddr(ctx)
	if err != nil {
		return nil, nil, err
	}
	code, body, err := c.doAt(ctx, addr, http.MethodGet, "/v1/sys/replication/dr/status", nil)
	if err != nil {
		return nil, body, err
	}
	if code != http.StatusOK {
		return nil, body, fmt.Errorf("%s DR status returned %d: %s", addr, code, summarizeBody(body))
	}
	status, err := decodeDRStatus(body)
	if err != nil {
		return nil, body, fmt.Errorf("decode %s DR status: %w", addr, err)
	}
	return status, body, nil
}

func (c *Client) bestDRStatus(ctx context.Context) ([]byte, *DRStatus, error) {
	var bestRaw []byte
	var best *DRStatus
	var bestScore int
	var lastErr error
	for _, addr := range c.addrs {
		code, body, err := c.doAt(ctx, addr, http.MethodGet, "/v1/sys/replication/dr/status", nil)
		if err != nil {
			lastErr = err
			continue
		}
		if code != http.StatusOK {
			lastErr = fmt.Errorf("%s DR status returned %d: %s", addr, code, summarizeBody(body))
			continue
		}
		status, err := decodeDRStatus(body)
		if err != nil {
			lastErr = fmt.Errorf("decode %s DR status: %w", addr, err)
			continue
		}
		score := drStatusScore(status)
		if best == nil || score > bestScore {
			copyStatus := *status
			best = &copyStatus
			bestRaw = append([]byte(nil), body...)
			bestScore = score
		}
	}
	if best != nil {
		return bestRaw, best, nil
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, fmt.Errorf("no DR status response")
}

func decodeDRStatus(body []byte) (*DRStatus, error) {
	var envelope struct {
		Data DRStatus `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

func drStatusScore(status *DRStatus) int {
	score := 0
	if status.Mode != "" {
		score++
	}
	if status.Mode == "primary" || status.Mode == "secondary" {
		score += 10
	}
	if status.SecondaryState == "streaming" {
		score += 10
	}
	if status.LagEntries == 0 {
		score += 5
	}
	if status.PrimaryIndex > 0 && status.LastAppliedIndex >= status.PrimaryIndex {
		score += 5
	}
	return score
}

type CheckpointVerification struct {
	Pass              bool   `json:"pass"`
	Reason            string `json:"reason"`
	State             string `json:"state"`
	CheckpointIndex   uint64 `json:"checkpoint_index"`
	AccumulatorIndex  uint64 `json:"accumulator_index"`
	MatchedRanges     int    `json:"matched_ranges"`
	MismatchedRanges  int    `json:"mismatched_ranges"`
	MissingRanges     int    `json:"missing_ranges"`
	PhysicalScanUsed  bool   `json:"physical_scan_used"`
	OptimizerReseeded bool   `json:"optimizer_reseeded"`
}

func (c *Client) VerifyCheckpoint(ctx context.Context) (*CheckpointVerification, []byte, error) {
	var data CheckpointVerification
	raw, err := c.Read(ctx, "sys/replication/dr/secondary/verify-checkpoint", &data)
	if err != nil {
		return nil, raw, err
	}
	return &data, raw, nil
}

type ExportPlan struct {
	PlanID          string          `json:"plan_id"`
	State           string          `json:"state"`
	Manifest        string          `json:"manifest"`
	RelationshipID  string          `json:"relationship_id"`
	CheckpointID    string          `json:"checkpoint_id"`
	CheckpointIndex uint64          `json:"checkpoint_index"`
	EntryCount      int             `json:"entry_count"`
	SegmentCount    int             `json:"segment_count"`
	Error           string          `json:"error,omitempty"`
	ManifestParsed  PreSeedManifest `json:"-"`
}

type PreSeedManifest struct {
	Version         int                 `json:"version"`
	RelationshipID  string              `json:"relationship_id"`
	CheckpointID    string              `json:"checkpoint_id"`
	CheckpointIndex uint64              `json:"checkpoint_index"`
	BundleFormat    string              `json:"bundle_format"`
	BundleSegments  []PreSeedDescriptor `json:"bundle_segments"`
}

type PreSeedDescriptor struct {
	Index      int    `json:"index"`
	EntryCount int    `json:"entry_count"`
	ByteCount  uint64 `json:"byte_count"`
}

func (c *Client) ExportPlan(ctx context.Context, relationshipID string, ttlSeconds, segmentMaxBytes int, async bool) (*ExportPlan, []byte, error) {
	var data ExportPlan
	raw, err := c.Write(ctx, "sys/replication/dr/primary/preseed/export-plan", map[string]any{
		"relationship_id":   relationshipID,
		"ttl_seconds":       ttlSeconds,
		"segment_max_bytes": segmentMaxBytes,
		"async":             async,
	}, &data)
	if err != nil {
		return nil, raw, err
	}
	if data.Manifest != "" {
		if err := json.Unmarshal([]byte(data.Manifest), &data.ManifestParsed); err != nil {
			return nil, raw, fmt.Errorf("decode export plan manifest: %w", err)
		}
	}
	return &data, raw, nil
}

func (c *Client) ExportPlanStatus(ctx context.Context, planID string) (*ExportPlan, []byte, error) {
	var data ExportPlan
	raw, err := c.Write(ctx, "sys/replication/dr/primary/preseed/export-plan-status", map[string]any{
		"plan_id": planID,
	}, &data)
	if err != nil {
		return nil, raw, err
	}
	if data.Manifest != "" {
		if err := json.Unmarshal([]byte(data.Manifest), &data.ManifestParsed); err != nil {
			return nil, raw, fmt.Errorf("decode export plan manifest: %w", err)
		}
	}
	return &data, raw, nil
}

type ExportSegment struct {
	Segment      string `json:"segment"`
	SegmentIndex int    `json:"segment_index"`
	EntryCount   int    `json:"entry_count"`
	ByteCount    uint64 `json:"byte_count"`
	SHA256       string `json:"sha256"`
}

func (c *Client) ExportSegment(ctx context.Context, manifest string, segmentIndex int) (*ExportSegment, []byte, error) {
	var data ExportSegment
	raw, err := c.Write(ctx, "sys/replication/dr/primary/preseed/export-segment", map[string]any{
		"manifest":      manifest,
		"segment_index": segmentIndex,
	}, &data)
	if err != nil {
		return nil, raw, err
	}
	if data.Segment == "" {
		return nil, raw, fmt.Errorf("export segment %d response did not include segment", segmentIndex)
	}
	return &data, raw, nil
}

func (c *Client) ImportBegin(ctx context.Context, token, manifest string) ([]byte, error) {
	return c.Write(ctx, "sys/replication/dr/secondary/preseed/import-begin", map[string]any{
		"token":                              token,
		"manifest":                           manifest,
		"confirm_replace_replicated_storage": true,
	}, nil)
}

func (c *Client) ImportSegment(ctx context.Context, token, segment string) ([]byte, error) {
	return c.Write(ctx, "sys/replication/dr/secondary/preseed/import-segment", map[string]any{
		"token":   token,
		"segment": segment,
	}, nil)
}

type ImportComplete struct {
	Enabled bool `json:"enabled"`
}

func (c *Client) ImportComplete(ctx context.Context, token string) (*ImportComplete, []byte, error) {
	var data ImportComplete
	raw, err := c.Write(ctx, "sys/replication/dr/secondary/preseed/import-complete", map[string]any{
		"token":                              token,
		"confirm_replace_replicated_storage": true,
		"enable_secondary":                   true,
	}, &data)
	if err != nil {
		return nil, raw, err
	}
	return &data, raw, nil
}

type SealStatus struct {
	Sealed bool `json:"sealed"`
}

func (c *Client) SealStatus(ctx context.Context, addr string) (*SealStatus, error) {
	code, body, err := c.doAt(ctx, strings.TrimRight(addr, "/"), http.MethodGet, "/v1/sys/seal-status", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("%s seal-status returned %d: %s", addr, code, summarizeBody(body))
	}
	var status SealStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("decode %s seal-status: %w", addr, err)
	}
	return &status, nil
}

func (c *Client) UnsealAt(ctx context.Context, addr, key string) error {
	code, body, err := c.doAt(ctx, strings.TrimRight(addr, "/"), http.MethodPost, "/v1/sys/unseal", mustJSON(map[string]any{"key": key}))
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("%s unseal returned %d: %s", addr, code, summarizeBody(body))
	}
	return nil
}

type RaftConfiguration struct {
	Data struct {
		Config struct {
			Servers []RaftServer `json:"servers"`
		} `json:"config"`
	} `json:"data"`
}

type RaftServer struct {
	Leader   bool   `json:"leader"`
	Voter    bool   `json:"voter"`
	Suffrage string `json:"suffrage"`
}

func (c *Client) RaftConfiguration(ctx context.Context) (*RaftConfiguration, []byte, error) {
	code, body, err := c.do(ctx, http.MethodGet, "/v1/sys/storage/raft/configuration", nil)
	if err != nil {
		return nil, body, err
	}
	if code != http.StatusOK {
		return nil, body, fmt.Errorf("raft configuration returned %d: %s", code, summarizeBody(body))
	}
	var config RaftConfiguration
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, body, fmt.Errorf("decode raft configuration: %w", err)
	}
	return &config, body, nil
}

func (c *Client) WriteTuning(ctx context.Context, values map[string]any) ([]byte, error) {
	return c.Write(ctx, "sys/replication/dr/tuning", values, nil)
}

func (c *Client) ReadRaw(ctx context.Context, path string) ([]byte, error) {
	return c.Read(ctx, path, nil)
}
