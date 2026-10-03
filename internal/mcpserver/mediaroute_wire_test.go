package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// wireNode is a fleet node that advertises every media task and the media-job door, records the body of
// every job it is sent, and answers the job with an error so no output is fetched.
type wireNode struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
}

func startWireNode(t *testing.T) *wireNode {
	t.Helper()
	n := &wireNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fleet/health":
			fmt.Fprint(w, `{"node_id":"wire-node","schema_version":1,"queue_depth":0,"supported_task_types":["image-gen","video-gen","animate","audio-gen","run-graph","media-job"]}`)
		case r.URL.Path == "/fleet/dispatch" || r.URL.Path == "/fleet/media-job":
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			n.mu.Lock()
			n.bodies = append(n.bodies, m)
			n.paths = append(n.paths, r.URL.Path)
			n.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"accepted"}`)
		case strings.HasPrefix(r.URL.Path, "/fleet/jobs/"):
			fmt.Fprint(w, `{"state":"error","error":"stop: the wire test needs no render"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *wireNode) last(t *testing.T) (path string, body map[string]any) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.bodies) == 0 {
		t.Fatal("the node received no job")
	}
	return n.paths[len(n.paths)-1], n.bodies[len(n.bodies)-1]
}

// Every parameter an MCP door accepts reaches the node, in the payload, under the field name the node's
// builder decodes (dropping kind=music, family, seed, motion_prompt, ref_strength ... must fail here).
func TestEveryMediaDoorParameterReachesTheWire(t *testing.T) {
	n := startWireNode(t)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	mp4 := append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 16)...)
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 16)...)

	cfg := config.Default()
	cfg.DelegateRemotes = []string{n.srv.URL}
	cfg.FleetAuthToken = "tok"
	cfg.LedgerPath = ""
	s := New(pipeline.New(cfg, nil, nil, nil))
	remote := func(args map[string]any) map[string]any {
		args["route"] = "remote"
		return args
	}

	for _, tc := range []struct {
		name    string
		handler func() string
		path    string
		task    string
		payload map[string]any
		inputs  []string
	}{
		{"image", func() string {
			return callVision(t, s.handleGenerateImage, remote(map[string]any{"prompt": "a door", "negative": "text", "family": "qi21", "transparent": true,
				"width": 512, "height": 768, "steps": 20, "seed": 3}))
		}, "/fleet/dispatch", "image-gen", map[string]any{"prompt": "a door", "negative": "text", "family": "qi21", "transparent": true,
			"width": 512.0, "height": 768.0, "steps": 20.0, "seed": 3.0}, nil},
		{"video", func() string {
			return callVision(t, s.handleGenerateVideo, remote(map[string]any{"prompt": "a pan", "still": write("s.png", png), "model": "ltx25", "negative": "blur",
				"fast": true, "hero": true, "upscale": true, "frames": 33, "width": 640, "height": 360, "steps": 8, "seed": 7, "reserve_vram": 1.5}))
		}, "/fleet/media-job", "video-gen", map[string]any{"prompt": "a pan", "model": "ltx25", "negative": "blur", "fast": true, "hero": true, "upscale": true,
			"frames": 33.0, "width": 640.0, "height": 360.0, "steps": 8.0, "seed": 7.0, "reserve_vram": 1.5}, []string{"still"}},
		{"animate", func() string {
			return callVision(t, s.handleAnimateCharacter, remote(map[string]any{"prompt": "a fox", "ref": write("r.png", png), "driver": write("d.mp4", mp4),
				"motion_prompt": "a slow dance", "negative": "blur", "width": 482, "height": 854, "frames": 81, "steps": 10, "seed": 5,
				"pose_strength": "0.8", "ref_strength": "0.9", "reserve_vram": 2}))
		}, "/fleet/media-job", "animate", map[string]any{"prompt": "a fox", "motion_prompt": "a slow dance", "negative": "blur", "width": 482.0, "height": 854.0,
			"frames": 81.0, "steps": 10.0, "seed": 5.0, "pose_strength": "0.8", "ref_strength": "0.9", "reserve_vram": 2.0}, []string{"ref", "driver"}},
		{"audio", func() string {
			return callVision(t, s.handleGenerateAudio, remote(map[string]any{"text": "hola", "kind": "music", "voice": "generalist", "clone": write("c.wav", wav),
				"lang": "es", "seconds": 30, "seed": 4, "reserve_vram": 1}))
		}, "/fleet/media-job", "audio-gen", map[string]any{"text": "hola", "kind": "music", "voice": "generalist", "lang": "es", "seconds": 30.0, "seed": 4.0, "reserve_vram": 1.0}, []string{"clone"}},
		{"run-graph", func() string {
			return callVision(t, s.handleRunGraph, remote(map[string]any{"graph_json": `{"1":{"class_type":"X"}}`, "manifest_json": `{"models":[]}`, "reserve_vram": "0.5"}))
		}, "/fleet/dispatch", "run-graph", map[string]any{"graph": map[string]any{"1": map[string]any{"class_type": "X"}}, "manifest": map[string]any{"models": []any{}}, "reserve_vram": "0.5"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			txt := tc.handler()
			if !strings.Contains(txt, "stop: the wire test needs no render") {
				t.Fatalf("the job never reached the node: %s", txt)
			}
			path, body := n.last(t)
			if path != tc.path || body["task_type"] != tc.task {
				t.Fatalf("path %s task %v, want %s %s", path, body["task_type"], tc.path, tc.task)
			}
			payload, _ := body["payload"].(map[string]any)
			if !reflect.DeepEqual(payload, tc.payload) {
				t.Fatalf("payload\n got %#v\nwant %#v", payload, tc.payload)
			}
			inputs, _ := body["inputs"].(map[string]any)
			var got []string
			for k := range inputs {
				got = append(got, k)
			}
			if len(got) != len(tc.inputs) {
				t.Fatalf("input files %v, want %v", got, tc.inputs)
			}
			for _, k := range tc.inputs {
				if _, ok := inputs[k]; !ok {
					t.Errorf("input %q missing from %v", k, inputs)
				}
			}
		})
	}
}
