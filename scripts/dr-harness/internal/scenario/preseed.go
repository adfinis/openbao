package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/artifact"
	"github.com/openbao/openbao/scripts/dr-harness/internal/bao"
	"github.com/openbao/openbao/scripts/dr-harness/internal/topology"
	"github.com/openbao/openbao/scripts/dr-harness/internal/workload"
)

type PreSeedConfig struct {
	RootDir                    string
	Topology                   string
	EnvFile                    string
	ResultsDir                 string
	Timeout                    time.Duration
	HTTPTimeout                time.Duration
	Reset                      bool
	Build                      bool
	DatasetFixture             string
	AsyncExportPlan            bool
	SeedKeys                   int
	SeedConcurrency            int
	SegmentMaxBytes            int
	PostExportWriteKeys        int
	PostExportWriteConcurrency int
}

type preSeedSummary struct {
	RelationshipID                     string                      `json:"relationship_id"`
	EntryCount                         int                         `json:"entry_count"`
	SegmentCount                       int                         `json:"segment_count"`
	PostExportWriteKeys                int                         `json:"post_export_write_keys"`
	CheckpointIndex                    uint64                      `json:"checkpoint_index"`
	VerifiedCheckpointIndex            uint64                      `json:"verified_checkpoint_index"`
	PostHandoffVerifiedCheckpointIndex uint64                      `json:"post_handoff_verified_checkpoint_index,omitempty"`
	Secondary2Status                   *bao.DRStatus               `json:"secondary2_status,omitempty"`
	PostHandoffSecondary2Status        *bao.DRStatus               `json:"post_handoff_secondary2_status,omitempty"`
	CheckpointVerification             *bao.CheckpointVerification `json:"checkpoint_verification,omitempty"`
	PostHandoffCheckpointVerification  *bao.CheckpointVerification `json:"post_handoff_checkpoint_verification,omitempty"`
	SeedBulk                           workload.BulkKVResult       `json:"seed_bulk,omitempty"`
	PostExportBulk                     workload.BulkKVResult       `json:"post_export_bulk,omitempty"`
	PrimaryActiveBeforeHandoff         string                      `json:"primary_active_before_handoff,omitempty"`
	PrimaryActiveAfterHandoff          string                      `json:"primary_active_after_handoff,omitempty"`
	Secondary2ActiveBeforeHandoff      string                      `json:"secondary2_active_before_handoff,omitempty"`
	Secondary2ActiveAfterHandoff       string                      `json:"secondary2_active_after_handoff,omitempty"`
	CompletedAt                        string                      `json:"completed_at"`
}

func RunPreSeedSmoke(ctx context.Context, cfg PreSeedConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	if cfg.Reset {
		primaryOnly := cfg.DatasetFixture != ""
		if err := topology.Reset(ctx, topology.Config{
			RootDir:  cfg.RootDir,
			Topology: cfg.Topology,
			EnvFile:  cfg.EnvFile,
			Timeout:  cfg.Timeout,
			Build:    cfg.Build,
		}, cfg.DatasetFixture, primaryOnly); err != nil {
			return err
		}
	}

	env, err := topology.LoadEnv(cfg.EnvFile)
	if err != nil {
		return err
	}
	layout, err := topology.LayoutFromEnv(cfg.Topology, env)
	if err != nil {
		return err
	}

	primaryClient, err := topology.NewClient(layout.Primary, cfg.HTTPTimeout)
	if err != nil {
		return fmt.Errorf("primary client: %w", err)
	}
	secondary2PrimaryClient, err := topology.NewClient(topology.Cluster{
		Name:  layout.Secondary2.Name,
		Addrs: layout.Secondary2.Addrs,
		Token: layout.Primary.Token,
	}, cfg.HTTPTimeout)
	if err != nil {
		return fmt.Errorf("secondary2 primary-token client: %w", err)
	}
	secondary2TokenForImport := layout.Primary.Token
	if cfg.DatasetFixture != "" {
		secondary2TokenForImport = layout.Secondary2.Token
	}
	secondary2ImportClient, err := topology.NewClient(topology.Cluster{
		Name:  layout.Secondary2.Name,
		Addrs: layout.Secondary2.Addrs,
		Token: secondary2TokenForImport,
	}, cfg.HTTPTimeout)
	if err != nil {
		return fmt.Errorf("secondary2 import client: %w", err)
	}

	run, err := artifact.New(cfg.ResultsDir, "preseed-smoke")
	if err != nil {
		return err
	}

	primaryActive := layout.Primary.Addrs[0]
	secondary2Active := layout.Secondary2.Addrs[0]
	if cfg.Topology == "ha" {
		primaryActive, err = topology.WaitActiveAddr(ctx, primaryClient, "primary", cfg.Timeout)
		if err != nil {
			return err
		}
		secondary2Active, err = topology.WaitActiveAddr(ctx, secondary2PrimaryClient, "secondary2", cfg.Timeout)
		if err != nil {
			return err
		}
	}

	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("topology=%s", cfg.Topology)
	run.Logf("primary_active=%s", primaryActive)
	run.Logf("secondary2_active=%s", secondary2Active)
	run.Logf("dataset_fixture=%s", cfg.DatasetFixture)
	run.Logf("async_export_plan=%t", cfg.AsyncExportPlan)
	run.Logf("seed_keys=%d", cfg.SeedKeys)
	run.Logf("seed_concurrency=%d", cfg.SeedConcurrency)
	run.Logf("segment_max_bytes=%d", cfg.SegmentMaxBytes)
	run.Logf("post_export_write_keys=%d", cfg.PostExportWriteKeys)
	run.Logf("post_export_write_concurrency=%d", cfg.PostExportWriteConcurrency)

	baseKey := run.ID + "/base"
	deltaKey := run.ID + "/delta-after-export"
	seedPrefix := run.ID + "/seed"
	postExportPrefix := run.ID + "/post-export"

	run.Logf("Preparing primary baseline for %s...", run.ID)
	if err := primaryClient.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return fmt.Errorf("ensure kv mount: %w", err)
	}
	if err := primaryClient.KVPut(ctx, "kv", baseKey, map[string]any{
		"phase":  "base-before-preseed-export",
		"run_id": run.ID,
		"ts":     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("write base key: %w", err)
	}

	summary := &preSeedSummary{PostExportWriteKeys: cfg.PostExportWriteKeys}
	if cfg.SeedKeys > 0 {
		run.Logf("Bulk writing %d seed keys with concurrency %d...", cfg.SeedKeys, cfg.SeedConcurrency)
		result, err := workload.BulkKV(ctx, workload.BulkKVConfig{
			Client:      primaryClient,
			Mount:       "kv",
			KeyPrefix:   seedPrefix,
			Phase:       "seed-before-preseed-export",
			RunID:       run.ID,
			Count:       cfg.SeedKeys,
			Concurrency: cfg.SeedConcurrency,
			LogFile:     run.Path("seed-bulk.log"),
		})
		summary.SeedBulk = result
		_ = run.WriteJSON("seed-bulk-result.json", result)
		if err != nil {
			return fmt.Errorf("seed bulk write: %w", err)
		}
	}

	if cfg.DatasetFixture == "" {
		if _, err := topology.WaitSecondaryReady(ctx, secondary2PrimaryClient, "secondary2-original-lineage", cfg.Timeout); err != nil {
			return err
		}
		if _, err := topology.WaitSecondaryQuiescent(ctx, secondary2PrimaryClient, "secondary2-original-lineage", cfg.Timeout); err != nil {
			return err
		}
	}

	run.Logf("Issuing fresh pre-seed activation token...")
	activationRaw, activation, err := primaryClient.NewSecondaryToken(ctx)
	if err != nil {
		return fmt.Errorf("create secondary token: %w", err)
	}
	summary.RelationshipID = activation.RelationshipID

	run.Logf("Creating segmented pre-seed export plan for relationship %s...", activation.RelationshipID)
	exportPlan, exportRaw, err := exportPlan(ctx, primaryClient, activation.RelationshipID, cfg, run)
	if err != nil {
		return err
	}
	if err := run.WriteFile("export.json", exportRaw); err != nil {
		return err
	}
	manifest := exportPlan.Manifest
	if manifest == "" {
		return fmt.Errorf("pre-seed export plan did not include manifest")
	}
	if err := run.WriteFile("manifest.json", []byte(manifest+"\n")); err != nil {
		return err
	}
	if exportPlan.ManifestParsed.RelationshipID != activation.RelationshipID {
		return fmt.Errorf("manifest relationship %q does not match activation relationship %q", exportPlan.ManifestParsed.RelationshipID, activation.RelationshipID)
	}
	segmentCount := len(exportPlan.ManifestParsed.BundleSegments)
	if segmentCount == 0 {
		return fmt.Errorf("pre-seed manifest has no bundle segments")
	}
	if segmentCount < 2 {
		return fmt.Errorf("pre-seed segmented smoke expected at least two segments, got %d", segmentCount)
	}
	summary.EntryCount = exportPlan.EntryCount
	summary.SegmentCount = segmentCount
	summary.CheckpointIndex = exportPlan.CheckpointIndex

	run.Logf("Writing post-export delta on primary...")
	if err := primaryClient.KVPut(ctx, "kv", deltaKey, map[string]any{
		"phase":  "delta-after-preseed-export",
		"run_id": run.ID,
		"ts":     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("write post-export delta: %w", err)
	}

	if cfg.DatasetFixture == "" {
		if _, err := topology.WaitSecondaryQuiescent(ctx, secondary2PrimaryClient, "secondary2-post-export-delta", cfg.Timeout); err != nil {
			return err
		}
		run.Logf("Disabling secondary2 from its existing relationship...")
		if err := secondary2PrimaryClient.DisableSecondary(ctx); err != nil {
			if !strings.Contains(err.Error(), "not in DR secondary mode") {
				return fmt.Errorf("disable secondary2: %w", err)
			}
		}
	} else {
		run.Logf("Using disabled secondary2 as fresh pre-seed target from dataset fixture.")
	}

	postErrCh := make(chan error, 1)
	postResultCh := make(chan workload.BulkKVResult, 1)
	if cfg.PostExportWriteKeys > 0 {
		run.Logf("Starting bounded post-export write pressure: keys=%d concurrency=%d", cfg.PostExportWriteKeys, cfg.PostExportWriteConcurrency)
		go func() {
			result, err := workload.BulkKV(ctx, workload.BulkKVConfig{
				Client:      primaryClient,
				Mount:       "kv",
				KeyPrefix:   postExportPrefix,
				Phase:       "post-export-adversarial",
				RunID:       run.ID,
				Count:       cfg.PostExportWriteKeys,
				Concurrency: cfg.PostExportWriteConcurrency,
				LogFile:     run.Path("post-export-bulk.log"),
			})
			postResultCh <- result
			postErrCh <- err
		}()
	}

	run.Logf("Starting segmented pre-seed import on disabled secondary2...")
	importBeginRaw, err := secondary2ImportClient.ImportBegin(ctx, activationRaw, manifest)
	if err != nil {
		return fmt.Errorf("pre-seed import-begin: %w", err)
	}
	if err := run.WriteFile("import-begin.json", importBeginRaw); err != nil {
		return err
	}

	run.Logf("Exporting and staging %d pre-seed segments...", segmentCount)
	for i := 0; i < segmentCount; i++ {
		segment, segmentExportRaw, err := primaryClient.ExportSegment(ctx, manifest, i)
		if err != nil {
			return fmt.Errorf("export segment %d: %w", i, err)
		}
		if err := run.WriteFile(filepath.Join("segments", fmt.Sprintf("segment-%d-export.json", i)), segmentExportRaw); err != nil {
			return err
		}
		if err := run.WriteFile(filepath.Join("segments", fmt.Sprintf("segment-%d.json", i)), []byte(segment.Segment+"\n")); err != nil {
			return err
		}
		var segmentDoc struct {
			Version      int               `json:"version"`
			SegmentIndex int               `json:"segment_index"`
			Entries      []json.RawMessage `json:"entries"`
		}
		if err := json.Unmarshal([]byte(segment.Segment), &segmentDoc); err != nil {
			return fmt.Errorf("decode segment %d: %w", i, err)
		}
		if segmentDoc.Version != 1 || segmentDoc.SegmentIndex != i || len(segmentDoc.Entries) == 0 {
			return fmt.Errorf("segment %d malformed: version=%d segment_index=%d entries=%d", i, segmentDoc.Version, segmentDoc.SegmentIndex, len(segmentDoc.Entries))
		}
		importSegmentRaw, err := secondary2ImportClient.ImportSegment(ctx, activationRaw, segment.Segment)
		if err != nil {
			return fmt.Errorf("import segment %d: %w", i, err)
		}
		if err := run.WriteFile(filepath.Join("segments", fmt.Sprintf("segment-%d-import.json", i)), importSegmentRaw); err != nil {
			return err
		}
	}

	run.Logf("Completing segmented pre-seed import on disabled secondary2...")
	importComplete, importRaw, err := secondary2ImportClient.ImportComplete(ctx, activationRaw)
	if err != nil {
		return fmt.Errorf("pre-seed import-complete: %w", err)
	}
	if err := run.WriteFile("import.json", importRaw); err != nil {
		return err
	}

	if cfg.PostExportWriteKeys > 0 {
		run.Logf("Waiting for bounded post-export write pressure to finish...")
		result := <-postResultCh
		summary.PostExportBulk = result
		_ = run.WriteJSON("post-export-bulk-result.json", result)
		if err := <-postErrCh; err != nil {
			return fmt.Errorf("post-export write pressure failed: %w", err)
		}
	}

	if importComplete.Enabled {
		run.Logf("Secondary2 enabled from imported pre-seed baseline.")
	} else {
		run.Logf("Enabling secondary2 from imported pre-seed baseline...")
		if err := secondary2PrimaryClient.EnableSecondary(ctx, activationRaw); err != nil {
			return fmt.Errorf("enable secondary2: %w", err)
		}
	}

	if _, err := topology.WaitSecondaryReady(ctx, secondary2PrimaryClient, "secondary2-preseed-lineage", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryQuiescent(ctx, secondary2PrimaryClient, "secondary2-preseed-lineage", cfg.Timeout); err != nil {
		return err
	}

	run.Logf("Verifying secondary2 checkpoint convergence...")
	verify, verifyRaw, err := waitVerifyCheckpoint(ctx, secondary2PrimaryClient, "secondary2-preseed-lineage", cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("verify-secondary2-error.json", verifyRaw)
		return err
	}
	if err := run.WriteFile("verify-secondary2.json", verifyRaw); err != nil {
		return err
	}
	summary.VerifiedCheckpointIndex = verify.CheckpointIndex
	summary.CheckpointVerification = verify

	status, statusRaw, err := secondary2PrimaryClient.DRStatus(ctx)
	if err != nil {
		return fmt.Errorf("read secondary2 status: %w", err)
	}
	if err := run.WriteFile("secondary2-status.json", statusRaw); err != nil {
		return err
	}
	summary.Secondary2Status = status
	if err := requireStreamingLagZero("pre-seed secondary", status); err != nil {
		return err
	}
	if err := assertKVPresent(ctx, primaryClient, "primary", baseKey); err != nil {
		return err
	}
	if err := assertKVPresent(ctx, primaryClient, "primary", deltaKey); err != nil {
		return err
	}

	if cfg.Topology == "ha" {
		if err := runPostAcceptHAHandoff(ctx, cfg, run, primaryClient, secondary2PrimaryClient, primaryActive, secondary2Active, baseKey, deltaKey, summary); err != nil {
			return err
		}
	}

	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	if err := run.WriteJSON("result.json", summary); err != nil {
		return err
	}
	if err := run.WriteFile("summary.txt", []byte(summaryText(summary))); err != nil {
		return err
	}

	fmt.Print(summaryText(summary))
	fmt.Println("Pre-seed smoke passed.")
	fmt.Printf("Run dir: %s\n", run.Dir)
	return nil
}

func (cfg *PreSeedConfig) validate() error {
	if cfg.RootDir == "" {
		return fmt.Errorf("--root is required")
	}
	if cfg.Topology != "single" && cfg.Topology != "ha" {
		return fmt.Errorf("--topology must be single or ha")
	}
	if cfg.DatasetFixture != "" && cfg.Topology != "single" {
		return fmt.Errorf("dataset fixtures currently support --topology single only")
	}
	if cfg.DatasetFixture != "" && cfg.SeedKeys == 64 {
		cfg.SeedKeys = 0
	}
	if cfg.DatasetFixture == "" && cfg.SeedKeys < 1 {
		return fmt.Errorf("--seed-keys must be >= 1")
	}
	if cfg.SeedKeys < 0 {
		return fmt.Errorf("--seed-keys must be >= 0")
	}
	if cfg.SeedConcurrency < 1 {
		return fmt.Errorf("--seed-concurrency must be >= 1")
	}
	if cfg.SegmentMaxBytes < 1 {
		return fmt.Errorf("--segment-max-bytes must be >= 1")
	}
	if cfg.PostExportWriteKeys < 0 {
		return fmt.Errorf("--post-export-write-keys must be >= 0")
	}
	if cfg.PostExportWriteConcurrency < 1 {
		return fmt.Errorf("--post-export-write-concurrency must be >= 1")
	}
	if cfg.Timeout <= 0 {
		return fmt.Errorf("--timeout must be > 0")
	}
	return nil
}

func exportPlan(ctx context.Context, primary *bao.Client, relationshipID string, cfg PreSeedConfig, run *artifact.Run) (*bao.ExportPlan, []byte, error) {
	plan, raw, err := primary.ExportPlan(ctx, relationshipID, 3600, cfg.SegmentMaxBytes, cfg.AsyncExportPlan)
	if err != nil {
		return nil, raw, fmt.Errorf("pre-seed export-plan: %w", err)
	}
	if !cfg.AsyncExportPlan {
		if plan.State != "complete" {
			return nil, raw, fmt.Errorf("sync pre-seed export-plan returned state %q", plan.State)
		}
		return plan, raw, nil
	}

	if err := run.WriteFile("export.json.initial", raw); err != nil {
		return nil, raw, err
	}
	if plan.PlanID == "" {
		return nil, raw, fmt.Errorf("async pre-seed export-plan did not return plan_id")
	}

	deadline := time.Now().Add(cfg.Timeout)
	var lastRaw []byte
	for {
		status, statusRaw, err := primary.ExportPlanStatus(ctx, plan.PlanID)
		lastRaw = statusRaw
		if err != nil {
			return nil, statusRaw, fmt.Errorf("pre-seed export-plan-status: %w", err)
		}
		switch status.State {
		case "complete":
			return status, statusRaw, nil
		case "failed":
			return nil, statusRaw, fmt.Errorf("async pre-seed export plan failed: %s", status.Error)
		case "running":
		default:
			return nil, statusRaw, fmt.Errorf("async pre-seed export plan returned unexpected state %q", status.State)
		}
		if time.Now().After(deadline) {
			return nil, lastRaw, fmt.Errorf("timed out waiting for async pre-seed export plan %s", plan.PlanID)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return nil, lastRaw, err
		}
	}
}

func waitVerifyCheckpoint(ctx context.Context, client *bao.Client, label string, timeout time.Duration) (*bao.CheckpointVerification, []byte, error) {
	deadline := time.Now().Add(timeout)
	var lastRaw []byte
	var lastErr error
	for {
		verify, raw, err := client.VerifyCheckpoint(ctx)
		lastRaw = raw
		if err == nil && verify.Pass {
			fmt.Printf("%s checkpoint verification passed\n", label)
			return verify, raw, nil
		}
		if err != nil {
			lastErr = err
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "returned 403") || strings.Contains(msg, "permission denied") {
				return nil, lastRaw, fmt.Errorf("%s checkpoint verification unauthorized: %w", label, err)
			}
		} else {
			lastErr = fmt.Errorf("checkpoint verification did not pass: reason=%s state=%s mismatched=%d missing=%d", verify.Reason, verify.State, verify.MismatchedRanges, verify.MissingRanges)
		}
		if time.Now().After(deadline) {
			return nil, lastRaw, fmt.Errorf("timed out waiting for %s checkpoint verification: %w", label, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return nil, lastRaw, err
		}
	}
}

func runPostAcceptHAHandoff(ctx context.Context, cfg PreSeedConfig, run *artifact.Run, primaryClient, secondary2Client *bao.Client, primaryBefore, secondary2Before, baseKey, deltaKey string, summary *preSeedSummary) error {
	run.Logf("Forcing HA handoff after pre-seed accept: primary=%s secondary2=%s", primaryBefore, secondary2Before)
	summary.PrimaryActiveBeforeHandoff = primaryBefore
	summary.Secondary2ActiveBeforeHandoff = secondary2Before

	if err := primaryClient.StepDown(ctx); err != nil {
		_ = run.WriteFile("stepdown-primary.err", []byte(err.Error()+"\n"))
	}
	if err := secondary2Client.StepDown(ctx); err != nil {
		_ = run.WriteFile("stepdown-secondary2.err", []byte(err.Error()+"\n"))
	}

	primaryAfter, err := topology.WaitActiveChange(ctx, primaryClient, "primary", primaryBefore, 120*time.Second)
	if err != nil {
		return err
	}
	secondary2After, err := topology.WaitActiveChange(ctx, secondary2Client, "secondary2", secondary2Before, 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveAfterHandoff = primaryAfter
	summary.Secondary2ActiveAfterHandoff = secondary2After
	run.Logf("HA handoff complete after pre-seed accept: primary=%s secondary2=%s", primaryAfter, secondary2After)

	if _, err := topology.WaitSecondaryReady(ctx, secondary2Client, "secondary2-preseed-post-handoff", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryQuiescent(ctx, secondary2Client, "secondary2-preseed-post-handoff", cfg.Timeout); err != nil {
		return err
	}
	verify, verifyRaw, err := waitVerifyCheckpoint(ctx, secondary2Client, "secondary2-preseed-post-handoff", cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("verify-secondary2-post-handoff-error.json", verifyRaw)
		return err
	}
	if err := run.WriteFile("verify-secondary2-post-handoff.json", verifyRaw); err != nil {
		return err
	}
	summary.PostHandoffVerifiedCheckpointIndex = verify.CheckpointIndex
	summary.PostHandoffCheckpointVerification = verify

	status, statusRaw, err := secondary2Client.DRStatus(ctx)
	if err != nil {
		return fmt.Errorf("read secondary2 post-handoff status: %w", err)
	}
	if err := run.WriteFile("secondary2-status-post-handoff.json", statusRaw); err != nil {
		return err
	}
	summary.PostHandoffSecondary2Status = status
	if err := requireStreamingLagZero("pre-seed secondary after handoff", status); err != nil {
		return err
	}
	if err := assertKVPresent(ctx, primaryClient, "primary after handoff", baseKey); err != nil {
		return err
	}
	if err := assertKVPresent(ctx, primaryClient, "primary after handoff", deltaKey); err != nil {
		return err
	}
	return nil
}

func requireStreamingLagZero(label string, status *bao.DRStatus) error {
	if status == nil {
		return fmt.Errorf("%s status is nil", label)
	}
	if status.SecondaryState != "streaming" || status.LagEntries != 0 {
		return fmt.Errorf("%s did not finish streaming with lag 0: state=%s lag=%d applied=%d primary=%d", label, status.SecondaryState, status.LagEntries, status.LastAppliedIndex, status.PrimaryIndex)
	}
	return nil
}

func assertKVPresent(ctx context.Context, client *bao.Client, label, key string) error {
	ok, err := client.KVExists(ctx, "kv", key)
	if err != nil {
		return fmt.Errorf("check %s kv/%s: %w", label, key, err)
	}
	if !ok {
		return fmt.Errorf("expected %s to contain kv/%s", label, key)
	}
	return nil
}

func putKVEventually(ctx context.Context, client *bao.Client, mount, key string, data map[string]any, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := client.KVPut(ctx, mount, key, data); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out writing %s/%s: %w", mount, key, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func waitKVPresent(ctx context.Context, client *bao.Client, label, key string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := client.KVExists(ctx, "kv", key)
		if err == nil && ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for %s to contain kv/%s: %w", label, key, lastErr)
			}
			return fmt.Errorf("timed out waiting for %s to contain kv/%s", label, key)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func summaryText(summary *preSeedSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "relationship_id=%s\n", summary.RelationshipID)
	fmt.Fprintf(&b, "entry_count=%d\n", summary.EntryCount)
	fmt.Fprintf(&b, "segment_count=%d\n", summary.SegmentCount)
	fmt.Fprintf(&b, "post_export_write_keys=%d\n", summary.PostExportWriteKeys)
	fmt.Fprintf(&b, "checkpoint_index=%d\n", summary.CheckpointIndex)
	fmt.Fprintf(&b, "verified_checkpoint_index=%d\n", summary.VerifiedCheckpointIndex)
	if summary.PostHandoffVerifiedCheckpointIndex > 0 {
		fmt.Fprintf(&b, "post_handoff_verified_checkpoint_index=%d\n", summary.PostHandoffVerifiedCheckpointIndex)
	}
	if summary.CompletedAt != "" {
		fmt.Fprintf(&b, "completed_at=%s\n", summary.CompletedAt)
	}
	return b.String()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}
