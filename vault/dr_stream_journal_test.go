// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"errors"
	"testing"
	"time"

	"github.com/openbao/openbao/sdk/v2/physical"
)

func TestDRStreamJournalAppendReplayRange(t *testing.T) {
	j := newDRStreamJournal(nil, t.TempDir())
	if err := j.configure(true, 1<<20, 1<<20, time.Hour); err != nil {
		t.Fatalf("configure journal: %v", err)
	}

	entries := []physical.ChangeStreamEntry{
		{OpType: physical.PutOperation, Key: "a", Value: []byte("1"), SealWrap: false, RaftIndex: 10},
		{OpType: physical.DeleteOperation, Key: "b", SealWrap: true, RaftIndex: 11},
	}
	if err := j.append(entries); err != nil {
		t.Fatalf("append entries: %v", err)
	}

	var got []physical.ChangeStreamEntry
	if err := j.replayRange(10, 12, func(e physical.ChangeStreamEntry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("replay range: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("expected %d replayed entries, got %d", len(entries), len(got))
	}
	for i := range entries {
		if got[i].RaftIndex != entries[i].RaftIndex {
			t.Fatalf("entry %d raft index mismatch: got=%d want=%d", i, got[i].RaftIndex, entries[i].RaftIndex)
		}
		if got[i].Key != entries[i].Key {
			t.Fatalf("entry %d key mismatch: got=%q want=%q", i, got[i].Key, entries[i].Key)
		}
	}

	if err := j.replayRange(9, 10, func(physical.ChangeStreamEntry) error { return nil }); !errors.Is(err, errDRStreamJournalRangeTooOld) {
		t.Fatalf("expected range-too-old error, got: %v", err)
	}
}

func TestDRStreamJournalPrunesByMaxBytes(t *testing.T) {
	j := newDRStreamJournal(nil, t.TempDir())
	if err := j.configure(true, 600, 200, 24*time.Hour); err != nil {
		t.Fatalf("configure journal: %v", err)
	}

	payload := make([]byte, 256)
	for i := 1; i <= 8; i++ {
		err := j.append([]physical.ChangeStreamEntry{
			{
				OpType:    physical.PutOperation,
				Key:       "k",
				Value:     payload,
				SealWrap:  false,
				RaftIndex: uint64(i),
			},
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	bytes, segments, oldest, newest := j.stats()
	if segments == 0 {
		t.Fatal("expected at least one journal segment")
	}
	if oldest <= 1 {
		t.Fatalf("expected oldest index to advance after pruning, got %d", oldest)
	}
	if newest != 8 {
		t.Fatalf("expected newest index 8, got %d", newest)
	}
	if bytes > 1200 {
		t.Fatalf("expected pruned journal bytes <= 1200, got %d", bytes)
	}

	if err := j.replayRange(1, oldest, func(physical.ChangeStreamEntry) error { return nil }); !errors.Is(err, errDRStreamJournalRangeTooOld) {
		t.Fatalf("expected range-too-old error for pruned interval, got: %v", err)
	}
}
