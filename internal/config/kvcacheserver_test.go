package config

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// The tier is OPTIONAL: an install that never mentions it must load exactly as before,
// and a block that is present but disabled must never be inspected.
func TestKVCacheServerAbsentAndDisabledAreInert(t *testing.T) {
	def := Default()
	if def.KVCacheServer != nil {
		t.Fatalf("Default() must not declare a cache server; got %+v", def.KVCacheServer)
	}
	if err := ValidateKVCacheServer(nil); err != nil {
		t.Fatalf("nil block: %v", err)
	}
	// Disabled with garbage inside: still inert — the block is not consulted.
	if err := ValidateKVCacheServer(&KVCacheServer{Enabled: false, Store: "bogus", Address: "8.8.8.8:1"}); err != nil {
		t.Fatalf("disabled block must not validate its fields: %v", err)
	}
}

// withTailnet pins the process-global zone for one test and restores it: privateHost
// reads netguard's suffix, and sibling tests in this package set their own.
func withTailnet(t *testing.T, zone string) {
	t.Helper()
	prev := netguard.TailnetSuffix()
	if err := netguard.SetTailnetSuffix(zone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netguard.SetTailnetSuffix(prev) })
}

func TestKVCacheServerRefusesPublicAndMalformed(t *testing.T) {
	withTailnet(t, "tailnnnnnn.ts.net")
	cases := map[string]KVCacheServer{
		"public ip":            {Enabled: true, Address: "8.8.8.8:18799", Seat: "s"},
		"public mapped ip":     {Enabled: true, Address: "[::ffff:8.8.8.8]:18799", Seat: "s"},
		"no port":              {Enabled: true, Address: "10.1.2.3", Seat: "s"},
		"bad port":             {Enabled: true, Address: "10.1.2.3:99999", Seat: "s"},
		"empty":                {Enabled: true, Seat: "s"},
		"unknown store":        {Enabled: true, Store: "memcached", Address: "10.1.2.3:18799", Seat: "s"},
		"public host":          {Enabled: true, Address: "cache.example.com:18799", Seat: "s"},
		"foreign tailnet host": {Enabled: true, Address: "evil.ts.net:18799", Seat: "s"},
		"negative l1":          {Enabled: true, Address: "10.1.2.3:18799", L1StagingGB: -1, Seat: "s"},
		"negative chunk":       {Enabled: true, Address: "10.1.2.3:18799", ChunkSize: -1, Seat: "s"},
		"no namespace":         {Enabled: true, Address: "10.1.2.3:18799"},
		"fs_native public ip":  {Enabled: true, Store: "fs_native", Address: "8.8.8.8:6379", Seat: "s"},
		"fs_native url":        {Enabled: true, Store: "fs_native", Address: "https://evil.example.com/kv", Seat: "s"},
		"fs_native host:port":  {Enabled: true, Store: "fs_native", Address: "cache.example.com:18799", Seat: "s"},
		"fs_native relative":   {Enabled: true, Store: "fs_native", Address: "mnt/kv", Seat: "s"},
	}
	for name, k := range cases {
		k := k
		if err := ValidateKVCacheServer(&k); err == nil {
			t.Errorf("%s: expected a refusal, got nil", name)
		} else if !strings.Contains(err.Error(), "kv_cache_server.") {
			t.Errorf("%s: refusal must name the key, got %v", name, err)
		}
	}
	// A dotted hostname with NO zone configured is refused with the hint to set one.
	withTailnet(t, "")
	k := KVCacheServer{Enabled: true, Address: "box.tailnnnnnn.ts.net:18799", Seat: "s"}
	if err := ValidateKVCacheServer(&k); err == nil || !strings.Contains(err.Error(), "tailnet_suffix") {
		t.Fatalf("expected a tailnet_suffix hint, got %v", err)
	}
}

func TestKVCacheServerAcceptsLANTailnetAndDefaults(t *testing.T) {
	withTailnet(t, "tailnnnnnn.ts.net")
	for _, addr := range []string{"10.1.2.3:18799", "192.168.1.20:6379", "127.0.0.1:18799", "[::ffff:10.1.2.3]:18799", "[fd00::1]:6379", "lenovo:18799", "lenovo.local:18799", "box.tailnnnnnn.ts.net:18799", "  10.1.2.3:18799  "} {
		k := KVCacheServer{Enabled: true, Address: addr, Seat: "qwen3.8-27b-vllm"}
		if err := ValidateKVCacheServer(&k); err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		if k.Address != strings.TrimSpace(addr) {
			t.Errorf("%q: address not normalized: %q", addr, k.Address)
		}
		if k.StoreName() != "valkey" || k.EffectiveL1StagingGB() != 8 || k.EffectiveChunkSize() != 784 || !k.ChunkSizeDefaulted() || k.EffectiveKeyPrefix() != "qwen3.8-27b-vllm" {
			t.Errorf("%s: defaults not applied: %s %d %d %v %s", addr, k.StoreName(), k.EffectiveL1StagingGB(), k.EffectiveChunkSize(), k.ChunkSizeDefaulted(), k.EffectiveKeyPrefix())
		}
	}
	// A tailnet (carrier-grade NAT) address is accepted too — built from bytes so no
	// infrastructure-looking literal sits in the source.
	cg := netip.AddrFrom4([4]byte{100, 64, 1, 2})
	if err := privateHost(cg.String()); err != nil {
		t.Fatalf("CGNAT address must be accepted: %v", err)
	}
	// fs_native takes an absolute mounted path — never a network location.
	k := KVCacheServer{Enabled: true, Store: "fs_native", Address: "/mnt/lenovo-kv", Seat: "s"}
	if err := ValidateKVCacheServer(&k); err != nil {
		t.Fatalf("fs_native: %v", err)
	}
	if k.AddressIsIPLiteral() {
		t.Fatal("a path is not an IP literal")
	}
	if !(KVCacheServer{Address: "10.1.2.3:18799"}).AddressIsIPLiteral() || (KVCacheServer{Address: "lenovo:18799"}).AddressIsIPLiteral() {
		t.Fatal("AddressIsIPLiteral misclassifies")
	}
	explicit := KVCacheServer{Enabled: true, Address: "10.1.2.3:18799", ChunkSize: 1568, KeyPrefix: "gen2"}
	if explicit.ChunkSizeDefaulted() || explicit.EffectiveKeyPrefix() != "gen2" {
		t.Fatal("explicit values must win over defaults")
	}
}

// The refusal must surface through Load, attributed to its key, and a valid block
// must round-trip with its keys intact (the docs and the seat's seat.env quote them).
func TestKVCacheServerLoadAttributesTheKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	bad := `{"kv_cache_server":{"enabled":true,"address":"1.1.1.1:18799","seat":"s"}}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "kv_cache_server.address") {
		t.Fatalf("expected a kv_cache_server.address refusal, got %v", err)
	}
	good := `{"kv_cache_server":{"enabled":true,"store":"valkey","address":" 10.1.2.3:18799 ","l1_staging_gb":8,"chunk_size":784,"key_prefix":"qube-seat-v7","seat":"qwen3.8-27b-vllm"}}`
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.KVCacheServer == nil || !c.KVCacheServer.Enabled || c.KVCacheServer.Address != "10.1.2.3:18799" || c.KVCacheServer.EffectiveKeyPrefix() != "qube-seat-v7" {
		t.Fatalf("round-trip lost or failed to normalize the block: %+v", c.KVCacheServer)
	}
	raw, _ := json.Marshal(c.KVCacheServer)
	for _, key := range []string{`"enabled"`, `"store"`, `"address"`, `"l1_staging_gb"`, `"chunk_size"`, `"key_prefix"`, `"seat"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshalled block lacks %s: %s", key, raw)
		}
	}
}
