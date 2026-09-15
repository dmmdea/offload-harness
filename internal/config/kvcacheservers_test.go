package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The 0.117-and-earlier shape is ONE object. It must keep loading, and it must land
// as a one-element list bound to its own seat — the compatibility promise of B-01 is
// that no deployed config has to change to keep working.
func TestLegacySingleObjectDecodesAsAOneElementList(t *testing.T) {
	var l KVCacheServers
	raw := `{"enabled":true,"store":"fs_native","address":"/mnt/kvcache/lmcache-seat-tp2-fp8","chunk_size":1568,"key_prefix":"qube-seat-tp2-fp8","seat":"qwen3.8-27b-vllm"}`
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	if len(l) != 1 || l[0].Seat != "qwen3.8-27b-vllm" || l[0].EffectiveKeyPrefix() != "qube-seat-tp2-fp8" {
		t.Fatalf("legacy object did not become its own one-element binding: %+v", l)
	}
	if l.For("qwen3.8-27b-vllm") == nil {
		t.Fatal("the legacy binding must answer For() on its own seat")
	}
	// A legacy object that named NO seat is the box default and backs every seat.
	var d KVCacheServers
	if err := json.Unmarshal([]byte(`{"enabled":true,"address":"10.1.2.3:18799","key_prefix":"gen1"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.For("any-vllm-seat") == nil || d.Default() == nil {
		t.Fatalf("a seatless legacy object must be the box default: %+v", d)
	}
	// null and the list shape both decode.
	var n KVCacheServers
	if err := json.Unmarshal([]byte(`null`), &n); err != nil || n != nil {
		t.Fatalf("null must decode to no bindings: %v %+v", err, n)
	}
	var many KVCacheServers
	if err := json.Unmarshal([]byte(`[{"enabled":true,"store":"fs_native","address":"/mnt/a","chunk_size":1568,"key_prefix":"a","seat":"s1"},{"seat":"s2","storeless":true,"reason":"pipeline seat"}]`), &many); err != nil {
		t.Fatal(err)
	}
	if len(many) != 2 || many.For("s2") == nil || !many.For("s2").Storeless {
		t.Fatalf("list shape lost a binding: %+v", many)
	}
	// A shape that is neither is refused BY KEY, naming both accepted spellings.
	var bad KVCacheServers
	err := json.Unmarshal([]byte(`"lenovo"`), &bad)
	if err == nil || !strings.Contains(err.Error(), "kv_cache_server") {
		t.Fatalf("a string must be refused naming the key, got %v", err)
	}
}

// Seat selection: an exact match always beats the box default, so a box can declare
// one default store and still give one seat its own directory and namespace.
func TestBindingSelectionPrefersTheSeatOverTheDefault(t *testing.T) {
	l := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/default", ChunkSize: 1568, KeyPrefix: "box-default"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/3card", ChunkSize: 1568, KeyPrefix: "qube-3card", Seat: "qwen3.8-27b-vllm-3card"},
	}
	if got := l.For("qwen3.8-27b-vllm-3card"); got == nil || got.Address != "/mnt/kv/3card" {
		t.Fatalf("the seat's own binding must win over the default: %+v", got)
	}
	if got := l.For("qwen3.8-27b-vllm"); got == nil || got.Address != "/mnt/kv/default" {
		t.Fatalf("a seat with no binding of its own falls to the default: %+v", got)
	}
	if !l.AnyEnabled() {
		t.Fatal("AnyEnabled must see the store bindings")
	}
	if got := (KVCacheServers{}).For("x"); got != nil {
		t.Fatal("an empty list binds nothing")
	}
}

// The gate's roster arithmetic, in one place: a seat is COVERED by a store or by an
// explicit opt-out, and by nothing else — a binding that is merely present and
// disabled is the silence the gate exists to catch.
func TestUnboundSeatsCountsSilenceAsUnbound(t *testing.T) {
	l := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/pair", ChunkSize: 1568, KeyPrefix: "pair", Seat: "pair"},
		{Seat: "pipeline", Storeless: true, Reason: "three-stage pipeline seat: no L2 layout works"},
		{Enabled: false, Seat: "quiet"},
	}
	seats := []string{"pair", "pipeline", "quiet", "never-mentioned"}
	got := l.UnboundSeats(seats)
	want := []string{"quiet", "never-mentioned"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("UnboundSeats = %v, want %v", got, want)
	}
	if extra := l.BoundSeatsNotDeclared([]string{"pair"}); strings.Join(extra, ",") != "pipeline,quiet" {
		t.Fatalf("a binding for a seat the box does not list must be surfaced: %v", extra)
	}
}

// One binding per seat. Two bindings for one seat is not a merge — it is two stores
// whose order in the file decides which one the seat gets.
func TestTwoBindingsForOneSeatAreRefused(t *testing.T) {
	l := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "a", Seat: "s1"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 1568, KeyPrefix: "b", Seat: "s1"},
	}
	err := ValidateKVCacheServers(l)
	if err == nil || !strings.Contains(err.Error(), "two bindings for seat") || !strings.Contains(err.Error(), "kv_cache_server") {
		t.Fatalf("expected a duplicate-seat refusal naming the key, got %v", err)
	}
	// Two BOX DEFAULTS are the same defect wearing no name.
	d := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "a"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 1568, KeyPrefix: "b"},
	}
	if err := ValidateKVCacheServers(d); err == nil || !strings.Contains(err.Error(), "box default") {
		t.Fatalf("two box defaults must be refused, naming them, got %v", err)
	}
	// One of each is fine.
	ok := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "a"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 1568, KeyPrefix: "b", Seat: "s1"},
	}
	if err := ValidateKVCacheServers(ok); err != nil {
		t.Fatalf("one default plus one seat binding is legal: %v", err)
	}
}

// B-45, enforced: one key_prefix per stack generation. Pages written under another
// layout are unreadable, not stale, and the tier then serves nothing while reporting
// success — so a shared namespace must be shown to be safe, not assumed.
func TestSharedKeyPrefixAcrossGenerationsIsRefused(t *testing.T) {
	diff := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "shared", Seat: "pair", KVDtype: "fp8", TensorParallel: 2},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 784, KeyPrefix: "shared", Seat: "trio", KVDtype: "fp16", TensorParallel: 3},
	}
	err := ValidateKVCacheServers(diff)
	if err == nil || !strings.Contains(err.Error(), "kv_cache_server.key_prefix") || !strings.Contains(err.Error(), "DIFFERENT stack generations") {
		t.Fatalf("a prefix shared across generations must be refused by key, got %v", err)
	}
	// Undeclared generation on a SHARED prefix: cannot be shown safe, so refused —
	// with the two ways out named.
	silent := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "shared", Seat: "pair", KVDtype: "fp8", TensorParallel: 2},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 1568, KeyPrefix: "shared", Seat: "trio"},
	}
	err = ValidateKVCacheServers(silent)
	if err == nil || !strings.Contains(err.Error(), "declares no stack generation") {
		t.Fatalf("an undeclared generation on a shared prefix must be refused, got %v", err)
	}
	// The SAME generation may share a namespace deliberately.
	same := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, KeyPrefix: "shared", Seat: "pair", KVDtype: "fp8", TensorParallel: 2},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 1568, KeyPrefix: "shared", Seat: "twin", KVDtype: "FP8", TensorParallel: 2},
	}
	if err := ValidateKVCacheServers(same); err != nil {
		t.Fatalf("one generation may share one namespace: %v", err)
	}
	// Distinct prefixes never need a declaration — the default IS the seat name.
	apart := KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/a", ChunkSize: 1568, Seat: "pair"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kv/b", ChunkSize: 784, Seat: "trio"},
	}
	if err := ValidateKVCacheServers(apart); err != nil {
		t.Fatalf("per-seat prefixes need no generation declaration: %v", err)
	}
	// A storeless opt-out writes no pages and therefore shares no namespace: it can
	// never be the reason a prefix looks shared.
	withOptOut := append(KVCacheServers{}, apart...)
	withOptOut = append(withOptOut, &KVCacheServer{Seat: "quiet", Storeless: true, Reason: "no store on this box"})
	if err := ValidateKVCacheServers(withOptOut); err != nil {
		t.Fatalf("a storeless binding must not participate in the namespace rule: %v", err)
	}
}

// The opt-out is allowed, but only as a STATEMENT: storeless with no reason is the
// silence the doctor gate exists to refuse, and it must not be able to pass by also
// being disabled.
func TestStorelessNeedsAReasonAndNoStore(t *testing.T) {
	no := &KVCacheServer{Seat: "s", Storeless: true}
	if err := ValidateKVCacheServer(no); err == nil || !strings.Contains(err.Error(), "kv_cache_server.reason") {
		t.Fatalf("storeless without a reason must be refused by key, got %v", err)
	}
	// ...including while `enabled` is false, which is the shape that would otherwise
	// slip through the "a disabled block is never inspected" rule.
	if err := ValidateKVCacheServer(&KVCacheServer{Seat: "s", Storeless: true, Enabled: false}); err == nil {
		t.Fatal("a disabled storeless binding must still be required to say why")
	}
	half := &KVCacheServer{Seat: "s", Storeless: true, Reason: "why", Store: "fs_native", Address: "/mnt/kv/x"}
	if err := ValidateKVCacheServer(half); err == nil || !strings.Contains(err.Error(), "kv_cache_server.storeless") {
		t.Fatalf("a storeless binding that also names a store must be refused, got %v", err)
	}
	good := &KVCacheServer{Seat: "s", Storeless: true, Reason: "three-stage pipeline seat: no L2 layout works"}
	if err := ValidateKVCacheServer(good); err != nil {
		t.Fatalf("an explained opt-out is legal: %v", err)
	}
}

// The refusals must surface through Load, attributed to the key AND to the binding,
// because a config with several seats bound must not leave the operator guessing
// which one is broken.
func TestLoadAttributesTheBindingAndTheKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	bad := `{"kv_cache_server":[{"enabled":true,"store":"fs_native","address":"/mnt/kv/a","chunk_size":1568,"seat":"s1"},{"enabled":true,"address":"1.1.1.1:18799","seat":"s2"}]}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "kv_cache_server.address") || !strings.Contains(err.Error(), `binding 1 "s2"`) {
		t.Fatalf("expected a refusal naming binding 1 and the address key, got %v", err)
	}
	good := `{"vllm_seats":["pair","trio"],"kv_cache_server":[
	  {"enabled":true,"store":"fs_native","address":" /mnt/kv/pair ","chunk_size":1568,"key_prefix":"qube-pair-fp8","seat":"pair","kv_dtype":"fp8","tensor_parallel":2},
	  {"seat":"trio","storeless":true,"reason":"three-stage pipeline seat: LMCache has no working L2 layout for it"}]}`
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.KVCacheServers) != 2 || len(c.KVCacheServers.UnboundSeats(c.VLLMSeats)) != 0 {
		t.Fatalf("both seats must be covered: %+v", c.KVCacheServers)
	}
	if b := c.KVCacheServers.For("pair"); b == nil || b.Address != "/mnt/kv/pair" {
		t.Fatalf("address not normalized at load: %+v", b)
	}
	// The new keys must survive a marshal — the docs, the seat.env and the live-half
	// runbook all quote them.
	raw, _ := json.Marshal(c.KVCacheServers)
	for _, key := range []string{`"seat"`, `"key_prefix"`, `"kv_dtype"`, `"tensor_parallel"`, `"storeless"`, `"reason"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshalled bindings lack %s: %s", key, raw)
		}
	}
	// The canonical shape marshals as a LIST.
	if !strings.HasPrefix(string(raw), "[") {
		t.Errorf("bindings must marshal as a list, got %s", raw)
	}
}
