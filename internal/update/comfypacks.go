package update

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// ComfyPacks is the component that moves the installed ComfyUI node packs. It is not part of AllComponents and does not
// use the generation layout: the work is render/comfy-packs-update.mjs, which this verb starts and whose report and exit
// code it relays (docs/systems/update.md, "Node packs").
const ComfyPacks = "comfy-packs"

// ComfyPacksRun is one invocation of the node-pack updater.
type ComfyPacksRun struct {
	Node     string // the node executable ("" = node)
	Script   string // the render directory's comfy-packs-update.mjs
	ComfyDir string // the ComfyUI install; "" leaves COMFY_DIR as the environment has it
	DryRun   bool
	Resolve  bool // print the candidates and change nothing
	Rollback bool
	JSON     bool
	Stdout   io.Writer
	Stderr   io.Writer
}

// ComfyPacksScript names the updater beside the run-graph runner the node already uses: same render directory, same
// script resolution (relative to the binary), so a node that can run a graph can run this.
func ComfyPacksScript(runGraphScript string) string {
	return filepath.Join(filepath.Dir(runGraphScript), "comfy-packs-update.mjs")
}

// RunComfyPacks runs the updater to its end and returns its exit code: 0 done, no-op, inconclusive or refused; 1 a
// candidate was rejected and undone; 2 an error (also: the script could not be started). The script's own stdout is the
// report (one JSON line with --json), so it is passed through untouched.
func RunComfyPacks(ctx context.Context, r ComfyPacksRun) int {
	node := r.Node
	if node == "" {
		node = "node"
	}
	args := []string{r.Script}
	if r.DryRun {
		args = append(args, "--dry-run")
	}
	if r.Resolve {
		args = append(args, "--resolve")
	}
	if r.Rollback {
		args = append(args, "--rollback")
	}
	if r.JSON {
		args = append(args, "--json")
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = os.Environ()
	if r.ComfyDir != "" {
		cmd.Env = append(cmd.Env, "COMFY_DIR="+r.ComfyDir)
	}
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	err := cmd.Run()
	if err == nil {
		return ExitOK
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	if r.Stderr != nil {
		_, _ = io.WriteString(r.Stderr, "update: comfy-packs: could not run "+node+" "+r.Script+": "+err.Error()+"\n")
	}
	return ExitError
}
