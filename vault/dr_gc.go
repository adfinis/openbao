// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
)

const (
	// DefaultGCInterval is how often the tombstone GC runs.
	DefaultGCInterval = 60 * time.Second

	// MaxTombstoneAge is the maximum age of a tombstone before it is
	// eligible for removal, regardless of secondary acknowledgement.
	// Secondaries that have been disconnected longer than this must
	// perform a full resnapshot on reconnect.
	MaxTombstoneAge = 7 * 24 * time.Hour // 7 days

	// drGCDisconnectedThreshold is how long a secondary can be absent
	// from heartbeat pressure samples before it is considered
	// disconnected for GC purposes.
	drGCDisconnectedThreshold = 5 * time.Minute
)

// drTombstoneGC manages epoch-based garbage collection of tombstone
// entries on the primary. It computes a global low watermark from the
// minimum lastAppliedIndex across all active secondary relationships,
// then prunes stream journal segments and other tombstone tracking
// structures that are safely below the watermark.
type drTombstoneGC struct {
	logger  log.Logger
	primary *drReplicationPrimary

	interval        time.Duration
	maxTombstoneAge time.Duration

	// stopCh signals the GC goroutine to exit.
	stopCh chan struct{}
	wg     sync.WaitGroup

	// Telemetry counters.
	gcRuns                atomic.Uint64
	tombstonesPruned      atomic.Uint64
	journalSegmentsPruned atomic.Uint64
	lowWatermark          atomic.Uint64
	disconnectedPeers     atomic.Uint64
}

// newDRTombstoneGC creates a new tombstone GC manager for the given primary.
func newDRTombstoneGC(primary *drReplicationPrimary, logger log.Logger) *drTombstoneGC {
	if logger == nil {
		logger = log.NewNullLogger()
	}
	return &drTombstoneGC{
		logger:          logger.Named("dr-gc"),
		primary:         primary,
		interval:        DefaultGCInterval,
		maxTombstoneAge: MaxTombstoneAge,
		stopCh:          make(chan struct{}),
	}
}

// Start begins the periodic GC goroutine.
func (gc *drTombstoneGC) Start() {
	gc.wg.Add(1)
	go gc.run()
}

// Stop signals the GC goroutine to exit and waits for it to finish.
func (gc *drTombstoneGC) Stop() {
	close(gc.stopCh)
	gc.wg.Wait()
}

func (gc *drTombstoneGC) run() {
	defer gc.wg.Done()

	ticker := time.NewTicker(gc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-gc.stopCh:
			return
		case <-ticker.C:
			gc.runOnce()
		}
	}
}

// runOnce executes a single GC cycle.
func (gc *drTombstoneGC) runOnce() {
	gc.gcRuns.Add(1)
	now := time.Now()

	watermark, disconnected := gc.computeGlobalLowWatermark(now)
	gc.lowWatermark.Store(watermark)
	gc.disconnectedPeers.Store(uint64(disconnected))

	if watermark > 0 {
		gc.pruneStreamJournal(watermark, now)
	}

	gc.logger.Debug("GC cycle complete",
		"watermark", watermark,
		"disconnected_peers", disconnected,
		"gc_runs", gc.gcRuns.Load(),
		"journal_segments_pruned", gc.journalSegmentsPruned.Load(),
	)
}

// computeGlobalLowWatermark returns the minimum lastAppliedIndex across
// all recently-active secondary relationships. Secondaries that have
// not heartbeated within drGCDisconnectedThreshold are considered
// disconnected and excluded from the watermark (they will need to
// resnapshot). The second return value is the number of disconnected
// peers found.
func (gc *drTombstoneGC) computeGlobalLowWatermark(now time.Time) (uint64, int) {
	gc.primary.pressureMu.Lock()
	defer gc.primary.pressureMu.Unlock()

	var minApplied uint64
	active := 0
	disconnected := 0

	for _, sample := range gc.primary.secondaryPressure {
		if sample == nil {
			continue
		}
		if now.Sub(sample.lastSeen) > drGCDisconnectedThreshold {
			disconnected++
			continue
		}
		active++
		if minApplied == 0 || sample.lastApplied < minApplied {
			minApplied = sample.lastApplied
		}
	}

	if active == 0 {
		// No active secondaries: use the primary's current applied index
		// as the watermark so we can clean up everything.
		return gc.primary.indexApplied.Load(), disconnected
	}

	return minApplied, disconnected
}

// pruneStreamJournal removes stream journal segments whose entries are
// all below the global low watermark and older than MaxTombstoneAge.
func (gc *drTombstoneGC) pruneStreamJournal(watermark uint64, now time.Time) {
	journal := gc.primary.streamJournal
	if journal == nil {
		return
	}

	journal.mu.Lock()
	defer journal.mu.Unlock()

	pruned := 0
	remaining := make([]*drStreamJournalSegment, 0, len(journal.segments))
	for _, seg := range journal.segments {
		// Only prune if the segment's last index is below the watermark
		// AND the segment is older than the max tombstone age.
		if seg.LastIndex < watermark && now.Sub(seg.CreatedAt) > gc.maxTombstoneAge {
			// Skip the active segment -- it is still being written to.
			if journal.activeSeg != nil && seg.Path == journal.activeSeg.Path {
				remaining = append(remaining, seg)
				continue
			}
			if err := removeJournalSegment(seg.Path); err != nil {
				gc.logger.Warn("failed to remove journal segment during GC",
					"path", seg.Path, "error", err)
				remaining = append(remaining, seg)
				continue
			}
			pruned++
			gc.journalSegmentsPruned.Add(1)
			if journal.totalBytes >= seg.SizeBytes {
				journal.totalBytes -= seg.SizeBytes
			} else {
				journal.totalBytes = 0
			}
		} else {
			remaining = append(remaining, seg)
		}
	}
	journal.segments = remaining

	if pruned > 0 {
		gc.logger.Info("pruned journal segments via epoch GC",
			"pruned", pruned, "watermark", watermark)
	}
}

// removeJournalSegment safely removes a journal segment file from disk.
func removeJournalSegment(path string) error {
	return os.Remove(path)
}

// Stats returns the current GC telemetry.
func (gc *drTombstoneGC) Stats() map[string]uint64 {
	return map[string]uint64{
		"gc_runs_total":           gc.gcRuns.Load(),
		"tombstones_pruned_total": gc.tombstonesPruned.Load(),
		"journal_segments_pruned": gc.journalSegmentsPruned.Load(),
		"low_watermark":           gc.lowWatermark.Load(),
		"disconnected_peers":      gc.disconnectedPeers.Load(),
	}
}
