package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// ProgressReporter prints a periodic one-liner to stderr (and optionally to
// a log file) showing current throughput, error counts, and replication state.
type ProgressReporter struct {
	recorder *Recorder
	monitor  *Monitor
	cfg      *Config
	startAt  time.Time

	logPath string
}

// NewProgressReporter creates a reporter.
func NewProgressReporter(rec *Recorder, mon *Monitor, cfg *Config, logPath string) *ProgressReporter {
	return &ProgressReporter{
		recorder: rec,
		monitor:  mon,
		cfg:      cfg,
		startAt:  time.Now(),
		logPath:  logPath,
	}
}

// Run prints progress lines until ctx is cancelled.
func (p *ProgressReporter) Run(ctx context.Context) error {
	var logFile *os.File
	var logWriter io.Writer = os.Stderr

	if p.logPath != "" {
		f, err := os.Create(p.logPath)
		if err != nil {
			return fmt.Errorf("create progress log: %w", err)
		}
		logFile = f
		defer logFile.Close()
		logWriter = io.MultiWriter(os.Stderr, logFile)
	}

	ticker := time.NewTicker(p.cfg.ProgressInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.printLine(logWriter)
		}
	}
}

func (p *ProgressReporter) printLine(w io.Writer) {
	elapsed := time.Since(p.startAt)
	remaining := p.cfg.Duration - elapsed
	if remaining < 0 {
		remaining = 0
	}

	total := p.recorder.TotalOps.Load()
	putOK := p.recorder.PutOK.Load()
	putFail := p.recorder.PutFail.Load()
	getOK := p.recorder.GetOK.Load()
	getFail := p.recorder.GetFail.Load()
	statusOK := p.recorder.StatusOK.Load()
	statusFail := p.recorder.StatusFail.Load()
	dropped := p.recorder.Dropped.Load()

	var opsRate float64
	if elapsed.Seconds() > 0 {
		opsRate = float64(total) / elapsed.Seconds()
	}

	// Replication status from latest monitor snapshot.
	snap := p.monitor.Latest()
	s1State := "n/a"
	s1Idx := int64(0)
	s2State := "n/a"
	s2Idx := int64(0)
	pBuf := int64(0)
	pBufMax := int64(0)
	pHorizon := float64(0)

	if snap.Primary != nil {
		pBuf = snap.Primary.StreamBufferEntries
		pBufMax = snap.Primary.StreamBufferMaxEntries
		pHorizon = snap.Primary.StreamBufferHorizonSecs
	}
	if snap.Secondary1 != nil {
		s1State = snap.Secondary1.SecondaryState
		s1Idx = snap.Secondary1.LastAppliedIndex
	}
	if snap.Secondary2 != nil {
		s2State = snap.Secondary2.SecondaryState
		s2Idx = snap.Secondary2.LastAppliedIndex
	}

	fmt.Fprintf(
		w, "[mixed] elapsed=%s remaining=%s ops=%d rate=%.1f/s put(ok=%d fail=%d) get(ok=%d fail=%d) status(ok=%d fail=%d) s1=%s(idx=%d) s2=%s(idx=%d) buf=%d/%d horizon=%.0fs dropped=%d\n",
		formatDuration(elapsed), formatDuration(remaining),
		total, opsRate,
		putOK, putFail,
		getOK, getFail,
		statusOK, statusFail,
		s1State, s1Idx,
		s2State, s2Idx,
		pBuf, pBufMax, pHorizon,
		dropped,
	)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "n/a"
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
