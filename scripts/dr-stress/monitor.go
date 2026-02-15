package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// StatusSnapshot is a single timeline sample of all nodes' DR status.
type StatusSnapshot struct {
	TS         time.Time         `json:"ts"`
	Primary    *DRStatusResponse `json:"primary,omitempty"`
	Secondary1 *DRStatusResponse `json:"secondary1,omitempty"`
	Secondary2 *DRStatusResponse `json:"secondary2,omitempty"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// Monitor polls DR status on all configured nodes at a fixed interval
// and writes NDJSON to the timeline file.  It also exposes the latest
// snapshot for the progress reporter.
type Monitor struct {
	clients  *ClientSet
	interval time.Duration
	path     string

	mu     sync.RWMutex
	latest StatusSnapshot
}

// NewMonitor creates a Monitor.
func NewMonitor(clients *ClientSet, interval time.Duration, path string) *Monitor {
	return &Monitor{
		clients:  clients,
		interval: interval,
		path:     path,
	}
}

// Latest returns a copy of the most recent status snapshot.
func (m *Monitor) Latest() StatusSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latest
}

// Run polls until ctx is cancelled, writing each snapshot as NDJSON.
func (m *Monitor) Run(ctx context.Context) error {
	f, err := os.Create(m.path)
	if err != nil {
		return fmt.Errorf("create timeline file: %w", err)
	}
	defer f.Close()

	w := bufio.NewWriterSize(f, 64*1024)
	enc := json.NewEncoder(w)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.Flush()
			return nil
		case <-ticker.C:
			snap := m.poll(ctx)
			m.mu.Lock()
			m.latest = snap
			m.mu.Unlock()

			enc.Encode(snap)
			w.Flush()
		}
	}
}

func (m *Monitor) poll(ctx context.Context) StatusSnapshot {
	snap := StatusSnapshot{
		TS:     time.Now().UTC(),
		Errors: make(map[string]string),
	}

	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if m.clients.Primary != nil {
		status, _, err := m.clients.Primary.DRStatus(pollCtx)
		if err != nil {
			snap.Errors["primary"] = err.Error()
		} else {
			snap.Primary = status
		}
	}

	if m.clients.Secondary1 != nil {
		status, _, err := m.clients.Secondary1.DRStatus(pollCtx)
		if err != nil {
			snap.Errors["secondary1"] = err.Error()
		} else {
			snap.Secondary1 = status
		}
	}

	if m.clients.Secondary2 != nil {
		status, _, err := m.clients.Secondary2.DRStatus(pollCtx)
		if err != nil {
			snap.Errors["secondary2"] = err.Error()
		} else {
			snap.Secondary2 = status
		}
	}

	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}

	return snap
}
