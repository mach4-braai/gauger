// Package spool keeps encoded OTLP batches on disk until they are sent.
//
// Each batch is one file named by a sequence number, written to a temporary
// name and renamed, so a reader never sees a partial batch. When the total size
// passes the limit, the oldest batches are dropped.
package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Ext is the extension of a complete batch file.
const Ext = ".pb"

// Spool is a bounded FIFO of batches in one directory. It is safe for
// concurrent use.
type Spool struct {
	dir string
	max int64

	mu      sync.Mutex
	next    uint64
	entries []entry
	size    int64
	dropped int
}

type entry struct {
	seq  uint64
	size int64
}

// Open creates dir if needed and picks up batches left by an earlier process.
func Open(dir string, maxBytes int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, max: maxBytes}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range names {
		seq, ok := parse(de.Name())
		if !ok {
			continue
		}
		info, err := de.Info()
		if err != nil {
			return nil, err
		}
		s.entries = append(s.entries, entry{seq: seq, size: info.Size()})
		s.size += info.Size()
		s.next = max(s.next, seq+1)
	}
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].seq < s.entries[j].seq })
	return s, nil
}

// Put stores one batch, then drops the oldest batches while the spool is over
// its limit. The batch just written is never dropped.
func (s *Spool) Put(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.next
	s.next++
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(seq)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.entries = append(s.entries, entry{seq: seq, size: int64(len(data))})
	s.size += int64(len(data))
	for s.size > s.max && len(s.entries) > 1 {
		old := s.entries[0]
		if err := os.Remove(s.path(old.seq)); err != nil && !os.IsNotExist(err) {
			return err
		}
		s.entries = s.entries[1:]
		s.size -= old.size
		s.dropped++
	}
	return nil
}

// Batch is a stored batch.
type Batch struct {
	Seq  uint64
	Data []byte
}

// Oldest returns the oldest batch, or false when the spool is empty.
func (s *Spool) Oldest() (Batch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.entries) > 0 {
		e := s.entries[0]
		data, err := os.ReadFile(s.path(e.seq))
		if os.IsNotExist(err) {
			s.entries = s.entries[1:]
			s.size -= e.size
			continue
		}
		if err != nil {
			return Batch{}, false, err
		}
		return Batch{Seq: e.seq, Data: data}, true, nil
	}
	return Batch{}, false, nil
}

// Remove deletes a batch after it was sent. Removing a batch that Put already
// dropped is not an error.
func (s *Spool) Remove(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(seq)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i, e := range s.entries {
		if e.seq == seq {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			s.size -= e.size
			break
		}
	}
	return nil
}

// Len is the number of stored batches.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Dropped is the number of batches dropped to stay under the limit.
func (s *Spool) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *Spool) path(seq uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%020d%s", seq, Ext))
}

func parse(name string) (uint64, bool) {
	base, ok := strings.CutSuffix(name, Ext)
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(base, 10, 64)
	return seq, err == nil
}
