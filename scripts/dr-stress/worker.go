package main

import (
	"context"
	"sync"
)

// Worker is a single goroutine executing workload operations.
type Worker struct {
	ID    int
	Rand  *Xorshift64Star
	RunID string

	cfg      *Config
	workload Workload
	recorder *Recorder

	// Per-worker monotonic sequence counter for PUT ordering.
	seq uint64

	// Cumulative weight thresholds built from Ops().
	opThresholds []opThreshold
}

type opThreshold struct {
	cumulativeWeight int
	name             string
}

// NewWorker creates a Worker with a seeded PRNG.
func NewWorker(id int, cfg *Config, wl Workload, rec *Recorder) *Worker {
	w := &Worker{
		ID:       id,
		Rand:     NewXorshift64Star(WorkerSeed(cfg.Seed, id)),
		RunID:    cfg.RunID,
		cfg:      cfg,
		workload: wl,
		recorder: rec,
	}

	// Build cumulative weight thresholds for op selection.
	ops := wl.Ops()
	cum := 0
	for _, op := range ops {
		if op.Weight <= 0 {
			continue
		}
		cum += op.Weight
		w.opThresholds = append(w.opThresholds, opThreshold{
			cumulativeWeight: cum,
			name:             op.Name,
		})
	}

	return w
}

// NextSeq returns the next monotonic sequence number for this worker.
func (w *Worker) NextSeq() uint64 {
	w.seq++
	return w.seq
}

// ChooseOp selects an operation by weighted random distribution.
func (w *Worker) ChooseOp() string {
	roll := w.Rand.Intn(100)
	for _, t := range w.opThresholds {
		if roll < t.cumulativeWeight {
			return t.name
		}
	}
	// Fallback (shouldn't happen if weights sum to 100).
	return w.opThresholds[len(w.opThresholds)-1].name
}

// ChooseKeyIndex selects a key index using hot/cold skew.
// Returns 1-based index matching the bash script's k<N> naming.
func (w *Worker) ChooseKeyIndex() int {
	hotCount := w.cfg.HotKeyCount
	coldCount := w.cfg.ColdKeyCount

	if hotCount <= 0 && coldCount <= 0 {
		return 1
	}
	if hotCount <= 0 {
		return 1 + w.Rand.Intn(coldCount)
	}
	if coldCount <= 0 {
		return 1 + w.Rand.Intn(hotCount)
	}

	if w.Rand.Intn(100) < w.cfg.HotPercent {
		return 1 + w.Rand.Intn(hotCount)
	}
	return hotCount + 1 + w.Rand.Intn(coldCount)
}

// Run executes the worker loop until stopCtx is cancelled or its deadline is
// reached. Individual operations use opCtx so natural workload expiry stops new
// operations without cancelling requests already in flight.
func (w *Worker) Run(stopCtx, opCtx context.Context) {
	for {
		select {
		case <-stopCtx.Done():
			return
		default:
		}

		op := w.ChooseOp()
		ev := w.workload.Execute(opCtx, op, w)
		w.recorder.Send(ev)
	}
}

// RunWorkers starts N workers and blocks until all complete.
func RunWorkers(stopCtx, opCtx context.Context, cfg *Config, wl Workload, rec *Recorder) {
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		w := NewWorker(i, cfg, wl, rec)
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(stopCtx, opCtx)
		}()
	}
	wg.Wait()
}
