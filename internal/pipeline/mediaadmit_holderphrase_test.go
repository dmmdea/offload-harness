package pipeline

// How a media call's deferral names the card's holder. The bare class word ("held by media",
// "text holds the lease") read as a SEAT holding the cards, and two sessions argued over who held
// a card on 2026-10-07 (F9); `gpu status` now leads with "a text-class lease"
// (gpulease.Class.LeasePhrase) and the answers the media tools hand back say the same, because a
// media session reads them far more often than it reads `gpu status`.

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A queued answer (a door that can resume a place) names the lease in the way as a lease of a
// class, whichever class holds it: a media render and a text reservation block a media call alike.
func TestAQueuedMediaAnswerNamesTheHolderAsALeaseOfAClass(t *testing.T) {
	for _, class := range []gpulease.Class{gpulease.ClassMedia, gpulease.ClassText} {
		t.Run(string(class), func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
			holder, err := f.m.TryAcquire(class, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()

			res := f.await(f.image(nil))
			if res.Meta.ErrClass != "gpu_queued" {
				t.Fatalf("want a queued answer, got %q: %s", res.Meta.ErrClass, res.Reason)
			}
			if want := "card(s) " + leaseIDOf(admitUUIDC) + ` held by a ` + string(class) + `-class lease ("its card")`; !strings.Contains(res.Reason, want) {
				t.Errorf("the queued answer must say %q:\n%s", want, res.Reason)
			}
			if bare := "held by " + string(class) + " ("; strings.Contains(res.Reason, bare) {
				t.Errorf("the queued answer names the bare class (%q), which reads as a seat holding the cards:\n%s", bare, res.Reason)
			}
		})
	}
}

// The plain busy answer (a door that cannot resume: the CLI verbs, the fleet dispatch, the image
// batch) names it the same way.
func TestAPlainMediaBusyAnswerNamesTheHolderAsALeaseOfAClass(t *testing.T) {
	for _, class := range []gpulease.Class{gpulease.ClassMedia, gpulease.ClassText} {
		t.Run(string(class), func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
			holder, err := f.m.TryAcquire(class, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()

			res := f.await(f.plainImage())
			busyNotQueued(t, res)
			if want := `gpu busy: held by a ` + string(class) + `-class lease (`; !strings.Contains(res.Reason, want) {
				t.Errorf("the busy answer must say %q:\n%s", want, res.Reason)
			}
			if !strings.Contains(res.Reason, `, reason "its card")`) {
				t.Errorf("the busy answer keeps the holder's age and reason:\n%s", res.Reason)
			}
			if bad := string(class) + " holds the lease"; strings.Contains(res.Reason, bad) {
				t.Errorf("the busy answer names the bare class as the holder (%q):\n%s", bad, res.Reason)
			}
		})
	}
}

// The wording itself, pinned without a fixture: a lease of a class with its age and reason; a
// zero Info (the holder released while the answer was being built) is a lease, not "held by  ";
// the in-process variant is untouched.
func TestErrGPUBusyNamesALeaseOfAClassNotABareClass(t *testing.T) {
	text := &errGPUBusy{info: gpulease.Info{Held: true, Class: gpulease.ClassText, Age: 30 * time.Second, Reason: "bench"}}
	if got, want := text.Error(), `gpu busy: held by a text-class lease (30s, reason "bench")`; got != want {
		t.Errorf("text lease: got %q, want %q", got, want)
	}
	media := busyAnswer(nil, &gpulease.ErrHeld{Info: gpulease.Info{Held: true, Class: gpulease.ClassMedia, Age: 3 * time.Minute, Reason: "film"}})
	if got, want := media.Error(), `gpu busy: held by a media-class lease (180s, reason "film")`; got != want {
		t.Errorf("media lease through busyAnswer: got %q, want %q", got, want)
	}
	if got, want := (&errGPUBusy{}).Error(), `gpu busy: held by a lease (0s, reason "")`; got != want {
		t.Errorf("zero Info: got %q, want %q", got, want)
	}
	if got, want := (&errGPUBusy{detail: "another generation job in this process still holds the card after 1m30s"}).Error(),
		"gpu busy: another generation job in this process still holds the card after 1m30s"; got != want {
		t.Errorf("the in-process variant must not change: got %q, want %q", got, want)
	}
}
