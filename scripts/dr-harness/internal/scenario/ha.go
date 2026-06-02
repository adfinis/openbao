package scenario

import (
	"bytes"
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
)

type HAConfig struct {
	RootDir     string
	Topology    string
	EnvFile     string
	ResultsDir  string
	Timeout     time.Duration
	HTTPTimeout time.Duration
	Reset       bool
	Build       bool
	StopSeconds time.Duration
}

type OutageConfig struct {
	RootDir          string
	Topology         string
	EnvFile          string
	ResultsDir       string
	DRStressBin      string
	Timeout          time.Duration
	HTTPTimeout      time.Duration
	Reset            bool
	Build            bool
	Duration         time.Duration
	Concurrency      int
	OutageAfter      time.Duration
	OutageSeconds    time.Duration
	ProgressInterval time.Duration
	MonitorInterval  time.Duration
	MaxWait          time.Duration
	ExpectReconcile  bool
	TuningProfile    string
	RunPrefix        string
}

type MixedLoadConfig struct {
	RootDir       string
	Topology      string
	EnvFile       string
	ResultsDir    string
	DRStressBin   string
	HTTPTimeout   time.Duration
	RunID         string
	TuningProfile string
	DRStressArgs  []string
}

type TuningLoadConfig struct {
	RootDir          string
	Topology         string
	EnvFile          string
	ResultsDir       string
	DRStressBin      string
	Timeout          time.Duration
	HTTPTimeout      time.Duration
	Reset            bool
	Build            bool
	Duration         time.Duration
	Concurrency      int
	StepdownInterval time.Duration
	FirstTuneAfter   time.Duration
	SecondTuneAfter  time.Duration
	ProgressInterval time.Duration
	MonitorInterval  time.Duration
	MaxWait          time.Duration
}

type harnessRuntime struct {
	layout           topology.Layout
	primary          *bao.Client
	secondary1       *bao.Client
	secondary2       *bao.Client
	secondary1DRRoot *bao.Client
	secondary2DRRoot *bao.Client
}

func RunQuiescentReconnect(ctx context.Context, cfg HAConfig) error {
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}

	run, err := artifact.New(cfg.ResultsDir, "quiescent-reconnect")
	if err != nil {
		return err
	}
	markerKey := run.ID + "/marker"
	run.Logf("run_id=%s", run.ID)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("timeout_seconds=%d", int(cfg.Timeout.Seconds()))

	primaryBefore, err := topology.WaitActiveAddr(ctx, rt.primary, "primary", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active_before=%s", primaryBefore)

	if err := rt.primary.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return err
	}
	if err := putKVEventually(ctx, rt.primary, "kv", markerKey, map[string]any{
		"phase":  "pre-handoff",
		"run_id": run.ID,
		"ts":     time.Now().UTC().Format(time.RFC3339),
	}, cfg.Timeout); err != nil {
		return err
	}

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 pre-handoff", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 pre-handoff", cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary1DRRoot, "secondary1 pre-handoff", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary2DRRoot, "secondary2 pre-handoff", markerKey, cfg.Timeout); err != nil {
		return err
	}

	before1, raw1, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	before2, raw2, err := rt.secondary2DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	_ = run.WriteFile("before-secondary1-status.json", raw1)
	_ = run.WriteFile("before-secondary2-status.json", raw2)

	run.Logf("fast_path_before_secondary1=%d", before1.FlatAccumulatorFastPathTotal)
	run.Logf("fast_path_before_secondary2=%d", before2.FlatAccumulatorFastPathTotal)
	run.Logf("reconcile_count_before_secondary1=%d", before1.ReconcileCount)
	run.Logf("reconcile_count_before_secondary2=%d", before2.ReconcileCount)

	run.Logf("forcing_primary_handoff_at=%s", time.Now().UTC().Format(time.RFC3339))
	if err := rt.primary.StepDown(ctx); err != nil {
		_ = run.WriteFile("stepdown-primary.err", []byte(err.Error()+"\n"))
	}
	primaryAfter, err := topology.WaitActiveChange(ctx, rt.primary, "primary after handoff", primaryBefore, 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active_after=%s", primaryAfter)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after handoff", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after handoff", cfg.Timeout); err != nil {
		return err
	}
	after1, rawAfter1, err := topology.WaitOptimizedReconnect(ctx, rt.secondary1DRRoot, "secondary1", before1.FlatAccumulatorFastPathTotal, before1.ReconcileCount, cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("after-secondary1-status.json", rawAfter1)
		return err
	}
	after2, rawAfter2, err := topology.WaitOptimizedReconnect(ctx, rt.secondary2DRRoot, "secondary2", before2.FlatAccumulatorFastPathTotal, before2.ReconcileCount, cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("after-secondary2-status.json", rawAfter2)
		return err
	}
	_ = run.WriteFile("after-secondary1-status.json", rawAfter1)
	_ = run.WriteFile("after-secondary2-status.json", rawAfter2)

	if err := waitKVPresent(ctx, rt.primary, "primary after handoff", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary1DRRoot, "secondary1 after handoff", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary2DRRoot, "secondary2 after handoff", markerKey, cfg.Timeout); err != nil {
		return err
	}

	result := map[string]any{
		"run_id":                       run.ID,
		"primary_active_before":        primaryBefore,
		"primary_active_after":         primaryAfter,
		"secondary1_reconcile_before":  before1.ReconcileCount,
		"secondary1_reconcile_after":   after1.ReconcileCount,
		"secondary1_fast_path_before":  before1.FlatAccumulatorFastPathTotal,
		"secondary1_fast_path_after":   after1.FlatAccumulatorFastPathTotal,
		"secondary2_reconcile_before":  before2.ReconcileCount,
		"secondary2_reconcile_after":   after2.ReconcileCount,
		"secondary2_fast_path_before":  before2.FlatAccumulatorFastPathTotal,
		"secondary2_fast_path_after":   after2.FlatAccumulatorFastPathTotal,
		"secondary1_lag_entries_after": after1.LagEntries,
		"secondary2_lag_entries_after": after2.LagEntries,
		"completed_at":                 time.Now().UTC().Format(time.RFC3339),
	}
	if err := run.WriteJSON("result.json", result); err != nil {
		return err
	}
	run.Logf("completed_at=%s", result["completed_at"])
	fmt.Println("Quiescent reconnect fast-path smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func RunAccumulatorColdRestart(ctx context.Context, cfg HAConfig) error {
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}
	if cfg.StopSeconds < 0 {
		return fmt.Errorf("--stop-seconds must be >= 0")
	}

	run, err := artifact.New(cfg.ResultsDir, "accumulator-cold-restart")
	if err != nil {
		return err
	}
	markerKey := run.ID + "/marker"
	run.Logf("run_id=%s", run.ID)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("timeout_seconds=%d", int(cfg.Timeout.Seconds()))
	run.Logf("stop_seconds=%d", int(cfg.StopSeconds.Seconds()))

	primaryActive, err := topology.WaitActiveAddr(ctx, rt.primary, "primary", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Before, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 pre-restart", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Before, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 pre-restart", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active=%s", primaryActive)
	run.Logf("secondary1_active_before=%s", secondary1Before)
	run.Logf("secondary2_active_before=%s", secondary2Before)

	if err := rt.primary.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return err
	}
	if err := putKVEventually(ctx, rt.primary, "kv", markerKey, map[string]any{
		"phase":  "pre-cold-restart",
		"run_id": run.ID,
		"ts":     time.Now().UTC().Format(time.RFC3339),
	}, cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 pre-restart", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 pre-restart", cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary1DRRoot, "secondary1 pre-restart", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary2DRRoot, "secondary2 pre-restart", markerKey, cfg.Timeout); err != nil {
		return err
	}

	before, beforeRaw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	_ = run.WriteFile("before-secondary1-status.json", beforeRaw)
	run.Logf("last_applied_before_secondary1=%d", before.LastAppliedIndex)
	run.Logf("reconcile_count_before_secondary1=%d", before.ReconcileCount)

	restartSince := time.Now().UTC()
	run.Logf("stopping_secondary1_at=%s", restartSince.Format(time.RFC3339))
	stopOut, err := topology.ComposeStop(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-stop.out", stopOut)
	if err != nil {
		return err
	}
	if cfg.StopSeconds > 0 {
		if err := sleepContext(ctx, cfg.StopSeconds); err != nil {
			return err
		}
	}
	run.Logf("starting_secondary1_at=%s", time.Now().UTC().Format(time.RFC3339))
	startOut, err := topology.ComposeStart(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-start.out", startOut)
	if err != nil {
		return err
	}
	if err := topology.UnsealCluster(ctx, rt.secondary1DRRoot, rt.layout.Secondary1, 180*time.Second); err != nil {
		return err
	}
	raftRaw, err := topology.WaitRaftPeers(ctx, rt.secondary1DRRoot, "secondary1 restarted", 3, cfg.Timeout)
	_ = run.WriteFile("secondary1-raft-after-restart.json", raftRaw)
	if err != nil {
		return err
	}

	secondary1After, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 after cold restart", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("secondary1_active_after=%s", secondary1After)
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after cold restart", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after secondary1 restart", cfg.Timeout); err != nil {
		return err
	}

	after, afterRaw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	_ = run.WriteFile("after-secondary1-status.json", afterRaw)
	if after.LastAppliedIndex < before.LastAppliedIndex {
		return fmt.Errorf("secondary1 last_applied_index moved backwards after cold restart: %d -> %d", before.LastAppliedIndex, after.LastAppliedIndex)
	}
	if after.FlatAccumulatorCursorIndex < before.LastAppliedIndex {
		return fmt.Errorf("secondary1 flat accumulator cursor did not cover pre-restart applied index: cursor=%d before=%d", after.FlatAccumulatorCursorIndex, before.LastAppliedIndex)
	}
	if after.ReconcileCount > before.ReconcileCount {
		return fmt.Errorf("secondary1 ran reconciliation after cold restart; expected stream replay from persisted accumulator cursor: %d -> %d", before.ReconcileCount, after.ReconcileCount)
	}
	if after.ScanFailuresTotal > before.ScanFailuresTotal || after.LocalKIDIndexFallbackScans > before.LocalKIDIndexFallbackScans || after.FlatAccumulatorFullBucket > before.FlatAccumulatorFullBucket || after.FlatAccumulatorProofMismatch > before.FlatAccumulatorProofMismatch || after.LocalKIDIndexLoadFailures > before.LocalKIDIndexLoadFailures {
		return fmt.Errorf("secondary1 optimizer fallback counters increased after cold restart: scan_failures=%d->%d fallback_scans=%d->%d full_bucket=%d->%d proof_mismatch=%d->%d load_failures=%d->%d",
			before.ScanFailuresTotal, after.ScanFailuresTotal,
			before.LocalKIDIndexFallbackScans, after.LocalKIDIndexFallbackScans,
			before.FlatAccumulatorFullBucket, after.FlatAccumulatorFullBucket,
			before.FlatAccumulatorProofMismatch, after.FlatAccumulatorProofMismatch,
			before.LocalKIDIndexLoadFailures, after.LocalKIDIndexLoadFailures)
	}

	logs, _ := topology.ComposeLogs(ctx, cfg.RootDir, cfg.Topology, restartSince, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-restart.log", logs)
	if bytes.Contains(logs, []byte("local scan complete")) || bytes.Contains(logs, []byte("starting reconciliation")) {
		return fmt.Errorf("secondary1 restart logs show scanned reconciliation during cold restart")
	}
	if err := waitKVPresent(ctx, rt.primary, "primary after secondary1 restart", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary1DRRoot, "secondary1 after cold restart", markerKey, cfg.Timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary2DRRoot, "secondary2 after secondary1 restart", markerKey, cfg.Timeout); err != nil {
		return err
	}

	result := map[string]any{
		"run_id":                          run.ID,
		"secondary1_active_before":        secondary1Before,
		"secondary1_active_after":         secondary1After,
		"last_applied_before_secondary1":  before.LastAppliedIndex,
		"last_applied_after_secondary1":   after.LastAppliedIndex,
		"flat_accumulator_cursor_after":   after.FlatAccumulatorCursorIndex,
		"flat_accumulator_snapshot_after": after.FlatAccumulatorSnapshotIndex,
		"reconcile_count_before":          before.ReconcileCount,
		"reconcile_count_after":           after.ReconcileCount,
		"scan_failures_before":            before.ScanFailuresTotal,
		"scan_failures_after":             after.ScanFailuresTotal,
		"local_kid_fallback_before":       before.LocalKIDIndexFallbackScans,
		"local_kid_fallback_after":        after.LocalKIDIndexFallbackScans,
		"completed_at":                    time.Now().UTC().Format(time.RFC3339),
	}
	if err := run.WriteJSON("result.json", result); err != nil {
		return err
	}
	run.Logf("completed_at=%s", result["completed_at"])
	fmt.Println("Accumulator cold-restart smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func RunSecondaryOutage(ctx context.Context, cfg OutageConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}
	if cfg.TuningProfile != "" {
		if err := applyTuningProfile(ctx, rt, cfg.TuningProfile); err != nil {
			return err
		}
	}

	run, err := artifact.New(cfg.ResultsDir, cfg.RunPrefix)
	if err != nil {
		return err
	}
	run.Logf("run_id=%s", run.ID)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("duration_seconds=%d", int(cfg.Duration.Seconds()))
	run.Logf("concurrency=%d", cfg.Concurrency)
	run.Logf("outage_after_seconds=%d", int(cfg.OutageAfter.Seconds()))
	run.Logf("outage_seconds=%d", int(cfg.OutageSeconds.Seconds()))
	run.Logf("expect_reconcile=%t", cfg.ExpectReconcile)
	run.Logf("tuning_profile=%s", cfg.TuningProfile)

	primaryActive, err := topology.WaitActiveAddr(ctx, rt.primary, "primary pre-outage", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Active, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 pre-outage", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Active, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 pre-outage", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active_before=%s", primaryActive)
	run.Logf("secondary1_active_before=%s", secondary1Active)
	run.Logf("secondary2_active_before=%s", secondary2Active)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 pre-outage", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 pre-outage", cfg.Timeout); err != nil {
		return err
	}
	beforeSecondary1, beforeSecondary1Raw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	beforePrimary, beforePrimaryRaw, err := rt.primary.DRStatus(ctx)
	if err != nil {
		return err
	}
	_ = run.WriteFile("before-secondary1-status.json", beforeSecondary1Raw)
	_ = run.WriteFile("before-primary-status.json", beforePrimaryRaw)

	stressCmd, harnessOut, harnessErr, err := startDRStress(ctx, cfg, run, primaryActive, rt.layout, cfg.DRStressBin)
	if err != nil {
		return err
	}
	run.Logf("stress_pid=%d", stressCmd.Process.Pid)

	if err := sleepContext(ctx, cfg.OutageAfter); err != nil {
		return err
	}
	restartSince := time.Now().UTC()
	run.Logf("stopping_secondary1_at=%s", restartSince.Format(time.RFC3339))
	stopOut, err := topology.ComposeStop(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-stop.out", stopOut)
	if err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	if err := sleepContext(ctx, cfg.OutageSeconds); err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	run.Logf("starting_secondary1_at=%s", time.Now().UTC().Format(time.RFC3339))
	startOut, err := topology.ComposeStart(ctx, cfg.RootDir, cfg.Topology, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-start.out", startOut)
	if err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	if err := topology.UnsealCluster(ctx, rt.secondary1DRRoot, rt.layout.Secondary1, 180*time.Second); err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	raftRaw, err := topology.WaitRaftPeers(ctx, rt.secondary1DRRoot, "secondary1 after outage", 3, cfg.MaxWait)
	_ = run.WriteFile("secondary1-raft-after-outage.json", raftRaw)
	if err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	secondary1Active, err = topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 after outage", 120*time.Second)
	if err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}
	run.Logf("secondary1_active_after=%s", secondary1Active)
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after outage", cfg.MaxWait); err != nil {
		_ = stressCmd.Process.Kill()
		return err
	}

	run.Logf("waiting_for_stress_pid=%d", stressCmd.Process.Pid)
	stressErr := stressCmd.Wait()
	closeFiles(harnessOut, harnessErr)
	if stressErr != nil {
		return fmt.Errorf("dr-stress exited non-zero; artifacts preserved in %s: %w", run.Dir, stressErr)
	}
	run.Logf("stress_rc=0")

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 final", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 final", cfg.MaxWait); err != nil {
		return err
	}
	afterSecondary1, afterSecondary1Raw, err := rt.secondary1DRRoot.DRStatus(ctx)
	if err != nil {
		return err
	}
	primaryActive, _ = topology.WaitActiveAddr(ctx, rt.primary, "primary final", 120*time.Second)
	afterPrimary, afterPrimaryRaw, err := rt.primary.DRStatus(ctx)
	if err != nil {
		return err
	}
	_ = run.WriteFile("after-secondary1-status.json", afterSecondary1Raw)
	_ = run.WriteFile("after-primary-status.json", afterPrimaryRaw)

	if err := validateOutageCounters(cfg, beforeSecondary1, afterSecondary1, beforePrimary, afterPrimary); err != nil {
		return err
	}

	logs, _ := topology.ComposeLogs(ctx, cfg.RootDir, cfg.Topology, restartSince, rt.layout.Secondary1.Services...)
	_ = run.WriteFile("secondary1-outage-restart.log", logs)
	if !cfg.ExpectReconcile && (bytes.Contains(logs, []byte("local scan complete")) || bytes.Contains(logs, []byte("starting reconciliation"))) {
		return fmt.Errorf("secondary1 restart logs show scanned reconciliation during within-horizon outage")
	}

	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "primary", primaryActive, rt.layout.Primary.Token, "api"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary1", secondary1Active, rt.layout.Secondary1.Token, "checkpoint"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary2", secondary2Active, rt.layout.Secondary2.Token, "checkpoint"); err != nil {
		return err
	}

	result := map[string]any{
		"run_id":                           run.ID,
		"expect_reconcile":                 cfg.ExpectReconcile,
		"primary_active_final":             primaryActive,
		"secondary1_active_final":          secondary1Active,
		"last_applied_before_secondary1":   beforeSecondary1.LastAppliedIndex,
		"last_applied_after_secondary1":    afterSecondary1.LastAppliedIndex,
		"reconcile_before_secondary1":      beforeSecondary1.ReconcileCount,
		"reconcile_after_secondary1":       afterSecondary1.ReconcileCount,
		"indexed_repair_before_secondary1": beforeSecondary1.FlatAccumulatorIndexedRepair,
		"indexed_repair_after_secondary1":  afterSecondary1.FlatAccumulatorIndexedRepair,
		"local_kid_fallback_before":        beforeSecondary1.LocalKIDIndexFallbackScans,
		"local_kid_fallback_after":         afterSecondary1.LocalKIDIndexFallbackScans,
		"journal_range_too_old_before":     beforePrimary.JournalRangeTooOldTotal,
		"journal_range_too_old_after":      afterPrimary.JournalRangeTooOldTotal,
		"completed_at":                     time.Now().UTC().Format(time.RFC3339),
	}
	if err := run.WriteJSON("result.json", result); err != nil {
		return err
	}
	run.Logf("completed_at=%s", result["completed_at"])
	if cfg.ExpectReconcile {
		fmt.Println("Secondary outage reconcile smoke passed.")
	} else {
		fmt.Println("Secondary outage smoke passed.")
	}
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func RunMixedLoadSmoke(ctx context.Context, cfg MixedLoadConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	env, err := topology.LoadEnv(cfg.EnvFile)
	if err != nil {
		return err
	}
	layout, err := topology.LayoutFromEnv(cfg.Topology, env)
	if err != nil {
		return err
	}
	primary, err := topology.NewClient(layout.Primary, cfg.HTTPTimeout)
	if err != nil {
		return err
	}

	if cfg.RunID == "" {
		cfg.RunID = fmt.Sprintf("drmixed-%s", time.Now().UTC().Format("20060102T150405Z"))
	}
	runDir := filepath.Join(cfg.ResultsDir, cfg.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	run := &artifact.Run{ID: cfg.RunID, Dir: runDir}
	run.Logf("run_id=%s", cfg.RunID)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("topology=%s", cfg.Topology)

	primaryAddr := strings.Join(layout.Primary.Addrs, ",")
	secondary1Addr := strings.Join(layout.Secondary1.Addrs, ",")
	secondary2Addr := strings.Join(layout.Secondary2.Addrs, ",")

	tuningProfile := strings.TrimSpace(cfg.TuningProfile)
	if cfg.Topology == "ha" && tuningProfile == "" {
		tuningProfile = "constrained"
	}
	if cfg.Topology == "ha" && tuningProfile != "" && tuningProfile != "none" {
		run.Logf("ha_smoke_tuning_profile=%s", tuningProfile)
		if err := applyPrimaryTuningProfile(ctx, primary, tuningProfile, run); err != nil {
			return err
		}
	}

	args := []string{
		"run",
		"-primary-addr", primaryAddr,
		"-primary-token", layout.Primary.Token,
		"-secondary1-addr", secondary1Addr,
		"-secondary1-token", layout.Primary.Token,
		"-secondary2-addr", secondary2Addr,
		"-secondary2-token", layout.Primary.Token,
		"-ensure-kv",
		"-output-dir", cfg.ResultsDir,
		"-run-id", cfg.RunID,
		"-duration", "120",
		"-concurrency", "24",
		"-max-wait-seconds", "180",
	}
	if cfg.Topology == "ha" && tuningProfile != "" && tuningProfile != "none" {
		args = append(args, "-disruption-profile", fmt.Sprintf("primary_stepdown_%s_tuning", tuningProfile))
	}
	args = append(args, cfg.DRStressArgs...)
	run.Logf("dr_stress_bin=%s", cfg.DRStressBin)
	run.Logf("dr_stress_args=%s", strings.Join(shellQuoteArgs(args), " "))

	cmd := exec.CommandContext(ctx, cfg.DRStressBin, args...)
	cmd.Dir = cfg.RootDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dr-stress run failed: %w", err)
	}
	run.Logf("completed_at=%s", time.Now().UTC().Format(time.RFC3339))
	fmt.Printf("Mixed-load smoke passed.\nRun: %s\n", run.Dir)
	return nil
}

func RunTuningLoadSmoke(ctx context.Context, cfg TuningLoadConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}

	run, err := artifact.New(cfg.ResultsDir, "tuning-ha-load")
	if err != nil {
		return err
	}
	run.Logf("run_id=%s", run.ID)
	run.Logf("run_dir=%s", run.Dir)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("duration_seconds=%d", int(cfg.Duration.Seconds()))
	run.Logf("concurrency=%d", cfg.Concurrency)
	run.Logf("stepdown_interval_seconds=%d", int(cfg.StepdownInterval.Seconds()))
	run.Logf("first_tune_after_seconds=%d", int(cfg.FirstTuneAfter.Seconds()))
	run.Logf("second_tune_after_seconds=%d", int(cfg.SecondTuneAfter.Seconds()))

	primaryActive, err := topology.WaitActiveAddr(ctx, rt.primary, "primary pre-tuning-load", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Active, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 pre-tuning-load", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Active, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 pre-tuning-load", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_active_before=%s", primaryActive)
	run.Logf("secondary1_active_before=%s", secondary1Active)
	run.Logf("secondary2_active_before=%s", secondary2Active)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 pre-tuning-load", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 pre-tuning-load", cfg.Timeout); err != nil {
		return err
	}
	captureTuningLoadState(ctx, run, "before", rt)

	stressCmd, harnessOut, harnessErr, err := startTuningLoadDRStress(ctx, cfg, run, rt.layout)
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

	if err := sleepContext(ctx, cfg.FirstTuneAfter); err != nil {
		return err
	}
	run.Logf("constrained_tuning_at=%s", time.Now().UTC().Format(time.RFC3339))
	if err := applyTuningProfileToAll(ctx, rt, "constrained", run); err != nil {
		return err
	}
	captureTuningLoadState(ctx, run, "after-constrained", rt)

	if err := sleepContext(ctx, cfg.SecondTuneAfter-cfg.FirstTuneAfter); err != nil {
		return err
	}
	run.Logf("relaxed_tuning_at=%s", time.Now().UTC().Format(time.RFC3339))
	if err := applyTuningProfileToAll(ctx, rt, "relaxed", run); err != nil {
		return err
	}
	captureTuningLoadState(ctx, run, "after-relaxed", rt)

	primaryAfterHandoff, secondary1AfterHandoff, secondary2AfterHandoff, err := forceHAHandoffForTuningLoad(ctx, rt, run, cfg.MaxWait)
	if err != nil {
		return err
	}
	captureTuningLoadState(ctx, run, "after-handoff", rt)

	run.Logf("waiting_for_stress_pid=%d", stressCmd.Process.Pid)
	stressErr := stressCmd.Wait()
	stressRunning = false
	closeFiles(harnessOut, harnessErr)
	if stressErr != nil {
		return fmt.Errorf("dr-stress exited non-zero; artifacts preserved in %s: %w", run.Dir, stressErr)
	}
	run.Logf("stress_rc=0")

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 final", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 final", cfg.MaxWait); err != nil {
		return err
	}
	primaryFinal, err := topology.WaitActiveAddr(ctx, rt.primary, "primary final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Final, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Final, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 final", 120*time.Second)
	if err != nil {
		return err
	}
	captureTuningLoadState(ctx, run, "final", rt)

	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "primary", primaryFinal, rt.layout.Primary.Token, "api"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary1", secondary1Final, rt.layout.Secondary1.Token, "checkpoint"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary2", secondary2Final, rt.layout.Secondary2.Token, "checkpoint"); err != nil {
		return err
	}

	result := map[string]any{
		"run_id":                    run.ID,
		"primary_active_before":     primaryActive,
		"secondary1_active_before":  secondary1Active,
		"secondary2_active_before":  secondary2Active,
		"primary_after_handoff":     primaryAfterHandoff,
		"secondary1_after_handoff":  secondary1AfterHandoff,
		"secondary2_after_handoff":  secondary2AfterHandoff,
		"primary_active_final":      primaryFinal,
		"secondary1_active_final":   secondary1Final,
		"secondary2_active_final":   secondary2Final,
		"duration_seconds":          int(cfg.Duration.Seconds()),
		"concurrency":               cfg.Concurrency,
		"stepdown_interval_seconds": int(cfg.StepdownInterval.Seconds()),
		"completed_at":              time.Now().UTC().Format(time.RFC3339),
	}
	if err := run.WriteJSON("scenario-result.json", result); err != nil {
		return err
	}
	run.Logf("completed_at=%s", result["completed_at"])
	fmt.Println("Dynamic tuning HA load smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func prepareHARuntime(ctx context.Context, root, topologyName, envFile string, timeout, httpTimeout time.Duration, reset, build bool) (*harnessRuntime, error) {
	if topologyName != "ha" {
		return nil, fmt.Errorf("scenario requires --topology ha")
	}
	if reset {
		if err := topology.Reset(ctx, topology.Config{RootDir: root, Topology: topologyName, EnvFile: envFile, Timeout: timeout, Build: build}, "", false); err != nil {
			return nil, err
		}
	}
	env, err := topology.LoadEnv(envFile)
	if err != nil {
		return nil, err
	}
	layout, err := topology.LayoutFromEnv(topologyName, env)
	if err != nil {
		return nil, err
	}
	primary, err := topology.NewClient(layout.Primary, httpTimeout)
	if err != nil {
		return nil, err
	}
	secondary1, err := topology.NewClient(layout.Secondary1, httpTimeout)
	if err != nil {
		return nil, err
	}
	secondary2, err := topology.NewClient(layout.Secondary2, httpTimeout)
	if err != nil {
		return nil, err
	}
	secondary1DRRoot, err := topology.NewClient(topology.Cluster{Name: layout.Secondary1.Name, Addrs: layout.Secondary1.Addrs, Token: layout.Primary.Token}, httpTimeout)
	if err != nil {
		return nil, err
	}
	secondary2DRRoot, err := topology.NewClient(topology.Cluster{Name: layout.Secondary2.Name, Addrs: layout.Secondary2.Addrs, Token: layout.Primary.Token}, httpTimeout)
	if err != nil {
		return nil, err
	}
	if !reset {
		if _, err := topology.WaitSecondaryReady(ctx, secondary1DRRoot, "secondary1", timeout); err != nil {
			return nil, err
		}
		if _, err := topology.WaitSecondaryReady(ctx, secondary2DRRoot, "secondary2", timeout); err != nil {
			return nil, err
		}
	}
	return &harnessRuntime{layout: layout, primary: primary, secondary1: secondary1, secondary2: secondary2, secondary1DRRoot: secondary1DRRoot, secondary2DRRoot: secondary2DRRoot}, nil
}

func (cfg *OutageConfig) validate() error {
	if cfg.Topology != "ha" {
		return fmt.Errorf("outage scenarios require --topology ha")
	}
	if cfg.DRStressBin == "" {
		return fmt.Errorf("--dr-stress-bin is required")
	}
	if _, err := os.Stat(cfg.DRStressBin); err != nil {
		return fmt.Errorf("dr-stress binary %s is not available: %w", cfg.DRStressBin, err)
	}
	if cfg.Duration <= 0 || cfg.Concurrency <= 0 || cfg.OutageAfter <= 0 || cfg.OutageSeconds <= 0 {
		return fmt.Errorf("duration, concurrency, outage-after, and outage-seconds must be positive")
	}
	if cfg.OutageAfter >= cfg.Duration {
		return fmt.Errorf("--outage-after must be less than --duration")
	}
	if cfg.OutageAfter+cfg.OutageSeconds+15*time.Second >= cfg.Duration {
		return fmt.Errorf("--duration must leave at least 15s after secondary restart")
	}
	if cfg.RunPrefix == "" {
		cfg.RunPrefix = "secondary-outage"
	}
	return nil
}

func (cfg *MixedLoadConfig) validate() error {
	switch cfg.Topology {
	case "single", "ha":
	default:
		return fmt.Errorf("smoke requires --topology single or ha")
	}
	if cfg.DRStressBin == "" {
		return fmt.Errorf("--dr-stress-bin is required")
	}
	if _, err := os.Stat(cfg.DRStressBin); err != nil {
		return fmt.Errorf("dr-stress binary %s is not available: %w", cfg.DRStressBin, err)
	}
	if cfg.ResultsDir == "" {
		return fmt.Errorf("--results-dir is required")
	}
	return nil
}

func (cfg *TuningLoadConfig) validate() error {
	if cfg.Topology != "ha" {
		return fmt.Errorf("tuning-load-smoke requires --topology ha")
	}
	if cfg.DRStressBin == "" {
		return fmt.Errorf("--dr-stress-bin is required")
	}
	if _, err := os.Stat(cfg.DRStressBin); err != nil {
		return fmt.Errorf("dr-stress binary %s is not available: %w", cfg.DRStressBin, err)
	}
	if cfg.ResultsDir == "" {
		return fmt.Errorf("--results-dir is required")
	}
	if cfg.Duration <= 0 || cfg.Concurrency <= 0 || cfg.FirstTuneAfter <= 0 || cfg.SecondTuneAfter <= 0 {
		return fmt.Errorf("duration, concurrency, first-tune-after, and second-tune-after must be positive")
	}
	if cfg.StepdownInterval < 0 || cfg.ProgressInterval <= 0 || cfg.MonitorInterval <= 0 || cfg.MaxWait <= 0 {
		return fmt.Errorf("stepdown-interval must be >= 0 and progress/monitor/max-wait must be positive")
	}
	if cfg.FirstTuneAfter >= cfg.Duration {
		return fmt.Errorf("--first-tune-after must be less than --duration")
	}
	if cfg.SecondTuneAfter <= cfg.FirstTuneAfter || cfg.SecondTuneAfter >= cfg.Duration {
		return fmt.Errorf("--second-tune-after must be greater than --first-tune-after and less than --duration")
	}
	return nil
}

func startTuningLoadDRStress(ctx context.Context, cfg TuningLoadConfig, run *artifact.Run, layout topology.Layout) (*exec.Cmd, *os.File, *os.File, error) {
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

func startDRStress(ctx context.Context, cfg OutageConfig, run *artifact.Run, primaryActive string, layout topology.Layout, bin string) (*exec.Cmd, *os.File, *os.File, error) {
	args := []string{
		"run",
		"-run-id", run.ID,
		"-output-dir", cfg.ResultsDir,
		"-primary-addr", primaryActive,
		"-primary-token", layout.Primary.Token,
		"-secondary1-addr", strings.Join(layout.Secondary1.Addrs, ","),
		"-secondary1-token", layout.Primary.Token,
		"-secondary2-addr", strings.Join(layout.Secondary2.Addrs, ","),
		"-secondary2-token", layout.Primary.Token,
		"-ensure-kv",
		"-duration", fmt.Sprintf("%d", int(cfg.Duration.Seconds())),
		"-concurrency", fmt.Sprintf("%d", cfg.Concurrency),
		"-put-percent", "70",
		"-get-primary-percent", "20",
		"-status-s1-percent", "0",
		"-status-s2-percent", "10",
		"-test-class", "recovery_disruption",
		"-topology-label", "primary+2-secondary",
		"-disruption-profile", fmt.Sprintf("secondary1_full_cluster_outage_%ds_%s", int(cfg.OutageSeconds.Seconds()), emptyDefault(cfg.TuningProfile, "default")),
		"-max-wait-seconds", fmt.Sprintf("%d", int(cfg.MaxWait.Seconds())),
		"-progress-interval", fmt.Sprintf("%d", int(cfg.ProgressInterval.Seconds())),
		"-monitor-interval", fmt.Sprintf("%d", int(cfg.MonitorInterval.Seconds())),
	}
	cmd := exec.CommandContext(ctx, bin, args...)
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

func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

func validateOutageCounters(cfg OutageConfig, before, after, beforePrimary, afterPrimary *bao.DRStatus) error {
	if after.LastAppliedIndex < before.LastAppliedIndex {
		return fmt.Errorf("secondary1 last_applied_index moved backwards after outage: %d -> %d", before.LastAppliedIndex, after.LastAppliedIndex)
	}
	if after.FlatAccumulatorCursorIndex < before.LastAppliedIndex {
		return fmt.Errorf("secondary1 flat accumulator cursor did not cover pre-outage applied index: cursor=%d before=%d", after.FlatAccumulatorCursorIndex, before.LastAppliedIndex)
	}
	if after.ScanFailuresTotal > before.ScanFailuresTotal {
		return fmt.Errorf("secondary1 reported new scan failures after outage: %d -> %d", before.ScanFailuresTotal, after.ScanFailuresTotal)
	}
	if cfg.ExpectReconcile {
		if after.ReconcileCount <= before.ReconcileCount &&
			after.FlatAccumulatorIndexedRepair <= before.FlatAccumulatorIndexedRepair &&
			after.LocalKIDIndexBucketLoads <= before.LocalKIDIndexBucketLoads &&
			after.LocalKIDIndexEntriesLoaded <= before.LocalKIDIndexEntriesLoaded {
			return fmt.Errorf("secondary1 did not run reconciliation after out-of-horizon outage")
		}
		if afterPrimary.JournalRangeTooOldTotal <= beforePrimary.JournalRangeTooOldTotal {
			return fmt.Errorf("primary did not report journal range too old during out-of-horizon outage: %d -> %d", beforePrimary.JournalRangeTooOldTotal, afterPrimary.JournalRangeTooOldTotal)
		}
		if after.FlatAccumulatorCursorIndex < after.LastAppliedIndex {
			return fmt.Errorf("secondary1 flat accumulator cursor did not cover final applied index after reconciliation: cursor=%d applied=%d", after.FlatAccumulatorCursorIndex, after.LastAppliedIndex)
		}
		if after.FlatAccumulatorIndexedRepair <= before.FlatAccumulatorIndexedRepair &&
			after.LocalKIDIndexEntriesLoaded <= before.LocalKIDIndexEntriesLoaded {
			return fmt.Errorf("secondary1 did not use indexed repair during out-of-horizon reconciliation")
		}
		if after.LocalKIDIndexFallbackScans > before.LocalKIDIndexFallbackScans ||
			after.FlatAccumulatorFullBucket > before.FlatAccumulatorFullBucket ||
			after.FlatAccumulatorProofMismatch > before.FlatAccumulatorProofMismatch ||
			after.LocalKIDIndexLoadFailures > before.LocalKIDIndexLoadFailures {
			return fmt.Errorf("secondary1 indexed repair fallback/proof/load failure counters increased")
		}
		return nil
	}
	if after.ReconcileCount != before.ReconcileCount {
		return fmt.Errorf("secondary1 ran reconciliation after within-horizon outage: %d -> %d", before.ReconcileCount, after.ReconcileCount)
	}
	if after.LocalKIDIndexFallbackScans > before.LocalKIDIndexFallbackScans ||
		after.FlatAccumulatorFullBucket > before.FlatAccumulatorFullBucket ||
		after.FlatAccumulatorProofMismatch > before.FlatAccumulatorProofMismatch ||
		after.LocalKIDIndexLoadFailures > before.LocalKIDIndexLoadFailures {
		return fmt.Errorf("secondary1 fallback/proof/load failure counters increased after within-horizon outage")
	}
	if afterPrimary.JournalRangeTooOldTotal > beforePrimary.JournalRangeTooOldTotal {
		return fmt.Errorf("primary reported journal range too old during within-horizon outage: %d -> %d", beforePrimary.JournalRangeTooOldTotal, afterPrimary.JournalRangeTooOldTotal)
	}
	return nil
}

func runDRStressVerify(ctx context.Context, bin, runDir, label, addr, token, method string) error {
	outPath := filepath.Join(runDir, fmt.Sprintf("verify-%s.json", label))
	errPath := filepath.Join(runDir, fmt.Sprintf("verify-%s.err", label))
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for attempt := 1; ; attempt++ {
		err := runDRStressVerifyOnce(ctx, bin, runDir, label, addr, token, method, outPath, errPath)
		if err == nil {
			return nil
		}
		lastErr = err
		if method != "checkpoint" || time.Now().After(deadline) || !checkpointVerifyRetryable(outPath) {
			return lastErr
		}
		archiveVerifyAttempt(outPath, errPath, attempt)
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func runDRStressVerifyOnce(ctx context.Context, bin, runDir, label, addr, token, method, outPath, errPath string) error {
	args := []string{"verify", "-addr", addr, "-token", token, "-run-dir", runDir, "-sample", "0", "-method", method, "-json"}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	errOut, err := os.Create(errPath)
	if err != nil {
		return err
	}
	defer errOut.Close()
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify %s with %s method: %w", label, method, err)
	}
	return nil
}

func checkpointVerifyRetryable(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var result struct {
		Pass       bool `json:"pass"`
		Checkpoint struct {
			Reason string `json:"reason"`
		} `json:"checkpoint"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Pass {
		return false
	}
	if checkpointVerifyReasonRetryable(result.Checkpoint.Reason) {
		return true
	}
	for _, detail := range result.Details {
		if checkpointVerifyReasonRetryable(detail.Reason) {
			return true
		}
	}
	return false
}

func checkpointVerifyReasonRetryable(reason string) bool {
	for _, needle := range []string{
		"accumulator_checkpoint_index_mismatch",
		"checkpoint build concurrency exceeded",
		"budget_exceeded",
		"code = ResourceExhausted",
		"code = Unavailable",
		"checkpoint index fence failed",
		"checkpoint index fence timeout",
		"transport: Error while dialing",
		"tls: internal error",
	} {
		if strings.Contains(reason, needle) {
			return true
		}
	}
	return false
}

func archiveVerifyAttempt(outPath, errPath string, attempt int) {
	outAttempt := strings.TrimSuffix(outPath, ".json") + fmt.Sprintf("-attempt-%d.json", attempt)
	errAttempt := strings.TrimSuffix(errPath, ".err") + fmt.Sprintf("-attempt-%d.err", attempt)
	_ = os.Rename(outPath, outAttempt)
	_ = os.Rename(errPath, errAttempt)
}

func applyTuningProfile(ctx context.Context, rt *harnessRuntime, profile string) error {
	values, err := tuningProfileValues(profile)
	if err != nil {
		return err
	}
	if _, err := rt.primary.WriteTuning(ctx, values); err != nil {
		return fmt.Errorf("write primary tuning: %w", err)
	}
	if _, err := rt.secondary1DRRoot.WriteTuning(ctx, values); err != nil {
		return fmt.Errorf("write secondary1 tuning: %w", err)
	}
	if _, err := rt.secondary2DRRoot.WriteTuning(ctx, values); err != nil {
		return fmt.Errorf("write secondary2 tuning: %w", err)
	}
	return nil
}

func applyTuningProfileToAll(ctx context.Context, rt *harnessRuntime, profile string, run *artifact.Run) error {
	run.Logf("applying_tuning_profile=%s", profile)
	if err := writeTuningProfileArtifact(ctx, rt.primary, profile, "primary", run); err != nil {
		return err
	}
	if err := writeTuningProfileArtifact(ctx, rt.secondary1DRRoot, profile, "secondary1", run); err != nil {
		return err
	}
	if err := writeTuningProfileArtifact(ctx, rt.secondary2DRRoot, profile, "secondary2", run); err != nil {
		return err
	}
	return nil
}

func writeTuningProfileArtifact(ctx context.Context, client *bao.Client, profile, label string, run *artifact.Run) error {
	values, err := tuningProfileValues(profile)
	if err != nil {
		return err
	}
	var raw []byte
	var lastErr error
	deadline := time.Now().Add(90 * time.Second)
	for attempt := 1; ; attempt++ {
		client.ClearActiveAddr()
		if _, err := topology.WaitActiveAddr(ctx, client, label+" tuning write", 30*time.Second); err != nil {
			lastErr = err
		} else {
			raw, lastErr = client.WriteTuning(ctx, values)
			if lastErr == nil {
				break
			}
		}
		_ = run.WriteFile(fmt.Sprintf("tuning-%s-%s-attempt-%d.err", profile, label, attempt), []byte(lastErr.Error()+"\n"))
		if time.Now().After(deadline) || !tuningWriteRetryable(lastErr) {
			err = lastErr
			break
		}
		if sleepErr := sleepContext(ctx, 2*time.Second); sleepErr != nil {
			return sleepErr
		}
	}
	if err == nil && len(bytes.TrimSpace(raw)) == 0 {
		raw, _ = json.MarshalIndent(map[string]any{
			"ok":      true,
			"profile": profile,
			"target":  label,
		}, "", "  ")
		raw = append(raw, '\n')
	}
	_ = run.WriteFile(fmt.Sprintf("tuning-%s-%s.json", profile, label), raw)
	if err != nil {
		_ = run.WriteFile(fmt.Sprintf("tuning-%s-%s.err", profile, label), []byte(err.Error()+"\n"))
		return fmt.Errorf("write %s tuning to %s: %w", profile, label, err)
	}
	return nil
}

func tuningWriteRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"active context canceled",
		"connection refused",
		"connection reset",
		"context deadline exceeded",
		"eof",
		"no active node",
		"standby",
		"timeout",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func applyPrimaryTuningProfile(ctx context.Context, primary *bao.Client, profile string, run *artifact.Run) error {
	values, err := tuningProfileValues(profile)
	if err != nil {
		return err
	}
	active, err := topology.WaitActiveAddr(ctx, primary, "primary tuning", 120*time.Second)
	if err != nil {
		return err
	}
	run.Logf("primary_tuning_active=%s", active)
	raw, err := primary.WriteTuning(ctx, values)
	if err == nil && len(bytes.TrimSpace(raw)) == 0 {
		raw, _ = json.MarshalIndent(map[string]any{
			"ok":      true,
			"profile": profile,
		}, "", "  ")
		raw = append(raw, '\n')
	}
	_ = run.WriteFile(fmt.Sprintf("tuning-%s-primary.json", profile), raw)
	if err != nil {
		return fmt.Errorf("write primary tuning: %w", err)
	}
	tuningRaw, err := primary.ReadRaw(ctx, "sys/replication/dr/tuning")
	_ = run.WriteFile(fmt.Sprintf("after-%s-primary-tuning.json", profile), tuningRaw)
	if err != nil {
		return fmt.Errorf("read primary tuning: %w", err)
	}
	return nil
}

func captureTuningLoadState(ctx context.Context, run *artifact.Run, label string, rt *harnessRuntime) {
	captureStatus := func(name string, client *bao.Client) {
		_, raw, err := client.DRStatus(ctx)
		if len(raw) > 0 {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-status.json", label, name), raw)
		}
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-status.err", label, name), []byte(err.Error()+"\n"))
		}
	}
	captureTuning := func(name string, client *bao.Client) {
		client.ClearActiveAddr()
		if _, err := topology.WaitActiveAddr(ctx, client, name+" tuning", 30*time.Second); err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-tuning.err", label, name), []byte(err.Error()+"\n"))
			return
		}
		raw, err := client.ReadRaw(ctx, "sys/replication/dr/tuning")
		if len(raw) > 0 {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-tuning.json", label, name), raw)
		}
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-tuning.err", label, name), []byte(err.Error()+"\n"))
		}
	}

	captureStatus("primary", rt.primary)
	captureStatus("secondary1", rt.secondary1DRRoot)
	captureStatus("secondary2", rt.secondary2DRRoot)
	captureTuning("primary", rt.primary)
	captureTuning("secondary1", rt.secondary1DRRoot)
	captureTuning("secondary2", rt.secondary2DRRoot)
}

func forceHAHandoffForTuningLoad(ctx context.Context, rt *harnessRuntime, run *artifact.Run, timeout time.Duration) (string, string, string, error) {
	primaryActive, err := topology.WaitActiveAddr(ctx, rt.primary, "primary tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	secondary1Active, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	secondary2Active, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	run.Logf("forcing_ha_handoff primary=%s secondary1=%s secondary2=%s", primaryActive, secondary1Active, secondary2Active)

	stepDownForTuning(ctx, rt.primary, "primary", run)
	stepDownForTuning(ctx, rt.secondary1DRRoot, "secondary1", run)
	stepDownForTuning(ctx, rt.secondary2DRRoot, "secondary2", run)

	primaryAfter, err := topology.WaitActiveAddr(ctx, rt.primary, "primary after tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after tuning handoff", timeout); err != nil {
		return "", "", "", err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after tuning handoff", timeout); err != nil {
		return "", "", "", err
	}
	secondary1After, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 after tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	secondary2After, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 after tuning handoff", 120*time.Second)
	if err != nil {
		return "", "", "", err
	}
	run.Logf("ha_handoff_complete primary=%s secondary1=%s secondary2=%s", primaryAfter, secondary1After, secondary2After)
	return primaryAfter, secondary1After, secondary2After, nil
}

func stepDownForTuning(ctx context.Context, client *bao.Client, label string, run *artifact.Run) {
	if err := client.StepDown(ctx); err != nil {
		_ = run.WriteFile(fmt.Sprintf("stepdown-%s.err", label), []byte(err.Error()+"\n"))
		return
	}
	_ = run.WriteFile(fmt.Sprintf("stepdown-%s.out", label), []byte("ok\n"))
}

func tuningProfileValues(profile string) (map[string]any, error) {
	switch strings.TrimSpace(profile) {
	case "constrained":
		return constrainedTuning(), nil
	case "oversized-checkpoint":
		return oversizedCheckpointTuning(), nil
	case "out-of-horizon":
		return outOfHorizonTuning(), nil
	case "relaxed":
		return relaxedTuning(), nil
	default:
		return nil, fmt.Errorf("unknown tuning profile %q", profile)
	}
}

func constrainedTuning() map[string]any {
	return map[string]any{
		"checkpoint_ttl_seconds":                            900,
		"checkpoint_global_budget_bytes":                    536870912,
		"checkpoint_per_relationship_budget_bytes":          134217728,
		"stream_buffer_max_entries":                         20000,
		"stream_buffer_max_bytes":                           134217728,
		"reconcile_max_rpc_bytes":                           67108864,
		"reconcile_max_wall_time_seconds":                   900,
		"reconcile_max_inflight_tasks":                      8,
		"stream_batch_max_entries":                          128,
		"stream_batch_max_bytes":                            524288,
		"stream_batch_max_wait_milliseconds":                20,
		"stream_journal_enabled":                            true,
		"stream_journal_max_bytes":                          67108864,
		"stream_journal_segment_bytes":                      4194304,
		"stream_journal_retention_seconds":                  900,
		"reconcile_apply_workers":                           8,
		"reconcile_put_batch_max_entries":                   256,
		"reconcile_put_batch_max_bytes":                     1048576,
		"convergence_min_rate_ratio":                        0.40,
		"convergence_stall_seconds":                         90,
		"fallback_enabled":                                  true,
		"fallback_stall_seconds":                            90,
		"fallback_failure_threshold":                        2,
		"fallback_min_lag_entries":                          256,
		"fallback_cooldown_seconds":                         180,
		"fallback_max_per_hour":                             4,
		"checkpoint_artifact_enabled":                       true,
		"checkpoint_artifact_global_budget_bytes":           536870912,
		"checkpoint_artifact_per_relationship_budget_bytes": 134217728,
		"checkpoint_artifact_ttl_seconds":                   900,
		"checkpoint_artifact_segment_bytes":                 4194304,
		"dr_backpressure_enabled":                           true,
		"dr_backpressure_degraded_ratio":                    0.75,
		"dr_backpressure_critical_ratio":                    0.50,
		"dr_backpressure_min_lag_entries":                   512,
		"dr_backpressure_horizon_seconds":                   60,
		"dr_backpressure_degraded_min_qps":                  96,
		"dr_backpressure_critical_min_qps":                  48,
	}
}

func oversizedCheckpointTuning() map[string]any {
	values := constrainedTuning()
	values["checkpoint_per_relationship_budget_bytes"] = 262144
	values["checkpoint_artifact_per_relationship_budget_bytes"] = 262144
	return values
}

func outOfHorizonTuning() map[string]any {
	return map[string]any{
		"checkpoint_ttl_seconds":                            900,
		"checkpoint_global_budget_bytes":                    536870912,
		"checkpoint_per_relationship_budget_bytes":          134217728,
		"stream_buffer_max_entries":                         1024,
		"stream_buffer_max_bytes":                           4194304,
		"reconcile_max_rpc_bytes":                           67108864,
		"reconcile_max_wall_time_seconds":                   900,
		"reconcile_max_inflight_tasks":                      8,
		"stream_batch_max_entries":                          128,
		"stream_batch_max_bytes":                            524288,
		"stream_batch_max_wait_milliseconds":                20,
		"stream_journal_enabled":                            true,
		"stream_journal_max_bytes":                          262144,
		"stream_journal_segment_bytes":                      32768,
		"stream_journal_retention_seconds":                  30,
		"reconcile_apply_workers":                           8,
		"reconcile_put_batch_max_entries":                   256,
		"reconcile_put_batch_max_bytes":                     1048576,
		"convergence_min_rate_ratio":                        0.40,
		"convergence_stall_seconds":                         90,
		"fallback_enabled":                                  true,
		"fallback_stall_seconds":                            90,
		"fallback_failure_threshold":                        2,
		"fallback_min_lag_entries":                          256,
		"fallback_cooldown_seconds":                         180,
		"fallback_max_per_hour":                             4,
		"checkpoint_artifact_enabled":                       true,
		"checkpoint_artifact_global_budget_bytes":           536870912,
		"checkpoint_artifact_per_relationship_budget_bytes": 134217728,
		"checkpoint_artifact_ttl_seconds":                   900,
		"checkpoint_artifact_segment_bytes":                 4194304,
		"dr_backpressure_enabled":                           false,
		"dr_backpressure_degraded_ratio":                    0.75,
		"dr_backpressure_critical_ratio":                    0.50,
		"dr_backpressure_min_lag_entries":                   512,
		"dr_backpressure_horizon_seconds":                   60,
		"dr_backpressure_degraded_min_qps":                  96,
		"dr_backpressure_critical_min_qps":                  48,
	}
}

func relaxedTuning() map[string]any {
	return map[string]any{
		"checkpoint_ttl_seconds":                            1800,
		"checkpoint_global_budget_bytes":                    1073741824,
		"checkpoint_per_relationship_budget_bytes":          268435456,
		"stream_buffer_max_entries":                         50000,
		"stream_buffer_max_bytes":                           268435456,
		"reconcile_max_rpc_bytes":                           134217728,
		"reconcile_max_wall_time_seconds":                   1800,
		"reconcile_max_inflight_tasks":                      16,
		"stream_batch_max_entries":                          256,
		"stream_batch_max_bytes":                            1048576,
		"stream_batch_max_wait_milliseconds":                10,
		"stream_journal_enabled":                            true,
		"stream_journal_max_bytes":                          4294967296,
		"stream_journal_segment_bytes":                      67108864,
		"stream_journal_retention_seconds":                  7200,
		"reconcile_apply_workers":                           16,
		"reconcile_put_batch_max_entries":                   512,
		"reconcile_put_batch_max_bytes":                     2097152,
		"convergence_min_rate_ratio":                        0.80,
		"convergence_stall_seconds":                         180,
		"fallback_enabled":                                  true,
		"fallback_stall_seconds":                            180,
		"fallback_failure_threshold":                        3,
		"fallback_min_lag_entries":                          1024,
		"fallback_cooldown_seconds":                         600,
		"fallback_max_per_hour":                             2,
		"checkpoint_artifact_enabled":                       true,
		"checkpoint_artifact_global_budget_bytes":           8589934592,
		"checkpoint_artifact_per_relationship_budget_bytes": 2147483648,
		"checkpoint_artifact_ttl_seconds":                   1800,
		"checkpoint_artifact_segment_bytes":                 67108864,
		"dr_backpressure_enabled":                           true,
		"dr_backpressure_degraded_ratio":                    0.80,
		"dr_backpressure_critical_ratio":                    0.50,
		"dr_backpressure_min_lag_entries":                   1024,
		"dr_backpressure_horizon_seconds":                   180,
		"dr_backpressure_degraded_min_qps":                  50,
		"dr_backpressure_critical_min_qps":                  10,
	}
}

func emptyDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func shellQuoteArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" {
			out = append(out, "''")
			continue
		}
		if strings.IndexFunc(arg, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) == -1 {
			out = append(out, arg)
			continue
		}
		out = append(out, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
	}
	return out
}
