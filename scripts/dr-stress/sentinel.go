package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// SentinelResult records the convergence outcome for one secondary.
type SentinelResult struct {
	Role            string  `json:"role"`
	Converged       bool    `json:"converged"`
	ConvergeSeconds float64 `json:"converge_seconds"`
	Polls           int     `json:"polls"`
	FinalLag        int64   `json:"final_lag"`
	FinalApplied    int64   `json:"final_applied"`
	FinalPrimaryIdx int64   `json:"final_primary_index"`
}

// RunSentinel writes a sentinel key to the primary, then polls each
// configured secondary until lag_entries == 0 or last_applied_index >=
// primary_index, or the timeout is reached.
func RunSentinel(ctx context.Context, cfg *Config, clients *ClientSet) (s1, s2 SentinelResult) {
	sentinelPath := fmt.Sprintf("%s/%s/sentinel", cfg.KeyPrefix, cfg.RunID)

	// Write sentinel key.
	data := map[string]interface{}{
		"payload": fmt.Sprintf("sentinel-%s", cfg.RunID),
		"seq":     999999,
		"run_id":  cfg.RunID,
	}

	code, err := clients.Primary.KVPut(ctx, cfg.KVMount, sentinelPath, data)
	if err != nil || (code != 200 && code != 204) {
		log.Printf("[sentinel] failed to write sentinel key: code=%d err=%v", code, err)
		s1.Role = "secondary1"
		s1.ConvergeSeconds = -1
		s2.Role = "secondary2"
		s2.ConvergeSeconds = -1
		return
	}

	// Brief pause to let the write propagate.
	time.Sleep(2 * time.Second)
	log.Println("[sentinel] sentinel written, waiting for secondaries to converge")

	timeout := time.Duration(cfg.MaxWaitSeconds) * time.Second

	if clients.Secondary1 != nil {
		s1 = waitForConvergence(ctx, "secondary1", clients.Secondary1, timeout)
	} else {
		s1 = SentinelResult{Role: "secondary1", ConvergeSeconds: -1}
	}

	if clients.Secondary2 != nil {
		s2 = waitForConvergence(ctx, "secondary2", clients.Secondary2, timeout)
	} else {
		s2 = SentinelResult{Role: "secondary2", ConvergeSeconds: -1}
	}

	return
}

func waitForConvergence(ctx context.Context, role string, client *BaoClient, timeout time.Duration) SentinelResult {
	result := SentinelResult{Role: role}
	start := time.Now()
	deadline := start.Add(timeout)

	log.Printf("[sentinel] waiting for %s lag_entries=0 (timeout=%s)", role, timeout)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			result.ConvergeSeconds = -1
			return result
		case <-ticker.C:
		}

		result.Polls++

		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, _, err := client.DRStatus(pollCtx)
		cancel()

		if err != nil {
			if time.Now().After(deadline) {
				log.Printf("[sentinel] %s: TIMEOUT after %ds (%d polls, err=%v)",
					role, int(time.Since(start).Seconds()), result.Polls, err)
				result.ConvergeSeconds = -1
				return result
			}
			if result.Polls%15 == 0 {
				log.Printf("[sentinel] %s: poll error (%d polls, %ds)... %v",
					role, result.Polls, int(time.Since(start).Seconds()), err)
			}
			continue
		}

		result.FinalLag = status.LagEntries
		result.FinalApplied = status.LastAppliedIndex
		result.FinalPrimaryIdx = status.PrimaryIndex

		if status.LagEntries == 0 || (status.LastAppliedIndex >= status.PrimaryIndex && status.PrimaryIndex > 0) {
			elapsed := time.Since(start).Seconds()
			result.Converged = true
			result.ConvergeSeconds = elapsed
			log.Printf("[sentinel] %s: converged (lag=0, applied=%d primary=%d) after %.1fs (%d polls)",
				role, status.LastAppliedIndex, status.PrimaryIndex, elapsed, result.Polls)
			return result
		}

		if time.Now().After(deadline) {
			log.Printf("[sentinel] %s: TIMEOUT after %ds (%d polls, lag=%d applied=%d primary=%d)",
				role, int(time.Since(start).Seconds()), result.Polls,
				status.LagEntries, status.LastAppliedIndex, status.PrimaryIndex)
			result.ConvergeSeconds = -1
			return result
		}

		if result.Polls%15 == 0 {
			log.Printf("[sentinel] %s: lag=%d applied=%d primary=%d (%ds, %d polls)...",
				role, status.LagEntries, status.LastAppliedIndex, status.PrimaryIndex,
				int(time.Since(start).Seconds()), result.Polls)
		}
	}
}
