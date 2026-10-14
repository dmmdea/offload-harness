package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// Register D-101 (ADR 0087): the asker's session and run class ride in-process fields, never the wire body. A
// request body must not be able to name its own session or class, and Meta (which a node marshals whole into its
// poll results) must not echo the asker's session id back through them.

func TestRequestNeverCarriesTheOriginOnTheWire(t *testing.T) {
	b, err := json.Marshal(Request{Task: TaskSummarize, Input: "x", OriginSession: "sess-abc", RunClass: "bake"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sess-abc", "bake", "origin_session", "run_class"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("a populated request marshals %q: %s", leak, b)
		}
	}
	var r Request
	if err := json.Unmarshal([]byte(`{"task":"summarize","input":"x","origin_session":"sess-abc","run_class":"bake","OriginSession":"sess-abc","RunClass":"bake"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.OriginSession != "" || r.RunClass != "" {
		t.Fatalf("a body named its own session or class: %+v", r)
	}
}

func TestMetaNeverEchoesTheAskersSessionThroughAResult(t *testing.T) {
	b, err := json.Marshal(Result{OK: true, Meta: Meta{Model: "m", AskerSession: "sess-abc", AskerClass: "bake"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sess-abc", "bake", "origin_session", "run_class", "asker_session", "asker_class", "AskerSession", "AskerClass"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("a populated meta marshals %q: %s", leak, b)
		}
	}
}
