package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/artifact"
	"github.com/openbao/openbao/scripts/dr-harness/internal/bao"
	"github.com/openbao/openbao/scripts/dr-harness/internal/topology"
)

type transportCARotationSummary struct {
	RunID                        string                      `json:"run_id"`
	PrimaryActiveInitial         string                      `json:"primary_active_initial"`
	PrimaryActiveAfterActivation string                      `json:"primary_active_after_activation_handoff"`
	PrimaryActiveFinal           string                      `json:"primary_active_final"`
	Secondary2ActiveInitial      string                      `json:"secondary2_active_initial"`
	Secondary2ActiveFinal        string                      `json:"secondary2_active_final"`
	InitialTrustBundle           *bao.TransportCARotation    `json:"initial_trust_bundle,omitempty"`
	Stage                        *bao.TransportCARotation    `json:"stage,omitempty"`
	AcceptStageSecondary1        *bao.TransportCARotation    `json:"accept_stage_secondary1,omitempty"`
	AcceptStageSecondary2        *bao.TransportCARotation    `json:"accept_stage_secondary2,omitempty"`
	Activate                     *bao.TransportCARotation    `json:"activate,omitempty"`
	AcceptActivateSecondary1     *bao.TransportCARotation    `json:"accept_activate_secondary1,omitempty"`
	AcceptActivateSecondary2     *bao.TransportCARotation    `json:"accept_activate_secondary2,omitempty"`
	Retire                       *bao.TransportCARotation    `json:"retire,omitempty"`
	AcceptRetireSecondary1       *bao.TransportCARotation    `json:"accept_retire_secondary1,omitempty"`
	AcceptRetireSecondary2       *bao.TransportCARotation    `json:"accept_retire_secondary2,omitempty"`
	StaleActivatedBundleRejected bool                        `json:"stale_activated_bundle_rejected"`
	StaleStageBundleRejected     bool                        `json:"stale_stage_bundle_rejected"`
	Secondary1CheckpointVerify   *bao.CheckpointVerification `json:"secondary1_checkpoint_verification,omitempty"`
	Secondary2CheckpointVerify   *bao.CheckpointVerification `json:"secondary2_checkpoint_verification,omitempty"`
	CompletedAt                  string                      `json:"completed_at"`
}

type transportCARotationLoadSummary struct {
	RunID                         string                      `json:"run_id"`
	StartedAt                     string                      `json:"started_at"`
	CompletedAt                   string                      `json:"completed_at"`
	DurationSeconds               int                         `json:"duration_seconds"`
	Concurrency                   int                         `json:"concurrency"`
	StepdownIntervalSeconds       int                         `json:"stepdown_interval_seconds"`
	StageAfterSeconds             int                         `json:"stage_after_seconds"`
	ActivateAfterSeconds          int                         `json:"activate_after_seconds"`
	RetireAfterSeconds            int                         `json:"retire_after_seconds"`
	PrimaryActiveBefore           string                      `json:"primary_active_before"`
	Secondary1ActiveBefore        string                      `json:"secondary1_active_before"`
	Secondary2ActiveBefore        string                      `json:"secondary2_active_before"`
	PrimaryActiveAfterWorkload    string                      `json:"primary_active_after_workload"`
	Secondary1ActiveAfterWorkload string                      `json:"secondary1_active_after_workload"`
	Secondary2ActiveAfterWorkload string                      `json:"secondary2_active_after_workload"`
	Lifecycle                     *transportCARotationSummary `json:"lifecycle"`
	WorkloadResult                json.RawMessage             `json:"workload_result,omitempty"`
}

type transportCAChainSummary struct {
	RunID                      string                      `json:"run_id"`
	StartedAt                  string                      `json:"started_at"`
	CompletedAt                string                      `json:"completed_at"`
	Rotations                  int                         `json:"rotations"`
	PrimaryActiveInitial       string                      `json:"primary_active_initial"`
	Secondary1ActiveInitial    string                      `json:"secondary1_active_initial"`
	Secondary2ActiveInitial    string                      `json:"secondary2_active_initial"`
	PrimaryActiveFinal         string                      `json:"primary_active_final"`
	Secondary1ActiveFinal      string                      `json:"secondary1_active_final"`
	Secondary2ActiveFinal      string                      `json:"secondary2_active_final"`
	Steps                      []transportCAChainStep      `json:"steps"`
	Secondary1CheckpointVerify *bao.CheckpointVerification `json:"secondary1_checkpoint_verification,omitempty"`
	Secondary2CheckpointVerify *bao.CheckpointVerification `json:"secondary2_checkpoint_verification,omitempty"`
}

type transportCAChainStep struct {
	Rotation                     int                      `json:"rotation"`
	ActiveBeforeKeyID            string                   `json:"active_before_key_id"`
	StagedKeyID                  string                   `json:"staged_key_id"`
	ActiveAfterKeyID             string                   `json:"active_after_key_id"`
	PreviousAfterActivateKeyID   string                   `json:"previous_after_activate_key_id"`
	PrimaryActiveAfterHandoff    string                   `json:"primary_active_after_handoff"`
	Secondary2ActiveAfterHandoff string                   `json:"secondary2_active_after_handoff"`
	StaleActivatedBundleRejected bool                     `json:"stale_activated_bundle_rejected"`
	StaleStageBundleRejected     bool                     `json:"stale_stage_bundle_rejected"`
	PriorStaleBundlesRejected    int                      `json:"prior_stale_bundles_rejected"`
	PriorStaleBundlesChecked     int                      `json:"prior_stale_bundles_checked"`
	Retired                      *bao.TransportCARotation `json:"retired,omitempty"`
}

type staleTransportCABundle struct {
	Label  string
	Bundle string
}

func RunTransportCARotationSmoke(ctx context.Context, cfg HAConfig) error {
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}

	run, err := artifact.New(cfg.ResultsDir, "transport-ca-rotation")
	if err != nil {
		return err
	}
	summary := &transportCARotationSummary{RunID: run.ID}
	run.Logf("run_id=%s", run.ID)
	run.Logf("started_at=%s", time.Now().UTC().Format(time.RFC3339))
	run.Logf("timeout_seconds=%d", int(cfg.Timeout.Seconds()))

	primaryInitial, err := topology.WaitActiveAddr(ctx, rt.primary, "primary initial", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Initial, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 initial", 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveInitial = primaryInitial
	summary.Secondary2ActiveInitial = secondary2Initial
	run.Logf("primary_active_initial=%s", primaryInitial)
	run.Logf("secondary2_active_initial=%s", secondary2Initial)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 initial", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 initial", cfg.Timeout); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "initial", rt)

	if err := rt.primary.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return err
	}
	if err := writeTransportCAMarker(ctx, rt, run, "initial", cfg.Timeout); err != nil {
		return err
	}

	initial, raw, err := rt.primary.DRPrimaryTransportCATrustBundle(ctx)
	if err != nil {
		_ = run.WriteFile("initial-trust-bundle-error.json", raw)
		return err
	}
	summary.InitialTrustBundle = initial
	_ = run.WriteFile("initial-trust-bundle.json", raw)
	if initial.ActiveKeyID == "" || initial.TrustBundle == "" {
		return fmt.Errorf("initial transport CA trust bundle missing active key or bundle")
	}

	stage, raw, err := rt.primary.StageDRPrimaryTransportCA(ctx)
	if err != nil {
		_ = run.WriteFile("stage-error.json", raw)
		return err
	}
	summary.Stage = stage
	_ = run.WriteFile("stage.json", raw)
	if stage.ActiveKeyID != initial.ActiveKeyID {
		return fmt.Errorf("stage changed active CA before activation: initial=%s stage=%s", initial.ActiveKeyID, stage.ActiveKeyID)
	}
	run.Logf("staged_transport_ca operation_id=%s active=%s staged=%s", stage.OperationID, stage.ActiveKeyID, stage.StagedKeyID)

	acceptStage1, acceptStage2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "stage", stage.TrustBundle)
	if err != nil {
		return err
	}
	summary.AcceptStageSecondary1 = acceptStage1
	summary.AcceptStageSecondary2 = acceptStage2
	if acceptStage1.StagedKeyID != stage.StagedKeyID || acceptStage2.StagedKeyID != stage.StagedKeyID {
		return fmt.Errorf("stage accept did not install staged key on both secondaries")
	}
	if err := writeTransportCAMarker(ctx, rt, run, "stage-accepted", cfg.Timeout); err != nil {
		return err
	}

	activated, raw, err := rt.primary.ActivateDRPrimaryTransportCA(ctx, stage.OperationID)
	if err != nil {
		_ = run.WriteFile("activate-error.json", raw)
		return err
	}
	summary.Activate = activated
	_ = run.WriteFile("activate.json", raw)
	if activated.ActiveKeyID != stage.StagedKeyID || activated.PreviousKeyID != stage.ActiveKeyID {
		return fmt.Errorf("activation key transition mismatch: stage active=%s staged=%s activated active=%s previous=%s",
			stage.ActiveKeyID, stage.StagedKeyID, activated.ActiveKeyID, activated.PreviousKeyID)
	}
	run.Logf("activated_transport_ca active=%s previous=%s", activated.ActiveKeyID, activated.PreviousKeyID)

	primaryAfterActivation, err := forceTransportCAHandoff(ctx, run, rt.primary, "primary-after-activation", cfg.Timeout)
	if err != nil {
		return err
	}
	summary.PrimaryActiveAfterActivation = primaryAfterActivation
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 after activation handoff", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 after activation handoff", cfg.Timeout); err != nil {
		return err
	}
	if err := writeTransportCAMarker(ctx, rt, run, "activated-handoff", cfg.Timeout); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "after-activation-handoff", rt)

	acceptActivate1, acceptActivate2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "activate", activated.TrustBundle)
	if err != nil {
		return err
	}
	summary.AcceptActivateSecondary1 = acceptActivate1
	summary.AcceptActivateSecondary2 = acceptActivate2
	if acceptActivate1.ActiveKeyID != activated.ActiveKeyID || acceptActivate2.ActiveKeyID != activated.ActiveKeyID {
		return fmt.Errorf("activated trust bundle did not advance active key on both secondaries")
	}
	if err := writeTransportCAMarker(ctx, rt, run, "activate-accepted", cfg.Timeout); err != nil {
		return err
	}

	retired, raw, err := rt.primary.RetirePreviousDRPrimaryTransportCA(ctx)
	if err != nil {
		_ = run.WriteFile("retire-error.json", raw)
		return err
	}
	summary.Retire = retired
	_ = run.WriteFile("retire.json", raw)
	if retired.ActiveKeyID != activated.ActiveKeyID {
		return fmt.Errorf("retire changed active CA unexpectedly: activated=%s retired=%s", activated.ActiveKeyID, retired.ActiveKeyID)
	}
	run.Logf("retired_previous_transport_ca active=%s previous=%s", retired.ActiveKeyID, retired.PreviousKeyID)

	acceptRetire1, acceptRetire2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "retire", retired.TrustBundle)
	if err != nil {
		return err
	}
	summary.AcceptRetireSecondary1 = acceptRetire1
	summary.AcceptRetireSecondary2 = acceptRetire2
	if acceptRetire1.PreviousKeyID != "" || acceptRetire2.PreviousKeyID != "" {
		return fmt.Errorf("retired trust bundle still exposed previous key on secondary accept")
	}

	if _, raw, err := rt.secondary1DRRoot.AcceptDRSecondaryTransportCA(ctx, activated.TrustBundle); err == nil {
		_ = run.WriteFile("stale-activated-accept-unexpected.json", raw)
		return fmt.Errorf("stale activated trust bundle was accepted after previous CA retirement")
	} else {
		summary.StaleActivatedBundleRejected = true
		_ = run.WriteFile("stale-activated-accept.err", []byte(err.Error()+"\n"))
	}
	if _, raw, err := rt.secondary1DRRoot.AcceptDRSecondaryTransportCA(ctx, stage.TrustBundle); err == nil {
		_ = run.WriteFile("stale-stage-accept-unexpected.json", raw)
		return fmt.Errorf("stale staged trust bundle was accepted after previous CA retirement")
	} else {
		summary.StaleStageBundleRejected = true
		_ = run.WriteFile("stale-stage-accept.err", []byte(err.Error()+"\n"))
	}

	primaryFinal, err := forceTransportCAHandoff(ctx, run, rt.primary, "primary-after-retire", cfg.Timeout)
	if err != nil {
		return err
	}
	summary.PrimaryActiveFinal = primaryFinal
	secondary2Final, err := forceTransportCAHandoff(ctx, run, rt.secondary2DRRoot, "secondary2-after-retire", cfg.Timeout)
	if err != nil {
		return err
	}
	summary.Secondary2ActiveFinal = secondary2Final
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 final", cfg.Timeout); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 final", cfg.Timeout); err != nil {
		return err
	}
	if err := writeTransportCAMarker(ctx, rt, run, "retire-accepted-final-handoff", cfg.Timeout); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "final", rt)

	verify1, verifyRaw1, err := waitVerifyCheckpoint(ctx, rt.secondary1, "secondary1 transport CA rotation", cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("verify-secondary1-error.json", verifyRaw1)
		return err
	}
	verify2, verifyRaw2, err := waitVerifyCheckpoint(ctx, rt.secondary2, "secondary2 transport CA rotation", cfg.Timeout)
	if err != nil {
		_ = run.WriteFile("verify-secondary2-error.json", verifyRaw2)
		return err
	}
	summary.Secondary1CheckpointVerify = verify1
	summary.Secondary2CheckpointVerify = verify2
	_ = run.WriteFile("verify-secondary1.json", verifyRaw1)
	_ = run.WriteFile("verify-secondary2.json", verifyRaw2)

	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	if err := run.WriteJSON("result.json", summary); err != nil {
		return err
	}
	run.Logf("completed_at=%s", summary.CompletedAt)
	fmt.Println("DR transport CA rotation HA smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func RunTransportCARotationLoadSmoke(ctx context.Context, cfg TransportCALoadConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}

	run, err := artifact.New(cfg.ResultsDir, "transport-ca-rotation-load")
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	lifecycle := &transportCARotationSummary{RunID: run.ID}
	summary := &transportCARotationLoadSummary{
		RunID:                   run.ID,
		StartedAt:               startedAt,
		DurationSeconds:         int(cfg.Duration.Seconds()),
		Concurrency:             cfg.Concurrency,
		StepdownIntervalSeconds: int(cfg.StepdownInterval.Seconds()),
		StageAfterSeconds:       int(cfg.StageAfter.Seconds()),
		ActivateAfterSeconds:    int(cfg.ActivateAfter.Seconds()),
		RetireAfterSeconds:      int(cfg.RetireAfter.Seconds()),
		Lifecycle:               lifecycle,
	}
	run.Logf("run_id=%s", run.ID)
	run.Logf("run_dir=%s", run.Dir)
	run.Logf("started_at=%s", startedAt)
	run.Logf("duration_seconds=%d", summary.DurationSeconds)
	run.Logf("concurrency=%d", cfg.Concurrency)
	run.Logf("stepdown_interval_seconds=%d", summary.StepdownIntervalSeconds)
	run.Logf("stage_after_seconds=%d", summary.StageAfterSeconds)
	run.Logf("activate_after_seconds=%d", summary.ActivateAfterSeconds)
	run.Logf("retire_after_seconds=%d", summary.RetireAfterSeconds)

	tuningProfile := strings.TrimSpace(cfg.TuningProfile)
	if tuningProfile == "" {
		tuningProfile = "constrained"
	}
	if tuningProfile != "none" {
		run.Logf("ha_transport_ca_load_tuning_profile=%s", tuningProfile)
		if err := applyPrimaryTuningProfile(ctx, rt.primary, tuningProfile, run); err != nil {
			return err
		}
	}

	primaryBefore, err := topology.WaitActiveAddr(ctx, rt.primary, "primary transport CA load before", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Before, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 transport CA load before", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Before, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 transport CA load before", 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveBefore = primaryBefore
	summary.Secondary1ActiveBefore = secondary1Before
	summary.Secondary2ActiveBefore = secondary2Before
	lifecycle.PrimaryActiveInitial = primaryBefore
	lifecycle.Secondary2ActiveInitial = secondary2Before
	run.Logf("primary_active_before=%s", primaryBefore)
	run.Logf("secondary1_active_before=%s", secondary1Before)
	run.Logf("secondary2_active_before=%s", secondary2Before)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 transport CA load before", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 transport CA load before", cfg.MaxWait); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "before-load", rt)

	stressCmd, harnessOut, harnessErr, err := startTransportCALoadDRStress(ctx, cfg, run, rt.layout)
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

	if err := sleepContext(ctx, cfg.StageAfter); err != nil {
		return err
	}
	initial, raw, err := rt.primary.DRPrimaryTransportCATrustBundle(ctx)
	if err != nil {
		_ = run.WriteFile("initial-trust-bundle-error.json", raw)
		return err
	}
	lifecycle.InitialTrustBundle = initial
	_ = run.WriteFile("initial-trust-bundle.json", raw)
	if initial.ActiveKeyID == "" || initial.TrustBundle == "" {
		return fmt.Errorf("initial transport CA trust bundle missing active key or bundle")
	}
	stage, raw, err := rt.primary.StageDRPrimaryTransportCA(ctx)
	if err != nil {
		_ = run.WriteFile("stage-error.json", raw)
		return err
	}
	lifecycle.Stage = stage
	_ = run.WriteFile("stage.json", raw)
	if stage.ActiveKeyID != initial.ActiveKeyID {
		return fmt.Errorf("stage changed active CA before activation: initial=%s stage=%s", initial.ActiveKeyID, stage.ActiveKeyID)
	}
	run.Logf("staged_transport_ca operation_id=%s active=%s staged=%s", stage.OperationID, stage.ActiveKeyID, stage.StagedKeyID)
	acceptStage1, acceptStage2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "stage", stage.TrustBundle)
	if err != nil {
		return err
	}
	lifecycle.AcceptStageSecondary1 = acceptStage1
	lifecycle.AcceptStageSecondary2 = acceptStage2
	if acceptStage1.StagedKeyID != stage.StagedKeyID || acceptStage2.StagedKeyID != stage.StagedKeyID {
		return fmt.Errorf("stage accept did not install staged key on both secondaries")
	}
	if err := writeTransportCAMarker(ctx, rt, run, "load-stage-accepted", cfg.MaxWait); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "after-stage", rt)

	if err := sleepContext(ctx, cfg.ActivateAfter-cfg.StageAfter); err != nil {
		return err
	}
	activated, raw, err := rt.primary.ActivateDRPrimaryTransportCA(ctx, stage.OperationID)
	if err != nil {
		_ = run.WriteFile("activate-error.json", raw)
		return err
	}
	lifecycle.Activate = activated
	_ = run.WriteFile("activate.json", raw)
	if activated.ActiveKeyID != stage.StagedKeyID || activated.PreviousKeyID != stage.ActiveKeyID {
		return fmt.Errorf("activation key transition mismatch: stage active=%s staged=%s activated active=%s previous=%s",
			stage.ActiveKeyID, stage.StagedKeyID, activated.ActiveKeyID, activated.PreviousKeyID)
	}
	run.Logf("activated_transport_ca active=%s previous=%s", activated.ActiveKeyID, activated.PreviousKeyID)
	primaryAfterActivation, err := requestTransportCAHandoff(ctx, run, rt.primary, "primary-load-after-activation", cfg.MaxWait)
	if err != nil {
		return err
	}
	lifecycle.PrimaryActiveAfterActivation = primaryAfterActivation
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 transport CA load after activation", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 transport CA load after activation", cfg.MaxWait); err != nil {
		return err
	}
	acceptActivate1, acceptActivate2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "activate", activated.TrustBundle)
	if err != nil {
		return err
	}
	lifecycle.AcceptActivateSecondary1 = acceptActivate1
	lifecycle.AcceptActivateSecondary2 = acceptActivate2
	if acceptActivate1.ActiveKeyID != activated.ActiveKeyID || acceptActivate2.ActiveKeyID != activated.ActiveKeyID {
		return fmt.Errorf("activated trust bundle did not advance active key on both secondaries")
	}
	if err := writeTransportCAMarker(ctx, rt, run, "load-activate-accepted", cfg.MaxWait); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "after-activate", rt)

	if err := sleepContext(ctx, cfg.RetireAfter-cfg.ActivateAfter); err != nil {
		return err
	}
	retired, raw, err := rt.primary.RetirePreviousDRPrimaryTransportCA(ctx)
	if err != nil {
		_ = run.WriteFile("retire-error.json", raw)
		return err
	}
	lifecycle.Retire = retired
	_ = run.WriteFile("retire.json", raw)
	if retired.ActiveKeyID != activated.ActiveKeyID {
		return fmt.Errorf("retire changed active CA unexpectedly: activated=%s retired=%s", activated.ActiveKeyID, retired.ActiveKeyID)
	}
	run.Logf("retired_previous_transport_ca active=%s previous=%s", retired.ActiveKeyID, retired.PreviousKeyID)
	acceptRetire1, acceptRetire2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, "retire", retired.TrustBundle)
	if err != nil {
		return err
	}
	lifecycle.AcceptRetireSecondary1 = acceptRetire1
	lifecycle.AcceptRetireSecondary2 = acceptRetire2
	if acceptRetire1.PreviousKeyID != "" || acceptRetire2.PreviousKeyID != "" {
		return fmt.Errorf("retired trust bundle still exposed previous key on secondary accept")
	}
	if _, raw, err := rt.secondary1DRRoot.AcceptDRSecondaryTransportCA(ctx, activated.TrustBundle); err == nil {
		_ = run.WriteFile("stale-activated-accept-unexpected.json", raw)
		return fmt.Errorf("stale activated trust bundle was accepted after previous CA retirement")
	} else {
		lifecycle.StaleActivatedBundleRejected = true
		_ = run.WriteFile("stale-activated-accept.err", []byte(err.Error()+"\n"))
	}
	if _, raw, err := rt.secondary1DRRoot.AcceptDRSecondaryTransportCA(ctx, stage.TrustBundle); err == nil {
		_ = run.WriteFile("stale-stage-accept-unexpected.json", raw)
		return fmt.Errorf("stale staged trust bundle was accepted after previous CA retirement")
	} else {
		lifecycle.StaleStageBundleRejected = true
		_ = run.WriteFile("stale-stage-accept.err", []byte(err.Error()+"\n"))
	}
	primaryAfterRetire, err := requestTransportCAHandoff(ctx, run, rt.primary, "primary-load-after-retire", cfg.MaxWait)
	if err != nil {
		return err
	}
	lifecycle.PrimaryActiveFinal = primaryAfterRetire
	secondary2AfterRetire, err := requestTransportCAHandoff(ctx, run, rt.secondary2DRRoot, "secondary2-load-after-retire", cfg.MaxWait)
	if err != nil {
		return err
	}
	lifecycle.Secondary2ActiveFinal = secondary2AfterRetire
	if err := writeTransportCAMarker(ctx, rt, run, "load-retire-accepted-final-handoff", cfg.MaxWait); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "after-retire", rt)

	run.Logf("waiting_for_stress_pid=%d", stressCmd.Process.Pid)
	stressErr := stressCmd.Wait()
	stressRunning = false
	closeFiles(harnessOut, harnessErr)
	if stressErr != nil {
		return fmt.Errorf("dr-stress exited non-zero; artifacts preserved in %s: %w", run.Dir, stressErr)
	}
	run.Logf("stress_rc=0")

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 transport CA load final", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 transport CA load final", cfg.MaxWait); err != nil {
		return err
	}
	primaryFinal, err := topology.WaitActiveAddr(ctx, rt.primary, "primary transport CA load final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Final, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 transport CA load final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Final, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 transport CA load final", 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveAfterWorkload = primaryFinal
	summary.Secondary1ActiveAfterWorkload = secondary1Final
	summary.Secondary2ActiveAfterWorkload = secondary2Final
	captureTransportCAStatus(ctx, run, "final", rt)

	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "primary", primaryFinal, rt.layout.Primary.Token, "api"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary1", secondary1Final, rt.layout.Secondary1.Token, "checkpoint"); err != nil {
		return err
	}
	if err := runDRStressVerify(ctx, cfg.DRStressBin, run.Dir, "secondary2", secondary2Final, rt.layout.Secondary2.Token, "checkpoint"); err != nil {
		return err
	}
	if workloadRaw, err := os.ReadFile(run.Path("result.json")); err == nil {
		summary.WorkloadResult = json.RawMessage(workloadRaw)
	}
	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	lifecycle.CompletedAt = summary.CompletedAt
	if err := run.WriteJSON("scenario-result.json", summary); err != nil {
		return err
	}
	run.Logf("completed_at=%s", summary.CompletedAt)
	fmt.Println("DR transport CA rotation HA load smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func RunTransportCARotationChainSmoke(ctx context.Context, cfg TransportCAChainConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	rt, err := prepareHARuntime(ctx, cfg.RootDir, cfg.Topology, cfg.EnvFile, cfg.Timeout, cfg.HTTPTimeout, cfg.Reset, cfg.Build)
	if err != nil {
		return err
	}

	run, err := artifact.New(cfg.ResultsDir, "transport-ca-rotation-chain")
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	summary := &transportCAChainSummary{
		RunID:     run.ID,
		StartedAt: startedAt,
		Rotations: cfg.Rotations,
	}
	run.Logf("run_id=%s", run.ID)
	run.Logf("run_dir=%s", run.Dir)
	run.Logf("started_at=%s", startedAt)
	run.Logf("rotations=%d", cfg.Rotations)

	primaryInitial, err := topology.WaitActiveAddr(ctx, rt.primary, "primary transport CA chain initial", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Initial, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 transport CA chain initial", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Initial, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 transport CA chain initial", 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveInitial = primaryInitial
	summary.Secondary1ActiveInitial = secondary1Initial
	summary.Secondary2ActiveInitial = secondary2Initial
	run.Logf("primary_active_initial=%s", primaryInitial)
	run.Logf("secondary1_active_initial=%s", secondary1Initial)
	run.Logf("secondary2_active_initial=%s", secondary2Initial)

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 transport CA chain initial", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 transport CA chain initial", cfg.MaxWait); err != nil {
		return err
	}
	captureTransportCAStatus(ctx, run, "chain-initial", rt)

	if err := rt.primary.EnsureKVV2Mount(ctx, "kv"); err != nil {
		return err
	}

	var priorStale []staleTransportCABundle
	for rotation := 1; rotation <= cfg.Rotations; rotation++ {
		label := fmt.Sprintf("rotation-%02d", rotation)
		run.Logf("rotation_start=%s", label)

		current, raw, err := rt.primary.DRPrimaryTransportCATrustBundle(ctx)
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-current-error.json", label), raw)
			return err
		}
		_ = run.WriteFile(fmt.Sprintf("%s-current.json", label), raw)
		if current.ActiveKeyID == "" || current.TrustBundle == "" {
			return fmt.Errorf("%s current transport CA bundle missing active key or bundle", label)
		}

		stage, raw, err := rt.primary.StageDRPrimaryTransportCA(ctx)
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-stage-error.json", label), raw)
			return err
		}
		_ = run.WriteFile(fmt.Sprintf("%s-stage.json", label), raw)
		if stage.ActiveKeyID != current.ActiveKeyID {
			return fmt.Errorf("%s stage changed active CA before activation: current=%s stage=%s", label, current.ActiveKeyID, stage.ActiveKeyID)
		}
		run.Logf("staged_transport_ca rotation=%d operation_id=%s active=%s staged=%s", rotation, stage.OperationID, stage.ActiveKeyID, stage.StagedKeyID)
		acceptStage1, acceptStage2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, label+"-stage", stage.TrustBundle)
		if err != nil {
			return err
		}
		if acceptStage1.StagedKeyID != stage.StagedKeyID || acceptStage2.StagedKeyID != stage.StagedKeyID {
			return fmt.Errorf("%s stage accept did not install staged key on both secondaries", label)
		}

		activated, raw, err := rt.primary.ActivateDRPrimaryTransportCA(ctx, stage.OperationID)
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-activate-error.json", label), raw)
			return err
		}
		_ = run.WriteFile(fmt.Sprintf("%s-activate.json", label), raw)
		if activated.ActiveKeyID != stage.StagedKeyID || activated.PreviousKeyID != stage.ActiveKeyID {
			return fmt.Errorf("%s activation key transition mismatch: stage active=%s staged=%s activated active=%s previous=%s",
				label, stage.ActiveKeyID, stage.StagedKeyID, activated.ActiveKeyID, activated.PreviousKeyID)
		}
		run.Logf("activated_transport_ca rotation=%d active=%s previous=%s", rotation, activated.ActiveKeyID, activated.PreviousKeyID)
		primaryAfterActivation, err := requestTransportCAHandoff(ctx, run, rt.primary, label+"-primary-after-activation", cfg.MaxWait)
		if err != nil {
			return err
		}
		if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 "+label+" after activation", cfg.MaxWait); err != nil {
			return err
		}
		if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 "+label+" after activation", cfg.MaxWait); err != nil {
			return err
		}
		acceptActivate1, acceptActivate2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, label+"-activate", activated.TrustBundle)
		if err != nil {
			return err
		}
		if acceptActivate1.ActiveKeyID != activated.ActiveKeyID || acceptActivate2.ActiveKeyID != activated.ActiveKeyID {
			return fmt.Errorf("%s activated trust bundle did not advance active key on both secondaries", label)
		}

		retired, raw, err := rt.primary.RetirePreviousDRPrimaryTransportCA(ctx)
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-retire-error.json", label), raw)
			return err
		}
		_ = run.WriteFile(fmt.Sprintf("%s-retire.json", label), raw)
		if retired.ActiveKeyID != activated.ActiveKeyID {
			return fmt.Errorf("%s retire changed active CA unexpectedly: activated=%s retired=%s", label, activated.ActiveKeyID, retired.ActiveKeyID)
		}
		run.Logf("retired_previous_transport_ca rotation=%d active=%s previous=%s", rotation, retired.ActiveKeyID, retired.PreviousKeyID)
		acceptRetire1, acceptRetire2, err := acceptTransportCABundleOnSecondaries(ctx, run, rt, label+"-retire", retired.TrustBundle)
		if err != nil {
			return err
		}
		if acceptRetire1.PreviousKeyID != "" || acceptRetire2.PreviousKeyID != "" {
			return fmt.Errorf("%s retired trust bundle still exposed previous key on secondary accept", label)
		}

		step := transportCAChainStep{
			Rotation:                   rotation,
			ActiveBeforeKeyID:          current.ActiveKeyID,
			StagedKeyID:                stage.StagedKeyID,
			ActiveAfterKeyID:           retired.ActiveKeyID,
			PreviousAfterActivateKeyID: activated.PreviousKeyID,
			PrimaryActiveAfterHandoff:  primaryAfterActivation,
			PriorStaleBundlesChecked:   len(priorStale),
			Retired:                    retired,
		}
		if err := rejectTransportCABundleOnSecondaries(ctx, run, rt, label+"-stale-activated", activated.TrustBundle); err != nil {
			return err
		}
		step.StaleActivatedBundleRejected = true
		if err := rejectTransportCABundleOnSecondaries(ctx, run, rt, label+"-stale-stage", stage.TrustBundle); err != nil {
			return err
		}
		step.StaleStageBundleRejected = true
		for _, stale := range priorStale {
			if err := rejectTransportCABundleOnSecondaries(ctx, run, rt, label+"-prior-"+stale.Label, stale.Bundle); err != nil {
				return err
			}
			step.PriorStaleBundlesRejected++
		}

		secondary2AfterRetire, err := requestTransportCAHandoff(ctx, run, rt.secondary2DRRoot, label+"-secondary2-after-retire", cfg.MaxWait)
		if err != nil {
			return err
		}
		step.Secondary2ActiveAfterHandoff = secondary2AfterRetire
		if err := writeTransportCAMarker(ctx, rt, run, label+"-retired", cfg.MaxWait); err != nil {
			return err
		}
		captureTransportCAStatus(ctx, run, label+"-after-retire", rt)

		summary.Steps = append(summary.Steps, step)
		priorStale = append(
			priorStale,
			staleTransportCABundle{Label: label + "-stage", Bundle: stage.TrustBundle},
			staleTransportCABundle{Label: label + "-activated", Bundle: activated.TrustBundle},
			staleTransportCABundle{Label: label + "-retired", Bundle: retired.TrustBundle},
		)
		run.Logf("rotation_complete=%s prior_stale_checked=%d", label, step.PriorStaleBundlesChecked)
	}

	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary1DRRoot, "secondary1 transport CA chain final", cfg.MaxWait); err != nil {
		return err
	}
	if _, err := topology.WaitSecondaryReady(ctx, rt.secondary2DRRoot, "secondary2 transport CA chain final", cfg.MaxWait); err != nil {
		return err
	}
	primaryFinal, err := topology.WaitActiveAddr(ctx, rt.primary, "primary transport CA chain final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary1Final, err := topology.WaitActiveAddr(ctx, rt.secondary1DRRoot, "secondary1 transport CA chain final", 120*time.Second)
	if err != nil {
		return err
	}
	secondary2Final, err := topology.WaitActiveAddr(ctx, rt.secondary2DRRoot, "secondary2 transport CA chain final", 120*time.Second)
	if err != nil {
		return err
	}
	summary.PrimaryActiveFinal = primaryFinal
	summary.Secondary1ActiveFinal = secondary1Final
	summary.Secondary2ActiveFinal = secondary2Final
	captureTransportCAStatus(ctx, run, "chain-final", rt)

	verify1, verifyRaw1, err := waitVerifyCheckpoint(ctx, rt.secondary1, "secondary1 transport CA rotation chain", cfg.MaxWait)
	if err != nil {
		_ = run.WriteFile("verify-secondary1-error.json", verifyRaw1)
		return err
	}
	verify2, verifyRaw2, err := waitVerifyCheckpoint(ctx, rt.secondary2, "secondary2 transport CA rotation chain", cfg.MaxWait)
	if err != nil {
		_ = run.WriteFile("verify-secondary2-error.json", verifyRaw2)
		return err
	}
	summary.Secondary1CheckpointVerify = verify1
	summary.Secondary2CheckpointVerify = verify2
	_ = run.WriteFile("verify-secondary1.json", verifyRaw1)
	_ = run.WriteFile("verify-secondary2.json", verifyRaw2)

	summary.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	if err := run.WriteJSON("result.json", summary); err != nil {
		return err
	}
	run.Logf("completed_at=%s", summary.CompletedAt)
	fmt.Println("DR transport CA rotation chain HA smoke passed.")
	fmt.Printf("Run: %s\n", run.Dir)
	return nil
}

func acceptTransportCABundleOnSecondaries(ctx context.Context, run *artifact.Run, rt *harnessRuntime, phase, bundle string) (*bao.TransportCARotation, *bao.TransportCARotation, error) {
	first, raw, err := rt.secondary1DRRoot.AcceptDRSecondaryTransportCA(ctx, bundle)
	if err != nil {
		_ = run.WriteFile(fmt.Sprintf("accept-%s-secondary1-error.json", phase), raw)
		return nil, nil, fmt.Errorf("accept %s transport CA bundle on secondary1: %w", phase, err)
	}
	_ = run.WriteFile(fmt.Sprintf("accept-%s-secondary1.json", phase), raw)

	second, raw, err := rt.secondary2DRRoot.AcceptDRSecondaryTransportCA(ctx, bundle)
	if err != nil {
		_ = run.WriteFile(fmt.Sprintf("accept-%s-secondary2-error.json", phase), raw)
		return nil, nil, fmt.Errorf("accept %s transport CA bundle on secondary2: %w", phase, err)
	}
	_ = run.WriteFile(fmt.Sprintf("accept-%s-secondary2.json", phase), raw)
	return first, second, nil
}

func rejectTransportCABundleOnSecondaries(ctx context.Context, run *artifact.Run, rt *harnessRuntime, label, bundle string) error {
	checks := []struct {
		name   string
		client *bao.Client
	}{
		{name: "secondary1", client: rt.secondary1DRRoot},
		{name: "secondary2", client: rt.secondary2DRRoot},
	}
	for _, check := range checks {
		_, raw, err := check.client.AcceptDRSecondaryTransportCA(ctx, bundle)
		if err == nil {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-unexpected.json", label, check.name), raw)
			return fmt.Errorf("%s accepted stale transport CA bundle %s", check.name, label)
		}
		_ = run.WriteFile(fmt.Sprintf("%s-%s.err", label, check.name), []byte(err.Error()+"\n"))
	}
	return nil
}

func forceTransportCAHandoff(ctx context.Context, run *artifact.Run, client *bao.Client, label string, timeout time.Duration) (string, error) {
	before, err := topology.WaitActiveAddr(ctx, client, label+" before", 120*time.Second)
	if err != nil {
		return "", err
	}
	run.Logf("forcing_handoff label=%s before=%s", label, before)
	if err := client.StepDown(ctx); err != nil {
		_ = run.WriteFile(fmt.Sprintf("stepdown-%s.err", label), []byte(err.Error()+"\n"))
	}
	after, err := topology.WaitActiveChange(ctx, client, label+" after", before, timeout)
	if err != nil {
		return "", err
	}
	run.Logf("handoff_complete label=%s after=%s", label, after)
	return after, nil
}

func requestTransportCAHandoff(ctx context.Context, run *artifact.Run, client *bao.Client, label string, timeout time.Duration) (string, error) {
	before, err := topology.WaitActiveAddr(ctx, client, label+" before", 120*time.Second)
	if err != nil {
		return "", err
	}
	run.Logf("requesting_handoff label=%s before=%s", label, before)
	if err := client.StepDown(ctx); err != nil {
		_ = run.WriteFile(fmt.Sprintf("stepdown-%s.err", label), []byte(err.Error()+"\n"))
	}
	after, err := topology.WaitActiveAddr(ctx, client, label+" after", timeout)
	if err != nil {
		return "", err
	}
	run.Logf("handoff_request_complete label=%s after=%s changed=%t", label, after, after != before)
	return after, nil
}

func writeTransportCAMarker(ctx context.Context, rt *harnessRuntime, run *artifact.Run, phase string, timeout time.Duration) error {
	key := fmt.Sprintf("%s/%s", run.ID, phase)
	if err := putKVEventually(ctx, rt.primary, "kv", key, map[string]any{
		"phase":  phase,
		"run_id": run.ID,
		"ts":     time.Now().UTC().Format(time.RFC3339),
	}, timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary1DRRoot, "secondary1 "+phase, key, timeout); err != nil {
		return err
	}
	if err := waitKVPresent(ctx, rt.secondary2DRRoot, "secondary2 "+phase, key, timeout); err != nil {
		return err
	}
	return nil
}

func (cfg TransportCALoadConfig) validate() error {
	if cfg.Topology != "ha" {
		return fmt.Errorf("transport-ca-rotation-load-smoke requires --topology ha")
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
	if cfg.Duration <= 0 || cfg.Concurrency <= 0 || cfg.StageAfter <= 0 || cfg.ActivateAfter <= 0 || cfg.RetireAfter <= 0 {
		return fmt.Errorf("duration, concurrency, stage-after, activate-after, and retire-after must be positive")
	}
	if cfg.StepdownInterval < 0 || cfg.ProgressInterval <= 0 || cfg.MonitorInterval <= 0 || cfg.MaxWait <= 0 {
		return fmt.Errorf("stepdown-interval must be >= 0 and progress/monitor/max-wait must be positive")
	}
	if cfg.StageAfter >= cfg.ActivateAfter || cfg.ActivateAfter >= cfg.RetireAfter || cfg.RetireAfter >= cfg.Duration {
		return fmt.Errorf("stage-after must be < activate-after < retire-after < duration")
	}
	return nil
}

func (cfg TransportCAChainConfig) validate() error {
	if cfg.Topology != "ha" {
		return fmt.Errorf("transport-ca-rotation-chain-smoke requires --topology ha")
	}
	if cfg.ResultsDir == "" {
		return fmt.Errorf("--results-dir is required")
	}
	if cfg.Rotations <= 0 {
		return fmt.Errorf("--rotations must be positive")
	}
	if cfg.MaxWait <= 0 {
		return fmt.Errorf("--max-wait-seconds must be positive")
	}
	return nil
}

func startTransportCALoadDRStress(ctx context.Context, cfg TransportCALoadConfig, run *artifact.Run, layout topology.Layout) (*exec.Cmd, *os.File, *os.File, error) {
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
		"-test-class", "security_lifecycle",
		"-topology-label", "ha-primary-plus-two-secondaries",
		"-disruption-profile", fmt.Sprintf("transport_ca_rotation_load_stepdown_%ds_%s", int(cfg.StepdownInterval.Seconds()), emptyDefault(cfg.TuningProfile, "default")),
		"-workload-profile", "mixed_read_write_status",
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

func captureTransportCAStatus(ctx context.Context, run *artifact.Run, label string, rt *harnessRuntime) {
	capture := func(name string, client *bao.Client) {
		_, raw, err := client.DRStatus(ctx)
		if len(raw) > 0 {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-status.json", label, name), raw)
		}
		if err != nil {
			_ = run.WriteFile(fmt.Sprintf("%s-%s-status.err", label, name), []byte(err.Error()+"\n"))
		}
	}
	capture("primary", rt.primary)
	capture("secondary1", rt.secondary1DRRoot)
	capture("secondary2", rt.secondary2DRRoot)
}
