package rungraph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The node-side half of the run-template tool is render/run-template.mjs: list, get, readiness and resolve over the workflow templates a
// ComfyUI tree carries. It is read-only (it starts no ComfyUI, takes no lease, converts nothing, downloads nothing and writes no file),
// so it is run like any child process of the harness, hidden, with a deadline, and its answer is the one JSON object it prints.

// templateScriptName sits beside the run-graph runner: both are part of the same render/ release, so a machine that has the one has the
// other, and a path that was set for the run-graph route needs no second setting.
const templateScriptName = "run-template.mjs"

// defaultTemplateTimeout bounds a query that is given none. A catalog build reads a few hundred small files and a readiness scan walks
// the model directories of the tree (a model drive of a few hundred files takes seconds), so a minute or two is the working range.
const defaultTemplateTimeout = 2 * time.Minute

// TemplateScript returns the path of render/run-template.mjs given the configured run-graph runner (run_graph_script): the file of that
// name in the same directory. A box with no run-graph route has no template tool to speak of.
func TemplateScript(runGraphScript string) (string, error) {
	if strings.TrimSpace(runGraphScript) == "" {
		return "", errors.New("no run-graph route is configured (run_graph_script), so there is no run-template script beside it")
	}
	return filepath.Join(filepath.Dir(runGraphScript), templateScriptName), nil
}

// QueryTemplate runs `node <script> <args...> --comfy-dir <comfyDir>` and returns its stdout. A non-zero exit is an error that carries the
// script's own message (its stderr), because that message is what names the mistake (an unknown verb, a snapshot that is not one, a tree
// with no templates package). The child is hidden (winexec) and stops with ctx or at the deadline.
func QueryTemplate(ctx context.Context, node, script, comfyDir string, args []string, timeout time.Duration) ([]byte, error) {
	if strings.TrimSpace(node) == "" {
		return nil, errors.New("no node runtime is configured (node_path)")
	}
	if strings.TrimSpace(script) == "" {
		return nil, errors.New("no run-template script")
	}
	if strings.TrimSpace(comfyDir) == "" {
		return nil, errors.New("comfy_dir is not configured on this machine")
	}
	if timeout <= 0 {
		timeout = defaultTemplateTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{script}, args...)
	argv = append(argv, "--comfy-dir", comfyDir)
	cmd := winexec.CommandContext(cctx, node, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if cerr := cctx.Err(); cerr != nil {
			return nil, fmt.Errorf("%s: %w", args[0], cerr)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s: %w", args[0], err)
		}
		return nil, fmt.Errorf("%s: %w: %s", args[0], err, firstLines(msg, 3))
	}
	return stdout.Bytes(), nil
}

// firstLines keeps the first n lines of a message: a usage text after an error line is not what the caller needs to read.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}
