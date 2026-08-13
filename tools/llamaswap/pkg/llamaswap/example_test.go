// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package llamaswap_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"llamaswap-pp-cli/pkg/llamaswap"
)

// rosterJSON is the shape llama-swap returns from GET /v1/models: a canonical
// id plus the aliases the model also answers to.
const rosterJSON = `{"data":[
  {"id":"embeddinggemma","name":"EmbeddingGemma-300m",
   "meta":{"llamaswap":{"aliases":["text-embedding","local-embed"]}},
   "status":{"value":"loaded"}},
  {"id":"bge-reranker-v2-m3","name":"bge reranker",
   "meta":{"llamaswap":{"aliases":["reranker-v2-m3","v0.12-reranker"]}},
   "status":{"value":"loaded"}}
]}`

// ExampleClient_Resolve shows the alias resolution that is the whole reason
// this package exists: "local-embed" is not a model id, it is an alias, and
// addressing llama-swap by the wrong one of the two is the single most common
// integration bug on this API.
func ExampleClient_Resolve() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, rosterJSON)
	}))
	defer srv.Close()

	c, err := llamaswap.New(srv.URL, nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	id, aliases, err := c.Resolve(context.Background(), "local-embed")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("canonical id:", id)
	fmt.Println("answers to:  ", aliases)

	// An unknown name is a typed error, not a guess.
	if _, _, err := c.Resolve(context.Background(), "gemma-4-99b"); err != nil {
		fmt.Println("exit code:   ", llamaswap.ExitCode(err))
	}

	// Output:
	// canonical id: embeddinggemma
	// answers to:   [text-embedding local-embed]
	// exit code:    3
}
