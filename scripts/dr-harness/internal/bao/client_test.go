package bao

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveActiveAddrAcceptsTopLevelLeaderShape(t *testing.T) {
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ha_enabled":true,"is_self":true}`)
	}))
	defer active.Close()
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ha_enabled":true}`)
	}))
	defer standby.Close()

	client, err := NewClient(Node{
		Addrs:       []string{standby.URL, active.URL},
		Token:       "root",
		HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.ResolveActiveAddr(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != active.URL {
		t.Fatalf("active addr = %q, want %q", got, active.URL)
	}
}

func TestResolveActiveAddrAcceptsEnvelopeLeaderShape(t *testing.T) {
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"is_self":true}}`)
	}))
	defer active.Close()

	client, err := NewClient(Node{
		Addrs:       []string{active.URL},
		Token:       "root",
		HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.ResolveActiveAddr(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != active.URL {
		t.Fatalf("active addr = %q, want %q", got, active.URL)
	}
}
