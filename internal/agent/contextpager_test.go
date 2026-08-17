package agent

import (
	"strings"
	"sync"
	"testing"
)

func bigPayload(seed string) string { return seed + strings.Repeat(" x", minPayloadBytes) }

// The gate's whole value is that it can CLOSE the item. It must therefore be able to reach a
// genuine "below gate" verdict — and must not reach it by accident when nothing was measured.
func TestBelowGateClosesTheItem(t *testing.T) {
	var p PagerStats
	for i := 0; i < 20; i++ {
		p.NoteEvicted(bigPayload(string(rune('a' + i))))
	}
	p.NoteFetched(bigPayload("a")) // 1 of 20 distinct = 5%
	r := p.Report()
	if r.Basis != "measured" {
		t.Fatalf("basis = %q, want measured", r.Basis)
	}
	if r.RefetchRate == nil || *r.RefetchRate > 0.06 {
		t.Fatalf("rate = %v, want ~0.05", r.RefetchRate)
	}
	if !strings.HasPrefix(r.Verdict, "BELOW GATE") {
		t.Fatalf("verdict = %q, want BELOW GATE", r.Verdict)
	}
}

func TestAboveGateKeepsTheItemOpen(t *testing.T) {
	var p PagerStats
	for i := 0; i < 10; i++ {
		p.NoteEvicted(bigPayload(string(rune('a' + i))))
	}
	for i := 0; i < 3; i++ {
		p.NoteFetched(bigPayload(string(rune('a' + i)))) // 3 of 10 = 30%
	}
	r := p.Report()
	if !strings.HasPrefix(r.Verdict, "ABOVE GATE") {
		t.Fatalf("verdict = %q, want ABOVE GATE", r.Verdict)
	}
}

// THE FAILURE THAT WOULD CLOSE THE ITEM WRONGLY: a run that evicted nothing has not
// demonstrated a 0% re-fetch rate — it never asked the question. A float64 zero here would be
// indistinguishable from "compaction is perfect".
func TestNothingEvictedIsNotAZeroPercentResult(t *testing.T) {
	var p PagerStats
	p.NoteFetched(bigPayload("never evicted"))
	r := p.Report()
	if r.Basis != "insufficient_data" {
		t.Fatalf("basis = %q, want insufficient_data", r.Basis)
	}
	if r.RefetchRate != nil {
		t.Fatalf("RefetchRate = %v, want nil — nothing was evicted, so there is no rate", *r.RefetchRate)
	}
	if strings.Contains(r.Verdict, "BELOW GATE") {
		t.Fatal("an unexercised run produced a gate verdict")
	}
}

// Ordering IS the measurement. Content fetched BEFORE any eviction is not a re-fetch;
// counting it would manufacture the signal the gate tests for.
func TestFetchBeforeEvictionIsNotARefetch(t *testing.T) {
	var p PagerStats
	p.NoteFetched(bigPayload("a"))
	p.NoteEvicted(bigPayload("a"))
	r := p.Report()
	if r.Refetched != 0 {
		t.Fatalf("Refetched = %d, want 0 — the fetch preceded the eviction", r.Refetched)
	}
}

// A reflowed but identical payload must count as the same content, or the rate reads low for
// a formatting reason and the gate closes on an artefact rather than on agent behaviour.
func TestWhitespaceReflowStillCountsAsTheSameContent(t *testing.T) {
	var p PagerStats
	body := bigPayload("shared")
	p.NoteEvicted(body)
	p.NoteFetched(strings.ReplaceAll(body, " ", "\n  "))
	if r := p.Report(); r.Refetched != 1 {
		t.Fatalf("Refetched = %d, want 1 — reflowed identical content was treated as different", r.Refetched)
	}
}

func TestTrivialPayloadsAreIgnored(t *testing.T) {
	var p PagerStats
	p.NoteEvicted("tiny")
	if r := p.Report(); r.Evictions != 0 || r.Basis != "insufficient_data" {
		t.Fatalf("a sub-threshold payload was counted: %+v", r)
	}
}

func TestPagerIsSafeUnderConcurrency(t *testing.T) {
	var p PagerStats
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.NoteEvicted(bigPayload(string(rune('a'+i)) + string(rune('0'+j%10))))
			}
		}(i)
	}
	wg.Wait()
	if r := p.Report(); r.Evictions != 800 {
		t.Fatalf("Evictions = %d, want 800 — updates lost under concurrency", r.Evictions)
	}
}
