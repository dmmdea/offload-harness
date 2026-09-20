package seatrate

import (
	"path/filepath"
	"testing"
	"time"
)

func TestObservePrefillRecordsAndRejectsSmallSamples(t *testing.T) {
	s := &Store{}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if s.ObservePrefill("seat", 500, 100, now) {
		t.Fatal("a 500-token / 100 ms sample is a cache hit, not a prefill measurement")
	}
	if !s.ObservePrefill("seat", 20000, 10000, now) { // 2,000 tok/s
		t.Fatal("sample rejected")
	}
	if got := s.Get("seat").PrefillTokS; got != 2000 {
		t.Fatalf("first sample must set the rate outright, got %.1f", got)
	}
	s.ObservePrefill("seat", 10000, 10000, now) // 1,000 tok/s -> EWMA 0.3*1000 + 0.7*2000 = 1700
	if got := s.Get("seat").PrefillTokS; got < 1699 || got > 1701 {
		t.Fatalf("EWMA = %.1f, want 1700", got)
	}
	// decode observations leave the prefill rate alone and vice versa
	s.Observe("seat", 30, 0, now)
	if s.Get("seat").PrefillTokS < 1699 || s.Get("seat").TokS != 30 {
		t.Fatalf("fields must be independent: %+v", s.Get("seat"))
	}
}

func TestPrefillRateSurvivesSaveAndLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if err := Update(p, func(s *Store) { s.ObservePrefill("seat", 20000, 10000, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get("seat").PrefillTokS != 2000 {
		t.Fatalf("not persisted: %+v", s.Get("seat"))
	}
}
