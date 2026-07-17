package cache

import (
	"bytes"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPutGetRoundTrip(t *testing.T) {
	c := openTemp(t)
	want := []byte(`{"summary":"ok"}`)
	if err := c.Put("k1", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get("k1")
	if !ok {
		t.Fatal("Get missed a key that was just written")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get = %s, want %s", got, want)
	}
}

func TestGetMiss(t *testing.T) {
	c := openTemp(t)
	got, ok := c.Get("never-written")
	if ok {
		t.Errorf("Get reported a hit for an absent key (value %q)", got)
	}
	if got != nil {
		t.Errorf("Get = %q on a miss, want nil", got)
	}
}

func TestPutOverwrites(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("k", []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put("k", []byte("new")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := c.Get("k")
	if string(got) != "new" {
		t.Errorf("Get = %s, want the overwritten value", got)
	}
}

// Get must copy out of the bbolt transaction: the mmap-backed slice is only
// valid inside View, so a returned alias would be freed under the caller.
func TestGetReturnsACopy(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("k", []byte("original")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := c.Get("k")
	copy(got, "MUTATED!")

	again, _ := c.Get("k")
	if string(again) != "original" {
		t.Errorf("mutating a Get result corrupted the store: %s", again)
	}
}

// The cache is the reason a repeat request skips the model, so a hit has to
// survive a process restart, not just live in memory.
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c1.Put("k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	got, ok := c2.Get("k")
	if !ok || string(got) != "v" {
		t.Errorf("after reopen Get = (%q, %v), want (\"v\", true)", got, ok)
	}
}

// bbolt is single-writer: a second opener must fail fast on the Timeout rather
// than block forever. The pipeline relies on this to degrade to cache-less when
// the MCP server holds the lock.
func TestOpenIsSingleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c1.Close()

	if _, err := Open(path); err == nil {
		t.Error("a second Open on a held db succeeded; expected a lock timeout")
	}
}

func TestKeyIsDeterministic(t *testing.T) {
	a := Key("summarize", "some input", "model-x")
	b := Key("summarize", "some input", "model-x")
	if a != b {
		t.Errorf("Key is not deterministic: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("Key = %q (len %d), want a 64-char sha256 hex digest", a, len(a))
	}
}

func TestKeyDistinguishesInputs(t *testing.T) {
	base := Key("summarize", "input", "model-x")
	others := map[string]string{
		"different task":  Key("classify", "input", "model-x"),
		"different input": Key("summarize", "other", "model-x"),
		"different model": Key("summarize", "input", "model-y"),
		"extra part":      Key("summarize", "input", "model-x", "grammar"),
		"fewer parts":     Key("summarize", "input"),
	}
	for name, k := range others {
		if k == base {
			t.Errorf("%s produced the same key as the base request (%s)", name, k)
		}
	}
}

// Documents a real collision: Key joins parts with \x00, so a part that itself
// contains \x00 can forge the boundary between two parts. Model input is
// arbitrary text, so this is reachable in principle — a hit here would serve
// one request's result to a different request. Pinned as the current behaviour;
// fixing it (length-prefixing the parts) changes every existing key and so
// belongs in its own PR.
func TestKeySeparatorCollision(t *testing.T) {
	twoParts := Key("a", "b")
	onePart := Key("a\x00b")
	if twoParts != onePart {
		t.Skip("Key no longer collides on the \\x00 separator — the encoding was fixed; " +
			"update this test to assert the parts are unambiguous")
	}
	t.Logf("known collision: Key(%q, %q) == Key(%q) == %s", "a", "b", "a\x00b", twoParts)
}
