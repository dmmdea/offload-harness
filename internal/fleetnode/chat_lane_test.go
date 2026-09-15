// Chat-lane tests (C-41b). Three invariants carry the lane, and the fourth
// test is the one the whole register item turns on:
//
//  1. Advertisement == admission: `chat_lane` appears in health exactly when
//     POST /fleet/chat will admit — a bound endpoint plus the agent lane's
//     reachability rule — and the lane rides the agent lane's bearer gate.
//  2. The node serves only what its OWN roster serves, alias-aware, and says
//     404 for anything else rather than forwarding a swap that would fail.
//  3. The forward is BYTE FOR BYTE: grammar, template kwargs, logprobs and
//     every other field the caller set reach llama-swap unchanged, and the
//     upstream status comes back unflattened (the caller's seat-wait loop
//     keys on 429/503).
//  4. The route string this node serves is the one the caller spells
//     (llamaclient.FleetChatPath) — the two constants live on opposite sides
//     of an import cycle, so only a test can hold them together.
package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// chatUpstreamServer is a fake llama-swap: it records the last chat body it
// was handed and answers with whatever status/body the test set.
type chatUpstreamServer struct {
	srv    *httptest.Server
	hits   atomic.Int64
	body   atomic.Value // string
	status atomic.Int64
	answer atomic.Value // string
}

func newChatUpstream(t *testing.T) *chatUpstreamServer {
	t.Helper()
	u := &chatUpstreamServer{}
	u.status.Store(int64(http.StatusOK))
	u.answer.Store(`{"choices":[{"message":{"content":"ok"}}]}`)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		u.hits.Add(1)
		b := make([]byte, 0, 1024)
		buf := make([]byte, 512)
		for {
			n, err := r.Body.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil {
				break
			}
		}
		u.body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(u.status.Load()))
		if _, err := w.Write([]byte(u.answer.Load().(string))); err != nil {
			t.Errorf("write upstream answer: %v", err)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *chatUpstreamServer) lastBody() string {
	v, _ := u.body.Load().(string)
	return v
}

// chatCfg points a node at the fake llama-swap with the given fleet token.
func chatCfg(endpoint, token string) config.Config {
	c := imageCfg()
	c.Endpoint = endpoint
	c.FleetAuthToken = token
	return c
}

// servingNode builds a node whose roster serves exactly the given names
// (alias-aware is the roster's job; the seam answers the same question).
func servingNode(t *testing.T, cfg config.Config, loopback bool, names ...string) *Server {
	t.Helper()
	s, _ := newTestServer(t, cfg, &visionRunner{res: core.Result{OK: true}}, authOpts(loopback))
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) {
		for _, n := range names {
			if strings.EqualFold(n, seat) {
				return true, nil
			}
		}
		return false, nil
	}
	s.rosterServedModels = func(ctx context.Context, endpoint string) ([]string, error) {
		return append([]string(nil), names...), nil
	}
	return s
}

const chatBody = `{"model":"gemma-4-e4b","messages":[{"role":"user","content":"hi"}],"grammar":"root ::= \"x\"","temperature":0.1}`

// TestChatLanePathMatchesTheCallersConstant is invariant 4: the delegator
// spells this route in internal/llamaclient (which cannot import this package
// without a cycle), so nothing but a test stops the two from drifting apart —
// and a drift here is a 404 on every cascade call, in production only.
func TestChatLanePathMatchesTheCallersConstant(t *testing.T) {
	if ChatLanePath != llamaclient.FleetChatPath {
		t.Fatalf("node route %q != caller route %q", ChatLanePath, llamaclient.FleetChatPath)
	}
}

// TestChatLaneAdvertisementMatchesAdmission is invariant 1: whenever health
// publishes chat_lane the route admits, and whenever it does not the route
// refuses with the agent lane's own verdicts. health must also publish
// served_models on a chat-only node — it is the delegator's residency source.
func TestChatLaneAdvertisementMatchesAdmission(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		loopback   bool
		header     string
		advertised bool
		wantStatus int
	}{
		{"loopback, no token", "", true, "", true, http.StatusOK},
		{"non-loopback, no token", "", false, "", false, http.StatusForbidden},
		{"non-loopback, token, no header", "s3cret", false, "", true, http.StatusUnauthorized},
		{"non-loopback, token, wrong header", "s3cret", false, "Bearer nope", true, http.StatusUnauthorized},
		{"non-loopback, token, right header", "s3cret", false, "Bearer s3cret", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newChatUpstream(t)
			s := servingNode(t, chatCfg(up.srv.URL, tc.token), tc.loopback, "gemma-4-e4b")

			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			if _, has := h["chat_lane"]; has != tc.advertised {
				t.Fatalf("chat_lane present = %v, want %v (health %v)", has, tc.advertised, h)
			}

			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Authorization"] = tc.header
			}
			rec := do(t, s, http.MethodPost, ChatLanePath, chatBody, hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden && !strings.Contains(rec.Body.String(), chatLaneTokenRequired) {
				t.Fatalf("403 body = %s, want the token-required signal", rec.Body.String())
			}
		})
	}
}

// TestChatLaneHealthPublishesServedModelsWithoutTheAgentLane: served_models is
// the ONLY thing a delegator's cascade lane can read residency from, and it
// used to be published only under the agent lane. A node that serves the
// cascade GGUFs but runs no agent seat must still publish it, or every lane
// call it could answer stays on the busy box.
func TestChatLaneHealthPublishesServedModelsWithoutTheAgentLane(t *testing.T) {
	up := newChatUpstream(t)
	cfg := chatCfg(up.srv.URL, "")
	cfg.FleetAgentEnabled = false
	s := servingNode(t, cfg, true, "gemma-4-e4b", "offload-e4b")
	s.RefreshAgentResidency() // land one probe deterministically, as the delegation E2E does

	h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if h["chat_lane"] != true {
		t.Fatalf("chat_lane = %v, want true", h["chat_lane"])
	}
	got, _ := h["served_models"].([]any)
	if len(got) != 2 {
		t.Fatalf("served_models = %v, want both the id and the alias", h["served_models"])
	}
}

// TestChatLaneServesOnlyItsOwnRoster is invariant 2: a model this node serves
// is forwarded; one it does not is a 404 that never touches llama-swap. The
// alias case is the one that matters in production — every harness-bound seat
// name on the fleet is an alias.
func TestChatLaneServesOnlyItsOwnRoster(t *testing.T) {
	up := newChatUpstream(t)
	s := servingNode(t, chatCfg(up.srv.URL, ""), true, "gemma-4-e4b", "offload-e4b")

	t.Run("a served model is forwarded", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, ChatLanePath, chatBody, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
			t.Fatalf("body = %s, want the upstream answer copied back", rec.Body.String())
		}
	})

	t.Run("a served ALIAS is forwarded", func(t *testing.T) {
		before := up.hits.Load()
		rec := do(t, s, http.MethodPost, ChatLanePath, `{"model":"offload-e4b","messages":[]}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if up.hits.Load() != before+1 {
			t.Fatal("an alias-bound seat must reach llama-swap")
		}
	})

	t.Run("a model this node does not serve is a 404", func(t *testing.T) {
		before := up.hits.Load()
		rec := do(t, s, http.MethodPost, ChatLanePath, `{"model":"qwen3.8-27b-vllm","messages":[]}`, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
		}
		if up.hits.Load() != before {
			t.Fatal("an unserved model must never be forwarded to llama-swap")
		}
	})

	t.Run("a body with no model is a 400", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, ChatLanePath, `{"messages":[]}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// TestChatLaneForwardsTheBodyVerbatim is invariant 3. A lane that rebuilt the
// request from a decoded struct would silently drop `grammar` — and a cascade
// tier without its grammar answers prose where the pipeline expects JSON, which
// is a defer, not an error, and would have looked like a model problem.
func TestChatLaneForwardsTheBodyVerbatim(t *testing.T) {
	up := newChatUpstream(t)
	s := servingNode(t, chatCfg(up.srv.URL, ""), true, "gemma-4-e4b")
	if rec := do(t, s, http.MethodPost, ChatLanePath, chatBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := up.lastBody(); got != chatBody {
		t.Fatalf("upstream body =\n%s\nwant byte-identical:\n%s", got, chatBody)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(up.lastBody()), &sent); err != nil {
		t.Fatalf("forwarded body is not JSON: %v", err)
	}
	if sent["grammar"] == nil {
		t.Fatal("the grammar must survive the forward")
	}
}

// TestChatLaneCopiesTheUpstreamStatus: llama-swap answers 503 while a peer
// holds the seat, and the CALLER's seat-wait loop is what retries on it. A
// lane that flattened every non-200 into a 502 would turn a retryable wait
// into a hard failure.
func TestChatLaneCopiesTheUpstreamStatus(t *testing.T) {
	up := newChatUpstream(t)
	up.status.Store(int64(http.StatusServiceUnavailable))
	up.answer.Store(`{"error":"model not ready"}`)
	s := servingNode(t, chatCfg(up.srv.URL, ""), true, "gemma-4-e4b")

	rec := do(t, s, http.MethodPost, ChatLanePath, chatBody, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream's 503 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model not ready") {
		t.Fatalf("body = %s, want the upstream's own body", rec.Body.String())
	}
}
