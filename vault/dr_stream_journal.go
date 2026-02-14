// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/openbao/openbao/sdk/v2/physical"
)

const (
	drDefaultStreamJournalMaxBytes      = 4 << 30  // 4 GiB
	drDefaultStreamJournalSegmentBytes  = 64 << 20 // 64 MiB
	drDefaultStreamJournalRetention     = 2 * time.Hour
	drDefaultStreamJournalFlushInterval = 1024
)

var errDRStreamJournalRangeTooOld = errors.New("stream journal range unavailable")

type drStreamJournalRecord struct {
	TimestampUnix int64  `json:"ts"`
	OpType        string `json:"op_type"`
	Key           string `json:"key"`
	Value         []byte `json:"value,omitempty"`
	SealWrap      bool   `json:"seal_wrap,omitempty"`
	RaftIndex     uint64 `json:"raft_index"`
}

type drStreamJournalSegment struct {
	Path       string
	CreatedAt  time.Time
	FirstIndex uint64
	LastIndex  uint64
	SizeBytes  uint64
}

type drStreamJournal struct {
	logger log.Logger

	mu sync.RWMutex

	enabled      bool
	dir          string
	maxBytes     uint64
	segmentBytes uint64
	retention    time.Duration

	segments   []*drStreamJournalSegment
	totalBytes uint64
	active     *os.File
	activeSeg  *drStreamJournalSegment
	writeCount int
}

func newDRStreamJournal(logger log.Logger, dir string) *drStreamJournal {
	if logger == nil {
		logger = log.NewNullLogger()
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "openbao-dr-stream-journal")
	}
	return &drStreamJournal{
		logger:       logger.Named("dr-stream-journal"),
		dir:          dir,
		enabled:      true,
		maxBytes:     drDefaultStreamJournalMaxBytes,
		segmentBytes: drDefaultStreamJournalSegmentBytes,
		retention:    drDefaultStreamJournalRetention,
	}
}

func (j *drStreamJournal) configure(enabled bool, maxBytes uint64, segmentBytes uint64, retention time.Duration) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if maxBytes == 0 {
		maxBytes = drDefaultStreamJournalMaxBytes
	}
	if segmentBytes == 0 {
		segmentBytes = drDefaultStreamJournalSegmentBytes
	}
	if retention <= 0 {
		retention = drDefaultStreamJournalRetention
	}

	j.enabled = enabled
	j.maxBytes = maxBytes
	j.segmentBytes = segmentBytes
	j.retention = retention

	if !j.enabled {
		if j.active != nil {
			_ = j.active.Close()
			j.active = nil
			j.activeSeg = nil
		}
		return nil
	}

	if err := os.MkdirAll(j.dir, 0o750); err != nil {
		return fmt.Errorf("create stream journal dir: %w", err)
	}

	if err := j.loadSegmentsLocked(); err != nil {
		return err
	}
	j.pruneLocked(time.Now().UTC())
	return nil
}

func (j *drStreamJournal) append(entries []physical.ChangeStreamEntry) error {
	if len(entries) == 0 {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.enabled {
		return nil
	}

	if err := os.MkdirAll(j.dir, 0o750); err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, e := range entries {
		record := drStreamJournalRecord{
			TimestampUnix: now.Unix(),
			OpType:        string(e.OpType),
			Key:           e.Key,
			Value:         e.Value,
			SealWrap:      e.SealWrap,
			RaftIndex:     e.RaftIndex,
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := j.ensureActiveSegmentLocked(e.RaftIndex, uint64(len(raw)+4), now); err != nil {
			return err
		}
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(raw)))
		if _, err := j.active.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := j.active.Write(raw); err != nil {
			return err
		}

		j.activeSeg.LastIndex = e.RaftIndex
		j.activeSeg.SizeBytes += uint64(len(raw) + 4)
		j.totalBytes += uint64(len(raw) + 4)
		j.writeCount++
	}

	if j.writeCount >= drDefaultStreamJournalFlushInterval {
		_ = j.active.Sync()
		j.writeCount = 0
	}
	j.pruneLocked(now)
	return nil
}

func (j *drStreamJournal) replayRange(startInclusive uint64, endExclusive uint64, fn func(physical.ChangeStreamEntry) error) error {
	j.mu.RLock()
	if !j.enabled || len(j.segments) == 0 {
		j.mu.RUnlock()
		return errDRStreamJournalRangeTooOld
	}
	segments := make([]drStreamJournalSegment, 0, len(j.segments))
	for _, seg := range j.segments {
		segments = append(segments, *seg)
	}
	j.mu.RUnlock()

	oldest := segments[0].FirstIndex
	if startInclusive < oldest {
		return errDRStreamJournalRangeTooOld
	}

	for _, seg := range segments {
		if seg.LastIndex < startInclusive {
			continue
		}
		if endExclusive > 0 && seg.FirstIndex >= endExclusive {
			break
		}
		if err := readJournalSegment(seg.Path, startInclusive, endExclusive, fn); err != nil {
			return err
		}
	}
	return nil
}

func (j *drStreamJournal) stats() (bytes uint64, segments int, oldest uint64, newest uint64) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	bytes = j.totalBytes
	segments = len(j.segments)
	if segments > 0 {
		oldest = j.segments[0].FirstIndex
		newest = j.segments[segments-1].LastIndex
	}
	return
}

func (j *drStreamJournal) ensureActiveSegmentLocked(firstIndex uint64, appendBytes uint64, now time.Time) error {
	if j.active != nil && j.activeSeg != nil {
		if j.activeSeg.SizeBytes+appendBytes <= j.segmentBytes {
			return nil
		}
		_ = j.active.Sync()
		_ = j.active.Close()
		j.active = nil
		j.activeSeg = nil
	}

	name := fmt.Sprintf("%020d-%d.seg", firstIndex, now.UnixNano())
	path := filepath.Join(j.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	seg := &drStreamJournalSegment{
		Path:       path,
		CreatedAt:  now,
		FirstIndex: firstIndex,
		LastIndex:  firstIndex,
		SizeBytes:  0,
	}
	j.active = f
	j.activeSeg = seg
	j.segments = append(j.segments, seg)
	sort.Slice(j.segments, func(i, k int) bool {
		if j.segments[i].FirstIndex == j.segments[k].FirstIndex {
			return j.segments[i].Path < j.segments[k].Path
		}
		return j.segments[i].FirstIndex < j.segments[k].FirstIndex
	})
	return nil
}

func (j *drStreamJournal) loadSegmentsLocked() error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	j.segments = j.segments[:0]
	j.totalBytes = 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".seg" {
			continue
		}
		path := filepath.Join(j.dir, entry.Name())
		seg, err := inspectJournalSegment(path)
		if err != nil {
			j.logger.Warn("dropping unreadable stream journal segment", "path", path, "error", err)
			_ = os.Remove(path)
			continue
		}
		j.segments = append(j.segments, seg)
		j.totalBytes += seg.SizeBytes
	}
	sort.Slice(j.segments, func(i, k int) bool {
		if j.segments[i].FirstIndex == j.segments[k].FirstIndex {
			return j.segments[i].Path < j.segments[k].Path
		}
		return j.segments[i].FirstIndex < j.segments[k].FirstIndex
	})
	return nil
}

func (j *drStreamJournal) pruneLocked(now time.Time) {
	if len(j.segments) == 0 {
		return
	}
	keepFrom := 0
	cutoff := now.Add(-j.retention)
	for keepFrom < len(j.segments) {
		seg := j.segments[keepFrom]
		if !seg.CreatedAt.IsZero() && seg.CreatedAt.After(cutoff) {
			break
		}
		// Never delete active segment.
		if j.activeSeg != nil && seg.Path == j.activeSeg.Path {
			break
		}
		if len(j.segments)-keepFrom <= 1 {
			break
		}
		if err := os.Remove(seg.Path); err == nil {
			if seg.SizeBytes <= j.totalBytes {
				j.totalBytes -= seg.SizeBytes
			} else {
				j.totalBytes = 0
			}
		}
		keepFrom++
	}
	if keepFrom > 0 {
		j.segments = j.segments[keepFrom:]
	}

	for j.totalBytes > j.maxBytes && len(j.segments) > 1 {
		seg := j.segments[0]
		if j.activeSeg != nil && seg.Path == j.activeSeg.Path {
			break
		}
		_ = os.Remove(seg.Path)
		if seg.SizeBytes <= j.totalBytes {
			j.totalBytes -= seg.SizeBytes
		} else {
			j.totalBytes = 0
		}
		j.segments = j.segments[1:]
	}
}

func inspectJournalSegment(path string) (*drStreamJournalSegment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var first uint64
	var last uint64
	reader := bufio.NewReader(f)
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(reader, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		payload := make([]byte, n)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		var rec drStreamJournalRecord
		if err := json.Unmarshal(payload, &rec); err != nil {
			return nil, err
		}
		if first == 0 {
			first = rec.RaftIndex
		}
		last = rec.RaftIndex
	}
	if first == 0 {
		return nil, fmt.Errorf("empty segment")
	}
	return &drStreamJournalSegment{
		Path:       path,
		CreatedAt:  stat.ModTime().UTC(),
		FirstIndex: first,
		LastIndex:  last,
		SizeBytes:  uint64(stat.Size()),
	}, nil
}

func readJournalSegment(path string, startInclusive uint64, endExclusive uint64, fn func(physical.ChangeStreamEntry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(reader, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		payload := make([]byte, n)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		var rec drStreamJournalRecord
		if err := json.Unmarshal(payload, &rec); err != nil {
			return err
		}
		if rec.RaftIndex < startInclusive {
			continue
		}
		if endExclusive > 0 && rec.RaftIndex >= endExclusive {
			return nil
		}
		if err := fn(physical.ChangeStreamEntry{
			OpType:    physical.Operation(rec.OpType),
			Key:       rec.Key,
			Value:     rec.Value,
			SealWrap:  rec.SealWrap,
			RaftIndex: rec.RaftIndex,
		}); err != nil {
			return err
		}
	}
}
