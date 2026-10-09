package mediaremote

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// get reads path from the node with the given Authorization header ("" = none).
func get(t *testing.T, n *node, path, authz string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, n.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// mediaGETs are the fetches of rendered files the node received.
func mediaGETs(n *node) []seen {
	var out []seen
	for _, r := range n.requests() {
		if r.method == http.MethodGet && strings.HasPrefix(r.path, "/fleet/media/") {
			out = append(out, r)
		}
	}
	return out
}

// The output of a job sent through the media-job door is rendered from the caller's private files, so the
// node serves it only to a holder of the fleet token (ADR 0077): the client sends the bearer when it fetches
// it, and the same file read without the token is refused. The output of a plain dispatch stays readable by
// bare name.
func TestAMediaJobOutputIsFetchedWithTheBearerAndRefusedWithout(t *testing.T) {
	n := startNode(t, nodeOpts{mediaInputs: true})
	cfg := clientCfg(t, n)
	still := writeFile(t, t.TempDir(), "photo.png", png)
	res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"still": still}), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	m := decode(t, res)
	local, _ := m["video_path"].(string)
	if local == "" || filepath.Dir(local) != cfg.MediaDir {
		t.Fatalf("video_path = %q, want a file in the caller's media_dir", local)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != "VIDEO" {
		t.Fatalf("fetched file = %q, %v", b, err)
	}
	name := filepath.Base(local)
	if !strings.HasPrefix(name, "mediajob-") {
		t.Fatalf("the node named a media-job output %q: the gate cannot recognise it", name)
	}
	gets := mediaGETs(n)
	if len(gets) != 1 || gets[0].path != "/fleet/media/"+name || gets[0].auth != "Bearer tok" {
		t.Fatalf("fetches = %+v, want one GET of %s carrying the fleet bearer", gets, name)
	}
	if code, _ := get(t, n, "/fleet/media/"+name, ""); code != http.StatusUnauthorized {
		t.Errorf("a tokenless read of the media-job output = %d, want 401", code)
	}
	if code, body := get(t, n, "/fleet/media/"+name, "Bearer tok"); code != http.StatusOK || body != "VIDEO" {
		t.Errorf("a read with the token = %d %q", code, body)
	}

	// A job with no input file goes through the tokenless dispatch; its output keeps its bare-name read.
	plain := Run(context.Background(), cfg, &recordingRunner{}, video(nil), "remote", nil)
	if !plain.OK {
		t.Fatalf("%+v", plain)
	}
	pname := filepath.Base(decode(t, plain)["video_path"].(string))
	if strings.HasPrefix(pname, "mediajob-") {
		t.Fatalf("a plain dispatch output was given the media-job stem: %q", pname)
	}
	if code, _ := get(t, n, "/fleet/media/"+pname, ""); code != http.StatusOK {
		t.Errorf("a tokenless read of a plain dispatch output = %d, want 200", code)
	}
}
