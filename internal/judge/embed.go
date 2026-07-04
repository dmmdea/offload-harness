// Package judge provides local-model judges for the shadow-labeling flywheel:
// an embeddings-based similarity used to score whether a counterfactual tier's
// summary agrees with the reference summary (exact-match doesn't work for
// paraphrase). Calls the warm embeddinggemma on llama-swap :11436.
package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

type Embedder struct {
	endpoint string
	hc       *http.Client
}

func NewEmbedder(endpoint string, timeout time.Duration) *Embedder {
	return &Embedder{endpoint: endpoint, hc: &http.Client{Timeout: timeout}}
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

// embed posts inputs to /v1/embeddings and returns vectors ordered by the API
// `index` field (never response position). Errors on non-200 or a missing index.
func (e *Embedder) embed(inputs []string) ([][]float64, error) {
	body, err := json.Marshal(map[string]any{"model": "embeddinggemma", "input": inputs})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}
	req, err := http.NewRequest("POST", e.endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: status %d", resp.StatusCode)
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
