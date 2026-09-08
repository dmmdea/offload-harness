package accelremote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// fakeNode is a fleet node that advertises the given accelerators, records the
// dispatch it receives, and answers the job with the given core.Result body.
type fakeNode struct {
	t        *testing.T
	node     string
	accels   []string
	result   string
	errText  string
	mu       sync.Mutex
	dispatch map[string]any
	auth     string
	srv      *httptest.Server
}

func newFakeNode(t *testing.T, node string, accels []string, result string) *fakeNode {
	f := &fakeNode{t: t, node: node, accels: accels, result: result}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"node_id": node, "accelerators": accels})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.dispatch = env
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"job_id": env["job_id"], "state": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.errText != "" {
			json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "error", "error": f.errText})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(result)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestCallShipsTheImageToTheNodeThatHasTheDevice(t *testing.T) {
	without := newFakeNode(t, "aorus", nil, `{}`)
	with := newFakeNode(t, "lenovo", []string{"coral-edgetpu"}, `{"best":{"label":"Ara macao","score":0.75},"model":"m.tflite"}`)
	img := filepath.Join(t.TempDir(), "parrot.jpg")
	if err := os.WriteFile(img, []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DelegateRemotes = []string{without.srv.URL, with.srv.URL + "/"}
	cfg.FleetAuthToken = "tok"

	out, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"image_path": img, "domain": "birds", "out_path": "C:/somewhere/mask.png"})
	if err != nil {
		t.Fatal(err)
	}
	// Placement: the node WITH the device, never the first one.
	pl, _ := out["placement"].(Placement)
	if pl.Node != "lenovo" || !pl.Remote || pl.Accelerator != "coral-edgetpu" || !strings.HasPrefix(pl.JobID, "accel-") {
		t.Fatalf("placement = %+v", pl)
	}
	if without.dispatch != nil {
		t.Fatal("a node without the device received the dispatch")
	}
	// The result dict is the node's, untouched.
	best, _ := out["best"].(map[string]any)
	if best["label"] != "Ara macao" || out["model"] != "m.tflite" {
		t.Fatalf("result = %v", out)
	}
	// The dispatch carried the bytes, not the caller's path, and dropped the
	// caller-side out_path.
	with.mu.Lock()
	env := with.dispatch
	auth := with.auth
	with.mu.Unlock()
	if env["task_type"] != "accel" || auth != "Bearer tok" {
		t.Fatalf("envelope task_type=%v auth=%q", env["task_type"], auth)
	}
	payload, _ := env["payload"].(map[string]any)
	raw, _ := base64.StdEncoding.DecodeString(payload["image_b64"].(string))
	if string(raw) != "not really a jpeg" || payload["image_name"] != "parrot.jpg" {
		t.Fatalf("payload image = %q / %v", raw, payload["image_name"])
	}
	args, _ := payload["args"].(map[string]any)
	if _, leaked := args["image_path"]; leaked {
		t.Fatal("caller-side image_path leaked to the node")
	}
	if _, leaked := args["out_path"]; leaked {
		t.Fatal("caller-side out_path leaked to the node")
	}
	if args["domain"] != "birds" {
		t.Fatalf("args = %v", args)
	}
}

func TestCallRefusesWhenNoNodeAdvertisesTheDevice(t *testing.T) {
	a := newFakeNode(t, "a", []string{"hailo-8l"}, `{}`)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{a.srv.URL}
	_, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"domain": "birds"})
	if err == nil || !strings.Contains(err.Error(), "no fleet node advertises coral-edgetpu") || !strings.Contains(err.Error(), "hailo-8l") {
		t.Fatalf("err = %v", err)
	}
	if a.dispatch != nil {
		t.Fatal("dispatched to a node that lacks the device")
	}
}

func TestCallRefusesAMissingOrOversizedImageBeforeDispatch(t *testing.T) {
	with := newFakeNode(t, "lenovo", []string{"coral-edgetpu"}, `{}`)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{with.srv.URL}
	if _, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"image_path": filepath.Join(t.TempDir(), "nope.jpg")}); err == nil || !strings.Contains(err.Error(), "must exist on THIS box") {
		t.Fatalf("missing image: err = %v", err)
	}
	big := filepath.Join(t.TempDir(), "big.jpg")
	if err := os.WriteFile(big, make([]byte, 8<<20+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"image_path": big}); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized image: err = %v", err)
	}
	if with.dispatch != nil {
		t.Fatal("a refused call reached the node")
	}
}

func TestCallPassesTheNodesDeferThrough(t *testing.T) {
	with := newFakeNode(t, "lenovo", []string{"coral-edgetpu"}, ``)
	with.errText = "coral-edgetpu: sidecar did not become healthy within 45s"
	cfg := config.Default()
	cfg.DelegateRemotes = []string{with.srv.URL}
	out, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"domain": "birds"})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := out["deferred"].(bool); !d || !strings.Contains(out["reason"].(string), "did not become healthy") {
		t.Fatalf("out = %v", out)
	}
}

func TestCallNeedsRemotes(t *testing.T) {
	if _, err := Call(context.Background(), config.Default(), "coral-edgetpu", "classify", nil); err == nil || !strings.Contains(err.Error(), "delegate_remotes") {
		t.Fatalf("err = %v", err)
	}
}
