package config

// UpdateConfig is the `update` object of config.json, read by `local-offload update` (docs/systems/update.md).
// Every field is optional. internal/update.FileConfig decodes the same JSON; a key added here is added there.
type UpdateConfig struct {
	// Channel is "nightly" (default: the newest complete build) or "release" (llama.cpp only: the build the
	// publisher's release-tag file names).
	Channel string `json:"channel,omitempty"`
	// Paths overrides a component's live path, keyed by component (llama, llama-swap, whisper, sdcpp,
	// hyperframes). The default is <home>/<component>; llama-swap's is the executable itself.
	Paths map[string]string `json:"paths,omitempty"`
	// Restart overrides how the llama-swap daemon is stopped and restarted around a swap, keyed by component
	// (only llama-swap restarts).
	Restart map[string]UpdateRestart `json:"restart,omitempty"`
	// LlamaSwapYAML is the rendered llama-swap config the llama flag check and the smoke gate read
	// (default <home>/llama-swap.yaml).
	LlamaSwapYAML string `json:"llama_swap_yaml,omitempty"`
	// Gate configures the side-port smoke gate and the post-flip watch.
	Gate *UpdateGate `json:"gate,omitempty"`
	// Home is the install root the update verb works on when neither --home nor $OFFLOAD_HOME names one.
	Home string `json:"home,omitempty"`
	// Variant names a component's build family (cuda13, cuda12, vulkan or cpu), keyed by component. The default is
	// the family of the build at the component's live path, else the node's serving backend.
	Variant map[string]string `json:"variant,omitempty"`
}

// UpdateRestart names how the daemon restarts: a scheduled task, a shell command, or None (no restart).
type UpdateRestart struct {
	Task    string `json:"task,omitempty"`
	Command string `json:"command,omitempty"`
	None    bool   `json:"none,omitempty"`
}

// UpdateGate is `update.gate`. Durations are Go duration strings ("30m").
type UpdateGate struct {
	// Enabled false turns the smoke gate and the post-flip watch off (default on).
	Enabled *bool `json:"enabled,omitempty"`
	// LeaseWait is how long the gate queues for the GPU lease (default 30m); past it the answer is inconclusive.
	LeaseWait string `json:"lease_wait,omitempty"`
	// SidePort is the loopback port of the gate's llama-server (default 11499).
	SidePort int `json:"side_port,omitempty"`
	// LoadTimeout is how long one llama-server may take to answer /health (default 5m).
	LoadTimeout string `json:"load_timeout,omitempty"`
	// PostFlipTimeout is how long the first real seat load after the flip is watched (default 5m).
	PostFlipTimeout string `json:"post_flip_timeout,omitempty"`
}
