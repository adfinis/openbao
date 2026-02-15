package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	mode := os.Args[1]
	switch mode {
	case "run":
		runMode(os.Args[2:])
	case "analyze":
		analyzeMode(os.Args[2:])
	case "verify":
		verifyMode(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", mode)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  dr-stress run [options]       Execute mixed workload + record timelines + per-op latencies
  dr-stress analyze [paths...]  Read run artifacts, compute percentiles/histograms
  dr-stress verify [options]    Post-run correctness checks against primary
  dr-stress help                Show this help

Run "dr-stress <mode> --help" for mode-specific flags.`)
}

func runMode(args []string) {
	cfg := DefaultConfig()
	cfg.Mode = "run"

	fs := flag.NewFlagSet("run", flag.ExitOnError)

	// Node connection flags.
	fs.StringVar(&cfg.Primary.Addr, "primary-addr", cfg.Primary.Addr, "Primary BAO_ADDR (required)")
	fs.StringVar(&cfg.Primary.Token, "primary-token", cfg.Primary.Token, "Primary BAO_TOKEN (required)")
	fs.StringVar(&cfg.Secondary1.Addr, "secondary1-addr", cfg.Secondary1.Addr, "Secondary #1 BAO_ADDR")
	fs.StringVar(&cfg.Secondary1.Token, "secondary1-token", cfg.Secondary1.Token, "Secondary #1 BAO_TOKEN")
	fs.StringVar(&cfg.Secondary2.Addr, "secondary2-addr", cfg.Secondary2.Addr, "Secondary #2 BAO_ADDR")
	fs.StringVar(&cfg.Secondary2.Token, "secondary2-token", cfg.Secondary2.Token, "Secondary #2 BAO_TOKEN")

	// TLS flags.
	fs.StringVar(&cfg.Primary.CACert, "primary-cacert", "", "Primary CA cert path")
	fs.StringVar(&cfg.Secondary1.CACert, "secondary1-cacert", "", "Secondary #1 CA cert path")
	fs.StringVar(&cfg.Secondary2.CACert, "secondary2-cacert", "", "Secondary #2 CA cert path")
	fs.StringVar(&cfg.Primary.TLSServerName, "primary-tls-server-name", "", "Primary TLS server name")
	fs.StringVar(&cfg.Secondary1.TLSServerName, "secondary1-tls-server-name", "", "Secondary #1 TLS server name")
	fs.StringVar(&cfg.Secondary2.TLSServerName, "secondary2-tls-server-name", "", "Secondary #2 TLS server name")
	fs.BoolVar(&cfg.Primary.SkipVerify, "primary-skip-verify", false, "Skip TLS verify for primary")
	fs.BoolVar(&cfg.Secondary1.SkipVerify, "secondary1-skip-verify", false, "Skip TLS verify for secondary #1")
	fs.BoolVar(&cfg.Secondary2.SkipVerify, "secondary2-skip-verify", false, "Skip TLS verify for secondary #2")

	// KV settings.
	fs.StringVar(&cfg.KVMount, "kv-mount", cfg.KVMount, "KV v2 mount path")
	fs.StringVar(&cfg.KeyPrefix, "key-prefix", cfg.KeyPrefix, "Key prefix")
	fs.BoolVar(&cfg.EnsureKV, "ensure-kv", cfg.EnsureKV, "Create KV mount if missing")

	// Run identity.
	fs.StringVar(&cfg.RunID, "run-id", cfg.RunID, "Run identifier")
	fs.StringVar(&cfg.OutputDir, "output-dir", cfg.OutputDir, "Results root directory")

	// Workload.
	fs.StringVar(&cfg.Workload, "workload", cfg.Workload, "Workload type (kv)")

	// Duration and concurrency.
	var durationSec int
	fs.IntVar(&durationSec, "duration", 900, "Workload duration in seconds")
	fs.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "Worker count")
	fs.IntVar(&cfg.WriteRetries, "write-retries", cfg.WriteRetries, "PUT retry count")

	// Intervals.
	var monitorSec, progressSec, stepdownSec int
	fs.IntVar(&monitorSec, "monitor-interval", 2, "Status poll interval in seconds")
	fs.IntVar(&progressSec, "progress-interval", 2, "Console progress interval in seconds")
	fs.IntVar(&cfg.MaxWaitSeconds, "max-wait-seconds", cfg.MaxWaitSeconds, "Sentinel convergence timeout")
	fs.IntVar(&stepdownSec, "stepdown-interval", 0, "Primary stepdown interval; 0 disables")

	// Operation mix.
	fs.IntVar(&cfg.PutPercent, "put-percent", cfg.PutPercent, "Percent PUT ops")
	fs.IntVar(&cfg.GetPrimaryPercent, "get-primary-percent", cfg.GetPrimaryPercent, "Percent GETs from primary")
	fs.IntVar(&cfg.StatusS1Percent, "status-s1-percent", cfg.StatusS1Percent, "Percent DR status checks on secondary #1")
	fs.IntVar(&cfg.StatusS2Percent, "status-s2-percent", cfg.StatusS2Percent, "Percent DR status checks on secondary #2")

	// Key distribution.
	fs.IntVar(&cfg.HotKeyCount, "hot-key-count", cfg.HotKeyCount, "Number of hot keys")
	fs.IntVar(&cfg.ColdKeyCount, "cold-key-count", cfg.ColdKeyCount, "Number of cold keys")
	fs.IntVar(&cfg.HotPercent, "hot-percent", cfg.HotPercent, "Chance to target hot key [0-100]")

	// Payload sizes.
	fs.IntVar(&cfg.SmallBytes, "small-bytes", cfg.SmallBytes, "Small payload size in bytes")
	fs.IntVar(&cfg.LargeBytes, "large-bytes", cfg.LargeBytes, "Large payload size in bytes")
	fs.IntVar(&cfg.LargePayloadPercent, "large-percent", cfg.LargePayloadPercent, "Large payload chance [0-100]")

	// HTTP tuning.
	var httpTimeoutSec int
	fs.IntVar(&httpTimeoutSec, "http-timeout", 30, "HTTP response timeout in seconds")
	fs.IntVar(&cfg.MaxIdleConns, "max-idle-conns", cfg.MaxIdleConns, "Max idle HTTP connections")
	fs.IntVar(&cfg.MaxIdlePerHost, "max-idle-per-host", cfg.MaxIdlePerHost, "Max idle HTTP connections per host")

	// Reproducibility.
	fs.Uint64Var(&cfg.Seed, "seed", cfg.Seed, "PRNG seed (0 = current time)")

	// Internal tuning.
	fs.IntVar(&cfg.EventBuffer, "event-buffer", cfg.EventBuffer, "Event channel buffer size (0 = auto)")

	fs.Parse(args)

	// Convert second-based flags to durations.
	cfg.Duration = time.Duration(durationSec) * time.Second
	cfg.MonitorInterval = time.Duration(monitorSec) * time.Second
	cfg.ProgressInterval = time.Duration(progressSec) * time.Second
	cfg.StepdownInterval = time.Duration(stepdownSec) * time.Second
	cfg.HTTPTimeout = time.Duration(httpTimeoutSec) * time.Second

	if err := cfg.Validate(); err != nil {
		log.Fatalf("config error: %v", err)
	}

	if err := executeRun(cfg); err != nil {
		log.Fatalf("run failed: %v", err)
	}
}

func executeRun(cfg *Config) error {
	// Create output directory.
	runDir := filepath.Join(cfg.OutputDir, cfg.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	// Build clients.
	primaryClient, err := NewBaoClient(cfg.Primary, cfg)
	if err != nil {
		return fmt.Errorf("primary client: %w", err)
	}

	clients := &ClientSet{Primary: primaryClient}

	if cfg.Secondary1.Configured() {
		c, err := NewBaoClient(cfg.Secondary1, cfg)
		if err != nil {
			return fmt.Errorf("secondary1 client: %w", err)
		}
		clients.Secondary1 = c
	}
	if cfg.Secondary2.Configured() {
		c, err := NewBaoClient(cfg.Secondary2, cfg)
		if err != nil {
			return fmt.Errorf("secondary2 client: %w", err)
		}
		clients.Secondary2 = c
	}

	// Ensure KV mount exists.
	if cfg.EnsureKV {
		if err := ensureKVMount(cfg, clients.Primary); err != nil {
			return err
		}
	}

	// Write config snapshot.
	configPath := filepath.Join(runDir, "config.json")
	snap := cfg.Snapshot()
	if err := snap.WriteJSON(configPath); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	// Capture before status.
	beforeStatus := captureStatus(clients)

	// Create workload.
	wl, err := NewWorkload(cfg.Workload, cfg, clients)
	if err != nil {
		return fmt.Errorf("create workload: %w", err)
	}

	// Create recorder.
	eventsPath := filepath.Join(runDir, "events.ndjson")
	writesPath := filepath.Join(runDir, "writes.ndjson")
	rec, err := NewRecorder(eventsPath, writesPath, cfg.EventBuffer)
	if err != nil {
		return fmt.Errorf("create recorder: %w", err)
	}
	if err := rec.Run(); err != nil {
		return fmt.Errorf("start recorder: %w", err)
	}

	// Create monitor.
	timelinePath := filepath.Join(runDir, "status_timeline.ndjson")
	mon := NewMonitor(clients, cfg.MonitorInterval, timelinePath)

	// Create progress reporter.
	progressPath := filepath.Join(runDir, "progress.log")
	progress := NewProgressReporter(rec, mon, cfg, progressPath)

	// Set up context with deadline and signal handling.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("received %v, stopping workload...", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	startAt := time.Now()

	// Start background goroutines.
	go mon.Run(ctx)
	go progress.Run(ctx)
	go RunStepdownLoop(ctx, clients.Primary, cfg.StepdownInterval)

	// Run workers (blocks until all done).
	log.Printf("starting %d workers for %s (seed=%d, workload=%s)",
		cfg.Concurrency, cfg.Duration, cfg.Seed, cfg.Workload)
	RunWorkers(ctx, cfg, wl, rec)

	// Stop recorder (flush remaining events).
	rec.Close()

	// Cancel background goroutines.
	cancel()

	// Run sentinel convergence check.
	sentinelCtx, sentinelCancel := context.WithTimeout(context.Background(), time.Duration(cfg.MaxWaitSeconds)*time.Second+10*time.Second)
	defer sentinelCancel()
	s1Result, s2Result := RunSentinel(sentinelCtx, cfg, clients)

	endAt := time.Now()

	// Capture after status.
	afterStatus := captureStatus(clients)

	// Build result.
	durationMS := endAt.Sub(startAt).Milliseconds()
	totalOps := rec.TotalOps.Load()
	var opsPerSec float64
	if durationMS > 0 {
		opsPerSec = float64(totalOps) * 1000.0 / float64(durationMS)
	}

	result := map[string]interface{}{
		"run_id":      cfg.RunID,
		"created_at":  startAt.UTC().Format(time.RFC3339),
		"data_prefix": fmt.Sprintf("%s/%s/%s", cfg.KVMount, cfg.KeyPrefix, cfg.RunID),
		"parameters": map[string]interface{}{
			"duration_seconds":      int(cfg.Duration.Seconds()),
			"concurrency":           cfg.Concurrency,
			"put_percent":           cfg.PutPercent,
			"get_primary_percent":   cfg.GetPrimaryPercent,
			"status_s1_percent":     cfg.StatusS1Percent,
			"status_s2_percent":     cfg.StatusS2Percent,
			"hot_key_count":         cfg.HotKeyCount,
			"cold_key_count":        cfg.ColdKeyCount,
			"hot_percent":           cfg.HotPercent,
			"small_bytes":           cfg.SmallBytes,
			"large_bytes":           cfg.LargeBytes,
			"large_payload_percent": cfg.LargePayloadPercent,
			"write_retries":         cfg.WriteRetries,
			"stepdown_interval":     int(cfg.StepdownInterval.Seconds()),
			"seed":                  cfg.Seed,
			"workload":              cfg.Workload,
		},
		"workload": map[string]interface{}{
			"duration_ms":           durationMS,
			"operations_total":      totalOps,
			"operations_per_second": opsPerSec,
			"put_ok":                rec.PutOK.Load(),
			"put_fail":              rec.PutFail.Load(),
			"get_ok":                rec.GetOK.Load(),
			"get_fail":              rec.GetFail.Load(),
			"status_ok":             rec.StatusOK.Load(),
			"status_fail":           rec.StatusFail.Load(),
			"events_dropped":        rec.Dropped.Load(),
		},
		"replication": map[string]interface{}{
			"sentinel_secondary1": s1Result,
			"sentinel_secondary2": s2Result,
		},
		"status_before": beforeStatus,
		"status_after":  afterStatus,
		"artifacts": map[string]interface{}{
			"config_file":   configPath,
			"events_file":   eventsPath,
			"writes_file":   writesPath,
			"timeline_file": timelinePath,
			"progress_file": progressPath,
		},
	}

	resultPath := filepath.Join(runDir, "result.json")
	resultJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	if err := os.WriteFile(resultPath, resultJSON, 0o644); err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	// Print summary.
	fmt.Println("Run complete:")
	fmt.Printf("  run_id=%s\n", cfg.RunID)
	fmt.Printf("  result_file=%s\n", resultPath)
	fmt.Printf("  ops_total=%d ops_per_sec=%.2f\n", totalOps, opsPerSec)
	fmt.Printf("  put_fail=%d get_fail=%d status_fail=%d\n",
		rec.PutFail.Load(), rec.GetFail.Load(), rec.StatusFail.Load())
	fmt.Printf("  sentinel: s1=%.1fs s2=%.1fs\n",
		s1Result.ConvergeSeconds, s2Result.ConvergeSeconds)

	return nil
}

func ensureKVMount(cfg *Config, client *BaoClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keys, err := client.SecretsList(ctx, "sys/mounts")
	if err != nil {
		return fmt.Errorf("list mounts: %w", err)
	}

	mountPath := cfg.KVMount + "/"
	for _, k := range keys {
		if k == mountPath {
			return nil // already mounted
		}
	}

	log.Printf("creating KV mount at %s/", cfg.KVMount)
	if err := client.SecretsEnable(ctx, cfg.KVMount, "kv-v2"); err != nil {
		return fmt.Errorf("enable KV mount: %w", err)
	}
	return nil
}

type statusCapture struct {
	Primary    *DRStatusResponse `json:"primary,omitempty"`
	Secondary1 *DRStatusResponse `json:"secondary1,omitempty"`
	Secondary2 *DRStatusResponse `json:"secondary2,omitempty"`
}

func captureStatus(clients *ClientSet) statusCapture {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sc := statusCapture{}
	if clients.Primary != nil {
		s, _, _ := clients.Primary.DRStatus(ctx)
		sc.Primary = s
	}
	if clients.Secondary1 != nil {
		s, _, _ := clients.Secondary1.DRStatus(ctx)
		sc.Secondary1 = s
	}
	if clients.Secondary2 != nil {
		s, _, _ := clients.Secondary2.DRStatus(ctx)
		sc.Secondary2 = s
	}
	return sc
}
