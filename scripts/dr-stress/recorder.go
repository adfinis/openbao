package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Event is a single recorded operation from a worker.
type Event struct {
	TS        time.Time `json:"ts"`
	Worker    int       `json:"worker"`
	Op        string    `json:"op"`
	Role      string    `json:"role"`
	Key       string    `json:"key,omitempty"`
	Seq       uint64    `json:"seq,omitempty"`
	Bytes     int       `json:"bytes"`
	LatencyNS int64     `json:"latency_ns"`
	Code      int       `json:"code"`
	Err       string    `json:"err,omitempty"`
	Control   bool      `json:"control,omitempty"`
}

// Recorder consumes events from a buffered channel and writes NDJSON to
// events.ndjson (all ops) and writes.ndjson (successful PUTs only).
// It runs a single writer goroutine to avoid contention on the files.
type Recorder struct {
	ch chan Event
	wg sync.WaitGroup

	eventsPath string
	writesPath string

	// Counters (read via atomics from progress reporter).
	TotalOps   atomic.Int64
	PutOK      atomic.Int64
	PutFail    atomic.Int64
	GetOK      atomic.Int64
	GetFail    atomic.Int64
	StatusOK   atomic.Int64
	StatusFail atomic.Int64
	Dropped    atomic.Int64
}

// NewRecorder creates a Recorder and opens the output files.
func NewRecorder(eventsPath, writesPath string, bufSize int) (*Recorder, error) {
	r := &Recorder{
		ch:         make(chan Event, bufSize),
		eventsPath: eventsPath,
		writesPath: writesPath,
	}
	return r, nil
}

// Send attempts to enqueue an event.  Non-blocking: if the channel is full
// the event is dropped and the drop counter incremented.  Workers should
// never block on recording.
func (r *Recorder) Send(ev Event) {
	select {
	case r.ch <- ev:
	default:
		r.Dropped.Add(1)
	}

	if ev.Control {
		return
	}

	// Update atomic counters regardless of channel drop so progress is accurate.
	r.TotalOps.Add(1)
	r.updateCounters(ev)
}

// SendControl records a harness control event, such as a requested primary
// stepdown, without counting it as workload throughput.
func (r *Recorder) SendControl(ev Event) {
	ev.Control = true
	r.Send(ev)
}

func (r *Recorder) updateCounters(ev Event) {
	isErr := ev.Err != "" || (ev.Code != 0 && ev.Code != 200 && ev.Code != 204 && ev.Code != 404)

	switch {
	case ev.Op == "put":
		if isErr {
			r.PutFail.Add(1)
		} else {
			r.PutOK.Add(1)
		}
	case ev.Op == "get_primary":
		if isErr {
			r.GetFail.Add(1)
		} else {
			r.GetOK.Add(1)
		}
	default: // status_s1, status_s2
		if isErr {
			r.StatusFail.Add(1)
		} else {
			r.StatusOK.Add(1)
		}
	}
}

// Run starts the writer goroutine.  Call Close() to flush and stop.
func (r *Recorder) Run() error {
	evFile, err := os.Create(r.eventsPath)
	if err != nil {
		return fmt.Errorf("create events file: %w", err)
	}
	wrFile, err := os.Create(r.writesPath)
	if err != nil {
		evFile.Close()
		return fmt.Errorf("create writes file: %w", err)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer evFile.Close()
		defer wrFile.Close()

		evBuf := bufio.NewWriterSize(evFile, 256*1024)
		wrBuf := bufio.NewWriterSize(wrFile, 64*1024)

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		enc := json.NewEncoder(evBuf)
		wrEnc := json.NewEncoder(wrBuf)

		for {
			select {
			case ev, ok := <-r.ch:
				if !ok {
					// Channel closed -- flush and exit.
					evBuf.Flush()
					wrBuf.Flush()
					return
				}
				enc.Encode(ev)

				// Write successful PUTs to truth log.
				if ev.Op == "put" && ev.Err == "" && (ev.Code == 200 || ev.Code == 204) {
					wrEnc.Encode(ev)
				}

			case <-ticker.C:
				evBuf.Flush()
				wrBuf.Flush()
			}
		}
	}()

	return nil
}

// Close signals the writer goroutine to flush and exit, then waits for it.
func (r *Recorder) Close() {
	close(r.ch)
	r.wg.Wait()
}
