// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
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

	loopCtx, cancel := context.WithCancel(context.Background())
	m.secondaryLoopCancel = cancel

	primaryAddr := m.config.PrimaryAddr
	secondary := m.secondary

	go func() {
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
				time.Sleep(backoff + jitter)
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
				m.logger.Warn("DR secondary replication loop exited; reconnecting",
					"error", err)
				jitter := time.Duration(randIntn(int(backoff / 5)))
				time.Sleep(backoff + jitter)
			}
		}
	}()
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
