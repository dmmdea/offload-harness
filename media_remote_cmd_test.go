package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mediaFakeNode is a hand-written fleet node for the media CLI verbs: health advertising the tasks and routes,
// a dispatch door that records its bodies, a job that is done at once, and one file under /fleet/media.
type mediaFakeNode struct {
	srv  *httptest.Server
	body chan map[string]any
}

func startMediaFakeNode(t *testing.T, tasks []string, jobData, fileName, fileBody string) *mediaFakeNode {
	t.Helper()
	n := &mediaFakeNode{body: make(chan map[string]any, 4)}
	taskJSON, _ := json.Marshal(tasks)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fleet/health":
			fmt.Fprintf(w, `{"node_id":"cli-node","schema_version":1,"supported_task_types":%s,"queue_depth":0}`, taskJSON)
		case r.URL.Path == "/fleet/dispatch" || r.URL.Path == "/fleet/media-job":
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			m["_path"] = r.URL.Path
			n.body <- m
			w.WriteHeader(202)
			fmt.Fprint(w, `{"status":"accepted"}`)
		case strings.HasPrefix(r.URL.Path, "/fleet/jobs/"):
			fmt.Fprintf(w, `{"state":"done","data":%s}`, jobData)
		case r.URL.Path == "/fleet/media/"+fileName:
			fmt.Fprint(w, fileBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// cliConfig writes a config whose only fleet is the given node and whose state lives under the test's dirs.
func cliConfig(t *testing.T, node string, extra map[string]any) (cfgPath, mediaDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mediaDir = filepath.Join(home, "media")
	cfg := map[string]any{
		"delegate_remotes": []string{node}, "media_dir": mediaDir, "fleet_auth_token": "tok",
		"ledger_path": filepath.Join(home, "ledger.jsonl"), "cache_path": filepath.Join(home, "cache.json"),
		"state_dir": filepath.Join(home, "state"),
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	cfgPath = filepath.Join(home, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, mediaDir
}

// captureVerbStdout runs fn and returns what it printed.
func captureVerbStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	runErr := fn()
	w.Close()
	os.Stdout = old
	return <-done, runErr
}

func resultOf(t *testing.T, out string) (data map[string]any, meta map[string]any) {
	t.Helper()
	var res struct {
		OK       bool            `json:"ok"`
		Deferred bool            `json:"deferred"`
		Reason   string          `json:"reason"`
		Data     json.RawMessage `json:"result"`
		Meta     map[string]any  `json:"meta"`
	}
	start := strings.Index(out, "{")
	if start < 0 || json.Unmarshal([]byte(out[start:]), &res) != nil {
		t.Fatalf("no JSON result in %q", out)
	}
	if !res.OK {
		t.Fatalf("the verb did not succeed: %s", out)
	}
	_ = json.Unmarshal(res.Data, &data)
	return data, res.Meta
}

// The four verbs take --route and a repeatable --remote, and a remote run lands the file where the caller asked.
func TestMediaVerbsRouteToAFleetNode(t *testing.T) {
	// image
	n := startMediaFakeNode(t, []string{"image-gen"}, `{"image_path":"/node/media/img-1.png"}`, "img-1.png", "PNGDATA")
	cfgPath, mediaDir := cliConfig(t, n.srv.URL, nil)
	out, err := captureVerbStdout(t, func() error {
		return runGenerateImage([]string{"--config", cfgPath, "--route", "remote", "--remote", n.srv.URL, "--remote", n.srv.URL + "/", "--json", "a red door", "--width", "512"})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, meta := resultOf(t, out)
	if meta["node"] != "cli-node" || meta["placement"] != "remote: forced" {
		t.Fatalf("meta %v", meta)
	}
	if got := data["image_path"]; got != filepath.Join(mediaDir, "img-1.png") {
		t.Fatalf("image_path %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(mediaDir, "img-1.png")); string(b) != "PNGDATA" {
		t.Fatalf("the image was not fetched: %q", b)
	}
	if m := <-n.body; m["_path"] != "/fleet/dispatch" || m["task_type"] != "image-gen" ||
		m["payload"].(map[string]any)["prompt"] != "a red door" || m["payload"].(map[string]any)["width"] != float64(512) {
		t.Fatalf("wire %v", m)
	}

	// run-graph
	n2 := startMediaFakeNode(t, []string{"run-graph"}, `{"image_path":"g-1.png"}`, "g-1.png", "GRAPH")
	cfgPath, mediaDir = cliConfig(t, n2.srv.URL, nil)
	graph := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(graph, []byte(`{"1":{"class_type":"KSampler"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureVerbStdout(t, func() error {
		return runRunGraph([]string{"--config", cfgPath, "--graph", graph, "--route", "remote", "--remote", n2.srv.URL, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, meta = resultOf(t, out)
	if meta["placement"] != "remote: forced" {
		t.Fatalf("run-graph: --route remote must stamp the placement forced, meta %v", meta)
	}
	if got := data["image_path"]; got != filepath.Join(mediaDir, "g-1.png") {
		t.Fatalf("run-graph image_path %v", got)
	}
	if m := <-n2.body; m["task_type"] != "run-graph" || m["payload"].(map[string]any)["graph"] == nil {
		t.Fatalf("run-graph wire %v", m)
	}

	// audio: the positional output path is where the file lands; --remote does not eat a positional.
	n3 := startMediaFakeNode(t, []string{"audio-gen"}, `{"audio_path":"a-1.wav","kind":"voice"}`, "a-1.wav", "WAVDATA")
	cfgPath, _ = cliConfig(t, n3.srv.URL, nil)
	outPath := filepath.Join(t.TempDir(), "narration.wav")
	out, err = captureVerbStdout(t, func() error {
		return runGenerateAudio([]string{"--config", cfgPath, "--route", "remote", "--remote", n3.srv.URL, "--json", outPath, "hola mundo"})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, meta = resultOf(t, out)
	if meta["placement"] != "remote: forced" {
		t.Fatalf("audio: --route remote must stamp the placement forced, meta %v", meta)
	}
	if data["audio_path"] != outPath {
		t.Fatalf("audio_path %v, want %s", data["audio_path"], outPath)
	}
	if m := <-n3.body; m["payload"].(map[string]any)["text"] != "hola mundo" {
		t.Fatalf("audio wire %v", m)
	}

	// video with a still: three positionals and the new flags together; the still travels as a media-job.
	n4 := startMediaFakeNode(t, []string{"video-gen", "media-job"}, `{"video_path":"v-1.mp4"}`, "v-1.mp4", "MP4DATA")
	cfgPath, _ = cliConfig(t, n4.srv.URL, nil)
	dir := t.TempDir()
	still := filepath.Join(dir, "still.png")
	if err := os.WriteFile(still, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...), 0o644); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(dir, "clip.mp4")
	out, err = captureVerbStdout(t, func() error {
		return runGenerateVideo([]string{"--config", cfgPath, "--route", "remote", "--remote", n4.srv.URL, "--json", clip, still, "a slow pan", "--frames", "33"})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, meta = resultOf(t, out)
	if meta["placement"] != "remote: forced" {
		t.Fatalf("video: --route remote must stamp the placement forced, meta %v", meta)
	}
	if data["video_path"] != clip {
		t.Fatalf("video_path %v, want %s", data["video_path"], clip)
	}
	m := <-n4.body
	if m["_path"] != "/fleet/media-job" || m["task_type"] != "video-gen" || m["inputs"].(map[string]any)["still"] != "still.png" || m["bundle_sha256"] == "" {
		t.Fatalf("video wire %v", m)
	}
	if p := m["payload"].(map[string]any); p["prompt"] != "a slow pan" || p["frames"] != float64(33) {
		t.Fatalf("video payload %v", p)
	}
}

// parseGenerateVideo keeps the three positionals when --remote (a value flag) sits among them, collects every
// --remote, and defaults the route to auto.
func TestParseGenerateVideoCarriesRouteAndRemotes(t *testing.T) {
	c, err := parseGenerateVideo([]string{"--remote", "http://192.0.2.1:18811", "out.mp4", "--remote=http://192.0.2.2:18811", "still.png", "--route", "remote", "a prompt"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if c.route != "remote" || len(c.remotes) != 2 || c.remotes[0] != "http://192.0.2.1:18811" || c.remotes[1] != "http://192.0.2.2:18811" {
		t.Fatalf("route %q remotes %v", c.route, c.remotes)
	}
	if c.video.out != "out.mp4" || c.video.still != "still.png" || c.prompt != "a prompt" {
		t.Fatalf("positionals out=%q still=%q prompt=%q", c.video.out, c.video.still, c.prompt)
	}
	d, err := parseGenerateVideo([]string{"out.mp4", "still.png", "a prompt"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if d.route != "auto" || len(d.remotes) != 0 {
		t.Fatalf("defaults: route %q remotes %v", d.route, d.remotes)
	}
}

// The animate-character verb takes --route and --remote like the others, and a remote run sends the
// reference and the driver through the media-job door and lands the clip where the caller asked.
func TestAnimateCharacterVerbRoutesToAFleetNode(t *testing.T) {
	n := startMediaFakeNode(t, []string{"animate", "media-job"}, `{"video_path":"/node/media/a-1.mp4"}`, "a-1.mp4", "ANIMDATA")
	cfgPath, _ := cliConfig(t, n.srv.URL, nil)
	dir := t.TempDir()
	ref := filepath.Join(dir, "fox.png")
	driver := filepath.Join(dir, "dance.mp4")
	if err := os.WriteFile(ref, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 16)...), 0o644); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(dir, "out", "fox.mp4")
	out, err := captureVerbStdout(t, func() error {
		return runAnimateCharacter([]string{"--config", cfgPath, "--route", "remote", "--remote", n.srv.URL, "--json",
			clip, ref, driver, "a fox dancing", "--frames", "33", "--pose-strength", "0.8", "--motion-prompt", "a slow dance"})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, meta := resultOf(t, out)
	if meta["node"] != "cli-node" || meta["placement"] != "remote: forced" {
		t.Fatalf("meta %v", meta)
	}
	if data["video_path"] != clip {
		t.Fatalf("video_path %v, want %s", data["video_path"], clip)
	}
	if b, _ := os.ReadFile(clip); string(b) != "ANIMDATA" {
		t.Fatalf("the clip at out holds %q", b)
	}
	m := <-n.body
	inputs, _ := m["inputs"].(map[string]any)
	p, _ := m["payload"].(map[string]any)
	if m["_path"] != "/fleet/media-job" || m["task_type"] != "animate" || inputs["ref"] != "ref.png" || inputs["driver"] != "driver.mp4" ||
		p["prompt"] != "a fox dancing" || p["frames"] != float64(33) || p["pose_strength"] != "0.8" || p["motion_prompt"] != "a slow dance" {
		t.Fatalf("animate wire %v", m)
	}
}

// Ignoring --route or --remote must not survive: on every media verb a bad route and a remote outside
// delegate_remotes are refused by Run (they never reach a render), and a good route is stamped forced.
func TestEveryMediaVerbPassesRouteAndRemotesToTheRouter(t *testing.T) {
	n := startMediaFakeNode(t, []string{"image-gen"}, `{"image_path":"x.png"}`, "x.png", "X")
	cfgPath, _ := cliConfig(t, n.srv.URL, nil)
	dir := t.TempDir()
	graph := filepath.Join(dir, "g.json")
	if err := os.WriteFile(graph, []byte(`{"1":{"class_type":"X"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	verbs := map[string]func(extra ...string) (string, error){
		"generate-image": func(extra ...string) (string, error) {
			return captureVerbStdout(t, func() error {
				return runGenerateImage(append([]string{"--config", cfgPath, "--json", "a door"}, extra...))
			})
		},
		"generate-video": func(extra ...string) (string, error) {
			return captureVerbStdout(t, func() error {
				return runGenerateVideo(append([]string{"--config", cfgPath, "--json", filepath.Join(dir, "o.mp4"), filepath.Join(dir, "s.png"), "pan"}, extra...))
			})
		},
		"generate-audio": func(extra ...string) (string, error) {
			return captureVerbStdout(t, func() error {
				return runGenerateAudio(append([]string{"--config", cfgPath, "--json", filepath.Join(dir, "o.wav"), "hola"}, extra...))
			})
		},
		"run-graph": func(extra ...string) (string, error) {
			return captureVerbStdout(t, func() error {
				return runRunGraph(append([]string{"--config", cfgPath, "--json", "--graph", graph}, extra...))
			})
		},
		"animate-character": func(extra ...string) (string, error) {
			return captureVerbStdout(t, func() error {
				return runAnimateCharacter(append([]string{"--config", cfgPath, "--json", filepath.Join(dir, "o.mp4"), filepath.Join(dir, "r.png"), filepath.Join(dir, "d.mp4"), "a fox"}, extra...))
			})
		},
	}
	reason := func(t *testing.T, out string) string {
		t.Helper()
		var res struct {
			Deferred bool   `json:"deferred"`
			Reason   string `json:"reason"`
		}
		start := strings.Index(out, "{")
		if start < 0 || json.Unmarshal([]byte(out[start:]), &res) != nil {
			t.Fatalf("no JSON result in %q", out)
		}
		if !res.Deferred {
			t.Fatalf("expected a defer, got %q", out)
		}
		return res.Reason
	}
	for name, run := range verbs {
		t.Run(name, func(t *testing.T) {
			out, err := run("--route", "sideways")
			if err != nil {
				t.Fatal(err)
			}
			if r := reason(t, out); !strings.Contains(r, "unrecognized route") {
				t.Errorf("--route did not reach the router: %s", r)
			}
			out, err = run("--route", "remote", "--remote", "http://192.0.2.77:18811")
			if err != nil {
				t.Fatal(err)
			}
			if r := reason(t, out); !strings.Contains(r, "not in delegate_remotes") {
				t.Errorf("--remote did not reach the router: %s", r)
			}
		})
	}
	// And the one verb whose job needs no input file places it: --route remote is stamped forced.
	out, err := verbs["generate-image"]("--route", "remote", "--remote", n.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, meta := resultOf(t, out); meta["placement"] != "remote: forced" {
		t.Fatalf("meta %v", meta)
	}
}
