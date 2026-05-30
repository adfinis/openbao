package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteSentinelWithRetryRetriesTransientFailures(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := atomic.AddInt32(&attempts, 1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/kv/data/dr-mixed/run-1/sentinel" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got < 3 {
			http.Error(w, "active handoff", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := sentinelTestConfig(server.URL)
	client := &BaoClient{httpClient: server.Client(), addr: server.URL, token: "root"}

	code, err, gotAttempts, _ := writeSentinelWithRetry(context.Background(), cfg, client, "dr-mixed/run-1/sentinel", map[string]interface{}{"run_id": "run-1"})
	if err != nil {
		t.Fatalf("writeSentinelWithRetry returned error: %v", err)
	}
	if code != http.StatusNoContent {
		t.Fatalf("code = %d, want %d", code, http.StatusNoContent)
	}
	if gotAttempts != 3 {
		t.Fatalf("attempts = %d, want 3", gotAttempts)
	}
}

func TestWriteSentinelWithRetryDoesNotRetryPermanentFailure(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	defer server.Close()

	cfg := sentinelTestConfig(server.URL)
	client := &BaoClient{httpClient: server.Client(), addr: server.URL, token: "root"}

	code, err, gotAttempts, _ := writeSentinelWithRetry(context.Background(), cfg, client, "dr-mixed/run-1/sentinel", map[string]interface{}{"run_id": "run-1"})
	if err != nil {
		t.Fatalf("writeSentinelWithRetry returned error: %v", err)
	}
	if code != http.StatusForbidden {
		t.Fatalf("code = %d, want %d", code, http.StatusForbidden)
	}
	if gotAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", gotAttempts)
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("server attempts = %d, want 1", attempts)
	}
}

func sentinelTestConfig(addr string) *Config {
	return &Config{
		Primary: NodeConfig{
			Addr:  addr,
			Token: "root",
		},
		KVMount:                    "kv",
		SentinelWriteTimeout:       5 * time.Second,
		SentinelWriteRetryInterval: 10 * time.Millisecond,
	}
}
