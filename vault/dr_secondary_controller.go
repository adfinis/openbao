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

type drPrimaryAddrRing struct {
	candidates []string
	index      map[string]int
	current    int
	failures   int
}

func newDRPrimaryAddrRing(addrs []string) *drPrimaryAddrRing {
	normalized := normalizePrimaryAddrs(addrs)
	r := &drPrimaryAddrRing{
		candidates: normalized,
		index:      make(map[string]int, len(normalized)),
	}
	for i, addr := range normalized {
		r.index[addr] = i
	}
	return r
}

func (r *drPrimaryAddrRing) Size() int {
	return len(r.candidates)
}

func (r *drPrimaryAddrRing) Current() string {
	if len(r.candidates) == 0 {
		return ""
	}
	return r.candidates[r.current]
}

func (r *drPrimaryAddrRing) MarkSuccess() {
	r.failures = 0
}

func (r *drPrimaryAddrRing) Add(addr string) (string, bool) {
	addr = normalizePrimaryAddr(addr)
	if addr == "" {
		return "", false
	}
	if _, ok := r.index[addr]; ok {
		return addr, false
	}
	r.candidates = append(r.candidates, addr)
	r.index[addr] = len(r.candidates) - 1
	return addr, true
}

func (r *drPrimaryAddrRing) Use(addr string) bool {
	addr = normalizePrimaryAddr(addr)
	if addr == "" {
		return false
	}
	idx, ok := r.index[addr]
	if !ok {
		return false
	}
	r.current = idx
	return true
}

func (r *drPrimaryAddrRing) RotateFailure() (oldAddr, nextAddr string, rotated bool, cycleComplete bool) {
	if len(r.candidates) == 0 {
		return "", "", false, true
	}
	oldAddr = r.candidates[r.current]
	r.failures++
	cycleComplete = (r.failures % len(r.candidates)) == 0
	if len(r.candidates) > 1 {
		r.current = (r.current + 1) % len(r.candidates)
		rotated = true
	}
	nextAddr = r.candidates[r.current]
	return oldAddr, nextAddr, rotated, cycleComplete
}

func (m *drRelationshipManager) startSecondaryControllerLocked() {
	initialPrimaryAddrs := normalizePrimaryAddrs(m.config.PrimaryAddrs)
	if m.secondary == nil || len(initialPrimaryAddrs) == 0 {
		return
	}
	if !m.shouldRunSecondaryControllerLocked() {
		m.logger.Debug("skipping DR secondary controller start on standby node")
		return
	}
	if m.secondaryLoopCancel != nil {
		return
	}

	// Derive the controller context from the core's activeContext so that
	// the loop terminates automatically when this node steps down or is
	// sealed (activeContext cancelled). The explicit cancel() is still
	// used by DisableSecondary / Promote to stop the controller on demand.
	loopCtx, cancel := context.WithCancel(m.core.activeContext.Load())
	m.secondaryLoopCancel = cancel

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
		addrRing := newDRPrimaryAddrRing(initialPrimaryAddrs)
		if addrRing.Size() == 0 {
			m.logger.Warn("DR secondary controller has no primary addresses; exiting")
			return
		}
		primaryAddr := addrRing.Current()

		// Redirect rate limiting: track timestamps to enforce max
		// redirects per sliding window.
		const drMaxRedirectsPerMinute = 10
		const redirectWindow = 60 * time.Second
		redirectTimestamps := make([]time.Time, 0, drMaxRedirectsPerMinute)
		redirectBackoff := time.Duration(0)

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
				m.logger.Warn("DR secondary connect failed; retrying",
					"primary_addr", primaryAddr,
					"error", err)

				oldAddr, nextAddr, rotated, cycleComplete := addrRing.RotateFailure()
				primaryAddr = nextAddr
				if rotated {
					m.logger.Info("DR secondary rotating to next primary candidate after connect failure",
						"old_addr", oldAddr,
						"new_addr", nextAddr)
				}

				if cycleComplete {
					metrics.SetGauge([]string{"replication", "dr", "secondary", "connect_backoff_seconds"}, float32(backoff.Seconds()))
					jitter := time.Duration(randIntn(int(backoff / 5)))
					if !sleepCtx(loopCtx, backoff+jitter) {
						return // context cancelled during backoff
					}
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				} else {
					metrics.SetGauge([]string{"replication", "dr", "secondary", "connect_backoff_seconds"}, 0)
				}
				continue
			}

			addrRing.MarkSuccess()
			backoff = 500 * time.Millisecond
			metrics.SetGauge([]string{"replication", "dr", "secondary", "connect_backoff_seconds"}, float32(backoff.Seconds()))
			err := secondary.Start(loopCtx)
			if err != nil && loopCtx.Err() == nil {
				// If Start() returned a redirect, update the target address
				// so the next Connect() goes to the actual leader.
				var redirect *errDRRedirect
				if errors.As(err, &redirect) && redirect.LeaderAddr != "" {
					// Enforce redirect rate limit: if we've had too many
					// redirects in the sliding window, apply exponential backoff.
					now := time.Now()
					cutoff := now.Add(-redirectWindow)
					filtered := redirectTimestamps[:0]
					for _, ts := range redirectTimestamps {
						if ts.After(cutoff) {
							filtered = append(filtered, ts)
						}
					}
					redirectTimestamps = append(filtered, now)

					if len(redirectTimestamps) > drMaxRedirectsPerMinute {
						if redirectBackoff == 0 {
							redirectBackoff = 2 * time.Second
						} else {
							redirectBackoff *= 2
						}
						if redirectBackoff > maxBackoff {
							redirectBackoff = maxBackoff
						}
						m.logger.Warn("redirect rate limit exceeded; applying backoff",
							"redirects_in_window", len(redirectTimestamps),
							"backoff", redirectBackoff)
						if !sleepCtx(loopCtx, redirectBackoff) {
							return
						}
					} else {
						redirectBackoff = 0
					}

					hintedAddr, added := addrRing.Add(redirect.LeaderAddr)
					if hintedAddr != "" && added {
						m.logger.Info("DR secondary discovered new primary candidate from redirect",
							"addr", hintedAddr)
					}
					if hintedAddr != "" {
						oldAddr := primaryAddr
						addrRing.Use(hintedAddr)
						primaryAddr = addrRing.Current()
						if oldAddr != primaryAddr {
							m.logger.Info("DR secondary redirected to new leader",
								"old_addr", oldAddr,
								"new_addr", primaryAddr)
						}
					}

					addrRing.MarkSuccess()
					// Reconnect (rate-limited above if needed).
					continue
				}

				m.logger.Warn("DR secondary replication loop exited; reconnecting",
					"error", err)

				if isDRTransportReconnectError(err) {
					oldAddr, nextAddr, rotated, cycleComplete := addrRing.RotateFailure()
					primaryAddr = nextAddr
					if rotated {
						m.logger.Info("DR secondary rotating to next primary candidate after transport failure",
							"old_addr", oldAddr,
							"new_addr", nextAddr)
					}
					if cycleComplete {
						jitter := time.Duration(randIntn(int(backoff / 5)))
						if !sleepCtx(loopCtx, backoff+jitter) {
							return // context cancelled during backoff
						}
						backoff *= 2
						if backoff > maxBackoff {
							backoff = maxBackoff
						}
					}
					continue
				}

				addrRing.MarkSuccess()
				jitter := time.Duration(randIntn(int(backoff / 5)))
				if !sleepCtx(loopCtx, backoff+jitter) {
					return // context cancelled during backoff
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()
}

func (m *drRelationshipManager) shouldRunSecondaryControllerLocked() bool {
	if m == nil || m.core == nil || m.secondary == nil {
		return false
	}

	// During HA standby read-only unseal, core.standby is true and we do not
	// hold the HA lock. In that state DR streaming/bootstrap must not run on
	// the node.
	if m.core.standby.Load() && m.core.heldHALock == nil {
		return false
	}

	return true
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
