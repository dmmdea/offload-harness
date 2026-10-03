package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// Resumable is a fact about the door that admitted a call, never something a caller can assert:
// a remote delegator's payload must not be able to make a node keep a place for it. It is
// neither decoded from nor encoded to JSON.
func TestResumableNeverCrossesTheWire(t *testing.T) {
	for _, body := range []string{`{"task":"image-gen","resumable":true}`, `{"task":"image-gen","Resumable":true}`} {
		var r Request
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		if r.Resumable {
			t.Errorf("%s decoded to Resumable=true: a payload must not be able to ask for a place in line", body)
		}
	}
	b, err := json.Marshal(Request{Task: "image-gen", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "resumable") {
		t.Errorf("Resumable was encoded: %s", b)
	}
}
