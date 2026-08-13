// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// cli-printing-press: novel-scaffold-test
// Novel command scaffold tests. Keep the wiring smoke test and add behavior cases as needed.

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestNovelSwapsHelpWires smoke-tests that the swaps command
// resolves at runtime and renders useful --help output. Catches wiring
// regressions (missing AddCommand, panicking RunE on --help, etc.) before
// review. Keep this smoke test when adding behavior-specific cases.
func TestNovelSwapsHelpWires(t *testing.T) {
	cmd := RootCmd()
	cmd.SetArgs([]string{"swaps", "--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("swaps --help error = %v (novel command not wired correctly?)", err)
	}
	help := out.String()
	for _, want := range []string{"Usage:", "swaps"} {
		if !strings.Contains(help, want) {
			t.Fatalf("swaps --help missing %q in output:\n%s", want, help)
		}
	}
}

// TestSwapsThrashEmitsEmptyArrayOnColdMirror pins the finding that
// `swaps --thrash --json` was byte-identical to plain `swaps --json` whenever
// no mutual-eviction pair existed: `omitempty` dropped the key, so a consumer
// could not tell "the thrash analysis ran and found nothing" from "the thrash
// analysis never ran". An empty result is a finding and must be visible as
// "thrash": [].
func TestSwapsThrashEmitsEmptyArrayOnColdMirror(t *testing.T) {
	env := newSpineTestEnv(t) // cold mirror: no swap_events seeded at all

	withThrash, _, err := runSpine(t, "swaps", "--thrash", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("swaps --thrash: %v\n%s", err, withThrash)
	}
	envelope := lastJSONObject(t, withThrash)
	raw, present := envelope["thrash"]
	if !present {
		t.Fatalf("--thrash was passed but the \"thrash\" key is ABSENT — an empty analysis must still report itself:\n%s", withThrash)
	}
	pairs, ok := raw.([]any)
	if !ok {
		t.Fatalf("thrash = %#v, want a JSON array", raw)
	}
	if len(pairs) != 0 {
		t.Fatalf("thrash = %v, want an empty array on a cold mirror", pairs)
	}

	// Without the flag the key must stay absent — "not computed" and
	// "computed, empty" are different answers and must not both be [].
	plain, _, err := runSpine(t, "swaps", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("swaps: %v\n%s", err, plain)
	}
	if _, present := lastJSONObject(t, plain)["thrash"]; present {
		t.Fatalf("plain swaps must NOT emit a \"thrash\" key:\n%s", plain)
	}

	// The observable symptom from the review: the two outputs were identical.
	if normalizeJSONForCompare(t, withThrash) == normalizeJSONForCompare(t, plain) {
		t.Fatalf("swaps --thrash --json is still byte-identical to swaps --json:\n%s", withThrash)
	}
}

// normalizeJSONForCompare re-marshals the envelope so the comparison is about
// content, not incidental whitespace or key ordering.
func normalizeJSONForCompare(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(lastJSONObject(t, s))
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(b)
}
