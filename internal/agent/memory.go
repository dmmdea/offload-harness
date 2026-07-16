package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Memory is the loop's optional memory layer: recall relevant context before
// planning, persist a run outcome after. nil => the loop runs without memory.
type Memory interface {
	Recall(ctx context.Context, query string, limit int) ([]Recalled, error)
	Persist(ctx context.Context, text string, meta map[string]string) (string, error)
}

// Recalled is one retrieved memory.
type Recalled struct {
	ID     string
	Text   string
	Score  float64
	Source string // which namespace (user_id) it came from
}

// MemoryClient talks to the mem0 REST server (the agentic-memory system). It is
// WRITE-ISOLATED by construction: it recalls across readUsers (e.g. the agent's
// own namespace for run-to-run continuity + the canonical "dmmdea" namespace for
// the operator's knowledge) but only ever WRITES under writeUser, and only at the
// "evidence" tier — the mem0 server independently rejects any attempt to write
// the canonical tier (403), so the agent cannot pollute the canonical anchor.
type MemoryClient struct {
	base      string
	apiKey    string
	readUsers []string
	writeUser string
	agentID   string
	http      *http.Client
}

// NewMemoryClient builds a client. readUsers are the namespaces to recall from;
// writeUser is the (isolated) namespace to persist to. A trailing slash on base
// is trimmed.
func NewMemoryClient(base, apiKey string, readUsers []string, writeUser, agentID string, timeout time.Duration) *MemoryClient {
	return &MemoryClient{
		base:      strings.TrimRight(base, "/"),
		apiKey:    apiKey,
		readUsers: readUsers,
		writeUser: writeUser,
		agentID:   agentID,
		http:      &http.Client{Timeout: timeout},
	}
}

// Mem0KeyFromEnvOrFile resolves the mem0 API key: $MEM0_API_KEY first, then the
// on-disk key file (~/.mem0/api-key, mode 600 — the mem0 server's own key
// source). Returns "" if neither is present (memory then stays disabled rather
// than crashing).
func Mem0KeyFromEnvOrFile() string {
	if k := strings.TrimSpace(os.Getenv("MEM0_API_KEY")); k != "" {
		return k
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".mem0", "api-key")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func (c *MemoryClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return c.http.Do(req)
}

type searchResp struct {
	Results []struct {
		ID     string          `json:"id"`
		Memory string          `json:"memory"`
		Text   string          `json:"text"`
		Score  json.RawMessage `json:"score"` // server returns it as a string OR number
	} `json:"results"`
}

// recallTimeout bounds the whole recall — it sits before the first model call,
// so it must be fast and never inherit the (long) model-call timeout.
const recallTimeout = 15 * time.Second

// Recall searches each readUser namespace CONCURRENTLY under one short deadline
// and returns the merged, score-sorted, de-duplicated top-`limit` memories. A
// per-namespace error is tolerated (that namespace contributes nothing); a total
// failure returns the last error with whatever was gathered — the caller treats
// an empty recall as "no memory". Concurrency + the deadline mean a single slow/
// wedged namespace can't stall planning.
func (c *MemoryClient) Recall(ctx context.Context, query string, limit int) ([]Recalled, error) {
	if limit <= 0 {
		limit = 8
	}
	rctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	type result struct {
		mems []Recalled
		err  error
	}
	ch := make(chan result, len(c.readUsers))
	for _, u := range c.readUsers {
		go func(u string) {
			mems, err := c.searchOne(rctx, query, u, limit)
			ch <- result{mems, err}
		}(u)
	}
	seen := map[string]bool{}
	var out []Recalled
	var lastErr error
	for range c.readUsers {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		for _, m := range r.mems {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// searchOne queries one namespace and returns its hits (newest decode of the
// {results:[{id,memory,score}]} shape). Empty-text results are dropped.
func (c *MemoryClient) searchOne(ctx context.Context, query, user string, limit int) ([]Recalled, error) {
	body := map[string]any{"query": query, "filters": map[string]any{"user_id": user}, "limit": limit}
	resp, err := c.do(ctx, http.MethodPost, "/v1/memories/search", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("mem0 search %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var sr searchResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	out := make([]Recalled, 0, len(sr.Results))
	for _, m := range sr.Results {
		text := m.Memory
		if text == "" {
			text = m.Text
		}
		if text == "" {
			continue
		}
		out = append(out, Recalled{ID: m.ID, Text: text, Score: parseScore(m.Score), Source: user})
	}
	return out, nil
}

// parseScore handles mem0 returning score as either a JSON string or number.
func parseScore(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var g float64
		fmt.Sscanf(s, "%g", &g)
		return g
	}
	return 0
}

type addResp struct {
	Results []struct {
		ID string `json:"id"`
	} `json:"results"`
	ID string `json:"id"`
}

// forbiddenMeta lists metadata keys the agent must NEVER set (matched
// case-insensitively): the storage tier plus the mem0 server's retrieval-gating
// keys (_ADD_FORBIDDEN_META). The SERVER is the real boundary — it 403s canonical
// and strips these itself; this client guard is defense-in-depth so the client
// never even attempts to forge tier/burial metadata, and matches the server's
// contract (lowercase-keyed) regardless of caller casing.
var forbiddenMeta = map[string]bool{
	"tier": true, "contradicts_canonical": true, "superseded_by": true,
	"tier_actor": true, "actor": true,
}

// Persist writes text as an EVIDENCE-tier memory under the isolated writeUser
// namespace, tagged source=local-agent. It NEVER sends tier=canonical (the
// server would 403 anyway) — the agent cannot reach the canonical anchor.
func (c *MemoryClient) Persist(ctx context.Context, text string, meta map[string]string) (string, error) {
	md := map[string]any{"tier": "evidence", "source": "local-agent"}
	for k, v := range meta {
		if forbiddenMeta[strings.ToLower(strings.TrimSpace(k))] { // case-fold: "Tier"/"TIER" are caught too
			continue
		}
		md[k] = v
	}
	body := map[string]any{
		"messages": text,
		"user_id":  c.writeUser,
		"agent_id": c.agentID,
		"metadata": md,
		"infer":    false, // store the record verbatim (it's an agent run outcome, not a fact to distill)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/memories", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", fmt.Errorf("mem0 add %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ar addResp
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", nil // stored; id parsing is best-effort
	}
	if len(ar.Results) > 0 {
		return ar.Results[0].ID, nil
	}
	return ar.ID, nil
}
