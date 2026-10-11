package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// F43 lets a llama.cpp seat write a list of OBJECTS (before it, an object compiled to a string, and the
// scalar loop detector covered everything the seat could write). The loop guard of C14 has to read an
// object as an item too: a complete object whose list repeats one object closed its own loop before the
// cap, validates, and was published as the structured result.

func objectList(item string, n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(item+",", n), ",") + "]"
}

func TestDegenerateListSeesAListOfObjects(t *testing.T) {
	label := `{"name":"x","score":0.5,"kind":"a"}`
	other := `{"name":"y","score":2,"kind":"b"}`
	cases := []struct {
		name    string
		content string
		source  string
		flagged bool
	}{
		// The threshold is pinned by literals, as for the scalar list: 6 repeats read as a loop, 5 do not.
		{"eight identical objects the answer holds once", `{"labels":` + objectList(label, 8) + `}`, "Label x (a).", true},
		{"six identical objects, at the threshold", `{"labels":` + objectList(label, 6) + `}`, "Label x (a).", true},
		{"five identical objects, under it", `{"labels":` + objectList(label, 5) + `}`, "Label x (a).", false},
		{"keys written in another order are the same object", `{"labels":[` + strings.TrimSuffix(strings.Repeat(`{"kind":"a","score":0.5,"name":"x"},{"name":"x","kind":"a","score":0.5},`, 4), ",") + `]}`, "Label x (a).", true},
		{"an alternating pair of objects, six times", `{"labels":` + objectList(label+","+other, 6) + `}`, "Labels x and y.", true},
		{"distinct objects", `{"labels":[` + label + `,` + other + `,{"name":"z","score":1,"kind":"a"},{"name":"w","score":1,"kind":"b"},{"name":"v","score":1,"kind":"a"},{"name":"u","score":1,"kind":"b"},{"name":"t","score":1,"kind":"a"}]}`, "x", false},
		// An answer that really holds the repeat is spared, as for a scalar: the object's own words
		// (its longest string) appear in the answer as often as the object was written.
		{"identical rows the answer holds as often", `{"rows":` + objectList(`{"name":"unknown","n":1}`, 7) + `}`, strings.Repeat("unknown ", 7), false},
		{"identical rows the answer holds once", `{"rows":` + objectList(`{"name":"unknown","n":1}`, 7) + `}`, "unknown", true},
		{"an object with no string in it is judged by its repeat alone", `{"points":` + objectList(`{"x":3,"y":4}`, 8) + `}`, "3 4", true},
		// An empty item is the schema's empty value, never a loop.
		{"empty objects", `{"rows":` + objectList(`{}`, 10) + `}`, "", false},
		{"objects holding only empty values", `{"rows":` + objectList(`{"name":"","n":0,"ok":false,"tags":[]}`, 10) + `}`, "", false},
		{"lists of lists, repeated", `{"grid":` + objectList(`["a","b"]`, 8) + `}`, "a b", true},
		// A list at depth is read too: the grammar writes nested objects, and so does a loop.
		{"a repeated object in a list of a nested object", `{"meta":{"labels":` + objectList(label, 8) + `}}`, "Label x (a).", true},
		{"a repeated string in a list of a nested object", `{"meta":{"tags":` + objectList(`"a"`, 8) + `}}`, "tag a", true},
		{"a repeated string in a list two objects deep", `{"meta":{"owner":{"tags":` + objectList(`"a"`, 8) + `}}}`, "tag a", true},
		{"a repeated string in a list inside a list of objects", `{"rows":[{"tags":` + objectList(`"a"`, 8) + `}]}`, "tag a", true},
		{"a nested list that is not repeated", `{"meta":{"tags":["a","b","c","a","b","c","a"]}}`, "a b c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := degenerateList([]byte(tc.content), tc.source)
			if (got != "") != tc.flagged {
				t.Fatalf("degenerateList = %q, flagged want %v", got, tc.flagged)
			}
		})
	}
}

// What the guard says names the field (with its path at depth) and stays short: a JSON object is
// long, and the text lands in a ledger row, a defer reason and the node log.
func TestDegenerateListNamesTheFieldAndClipsAnObject(t *testing.T) {
	long := `{"name":"` + strings.Repeat("w", 400) + `","score":1}`
	got := degenerateList([]byte(`{"meta":{"labels":`+objectList(long, 8)+`}}`), "one")
	if !strings.Contains(got, `"meta.labels"`) {
		t.Fatalf("the finding does not name the nested field: %q", got)
	}
	if len(got) > 500 {
		t.Fatalf("the finding is %d bytes: a repeated object must be clipped, got %q", len(got), got)
	}
	if !strings.Contains(got, "8 times in a row") {
		t.Fatalf("the finding does not count the repeats: %q", got)
	}
}

// loopyObjectSeat answers every re-pack request the way a seat does that obeys the grammar and
// loops inside it: without the frequency penalty a COMPLETE, schema-valid object whose `labels`
// list holds the same object eight times, with it the object the answer holds.
func loopyObjectSeat(answer, loop, clean string) *agentFake {
	respond := func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
		out := loop
		if askedNoRepeat(body) {
			out = clean
		}
		content, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":60}}`))
	}
	return &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat(answer) },
		repackStream: respond,
		chatStream:   respond,
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
}

// End to end, on the schema F43 compiles to real objects: the self-closed loop is not published, the
// next attempt is told not to repeat, and what it writes is delivered.
func TestARepackedListOfObjectsThatLoopsIsNotPublished(t *testing.T) {
	label := `{"name":"x","score":0.5,"kind":"a"}`
	loop := `{"title":"t","labels":` + objectList(label, 8) + `,"meta":{"source":"s","review_notes":["n1"],"owner":{"id":7,"active":true}}}`
	fake := loopyObjectSeat("Two labels: x (a) and y (b). Source s.", loop, nestedRepackAnswer)
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.OutputSchema = json.RawMessage(nestedRepackSchema)

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	var got, want any
	_ = json.Unmarshal(wire.Structured, &got)
	_ = json.Unmarshal([]byte(nestedRepackAnswer), &want)
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("structured = %s, want the object the conditioned attempt wrote: %s", gb, wb)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || !strings.Contains(d[0].Why, "repeated the list item") || !strings.Contains(d[1].Lane, "no-repeat") {
		t.Fatalf("detail = %+v, want a first attempt refused as a loop, then one whose lane says it was told not to repeat", d)
	}
}
