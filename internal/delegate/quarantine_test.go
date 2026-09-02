package delegate

import (
	"testing"
	"time"
)

func TestQuarantineFlipsOnTheSecondStrikeAndExpires(t *testing.T) {
	clock := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	q := NewQuarantine(10 * time.Minute)
	q.now = func() time.Time { return clock }

	if q.Strike("http://lenovo:18811") {
		t.Fatal("one strike must not quarantine")
	}
	if q.Blocked("http://lenovo:18811") {
		t.Fatal("not blocked after one strike")
	}
	if !q.Strike("http://lenovo:18811") {
		t.Fatal("the second strike must flip the node into quarantine")
	}
	if !q.Blocked("http://lenovo:18811") {
		t.Fatal("blocked after two strikes")
	}
	if q.Strike("http://lenovo:18811") {
		t.Fatal("a strike while blocked must not report a second flip")
	}
	if q.Blocked("http://aorus:18811") {
		t.Fatal("other nodes are unaffected")
	}
	clock = clock.Add(11 * time.Minute)
	if q.Blocked("http://lenovo:18811") {
		t.Fatal("the block must expire after TTL")
	}
	if !q.Until("http://lenovo:18811").IsZero() {
		t.Fatal("Until must be zero once expired")
	}
}

func TestQuarantineStrikesOutsideTTLDoNotAccumulate(t *testing.T) {
	clock := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	q := NewQuarantine(10 * time.Minute)
	q.now = func() time.Time { return clock }
	q.Strike("n")
	clock = clock.Add(11 * time.Minute)
	if q.Strike("n") {
		t.Fatal("a strike 11 min after the first is a fresh first strike, not a flip")
	}
	if q.Blocked("n") {
		t.Fatal("not blocked")
	}
}

func TestNilQuarantineIsInert(t *testing.T) {
	var q *Quarantine
	if q.Strike("n") || q.Blocked("n") || !q.Until("n").IsZero() {
		t.Fatal("a nil Quarantine records and blocks nothing")
	}
	if NewQuarantine(0).ttl != DefaultQuarantineTTL {
		t.Fatal("ttl <= 0 must use the default")
	}
}
