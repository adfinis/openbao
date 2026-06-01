package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDRStatusChoosesActiveSecondaryFromAddressList(t *testing.T) {
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/replication/dr/status" {
			t.Fatalf("unexpected standby path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"mode":"secondary","secondary_state":"idle","lag_entries":0,"last_applied_index":0,"primary_index":0}}`)
	}))
	defer standby.Close()

	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/replication/dr/status" {
			t.Fatalf("unexpected active path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"mode":"secondary","secondary_state":"streaming","lag_entries":0,"last_applied_index":101,"primary_index":100,"stream_batch_adaptive_adjustments_total":6,"stream_batch_adaptive_level":2,"stream_batch_effective_max_entries":256,"stream_batch_effective_max_wait_milliseconds":100}}`)
	}))
	defer active.Close()

	cfg := DefaultConfig()
	client, err := NewBaoClient(NodeConfig{
		Addr:  standby.URL + "," + active.URL,
		Token: "test-token",
	}, cfg)
	if err != nil {
		t.Fatalf("NewBaoClient returned error: %v", err)
	}

	status, code, err := client.DRStatus(context.Background())
	if err != nil {
		t.Fatalf("DRStatus returned error: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("code = %d, want %d", code, http.StatusOK)
	}
	if status.SecondaryState != "streaming" || status.PrimaryIndex != 100 || status.LastAppliedIndex != 101 {
		t.Fatalf("selected status = %+v, want active streaming status", status)
	}
	if status.StreamBatchAdaptiveAdjustments != 6 || status.StreamBatchAdaptiveLevel != 2 || status.StreamBatchEffectiveMaxEntries != 256 || status.StreamBatchEffectiveMaxWaitMS != 100 {
		t.Fatalf("adaptive stream batch status = %+v, want parsed adaptive counters", status)
	}
}

func TestKVGetChoosesActiveFromAddressList(t *testing.T) {
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/leader":
			fmt.Fprint(w, `{"data":{"ha_enabled":true,"is_self":false}}`)
		default:
			t.Fatalf("unexpected standby path: %s", r.URL.Path)
		}
	}))
	defer standby.Close()

	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/leader":
			fmt.Fprint(w, `{"data":{"ha_enabled":true,"is_self":true}}`)
		case "/v1/kv/data/dr-mixed/k1":
			fmt.Fprint(w, `{"data":{"data":{"seq":42,"run_id":"run-1"},"metadata":{}}}`)
		default:
			t.Fatalf("unexpected active path: %s", r.URL.Path)
		}
	}))
	defer active.Close()

	cfg := DefaultConfig()
	client, err := NewBaoClient(NodeConfig{
		Addr:  standby.URL + "," + active.URL,
		Token: "test-token",
	}, cfg)
	if err != nil {
		t.Fatalf("NewBaoClient returned error: %v", err)
	}

	code, resp, err := client.KVGet(context.Background(), "kv", "dr-mixed/k1")
	if err != nil {
		t.Fatalf("KVGet returned error: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("code = %d, want %d", code, http.StatusOK)
	}
	if got := resp.Data.Data["seq"]; got != float64(42) {
		t.Fatalf("seq = %v, want 42", got)
	}
}

func TestParseAddrsTrimsEmptyParts(t *testing.T) {
	got := parseAddrs(" http://one:8200/, ,http://two:8200/ ")
	want := []string{"http://one:8200", "http://two:8200"}
	if len(got) != len(want) {
		t.Fatalf("len(parseAddrs) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseAddrs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
