package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// WouldBlockUpstream is the non-waiting twin of AwaitUpstream's fence: it answers, without sending
// anything to the model route, whether admitting a request for model right now would be refused.
// A route that picks "spill to another node or run here" (the stt auto route) must read the SAME
// verdict the fence will give at the request, so the test runs both over every case and demands
// they agree.
func TestWouldBlockUpstreamAgreesWithTheFence(t *testing.T) {
	cases := []struct {
		name    string
		class   gpulease.Class
		opts    gpulease.Options
		held    bool
		running string // /running body; "" = the default empty roster of running models
		want    bool
	}{
		{name: "no lease", held: false, want: false},
		{name: "media render, cold model", class: gpulease.ClassMedia, opts: gpulease.Options{Reason: "render"}, held: true, want: true},
		{name: "media render, model resident", class: gpulease.ClassMedia, opts: gpulease.Options{Reason: "render"}, held: true,
			running: `{"running":[{"model":"seat","state":"ready"}]}`, want: false},
		{name: "media render, model starting", class: gpulease.ClassMedia, opts: gpulease.Options{Reason: "render"}, held: true,
			running: `{"running":[{"model":"seat","state":"starting"}]}`, want: true},
		{name: "media render, model stopping", class: gpulease.ClassMedia, opts: gpulease.Options{Reason: "render"}, held: true,
			running: `{"running":[{"model":"seat","state":"stopping"}]}`, want: true},
		{name: "exclusive text hold, cold", class: gpulease.ClassText, opts: gpulease.Options{Reason: "bench", Exclusive: true}, held: true, want: true},
		{name: "plain text reservation", class: gpulease.ClassText, opts: gpulease.Options{Reason: "bench"}, held: true, want: false},
		{name: "draining media hold", class: gpulease.ClassMedia, opts: gpulease.Options{Reason: "render", Draining: true}, held: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := armLease(t)
			s := newStubSwap(t, map[string][]string{"seat": nil})
			if tc.running != "" {
				s.running.Store(tc.running)
			}
			if tc.held {
				holdLease(t, m, tc.class, tc.opts)
			}
			got := WouldBlockUpstream(context.Background(), s.srv.URL, "seat")
			_, err := AwaitUpstream(context.Background(), s.srv.URL, "seat", "/inference", time.Now())
			if fence := IsLeaseRefusal(err); got != fence {
				t.Fatalf("WouldBlockUpstream = %v but the fence's own verdict is %v (err %v): the probe and the fence read different things", got, fence, err)
			}
			if got != tc.want {
				t.Fatalf("WouldBlockUpstream = %v, want %v", got, tc.want)
			}
			if n := s.upstream.Load(); n != 0 {
				t.Fatalf("the probe sent %d request(s) to the auto-loading /upstream route", n)
			}
		})
	}
}

// A probe is one inspection: it never waits for the card, even when the lease is held and the
// model is cold (the fence would wait up to its deadline).
func TestWouldBlockUpstreamNeverWaits(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"seat": nil})
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	start := time.Now()
	if !WouldBlockUpstream(context.Background(), s.srv.URL, "seat") {
		t.Fatal("a held card over a cold model must read as blocking")
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("the probe waited %s", el)
	}
}

// An unreadable /running proves nothing, like the fence: blocking, never an assumed resident model.
func TestWouldBlockUpstreamTreatsUnreadableResidencyAsBlocking(t *testing.T) {
	m := armLease(t)
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if !WouldBlockUpstream(context.Background(), "http://127.0.0.1:1", "seat") {
		t.Fatal("an unreadable residency view must read as blocking")
	}
}

// A gate nobody armed (no lease directory) never blocks.
func TestWouldBlockUpstreamIsInertWhenTheGateIsNotArmed(t *testing.T) {
	leaseMu.Lock()
	leaseDir = ""
	leaseMu.Unlock()
	if WouldBlockUpstream(context.Background(), "http://127.0.0.1:1", "seat") {
		t.Fatal("an unarmed gate blocked")
	}
}
