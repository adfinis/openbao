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

	// Build the truth map from events.ndjson when available. Timeout/cancelled
	// client writes may still commit server-side, so writes.ndjson alone is not
	// enough to determine post-run correctness.
	truthPath := filepath.Join(*runDir, "events.ndjson")
	if _, err := os.Stat(truthPath); err != nil {
		truthPath = filepath.Join(*runDir, "writes.ndjson")
	}
	truthMap := buildTruthMap(truthPath, *kvMount, runIDFromConfig)
	if len(truthMap) == 0 {
		log.Fatal("no writes found in truth log")
	}

	log.Printf("loaded %d unique keys from truth log", len(truthMap))

	// Sample keys.
	keys := sampleKeys(truthMap, *sampleCount)
	log.Printf("verifying %d sampled keys", len(keys))

	// Verify each key.
	var matches, mismatches, missing, uncertainMissing, errors int
	type mismatchDetail struct {
		Key         string   `json:"key"`
		ExpectedSeq uint64   `json:"expected_seq,omitempty"`
		AllowedSeqs []uint64 `json:"allowed_seqs,omitempty"`
		ActualSeq   string   `json:"actual_seq,omitempty"`
		ActualRunID string   `json:"actual_run_id,omitempty"`
		Reason      string   `json:"reason"`
	}
	var details []mismatchDetail
	var ambiguousSampled int

	for _, key := range keys {
		expected := truthMap[key]
		if len(expected.allowedSeqs) > 1 {
			ambiguousSampled++
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		code, resp, err := client.KVGet(ctx, *kvMount, key)
		cancel()

		if err != nil {
			errors++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq(),
				AllowedSeqs: expected.allowedSeqs,
				Reason:      fmt.Sprintf("error: %v", err),
			})
			continue
		}

		if code == 404 {
			if hasNoHandlerError(resp) {
				errors++
				details = append(details, mismatchDetail{
					Key:         key,
					ExpectedSeq: expected.seq(),
					AllowedSeqs: expected.allowedSeqs,
					Reason:      fmt.Sprintf("route not found: %s", strings.Join(resp.Errors, "; ")),
				})
				continue
			}
			if expected.allowMissing {
				uncertainMissing++
				continue
			}
			missing++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq(),
				AllowedSeqs: expected.allowedSeqs,
				Reason:      "key not found",
			})
			continue
		}
		if code != 200 {
			errors++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq(),
				AllowedSeqs: expected.allowedSeqs,
				Reason:      fmt.Sprintf("unexpected HTTP status: %d", code),
			})
			continue
		}
		if resp == nil || resp.Data == nil || resp.Data.Data == nil {
			errors++
			details = append(details, mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq(),
				AllowedSeqs: expected.allowedSeqs,
				Reason:      "malformed KV response: missing data",
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

		seqOK := expected.allowsSeq(actualSeq)
		runIDOK := expected.runID == "" || actualRunID == expected.runID

		if seqOK && runIDOK {
			matches++
		} else {
			mismatches++
			d := mismatchDetail{
				Key:         key,
				ExpectedSeq: expected.seq(),
				AllowedSeqs: expected.allowedSeqs,
				ActualSeq:   fmt.Sprintf("%d", actualSeq),
				ActualRunID: actualRunID,
			}
			if !seqOK {
				if len(expected.allowedSeqs) > 1 {
					d.Reason = fmt.Sprintf("seq mismatch: expected one of %v got %d", expected.allowedSeqs, actualSeq)
				} else {
					d.Reason = fmt.Sprintf("seq mismatch: expected %d got %d", expected.seq(), actualSeq)
				}
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
		"uncertain_missing":   uncertainMissing,
		"errors":              errors,
		"ambiguous_sampled":   ambiguousSampled,
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
		fmt.Printf("Uncertain miss: %d\n", uncertainMissing)
		fmt.Printf("Errors:         %d\n", errors)
		fmt.Printf("Ambiguous:      %d\n", ambiguousSampled)

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
	allowedSeqs  []uint64
	runID        string
	allowMissing bool
}

func (e truthEntry) seq() uint64 {
	if len(e.allowedSeqs) == 0 {
		return 0
	}
	return e.allowedSeqs[0]
}

func (e truthEntry) allowsSeq(seq uint64) bool {
	for _, allowed := range e.allowedSeqs {
		if seq == allowed {
			return true
		}
	}
	return false
}

type truthWrite struct {
	seq       uint64
	start     time.Time
	end       time.Time
	confirmed bool
}

// buildTruthMap reads events.ndjson or writes.ndjson and builds a map from KV API path
// (without the mount prefix) to the possible final seq values for that key.
// The event Key field is stored as <mount>/<prefix>/<run_id>/k<N>.
// We strip the mount prefix so the map keys are the path to pass to KVGet.
//
// The stress workload writes hot keys concurrently from many workers. The seq
// field is per-worker, so "highest seq" is not a valid last-writer rule. Use
// the client-visible operation intervals instead: a write is a possible final
// value only if no confirmed write to that key started after it completed. If
// exactly one candidate remains, the final value is deterministic; if multiple
// overlapping or commit-uncertain tail writes remain, any candidate is
// linearizable.
func buildTruthMap(truthPath, kvMount, runID string) map[string]truthEntry {
	f, err := os.Open(truthPath)
	if err != nil {
		log.Printf("cannot open writes file: %v", err)
		return nil
	}
	defer f.Close()

	mountPrefix := kvMount + "/"
	writesByKey := make(map[string][]truthWrite)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Op != "" && ev.Op != "put" {
			continue
		}
		confirmed := ev.Err == "" && (ev.Code == 200 || ev.Code == 204)
		if !confirmed && !isCommitUncertainPut(ev) {
			continue
		}
		if ev.Key == "" || ev.Seq == 0 || ev.TS.IsZero() {
			continue
		}

		// Strip mount prefix to get the KV API path.
		apiPath := ev.Key
		if strings.HasPrefix(apiPath, mountPrefix) {
			apiPath = strings.TrimPrefix(apiPath, mountPrefix)
		}

		writesByKey[apiPath] = append(writesByKey[apiPath], truthWrite{
			seq:       ev.Seq,
			start:     ev.TS,
			end:       ev.TS.Add(time.Duration(ev.LatencyNS)),
			confirmed: confirmed,
		})
	}

	truth := make(map[string]truthEntry, len(writesByKey))
	for key, writes := range writesByKey {
		allowed := terminalWriteSeqs(writes)
		if len(allowed) == 0 {
			continue
		}
		truth[key] = truthEntry{allowedSeqs: allowed, runID: runID, allowMissing: !hasConfirmedWrite(writes)}
	}
	return truth
}

func hasConfirmedWrite(writes []truthWrite) bool {
	for _, write := range writes {
		if write.confirmed {
			return true
		}
	}
	return false
}

func terminalWriteSeqs(writes []truthWrite) []uint64 {
	seqs := make([]uint64, 0, len(writes))
	seen := make(map[uint64]struct{}, len(writes))
	for i, write := range writes {
		overwritten := false
		for j, other := range writes {
			if i == j {
				continue
			}
			if other.confirmed && other.start.After(write.end) {
				overwritten = true
				break
			}
		}
		if overwritten {
			continue
		}
		if _, ok := seen[write.seq]; ok {
			continue
		}
		seen[write.seq] = struct{}{}
		seqs = append(seqs, write.seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func isCommitUncertainPut(ev Event) bool {
	if ev.Op != "" && ev.Op != "put" {
		return false
	}
	if ev.Code != 0 || ev.Err == "" {
		return false
	}
	switch ev.Err {
	case "ctx_deadline", "ctx_canceled", "timeout", "eof", "conn_reset":
		return true
	default:
		return false
	}
}

func hasNoHandlerError(resp *KVGetResponse) bool {
	if resp == nil {
		return false
	}
	for _, errText := range resp.Errors {
		if strings.Contains(errText, "no handler for route") || strings.Contains(errText, "route entry not found") {
			return true
		}
	}
	return false
}

func sampleKeys(truth map[string]truthEntry, n int) []string {
	keys := make([]string, 0, len(truth))
	for k := range truth {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if n <= 0 || n >= len(keys) {
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
