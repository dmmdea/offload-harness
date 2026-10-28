package fleetnode

// A holder of the fleet bearer is trusted to run renders, not to run code: a run-graph manifest may name a node
// pack only when its repository is allowlisted (render/node-packs.json), because naming one makes the
// node clone the repository and pip-install its requirements. The refusal is at the door, before a job, a
// temp file or a lease exists, and it holds for every caller, a loopback one with no token included.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

const allowedPackBody = `{"job_id":"rg-allowed","task_type":"run-graph","payload":{"graph":` + plainGraph +
	`,"manifest":{"node_packs":[{"name":"ComfyUI-VideoHelperSuite","repo":"https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite"}]}}}`

func TestATokenHolderCannotNameAnUnlistedPack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		loopback bool
		hdr      map[string]string
	}{
		{"a tokened node, the bearer presented", "s3cret", false, map[string]string{"Authorization": "Bearer s3cret"}},
		{"a loopback node with no token", "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			s, jobs := newTestServer(t, tokenCfg(tc.token), runner, authOpts(tc.loopback))
			rec := do(t, s, http.MethodPost, "/fleet/dispatch", hostilePackBody, tc.hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			msg, _ := decodeMap(t, rec)["error"].(string)
			if !strings.Contains(msg, "payload") || !strings.Contains(msg, "allowlisted") {
				t.Fatalf("error = %q, want it to name the pack and say the list is the allowlisted one", msg)
			}
			if _, ok := jobs.Get("rg-hostile"); ok {
				t.Fatal("a refused manifest left a job behind")
			}
			if n := len(runner.requests()); n != 0 {
				t.Fatalf("the runner ran %d request(s)", n)
			}
		})
	}
}

func TestAnAllowlistedPackIsAdmitted(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(false))
	hdr := map[string]string{"Authorization": "Bearer s3cret"}
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", allowedPackBody, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJobWith(t, s, "rg-allowed", JobDone, hdr)
}

// The builder is where the manifest is already decoded: it refuses with the typed error, and before it writes the
// graph or the manifest to disk.
func TestBuildRequestRefusesAnUnlistedPackBeforeWritingAnyFile(t *testing.T) {
	tmp := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, tmp)
	}
	payload := json.RawMessage(`{"graph":` + plainGraph + `,"manifest":{"node_packs":[{"repo":"https://example.invalid/payload"}]}}`)
	_, cleanup, err := BuildRequest(context.Background(), fullCfg(), true, "run-graph", payload)
	cleanup()
	var refused *mediacap.NodePackRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a *mediacap.NodePackRefusedError", err)
	}
	if refused.Pack != "payload" {
		t.Errorf("the refusal names %q, want the pack derived from its repository, \"payload\"", refused.Pack)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("a refused manifest wrote %d file(s) first", len(left))
	}
	if _, cleanup, err := BuildRequest(context.Background(), fullCfg(), true, "run-graph", json.RawMessage(`{"graph":`+plainGraph+`,"manifest":{"node_packs":[]}}`)); err != nil {
		t.Errorf("a manifest with no packs was refused: %v", err)
	} else {
		cleanup()
	}
}
