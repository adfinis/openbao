package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/scenario"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "preseed-smoke":
		if err := runPreSeedSmoke(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "smoke":
		if err := runMixedLoadSmoke(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "quiescent-reconnect-smoke":
		if err := runQuiescentReconnect(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "accumulator-cold-restart-smoke":
		if err := runAccumulatorColdRestart(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "secondary-outage-smoke":
		if err := runSecondaryOutage(ctx, os.Args[2:], false, false, "secondary-outage"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "secondary-outage-reconcile-smoke":
		if err := runSecondaryOutage(ctx, os.Args[2:], true, false, "secondary-outage-reconcile"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "indexed-repair-smoke":
		if err := runSecondaryOutage(ctx, os.Args[2:], true, false, "indexed-repair"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "reconcile-budget-smoke":
		if err := runSecondaryOutage(ctx, os.Args[2:], true, true, "reconcile-budget"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "tuning-load-smoke":
		if err := runTuningLoadSmoke(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "composite-lifecycle-soak":
		if err := runCompositeLifecycleSoak(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  dr-harness smoke [options] [dr-stress run flags...]
  dr-harness preseed-smoke [options]
  dr-harness quiescent-reconnect-smoke [options]
  dr-harness accumulator-cold-restart-smoke [options]
  dr-harness secondary-outage-smoke [options]
  dr-harness secondary-outage-reconcile-smoke [options]
  dr-harness indexed-repair-smoke [options]
  dr-harness reconcile-budget-smoke [options]
  dr-harness tuning-load-smoke [options]
  dr-harness composite-lifecycle-soak [options]

Commands:
  smoke                             Run the mixed-load dr-stress smoke workload
  preseed-smoke                     Run the segmented DR pre-seed smoke scenario
  quiescent-reconnect-smoke         Verify HA active handoff avoids scanned reconciliation
  accumulator-cold-restart-smoke    Verify persisted accumulator restore after secondary restart
  secondary-outage-smoke            Verify within-horizon replay after secondary outage
  secondary-outage-reconcile-smoke  Verify out-of-horizon checkpoint reconciliation
  indexed-repair-smoke              Verify indexed repair during out-of-horizon reconciliation
  reconcile-budget-smoke            Verify clustered reconcile budget exhaustion fails closed
  tuning-load-smoke                 Verify dynamic tuning updates under HA mixed load
  composite-lifecycle-soak          Run 100k pre-seed, HA load, secondary outage lifecycle soak

Run "dr-harness <command> --help" for command-specific flags.`)
}

func runMixedLoadSmoke(ctx context.Context, args []string) error {
	cfg := scenario.MixedLoadConfig{
		RootDir:     defaultRootDir(),
		Topology:    "single",
		HTTPTimeout: 600 * time.Second,
	}
	cfg.EnvFile = filepath.Join(cfg.RootDir, ".dr-test.env")
	cfg.ResultsDir = filepath.Join(cfg.RootDir, "dr-stress-results")
	cfg.DRStressBin = filepath.Join(cfg.RootDir, "bin", "dr-stress")

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			smokeUsage()
			return nil
		case arg == "--root":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.RootDir = value
			i = next
		case strings.HasPrefix(arg, "--root="):
			cfg.RootDir = strings.TrimPrefix(arg, "--root=")
		case arg == "--topology":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.Topology = value
			i = next
		case strings.HasPrefix(arg, "--topology="):
			cfg.Topology = strings.TrimPrefix(arg, "--topology=")
		case arg == "--env-file":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.EnvFile = value
			i = next
		case strings.HasPrefix(arg, "--env-file="):
			cfg.EnvFile = strings.TrimPrefix(arg, "--env-file=")
		case arg == "--results-dir" || arg == "-output-dir" || arg == "--output-dir":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.ResultsDir = value
			i = next
		case strings.HasPrefix(arg, "--results-dir="):
			cfg.ResultsDir = strings.TrimPrefix(arg, "--results-dir=")
		case strings.HasPrefix(arg, "-output-dir="):
			cfg.ResultsDir = strings.TrimPrefix(arg, "-output-dir=")
		case strings.HasPrefix(arg, "--output-dir="):
			cfg.ResultsDir = strings.TrimPrefix(arg, "--output-dir=")
		case arg == "--dr-stress-bin":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.DRStressBin = value
			i = next
		case strings.HasPrefix(arg, "--dr-stress-bin="):
			cfg.DRStressBin = strings.TrimPrefix(arg, "--dr-stress-bin=")
		case arg == "--tuning-profile":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.TuningProfile = value
			i = next
		case strings.HasPrefix(arg, "--tuning-profile="):
			cfg.TuningProfile = strings.TrimPrefix(arg, "--tuning-profile=")
		case arg == "--no-tuning" || arg == "--skip-tuning":
			cfg.TuningProfile = "none"
		case arg == "-run-id" || arg == "--run-id":
			value, next, err := requireValue(args, i, arg)
			if err != nil {
				return err
			}
			cfg.RunID = value
			i = next
		case strings.HasPrefix(arg, "-run-id="):
			cfg.RunID = strings.TrimPrefix(arg, "-run-id=")
		case strings.HasPrefix(arg, "--run-id="):
			cfg.RunID = strings.TrimPrefix(arg, "--run-id=")
		default:
			cfg.DRStressArgs = append(cfg.DRStressArgs, arg)
		}
	}

	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if !filepath.IsAbs(cfg.EnvFile) {
		cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
	}
	if !filepath.IsAbs(cfg.ResultsDir) {
		cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
	}
	if !filepath.IsAbs(cfg.DRStressBin) {
		cfg.DRStressBin = filepath.Join(cfg.RootDir, cfg.DRStressBin)
	}
	return scenario.RunMixedLoadSmoke(ctx, cfg)
}

func smokeUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  dr-harness smoke [harness options] [dr-stress run flags...]

Harness options:
  --root DIR
  --topology single|ha
  --env-file PATH
  --results-dir DIR, --output-dir DIR, -output-dir DIR
  --dr-stress-bin PATH
  --run-id ID, -run-id ID
  --tuning-profile constrained|relaxed|out-of-horizon|oversized-checkpoint|none
  --no-tuning, --skip-tuning

Unknown flags are forwarded to "dr-stress run". The harness supplies the local
topology addresses, tokens, -ensure-kv, default duration/concurrency/wait flags,
and a generated drmixed-* run id unless overridden.`)
}

func runPreSeedSmoke(ctx context.Context, args []string) error {
	cfg := scenario.PreSeedConfig{
		RootDir:                    defaultRootDir(),
		Topology:                   "single",
		Timeout:                    300 * time.Second,
		Reset:                      true,
		SeedKeys:                   64,
		SeedConcurrency:            32,
		SegmentMaxBytes:            8192,
		PostExportWriteKeys:        0,
		PostExportWriteConcurrency: 32,
		HTTPTimeout:                600 * time.Second,
	}
	cfg.EnvFile = filepath.Join(cfg.RootDir, ".dr-test.env")
	cfg.ResultsDir = filepath.Join(cfg.RootDir, "dr-stress-results")

	fs := flag.NewFlagSet("preseed-smoke", flag.ExitOnError)
	fs.StringVar(&cfg.RootDir, "root", cfg.RootDir, "OpenBao repository root")
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "DR local env file")
	fs.StringVar(&cfg.ResultsDir, "results-dir", cfg.ResultsDir, "Result artifact directory")
	fs.StringVar(&cfg.Topology, "topology", cfg.Topology, "Topology: single or ha")
	fs.BoolVar(&cfg.Reset, "reset", cfg.Reset, "Reset topology before running")
	fs.BoolVar(&cfg.Reset, "do-reset", cfg.Reset, "Compatibility alias for --reset")
	noReset := fs.Bool("no-reset", false, "Do not reset topology before running")
	fs.BoolVar(&cfg.Build, "build", false, "Build docker image before reset")
	fs.IntVar(&cfg.SeedKeys, "seed-keys", cfg.SeedKeys, "Seed key count")
	fs.IntVar(&cfg.SeedConcurrency, "seed-concurrency", cfg.SeedConcurrency, "Seed write concurrency")
	fs.IntVar(&cfg.SegmentMaxBytes, "segment-max-bytes", cfg.SegmentMaxBytes, "Pre-seed segment max bytes")
	fs.IntVar(&cfg.PostExportWriteKeys, "post-export-write-keys", cfg.PostExportWriteKeys, "Post-export write count")
	fs.IntVar(&cfg.PostExportWriteConcurrency, "post-export-write-concurrency", cfg.PostExportWriteConcurrency, "Post-export write concurrency")
	fs.StringVar(&cfg.DatasetFixture, "dataset-fixture", "", "Reusable primary dataset fixture name")
	fs.BoolVar(&cfg.AsyncExportPlan, "async-export-plan", false, "Build export plan asynchronously")
	timeoutSeconds := fs.Int("timeout", int(cfg.Timeout.Seconds()), "Scenario timeout in seconds")
	httpTimeoutSeconds := fs.Int("http-timeout", int(cfg.HTTPTimeout.Seconds()), "HTTP timeout in seconds")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noReset {
		cfg.Reset = false
	}
	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if !filepath.IsAbs(cfg.EnvFile) {
		cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
	}
	if !filepath.IsAbs(cfg.ResultsDir) {
		cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
	}
	cfg.Timeout = time.Duration(*timeoutSeconds) * time.Second
	cfg.HTTPTimeout = time.Duration(*httpTimeoutSeconds) * time.Second

	return scenario.RunPreSeedSmoke(ctx, cfg)
}

func runQuiescentReconnect(ctx context.Context, args []string) error {
	cfg := defaultHAConfig()
	fs := flag.NewFlagSet("quiescent-reconnect-smoke", flag.ExitOnError)
	finalize := bindHAFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	finalize()
	return scenario.RunQuiescentReconnect(ctx, cfg)
}

func runAccumulatorColdRestart(ctx context.Context, args []string) error {
	cfg := defaultHAConfig()
	cfg.StopSeconds = 10 * time.Second
	fs := flag.NewFlagSet("accumulator-cold-restart-smoke", flag.ExitOnError)
	finalize := bindHAFlags(fs, &cfg)
	stopSeconds := fs.Int("stop-seconds", int(cfg.StopSeconds.Seconds()), "Seconds to keep secondary1 stopped")
	if err := fs.Parse(args); err != nil {
		return err
	}
	finalize()
	cfg.StopSeconds = time.Duration(*stopSeconds) * time.Second
	return scenario.RunAccumulatorColdRestart(ctx, cfg)
}

func runSecondaryOutage(ctx context.Context, args []string, expectReconcile, expectBudget bool, runPrefix string) error {
	cfg := scenario.OutageConfig{
		RootDir:          defaultRootDir(),
		Topology:         "ha",
		Timeout:          240 * time.Second,
		HTTPTimeout:      600 * time.Second,
		Reset:            true,
		Duration:         180 * time.Second,
		Concurrency:      36,
		OutageAfter:      30 * time.Second,
		OutageSeconds:    60 * time.Second,
		ProgressInterval: 10 * time.Second,
		MonitorInterval:  2 * time.Second,
		MaxWait:          300 * time.Second,
		ExpectReconcile:  expectReconcile,
		ExpectBudget:     expectBudget,
		RunPrefix:        runPrefix,
	}
	if expectReconcile {
		cfg.Duration = 180 * time.Second
		cfg.Concurrency = 48
		cfg.OutageAfter = 20 * time.Second
		cfg.OutageSeconds = 90 * time.Second
		cfg.MaxWait = 900 * time.Second
		cfg.TuningProfile = "out-of-horizon"
	}
	if expectBudget {
		cfg.Duration = 120 * time.Second
		cfg.Concurrency = 32
		cfg.OutageAfter = 10 * time.Second
		cfg.OutageSeconds = 60 * time.Second
		cfg.MaxWait = 300 * time.Second
		cfg.TuningProfile = "reconcile-budget-pressure"
	}
	cfg.EnvFile = filepath.Join(cfg.RootDir, ".dr-test.env")
	cfg.ResultsDir = filepath.Join(cfg.RootDir, "dr-stress-results")
	cfg.DRStressBin = filepath.Join(cfg.RootDir, "bin", "dr-stress")

	fs := flag.NewFlagSet(runPrefix, flag.ExitOnError)
	fs.StringVar(&cfg.RootDir, "root", cfg.RootDir, "OpenBao repository root")
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "DR local env file")
	fs.StringVar(&cfg.ResultsDir, "results-dir", cfg.ResultsDir, "Result artifact directory")
	fs.StringVar(&cfg.DRStressBin, "dr-stress-bin", cfg.DRStressBin, "dr-stress binary path")
	fs.StringVar(&cfg.Topology, "topology", cfg.Topology, "Topology: ha")
	fs.BoolVar(&cfg.Reset, "reset", cfg.Reset, "Reset topology before running")
	noReset := fs.Bool("no-reset", false, "Do not reset topology before running")
	fs.BoolVar(&cfg.Build, "build", false, "Build docker image before reset")
	duration := fs.Int("duration", int(cfg.Duration.Seconds()), "Workload duration in seconds")
	concurrency := fs.Int("concurrency", cfg.Concurrency, "Workload concurrency")
	outageAfter := fs.Int("outage-after", int(cfg.OutageAfter.Seconds()), "Seconds before stopping secondary1")
	outageSeconds := fs.Int("outage-seconds", int(cfg.OutageSeconds.Seconds()), "Seconds to keep secondary1 stopped")
	progress := fs.Int("progress-interval", int(cfg.ProgressInterval.Seconds()), "dr-stress progress interval seconds")
	monitor := fs.Int("monitor-interval", int(cfg.MonitorInterval.Seconds()), "dr-stress monitor interval seconds")
	maxWait := fs.Int("max-wait-seconds", int(cfg.MaxWait.Seconds()), "Convergence timeout seconds")
	timeout := fs.Int("timeout", int(cfg.Timeout.Seconds()), "Scenario timeout seconds")
	fs.BoolVar(&cfg.ExpectReconcile, "expect-reconcile", cfg.ExpectReconcile, "Expect out-of-horizon reconciliation")
	fs.BoolVar(&cfg.ExpectBudget, "expect-budget", cfg.ExpectBudget, "Expect reconcile budget exhaustion instead of convergence")
	fs.StringVar(&cfg.TuningProfile, "tuning-profile", cfg.TuningProfile, "Tuning profile")
	fs.StringVar(&cfg.RunPrefix, "run-prefix", cfg.RunPrefix, "Run ID prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noReset {
		cfg.Reset = false
	}
	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if !filepath.IsAbs(cfg.EnvFile) {
		cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
	}
	if !filepath.IsAbs(cfg.ResultsDir) {
		cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
	}
	if !filepath.IsAbs(cfg.DRStressBin) {
		cfg.DRStressBin = filepath.Join(cfg.RootDir, cfg.DRStressBin)
	}
	cfg.Duration = time.Duration(*duration) * time.Second
	cfg.Concurrency = *concurrency
	cfg.OutageAfter = time.Duration(*outageAfter) * time.Second
	cfg.OutageSeconds = time.Duration(*outageSeconds) * time.Second
	cfg.ProgressInterval = time.Duration(*progress) * time.Second
	cfg.MonitorInterval = time.Duration(*monitor) * time.Second
	cfg.MaxWait = time.Duration(*maxWait) * time.Second
	cfg.Timeout = time.Duration(*timeout) * time.Second
	return scenario.RunSecondaryOutage(ctx, cfg)
}

func runTuningLoadSmoke(ctx context.Context, args []string) error {
	cfg := scenario.TuningLoadConfig{
		RootDir:          defaultRootDir(),
		Topology:         "ha",
		Timeout:          600 * time.Second,
		HTTPTimeout:      600 * time.Second,
		Reset:            true,
		Duration:         360 * time.Second,
		Concurrency:      36,
		StepdownInterval: 90 * time.Second,
		FirstTuneAfter:   60 * time.Second,
		SecondTuneAfter:  180 * time.Second,
		ProgressInterval: 10 * time.Second,
		MonitorInterval:  2 * time.Second,
		MaxWait:          600 * time.Second,
	}
	cfg.EnvFile = filepath.Join(cfg.RootDir, ".dr-test.env")
	cfg.ResultsDir = filepath.Join(cfg.RootDir, "dr-stress-results")
	cfg.DRStressBin = filepath.Join(cfg.RootDir, "bin", "dr-stress")

	fs := flag.NewFlagSet("tuning-load-smoke", flag.ExitOnError)
	fs.StringVar(&cfg.RootDir, "root", cfg.RootDir, "OpenBao repository root")
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "DR local env file")
	fs.StringVar(&cfg.ResultsDir, "results-dir", cfg.ResultsDir, "Result artifact directory")
	fs.StringVar(&cfg.DRStressBin, "dr-stress-bin", cfg.DRStressBin, "dr-stress binary path")
	fs.StringVar(&cfg.Topology, "topology", cfg.Topology, "Topology: ha")
	fs.BoolVar(&cfg.Reset, "reset", cfg.Reset, "Reset topology before running")
	noReset := fs.Bool("no-reset", false, "Do not reset topology before running")
	fs.BoolVar(&cfg.Build, "build", false, "Build docker image before reset")
	duration := fs.Int("duration", int(cfg.Duration.Seconds()), "Workload duration in seconds")
	concurrency := fs.Int("concurrency", cfg.Concurrency, "Workload concurrency")
	stepdown := fs.Int("stepdown-interval", int(cfg.StepdownInterval.Seconds()), "Primary stepdown interval seconds for dr-stress")
	firstTune := fs.Int("first-tune-after", int(cfg.FirstTuneAfter.Seconds()), "Seconds before applying constrained tuning")
	secondTune := fs.Int("second-tune-after", int(cfg.SecondTuneAfter.Seconds()), "Seconds before applying relaxed tuning")
	progress := fs.Int("progress-interval", int(cfg.ProgressInterval.Seconds()), "dr-stress progress interval seconds")
	monitor := fs.Int("monitor-interval", int(cfg.MonitorInterval.Seconds()), "dr-stress monitor interval seconds")
	maxWait := fs.Int("max-wait-seconds", int(cfg.MaxWait.Seconds()), "Convergence timeout seconds")
	timeout := fs.Int("timeout", int(cfg.Timeout.Seconds()), "Scenario timeout seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noReset {
		cfg.Reset = false
	}
	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if !filepath.IsAbs(cfg.EnvFile) {
		cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
	}
	if !filepath.IsAbs(cfg.ResultsDir) {
		cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
	}
	if !filepath.IsAbs(cfg.DRStressBin) {
		cfg.DRStressBin = filepath.Join(cfg.RootDir, cfg.DRStressBin)
	}
	cfg.Duration = time.Duration(*duration) * time.Second
	cfg.Concurrency = *concurrency
	cfg.StepdownInterval = time.Duration(*stepdown) * time.Second
	cfg.FirstTuneAfter = time.Duration(*firstTune) * time.Second
	cfg.SecondTuneAfter = time.Duration(*secondTune) * time.Second
	cfg.ProgressInterval = time.Duration(*progress) * time.Second
	cfg.MonitorInterval = time.Duration(*monitor) * time.Second
	cfg.MaxWait = time.Duration(*maxWait) * time.Second
	cfg.Timeout = time.Duration(*timeout) * time.Second
	return scenario.RunTuningLoadSmoke(ctx, cfg)
}

func runCompositeLifecycleSoak(ctx context.Context, args []string) error {
	cfg := scenario.CompositeLifecycleConfig{
		RootDir:                 defaultRootDir(),
		Topology:                "ha",
		Timeout:                 1800 * time.Second,
		HTTPTimeout:             600 * time.Second,
		Reset:                   true,
		SeedKeys:                100000,
		SeedConcurrency:         64,
		SegmentMaxBytes:         524288,
		AsyncExportPlan:         true,
		Duration:                3600 * time.Second,
		Concurrency:             48,
		StepdownInterval:        900 * time.Second,
		PreSeedAfter:            60 * time.Second,
		Secondary2OutageAfter:   1200 * time.Second,
		Secondary2OutageSeconds: 300 * time.Second,
		ProgressInterval:        30 * time.Second,
		MonitorInterval:         5 * time.Second,
		MaxWait:                 1800 * time.Second,
		TuningProfile:           "constrained",
	}
	cfg.EnvFile = filepath.Join(cfg.RootDir, ".dr-test.env")
	cfg.ResultsDir = filepath.Join(cfg.RootDir, "dr-stress-results")
	cfg.DRStressBin = filepath.Join(cfg.RootDir, "bin", "dr-stress")

	fs := flag.NewFlagSet("composite-lifecycle-soak", flag.ExitOnError)
	fs.StringVar(&cfg.RootDir, "root", cfg.RootDir, "OpenBao repository root")
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "DR local env file")
	fs.StringVar(&cfg.ResultsDir, "results-dir", cfg.ResultsDir, "Result artifact directory")
	fs.StringVar(&cfg.DRStressBin, "dr-stress-bin", cfg.DRStressBin, "dr-stress binary path")
	fs.StringVar(&cfg.Topology, "topology", cfg.Topology, "Topology: ha")
	fs.BoolVar(&cfg.Reset, "reset", cfg.Reset, "Reset topology before running")
	noReset := fs.Bool("no-reset", false, "Do not reset topology before running")
	fs.BoolVar(&cfg.Build, "build", false, "Build docker image before reset")
	fs.IntVar(&cfg.SeedKeys, "seed-keys", cfg.SeedKeys, "Initial primary seed key count")
	fs.IntVar(&cfg.SeedConcurrency, "seed-concurrency", cfg.SeedConcurrency, "Initial seed write concurrency")
	fs.IntVar(&cfg.SegmentMaxBytes, "segment-max-bytes", cfg.SegmentMaxBytes, "Pre-seed segment max bytes")
	fs.BoolVar(&cfg.AsyncExportPlan, "async-export-plan", cfg.AsyncExportPlan, "Build pre-seed export plan asynchronously")
	fs.StringVar(&cfg.TuningProfile, "tuning-profile", cfg.TuningProfile, "Initial DR tuning profile: constrained|relaxed|out-of-horizon|oversized-checkpoint|none")
	duration := fs.Int("duration", int(cfg.Duration.Seconds()), "Workload duration in seconds")
	concurrency := fs.Int("concurrency", cfg.Concurrency, "Workload concurrency")
	stepdown := fs.Int("stepdown-interval", int(cfg.StepdownInterval.Seconds()), "Primary stepdown interval seconds for dr-stress")
	preseedAfter := fs.Int("preseed-after", int(cfg.PreSeedAfter.Seconds()), "Seconds after workload start before pre-seeding secondary1")
	outageAfter := fs.Int("secondary2-outage-after", int(cfg.Secondary2OutageAfter.Seconds()), "Seconds after workload start before stopping secondary2")
	outageSeconds := fs.Int("secondary2-outage-seconds", int(cfg.Secondary2OutageSeconds.Seconds()), "Seconds to keep secondary2 stopped")
	progress := fs.Int("progress-interval", int(cfg.ProgressInterval.Seconds()), "dr-stress progress interval seconds")
	monitor := fs.Int("monitor-interval", int(cfg.MonitorInterval.Seconds()), "dr-stress monitor interval seconds")
	maxWait := fs.Int("max-wait-seconds", int(cfg.MaxWait.Seconds()), "Convergence timeout seconds")
	timeout := fs.Int("timeout", int(cfg.Timeout.Seconds()), "Scenario timeout seconds")
	httpTimeout := fs.Int("http-timeout", int(cfg.HTTPTimeout.Seconds()), "HTTP timeout seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *noReset {
		cfg.Reset = false
	}
	cfg.RootDir = filepath.Clean(cfg.RootDir)
	if !filepath.IsAbs(cfg.EnvFile) {
		cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
	}
	if !filepath.IsAbs(cfg.ResultsDir) {
		cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
	}
	if !filepath.IsAbs(cfg.DRStressBin) {
		cfg.DRStressBin = filepath.Join(cfg.RootDir, cfg.DRStressBin)
	}
	cfg.Duration = time.Duration(*duration) * time.Second
	cfg.Concurrency = *concurrency
	cfg.StepdownInterval = time.Duration(*stepdown) * time.Second
	cfg.PreSeedAfter = time.Duration(*preseedAfter) * time.Second
	cfg.Secondary2OutageAfter = time.Duration(*outageAfter) * time.Second
	cfg.Secondary2OutageSeconds = time.Duration(*outageSeconds) * time.Second
	cfg.ProgressInterval = time.Duration(*progress) * time.Second
	cfg.MonitorInterval = time.Duration(*monitor) * time.Second
	cfg.MaxWait = time.Duration(*maxWait) * time.Second
	cfg.Timeout = time.Duration(*timeout) * time.Second
	cfg.HTTPTimeout = time.Duration(*httpTimeout) * time.Second
	return scenario.RunCompositeLifecycleSoak(ctx, cfg)
}

func defaultHAConfig() scenario.HAConfig {
	root := defaultRootDir()
	return scenario.HAConfig{
		RootDir:     root,
		Topology:    "ha",
		EnvFile:     filepath.Join(root, ".dr-test.env"),
		ResultsDir:  filepath.Join(root, "dr-stress-results"),
		Timeout:     240 * time.Second,
		HTTPTimeout: 600 * time.Second,
		Reset:       true,
	}
}

func bindHAFlags(fs *flag.FlagSet, cfg *scenario.HAConfig) func() {
	fs.StringVar(&cfg.RootDir, "root", cfg.RootDir, "OpenBao repository root")
	fs.StringVar(&cfg.EnvFile, "env-file", cfg.EnvFile, "DR local env file")
	fs.StringVar(&cfg.ResultsDir, "results-dir", cfg.ResultsDir, "Result artifact directory")
	fs.StringVar(&cfg.Topology, "topology", cfg.Topology, "Topology: ha")
	fs.BoolVar(&cfg.Reset, "reset", cfg.Reset, "Reset topology before running")
	noReset := fs.Bool("no-reset", false, "Do not reset topology before running")
	fs.BoolVar(&cfg.Build, "build", false, "Build docker image before reset")
	timeout := fs.Int("timeout", int(cfg.Timeout.Seconds()), "Scenario timeout seconds")
	return func() {
		if *noReset {
			cfg.Reset = false
		}
		cfg.RootDir = filepath.Clean(cfg.RootDir)
		if !filepath.IsAbs(cfg.EnvFile) {
			cfg.EnvFile = filepath.Join(cfg.RootDir, cfg.EnvFile)
		}
		if !filepath.IsAbs(cfg.ResultsDir) {
			cfg.ResultsDir = filepath.Join(cfg.RootDir, cfg.ResultsDir)
		}
		cfg.Timeout = time.Duration(*timeout) * time.Second
	}
}

func defaultRootDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "scripts", "dr_local_test.sh")) && fileExists(filepath.Join(dir, "docker-compose.dr-test.yml")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return filepath.Clean(filepath.Join(cwd, "..", ".."))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func requireValue(args []string, index int, flagName string) (string, int, error) {
	next := index + 1
	if next >= len(args) {
		return "", index, fmt.Errorf("missing value for %s", flagName)
	}
	return args[next], next, nil
}
