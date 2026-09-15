package main

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// applySeatBinding gives a rendered seat THE BINDING THAT NAMES IT.
//
// Why by seat name (B-01). The tier declares one reference cache server for the seat
// it ships, but a box can run more than one vLLM seat — the reference workstation's
// tensor-parallel pair and its opt-in 3-card layout, the second box's own small seat
// — and each needs its OWN store directory and key_prefix: sharing either across
// layouts is the measured silent failure (reads fail with "value size exceeds buffer
// capacity" and the tier serves nothing while reporting success). Before this, the
// harness config carried one `kv_cache_server` object with one `seat`, so the second
// seat could only be rendered by editing the tier — which is how a hardware class
// ends up describing one host's deployment.
//
// The tier keeps what only the tier knows (the mount point, the write floor, the
// prune target, the writer count, the cap): those are properties of the seat's
// hardware and share, not of the namespace. The config supplies what the DEPLOYMENT
// decides: which adapter, which directory, which namespace, how much L1 staging, and
// the chunk — which must equal the engine's unified block size for the model.
//
// A storeless binding renders a seat with NO L2 at all (the wrapper reads an empty
// SEAT_L2 as "same-box tier"), because an opt-out that still rendered the tier's
// store would be an opt-out in the config and a store on disk.
func applySeatBinding(s vllmseat.Spec, cfgPath string) (vllmseat.Spec, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return s, fmt.Errorf("--config %s: %w", cfgPath, err)
	}
	b := cfg.KVCacheServers.For(s.ID)
	if b == nil {
		// Not an error: the box may run this seat with no tier. Say so, loudly
		// enough to be read, and render the tier's own declaration unchanged.
		fmt.Printf("NOTE  %s has no kv_cache_server binding in %s — rendering the tier's own cache_server declaration; `local-offload doctor` will fail this seat until the box binds it or opts out explicitly\n", s.ID, cfgPath)
		return s, nil
	}
	if b.Storeless {
		fmt.Printf("NOTE  %s is bound storeless in %s (%s) — rendering the same-box tier: L1 staging only, no L2\n", s.ID, cfgPath, b.Reason)
		s.CacheServer = nil
		return s, nil
	}
	if !b.Enabled {
		fmt.Printf("NOTE  %s has a kv_cache_server binding in %s that is disabled and carries no storeless reason — rendering the tier's own cache_server declaration; `local-offload doctor` fails that state on purpose\n", s.ID, cfgPath)
		return s, nil
	}
	cs := vllmseat.CacheServer{}
	if s.CacheServer != nil {
		cs = *s.CacheServer
	}
	cs.Store = b.StoreName()
	cs.Address = b.Address
	cs.L1StagingGB = b.EffectiveL1StagingGB()
	cs.ChunkSize = b.EffectiveChunkSize()
	cs.KeyPrefix = b.EffectiveKeyPrefix()
	s.CacheServer = &cs
	if err := cs.Validate(); err != nil {
		return s, fmt.Errorf("kv_cache_server binding for seat %q in %s: %w", s.ID, cfgPath, err)
	}
	return s, nil
}
