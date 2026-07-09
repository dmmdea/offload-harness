package main

// serve.go exposes the agent loop as a minimal OpenAI-compatible HTTP server
// (/v1/models + /v1/chat/completions) so any chat GUI — OpenWebUI, etc. — can
// drive it. Each chat request runs the FULL agent loop (write/edit/run tools,
// broker-gated, worktree-confined) over the latest user message and returns the
// final answer as an OpenAI chat completion. The heavy lifting (tools, policy,
// llama-swap client) is the same shared Loop the CLI uses — this file is only
// the HTTP shell + OpenAI wire format.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// validateListenAddr refuses any --listen host that is not loopback. The server
// is UNAUTHENTICATED by design and each chat request drives an agent loop with
// file-write/GitHub tools, so binding beyond loopback exposes an RCE-class
// surface to the local network — a footgun for anyone publishing this repo.
//
// Empty-host forms ("", ":18800") are treated as non-loopback: Go's net.Listen
// binds ALL interfaces when the host is empty, so they are exactly as exposed as
// "0.0.0.0" and must be refused too. Bracketed IPv6 ("[::1]") is handled by
// net.SplitHostPort, which strips the brackets; we also accept a bare "[::1]"
// host defensively in case brackets survive.
//
// allowNonLocal (from --listen-trusted-network) overrides the refusal; the loud
// warning is the caller's responsibility, not the validator's — this stays a
// pure, side-effect-free function so it is trivial to test.
func validateListenAddr(addr string, allowNonLocal bool) error {
	if allowNonLocal {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Missing port / malformed — we cannot prove it is loopback, so refuse.
		return fmt.Errorf("cannot parse --listen %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to bind --listen %q: this endpoint is UNAUTHENTICATED and "+
		"each request drives the agent's write/GitHub tools; binding beyond loopback exposes "+
		"an RCE-class surface to your network. Use a loopback address (127.0.0.1, [::1], localhost) "+
		"or pass --listen-trusted-network to override (only on a network you fully trust)", addr)
}

// isLoopbackHost reports whether host is a loopback address the guard permits.
// "localhost" is accepted by name (it resolves to loopback); an empty host is
// NOT loopback (it binds all interfaces). Numeric hosts are checked via
// net.IP.IsLoopback so any 127.0.0.0/8 address and ::1 count.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.Trim(host, "[]") // tolerate a surviving bracketed IPv6 literal
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
}

// lastUserMessage returns the content of the final user-role message — the goal
// the agent should act on this turn. Empty if there is no user message.
func lastUserMessage(msgs []openAIMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// newChatCompletion builds a non-streaming OpenAI chat completion carrying the
// agent's final output as the assistant message.
func newChatCompletion(id, model, content string, created int64) openAIChatResponse {
	return openAIChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      openAIMessage{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
	}
}

// serveOpenAI starts the HTTP server. loop is the shared agent loop; modelID is
// the name advertised to the client (what the user picks in the GUI).
func serveOpenAI(listen string, loop *agent.Loop, modelID string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": modelID, "object": "model", "owned_by": "local-offload"},
			},
		})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request json: "+err.Error(), http.StatusBadRequest)
			return
		}
		goal := lastUserMessage(req.Messages)
		if strings.TrimSpace(goal) == "" {
			http.Error(w, "no user message in request", http.StatusBadRequest)
			return
		}

		res, err := loop.Run(r.Context(), goal)
		content := res.Output
		if err != nil {
			content = "agent error: " + err.Error()
		} else if strings.TrimSpace(content) == "" {
			content = fmt.Sprintf("(agent stopped: %s, %d steps, no text output)", res.StopReason, res.Steps)
		}

		id := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
		created := time.Now().Unix()
		if req.Stream {
			writeSSE(w, id, modelID, content, created)
			return
		}
		writeJSON(w, newChatCompletion(id, modelID, content, created))
	})

	// A health/root ping so the GUI's "test connection" succeeds.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("local-agent OpenAI-compatible server\n"))
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("[local-agent] OpenAI server on http://%s  (model=%q)\n", listen, modelID)
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeSSE emits the full content as a single OpenAI streaming chunk followed by
// a finish chunk and [DONE]. The agent runs the whole loop before producing
// output, so token-by-token streaming isn't available — one content chunk is the
// honest representation and satisfies GUIs that require stream:true.
func writeSSE(w http.ResponseWriter, id, model, content string, created int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	chunk := func(delta openAIMessage, finish *string) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	chunk(openAIMessage{Role: "assistant", Content: content}, nil)
	stop := "stop"
	chunk(openAIMessage{}, &stop)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
