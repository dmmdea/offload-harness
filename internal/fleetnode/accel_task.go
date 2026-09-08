package fleetnode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// AccelPayload is the wire shape of a fleet "accel" job (Coral design Phase B):
// one accelerator tool call for a box that lacks the device. The image, when
// the tool takes one, travels INSIDE the job as base64 — the caller's
// image_path is on the caller's disk, which this node cannot read — and lands
// in a job-scoped directory that lives exactly as long as the job.
type AccelPayload struct {
	Accelerator string         `json:"accelerator"`
	Tool        string         `json:"tool"`
	Args        map[string]any `json:"args,omitempty"`
	ImageB64    string         `json:"image_b64,omitempty"`
	ImageName   string         `json:"image_name,omitempty"`
}

// accelImageName is the only file name an accel job may write: a bare name
// (the sidecar keys its default mask path off the extension, so it is kept).
var accelImageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// buildAccel decodes an accel payload into the pipeline request that runs the
// LOCAL lane. Refusals here are 400s at dispatch (the caller can get every one
// of them wrong); the tool's own defers come back inside the job result.
func buildAccel(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var p AccelPayload
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return core.Request{}, noop, fmt.Errorf("accel: payload: %w", err)
	}
	if p.Accelerator == "" || !slices.Contains(cfg.Accelerators, p.Accelerator) {
		return core.Request{}, noop, fmt.Errorf("accel: accelerator %q is not listed on this node (accelerators: %v)", p.Accelerator, cfg.Accelerators)
	}
	if strings.TrimSpace(p.Tool) == "" {
		return core.Request{}, noop, fmt.Errorf("accel: tool is required")
	}
	if p.Args == nil {
		p.Args = map[string]any{}
	}
	jobsRoot := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	if err := os.MkdirAll(jobsRoot, 0o755); err != nil {
		return core.Request{}, noop, fmt.Errorf("accel: creating pipeline-jobs dir: %w", err)
	}
	jobDir, err := os.MkdirTemp(jobsRoot, "accel-*")
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("accel: creating job dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(jobDir) }
	if p.ImageB64 != "" {
		name := p.ImageName
		if name == "" {
			name = "image"
		}
		if !accelImageName.MatchString(name) || strings.Contains(name, "..") {
			cleanup()
			return core.Request{}, noop, fmt.Errorf("accel: image_name %q must be a bare file name", p.ImageName)
		}
		raw, derr := base64.StdEncoding.DecodeString(p.ImageB64)
		if derr != nil {
			cleanup()
			return core.Request{}, noop, fmt.Errorf("accel: image_b64 is not base64: %w", derr)
		}
		if len(raw) > core.AccelImageCap {
			cleanup()
			return core.Request{}, noop, fmt.Errorf("accel: image is %d bytes, cap %d", len(raw), core.AccelImageCap)
		}
		path := filepath.Join(jobDir, name)
		if werr := os.WriteFile(path, raw, 0o644); werr != nil {
			cleanup()
			return core.Request{}, noop, fmt.Errorf("accel: writing image: %w", werr)
		}
		p.Args["image_path"] = path
		// A caller-side out_path is on the caller's disk; the tool's default
		// (beside the image, inside the job dir) is the one the result ships back.
		delete(p.Args, "out_path")
	}
	return core.Request{
		Task:  core.TaskAccel,
		Input: p.Tool,
		Params: map[string]any{
			"accelerator": p.Accelerator,
			"tool":        p.Tool,
			"args":        p.Args,
			"job_dir":     jobDir,
		},
	}, cleanup, nil
}
