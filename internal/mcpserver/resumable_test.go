package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The MCP server is the one door that returns a queued answer's waiter_token to its caller and
// takes it again on the next call, so it is the one that marks its requests resumable
// (core.Request.Resumable); the pipeline leaves a place in line only for such a request.
func TestEveryMediaDoorMarksItsRequestResumable(t *testing.T) {
	s := New(nil)
	var got []core.Request
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		got = append(got, r)
		return core.Result{OK: true, Data: json.RawMessage(`{}`)}
	}
	for name, d := range mediaDoors(s) {
		got = nil
		if _, err := d.h(context.Background(), callReq(d.args+`}`)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || !got[0].Resumable {
			t.Errorf("%s: request = %+v, want Resumable (this door hands the waiter_token back)", name, got)
		}
	}
}
