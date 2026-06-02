package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/artifact"
	"github.com/openbao/openbao/scripts/dr-harness/internal/bao"
	"github.com/openbao/openbao/scripts/dr-harness/internal/topology"
	"github.com/openbao/openbao/scripts/dr-harness/internal/workload"
)

type CompositeLifecycleConfig struct {
	RootDir                 string
	Topology                string
	EnvFile                 string
	ResultsDir              string
	DRStressBin             string
	Timeout                 time.Duration
	HTTPTimeout             time.Duration
	Reset                   bool
	Build                   bool
	SeedKeys                int
	SeedConcurrency         int
	SegmentMaxBytes         int
	AsyncExportPlan         bool
	Duration                time.Duration
	Concurrency             int
	StepdownInterval        time.Duration
	PreSeedAfter            time.Duration
	Secondary2OutageAfter   time.Duration
	Secondary2OutageSeconds time.Duration
	ProgressInterval        time.Duration
	MonitorInterval         time.Duration
	MaxWait                 time.Duration
	TuningProfile           string
}

type compositeLifecycleSummary struct {
	RunID                       string                      `json:"run_id"`
	StartedAt                   string                      `json:"started_at"`
	CompletedAt                 string                      `json:"completed_at"`
	Seed                        workload.BulkKVResult       `json:"seed"`
	PreSeed                     *compositePreSeedSummary    `json:"preseed_secondary1,omitempty"`
	Secondary2Outage            *compositeOutageSummary     `json:"secondary2_outage,omitempty"`
	FinalPrimaryActive          string                      `json:"final_primary_active"`
	FinalSecondary1Active       string                      `json:"final_secondary1_active"`
	FinalSecondary2Active       string                      `json:"final_secondary2_active"`
	FinalSecondary1Status       *bao.DRStatus               `json:"final_secondary1_status,omitempty"`
	FinalSecondary2Status       *bao.DRStatus               `json:"final_secondary2_status,omitempty"`
	FinalSecondary1Verification *bao.CheckpointVerification `json:"final_secondary1_checkpoint,omitempty"`
	FinalSecondary2Verification *bao.CheckpointVerification `json:"final_secondary2_checkpoint,omitempty"`
	WorkloadResult              json.RawMessage             `json:"workload_result,omitempty"`
}

type compositePreSeedSummary struct {
	StartedAt               string                      `json:"started_at"`
	CompletedAt             string                      `json:"completed_at"`
	RelationshipID          string                      `json:"relationship_id"`
	EntryCount              int                         `json:"entry_count"`
	SegmentCount            int                         `json:"segment_count"`
	CheckpointIndex         uint64                      `json:"checkpoint_index"`
	VerifiedCheckpointIndex uint64                      `json:"verified_checkpoint_index"`
	Status                  *bao.DRStatus               `json:"status,omitempty"`
	Verification            *bao.CheckpointVerification `json:"checkpoint_verification,omitempty"`
}

type compositeOutageSummary struct {
	StartedAt                  string        `json:"started_at"`
	RestartedAt                string        `json:"restarted_at"`
	CompletedAt                string        `json:"completed_at"`
	OutageSeconds              int           `json:"outage_seconds"`
	Secondary2ActiveBefore     string        `json:"secondary2_active_before"`
	Secondary2ActiveAfter      string        `json:"secondary2_active_after"`
	BeforePrimary              *bao.DRStatus `json:"before_primary,omitempty"`
	AfterPrimary               *bao.DRStatus `json:"after_primary,omitempty"`
	BeforeSecondary2           *bao.DRStatus `json:"before_secondary2,omitempty"`
	AfterSecondary2            *bao.DRStatus `json:"after_secondary2,omitempty"`
	UsedReconciliation         bool          `json:"used_reconciliation"`
	UsedIndexedRepair          bool          `json:"used_indexed_repair"`
	JournalRangeTooOldObserved bool          `json:"journal_range_too_old_observed"`
}

func RunCompositeLifecycleSoak(ctx context.Context, cfg CompositeLifecycleConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	if cfg.Reset {
		if err := topology.Reset(ctx, topology.Config{
			RootDir:  cfg.RootDir,
			Topology: cfg.Topology,
			EnvFile:  cfg.EnvFile,
			Timeout:  cfg.Timeout,
			Build:    cfg.Build,
		}, "", false); err != nil {
			return err
		}
	}

	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, false, false)
	if err != nil {
		return err
	}
	run, err := artifact.New(cfg.ResultsDir, "composite-lifecycle")
	if err != nil {
		return err
	}
	summary := &compositeLifecycleSummary{
		RunID:     run.ID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	run.Logf("run_id=%s", run.ID)
	run.Logf("started_at=%s", summary.StartedAt)
	run.Logf("duration_seconds=%d", int(cfg.Duration.Seconds()))
	run.Logf("concurrency=%d", cfg.Concurrency)
	run.Logf("seed_keys=%d", cfg.SeedKeys)
	run.Logf("seed_concurrency=%d", cfg.SeedConcurrency)
	run.Logf("stepdown_interval_seconds=%d", int(cfg.StepdownInterval.Seconds()))
	run.Logf("preseed_after_seconds=%d", int(cfg.PreSeedAfter.Seconds()))
	run.Logf("secondary2_outage_after_seconds=%d", int(cfg.Secondary2OutageAfter.Seconds()))
	run.Logf("secondary2_outage_seconds=%d", int(cfg.Secondary2OutageSeconds.Seconds()))

	if cfg.TuningProfile != "" && cfg.TuningProfile != "none" {
		if err := applyTuningProfileToAll(ctx, rt, cfg.TuningProfile, run); err != nil {
			return err
		}
	}

	primaryActive, err := topology.WaitActiveAddr(ctx, rt.primary, "primary composite", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Active, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 composite", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Active, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 composite", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active=%s", primaryActive)
	run.Logf("secondary1_active_initial=%s", secondary1Active)
	run.Logf("secondary2_active_initial=%s", secondary2Active)

	if err := rt.primary.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return fmt.Errorf("ensure kv mount: %w", err)
	}

	run.Logf("disabling_secondary1_initial_at=%s", time.Now().UTC().Format(time.RFC3339))
	if err := rt.secondary1DRRoot.DisableSecondary(ctx); err != nil && !strings.Contains(err.Error(), "not in DR secondary mode") {
		return fmt.Errorf("disable secondary1 before composite seed: %w", err)
	}
	if err := waitDRMode(ctx, rt.secondary1DRRoot, "secondary1 disabled", "disabled", cfg.Timeout); err != nil {
		return err
	}
	_, disabledRaw, _ := rt.secondary1DRRoot.DRStatus(ctx)
	_ = run.WriteFile("secondary1-disabled-status.json", disabledRaw)

	run.Logf("seeding_primary_keys_at=%s", time.Now().UTC().Format(time.RFC3339))
	seed, err := workload.BulkKV(ctx, workload.BulkKVConfig{
		Client:      rt.primary,
		Mount:       "kv",
		KeyPrefix:   run.ID + "/seed",
		Phase:       "composite-initial-seed",
		RunID:       run.ID,
		Count:       cfg.SeedKeys,
		Concurrency: cfg.SeedConcurrency,
		LogFile:     run.Path("seed-bulk.log"),
	})
	summary.Seed = seed
	_ = run.WriteJSON("seed-bulk-result.json", seed)
	if err != nil {
		return fmt.Errorf("initial seed write: %w", err)
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after initial seed", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryQuiescent(ctx, rt.secondary2DRRoot, "secondary2 after initial seed", cfg.MaxWait); err != nil {
		return err
	}
	_, secondary2SeedRaw, _ := rt.secondary2DRRoot.DRStatus(ctx)
	_ = run.WriteFile("secondary2-after-seed-status.json", secondary2SeedRaw)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stressCmd, harnessOut, harnessErr, err := startCompositeDRStress(runCtx, cfg, run, rt.layout)
	if err != nil {
		return err
	}
	stressRunning := true
	defer func() {
		closeFiles(harnessOut, harnessErr)
		if stressRunning && stressCmd.Process != nil {
			_ = stressCmd.Process.Kill()
		}
	}()
	run.Logf("stress_pid=%d", stressCmd.Process.Pid)

	type sideResult struct {
		name string
		err  error
	}
	sideCh := make(chan sideResult, 2)
	preSeedCh := make(chan *compositePreSeedSummary, 1)
	outageCh := make(chan *compositeOutageSummary, 1)

	go func() {
		err := sleepContext(runCtx, cfg.PreSeedAfter)
		if err == nil {
			var preseed *compositePreSeedSummary
			preseed, err = runCompositePreSeedSecondary1(runCtx, cfg, run, rt)
			if err == nil {
				preSeedCh <- preseed
			}
		}
		sideCh <- sideResult{name: "secondary1-preseed", err: err}
	}()
	go func() {
		err := sleepContext(runCtx, cfg.Secondary2OutageAfter)
		if err == nil {
			var outage *compositeOutageSummary
			outage, err = runCompositeSecondary2Outage(runCtx, cfg, run, rt)
			if err == nil {
				outageCh <- outage
			}
		}
		sideCh <- sideResult{name: "secondary2-outage", err: err}
	}()

	stressDone := make(chan error, 1)
	go func() {
		stressDone <- stressCmd.Wait()
	}()

	sideDone := 0
	stressDoneSeen := false
	for sideDone < 2 || !stressDoneSeen {
		select {
		case result := <-sideCh:
			sideDone++
			if result.err != nil {
				cancel()
				return fmt.Errorf("%s failed: %w", result.name, result.err)
			}
			run.Logf("%s_complete_at=%s", result.name, time.Now().UTC().Format(time.RFC3339))
		case err := <-stressDone:
			stressDoneSeen = true
			stressRunning = false
			closeFiles(harnessOut, harnessErr)
			if err != nil {
				cancel()
				return fmt.Errorf("dr-stress exited non-zero; artifacts preserved in %s: %w", run.Dir, err)
			}
			run.Logf("stress_rc=0")
		case <-ctx.Done():
			cancel()
			return ctx.Err()
		}
	}
	if len(preSeedCh) > 0 {
		summary.PreSeed = <-preSeedCh
	}
	if len(outageCh) > 0 {
		summary.Secondary2Outage = <-outageCh
	}

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 final", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 final", cfg.MaxWait); err != nil {
		return err
	}

	summary.FinalPrimaryActive, _ = topology.WaitActiveAddr(ctx, rt.primary, "primary final", 120*time.Second)
	summary.FinalSecondary1Active, _ = topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 final", 120*time.Second)
	summary.FinalSecondary2Active, _ = topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 final", 120*time.Second)

	secondary1Status, secondary1Raw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	secondary2Status, secondary2Raw, err := rt.secondary2DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	summary.FinalSecondary1Status = secondary1Status
	summary.FinalSecondary2Status = secondary2Status
	_ = run.WriteFile("final-secondary1-status.json", secondary1Raw)
	_ = run.WriteFile("final-secondary2-status.json", secondary2Raw)

	if err := requireNoRepairFailures("secondary1 final", nil, secondary1Status); err != nil {
		return err
	}
	if err := requireNoRepairFailures("secondary2 final", nil, secondary2Status); err != nil {
		return err
	}

	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "primary", summary.FinalPrimaryActive, rt.layout.Primary.Token, "api"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary1", summary.FinalSecondary1Active, rt.layout.Primary.Token, "checkpoint"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary2", summary.FinalSecondary2Active, rt.layout.Secondary2.Token, "checkpoint"); err != nil {
		return err
	}
	if verify, raw, err := rt.secondary1DRRoot.VerifyCheckpoint(ctx); err == nil {
		summary.FinalSecondary1Verification = verify
		_ = run.WriteFile("final-secondary1-direct-verify.json", raw)
	}
	if verify, raw, err := rt.secondary2.VerifyCheckpoint(ctx); err == nil {
		summary.FinalSecondary2Verification = verify
		_ = run.WriteFile("final-secondary2-direct-verify.json", raw)
	}
	if workloadRaw, err := os.ReadFile(run.Path("result.json")); err == nil {
		summary.WorkloadResult = json.RawMessage(workloadRaw)
	}
	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	if err := run.WriteJSON("scenario-result.json", summary); err != nil {
		return err
	}
	run.Logf("completed_at=%s", summary.CompletedAt)
	fmt.Println("Composite lifecycle soak passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func (cfg CompositeLifecycleConfig) validate() error {
	if cfg.RootDir == "" || cfg.EnvFile == "" || cfg.ResultsDir == "" || cfg.DRStressBin == "" {
		return fmt.Errorf("root, env-file, results-dir, and dr-stress-bin are required")
	}
	if cfg.Topology != "ha" {
		return fmt.Errorf("composite lifecycle soak requires --topology ha")
	}
	if cfg.Timeout <= 0 || cfg.HTTPTimeout <= 0 || cfg.MaxWait <= 0 {
		return fmt.Errorf("timeout, http-timeout, and max-wait must be positive")
	}
	if cfg.SeedKeys < 1 || cfg.SeedConcurrency < 1 || cfg.SegmentMaxBytes < 1 {
		return fmt.Errorf("seed-keys, seed-concurrency, and segment-max-bytes must be positive")
	}
	if cfg.Duration <= 0 || cfg.Concurrency < 1 || cfg.ProgressInterval <= 0 || cfg.MonitorInterval <= 0 {
		return fmt.Errorf("duration, concurrency, progress-interval, and monitor-interval must be positive")
	}
	if cfg.StepdownInterval < 0 {
		return fmt.Errorf("stepdown-interval must be >= 0")
	}
	if cfg.PreSeedAfter < 0 || cfg.PreSeedAfter >= cfg.Duration {
		return fmt.Errorf("preseed-after must be >= 0 and less than duration")
	}
	if cfg.Secondary2OutageAfter < 0 || cfg.Secondary2OutageAfter >= cfg.Duration {
		return fmt.Errorf("secondary2-outage-after must be >= 0 and less than duration")
	}
	if cfg.Secondary2OutageSeconds <= 0 {
		return fmt.Errorf("secondary2-outage-seconds must be positive")
	}
	if cfg.Secondary2OutageAfter+cfg.Secondary2OutageSeconds >= cfg.Duration {
		return fmt.Errorf("secondary2 outage must end before workload duration")
	}
	if cfg.TuningProfile == "" {
		cfg.TuningProfile = "constrained"
	}
	return nil
}

func startCompositeDRStress(ctx context.Context, cfg CompositeLifecycleConfig, run *artifact.Run, layout topology.Layout) (*exec.Cmd, *os.File, *os.File, error) {
	args := []string{
		"run",
		"-run-id", run.ID,
		"-output-dir", cfg.ResultsDir,
		"-primary-addr", strings.Join(layout.Primary.Addrs, ","),
		"-primary-token", layout.Primary.Token,
		"-secondary1-addr", strings.Join(layout.Secondary1.Addrs, ","),
		"-secondary1-token", layout.Primary.Token,
		"-secondary2-addr", strings.Join(layout.Secondary2.Addrs, ","),
		"-secondary2-token", layout.Primary.Token,
		"-ensure-kv",
		"-duration", fmt.Sprintf("%d", int(cfg.Duration.Seconds())),
		"-concurrency", fmt.Sprintf("%d", cfg.Concurrency),
		"-stepdown-interval", fmt.Sprintf("%d", int(cfg.StepdownInterval.Seconds())),
		"-put-percent", "55",
		"-get-primary-percent", "25",
		"-status-s1-percent", "10",
		"-status-s2-percent", "10",
		"-test-class", "composite_lifecycle_soak",
		"-topology-label", "ha-primary-secondary1-preseed-secondary2-outage",
		"-disruption-profile", fmt.Sprintf("preseed_secondary1_secondary2_outage_%ds_stepdown_%ds", int(cfg.Secondary2OutageSeconds.Seconds()), int(cfg.StepdownInterval.Seconds())),
		"-max-wait-seconds", fmt.Sprintf("%d", int(cfg.MaxWait.Seconds())),
		"-progress-interval", fmt.Sprintf("%d", int(cfg.ProgressInterval.Seconds())),
		"-monitor-interval", fmt.Sprintf("%d", int(cfg.MonitorInterval.Seconds())),
	}
	run.Logf("dr_stress_bin=%s", cfg.DRStressBin)
	run.Logf("dr_stress_args=%s", strings.Join(shellQuoteArgs(args), " "))

	cmd := exec.CommandContext(ctx, cfg.DRStressBin, args...)
	cmd.Dir = cfg.RootDir
	out, err := os.Create(run.Path("harness.out"))
	if err != nil {
		return nil, nil, nil, err
	}
	errOut, err := os.Create(run.Path("harness.err"))
	if err != nil {
		_ = out.Close()
		return nil, nil, nil, err
	}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Start(); err != nil {
		closeFiles(out, errOut)
		return nil, nil, nil, err
	}
	return cmd, out, errOut, nil
}

func runCompositePreSeedSecondary1(ctx context.Context, cfg CompositeLifecycleConfig, run *artifact.Run, rt *harnessRuntime) (*compositePreSeedSummary, error) {
	summary := &compositePreSeedSummary{StartedAt: time.Now().UTC().Format(time.RFC3339)}
	run.Logf("secondary1_preseed_started_at=%s", summary.StartedAt)

	activationRaw, activation, err := rt.primary.NewSecondaryToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("create secondary1 pre-seed token: %w", err)
	}
	summary.RelationshipID = activation.RelationshipID

	psCfg := PreSeedConfig{
		RootDir:         cfg.RootDir,
		Topology:        cfg.Topology,
		ResultsDir:      cfg.ResultsDir,
		Timeout:         cfg.Timeout,
		SegmentMaxBytes: cfg.SegmentMaxBytes,
		AsyncExportPlan: cfg.AsyncExportPlan,
	}
	exportPlan, exportRaw, err := exportPlan(ctx, rt.primary, activation.RelationshipID, psCfg, run)
	if err != nil {
		return nil, err
	}
	_ = run.WriteFile("secondary1-preseed-export.json", exportRaw)
	manifest := exportPlan.Manifest
	if manifest == "" {
		return nil, fmt.Errorf("secondary1 pre-seed export did not include manifest")
	}
	_ = run.WriteFile("secondary1-preseed-manifest.json", []byte(manifest+"\n"))
	segmentCount := len(exportPlan.ManifestParsed.BundleSegments)
	if segmentCount == 0 {
		return nil, fmt.Errorf("secondary1 pre-seed manifest has no segments")
	}
	summary.EntryCount = exportPlan.EntryCount
	summary.SegmentCount = segmentCount
	summary.CheckpointIndex = exportPlan.CheckpointIndex
	run.Logf("secondary1_preseed_export checkpoint=%d entries=%d segments=%d", summary.CheckpointIndex, summary.EntryCount, summary.SegmentCount)

	importBeginRaw, err := rt.secondary1DRRoot.ImportBegin(ctx, activationRaw, manifest)
	if err != nil {
		return nil, fmt.Errorf("secondary1 pre-seed import-begin: %w", err)
	}
	_ = run.WriteFile("secondary1-preseed-import-begin.json", importBeginRaw)

	for i := 0; i < segmentCount; i++ {
		segment, segmentExportRaw, err := rt.primary.ExportSegment(ctx, manifest, i)
		if err != nil {
			return nil, fmt.Errorf("secondary1 export segment %d: %w", i, err)
		}
		_ = run.WriteFile(filepath.Join("secondary1-preseed-segments", fmt.Sprintf("segment-%d-export.json", i)), segmentExportRaw)
		importSegmentRaw, err := rt.secondary1DRRoot.ImportSegment(ctx, activationRaw, segment.Segment)
		if err != nil {
			return nil, fmt.Errorf("secondary1 import segment %d: %w", i, err)
		}
		_ = run.WriteFile(filepath.Join("secondary1-preseed-segments", fmt.Sprintf("segment-%d-import.json", i)), importSegmentRaw)
	}

	importComplete, importRaw, err := rt.secondary1DRRoot.ImportComplete(ctx, activationRaw)
	if err != nil {
		return nil, fmt.Errorf("secondary1 pre-seed import-complete: %w", err)
	}
	_ = run.WriteFile("secondary1-preseed-import.json", importRaw)
	if !importComplete.Enabled {
		if err := rt.secondary1DRRoot.EnableSecondary(ctx, activationRaw); err != nil {
			return nil, fmt.Errorf("enable secondary1 from pre-seed: %w", err)
		}
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after pre-seed", cfg.MaxWait); err != nil {
		return nil, err
	}
	verify, verifyRaw, err := waitVerifyCheckpoint(ctx, rt.secondary1DRRoot, "secondary1 after pre-seed", cfg.MaxWait)
	if err != nil {
		_ = run.WriteFile("secondary1-preseed-verify-error.json", verifyRaw)
		return nil, err
	}
	_ = run.WriteFile("secondary1-preseed-verify.json", verifyRaw)
	status, statusRaw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return nil, err
	}
	_ = run.WriteFile("secondary1-preseed-status.json", statusRaw)
	if err := requireNoRepairFailures("secondary1 pre-seed", nil, status); err != nil {
		return nil, err
	}
	summary.Status = status
	summary.Verification = verify
	summary.VerifiedCheckpointIndex = verify.CheckpointIndex
	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	_ = run.WriteJSON("secondary1-preseed-result.json", summary)
	return summary, nil
}

func runCompositeSecondary2Outage(ctx context.Context, cfg CompositeLifecycleConfig, run *artifact.Run, rt *harnessRuntime) (*compositeOutageSummary, error) {
	summary := &compositeOutageSummary{
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		OutageSeconds: int(cfg.Secondary2OutageSeconds.Seconds()),
	}
	run.Logf("secondary2_outage_started_at=%s", summary.StartedAt)

	beforeSecondary2, beforeSecondary2Raw, err := rt.secondary2DRRoot.DRStatus(ctx)
	if err != nil {
		return nil, err
	}
	beforePrimary, beforePrimaryRaw, err := rt.primary.DRStatus(ctx)
	if err != nil {
		return nil, err
	}
	summary.BeforeSecondary2 = beforeSecondary2
	summary.BeforePrimary = beforePrimary
	_ = run.WriteFile("secondary2-outage-before-secondary2-status.json", beforeSecondary2Raw)
	_ = run.WriteFile("secondary2-outage-before-primary-status.json", beforePrimaryRaw)

	activeBefore, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 before outage", 120*time.Second)
	if err != nil {
		return nil, err
	}
	summary.Secondary2ActiveBefore = activeBefore
	stopOut, err := topology.ComposeStop(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary2.Services...)
	_ = run.WriteFile("secondary2-stop.out", stopOut)
	if err != nil {
		return nil, err
	}
	if err := sleepContext(ctx, cfg.Secondary2OutageSeconds); err != nil {
		return nil, err
	}
	summary.RestartedAt = time.Now().UTC().Format(time.RFC3339)
	run.Logf("secondary2_restart_at=%s", summary.RestartedAt)
	startOut, err := topology.ComposeStart(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary2.Services...)
	_ = run.WriteFile("secondary2-start.out", startOut)
	if err != nil {
		return nil, err
	}
	if err := topology.UnsealCluster(ctx, rt.secondary2DRRoot, rt.layout.Secondary2, 180*time.Second); err != nil {
		return nil, err
	}
	raftRaw, err := topology.WaitRaftPeers(ctx, rt.secondary2DRRoot, "secondary2 after outage", 3, cfg.MaxWait)
	_ = run.WriteFile("secondary2-raft-after-outage.json", raftRaw)
	if err != nil {
		return nil, err
	}
	activeAfter, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 after outage", 120*time.Second)
	if err != nil {
		return nil, err
	}
	summary.Secondary2ActiveAfter = activeAfter
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after outage", cfg.MaxWait); err != nil {
		return nil, err
	}
	afterSecondary2, afterSecondary2Raw, err := rt.secondary2DRRoot.DRStatus(ctx)
	if err != nil {
		return nil, err
	}
	afterPrimary, afterPrimaryRaw, err := rt.primary.DRStatus(ctx)
	if err != nil {
		return nil, err
	}
	summary.AfterSecondary2 = afterSecondary2
	summary.AfterPrimary = afterPrimary
	_ = run.WriteFile("secondary2-outage-after-secondary2-status.json", afterSecondary2Raw)
	_ = run.WriteFile("secondary2-outage-after-primary-status.json", afterPrimaryRaw)
	if err := requireNoRepairFailures("secondary2 outage", beforeSecondary2, afterSecondary2); err != nil {
		return nil, err
	}
	summary.UsedReconciliation = afterSecondary2.ReconcileCount > beforeSecondary2.ReconcileCount
	summary.UsedIndexedRepair = afterSecondary2.FlatAccumulatorIndexedRepair > beforeSecondary2.FlatAccumulatorIndexedRepair ||
		afterSecondary2.LocalKIDIndexEntriesLoaded > beforeSecondary2.LocalKIDIndexEntriesLoaded
	summary.JournalRangeTooOldObserved = afterPrimary.JournalRangeTooOldTotal > beforePrimary.JournalRangeTooOldTotal
	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	_ = run.WriteJSON("secondary2-outage-result.json", summary)
	return summary, nil
}

func waitDRMode(ctx context.Context, client *bao.Client, label, mode string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		status, _, err := client.DRStatus(ctx)
		if err == nil && status.Mode == mode {
			fmt.Printf("%s mode=%s\n", label, mode)
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s mode=%s: %w", label, mode, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func requireNoRepairFailures(label string, before, after *bao.DRStatus) error {
	if after == nil {
		return fmt.Errorf("%s status is nil", label)
	}
	var beforeFallback, beforeFullBucket, beforeProof, beforeLoadFailures, beforeScan int64
	if before != nil {
		beforeFallback = before.LocalKIDIndexFallbackScans
		beforeFullBucket = before.FlatAccumulatorFullBucket
		beforeProof = before.FlatAccumulatorProofMismatch
		beforeLoadFailures = before.LocalKIDIndexLoadFailures
		beforeScan = before.ScanFailuresTotal
	}
	if after.LocalKIDIndexFallbackScans > beforeFallback {
		return fmt.Errorf("%s local KID fallback scans increased: %d -> %d", label, beforeFallback, after.LocalKIDIndexFallbackScans)
	}
	if after.FlatAccumulatorFullBucket > beforeFullBucket {
		return fmt.Errorf("%s full-bucket fallback increased: %d -> %d", label, beforeFullBucket, after.FlatAccumulatorFullBucket)
	}
	if after.FlatAccumulatorProofMismatch > beforeProof {
		return fmt.Errorf("%s proof mismatches increased: %d -> %d", label, beforeProof, after.FlatAccumulatorProofMismatch)
	}
	if after.LocalKIDIndexLoadFailures > beforeLoadFailures {
		return fmt.Errorf("%s local KID load failures increased: %d -> %d", label, beforeLoadFailures, after.LocalKIDIndexLoadFailures)
	}
	if after.ScanFailuresTotal > beforeScan {
		return fmt.Errorf("%s scan failures increased: %d -> %d", label, beforeScan, after.ScanFailuresTotal)
	}
	return nil
}
