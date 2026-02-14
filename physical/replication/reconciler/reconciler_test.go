// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package reconciler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
)

type mockScanStorage struct {
	entries     map[string][]byte
	getErrFor   map[string]error
	listPageErr error
}

func newMockScanStorage(entries map[string][]byte) *mockScanStorage {
	return &mockScanStorage{
		entries:   entries,
		getErrFor: make(map[string]error),
	}
}

func (m *mockScanStorage) List(_ context.Context, prefix string) ([]string, error) {
	keys, err := m.ListPage(context.Background(), prefix, "", 0)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (m *mockScanStorage) ListPage(_ context.Context, prefix string, after string, limit int) ([]string, error) {
	if m.listPageErr != nil {
		return nil, m.listPageErr
	}

	var keys []string
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(keys)

	start := 0
	if after != "" {
		for i, k := range keys {
			if k > after {
				start = i
				break
			}
			start = len(keys)
		}
	}

	if start >= len(keys) {
		return nil, nil
	}
	keys = keys[start:]

	if limit > 0 && len(keys) > limit {
		return keys[:limit], nil
	}
	return keys, nil
}

func (m *mockScanStorage) Get(_ context.Context, key string) (*logical.StorageEntry, error) {
	if err, ok := m.getErrFor[key]; ok {
		return nil, err
	}
	val, ok := m.entries[key]
	if !ok {
		return nil, nil
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return &logical.StorageEntry{Key: key, Value: cp}, nil
}

func (m *mockScanStorage) Put(_ context.Context, e *logical.StorageEntry) error {
	cp := make([]byte, len(e.Value))
	copy(cp, e.Value)
	m.entries[e.Key] = cp
	return nil
}

func (m *mockScanStorage) Delete(_ context.Context, key string) error {
	delete(m.entries, key)
	return nil
}

type txMockStorage struct {
	parentGetCalled bool
	tx              *txMockTransaction
}

type txMockTransaction struct {
	*mockScanStorage
}

func (m *txMockStorage) List(ctx context.Context, prefix string) ([]string, error) {
	return m.tx.List(ctx, prefix)
}

func (m *txMockStorage) ListPage(ctx context.Context, prefix string, after string, limit int) ([]string, error) {
	return m.tx.ListPage(ctx, prefix, after, limit)
}

func (m *txMockStorage) Get(_ context.Context, key string) (*logical.StorageEntry, error) {
	m.parentGetCalled = true
	return nil, fmt.Errorf("parent storage Get called for %q", key)
}

func (m *txMockStorage) Put(_ context.Context, _ *logical.StorageEntry) error {
	return nil
}

func (m *txMockStorage) Delete(_ context.Context, _ string) error {
	return nil
}

func (m *txMockStorage) BeginReadOnlyTx(_ context.Context) (logical.Transaction, error) {
	return m.tx, nil
}

func (m *txMockStorage) BeginTx(_ context.Context) (logical.Transaction, error) {
	return m.tx, nil
}

func (t *txMockTransaction) Commit(_ context.Context) error {
	return nil
}

func (t *txMockTransaction) Rollback(_ context.Context) error {
	return nil
}

func TestScannerFailsClosedOnGetError(t *testing.T) {
	ctx := context.Background()
	store := newMockScanStorage(map[string][]byte{
		"foo": []byte("bar"),
	})
	store.getErrFor["foo"] = fmt.Errorf("boom")

	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.RequireTransactionalSnapshot = false
	scanner := NewScanner(cfg)

	_, err := scanner.Scan(ctx, store, Checkpoint{ID: "cp", CommitIndex: 1})
	if err == nil {
		t.Fatal("expected scan to fail on Get error")
	}
	if !strings.Contains(err.Error(), "failed to read entry during scan") {
		t.Fatalf("expected fail-closed scan error, got: %v", err)
	}
}

func TestScannerFailsClosedOnListPageError(t *testing.T) {
	ctx := context.Background()
	store := newMockScanStorage(map[string][]byte{
		"foo": []byte("bar"),
	})
	store.listPageErr = fmt.Errorf("list failure")

	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.RequireTransactionalSnapshot = false
	scanner := NewScanner(cfg)

	_, err := scanner.Scan(ctx, store, Checkpoint{ID: "cp", CommitIndex: 1})
	if err == nil {
		t.Fatal("expected scan to fail on ListPage error")
	}
	if !strings.Contains(err.Error(), "scan failed") {
		t.Fatalf("expected scan failure error, got: %v", err)
	}
}

func TestScannerRequiresTransactionalSnapshotWhenConfigured(t *testing.T) {
	ctx := context.Background()
	store := newMockScanStorage(map[string][]byte{
		"foo": []byte("bar"),
	})

	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.RequireTransactionalSnapshot = true
	scanner := NewScanner(cfg)

	_, err := scanner.Scan(ctx, store, Checkpoint{ID: "cp", CommitIndex: 1})
	if err == nil {
		t.Fatal("expected error when transactional snapshot is required")
	}
	if !strings.Contains(err.Error(), "transactional snapshot required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScannerUsesTransactionHandleForGet(t *testing.T) {
	ctx := context.Background()
	txEntries := map[string][]byte{
		"foo": []byte("bar"),
	}
	txStore := &txMockStorage{
		tx: &txMockTransaction{
			mockScanStorage: newMockScanStorage(txEntries),
		},
	}

	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.RequireTransactionalSnapshot = true
	scanner := NewScanner(cfg)

	set, err := scanner.Scan(ctx, txStore, Checkpoint{ID: "cp", CommitIndex: 1})
	if err != nil {
		t.Fatalf("unexpected scan error: %v", err)
	}
	if set.KeyCount != 1 {
		t.Fatalf("expected one scanned key, got %d", set.KeyCount)
	}
	if txStore.parentGetCalled {
		t.Fatal("scanner should read entries from transactional snapshot, not parent storage")
	}
}

func TestComputeVIDWithSealWrap_CiphertextDomain(t *testing.T) {
	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.ValueDomain = ValueDomainCiphertext
	scanner := NewScanner(cfg)

	value := []byte("ciphertext-bytes")

	plain := scanner.ComputeVIDWithSealWrap(value, false)
	wrapped := scanner.ComputeVIDWithSealWrap(value, true)

	if plain == wrapped {
		t.Fatal("expected ciphertext-domain VID to include seal-wrap flag")
	}

	other := scanner.ComputeVIDWithSealWrap([]byte("ciphertext-bytes-2"), false)
	if plain == other {
		t.Fatal("expected ciphertext-domain VID to differ for different values")
	}
}

func TestComputeVIDWithSealWrap_PlaintextDomainIgnoresSealWrap(t *testing.T) {
	cfg := DefaultScanConfig([]byte("test-salt"))
	cfg.ValueDomain = ValueDomainPlaintext
	scanner := NewScanner(cfg)

	value := []byte("plaintext")
	a := scanner.ComputeVIDWithSealWrap(value, false)
	b := scanner.ComputeVIDWithSealWrap(value, true)

	if a != b {
		t.Fatal("expected plaintext-domain VID to ignore seal-wrap flag")
	}
}
