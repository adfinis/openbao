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

	code, err, attempts, elapsed := writeSentinelWithRetry(ctx, cfg, clients.Primary, sentinelPath, data)
	if err != nil || (code != 200 && code != 204) {
		log.Printf("[sentinel] failed to write sentinel key after %d attempts in %s: code=%d err=%v", attempts, elapsed.Round(time.Millisecond), code, err)
		s1.Role = "secondary1"
		s1.ConvergeSeconds = -1
		s2.Role = "secondary2"
		s2.ConvergeSeconds = -1
		return
	}
	if attempts > 1 {
		log.Printf("[sentinel] sentinel write succeeded after %d attempts in %s", attempts, elapsed.Round(time.Millisecond))
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

func writeSentinelWithRetry(ctx context.Context, cfg *Config, client *BaoClient, sentinelPath string, data map[string]interface{}) (code int, err error, attempts int, elapsed time.Duration) {
	start := time.Now()
	writeCtx, cancel := context.WithTimeout(ctx, cfg.SentinelWriteTimeout)
	defer cancel()

	for {
		attempts++
		code, err = client.KVPut(writeCtx, cfg.KVMount, sentinelPath, data)
		elapsed = time.Since(start)
		if err == nil && (code == 200 || code == 204) {
			return code, nil, attempts, elapsed
		}
		if !sentinelWriteRetriable(code, err) || writeCtx.Err() != nil {
			return code, err, attempts, elapsed
		}

		if attempts == 1 || attempts%10 == 0 {
			log.Printf("[sentinel] sentinel write retrying after transient failure: attempt=%d code=%d err=%v", attempts, code, err)
		}

		timer := time.NewTimer(cfg.SentinelWriteRetryInterval)
		select {
		case <-writeCtx.Done():
			timer.Stop()
			elapsed = time.Since(start)
			return code, err, attempts, elapsed
		case <-timer.C:
		}
	}
}

func sentinelWriteRetriable(code int, err error) bool {
	if err != nil {
		return true
	}
	switch code {
	case 0, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
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

		if statusConverged(status) {
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

func statusConverged(status *DRStatusResponse) bool {
	return status != nil && status.PrimaryIndex > 0 && status.LastAppliedIndex >= status.PrimaryIndex && status.LagEntries == 0
}
