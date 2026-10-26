// Package spool is a bounded on-disk FIFO of encoded batches that could not be
// shipped. Each batch is one file named so that lexical order is creation
// order; batches are replayed oldest first and the oldest are dropped when the
// spool exceeds its byte or age bound.
package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const suffix = ".batch"

// Entry is one spooled batch.
type Entry struct {
	Name      string
	Size      int64
	CreatedAt time.Time
}

// Spool is safe for concurrent use.
type Spool struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries []Entry // oldest first
	bytes   int64
	dropped uint64
	counter uint64
}

// Open loads (or creates) the spool in dir.
func Open(dir string, maxBytes int64, maxAge time.Duration) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	s := &Spool{dir: dir, maxBytes: maxBytes, maxAge: maxAge, now: time.Now}

	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		name := it.Name()
		if it.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name)) // interrupted write
			continue
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		created, ok := parseName(name)
		if !ok {
			continue
		}
		s.entries = append(s.entries, Entry{Name: name, Size: info.Size(), CreatedAt: created})
		s.bytes += info.Size()
	}
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].Name < s.entries[j].Name })

	s.mu.Lock()
	s.pruneLocked()
	s.mu.Unlock()

	return s, nil
}

// Push appends a batch. The write is atomic (temp file + rename).
func (s *Spool) Push(data []byte, createdAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counter++
	name := fmt.Sprintf("%016d-%06d%s", createdAt.UnixMilli(), s.counter%1_000_000, suffix)
	// Keep FIFO order even if the wall clock steps backwards.
	if n := len(s.entries); n > 0 && name <= s.entries[n-1].Name {
		last, _ := parseName(s.entries[n-1].Name)
		createdAt = last
		name = fmt.Sprintf("%016d-%06d%s", last.UnixMilli(), s.counter%1_000_000, suffix)
		if name <= s.entries[n-1].Name {
			name = strings.TrimSuffix(s.entries[n-1].Name, suffix) + "z" + suffix
		}
	}

	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write spool file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit spool file: %w", err)
	}

	s.entries = append(s.entries, Entry{Name: name, Size: int64(len(data)), CreatedAt: createdAt})
	s.bytes += int64(len(data))
	s.pruneLocked()
	return nil
}

// Oldest returns the oldest batch, or ok=false when the spool is empty.
func (s *Spool) Oldest() (Entry, []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked()
	for len(s.entries) > 0 {
		e := s.entries[0]
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name))
		if errors.Is(err, os.ErrNotExist) {
			s.removeFirstLocked()
			continue
		}
		if err != nil {
			return Entry{}, nil, false, err
		}
		return e, data, true, nil
	}
	return Entry{}, nil, false, nil
}

// Remove deletes a batch once it has been shipped (or rejected for good).
func (s *Spool) Remove(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, cur := range s.entries {
		if cur.Name == e.Name {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			s.bytes -= cur.Size
			break
		}
	}
	err := os.Remove(filepath.Join(s.dir, e.Name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Clear deletes every spooled batch and returns how many there were.
func (s *Spool) Clear() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(s.entries)
	var firstErr error
	for _, e := range s.entries {
		if err := os.Remove(filepath.Join(s.dir, e.Name)); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	s.entries = nil
	s.bytes = 0
	return n, firstErr
}

// Len returns the number of spooled batches.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Bytes returns the total size of spooled batches.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Dropped returns how many batches were evicted by the size/age bounds.
func (s *Spool) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *Spool) pruneLocked() {
	cutoff := s.now().Add(-s.maxAge)
	for len(s.entries) > 0 {
		e := s.entries[0]
		if s.bytes <= s.maxBytes && (s.maxAge <= 0 || !e.CreatedAt.Before(cutoff)) {
			return
		}
		_ = os.Remove(filepath.Join(s.dir, e.Name))
		s.removeFirstLocked()
		s.dropped++
	}
}

func (s *Spool) removeFirstLocked() {
	s.bytes -= s.entries[0].Size
	s.entries = s.entries[1:]
}

func parseName(name string) (time.Time, bool) {
	var ms int64
	if _, err := fmt.Sscanf(name, "%016d-", &ms); err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}
