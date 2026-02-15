package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func verifyMode(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)

	addr := fs.String("addr", "", "BAO_ADDR of the target node (primary or promoted secondary)")
	token := fs.String("token", "", "BAO_TOKEN for the target node")
	caCert := fs.String("cacert", "", "CA cert path")
	tlsServerName := fs.String("tls-server-name", "", "TLS server name")
	skipVerify := fs.Bool("skip-verify", false, "Skip TLS verification")
	kvMount := fs.String("kv-mount", "kv", "KV v2 mount path")
	runDir := fs.String("run-dir", "", "Path to a run directory (containing writes.ndjson)")
	sampleCount := fs.Int("sample", 100, "Number of keys to sample for verification")
	jsonOut := fs.Bool("json", false, "Output results as JSON")
	httpTimeout := fs.Int("http-timeout", 30, "HTTP timeout in seconds")

	fs.Parse(args)

	if *addr == "" || *token == "" {
		log.Fatal("--addr and --token are required for verify mode")
	}
	if *runDir == "" {
		log.Fatal("--run-dir is required for verify mode")
	}

	node := NodeConfig{
		Addr:          *addr,
		Token:         *token,
		CACert:        *caCert,
		TLSServerName: *tlsServerName,
		SkipVerify:    *skipVerify,
	}
	cfg := &Config{
		HTTPTimeout:    time.Duration(*httpTimeout) * time.Second,
		MaxIdleConns:   20,
		MaxIdlePerHost: 20,
	}

	client, err := NewBaoClient(node, cfg)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}

	// Read run_id from config.json if available.
	runIDFromConfig := ""
	configPath := filepath.Join(*runDir, "config.json")
	if cfgData, err := os.ReadFile(configPath); err == nil {
		var cfgSnap struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(cfgData, &cfgSnap) == nil {
			runIDFromConfig = cfgSnap.RunID
		}
	}

	// Read writes.ndjson and build truth map.
	writesPath := filepath.Join(*runDir, "writes.ndjson")
	truthMap := buildTruthMap(writesPath, *kvMount, runIDFromConfig)
	if len(truthMap) == 0 {
		log.Fatal("no writes found in truth log")
	}

	log.Printf("loaded %d unique keys from truth log", len(truthMap))

	// Sample keys.
	keys := sampleKeys(truthMap, *sampleCount)
	log.Printf("verifying %d sampled keys", len(keys))

	// Verify each key.
	var matches, mismatches, missing, errors int
	type mismatchDetail struct {
		Key         string `json:"key"`
		ExpectedSeq uint64 `json:"expected_seq"`
		ActualSeq   string `json:"actual_seq"`
		ActualRunID string `json:"actual_run_id"`
		Reason      string `json:"reason"`
	}
	var details []mismatchDetail

	for _, key := range keys {
		expected := truthMap[key]

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		code, resp, err := client.KVGet(ctx, *kvMount, key)
		cancel()

		if err != nil {
			errors++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq,
				Reason:      fmt.Sprintf("error: %v", err),
			})
			continue
		}

		if code == 404 || resp == nil || resp.Data == nil || resp.Data.Data == nil {
			missing++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq,
				Reason:      "key not found",
			})
			continue
		}

		data := resp.Data.Data

		// Check seq.
		var actualSeq uint64
		switch v := data["seq"].(type) {
		case float64:
			actualSeq = uint64(v)
		case json.Number:
			n, _ := v.Int64()
			actualSeq = uint64(n)
		}

		actualRunID, _ := data["run_id"].(string)

		seqOK := actualSeq == expected.seq
		runIDOK := expected.runID == "" || actualRunID == expected.runID

		if seqOK && runIDOK {
			matches++
		} else {
			mismatches++
			d := mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq,
				ActualSeq:   fmt.Sprintf("%d", actualSeq),
				ActualRunID: actualRunID,
			}
			if !seqOK {
				d.Reason = fmt.Sprintf("seq mismatch: expected %d got %d", expected.seq, actualSeq)
			} else {
				d.Reason = fmt.Sprintf("run_id mismatch: expected %s got %s", expected.runID, actualRunID)
			}
			details = append(details, d)
		}
	}

	result := map[string]interface{}{
		"total_keys_in_truth": len(truthMap),
		"sampled":             len(keys),
		"matches":             matches,
		"mismatches":          mismatches,
		"missing":             missing,
		"errors":              errors,
		"pass":                mismatches == 0 && missing == 0 && errors == 0,
	}
	if len(details) > 0 {
		result["details"] = details
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	} else {
		fmt.Printf("\n=== Verify Results ===\n")
		fmt.Printf("Truth log keys: %d\n", len(truthMap))
		fmt.Printf("Sampled:        %d\n", len(keys))
		fmt.Printf("Matches:        %d\n", matches)
		fmt.Printf("Mismatches:     %d\n", mismatches)
		fmt.Printf("Missing:        %d\n", missing)
		fmt.Printf("Errors:         %d\n", errors)

		pass := mismatches == 0 && missing == 0 && errors == 0
		if pass {
			fmt.Println("Result:         PASS")
		} else {
			fmt.Println("Result:         FAIL")
			for _, d := range details {
				fmt.Printf("  %s: %s\n", d.Key, d.Reason)
			}
		}
		fmt.Println()
	}

	if mismatches > 0 || missing > 0 || errors > 0 {
		os.Exit(1)
	}
}

type truthEntry struct {
	seq   uint64
	runID string
}

// buildTruthMap reads writes.ndjson and builds a map from KV API path
// (without the mount prefix) to the highest seq for that key.
// The event Key field is stored as <mount>/<prefix>/<run_id>/k<N>.
// We strip the mount prefix so the map keys are the path to pass to KVGet.
func buildTruthMap(writesPath, kvMount, runID string) map[string]truthEntry {
	f, err := os.Open(writesPath)
	if err != nil {
		log.Printf("cannot open writes file: %v", err)
		return nil
	}
	defer f.Close()

	mountPrefix := kvMount + "/"
	truth := make(map[string]truthEntry)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Key == "" || ev.Seq == 0 {
			continue
		}

		// Strip mount prefix to get the KV API path.
		apiPath := ev.Key
		if strings.HasPrefix(apiPath, mountPrefix) {
			apiPath = strings.TrimPrefix(apiPath, mountPrefix)
		}

		// Keep the highest seq per key (last writer wins).
		existing, exists := truth[apiPath]
		if !exists || ev.Seq > existing.seq {
			truth[apiPath] = truthEntry{seq: ev.Seq, runID: runID}
		}
	}

	return truth
}

func sampleKeys(truth map[string]truthEntry, n int) []string {
	keys := make([]string, 0, len(truth))
	for k := range truth {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if n >= len(keys) {
		return keys
	}

	// Deterministic uniform sampling.
	step := len(keys) / n
	if step < 1 {
		step = 1
	}
	sampled := make([]string, 0, n)
	for i := 0; i < len(keys) && len(sampled) < n; i += step {
		sampled = append(sampled, keys[i])
	}
	return sampled
}
