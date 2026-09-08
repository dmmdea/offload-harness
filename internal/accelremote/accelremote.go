// Package accelremote forwards ONE accelerator tool call to the fleet node that
// carries the device (Coral design Phase B, 0.115.0). A box that lists an id in
// config.FleetAccelerators but not in config.Accelerators registers that
// device's tools locally — MCP surface and agent loop alike — and every call
// lands here: read the caller's image from the caller's disk, ship its bytes
// inside a fleet "accel" job to the first delegate_remotes node whose health
// advertises the id, wait for the job, hand the tool's dict back with a
// placement block. The node runs only its LOCAL lane for the job, so a
// forwarded call can never forward again.
package accelremote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// Budget bounds one forwarded call end to end: a cold sidecar spawn on the
// node (45 s) plus the tool's own timeout, with room for a queued dispatch.
const Budget = 150 * time.Second

const (
	healthTimeout   = 2 * time.Second
	dispatchTimeout = 10 * time.Second
	pollEvery       = 500 * time.Millisecond
	maxBody         = 32 << 20
)

// HTTPClient is the transport every request uses; tests swap it.
var HTTPClient = &http.Client{Timeout: Budget}

var safeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Placement says where a forwarded call ran, so a slow call or a defer is
// attributable from the result alone.
type Placement struct {
	Node        string `json:"node"`
	Base        string `json:"base"`
	Accelerator string `json:"accelerator"`
	JobID       string `json:"job_id"`
	WallMs      int64  `json:"wall_ms"`
	Remote      bool   `json:"remote"`
}

// Call runs tool on accelerator id over the fleet. A transport, placement or
// dispatch failure is an error (the caller turns it into a device-prefixed
// defer); the node's own defers and the tool's structured refusals come back
// as the result dict, exactly as a local lane would return them.
func Call(ctx context.Context, cfg config.Config, id, tool string, args map[string]any) (map[string]any, error) {
	start := time.Now()
	if len(cfg.DelegateRemotes) == 0 {
		return nil, errors.New("no delegate_remotes configured — nothing to forward to")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budget)
		defer cancel()
	}
	payload, err := buildPayload(id, tool, args)
	if err != nil {
		return nil, err
	}
	base, node, err := pickNode(ctx, cfg, id)
	if err != nil {
		return nil, err
	}
	jobID, err := dispatch(ctx, cfg, base, payload)
	if err != nil {
		return nil, err
	}
	out, err := wait(ctx, cfg, base, jobID)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	out["placement"] = Placement{Node: node, Base: base, Accelerator: id, JobID: jobID,
		WallMs: time.Since(start).Milliseconds(), Remote: true}
	return out, nil
}

// buildPayload copies args and moves a caller-side image_path into the job as
// bytes. The file is read HERE, on the box that has it; the node never sees a
// path that is not its own.
func buildPayload(id, tool string, args map[string]any) ([]byte, error) {
	p := map[string]any{"accelerator": id, "tool": tool}
	a := map[string]any{}
	for k, v := range args {
		a[k] = v
	}
	if ip, ok := a["image_path"].(string); ok && strings.TrimSpace(ip) != "" {
		raw, err := os.ReadFile(ip)
		if err != nil {
			return nil, fmt.Errorf("image_path %q: %v (the file must exist on THIS box; its bytes travel to the node)", ip, err)
		}
		if len(raw) > core.AccelImageCap {
			return nil, fmt.Errorf("image_path %q is %d bytes, cap %d for a forwarded call", ip, len(raw), core.AccelImageCap)
		}
		name := safeName.ReplaceAllString(filepath.Base(ip), "_")
		if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
			name = "image" + filepath.Ext(ip)
		}
		p["image_b64"] = base64.StdEncoding.EncodeToString(raw)
		p["image_name"] = name
		delete(a, "image_path")
		delete(a, "out_path") // the node's default (beside the shipped image) is what comes back as mask_b64
	}
	p["args"] = a
	return json.Marshal(p)
}

type healthWire struct {
	NodeID       string   `json:"node_id"`
	Accelerators []string `json:"accelerators"`
}

// pickNode probes delegate_remotes IN ORDER and returns the first whose health
// lists the accelerator. Every miss is named in the error so "no node" is
// never a mystery.
func pickNode(ctx context.Context, cfg config.Config, id string) (base, node string, err error) {
	var misses []string
	for _, b := range cfg.DelegateRemotes {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		h, herr := getJSON[healthWire](hctx, cfg, b+"/fleet/health")
		cancel()
		if herr != nil {
			misses = append(misses, b+": "+herr.Error())
			continue
		}
		if slices.Contains(h.Accelerators, id) {
			n := h.NodeID
			if n == "" {
				n = b
			}
			return b, n, nil
		}
		misses = append(misses, fmt.Sprintf("%s (%s): accelerators %v", b, h.NodeID, h.Accelerators))
	}
	return "", "", fmt.Errorf("no fleet node advertises %s — probed %s", id, strings.Join(misses, "; "))
}

func dispatch(ctx context.Context, cfg config.Config, base string, payload []byte) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("job id: %w", err)
	}
	jobID := "accel-" + hex.EncodeToString(rnd[:])
	env, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": string(core.TaskAccel), "payload": json.RawMessage(payload)})
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodPost, base+"/fleet/dispatch", bytes.NewReader(env))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dispatch %s: %w", base, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("dispatch %s: status %d: %s", base, resp.StatusCode, truncate(body))
	}
	return jobID, nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// wait polls the job until it is done or errored. The node stores a
// successful run's core.Result.Data — the tool's dict — as the job data, and
// turns a pipeline defer into job state "error" with the defer reason; both
// are answers from the node, so an error state comes back as a deferred dict,
// never as a transport error.
func wait(ctx context.Context, cfg config.Config, base, jobID string) (map[string]any, error) {
	for {
		pctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
		j, err := getJSON[jobWire](pctx, cfg, base+"/fleet/jobs/"+jobID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			}
			return nil, fmt.Errorf("job %s: %w", jobID, err)
		}
		switch j.State {
		case "done":
			var out map[string]any
			if len(j.Data) > 0 && json.Unmarshal(j.Data, &out) == nil && out != nil {
				return out, nil
			}
			return map[string]any{"deferred": true, "reason": "job done with an unreadable result: " + truncate(j.Data)}, nil
		case "error":
			return map[string]any{"deferred": true, "reason": j.Error}, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("job %s still %s: %w", jobID, j.State, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

func getJSON[T any](ctx context.Context, cfg config.Config, url string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, truncate(body))
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("GET %s: not JSON: %w", url, err)
	}
	return v, nil
}

func auth(cfg config.Config, req *http.Request) {
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256] + "…"
	}
	return s
}
