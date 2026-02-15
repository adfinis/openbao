package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// NodeConfig holds connection details for a single OpenBao node.
type NodeConfig struct {
	Addr          string `json:"addr"`
	Token         string `json:"-"` // never serialized
	CACert        string `json:"ca_cert,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`
	SkipVerify    bool   `json:"skip_verify,omitempty"`
}

// Configured returns true if the node has at least an address set.
func (n *NodeConfig) Configured() bool {
	return n.Addr != ""
}

// Config is the full harness configuration.
type Config struct {
	// Mode
	Mode string `json:"mode"` // run | analyze | verify

	// Nodes
	Primary    NodeConfig `json:"primary"`
	Secondary1 NodeConfig `json:"secondary1"`
	Secondary2 NodeConfig `json:"secondary2"`

	// KV settings
	KVMount   string `json:"kv_mount"`
	KeyPrefix string `json:"key_prefix"`
	EnsureKV  bool   `json:"ensure_kv"`

	// Run identity
	RunID     string `json:"run_id"`
	OutputDir string `json:"output_dir"`

	// Workload
	Workload     string        `json:"workload"`
	Duration     time.Duration `json:"duration_seconds"`
	Concurrency  int           `json:"concurrency"`
	WriteRetries int           `json:"write_retries"`

	// Intervals
	MonitorInterval  time.Duration `json:"monitor_interval_seconds"`
	ProgressInterval time.Duration `json:"progress_interval_seconds"`
	MaxWaitSeconds   int           `json:"max_wait_seconds"`
	StepdownInterval time.Duration `json:"stepdown_interval_seconds"`

	// Operation mix (must sum to 100)
	PutPercent        int `json:"put_percent"`
	GetPrimaryPercent int `json:"get_primary_percent"`
	StatusS1Percent   int `json:"status_s1_percent"`
	StatusS2Percent   int `json:"status_s2_percent"`

	// Key distribution
	HotKeyCount  int `json:"hot_key_count"`
	ColdKeyCount int `json:"cold_key_count"`
	HotPercent   int `json:"hot_percent"`

	// Payload sizes
	SmallBytes          int `json:"small_bytes"`
	LargeBytes          int `json:"large_bytes"`
	LargePayloadPercent int `json:"large_payload_percent"`

	// HTTP tuning
	HTTPTimeout    time.Duration `json:"http_timeout_seconds"`
	MaxIdleConns   int           `json:"max_idle_conns"`
	MaxIdlePerHost int           `json:"max_idle_per_host"`

	// Reproducibility
	Seed uint64 `json:"seed"`

	// Internal
	EventBuffer int `json:"event_buffer"`

	// Analyze/verify paths (positional args for analyze mode)
	AnalyzePaths []string `json:"-"`
}

// DefaultConfig returns a Config with production defaults matching the
// original bash script.
func DefaultConfig() *Config {
	return &Config{
		Mode:      "run",
		Workload:  "kv",
		KVMount:   "kv",
		KeyPrefix: "dr-mixed",
		OutputDir: "./dr-stress-results",
		RunID:     fmt.Sprintf("drmixed-%s", time.Now().UTC().Format("20060102T150405Z")),

		Duration:     15 * time.Minute,
		Concurrency:  24,
		WriteRetries: 2,

		MonitorInterval:  2 * time.Second,
		ProgressInterval: 2 * time.Second,
		MaxWaitSeconds:   600,
		StepdownInterval: 0,

		PutPercent:        55,
		GetPrimaryPercent: 25,
		StatusS1Percent:   10,
		StatusS2Percent:   10,

		HotKeyCount:  200,
		ColdKeyCount: 20000,
		HotPercent:   80,

		SmallBytes:          512,
		LargeBytes:          8192,
		LargePayloadPercent: 15,

		HTTPTimeout:    30 * time.Second,
		MaxIdleConns:   100,
		MaxIdlePerHost: 100,

		Seed:        uint64(time.Now().UnixNano()),
		EventBuffer: 0, // 0 means auto = concurrency * 256
	}
}

// Validate checks invariants and returns the first error found.
func (c *Config) Validate() error {
	if c.Mode == "run" {
		if c.Primary.Addr == "" {
			return fmt.Errorf("--primary-addr is required")
		}
		if c.Primary.Token == "" {
			return fmt.Errorf("--primary-token is required")
		}
		if c.Concurrency <= 0 {
			return fmt.Errorf("--concurrency must be > 0")
		}
		if c.Duration <= 0 {
			return fmt.Errorf("--duration must be > 0")
		}

		mix := c.PutPercent + c.GetPrimaryPercent + c.StatusS1Percent + c.StatusS2Percent
		if mix != 100 {
			return fmt.Errorf("operation percentages must sum to 100, got %d", mix)
		}

		if !c.Secondary1.Configured() && c.StatusS1Percent > 0 {
			return fmt.Errorf("--status-s1-percent > 0 requires secondary1 endpoint")
		}
		if !c.Secondary2.Configured() && c.StatusS2Percent > 0 {
			return fmt.Errorf("--status-s2-percent > 0 requires secondary2 endpoint")
		}

		if c.Secondary1.Configured() && c.Secondary1.Token == "" {
			return fmt.Errorf("secondary1 addr and token must be provided together")
		}
		if c.Secondary2.Configured() && c.Secondary2.Token == "" {
			return fmt.Errorf("secondary2 addr and token must be provided together")
		}
	}

	if c.EventBuffer == 0 {
		c.EventBuffer = c.Concurrency * 256
		if c.EventBuffer < 1024 {
			c.EventBuffer = 1024
		}
	}

	return nil
}

// ConfigSnapshot holds all config plus environment metadata for config.json.
type ConfigSnapshot struct {
	Config
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	NumCPU    int    `json:"num_cpu"`
	VCSRev    string `json:"vcs_revision,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
}

// Snapshot returns a ConfigSnapshot with environment metadata.
func (c *Config) Snapshot() ConfigSnapshot {
	s := ConfigSnapshot{
		Config:    *c,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
	}
	if h, err := os.Hostname(); err == nil {
		s.Hostname = h
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range bi.Settings {
			if setting.Key == "vcs.revision" {
				s.VCSRev = setting.Value
				break
			}
		}
	}
	return s
}

// WriteJSON writes the config snapshot to path as indented JSON.
func (s *ConfigSnapshot) WriteJSON(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}
