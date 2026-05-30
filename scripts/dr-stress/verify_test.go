package main

import (
	"reflect"
	"testing"
	"time"
)

func TestTerminalWriteSeqs(t *testing.T) {
	base := time.Unix(100, 0)

	tests := []struct {
		name   string
		writes []truthWrite
		want   []uint64
	}{
		{
			name: "confirmed later write overwrites completed earlier write",
			writes: []truthWrite{
				{seq: 1, start: base, end: base.Add(10 * time.Millisecond), confirmed: true},
				{seq: 2, start: base.Add(11 * time.Millisecond), end: base.Add(20 * time.Millisecond), confirmed: true},
			},
			want: []uint64{2},
		},
		{
			name: "overlapping confirmed writes are both possible final values",
			writes: []truthWrite{
				{seq: 1, start: base, end: base.Add(10 * time.Millisecond), confirmed: true},
				{seq: 2, start: base.Add(5 * time.Millisecond), end: base.Add(20 * time.Millisecond), confirmed: true},
			},
			want: []uint64{1, 2},
		},
		{
			name: "commit uncertain tail does not overwrite confirmed earlier write",
			writes: []truthWrite{
				{seq: 1, start: base, end: base.Add(10 * time.Millisecond), confirmed: true},
				{seq: 2, start: base.Add(11 * time.Millisecond), end: base.Add(20 * time.Millisecond), confirmed: false},
			},
			want: []uint64{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalWriteSeqs(tt.writes); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("terminalWriteSeqs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasConfirmedWrite(t *testing.T) {
	if hasConfirmedWrite([]truthWrite{{seq: 1, confirmed: false}}) {
		t.Fatal("commit-uncertain-only writes should not require the key to exist")
	}
	if !hasConfirmedWrite([]truthWrite{{seq: 1, confirmed: false}, {seq: 2, confirmed: true}}) {
		t.Fatal("any confirmed write should require the key to exist")
	}
}

func TestSampleKeysZeroMeansAll(t *testing.T) {
	truth := map[string]truthEntry{
		"a": {allowedSeqs: []uint64{1}},
		"b": {allowedSeqs: []uint64{2}},
	}
	if got := sampleKeys(truth, 0); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("sampleKeys(..., 0) = %v, want all keys", got)
	}
}
