package pairworkloads

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register D-101 (ADR 0087): the asker's session label and run class ride the request that creates work on a
// fleet node as two headers, sent only when non-empty and only when the ledger's own cleaners accept them, so a
// label valid in the asker's own Record can never be dropped on the wire and an invalid one can never leave the box.

func emitterNaming(session, class string) *Emitter {
	e := New(Config{})
	e.originFn = func() (string, string) { return session, class }
	return e
}

func wireHeadersOf(e *Emitter) http.Header {
	h := http.Header{}
	e.SetWireHeaders(h)
	return h
}

func TestSetWireHeadersSendsTheOriginAndRunClass(t *testing.T) {
	h := wireHeadersOf(emitterNaming("sess-abc", "bake"))
	if got := h.Get(core.OriginHeader); got != "sess-abc" {
		t.Fatalf("%s = %q, want sess-abc", core.OriginHeader, got)
	}
	if got := h.Get(core.RunClassHeader); got != "bake" {
		t.Fatalf("%s = %q, want bake", core.RunClassHeader, got)
	}
}

// Nothing to say, nothing sent: an asker with no session and no class sends neither key, so its bytes are the ones
// every older asker sent.
func TestSetWireHeadersSendsNothingWhenThereIsNothingToSay(t *testing.T) {
	h := wireHeadersOf(emitterNaming("", ""))
	for _, k := range []string{core.OriginHeader, core.RunClassHeader} {
		if v, present := h[http.CanonicalHeaderKey(k)]; present {
			t.Errorf("%s present (%v) with nothing to send", k, v)
		}
	}
	// A session alone is sent without a class, and the reverse.
	h = wireHeadersOf(emitterNaming("sess-abc", ""))
	if _, present := h[http.CanonicalHeaderKey(core.RunClassHeader)]; present || h.Get(core.OriginHeader) != "sess-abc" {
		t.Errorf("session only: %v", h)
	}
	h = wireHeadersOf(emitterNaming("", "bake"))
	if _, present := h[http.CanonicalHeaderKey(core.OriginHeader)]; present || h.Get(core.RunClassHeader) != "bake" {
		t.Errorf("class only: %v", h)
	}
}

// The sender cleans by the ledger's rule: a label the ledger would refuse never leaves the box. Exactly at the cap
// it is kept; one byte over it is not sent at all (never cut to length, which would name another session).
func TestSetWireHeadersSendsOnlyALabelTheLedgerAccepts(t *testing.T) {
	cases := []struct {
		name, session string
		want          string
	}{
		{"96 bytes is kept", strings.Repeat("s", 96), strings.Repeat("s", 96)},
		{"97 bytes is not sent", strings.Repeat("s", 97), ""},
		{"non-ASCII is not sent", "sess-café", ""},
		{"a control character is not sent", "sess\x1b[31m", ""},
		{"padding is trimmed", "  sess-abc  ", "sess-abc"},
	}
	for _, c := range cases {
		h := wireHeadersOf(emitterNaming(c.session, ""))
		if got := h.Get(core.OriginHeader); got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.name, core.OriginHeader, got, c.want)
		}
	}
}

// "production" is no class and is never sent; a clean class is normalised the way the ledger stamps it.
func TestSetWireHeadersNeverSendsProductionAndNormalisesTheClass(t *testing.T) {
	cases := []struct{ class, want string }{
		{"production", ""},
		{"prod", ""},
		{"Bake ", "bake"},
		{"x y", ""},
		{strings.Repeat("c", 33), ""},
	}
	for _, c := range cases {
		h := wireHeadersOf(emitterNaming("sess-abc", c.class))
		if got := h.Get(core.RunClassHeader); got != c.want {
			t.Errorf("class %q: %s = %q, want %q", c.class, core.RunClassHeader, got, c.want)
		}
	}
}

// With no seam the asker names what this process's environment names, by the resolvers the ledger stamps its own
// rows with: the explicit label over the inherited session id, and the process class (LOCAL_OFFLOAD_RUN_CLASS, or a
// class-prefixed label). A nil emitter, and WireHeadersFor from a config alone (the remote lanes' request builders),
// read the same environment and do not panic.
func TestWireHeadersNameTheProcessEnvironmentWhenNoSeamIsSet(t *testing.T) {
	clearRunClassEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-claude")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "sess-explicit")
	t.Setenv(ledger.EnvRunClass, "soak")

	var nilEmitter *Emitter
	for name, send := range map[string]func(http.Header){
		"nil emitter":    nilEmitter.SetWireHeaders,
		"real emitter":   New(Config{}).SetWireHeaders,
		"WireHeadersFor": func(h http.Header) { WireHeadersFor(config.Config{}, h) },
	} {
		h := http.Header{}
		send(h)
		if h.Get(core.OriginHeader) != "sess-explicit" || h.Get(core.RunClassHeader) != "soak" {
			t.Errorf("%s: origin %q class %q, want sess-explicit and soak", name, h.Get(core.OriginHeader), h.Get(core.RunClassHeader))
		}
	}

	// A class-prefixed label is a process class of its own, with no variable set.
	clearRunClassEnv(t)
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "bake:run-7")
	h := http.Header{}
	New(Config{}).SetWireHeaders(h)
	if h.Get(core.OriginHeader) != "bake:run-7" || h.Get(core.RunClassHeader) != "bake" {
		t.Errorf("class-prefixed label: origin %q class %q, want bake:run-7 and bake", h.Get(core.OriginHeader), h.Get(core.RunClassHeader))
	}

	// A bare shell: neither header.
	clearRunClassEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	h = http.Header{}
	New(Config{}).SetWireHeaders(h)
	if h.Get(core.OriginHeader) != "" || h.Get(core.RunClassHeader) != "" {
		t.Errorf("bare environment: origin %q class %q, want neither", h.Get(core.OriginHeader), h.Get(core.RunClassHeader))
	}
}
