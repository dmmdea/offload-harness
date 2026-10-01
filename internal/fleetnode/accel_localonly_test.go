package fleetnode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Register E-08 (operator decision J-13): a standalone accelerator box stays local-only. The
// Hailo-8L is never published in /fleet/health, never opens the `accel` lane, and no accel job is
// accepted for it. The decision is PER DEVICE: another box advertises the Coral and another the
// RKNPU, and each must still be published, advertised and accepted — a guard keyed on "any
// accelerator" or on the standalone box itself would strip a device the fleet routes work to.

// accelNodeCfg is a config whose own accelerators list is exactly ids.
func accelNodeCfg(t *testing.T, ids ...string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Accelerators = ids
	cfg.Home = t.TempDir() // BaseDir() = Home; keep job dirs in the test's tmp
	return cfg
}

// accelNodeOpts is the server's static identity with a manifest list of ids, as fleet-serve builds it.
func accelNodeOpts(ids ...string) *Options {
	return &Options{
		NodeID:       "accel-node",
		Snapshot:     goodSnapshot,
		GpuVendor:    "nvidia",
		GpuArch:      "ampere",
		Accelerators: ids,
	}
}

func healthAccelerators(t *testing.T, s *Server) (accs []string, present bool, tasks []string) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	raw, present := m["accelerators"]
	if present {
		list, ok := raw.([]any)
		if !ok {
			t.Fatalf("accelerators = %v, want a list", raw)
		}
		for _, a := range list {
			accs = append(accs, a.(string))
		}
	}
	if list, ok := m["supported_task_types"].([]any); ok {
		for _, tt := range list {
			tasks = append(tasks, tt.(string))
		}
	}
	return accs, present, tasks
}

// What the node tells the fleet, device by device: the accelerators list and the `accel` task
// are read from the same health payload, so a delegator can never see a lane the list denies.
func TestAccelLocalOnlyHealthPublishesPerDevice(t *testing.T) {
	cases := []struct {
		name   string
		listed []string
		want   []string // nil: the accelerators key is absent
		accel  bool     // "accel" in supported_task_types
	}{
		{"the standalone device alone is not published and opens no accel lane", []string{"hailo-8l"}, nil, false},
		{"beside the Coral only the Coral is published", []string{"hailo-8l", "coral-edgetpu"}, []string{"coral-edgetpu"}, true},
		{"the order of the listing does not matter", []string{"coral-edgetpu", "hailo-8l"}, []string{"coral-edgetpu"}, true},
		{"beside the RKNPU only the RKNPU is published", []string{"hailo-8l", "rknpu"}, []string{"rknpu"}, true},
		{"the Coral alone is published", []string{"coral-edgetpu"}, []string{"coral-edgetpu"}, true},
		{"the RKNPU alone is published", []string{"rknpu"}, []string{"rknpu"}, true},
		{"the Coral and the RKNPU keep their order", []string{"coral-edgetpu", "rknpu"}, []string{"coral-edgetpu", "rknpu"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestServer(t, accelNodeCfg(t, c.listed...), &fakeRunner{}, accelNodeOpts(c.listed...))
			accs, present, tasks := healthAccelerators(t, s)
			if c.want == nil {
				if present {
					t.Fatalf("accelerators key must be absent, got %v", accs)
				}
			} else if !reflect.DeepEqual(accs, c.want) {
				t.Fatalf("accelerators = %v (present %v), want %v", accs, present, c.want)
			}
			if got := slices.Contains(tasks, "accel"); got != c.accel {
				t.Fatalf("accel advertised = %v, want %v (supported_task_types %v)", got, c.accel, tasks)
			}
		})
	}
}

// The list the verb hands the server comes from the installer manifest, which can name the device
// while the config does not: the guard is on the list the server publishes, not on the config's.
func TestAccelLocalOnlyHealthFiltersTheManifestList(t *testing.T) {
	s, _ := newTestServer(t, config.Default(), &fakeRunner{}, accelNodeOpts("hailo-8l", "rknpu"))
	accs, _, _ := healthAccelerators(t, s)
	if !reflect.DeepEqual(accs, []string{"rknpu"}) {
		t.Fatalf("accelerators = %v, want [rknpu]", accs)
	}
}

// Advertisement and admission are one predicate (the agent lane's rule): SupportedTasksFor is what
// health and the pull door advertise, and it lists `accel` exactly when a device the fleet may
// use is carried.
func TestAccelLocalOnlyTaskAdvertisement(t *testing.T) {
	for _, c := range []struct {
		listed []string
		want   bool
	}{
		{nil, false},
		{[]string{"hailo-8l"}, false},
		{[]string{"hailo-8l", "coral-edgetpu"}, true},
		{[]string{"coral-edgetpu"}, true},
		{[]string{"rknpu"}, true},
		{[]string{"hailo-8l", "rknpu"}, true},
	} {
		cfg := accelNodeCfg(t, c.listed...)
		if got := slices.Contains(SupportedTasksFor(cfg, true), "accel"); got != c.want {
			t.Errorf("accelerators %v: accel advertised = %v, want %v", c.listed, got, c.want)
		}
	}
}

// Acceptance: an accel job names one device, and the standalone one is refused wherever it sits.
// A node that carries ONLY that device does not run the lane at all, so the refusal there is the
// ordinary "unsupported task_type" any unbound task gets — it says nothing about what the box has.
func TestAccelLocalOnlyAcceptance(t *testing.T) {
	cases := []struct {
		name      string
		listed    []string
		ask       string
		wantOK    bool
		errHas    string
		errHasNot string
	}{
		{"a node carrying only the standalone device runs no accel lane", []string{"hailo-8l"}, "hailo-8l", false, `unsupported task_type "accel"`, "local-only"},
		{"the standalone device is refused beside the Coral", []string{"hailo-8l", "coral-edgetpu"}, "hailo-8l", false, "local-only", ""},
		{"the standalone device is refused beside the RKNPU", []string{"rknpu", "hailo-8l"}, "hailo-8l", false, "local-only", ""},
		{"asking for it of a node that never listed it is refused too", []string{"coral-edgetpu"}, "hailo-8l", false, "local-only", ""},
		{"the Coral is accepted beside the standalone device", []string{"hailo-8l", "coral-edgetpu"}, "coral-edgetpu", true, "", ""},
		{"the RKNPU is accepted beside the standalone device", []string{"hailo-8l", "rknpu"}, "rknpu", true, "", ""},
		{"the Coral alone is accepted", []string{"coral-edgetpu"}, "coral-edgetpu", true, "", ""},
		{"the RKNPU alone is accepted", []string{"rknpu"}, "rknpu", true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := accelNodeCfg(t, c.listed...)
			payload, _ := json.Marshal(map[string]any{"accelerator": c.ask, "tool": "classify"})
			req, cleanup, err := BuildRequest(t.Context(), cfg, true, "accel", payload)
			defer cleanup()
			if c.wantOK {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if req.Params["accelerator"] != c.ask {
					t.Fatalf("request = %+v, want accelerator %q", req, c.ask)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a job for %q on a node listing %v", c.ask, c.listed)
			}
			if c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("error = %q, want it to contain %q", err, c.errHas)
			}
			if c.errHasNot != "" && strings.Contains(err.Error(), c.errHasNot) {
				t.Errorf("error = %q, must not contain %q", err, c.errHasNot)
			}
			entries, _ := os.ReadDir(filepath.Join(cfg.BaseDir(), "pipeline-jobs"))
			if len(entries) != 0 {
				t.Fatalf("a refused job left dirs behind: %v", entries)
			}
		})
	}
}

// A 400 goes back to a fleet caller, so it must not list what the box carries beyond what the
// fleet may use: the standalone device's presence is not the fleet's business.
func TestAccelLocalOnlyRefusalDoesNotListTheStandaloneDevice(t *testing.T) {
	cfg := accelNodeCfg(t, "hailo-8l", "coral-edgetpu")
	payload, _ := json.Marshal(map[string]any{"accelerator": "rknpu", "tool": "classify"})
	_, cleanup, err := BuildRequest(t.Context(), cfg, true, "accel", payload)
	defer cleanup()
	if err == nil {
		t.Fatal("an unlisted accelerator was accepted")
	}
	if strings.Contains(err.Error(), "hailo-8l") {
		t.Fatalf("the refusal names the standalone device: %q", err)
	}
	if !strings.Contains(err.Error(), "coral-edgetpu") {
		t.Fatalf("the refusal should still list what the fleet may use: %q", err)
	}
}

// The push door end to end: the standalone device's job is a 400 that never reaches the runner,
// the Coral's is admitted, and a node that carries only the standalone device has no accel lane.
func TestAccelLocalOnlyDispatchOverHTTP(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestServer(t, accelNodeCfg(t, "hailo-8l", "coral-edgetpu"), runner, accelNodeOpts("hailo-8l", "coral-edgetpu"))
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"accel-h1","task_type":"accel","payload":{"accelerator":"hailo-8l","tool":"face_detect"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "local-only")
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("the runner saw %d request(s) for the standalone device", n)
	}
	rec = do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"accel-c1","task_type":"accel","payload":{"accelerator":"coral-edgetpu","tool":"classify"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the Coral's job = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(runner.requests()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the runner never saw the Coral's job")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := runner.requests()[0].Params["accelerator"]; got != "coral-edgetpu" {
		t.Fatalf("the runner's job names %v, want coral-edgetpu", got)
	}

	only, _ := newTestServer(t, accelNodeCfg(t, "hailo-8l"), &fakeRunner{}, accelNodeOpts("hailo-8l"))
	rec = do(t, only, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"accel-h2","task_type":"accel","payload":{"accelerator":"hailo-8l","tool":"face_detect"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, `unsupported task_type "accel"`)
}

// The pull door hand-writes the same sequence as the push door (the drift class the register keeps
// recording), so it gets the same proof: a pulled job for the standalone device is nacked at
// build, a pulled Coral job runs, and the claim names `accel` only when the node serves it.
func TestAccelLocalOnlyPulledJobs(t *testing.T) {
	runner := &fakeRunner{}
	cfg := accelNodeCfg(t, "hailo-8l", "coral-edgetpu")
	s, jobs := newTestServer(t, cfg, runner, &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		LoopbackListener: true,
		Accelerators:     []string{"hailo-8l", "coral-edgetpu"},
	})
	var mu sync.Mutex
	var nacks []map[string]any
	var claimed []map[string]any
	queue := []fleetqueue.Job{
		{ID: "pulled-hailo", TaskType: "accel", Payload: json.RawMessage(`{"accelerator":"hailo-8l","tool":"face_detect"}`)},
		{ID: "pulled-coral", TaskType: "accel", Payload: json.RawMessage(`{"accelerator":"coral-edgetpu","tool":"classify"}`)},
	}
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/fleet/queue/claim":
			claimed = append(claimed, m)
			if len(queue) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			job := queue[0]
			queue = queue[1:]
			_ = json.NewEncoder(w).Encode(job)
		case "/fleet/queue/nack":
			nacks = append(nacks, m)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}

	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-hailo" {
		t.Fatalf("claimOne = %q, %v", id, ok)
	}
	// Read the holder's record under its lock and assert outside it: a failing assertion must not
	// leave the handler goroutines blocked behind a lock the deferred Close is waiting on.
	mu.Lock()
	gotNacks := append([]map[string]any(nil), nacks...)
	mu.Unlock()
	if len(gotNacks) != 1 {
		t.Fatalf("nacks = %v, want exactly one, for pulled-hailo", gotNacks)
	}
	if reason, _ := gotNacks[0]["reason"].(string); gotNacks[0]["job_id"] != "pulled-hailo" || !strings.Contains(reason, "local-only") {
		t.Fatalf("nack = %v, want one for pulled-hailo naming the local-only rule", gotNacks[0])
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("the runner saw %d request(s) for a pulled standalone-device job", n)
	}

	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-coral" {
		t.Fatalf("claimOne = %q, %v", id, ok)
	}
	waitJobState(t, jobs, "pulled-coral", JobDone)
	if reqs := runner.requests(); len(reqs) != 1 || reqs[0].Params["accelerator"] != "coral-edgetpu" {
		t.Fatalf("runner requests = %+v, want one for coral-edgetpu", reqs)
	}

	// The claim advertises s.tasks: `accel` for the node that serves the Coral, not for a node that
	// carries only the standalone device.
	only := accelNodeCfg(t, "hailo-8l")
	so, _ := newTestServer(t, only, &fakeRunner{}, &Options{NodeID: "testnode", Snapshot: goodSnapshot, LoopbackListener: true, Accelerators: []string{"hailo-8l"}})
	so.claimOne(context.Background(), client, holder.URL, "testnode", only)
	mu.Lock()
	claims := append([]map[string]any(nil), claimed...)
	mu.Unlock()
	if len(claims) != 3 {
		t.Fatalf("holder saw %d claims, want 3", len(claims))
	}
	if first, _ := claims[0]["task_types"].([]any); !slices.Contains(toStrings(first), "accel") {
		t.Fatalf("claim task_types = %v, want accel for a node serving the Coral", first)
	}
	if last, _ := claims[2]["task_types"].([]any); slices.Contains(toStrings(last), "accel") {
		t.Fatalf("claim task_types = %v, a node carrying only the standalone device must not claim accel", last)
	}
}

func toStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
