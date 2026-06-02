package topology

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/bao"
)

type Env map[string]string

func LoadEnv(path string) (Env, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	env := Env{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
			value = strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'")
		}
		env[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	return env, nil
}

func (e Env) Required(key string) (string, error) {
	value := strings.TrimSpace(e[key])
	if value == "" {
		return "", fmt.Errorf("missing %s in env file", key)
	}
	return value, nil
}

type Config struct {
	RootDir  string
	Topology string
	EnvFile  string
	Timeout  time.Duration
	Build    bool
}

type Cluster struct {
	Name       string
	Addrs      []string
	Token      string
	UnsealKeys []string
	Services   []string
}

type Layout struct {
	Primary    Cluster
	Secondary1 Cluster
	Secondary2 Cluster
}

func Reset(ctx context.Context, cfg Config, datasetFixture string, primaryOnly bool) error {
	args := []string{"--topology", cfg.Topology}
	if datasetFixture != "" {
		args = append(args, "dataset-fixture-restore", datasetFixture)
		if primaryOnly {
			args = append(args, "--primary-only")
		}
	} else {
		args = append(args, "reset")
	}
	if cfg.Build {
		args = append(args, "--build")
	}
	return runLocalScript(ctx, cfg.RootDir, args...)
}

func runLocalScript(ctx context.Context, root string, args ...string) error {
	script := filepath.Join(root, "scripts", "dr_local_test.sh")
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", script, strings.Join(args, " "), err)
	}
	return nil
}

func LayoutFromEnv(topology string, env Env) (Layout, error) {
	primaryToken, err := env.Required("DR_PRIMARY_TOKEN")
	if err != nil {
		return Layout{}, err
	}
	secondary1Token, err := env.Required("DR_SECONDARY1_TOKEN")
	if err != nil {
		return Layout{}, err
	}
	secondary2Token, err := env.Required("DR_SECONDARY2_TOKEN")
	if err != nil {
		return Layout{}, err
	}

	primaryAddr := envValue(env, "DR_PRIMARY_ADDR", "http://localhost:8800")
	secondary1Addr := envValue(env, "DR_SECONDARY1_ADDR", "http://localhost:8900")
	secondary2Addr := envValue(env, "DR_SECONDARY2_ADDR", "http://localhost:9000")

	switch topology {
	case "single":
		return Layout{
			Primary: Cluster{
				Name: "primary", Addrs: []string{primaryAddr}, Token: primaryToken,
				UnsealKeys: nonEmpty(env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"primary"},
			},
			Secondary1: Cluster{
				Name: "secondary1", Addrs: []string{secondary1Addr}, Token: secondary1Token,
				UnsealKeys: nonEmpty(env["DR_SECONDARY1_UNSEAL_KEY"], env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"secondary1"},
			},
			Secondary2: Cluster{
				Name: "secondary2", Addrs: []string{secondary2Addr}, Token: secondary2Token,
				UnsealKeys: nonEmpty(env["DR_SECONDARY2_UNSEAL_KEY"], env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"secondary2"},
			},
		}, nil
	case "ha":
		return Layout{
			Primary: Cluster{Name: "primary", Addrs: []string{
				envValue(env, "DR_PRIMARY_NODE1_ADDR", primaryAddr),
				envValue(env, "DR_PRIMARY_NODE2_ADDR", "http://localhost:8802"),
				envValue(env, "DR_PRIMARY_NODE3_ADDR", "http://localhost:8804"),
			}, Token: primaryToken, UnsealKeys: nonEmpty(env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"primary-1", "primary-2", "primary-3"}},
			Secondary1: Cluster{Name: "secondary1", Addrs: []string{
				envValue(env, "DR_SECONDARY1_NODE1_ADDR", secondary1Addr),
				envValue(env, "DR_SECONDARY1_NODE2_ADDR", "http://localhost:8902"),
				envValue(env, "DR_SECONDARY1_NODE3_ADDR", "http://localhost:8904"),
			}, Token: secondary1Token, UnsealKeys: nonEmpty(env["DR_SECONDARY1_UNSEAL_KEY"], env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"secondary1-1", "secondary1-2", "secondary1-3"}},
			Secondary2: Cluster{Name: "secondary2", Addrs: []string{
				envValue(env, "DR_SECONDARY2_NODE1_ADDR", secondary2Addr),
				envValue(env, "DR_SECONDARY2_NODE2_ADDR", "http://localhost:9002"),
				envValue(env, "DR_SECONDARY2_NODE3_ADDR", "http://localhost:9004"),
			}, Token: secondary2Token, UnsealKeys: nonEmpty(env["DR_SECONDARY2_UNSEAL_KEY"], env["DR_PRIMARY_UNSEAL_KEY"]), Services: []string{"secondary2-1", "secondary2-2", "secondary2-3"}},
		}, nil
	default:
		return Layout{}, fmt.Errorf("unknown topology %q", topology)
	}
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func envValue(env Env, key, fallback string) string {
	if v := strings.TrimSpace(env[key]); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func NewClient(cluster Cluster, timeout time.Duration) (*bao.Client, error) {
	return bao.NewClient(bao.Node{
		Addrs:       cluster.Addrs,
		Token:       cluster.Token,
		HTTPTimeout: timeout,
	})
}

func WaitActiveAddr(ctx context.Context, client *bao.Client, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		addr, err := client.ResolveActiveAddr(ctx)
		if err == nil && addr != "" {
			return addr, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %s active node: %w", name, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

func WaitActiveChange(ctx context.Context, client *bao.Client, name, oldActive string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		client.ClearActiveAddr()
		addr, err := client.ResolveActiveAddr(ctx)
		if err == nil && addr != "" && addr != oldActive {
			return addr, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %s active handoff away from %s: %w", name, oldActive, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

func WaitSecondaryReady(ctx context.Context, client *bao.Client, label string, timeout time.Duration) (*bao.DRStatus, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastStatus *bao.DRStatus
	for {
		status, _, err := client.DRStatus(ctx)
		if err == nil && status.Mode == "secondary" && status.SecondaryState == "streaming" && status.LagEntries == 0 {
			fmt.Printf("%s ready: state=%s lag=%d applied=%d primary=%d\n", label, status.SecondaryState, status.LagEntries, status.LastAppliedIndex, status.PrimaryIndex)
			return status, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastStatus = status
		}
		if time.Now().After(deadline) {
			if lastStatus != nil {
				return nil, fmt.Errorf("timed out waiting for %s secondary ready: state=%s lag=%d applied=%d primary=%d reconcile_phase=%s reconcile_count=%d last_error=%v",
					label,
					lastStatus.SecondaryState,
					lastStatus.LagEntries,
					lastStatus.LastAppliedIndex,
					lastStatus.PrimaryIndex,
					lastStatus.ReconcilePhase,
					lastStatus.ReconcileCount,
					lastErr)
			}
			return nil, fmt.Errorf("timed out waiting for %s secondary ready: %w", label, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}
}

func WaitSecondaryQuiescent(ctx context.Context, client *bao.Client, label string, timeout time.Duration) (*bao.DRStatus, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	var previous *bao.DRStatus
	stable := 0
	for {
		status, _, err := client.DRStatus(ctx)
		if err == nil {
			if status.Mode == "secondary" && status.SecondaryState == "streaming" && status.LagEntries == 0 {
				if previous != nil &&
					status.LastAppliedIndex == previous.LastAppliedIndex &&
					status.PrimaryIndex == previous.PrimaryIndex &&
					status.StreamTxnBatchesTotal == previous.StreamTxnBatchesTotal {
					stable++
				} else {
					stable = 0
				}
				previous = status
				if stable >= 2 {
					fmt.Printf("%s quiescent: state=%s lag=%d applied=%d primary=%d batches=%d\n", label, status.SecondaryState, status.LagEntries, status.LastAppliedIndex, status.PrimaryIndex, status.StreamTxnBatchesTotal)
					return status, nil
				}
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s secondary quiescent: %w", label, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}
}

func WaitOptimizedReconnect(ctx context.Context, client *bao.Client, label string, beforeFastPath, beforeReconcile int64, timeout time.Duration) (*bao.DRStatus, []byte, error) {
	deadline := time.Now().Add(timeout)
	var lastRaw []byte
	var lastErr error
	for {
		status, raw, err := client.DRStatus(ctx)
		lastRaw = raw
		if err == nil && status.SecondaryState == "streaming" && status.LagEntries == 0 {
			if status.FlatAccumulatorFastPathTotal > beforeFastPath {
				fmt.Printf("%s optimized reconnect: flat accumulator fast path %d -> %d\n", label, beforeFastPath, status.FlatAccumulatorFastPathTotal)
				return status, raw, nil
			}
			if status.ReconcileCount == beforeReconcile {
				fmt.Printf("%s optimized reconnect: stream resumed without reconciliation\n", label)
				return status, raw, nil
			}
			return status, raw, fmt.Errorf("%s reconnected via scanned reconciliation: reconcile_count %d -> %d, flat_accumulator_fast_path_total %d -> %d", label, beforeReconcile, status.ReconcileCount, beforeFastPath, status.FlatAccumulatorFastPathTotal)
		}
		lastErr = err
		if time.Now().After(deadline) {
			return statusFromRaw(lastRaw), lastRaw, fmt.Errorf("timed out waiting for %s optimized reconnect: %w", label, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return nil, lastRaw, err
		}
	}
}

func statusFromRaw(raw []byte) *bao.DRStatus {
	var envelope struct {
		Data bao.DRStatus `json:"data"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	return &envelope.Data
}

func WaitRaftPeers(ctx context.Context, client *bao.Client, name string, expected int, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastRaw []byte
	var lastErr error
	for {
		config, raw, err := client.RaftConfiguration(ctx)
		lastRaw = raw
		if err == nil {
			servers := len(config.Data.Config.Servers)
			leaders := 0
			voters := 0
			for _, server := range config.Data.Config.Servers {
				if server.Leader {
					leaders++
				}
				if server.Voter || strings.EqualFold(server.Suffrage, "voter") {
					voters++
				}
			}
			if servers >= expected && leaders == 1 && voters >= expected {
				fmt.Printf("%s Raft ready: servers=%d voters=%d\n", name, servers, voters)
				return raw, nil
			}
			lastErr = fmt.Errorf("servers=%d leaders=%d voters=%d", servers, leaders, voters)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastRaw, fmt.Errorf("timed out waiting for %s Raft peers: %w", name, lastErr)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return lastRaw, err
		}
	}
}

func UnsealCluster(ctx context.Context, client *bao.Client, cluster Cluster, timeout time.Duration) error {
	if len(cluster.UnsealKeys) == 0 {
		return fmt.Errorf("%s has no unseal keys", cluster.Name)
	}
	for i, addr := range cluster.Addrs {
		name := fmt.Sprintf("%s-%d", cluster.Name, i+1)
		if len(cluster.Addrs) == 1 {
			name = cluster.Name
		}
		if err := waitUnsealNode(ctx, client, name, addr, cluster.UnsealKeys, timeout); err != nil {
			return err
		}
	}
	return nil
}

func waitUnsealNode(ctx context.Context, client *bao.Client, name, addr string, keys []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := client.SealStatus(ctx, addr)
		if err == nil {
			if !status.Sealed {
				return nil
			}
			fmt.Printf("Unsealing %s...\n", name)
			var lastErr error
			for _, key := range keys {
				if err := client.UnsealAt(ctx, addr, key); err == nil {
					return nil
				} else {
					lastErr = err
				}
			}
			return fmt.Errorf("failed to unseal %s with configured keys: %w", name, lastErr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s seal status: %w", name, err)
		}
		if err := sleepContext(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func ComposeStop(ctx context.Context, root, topology string, services ...string) ([]byte, error) {
	args := append([]string{"compose", "-f", composeFile(root, topology), "stop"}, services...)
	return runDocker(ctx, root, args...)
}

func ComposeStart(ctx context.Context, root, topology string, services ...string) ([]byte, error) {
	args := append([]string{"compose", "-f", composeFile(root, topology), "start"}, services...)
	return runDocker(ctx, root, args...)
}

func ComposeLogs(ctx context.Context, root, topology string, since time.Time, services ...string) ([]byte, error) {
	args := []string{"compose", "-f", composeFile(root, topology), "logs", "--no-color"}
	if !since.IsZero() {
		args = append(args, "--since", since.UTC().Format(time.RFC3339))
	}
	args = append(args, services...)
	return runDocker(ctx, root, args...)
}

func runDocker(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func composeFile(root, topology string) string {
	switch topology {
	case "ha":
		return filepath.Join(root, "docker-compose.dr-ha-test.yml")
	default:
		return filepath.Join(root, "docker-compose.dr-test.yml")
	}
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
