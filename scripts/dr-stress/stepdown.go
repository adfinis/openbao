package main

import (
	"context"
	"log"
	"time"
)

// RunStepdownLoop periodically requests a leader stepdown on the primary
// to exercise failover behavior during the workload.  Exits when ctx is
// cancelled.  interval <= 0 means no stepdowns.
func RunStepdownLoop(ctx context.Context, client *BaoClient, interval time.Duration) {
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
			if err := client.StepDown(ctx); err != nil {
				log.Printf("[stepdown] step-down failed: %v", err)
			} else {
				log.Println("[stepdown] requested primary step-down")
			}
		}
	}
}
