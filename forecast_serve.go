package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/forecastserve"
)

// runForecastServe is the launcher a forecast seat of llama-swap runs: it holds the Timestral
// python server (internal/forecastserve) for its own life, so llama-swap's stop never orphans the
// process that holds the card. The seat's cmd passes --forecast-dir, --model and --port; the card
// comes from the seat's CUDA_VISIBLE_DEVICES, so --device stays cuda.
func runForecastServe(args []string) error {
	fs := flag.NewFlagSet("forecast-serve", flag.ExitOnError)
	dir := fs.String("forecast-dir", "", "the forecast directory holding venv/ (default: <home>/forecast); the embedded server is written here")
	model := fs.String("model", "", "the Timestral checkpoint directory (config.json + model.safetensors)")
	port := fs.Int("port", 0, "port to listen on (llama-swap passes ${PORT})")
	host := fs.String("host", "127.0.0.1", "address to bind; loopback only")
	device := fs.String("device", "cuda", "torch device: cuda, cuda:N, xpu or mps (the server refuses cpu)")
	maxRowPatches := fs.Int("max-row-patches", 0, "rows x context patches per forward pass, which bounds the peak card memory (0 = the server's default)")
	_ = fs.Parse(args)

	if *model == "" {
		return fmt.Errorf("forecast-serve: --model is required (the Timestral checkpoint directory)")
	}
	if *port == 0 {
		return fmt.Errorf("forecast-serve: --port is required")
	}
	d := *dir
	if d == "" {
		d = filepath.Join(config.DefaultBase(), "forecast")
	}
	code, err := forecastserve.Serve(context.Background(), forecastserve.Options{
		Dir: d, Model: *model, Host: *host, Port: *port, Device: *device, MaxRowPatches: *maxRowPatches,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitCodeError{code: code, msg: fmt.Sprintf("forecast-serve: the forecast server exited with code %d", code)}
	}
	return nil
}
