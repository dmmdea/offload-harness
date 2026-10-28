package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// FileConfig is the `update` object of the node's config.json. Every field is optional.
type FileConfig struct {
	// Channel is "nightly" (default) or "release" (llama.cpp only).
	Channel string `json:"channel"`
	// Paths overrides a component's live path.
	Paths map[string]string `json:"paths"`
	// Restart overrides how the llama-swap consumer is restarted.
	Restart map[string]RestartSpec `json:"restart"`
	// LlamaSwapYAML is the rendered llama-swap config the llama flag check reads (default <home>/llama-swap.yaml).
	LlamaSwapYAML string `json:"llama_swap_yaml"`
	// Gate configures the side-port smoke gate and the post-flip watch (docs/systems/update.md, "Gate").
	Gate GateConfig `json:"gate"`
}

// RestartSpec names how a consumer restarts: a scheduled task, a shell command, or None (no restart: the swap
// is verified by hash alone).
type RestartSpec struct {
	Task    string `json:"task"`
	Command string `json:"command"`
	None    bool   `json:"none"`
}

// ReadFileConfig reads the `update` object of a config.json; a missing file or key is the zero value.
func ReadFileConfig(path string) FileConfig {
	var fc FileConfig
	if path == "" {
		return fc
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fc
	}
	var top struct {
		Update FileConfig `json:"update"`
	}
	if json.Unmarshal(b, &top) == nil {
		fc = top.Update
	}
	return fc
}

// Consumer is the local llama-swap, reached through the harness's own client: its unload with drain, never a kill.
type Consumer struct {
	Endpoint string
}

func (c Consumer) client(timeout time.Duration) (*llamaswap.Client, error) {
	if strings.TrimSpace(c.Endpoint) == "" {
		return nil, errors.New("no llama-swap endpoint configured")
	}
	return swapclient.New(c.Endpoint, timeout)
}

// RunningOn lists the models llama-swap runs from the live path (matched on the argv llama-swap actually spawned).
func (c Consumer) RunningOn(ctx context.Context, live string) ([]string, error) {
	cl, err := c.client(10 * time.Second)
	if err != nil {
		return nil, err
	}
	run, err := cl.Running(ctx)
	if err != nil {
		return nil, err
	}
	want := normPath(live) + "/"
	var ids []string
	for _, r := range run {
		if strings.Contains(normPath(r.Cmd), want) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// UnloadOn unloads, with drain, every model running from the live path so the next load starts the new build.
// A keep-set member is left (and named); a model that stays busy past the drain wait is an error and nothing
// further is touched. override unloads keep-set members too (only when the whole daemon is about to restart).
func (c Consumer) UnloadOn(ctx context.Context, live string, override bool, drain time.Duration) (unloaded, left []string, err error) {
	ids, err := c.RunningOn(ctx, live)
	if err != nil {
		return nil, nil, err
	}
	cl, err := c.client(30 * time.Second)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		_, uerr := cl.Unload(ctx, id, &llamaswap.UnloadOpts{Drain: true, DrainTimeout: drain, KeepsetOverride: override})
		switch {
		case uerr == nil:
			unloaded = append(unloaded, id)
		case errors.Is(uerr, llamaswap.ErrKeepsetRefusal):
			left = append(left, id)
		default:
			return unloaded, left, fmt.Errorf("unloading %s: %w", id, uerr)
		}
	}
	return unloaded, left, nil
}

// UnloadAll unloads, with drain, every running model, keep-set members included: the whole daemon is about to restart.
func (c Consumer) UnloadAll(ctx context.Context, drain time.Duration) ([]string, error) {
	cl, err := c.client(30 * time.Second)
	if err != nil {
		return nil, err
	}
	run, err := cl.Running(ctx)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, r := range run {
		if _, uerr := cl.Unload(ctx, r.ID, &llamaswap.UnloadOpts{Drain: true, DrainTimeout: drain, KeepsetOverride: true}); uerr != nil {
			return done, fmt.Errorf("unloading %s: %w", r.ID, uerr)
		}
		done = append(done, r.ID)
	}
	return done, nil
}

// Ready reports whether llama-swap answers /running, waiting up to timeout.
func (c Consumer) Ready(ctx context.Context, timeout time.Duration) error {
	cl, err := c.client(5 * time.Second)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		_, rerr := cl.Running(ctx)
		if rerr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama-swap did not answer /running within %s: %w", timeout, rerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// WatchFirstLoad is the post-flip watch: llama-swap has just been told to unload its seats, so the next request
// loads the seat on the new build. It sends one short completion for the resident seat through llama-swap (which
// loads it), checks the seat now runs from the live path and answers /health through llama-swap. An error here
// makes the caller roll back ("post-flip load failed"). Nothing to watch (no endpoint, no chat seat in the yaml,
// llama-swap not answering) is a note, not a failure.
func (e *Env) WatchFirstLoad(ctx context.Context, live string) (note string, err error) {
	if e.Endpoint == "" {
		return "", nil
	}
	seat, perr := PickSeat(e.yamlPath(), live)
	if perr != nil {
		return "first load not watched: " + perr.Error(), nil
	}
	c := Consumer{Endpoint: e.Endpoint}
	if rerr := c.Ready(ctx, 20*time.Second); rerr != nil {
		return "first load not watched: llama-swap is not answering", nil
	}
	timeout := e.Config.Gate.PostFlipTimeoutOr()
	started := time.Now()
	// The watch runs after the gate released its GPU lease, so a render may hold the cards by now. The completion that
	// loads the seat goes through the fence (modelaffinity.AwaitModelRoute): it waits, up to the post-flip timeout,
	// for the fence to lift or the seat to be resident, and never loads a model onto a fenced card. Still fenced at
	// the deadline is soft: rolled back, not rejected, retried next run.
	chatURL, ferr := modelaffinity.AwaitModelRoute(ctx, e.Endpoint, seat.ID, "/v1/chat/completions", started.Add(timeout))
	if ferr != nil {
		return "", softError{fmt.Errorf("seat %s was not loaded through llama-swap: %w", seat.ID, ferr)}
	}
	r := chat(ctx, chatURL, seat.ID, readyPrompt, 16, false, timeout)
	if r.Err != "" {
		err := fmt.Errorf("seat %s did not answer a completion through llama-swap within %s: %s", seat.ID, timeout, r.Err)
		if r.Soft {
			return "", softError{err}
		}
		return "", err
	}
	if r.Text == "" {
		return "", fmt.Errorf("seat %s answered an empty completion through llama-swap", seat.ID)
	}
	if ids, rerr := c.RunningOn(ctx, live); rerr == nil {
		found := false
		for _, id := range ids {
			found = found || id == seat.ID
		}
		if !found {
			return "", fmt.Errorf("seat %s answered but llama-swap does not list it running from %s", seat.ID, live)
		}
	}
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The seat is resident now, so the fence lets this through at once.
	healthURL, uerr := modelaffinity.AwaitUpstream(hctx, e.Endpoint, seat.ID, "/health", time.Now().Add(15*time.Second))
	if uerr != nil {
		return fmt.Sprintf("first load watched: seat %s answered through llama-swap in %.0f s (its /health was not read: %v)", seat.ID, time.Since(started).Seconds(), uerr), nil
	}
	if req, herr := http.NewRequestWithContext(hctx, http.MethodGet, healthURL, nil); herr == nil {
		if resp, derr := localClient().Do(req); derr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 500 {
				return "", fmt.Errorf("seat %s /health through llama-swap answered HTTP %d", seat.ID, resp.StatusCode)
			}
		}
	}
	return fmt.Sprintf("first load watched: seat %s loaded and answered through llama-swap in %.0f s", seat.ID, time.Since(started).Seconds()), nil
}
