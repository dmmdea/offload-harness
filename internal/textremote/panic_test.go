package textremote

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// panicOnPoll lets every request through until the node's job is polled, then panics: the lane dies
// inside its wait, after the dispatch has opened the call's PAIR card.
type panicOnPoll struct{ inner http.RoundTripper }

func (p panicOnPoll) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/fleet/jobs/") {
		panic("poll exploded")
	}
	return p.inner.RoundTrip(r)
}

// A lane that panics after it opened the call's card closes the card (failed, with the panic) and writes
// the call's row before the panic goes on, on both routes that send a call to a node: the door dies of
// the panic, and without the close the card stays queued until a later process's sweep calls it
// "harness process exited".
func TestAPanicAfterTheDispatchClosesTheCardBeforeItGoesOn(t *testing.T) {
	for _, route := range []string{"remote", "auto"} {
		t.Run(route, func(t *testing.T) {
			setBusy(t, route == "auto")
			node := newFakeNode(t, "rk-node-fleet", []string{"text"}, []string{"classify", "extract"}, remoteOK)
			rig := newPairRig(t, true, hostOf(t, node))
			prev := HTTPClient
			HTTPClient = &http.Client{Transport: panicOnPoll{prev.Transport}, CheckRedirect: prev.CheckRedirect, Timeout: prev.Timeout}
			t.Cleanup(func() { HTTPClient = prev })
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, textReq(), route)
			}()
			if recovered != "poll exploded" {
				t.Fatalf("the lane's panic must go on past the close, got %v", recovered)
			}
			cards := rig.cards()
			if got := fmt.Sprint(cards["failed"]["error"]); cards["queued"] == nil || cards["completed"] != nil || !strings.HasPrefix(got, "panic: poll exploded") {
				t.Fatalf("cards = %v, want the queued card closed failed with the panic", cards)
			}
			if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || !rows[0].CardByCaller || !strings.HasPrefix(rows[0].Reason, "panic: poll exploded") || rows[0].Node == "" {
				t.Fatalf("rows = %+v, want the call's one row carrying the panic", rows)
			}
		})
	}
}
