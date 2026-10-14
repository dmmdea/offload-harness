package backendchain

import (
	"context"
	"net/http"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Options tune one collection.
type Options struct {
	// HTTP is the client every read goes over; the caller supplies the one its package already
	// builds for the configured llama-swap endpoint. Required.
	HTTP *http.Client
	// ProbeCompletion adds a 1-token completion probe. It LOADS an idle seat.
	ProbeCompletion bool
	// ProbeTimeout bounds each cheap read; zero means 3 s.
	ProbeTimeout time.Duration
	// CompletionTimeout bounds each completion probe; zero means 6 min (a cold vLLM load takes minutes).
	CompletionTimeout time.Duration
}

// Collect reads the node once and reports every lane and every seat.
func Collect(ctx context.Context, cfg config.Config, opt Options) Report { return Report{} }
