package main

import (
	"context"
	"log"
	"time"
)

// RunStepdownLoop periodically requests a leader stepdown on the primary
// to exercise failover behavior during the workload. Exits when ctx is
// cancelled. interval <= 0 means no stepdowns.
func RunStepdownLoop(ctx context.Context, client *BaoClient, interval time.Duration, rec *Recorder) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start := time.Now()
			code, err := client.StepDown(ctx)
			ev := Event{
				TS:        start.UTC(),
				Op:        "stepdown",
				Role:      "primary",
				LatencyNS: time.Since(start).Nanoseconds(),
				Code:      code,
				Control:   true,
			}
			if err != nil {
				ev.Err = classifyError(err)
			}
			if rec != nil {
				rec.SendControl(ev)
			}
			if err != nil {
				log.Printf("[stepdown] step-down failed: %v", err)
			} else {
				log.Println("[stepdown] requested primary step-down")
			}
		}
	}
}
