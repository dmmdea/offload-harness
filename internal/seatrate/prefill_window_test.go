package seatrate

import (
	"encoding/json"
	"testing"
	"time"
)

// The stall bound must hold the slow prefill, not the average one (register F10). The
// store's PrefillTokS is an EMA over runs; a seat that heat-soaked for one run is
// still averaged with the cool ones, so a stall allowance sized from the EMA is the
// cool seat's. The cold load of the same seat is already kept as the slowest of a
// window for the same reason (coldLoadWindow); the prefill rate gets the same rule.
func TestStallPrefillRateIsTheSlowestRecentSoloSample(t *testing.T) {
	s := &Store{}
	now := time.Now()
	for i := 0; i < 4; i++ {
		s.ObservePrefill("seat", 20000, 10000, now) // 2,000 tok/s, cool
	}
	s.ObservePrefill("seat", 4000, 10000, now) // 400 tok/s, heat-soaked
	got := s.Get("seat")
	if got.PrefillTokS < 1400 {
		t.Fatalf("the EMA is the average and must stay one: %.0f", got.PrefillTokS)
	}
	if rate := got.StallPrefillTokS(); rate != 400 {
		t.Fatalf("stall prefill rate = %.1f tok/s, want the slow sample's 400", rate)
	}
	if got.PrefillSlowTokS != 400 {
		t.Fatalf("PrefillSlowTokS = %.1f, want 400", got.PrefillSlowTokS)
	}
}

// The slow sample leaves the window once enough newer solo samples have followed it,
// so a seat that cooled down gets its tight allowance back.
func TestStallPrefillRateForgetsASlowSampleAfterTheWindow(t *testing.T) {
	s := &Store{}
	now := time.Now()
	s.ObservePrefill("seat", 4000, 10000, now) // 400 tok/s
	for i := 0; i < prefillWindow-1; i++ {
		s.ObservePrefill("seat", 20000, 10000, now)
	}
	if rate := s.Get("seat").StallPrefillTokS(); rate != 400 {
		t.Fatalf("the slow sample is still inside the window: %.1f", rate)
	}
	s.ObservePrefill("seat", 20000, 10000, now) // the window now holds only cool samples
	if rate := s.Get("seat").StallPrefillTokS(); rate != 2000 {
		t.Fatalf("stall prefill rate = %.1f after the slow sample left the window, want 2000", rate)
	}
	if n := len(s.Get("seat").PrefillRates); n != prefillWindow {
		t.Fatalf("window holds %d samples, want %d", n, prefillWindow)
	}
}

// A sample a shared seat gave (load != 1) never enters the window, like it never
// enters the EMA: it would loosen every later allowance with a rate the seat does not
// have alone.
func TestStallPrefillRateIgnoresSharedSamples(t *testing.T) {
	s := &Store{}
	now := time.Now()
	s.ObservePrefill("seat", 20000, 10000, now) // 2,000 tok/s solo
	if s.ObservePrefillLoad("seat", 4000, 10000, 3, now) {
		t.Fatal("a shared sample was folded in")
	}
	if rate := s.Get("seat").StallPrefillTokS(); rate != 2000 {
		t.Fatalf("stall prefill rate = %.1f, a shared sample must not move it", rate)
	}
}

// A store written before the window existed has only the EMA: the stall rate is the
// EMA, exactly the pre-window behaviour, until a solo sample is observed.
func TestStallPrefillRateFallsBackToTheEMAForAnOldStore(t *testing.T) {
	var st Store
	if err := json.Unmarshal([]byte(`{"seats":{"seat":{"tok_s":20,"prefill_tok_s":1800,"samples":3}}}`), &st); err != nil {
		t.Fatal(err)
	}
	if rate := st.Get("seat").StallPrefillTokS(); rate != 1800 {
		t.Fatalf("an old store's stall rate = %.1f, want its EMA 1800", rate)
	}
	if rate := st.Get("never-measured").StallPrefillTokS(); rate != 0 {
		t.Fatalf("an unmeasured seat's stall rate = %.1f, want 0 (the policy assumes its own)", rate)
	}
}
