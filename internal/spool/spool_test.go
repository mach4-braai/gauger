package spool

import (
	"os"
	"path/filepath"
	"testing"
)

func drain(t *testing.T, s *Spool) []string {
	t.Helper()
	var got []string
	for {
		b, ok, err := s.Oldest()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return got
		}
		got = append(got, string(b.Data))
		if err := s.Remove(b.Seq); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFIFO(t *testing.T) {
	s, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"a", "b", "c"} {
		if err := s.Put([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	if got := drain(t, s); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("drained %v, want [a b c]", got)
	}
}

func TestPutDropsOldestOverTheLimit(t *testing.T) {
	s, err := Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"1111", "2222", "3333"} {
		if err := s.Put([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", s.Dropped())
	}
	if got := drain(t, s); len(got) != 2 || got[0] != "2222" {
		t.Fatalf("drained %v, want [2222 3333]", got)
	}
}

func TestPutKeepsABatchLargerThanTheLimit(t *testing.T) {
	s, err := Open(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte("newest")); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, s); len(got) != 1 || got[0] != "newest" {
		t.Fatalf("drained %v, want [newest]", got)
	}
}

func TestOpenResumesAndIgnoresPartialWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte("kept")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Put([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, again); len(got) != 2 || got[0] != "kept" || got[1] != "next" {
		t.Fatalf("drained %v, want [kept next]", got)
	}
}

func TestRemoveOfADroppedBatch(t *testing.T) {
	s, err := Open(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte("aaaa")); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Oldest()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte("bbbb")); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(first.Seq); err != nil {
		t.Fatalf("Remove of a dropped batch: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}
