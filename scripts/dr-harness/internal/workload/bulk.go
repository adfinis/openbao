package workload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbao/openbao/scripts/dr-harness/internal/bao"
)

type BulkKVConfig struct {
	Client      *bao.Client
	Mount       string
	KeyPrefix   string
	Phase       string
	RunID       string
	Count       int
	Concurrency int
	LogFile     string
}

type BulkKVResult struct {
	Count       int           `json:"count"`
	Concurrency int           `json:"concurrency"`
	Successes   int64         `json:"successes"`
	Failures    int64         `json:"failures"`
	Duration    time.Duration `json:"duration_nanos"`
}

func BulkKV(ctx context.Context, cfg BulkKVConfig) (BulkKVResult, error) {
	if cfg.Count <= 0 {
		return BulkKVResult{Count: cfg.Count, Concurrency: cfg.Concurrency}, nil
	}
	if cfg.Client == nil {
		return BulkKVResult{}, fmt.Errorf("bulk KV client is nil")
	}
	if cfg.Concurrency <= 0 {
		return BulkKVResult{}, fmt.Errorf("bulk KV concurrency must be > 0")
	}
	if cfg.Mount == "" {
		cfg.Mount = "kv"
	}

	if cfg.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
			return BulkKVResult{}, err
		}
		_ = os.WriteFile(cfg.LogFile, nil, 0o644)
	}

	start := time.Now()
	jobs := make(chan int)
	var successes int64
	var failures int64
	var firstErr atomic.Value
	var logMu sync.Mutex

	logf := func(format string, args ...any) {
		if cfg.LogFile == "" {
			return
		}
		logMu.Lock()
		defer logMu.Unlock()
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < cfg.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				err := putWithRetry(ctx, cfg.Client, cfg.Mount, fmt.Sprintf("%s/k-%d", cfg.KeyPrefix, idx), map[string]any{
					"phase":  cfg.Phase,
					"run_id": cfg.RunID,
					"index":  idx,
				})
				if err != nil {
					atomic.AddInt64(&failures, 1)
					if firstErr.Load() == nil {
						firstErr.Store(err)
					}
					logf("k-%d error: %v", idx, err)
					continue
				}
				atomic.AddInt64(&successes, 1)
			}
		}()
	}

	for i := 0; i < cfg.Count; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return BulkKVResult{
				Count:       cfg.Count,
				Concurrency: cfg.Concurrency,
				Successes:   successes,
				Failures:    failures,
				Duration:    time.Since(start),
			}, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	result := BulkKVResult{
		Count:       cfg.Count,
		Concurrency: cfg.Concurrency,
		Successes:   successes,
		Failures:    failures,
		Duration:    time.Since(start),
	}
	if failures > 0 {
		if err, ok := firstErr.Load().(error); ok {
			return result, err
		}
		return result, fmt.Errorf("%d bulk KV writes failed", failures)
	}
	return result, nil
}

func putWithRetry(ctx context.Context, client *bao.Client, mount, key string, data map[string]any) error {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := client.KVPut(reqCtx, mount, key, data)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == 5 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}
