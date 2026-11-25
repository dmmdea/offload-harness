// Package judge provides local-model judges for the shadow-labeling flywheel:
// an embeddings-based similarity used to score whether a counterfactual tier's
// summary agrees with the reference summary (exact-match doesn't work for
// paraphrase). Calls the warm embeddinggemma on llama-swap :11436.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

type Embedder struct {
	endpoint string
	model    string
	hc       *http.Client
}

// NewEmbedder builds an embedder that posts to endpoint/v1/embeddings using the
// given model. model is the caller's configured embedder (cfg.EmbedModel()) — this
// package never hardcodes a model name, so the harness stays model-agnostic.
func NewEmbedder(endpoint, model string, timeout time.Duration) *Embedder {
	return &Embedder{endpoint: endpoint, model: model, hc: &http.Client{Timeout: timeout}}
}

// Similar embeds a and b in one call and returns their cosine similarity.
func (e *Embedder) Similar(a, b string) (float64, error) {
	vecs, err := e.embed([]string{a, b})
	if err != nil {
		return 0, err
	}
	if len(vecs) != 2 {
		return 0, fmt.Errorf("embed: got %d vectors, want 2", len(vecs))
	}
	return cosine(vecs[0], vecs[1]), nil
}

// Embed returns the embedding vector for a single text from embeddinggemma.
func (e *Embedder) Embed(text string) ([]float64, error) {
	vecs, err := e.embed([]string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embed: got %d vectors, want 1", len(vecs))
	}
	return vecs[0], nil
}

// post is one embeddings request: the status and the whole body, whatever the status.
func (e *Embedder) post(url string, body []byte) (status int, answer []byte, err error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("embed: read body: %w", err)
	}
	return resp.StatusCode, answer, nil
}

// embed posts inputs to /v1/embeddings and returns vectors ordered by the API
// `index` field (never response position). Errors on non-200 or a missing index.
func (e *Embedder) embed(inputs []string) ([][]float64, error) {
	body, err := json.Marshal(map[string]any{"model": e.model, "input": inputs})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}
	// /v1/embeddings is a model-dispatched llama-swap route: it swaps the
	// embedding model in. Under a GPU-lease fence over a cold embedder the
	// request is not sent (2026-09-22); the wait for the card is bounded by this
	// embedder's own timeout, so the kNN pre-filter on the request path fails
	// open exactly as it does on a slow embedder. A resident embedder is served.
	budget := e.hc.Timeout
	if budget <= 0 {
		budget = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	again := seatwait.NewResend(budget)
	var raw []byte
	for {
		u, err := modelaffinity.AwaitModelRoute(ctx, e.endpoint, e.model, "/v1/embeddings", time.Now().Add(budget))
		if err != nil {
			return nil, again.Err(fmt.Errorf("embed: %w", err))
		}
		sentAt := time.Now()
		status, b, err := e.post(u, body)
		if err != nil {
			return nil, again.Err(err)
		}
		if status == http.StatusOK {
			raw = b
			break
		}
		// The model went away under the request (a lease that clears the cards, another client's swap): the same
		// request loads it back, inside this embedder's own budget.
		if seatwait.TakenAway(status, string(b[:min(len(b), 400)])) && again.Again(ctx, sentAt) {
			log.Printf("embed: %s went away under an embedding (HTTP %d); sending it again", e.model, status)
			continue
		}
		return nil, again.Err(fmt.Errorf("embed: status %d", status))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float64, len(out.Data))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(out.Data) {
			return nil, fmt.Errorf("embed: index %d out of range (n=%d)", d.Index, len(out.Data))
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
