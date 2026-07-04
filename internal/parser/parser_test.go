package parser

import (
	"strings"
	"testing"
)

func TestExtractPlain(t *testing.T) {
	d, err := Extract(`{"a":1,"b":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"a"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractFenced(t *testing.T) {
	d, err := Extract("Sure, here you go:\n```json\n{\"label\":\"tech\"}\n```\nhope that helps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"label"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractProseWrapped(t *testing.T) {
	d, err := Extract(`The answer is {"decision":"yes","reason":"ok"} as shown.`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"decision"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractTrailingComma(t *testing.T) {
	if _, err := Extract(`{"a":1,"b":2,}`); err != nil {
		t.Fatalf("trailing-comma repair failed: %v", err)
	}
}

func TestExtractNoJSON(t *testing.T) {
	if _, err := Extract("just some prose, no object here"); err == nil {
		t.Error("expected ErrNoJSON")
	}
}

func TestStripThink(t *testing.T) {
	// object output after a think span
	if got := StripThink("<think>reasoning here</think>\n{\"label\":\"a\"}"); got != `{"label":"a"}` {
		t.Errorf("object case: got %q", got)
	}
	// bare-string output (classify/triage enum) after a think span
	if got := StripThink("<think>r</think>  \"billing\""); got != `"billing"` {
		t.Errorf("enum/string case: got %q", got)
	}
	// no think span -> unchanged
	if got := StripThink(`{"label":"a"}`); got != `{"label":"a"}` {
		t.Errorf("no-think passthrough: got %q", got)
	}
	// a </think> INSIDE the JSON answer must be preserved: split on the FIRST close tag
	// (the grammar guarantees that is the structural separator), not the last.
	const withTag = `<think>reasoning</think>{"reason":"the log said </think> here"}`
	if got := StripThink(withTag); got != `{"reason":"the log said </think> here"}` {
		t.Errorf("answer containing </think>: got %q", got)
	}
	// content with no <think> prefix is left alone even if it contains </think>
	if got := StripThink(`plain text with </think> in it`); got != `plain text with </think> in it` {
		t.Errorf("no-prefix-with-tag passthrough: got %q", got)
	}
}
