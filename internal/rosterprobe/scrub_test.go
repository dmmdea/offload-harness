package rosterprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// marks are what no text about a roster entry may carry, however the credential was spelled.
var marks = []string{"s3cret", "tok%33", "frag-", "user:"}

func noMarks(t *testing.T, what, text string) {
	t.Helper()
	for _, m := range marks {
		if strings.Contains(text, m) {
			t.Errorf("%s carries %q:\n%s", what, m, text)
		}
	}
}

// closedBase is a loopback base with nothing listening, so a dial is refused at once.
func closedBase(t *testing.T) (hostPort string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort = l.Addr().String()
	_ = l.Close()
	return hostPort
}

// A dial error quotes the URL it dialled, the health reader quotes the one it built, and the HTTP client
// re-serializes the URL it dials (password replaced by ***, query and escapes untouched). Scrub replaces the
// configured base with its redacted form and masks every other spelling that is left, and keeps the cause.
func TestScrubRemovesEverySpellingOfACredentialAndKeepsTheCause(t *testing.T) {
	base := "http://user:pw-s3cret@node-a:18811/v1?token=tok%33-s3cret#frag-s3cret"
	cause := "dial tcp: connection refused"
	for name, text := range map[string]string{
		"health reader":    fmt.Sprintf("delegate: health GET %s/fleet/health: Get %q: %s", base, base+"/fleet/health", cause),
		"client re-serial": fmt.Sprintf("Get %q: %s", "http://user:***@node-a:18811/v1?token=tok%33-s3cret#frag-s3cret/fleet/health", cause),
		"query only":       fmt.Sprintf("Post %q: %s", "http://node-a:18811/v1?token=tok%33-s3cret/fleet/dispatch", cause),
		"quoted form":      fmt.Sprintf("%q: %s", base, cause),
		"bare in prose":    "refused " + base + " because " + cause,
		"upper-case":       "GET HTTP://USER:pw-s3cret@NODE-A:18811/?TOKEN=s3cret: " + cause,
	} {
		got := Scrub(base, errors.New(text))
		noMarks(t, name, got)
		if !strings.Contains(got, cause) || !strings.Contains(strings.ToLower(got), "node-a:18811") {
			t.Errorf("%s: scrubbing must keep the host and the cause, got %s", name, got)
		}
		if again := Scrub(base, errors.New(got)); again != got {
			t.Errorf("%s: scrubbing is not idempotent:\n%s\n%s", name, got, again)
		}
	}
	if got := Scrub(base, nil); got != "" {
		t.Errorf("Scrub of a nil error = %q, want empty", got)
	}
	// Nothing to hide: the text is untouched, down to the bytes.
	plain := "delegate: health GET http://node-a:18811/fleet/health: status 503: not today"
	if got := Scrub("http://node-a:18811", errors.New(plain)); got != plain {
		t.Errorf("a clean error was rewritten: %s", got)
	}
}

// Scrubbed keeps the chain: a *url.Error or a timeout inside it is still found by errors.As, so the
// negative cache's classification (unreachable, timedOut) works on the scrubbed error.
func TestScrubbedErrorStillUnwrapsToTheCause(t *testing.T) {
	cause := context.DeadlineExceeded
	err := Scrubbed("http://user:pw-s3cret@node-a:18811", fmt.Errorf("Get http://user:pw-s3cret@node-a:18811/fleet/health: %w", cause))
	if !errors.Is(err, cause) {
		t.Fatal("a scrubbed error must still unwrap to its cause")
	}
	noMarks(t, "scrubbed error", err.Error())
	clean := errors.New("boom")
	if got := Scrubbed("http://node-a:18811", clean); got != clean {
		t.Error("an error with nothing to hide must be returned as it is")
	}
	if Scrubbed("http://node-a:18811", nil) != nil {
		t.Error("nil must stay nil")
	}
}

// The doctor and the lanes print a roster entry's miss. Whatever refuses or fails the entry (the tailnet
// guard by name, by scheme and by shape; a dial refused at the transport; a hung node), no credential it
// was pasted with reaches the text, in Member.Miss, Reading.Miss, Refusal.Error or Reading.Err.
func TestNoMissOrErrorEverCarriesACredentialFromARosterEntry(t *testing.T) {
	rostertest.Zones(t, ownZone)
	closed := closedBase(t)
	hole := rostertest.NewBlackHole(t)
	holeHost := strings.TrimPrefix(hole.URL(), "http://")
	const (
		ui = "user:pw-s3cret@"
		q  = "?token=tok%33-s3cret&k=plain-s3cret#frag-s3cret"
	)
	remotes := []string{
		"http://" + ui + "node-x." + otherZone + ":18811/" + q, // refused: a zone nobody listed
		"http://" + ui + "example.com:18811" + q,               // refused: a public name
		"ftp://" + ui + "node-a:21/" + q,                       // refused: scheme
		"user:pw-s3cret@node-a:18811",                          // refused: reads as scheme "user"
		"tok-s3cret@node-a:18811",                              // refused: does not parse
		"http://" + ui + closed + q,                            // admitted, refused at the transport
		"http://" + closed + q,                                 // query only, refused at the transport
		"http://" + ui + holeHost + q,                          // admitted, never answers
	}
	got := NewCache().Probe(context.Background(), remotes, "the-fleet-bearer", 300*time.Millisecond)
	if len(got) != len(remotes) {
		t.Fatalf("got %d readings, want %d", len(got), len(remotes))
	}
	for i, r := range got {
		if r.Err == nil {
			t.Fatalf("reading %d (%s) did not fail; the test needs every entry to produce a message", i, r.Shown())
		}
		noMarks(t, fmt.Sprintf("reading %d Miss", i), r.Miss())
		noMarks(t, fmt.Sprintf("reading %d Err", i), r.Err.Error())
		if r.Refused != nil {
			noMarks(t, fmt.Sprintf("reading %d Refused", i), r.Refused.Error())
			noMarks(t, fmt.Sprintf("reading %d Member.Miss", i), r.Member.Miss())
			if !IsRefusal(r.Err) || !strings.Contains(r.Miss(), "not dialled") {
				t.Errorf("reading %d: a refused entry must stay a Refusal and say it was not dialled: %s", i, r.Miss())
			}
		}
		if r.Shown() == "" || strings.ContainsAny(r.Shown(), "?#") || strings.Contains(r.Shown(), "@") && !strings.Contains(r.Shown(), "<redacted>@") {
			t.Errorf("reading %d: Shown() = %q must carry no query, fragment or userinfo", i, r.Shown())
		}
	}
	// The transport failures still name their host and say what happened.
	if m := got[5].Miss(); !strings.Contains(m, closed) || !strings.Contains(m, "refused") && !strings.Contains(m, "connect") {
		t.Errorf("the dial failure must still name the node and the cause: %s", m)
	}
	// And a replay from the negative cache is as clean as the first reading.
	c := NewCache()
	c.Probe(context.Background(), remotes[5:7], "", time.Second)
	for i, r := range c.Probe(context.Background(), remotes[5:7], "", time.Second) {
		if r.Source != SourceNegative {
			t.Fatalf("reading %d was not served from the negative cache: %s", i, describe(r))
		}
		noMarks(t, fmt.Sprintf("replayed reading %d Miss", i), r.Miss())
	}
}

// The token probe is a separate request with its own errors. Its transport error never carries the
// credential either, and a refused entry is never sent the bearer.
func TestCheckTokenErrorsNeverCarryACredentialFromTheEntry(t *testing.T) {
	rostertest.Zones(t, ownZone)
	closed := closedBase(t)
	for _, base := range []string{
		"http://user:pw-s3cret@" + closed + "?token=tok%33-s3cret#frag-s3cret",
		"http://" + closed + "/?token=tok%33-s3cret",
		"http://user:pw-s3cret@node-x." + otherZone + ":18811/?token=s3cret", // refused by the guard
	} {
		_, _, err := CheckToken(context.Background(), base, "the-fleet-bearer")
		if err == nil {
			t.Fatalf("CheckToken(%q) succeeded against nothing", base)
		}
		noMarks(t, "CheckToken error for "+base, err.Error())
	}
}

// A Reading or Refusal can come from somewhere other than the cache (a test double, a future lane, the doctor
// building one from a stored error), so Miss scrubs what it prints rather than trusting the producer to have.
func TestMissScrubsAnErrorItWasHandedRaw(t *testing.T) {
	base := "http://user:pw-s3cret@node-a:18811/?token=tok%33-s3cret"
	raw := errors.New(`Get "` + base + `/fleet/health": dial tcp: refused`)

	got := Reading{Member: Member{Index: 2, Base: base}, Err: raw}.Miss()
	noMarks(t, "Reading.Miss for a raw error", got)
	if !strings.Contains(got, "dial tcp: refused") || !strings.Contains(got, "node-a:18811") {
		t.Errorf("the miss must keep the node and the cause: %s", got)
	}

	m := Member{Index: 2, Base: base, Refused: &Refusal{Reason: raw}}
	noMarks(t, "Member.Miss for a raw refusal", m.Miss())
	if !strings.Contains(m.Miss(), "not dialled") {
		t.Errorf("a refused member's miss must say it was not dialled: %s", m.Miss())
	}
}

// Scrub swaps the configured base for its redacted form in place, so the text still reads as the sentence
// the dialler wrote, not as a pile of masks.
func TestScrubPutsTheRedactedBaseWhereTheRawOneWas(t *testing.T) {
	base := "http://user:pw-s3cret@node-a:18811/v1?token=tok%33-s3cret"
	got := Scrub(base, errors.New("delegate: health GET "+base+"/fleet/health: status 503"))
	if want := "delegate: health GET http://node-a:18811/v1/fleet/health: status 503"; got != want {
		t.Errorf("Scrub = %q, want %q", got, want)
	}
}
