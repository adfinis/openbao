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
		fmt.Fprint(w, `{"data":{"mode":"secondary","secondary_state":"streaming","lag_entries":0,"last_applied_index":101,"primary_index":100}}`)
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
