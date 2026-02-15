// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	metrics "github.com/hashicorp/go-metrics/compat"
)

func (m *drRelationshipManager) startSecondaryControllerLocked() {
	if m.secondary == nil || m.config.PrimaryAddr == "" {
		return
	}
	if m.secondaryLoopCancel != nil {
		return
	}

	// Derive the controller context from the core's activeContext so that
	// the loop terminates automatically when this node steps down or is
	// sealed (activeContext cancelled). The explicit cancel() is still
	// used by DisableSecondary / Promote to stop the controller on demand.
	loopCtx, cancel := context.WithCancel(m.core.activeContext)
	m.secondaryLoopCancel = cancel

	primaryAddr := m.config.PrimaryAddr
	secondary := m.secondary

	go func() {
		defer func() {
			// Clear secondaryLoopCancel so that a subsequent call to
			// startSecondaryControllerLocked (e.g. after re-acquiring
			// leadership) does not think the controller is still running.
			m.mu.Lock()
			m.secondaryLoopCancel = nil
			m.mu.Unlock()
		}()

		backoff := 500 * time.Millisecond
		const maxBackoff = 30 * time.Second

		for {
			select {
			case <-loopCtx.Done():
				return
			default:
			}

			if err := secondary.Connect(loopCtx, primaryAddr); err != nil {
				secondary.connectRetries.Add(1)
				secondary.connectFailures.Add(1)
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "connect_retries"}, 1)
				metrics.IncrCounter([]string{"replication", "dr", "secondary", "connect_failures"}, 1)
				metrics.SetGauge([]string{"replication", "dr", "secondary", "connect_backoff_seconds"}, float32(backoff.Seconds()))
				m.logger.Warn("DR secondary connect failed; retrying",
					"primary_addr", primaryAddr,
					"backoff", backoff,
					"error", err)
				jitter := time.Duration(randIntn(int(backoff / 5)))
				if !sleepCtx(loopCtx, backoff+jitter) {
					return // context cancelled during backoff
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			backoff = 500 * time.Millisecond
			metrics.SetGauge([]string{"replication", "dr", "secondary", "connect_backoff_seconds"}, float32(backoff.Seconds()))
			err := secondary.Start(loopCtx)
			if err != nil && loopCtx.Err() == nil {
				// If Start() returned a redirect, update the target address
				// so the next Connect() goes to the actual leader.
				var redirect *errDRRedirect
				if errors.As(err, &redirect) && redirect.LeaderAddr != "" {
					m.logger.Info("DR secondary redirected to new leader",
						"old_addr", primaryAddr,
						"new_addr", redirect.LeaderAddr)
					primaryAddr = redirect.LeaderAddr
					// Reconnect immediately without backoff.
					continue
				}

				m.logger.Warn("DR secondary replication loop exited; reconnecting",
					"error", err)
				jitter := time.Duration(randIntn(int(backoff / 5)))
				if !sleepCtx(loopCtx, backoff+jitter) {
					return // context cancelled during backoff
				}
			}
		}
	}()
}

// sleepCtx blocks for d or until ctx is cancelled. Returns true if the
// full duration elapsed, false if the context was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	// crypto/rand already imported in this file.
	b := make([]byte, 1)
	if _, err := rand.Read(b); err != nil {
		return 0
	}
	return int(b[0]) % n
}
