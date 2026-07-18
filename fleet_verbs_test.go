package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestGpuArchFromName locks the product-name → architecture-class mapping the
// health payload advertises as gpu_arch (mirrors setup/detect.ps1 Get-GpuArch).
// The dispatcher routes on arch CLASSES, not product names — "NVIDIA GeForce
// RTX 3070 Laptop GPU" is not a schedulable fact; "ampere" is.
func TestGpuArchFromName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"NVIDIA GeForce RTX 5090", "blackwell"},
		{"NVIDIA GeForce RTX 5060 Ti", "blackwell"},
		// "RTX PRO" breaks the "RTX 50" substring ("RTX PRO 5000"), so the PRO
		// rule must match in its own right — detect.ps1's documented caveat.
		{"NVIDIA RTX PRO 5000 Blackwell", "blackwell"},
		{"NVIDIA RTX PRO 6000", "blackwell"},
		{"NVIDIA RTX 4000 Blackwell", "blackwell"},
		{"NVIDIA GeForce RTX 4090", "ada"},
		{"NVIDIA GeForce RTX 4060 Laptop GPU", "ada"},
		{"NVIDIA GeForce RTX 3070 Laptop GPU", "ampere"},
		{"NVIDIA GeForce RTX 3090 Ti", "ampere"},
		{"NVIDIA GeForce RTX 2080 Ti", "turing"},
		{"NVIDIA GeForce GTX 1660 SUPER", "turing"},
		{"Tesla V100-SXM2-16GB", "volta"},
		// Unrecognized products fall back to the lowercase vendor, never "".
		{"NVIDIA TITAN X (Pascal)", "nvidia"},
		{"", "nvidia"},
		// Case-insensitive (defensive; nvidia-smi emits uppercase RTX today).
		{"nvidia geforce rtx 3070", "ampere"},
	}
	for _, tc := range cases {
		if got := gpuArchFromName(tc.name); got != tc.want {
			t.Errorf("gpuArchFromName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFleetServeParams covers the fleet-serve arg-validation seam (mirrors the
// runGraphParams seam pattern): flag > config resolution for the listen
// address and node id, the hostname fallback, and the netguard loopback
// refusal unless --listen-trusted-network.
func TestFleetServeParams(t *testing.T) {
	hostNodeA := func() (string, error) { return "node-a", nil }

	t.Run("defaults resolve from config + hostname", func(t *testing.T) {
		listen, nodeID, err := fleetServeParams("", "", false, config.Default(), hostNodeA)
		if err != nil {
			t.Fatal(err)
		}
		if listen != "127.0.0.1:18811" {
			t.Errorf("listen = %q, want the config default 127.0.0.1:18811", listen)
		}
		if nodeID != "node-a" {
			t.Errorf("nodeID = %q, want the hostname fallback \"node-a\"", nodeID)
		}
	})

	t.Run("non-loopback refused without the trusted flag", func(t *testing.T) {
		_, _, err := fleetServeParams("100.64.0.10:18811", "", false, config.Default(), hostNodeA)
		if err == nil || !strings.Contains(err.Error(), "refusing to bind") {
			t.Fatalf("err = %v, want the netguard refusal", err)
		}
	})

	t.Run("trusted flag allows the Tailscale bind; explicit flags win", func(t *testing.T) {
		listen, nodeID, err := fleetServeParams("100.64.0.10:18811", "node-a", true, config.Default(), hostNodeA)
		if err != nil {
			t.Fatal(err)
		}
		if listen != "100.64.0.10:18811" || nodeID != "node-a" {
			t.Fatalf("got (%q, %q), want the explicit flag values", listen, nodeID)
		}
	})

	t.Run("config fleet_node_id beats the hostname", func(t *testing.T) {
		cfg := config.Default()
		cfg.FleetNodeID = "cfg-node"
		_, nodeID, err := fleetServeParams("", "", false, cfg, hostNodeA)
		if err != nil {
			t.Fatal(err)
		}
		if nodeID != "cfg-node" {
			t.Errorf("nodeID = %q, want the config value \"cfg-node\"", nodeID)
		}
	})

	t.Run("hostname failure falls back to a stable literal", func(t *testing.T) {
		_, nodeID, err := fleetServeParams("", "", false, config.Default(),
			func() (string, error) { return "", errors.New("no hostname") })
		if err != nil {
			t.Fatal(err)
		}
		if nodeID != "fleet-node" {
			t.Errorf("nodeID = %q, want \"fleet-node\"", nodeID)
		}
	})

	t.Run("config fleet_listen beats the built-in fallback", func(t *testing.T) {
		cfg := config.Default()
		cfg.FleetListen = "127.0.0.1:18899"
		listen, _, err := fleetServeParams("", "", false, cfg, hostNodeA)
		if err != nil {
			t.Fatal(err)
		}
		if listen != "127.0.0.1:18899" {
			t.Errorf("listen = %q, want the config value", listen)
		}
	})
}
