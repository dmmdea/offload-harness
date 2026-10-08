package fleetnode

// Artifacts (ADR 0077): a finished media job's stored data names every output file it produced together
// with its size and sha256, so the machine that fetches the file from GET /fleet/media can prove the bytes
// it received are the bytes the node wrote. The field is additive: a consumer that does not read it sees
// exactly the result it always saw, and a result that names no file inside media_dir is stored untouched.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Artifact is one output file of a media job: its bare name in media_dir, its size and its sha256.
type Artifact struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// mediaArtifactTasks are the task types whose results name files: the five media tasks and the media-job
// door that carries them.
var mediaArtifactTasks = map[string]bool{
	"image-gen": true, "video-gen": true, "animate": true, "audio-gen": true, "run-graph": true, MediaJobTask: true,
}

// withArtifacts adds `artifacts` to a finished media job's result. Only the named output keys are read
// (image_path, video_path, audio_path and run-graph's outputs); a file outside media_dir, a symlink, a
// directory or a name that cannot be hashed is left out and never fails the job.
func withArtifacts(cfg config.Config, taskType string, data json.RawMessage) json.RawMessage {
	if !mediaArtifactTasks[taskType] || len(data) == 0 || cfg.MediaDir == "" {
		return data
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil || m == nil {
		return data
	}
	var named []string
	for _, k := range []string{"image_path", "video_path", "audio_path"} {
		var p string
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &p) == nil && p != "" {
			named = append(named, p)
		}
	}
	if raw, ok := m["outputs"]; ok {
		var outs map[string][]struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &outs) == nil {
			for _, files := range outs {
				for _, f := range files {
					if f.Path != "" {
						named = append(named, f.Path)
					}
				}
			}
		}
	}
	var list []Artifact
	seen := map[string]bool{}
	for _, p := range named {
		a, ok := hashArtifact(cfg.MediaDir, p)
		if !ok || seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		list = append(list, a)
	}
	if len(list) == 0 {
		return data
	}
	enc, err := json.Marshal(list)
	if err != nil {
		return data
	}
	m["artifacts"] = enc
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// hashArtifact hashes path when it is a regular file directly inside mediaDir, by the rule GET /fleet/media
// serves under (names resolved through symlinks must stay inside the resolved media dir). A bare name is
// read as a file of media_dir, the way the media route reads one.
func hashArtifact(mediaDir, path string) (Artifact, bool) {
	if !filepath.IsAbs(path) && !strings.ContainsAny(path, `/\`) {
		path = filepath.Join(mediaDir, path)
	}
	dirResolved, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		return Artifact{}, false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Dir(resolved) != dirResolved {
		return Artifact{}, false
	}
	// The named file itself must be a regular file, not a symlink to one: the name published is the one
	// the media route serves, and it must hold the bytes hashed here.
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return Artifact{}, false
	}
	f, err := os.Open(resolved)
	if err != nil {
		log.Printf("fleet: artifact %s: %v (omitted)", filepath.Base(path), err)
		return Artifact{}, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		log.Printf("fleet: artifact %s: hashing: %v (omitted)", filepath.Base(path), err)
		return Artifact{}, false
	}
	return Artifact{Name: filepath.Base(path), Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, true
}
