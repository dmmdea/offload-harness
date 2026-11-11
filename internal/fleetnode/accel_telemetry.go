package fleetnode

import (
	"slices"

	"github.com/dmmdea/offload-harness/internal/accelclient"
	"github.com/dmmdea/offload-harness/internal/config"
)

// accelTelemetrySources lists the telemetry inputs of every device this node advertises
// (fleet-visible, so never the local-only Hailo-8L) AND lists locally in its own config, which is
// where the sidecar endpoint and launcher come from. Only a device whose launcher answers
// `--telemetry` has a source: today the Axera card (the lane table in mcpserver.accelLaneConfigFor
// marks it, config carries its endpoint and launcher).
func accelTelemetrySources(cfg config.Config, advertised []string) []accelclient.TelemetrySource {
	var out []accelclient.TelemetrySource
	for _, id := range advertised {
		if !slices.Contains(cfg.Accelerators, id) {
			continue
		}
		switch id {
		case "axera-ax650":
			out = append(out, accelclient.TelemetrySource{Device: id, Endpoint: cfg.AxeraEndpoint, Cmd: cfg.AxeraSidecarCmd})
		}
	}
	return out
}

// acceleratorTelemetry is health's `accelerator_telemetry`: per device, the compact telemetry the
// collector holds. It never blocks the request (the collector refreshes in the background, one
// read in flight per device), and a device with nothing read yet is left out until the first
// refresh lands. nil when no advertised device has telemetry, so the key is omitted.
func (s *Server) acceleratorTelemetry() map[string]map[string]any {
	if s.accelTelemetry == nil {
		return nil
	}
	var out map[string]map[string]any
	for _, src := range accelTelemetrySources(s.opts.Cfg, s.accelerators) {
		v := s.accelTelemetry.Get(src)
		if v == nil {
			continue
		}
		if out == nil {
			out = map[string]map[string]any{}
		}
		out[src.Device] = v
	}
	return out
}
