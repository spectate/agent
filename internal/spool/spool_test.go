package spool

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFIFOAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 5 {
		if err := s.Push([]byte(fmt.Sprintf("batch-%d", i)), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 5 || s.Bytes() != 5*7 {
		t.Fatalf("len=%d bytes=%d", s.Len(), s.Bytes())
	}

	e, data, ok, err := s.Oldest()
	if err != nil || !ok || string(data) != "batch-0" {
		t.Fatalf("oldest = %q %v %v", data, ok, err)
	}
	if err := s.Remove(e); err != nil {
		t.Fatal(err)
	}

	// Reopen: order and accounting survive a restart.
	s2, err := Open(dir, 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		e, data, ok, err := s2.Oldest()
		if err != nil || !ok || string(data) != fmt.Sprintf("batch-%d", i) {
			t.Fatalf("replay %d = %q", i, data)
		}
		_ = s2.Remove(e)
	}
	if _, _, ok, _ := s2.Oldest(); ok || s2.Bytes() != 0 {
		t.Fatal("spool should be empty")
	}
}

func TestSameMillisecondAndClockStepKeepOrder(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20, 0)
	now := time.Now()
	_ = s.Push([]byte("a"), now)
	_ = s.Push([]byte("b"), now)
	_ = s.Push([]byte("c"), now.Add(-time.Hour)) // clock stepped back
	var got []byte
	for {
		e, data, ok, _ := s.Oldest()
		if !ok {
			break
		}
		got = append(got, data...)
		_ = s.Remove(e)
	}
	if string(got) != "abc" {
		t.Fatalf("order = %q", got)
	}
}

func TestByteBoundDropsOldest(t *testing.T) {
	s, _ := Open(t.TempDir(), 25, time.Hour)
	now := time.Now()
	for i := range 5 {
		_ = s.Push(bytes.Repeat([]byte{byte('0' + i)}, 10), now.Add(time.Duration(i)*time.Second))
	}
	if s.Len() != 2 || s.Bytes() != 20 || s.Dropped() != 3 {
		t.Fatalf("len=%d bytes=%d dropped=%d", s.Len(), s.Bytes(), s.Dropped())
	}
	_, data, _, _ := s.Oldest()
	if data[0] != '3' {
		t.Fatalf("oldest kept batch = %c, want 3", data[0])
	}
}

func TestAgeBoundDropsExpired(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20, 24*time.Hour)
	now := time.Now()
	_ = s.Push([]byte("old"), now.Add(-25*time.Hour))
	_ = s.Push([]byte("new"), now)
	if s.Len() != 1 || s.Dropped() != 1 {
		t.Fatalf("len=%d dropped=%d", s.Len(), s.Dropped())
	}
}

func TestInterruptedWritesAreCleanedUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0000000000000001-000001.batch.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatal("tmp files must not be replayed")
	}
	if _, err := os.Stat(filepath.Join(dir, "0000000000000001-000001.batch.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file should be removed")
	}
}
