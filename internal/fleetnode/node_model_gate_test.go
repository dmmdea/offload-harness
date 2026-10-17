package fleetnode

// A holder of the fleet bearer can have the node render, not write files of its choosing: a run-graph manifest's
// models[] entry is downloaded into the ComfyUI tree before ComfyUI starts, so a path outside models/<class>/ (a
// custom_nodes drop, a ".." climb) is code run on the node, and a source that is not a listed https host is a fetch
// from where the caller chose. The refusal is at the door, before a job, a temp file or a lease exists, for every
// caller, a loopback one with no token included.

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

func modelBody(jobID, path, source string) string {
	entry := `{"path":` + jsonString(path)
	if source != "" {
		entry += `,"source_url":` + jsonString(source)
	}
	entry += `}`
	return `{"job_id":"` + jobID + `","task_type":"run-graph","payload":{"graph":` + plainGraph + `,"manifest":{"node_packs":[],"models":[` + entry + `]}}}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestATokenHolderCannotWriteAFileOutsideTheModelsTree(t *testing.T) {
	hostile := []struct{ name, path, source string }{
		{"a custom node package", "custom_nodes/evil/__init__.py", "https://huggingface.co/x/y"},
		{"a climb out of the install", "../../outside-comfy.txt", "https://huggingface.co/x/y"},
		{"a climb out of a class directory", "models/vae/../../custom_nodes/evil.py", ""},
		{"a fetch from an address of the caller's choosing", "models/vae/x.safetensors", "http://127.0.0.1:9/evil"},
		{"a fetch from a host that is not listed", "models/vae/x.safetensors", "https://example.invalid/x"},
	}
	for _, tc := range []struct {
		name     string
		token    string
		loopback bool
		hdr      map[string]string
	}{
		{"a tokened node, the bearer presented", "s3cret", false, map[string]string{"Authorization": "Bearer s3cret"}},
		{"a loopback node with no token", "", true, nil},
	} {
		for _, h := range hostile {
			t.Run(tc.name+", "+h.name, func(t *testing.T) {
				runner := &fakeRunner{}
				s, jobs := newTestServer(t, tokenCfg(tc.token), runner, authOpts(tc.loopback))
				rec := do(t, s, http.MethodPost, "/fleet/dispatch", modelBody("mf-hostile", h.path, h.source), tc.hdr)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
				}
				msg, _ := decodeMap(t, rec)["error"].(string)
				if !strings.Contains(msg, "model") || !strings.Contains(msg, "not allowed") {
					t.Fatalf("error = %q, want it to name the model and say it is not allowed", msg)
				}
				if _, ok := jobs.Get("mf-hostile"); ok {
					t.Fatal("a refused manifest left a job behind")
				}
				if n := len(runner.requests()); n != 0 {
					t.Fatalf("the runner ran %d request(s)", n)
				}
			})
		}
	}
}

func TestAModelUnderTheModelsTreeFromAListedHostIsAdmitted(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(false))
	hdr := map[string]string{"Authorization": "Bearer s3cret"}
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		modelBody("mf-ok", "models/diffusion_models/wan/high.safetensors", "https://huggingface.co/Org/Repo/resolve/main/high.safetensors"), hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJobWith(t, s, "mf-ok", JobDone, hdr)
}

// The builder is where the manifest is already decoded: it refuses with the typed error, before it writes the graph
// or the manifest to disk.
func TestBuildRequestRefusesAHostileModelBeforeWritingAnyFile(t *testing.T) {
	tmp := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, tmp)
	}
	payload := json.RawMessage(`{"graph":` + plainGraph + `,"manifest":{"node_packs":[],"models":[{"path":"custom_nodes/evil/__init__.py","source_url":"https://huggingface.co/x/y"}]}}`)
	_, cleanup, err := BuildRequest(context.Background(), fullCfg(), true, "run-graph", payload)
	cleanup()
	var refused *mediacap.ManifestModelRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a *mediacap.ManifestModelRefusedError", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("a refused manifest wrote %d file(s) first", len(left))
	}
}
