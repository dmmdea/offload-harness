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
	data, _ = resultOf(t, out)
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
	data, _ = resultOf(t, out)
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
	data, _ = resultOf(t, out)
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
