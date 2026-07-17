// Package rungraph shells out to render/comfy-run-graph.mjs — the generic "run an
// arbitrary ComfyUI graph + satisfy its node manifest" runner — through the same
// gpugen exec lifecycle imagegen uses (process-tree-kill, timeout, defer free-VRAM).
// The harness is 100% generic here: it passes the graph + manifest opaquely and reads
// back a node-addressed output envelope; it never interprets graph semantics.
package rungraph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpugen"
)

type Params struct {
	GraphPath, ManifestPath, OutDir, ResultPath, ReserveVram string
}

type OutFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Kind   string `json:"kind"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Envelope struct {
	Outputs          map[string][]OutFile `json:"outputs"`
	ImagePath        string               `json:"image_path"`
	UnverifiedModels []string             `json:"unverified_models"`
	// Typed DEFER (fix #4): the mjs now exits 0 and writes {deferred,code,ref,detail} to the
	// result on any handled failure, so the code reaches the Go side as data (not a lost
	// stderr line). runRunGraph turns Deferred==true into a pipeline defer carrying Code+Detail.
	Deferred bool   `json:"deferred"`
	Code     string `json:"code"`
	Ref      string `json:"ref"`
	Detail   string `json:"detail"`
}

func buildArgs(p Params) []string {
	args := []string{"--graph", p.GraphPath}
	if p.ManifestPath != "" {
		args = append(args, "--manifest", p.ManifestPath)
	}
	args = append(args, "--out-dir", p.OutDir, "--result", p.ResultPath)
	if p.ReserveVram != "" {
		args = append(args, "--reserve-vram", p.ReserveVram)
	}
	return args
}

// Run executes the graph and returns the parsed output envelope. A non-zero exit /
// timeout / missing result file surfaces as an error the caller maps to a DEFER.
func Run(ctx context.Context, node, script, comfyDir string, p Params, timeout time.Duration, extraEnv ...string) (Envelope, error) {
	env := []string{"COMFY_DIR=" + comfyDir}
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	// Out = the result envelope JSON: the mjs always writes it, so gpugen's size>0 stat
	// gate is meaningful for image AND non-image graphs.
	if _, err := gpugen.Generate(ctx, gpugen.Spec{
		Exe: node, Script: script, Args: buildArgs(p),
		Env: append(env, extraEnv...), Out: p.ResultPath, Timeout: timeout,
	}); err != nil {
		return Envelope{}, err
	}
	raw, err := os.ReadFile(p.ResultPath)
	if err != nil {
		return Envelope{}, fmt.Errorf("rungraph: reading result: %w", err)
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("rungraph: parsing result: %w", err)
	}
	return e, nil
}
