package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// dispatchLogPage is the contract page for state/dispatch-log.jsonl (register D-74). The file is written by
// two harnesses: the Claude Code hooks (which also rotate it) and the opencode plugin in this repo, tagged
// harness:"opencode". The weekly adherence scoreboard divides by hook-emitted `readonly_spawn` rows and checks
// a `session` heartbeat, so a rewrite of either writer that drops them fails in the reassuring direction: the
// instrument reads "nobody spawned anything" instead of "the instrument is dead". The H15 hook rewrite removed
// both and had to put them back.
const dispatchLogPage = "docs/systems/dispatch-log.md"

// pluginDispatchEvents reads the event names the opencode plugin may write from the DispatchEvent union in
// instrument.ts, so the page is held to the type the writer is compiled against and not to a copy of it.
func pluginDispatchEvents(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("integrations/opencode/src/instrument.ts")
	if err != nil {
		t.Fatal(err)
	}
	union := regexp.MustCompile(`(?s)export type DispatchEvent = \{\s*event:([^;]*);`).FindSubmatch(raw)
	if union == nil {
		t.Fatal("instrument.ts: the DispatchEvent type no longer opens with an `event:` union; this gate went blind")
	}
	var events []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(union[1], -1) {
		events = append(events, string(m[1]))
	}
	if len(events) < 5 {
		t.Fatalf("instrument.ts: parsed only %d events from the DispatchEvent union: %q", len(events), events)
	}
	return events
}

func TestDispatchLogContractPageNamesEveryEventTheWritersEmit(t *testing.T) {
	raw, err := os.ReadFile(strings.ReplaceAll(dispatchLogPage, "/", string(os.PathSeparator)))
	if err != nil {
		t.Fatalf("the dispatch-log contract page is missing: %v", err)
	}
	page := strings.ReplaceAll(string(raw), "\r\n", "\n")

	for _, ev := range pluginDispatchEvents(t) {
		if !strings.Contains(page, "`"+ev+"`") {
			t.Errorf("%s does not name the event `%s` that the opencode plugin may write (instrument.ts DispatchEvent)", dispatchLogPage, ev)
		}
	}
	// The events only the Claude Code hooks write, the two the scoreboard divides by, the file's tag, its one
	// rotation owner and its append-only foreign writer.
	docNeeds(t, dispatchLogPage, page,
		"`lead_turn`",
		"`log_truncated`",
		"`readonly_spawn`",
		"`session`",
		`"harness":"opencode"`,
		"1 MiB",
		"append-only",
		"denominator",
		"heartbeat",
		"unknown events",
	)
}

// The writer's own header and the integration page both point a reader at the contract, so a change to
// either writer is made with the contract in front of it.
func TestDispatchLogContractIsLinkedFromItsWriters(t *testing.T) {
	ts, err := os.ReadFile("integrations/opencode/src/instrument.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ts), "docs/systems/dispatch-log.md") {
		t.Error("integrations/opencode/src/instrument.ts does not point at docs/systems/dispatch-log.md in its header")
	}
	oc, err := os.ReadFile("docs/systems/opencode-integration.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oc), "(dispatch-log.md)") {
		t.Error("docs/systems/opencode-integration.md does not link dispatch-log.md from its Instrument row")
	}
	idx, err := os.ReadFile("docs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"systems/dispatch-log.md", "systems/delegate-contract-file.md"} {
		if !strings.Contains(string(idx), "]("+page+")") {
			t.Errorf("docs/README.md does not index %s", page)
		}
	}
}
