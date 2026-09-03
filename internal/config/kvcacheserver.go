package config

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// KVCacheServer is the OPTIONAL "cache server" tier (operator directive 2026-09-02):
// a second machine's RAM that holds the KV pages that left a vLLM seat's VRAM, behind
// LMCache MP's small L1 staging buffer on the serving box. Off by default; the
// install and every single-box seat never depend on it — a box with no second device
// runs exactly as before, and this block exists so a box WITH one can declare it.
//
// Why it is a tier and not a speedup: a context that fell out of VRAM comes back at
// parity cost or better with recomputing it (measured on Qwen3.8-27B, 24k tokens:
// 3.86 s from a second box over the LAN vs 24.68 s recompute with one store
// namespace per stack generation; 20.6 s when the store was shared across layouts)
// while the GPU is free for other requests and the serving box's RAM is untouched. Sized in tokens: ~255 KB per token as stored, so a 45 GB store holds
// ~175k tokens.
//
// The harness does not run the store or the engine. This block is DECLARATIVE: it is
// what `offload_status` reports and what the docs describe; the seat wrapper
// (setup/templates/vllm-seat/seat_fg.sh) reads its own seat.env, so the operator
// keeps the two in agreement — the status view says so in its note.
type KVCacheServer struct {
	// Enabled turns the tier on. False = the block is inert and reported as such.
	Enabled bool `json:"enabled"`
	// Store is the L2 adapter behind LMCache MP: "valkey" (Redis protocol; the
	// measured default) or "fs_native" (a filesystem export mounted on the serving
	// box). Empty = valkey.
	Store string `json:"store,omitempty"`
	// Address: for valkey, the store's host:port on the DIRECT LAN or the tailnet — a
	// private IPv4/IPv6, a tailnet CGNAT address, loopback, a bare hostname, a
	// `.local` name, or a name under `tailnet_suffix`. A public address is refused by
	// name: KV pages are unauthenticated bulk memory and never leave the operator's
	// networks. For fs_native, an ABSOLUTE local path (the mount point of the
	// export); a URL or host:port is refused so a network location cannot slip in
	// under the store that skips the network checks. Bulk KV prefers the direct LAN
	// over MagicDNS (WireGuard measured 6.6x slower, 2026-09-01) — guidance, not a
	// rule enforced here. Surrounding whitespace is trimmed at load.
	Address string `json:"address,omitempty"`
	// L1StagingGB is LMCache MP's pinned host buffer beside the engine (GB). It is a
	// staging area, not the tier: 8 GB restored a 24k context entirely from the store.
	// 0 = 8.
	L1StagingGB int `json:"l1_staging_gb,omitempty"`
	// ChunkSize is LMCache's chunk in tokens and must equal the engine's unified block
	// size for the seat's model ("Setting attention block size to N tokens" in the
	// vLLM log; 784 for Qwen3.8-27B with fp16 KV, 1568 with fp8). 0 = 784 — a
	// model-specific number, so status marks it as defaulted. A mismatch fails the
	// engine's registration at start, loudly.
	ChunkSize int `json:"chunk_size,omitempty"`
	// KeyPrefix namespaces the store. Change it (or flush the store) whenever the
	// engine layout, the KV dtype or the LMCache build changes: objects written
	// under another generation fail reads with "value size exceeds buffer capacity"
	// and the tier silently serves nothing (measured 2026-09-03 00:25). Empty = the
	// seat name; when both are empty the block is refused (two seats must never share
	// a namespace by accident).
	KeyPrefix string `json:"key_prefix,omitempty"`
	// Seat is the llama-swap model id of the vLLM seat this tier backs (e.g.
	// "qwen3.8-27b-vllm"). Informational: status reports it next to the store.
	Seat string `json:"seat,omitempty"`
}

// StoreName is the L2 adapter with the default applied.
func (k KVCacheServer) StoreName() string {
	if strings.TrimSpace(k.Store) == "" {
		return "valkey"
	}
	return strings.ToLower(strings.TrimSpace(k.Store))
}

// EffectiveL1StagingGB applies the 8 GB default.
func (k KVCacheServer) EffectiveL1StagingGB() int {
	if k.L1StagingGB <= 0 {
		return 8
	}
	return k.L1StagingGB
}

// EffectiveChunkSize applies the 784-token default (Qwen3.8-27B / fp16 KV).
func (k KVCacheServer) EffectiveChunkSize() int {
	if k.ChunkSize <= 0 {
		return 784
	}
	return k.ChunkSize
}

// ChunkSizeDefaulted reports whether EffectiveChunkSize is the model-specific
// default rather than an operator value — status surfaces it, since 784 is only
// right for one model.
func (k KVCacheServer) ChunkSizeDefaulted() bool { return k.ChunkSize <= 0 }

// EffectiveKeyPrefix applies the seat-name default. Empty means "neither set" — a
// state validation refuses when the block is enabled.
func (k KVCacheServer) EffectiveKeyPrefix() string {
	if p := strings.TrimSpace(k.KeyPrefix); p != "" {
		return p
	}
	return strings.TrimSpace(k.Seat)
}

// AddressIsIPLiteral reports whether Address names an IP (with or without a port)
// rather than a hostname — the status probe dials literals only, because a hostname
// is vetted by shape, not by what DNS answers (the same two-layer reasoning as
// netguard's tailnet dial gate).
func (k KVCacheServer) AddressIsIPLiteral() bool {
	host := strings.TrimSpace(k.Address)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// ValidateKVCacheServer refuses a block that could not work, BY NAME, and normalizes
// Address (trimmed) so every consumer dials what was vetted. Exported so the status
// view can re-check a block on a server that was started against a refused config. A
// disabled block is never inspected: the tier is optional and an install that never
// enables it must not be able to fail on it.
func ValidateKVCacheServer(k *KVCacheServer) error {
	if k == nil || !k.Enabled {
		return nil
	}
	k.Address = strings.TrimSpace(k.Address)
	switch k.StoreName() {
	case "valkey", "fs_native":
	default:
		return fmt.Errorf("kv_cache_server.store: %q is not a supported store (valkey, fs_native)", k.Store)
	}
	addr := k.Address
	if addr == "" {
		return fmt.Errorf("kv_cache_server.address: required when enabled (valkey: host:port of the store on the LAN or tailnet; fs_native: the absolute path of the mounted export)")
	}
	switch k.StoreName() {
	case "valkey":
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("kv_cache_server.address: %q must be host:port: %w", addr, err)
		}
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			return fmt.Errorf("kv_cache_server.address: %q has no valid port", addr)
		}
		if err := privateHost(host); err != nil {
			return fmt.Errorf("kv_cache_server.address: %w", err)
		}
	case "fs_native":
		// A path, never a network location: the store that skips the network checks
		// must not be able to smuggle one in.
		if strings.Contains(addr, "://") {
			return fmt.Errorf("kv_cache_server.address: %q is a URL; fs_native takes the absolute path of a mounted export (use store \"valkey\" for a network store)", addr)
		}
		if _, _, err := net.SplitHostPort(addr); err == nil {
			return fmt.Errorf("kv_cache_server.address: %q is host:port; fs_native takes the absolute path of a mounted export (use store \"valkey\" for a network store)", addr)
		}
		if !filepath.IsAbs(addr) && !strings.HasPrefix(addr, "/") {
			return fmt.Errorf("kv_cache_server.address: %q must be an absolute path for fs_native", addr)
		}
	}
	if k.L1StagingGB < 0 {
		return fmt.Errorf("kv_cache_server.l1_staging_gb: %d must be >= 0 (0 = default 8)", k.L1StagingGB)
	}
	if k.ChunkSize < 0 {
		return fmt.Errorf("kv_cache_server.chunk_size: %d must be >= 0 (0 = default 784, Qwen3.8-27B fp16 KV)", k.ChunkSize)
	}
	if k.EffectiveKeyPrefix() == "" {
		return fmt.Errorf("kv_cache_server.key_prefix or kv_cache_server.seat: one is required when enabled — the store namespace must be deliberate, never a shared constant")
	}
	return nil
}

// privateHost accepts a LAN or tailnet IP, loopback, a bare hostname, a `.local`
// name, or a name under the operator's tailnet zone; it refuses public addresses.
// The store carries raw KV memory with no authentication, so "somewhere on the
// internet" is never a valid answer, however the config was pasted.
func privateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("empty host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap() // ::ffff:10.1.2.3 is the same private address
		if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() {
			return nil
		}
		// Tailscale's carrier-grade NAT block (RFC 6598, the /10 whose first octet is 100 and
		// second octet is 64..127) is not "private" to the stdlib.
		if a.Is4() {
			b := a.As4()
			if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
				return nil
			}
		}
		return fmt.Errorf("%s is a public address; the KV store must be on the LAN or the tailnet", host)
	}
	// A hostname: bare names resolve on the LAN/MagicDNS; dotted names must be
	// under the tailnet zone (the same rule seat_endpoints apply) or a .local name.
	lower := strings.ToLower(host)
	if !strings.Contains(host, ".") || strings.HasSuffix(lower, ".local") {
		return nil
	}
	suf := strings.ToLower(strings.TrimSpace(netguard.TailnetSuffix()))
	if suf == "" {
		return fmt.Errorf("%s is a dotted hostname and no tailnet_suffix is configured — use the store's LAN IP, a bare hostname, or set config `tailnet_suffix` to your tailnet zone", host)
	}
	if strings.HasSuffix(lower, "."+suf) {
		return nil
	}
	return fmt.Errorf("%s is neither a private/tailnet address nor a bare, .local or %s hostname", host, suf)
}
