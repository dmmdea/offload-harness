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
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// Budget bounds one forwarded call end to end: a cold sidecar spawn on the
// node (45 s) plus the tool's own timeout, with room for a queued dispatch.
const Budget = 150 * time.Second

// healthTimeout is how long one call waits for one node's /fleet/health. The accelerator lane keeps
// its own 2 s (a forwarded Coral or RKNPU call is a few milliseconds of work, so a node that cannot
// answer health in 2 s is not worth waiting for) where the other single-shot lanes use the shared
// rosterprobe.DefaultTimeout; the roster cache honours the difference (a 2 s timeout is not held
// against a caller willing to wait 5). A var so a test compresses it; production never mutates it.
var healthTimeout = 2 * time.Second

const (
	dispatchTimeout = 10 * time.Second
	pollEvery       = 500 * time.Millisecond
	maxBody         = 32 << 20
)

// HTTPClient is the transport every request uses; tests swap it. It rides
// netguard.SafeTransport like every other fleet client (health, vision, text, compose):
// the lane may only ever reach loopback or the operator's tailnet (never-cloud, ADR 0001),
// enforced at dial time, so a delegate_remotes entry that drifts to a public address dies
// at the dial gate instead of carrying an image there.
var HTTPClient = &http.Client{Transport: netguard.SafeTransport(nil), Timeout: Budget, CheckRedirect: rosterprobe.NoRedirect}

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
	land(cfg, out, args)
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
			return nil, fmt.Errorf("image_path %q: %w (the file must exist on THIS box; its bytes travel to the node)", ip, err)
		}
		if len(raw) > core.AccelImageCap {
			return nil, fmt.Errorf("image_path %q is %d bytes, cap %d for a forwarded call", ip, len(raw), core.AccelImageCap)
		}
		p["image_b64"] = base64.StdEncoding.EncodeToString(raw)
		p["image_name"] = shippedName(ip)
		delete(a, "image_path")
		delete(a, "out_path") // the node writes beside the shipped image; land() puts the bytes at the caller's out_path
	}
	p["args"] = a
	return json.Marshal(p)
}

// shippedName is the file name the caller's image travels under: one plain path element, never a dot file.
func shippedName(ip string) string {
	name := safeName.ReplaceAllString(filepath.Base(ip), "_")
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		name = "image" + filepath.Ext(ip)
	}
	return name
}

// pngMagic is the PNG signature. Every file a forwarded tool ships back is a PNG (a semantic or instance
// mask, a depth map), so nothing else is written on this box.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// land writes every file the node shipped back onto THIS box, so a forwarded call answers like the local
// lane. The node turns each <stem>_path inside its job dir into <stem>_b64 + <stem>_name
// (pipeline.runAccelTask); here those become a file and <stem>_path names it, so the caller can open it and
// no image bytes ride into the caller's context (a mask or depth map is 40-110 KB of base64). With one
// shipped file the caller's out_path names it (never the caller's own image, and its directory must exist,
// as for the local sidecars); otherwise each lands beside the caller's image as <image>.<kind>.png, the
// local lane's default. That name is built HERE: the node only contributes <kind>, a plain word, so a
// node's answer can never name the caller's image, a sibling file or another directory. Only PNG bytes
// land, and an existing file is replaced only when it is a regular PNG file: never a symlink, never another
// kind of file (the sidecars' own out_path rule). A file that cannot land leaves <stem>_error.
func land(cfg config.Config, out, args map[string]any) {
	var stems []string
	for k := range out {
		if stem, ok := strings.CutSuffix(k, "_b64"); ok && stem != "image" {
			if _, shipped := out[stem+"_name"].(string); shipped {
				stems = append(stems, stem)
			}
		}
	}
	if len(stems) == 0 {
		return
	}
	slices.Sort(stems)
	outPath, _ := args["out_path"].(string)
	outPath = strings.TrimSpace(outPath)
	ip, _ := args["image_path"].(string)
	ip = strings.TrimSpace(ip)
	dir, img := filepath.Join(cfg.BaseDir(), "accel-out"), "image"
	if ip != "" {
		dir = filepath.Dir(ip)
		n := shippedName(ip)
		img = strings.TrimSuffix(n, filepath.Ext(n))
	}
	for _, stem := range stems {
		b64, _ := out[stem+"_b64"].(string)
		name, _ := out[stem+"_name"].(string)
		delete(out, stem+"_b64")
		delete(out, stem+"_name")
		delete(out, stem+"_path") // the node's job-dir path: removed with the job, never valid on this box
		if !landKind.MatchString(stem) {
			continue // not a key a tool writes; nothing lands under a name the node made up
		}
		var dst string
		var err error
		if outPath != "" && len(stems) == 1 {
			dst = outPath
			if ip != "" && sameFile(dst, ip) {
				err = fmt.Errorf("out_path %s is the input image; not replaced", dst)
			} else if fi, serr := os.Stat(filepath.Dir(dst)); serr != nil || !fi.IsDir() {
				err = fmt.Errorf("out_path %s: its directory does not exist", dst)
			}
		} else {
			dst = filepath.Join(dir, img+"."+landKindOf(name, img, stem)+".png")
			if filepath.Dir(dst) != filepath.Clean(dir) {
				err = fmt.Errorf("%s is not inside %s; not written", dst, dir)
			} else {
				err = os.MkdirAll(dir, 0o755)
			}
		}
		if err == nil {
			err = landFile(dst, b64)
		}
		if err != nil {
			out[stem+"_error"] = err.Error()
			continue
		}
		out[stem+"_path"] = dst
	}
}

// landKind is the shape of a result stem (mask, depth) and of the kind word in a landed file's name.
var landKind = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// landKindOf reads the kind out of the node's file name when it is the sidecars' default,
// <shipped image stem>.<kind>.png (street.segmask.png -> segmask), and is the stem otherwise.
func landKindOf(name, img, stem string) string {
	if rest, ok := strings.CutPrefix(filepath.Base(name), img+"."); ok {
		if kind, ok := strings.CutSuffix(rest, ".png"); ok && landKind.MatchString(kind) {
			return kind
		}
	}
	return stem
}

// sameFile says whether a and b are one file: the same cleaned path, or the same file on disk.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, ea := os.Stat(a)
	fb, eb := os.Stat(b)
	return ea == nil && eb == nil && os.SameFile(fa, fb)
}

func landFile(dst, b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("the node's file is not base64: %w", err)
	}
	if len(raw) > core.AccelImageCap {
		return fmt.Errorf("the node's file is %d bytes, cap %d", len(raw), core.AccelImageCap)
	}
	if !bytes.HasPrefix(raw, pngMagic) {
		return errors.New("the node's file is not a PNG; nothing written")
	}
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file; not replaced", dst)
		}
		if !isPNG(dst) {
			return fmt.Errorf("%s exists and is not a PNG; not replaced", dst)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".accel-*.png")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), dst)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return nil
}

func isPNG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(pngMagic))
	n, _ := io.ReadFull(f, head)
	return n == len(pngMagic) && bytes.Equal(head, pngMagic)
}

// pickNode reads delegate_remotes' health (concurrently, through the shared roster cache) and
// returns the first node IN CONFIGURED ORDER whose health lists the accelerator. Every miss is
// named in the error so "no node" is never a mystery. Probing the whole roster at once costs the
// slowest member, not the sum, and the first listing node still wins.
func pickNode(ctx context.Context, cfg config.Config, id string) (base, node string, err error) {
	var misses []string
	for _, r := range rosterprobe.Probe(ctx, cfg.DelegateRemotes, cfg.FleetAuthToken, healthTimeout) {
		if r.Err != nil {
			misses = append(misses, r.Miss())
			continue
		}
		if slices.Contains(r.View.Accelerators, id) {
			n := r.View.NodeID
			if n == "" {
				n = r.Base
			}
			return r.Base, n, nil
		}
		misses = append(misses, fmt.Sprintf("%s (%s): accelerators %v", r.Shown(), r.View.NodeID, r.View.Accelerators))
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
	// Who asked, and whether the serving node must card the job because this box will not (D7/D11).
	pairworkloads.WireHeadersFor(cfg, req.Header)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", rosterprobe.Scrubbed(base, fmt.Errorf("dispatch %s: %w", base, err))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", rosterprobe.Scrubbed(base, fmt.Errorf("dispatch %s: status %d: %s", base, resp.StatusCode, truncate(body)))
	}
	// The node accepted the job: whatever the negative cache holds against it is out of date.
	rosterprobe.Default.Forget(base)
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
