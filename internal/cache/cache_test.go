package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
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

// dirNames lists a directory by base name so a test can assert that NOTHING was
// created, rather than only that one expected file was absent.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
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
// than block forever. The pipeline relies on this to degrade to a sibling when
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

// Key must be injective over part LISTS: two different lists never share a key,
// whatever bytes the parts hold. The first encoding joined the parts with \x00, so
// a part that itself held \x00 could forge the boundary between two parts
// (Key("a", "b") == Key("a\x00b")). Model input is arbitrary text, so that was
// reachable: a hit would serve one request's cached result to a different
// request. The parts are length-prefixed now; this test fails against the old join
// and is the proof that the boundary cannot be forged.
func TestKeySeparatorCollision(t *testing.T) {
	// forgedHeader is one part that spells out, byte for byte, what the encoding
	// writes between "x" and "y" (the 8-byte big-endian length of "y"). It must
	// still not reproduce Key("x", "y").
	forgedHeader := "x" + string([]byte{0, 0, 0, 0, 0, 0, 0, 1}) + "y"
	// forgedFrame is one part that carries, byte for byte, the frames of the
	// two-part list a,b (length 1, "a", length 1, "b"). Its own length prefix has to
	// cover the forged bytes.
	forgedFrame := "\x00\x00\x00\x00\x00\x00\x00\x01a" + "\x00\x00\x00\x00\x00\x00\x00\x01b"

	lists := []struct {
		name  string
		parts []string
	}{
		{"no parts", nil},
		{"one empty part", []string{""}},
		{"two empty parts", []string{"", ""}},
		{"three empty parts", []string{"", "", ""}},
		{"a", []string{"a"}},
		{"a then empty", []string{"a", ""}},
		{"empty then a", []string{"", "a"}},
		{"a, b", []string{"a", "b"}},
		{"b, a", []string{"b", "a"}},
		{"ab, c", []string{"ab", "c"}},
		{"a, bc", []string{"a", "bc"}},
		{"abc as one part", []string{"abc"}},
		{"a NUL b as one part", []string{"a\x00b"}},
		{"a NUL b, c", []string{"a\x00b", "c"}},
		{"a, b NUL c", []string{"a", "b\x00c"}},
		{"a, b, c", []string{"a", "b", "c"}},
		{"a NUL b NUL c as one part", []string{"a\x00b\x00c"}},
		{"a NUL, b", []string{"a\x00", "b"}},
		{"a, NUL b", []string{"a", "\x00b"}},
		{"NUL as one part", []string{"\x00"}},
		{"two NULs as one part", []string{"\x00\x00"}},
		{"x, y", []string{"x", "y"}},
		{"forged length header", []string{forgedHeader}},
		{"forged frame of a, b", []string{forgedFrame}},
		// A decimal length with no delimiter ("<len><part>") reads these two lists as the
		// same fifteen digits: the lengths 1 and 11 and the digits of the parts run into
		// each other. It needs a digit part of eleven characters, which is why the
		// exhaustive short-list test below cannot reach it.
		{"a digit, then eleven digits", []string{"1", "11111111111"}},
		{"eleven digits, then a digit", []string{"11111111111", "1"}},
	}

	seen := make(map[string]string, len(lists))
	for _, l := range lists {
		k := Key(l.parts...)
		if len(k) != 64 {
			t.Fatalf("%s: key %q is not a 64-char sha256 hex digest", l.name, k)
		}
		if k2 := Key(l.parts...); k != k2 {
			t.Errorf("Key(%q) is not deterministic: %s != %s", l.parts, k, k2)
		}
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s share the key %s: Key(%q) collides with another part list", l.name, other, k, l.parts)
		}
		seen[k] = l.name
	}

	// The named cases from the defect, spelled out so a failure reads as the
	// requirement and not as a table row.
	if Key("a", "b") == Key("a\x00b") {
		t.Error(`Key("a", "b") == Key("a\x00b"): the \x00 boundary can be forged`)
	}
	if Key("a\x00b", "c") == Key("a", "b\x00c") {
		t.Error(`Key("a\x00b", "c") == Key("a", "b\x00c"): a NUL shifted across the boundary collides`)
	}
	if Key("a", "b") == Key(forgedFrame) {
		t.Error("a part carrying a forged length frame collides with the parts it imitates")
	}
	for _, c := range [][2][]string{
		{nil, {""}},
		{{""}, {"", ""}},
		{{"", "a"}, {"a", ""}},
		{{"a", ""}, {"a"}},
	} {
		if Key(c[0]...) == Key(c[1]...) {
			t.Errorf("Key(%q) == Key(%q): the empty-part cases must stay distinct", c[0], c[1])
		}
	}

	// Guard on the guard: these lists must have collided under the old join, or
	// the table above would not be exercising the defect it claims to.
	collidedBefore := [][2][]string{
		{{"a", "b"}, {"a\x00b"}},
		{{"a\x00b", "c"}, {"a", "b\x00c"}},
		{{"", ""}, {"\x00"}},
		{nil, {""}},
	}
	for _, c := range collidedBefore {
		if strings.Join(c[0], "\x00") != strings.Join(c[1], "\x00") {
			t.Errorf("%q and %q did not collide under the old \\x00 join; pick a pair that did", c[0], c[1])
		}
	}
}

// TestKeyEncodingIsPinned holds the encoding itself to account. Key's doc says a
// change to it is a deliberate, visible edit because it re-keys the whole cache;
// that is only true while a test fails when the encoding moves. The expected
// values were computed outside this package, straight from the written encoding
// (SHA-256 over "offload/cache.Key/v2", then each part as an 8-byte big-endian
// length followed by its bytes), so they pin the version tag and the width of the
// length as well as the framing. If this test goes red because the encoding was
// changed on purpose, bump the version tag and say so in the changelog: every
// stored key is unreachable afterwards.
func TestKeyEncodingIsPinned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"no parts", nil, "66a737870e2abb663d8ea2744a5bbdca16227f69aea9545a264bbdb28a510471"},
		{"one empty part", []string{""}, "e67eae136df6a6c7c7b2a9be3c56fd1ae76560d6d05d5940b2a39745f5e42f60"},
		{"a, b", []string{"a", "b"}, "b0ac631e407a1db3a4ec24f2680d3acb5b063147079a35ebba4de4fdb049a1b3"},
		{"three parts", []string{"summarize", "hello", "model-x"}, "17477d719a571c8868cbfc4326110780724e6a8192118733daf1056ebc622b9a"},
	} {
		if got := Key(tc.parts...); got != tc.want {
			t.Errorf("%s: Key(%q) = %s, want %s", tc.name, tc.parts, got, tc.want)
		}
	}
}

// TestKeyIsInjectiveOverSmallLists is the exhaustive form of the named cases above,
// for SHORT parts: every part list of up to three parts, each part a string of up to two
// characters from {"a", NUL, \x01}. No two different lists may share a key, whichever
// pair they are, so an encoding that only looks careful (a prefix on all but one part, a
// different separator) fails here on a list nobody thought to write down. About 2,400
// lists. What it cannot reach is an encoding whose collisions need long parts: a decimal
// length with no delimiter collides only on digit parts of eleven characters, so that
// case is named in TestKeySeparatorCollision instead.
func TestKeyIsInjectiveOverSmallLists(t *testing.T) {
	alphabet := []string{"a", "\x00", "\x01"}
	strs := []string{""}
	for _, c := range alphabet {
		strs = append(strs, c)
		for _, d := range alphabet {
			strs = append(strs, c+d)
		}
	}

	seen := map[string][]string{}
	check := func(parts []string) {
		k := Key(parts...)
		if prev, dup := seen[k]; dup {
			t.Fatalf("Key(%q) and Key(%q) are both %s", prev, parts, k)
		}
		seen[k] = append([]string(nil), parts...)
	}
	check(nil)
	for _, a := range strs {
		check([]string{a})
		for _, b := range strs {
			check([]string{a, b})
			for _, c := range strs {
				check([]string{a, b, c})
			}
		}
	}
	if want := 1 + 13 + 13*13 + 13*13*13; len(seen) != want {
		t.Fatalf("checked %d lists, want %d: the enumeration is not what the doc says", len(seen), want)
	}
}

// TestEmptyKeyNamesNoEntry: Key never returns an empty string, so the pipeline uses "" for
// "this call has no key and must not be cached". That has to hold in this package, not
// depend on what the store underneath does with an empty key.
func TestEmptyKeyNamesNoEntry(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("", []byte("v")); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("Put(\"\") = %v, want ErrEmptyKey", err)
	}
	if v, ok := c.Get(""); ok || v != nil {
		t.Errorf("Get(\"\") = %q, %v, want a miss", v, ok)
	}
	if n, err := c.Count(); err != nil || n != 0 {
		t.Errorf("Count() = %d, %v after a refused Put, want 0", n, err)
	}
	// Not vacuous: a real key stores and reads back.
	k := Key("x")
	if err := c.Put(k, []byte("v")); err != nil {
		t.Fatalf("Put(%q): %v", k, err)
	}
	if v, ok := c.Get(k); !ok || string(v) != "v" {
		t.Errorf("Get(%q) = %q, %v, want the stored value", k, v, ok)
	}
}

// ---------------------------------------------------------------------------
// D-05: lazy open
// ---------------------------------------------------------------------------

// TestNewOpensNothingUntilFirstUse is the D-05 fix itself. The eager open ran in
// openPipeline — i.e. in EVERY command and every MCP server start — and so took
// or lost the bbolt lock before anything knew whether a cacheable task would
// ever run. That is what left 49 empty 32 KB siblings on the workstation.
func TestNewOpensNothingUntilFirstUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")

	c := New(path)
	defer c.Close()
	if got := c.Mode(); got != ModeUnopened {
		t.Errorf("Mode after New = %q, want %q", got, ModeUnopened)
	}
	if got := c.Path(); got != "" {
		t.Errorf("Path after New = %q, want empty: nothing is open yet", got)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("New created %v; a handle nobody has used must create no file at all", names)
	}

	// Mode must not be the thing that opens it: a status reader asking what the
	// cache is doing must not become the writer that takes the lock.
	_ = c.Mode()
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("Mode() resolved the handle and created %v", names)
	}

	if _, ok := c.Get("k"); ok {
		t.Error("Get hit on an empty store")
	}
	if got := c.Mode(); got != ModePrimary {
		t.Errorf("Mode after first use = %q, want %q", got, ModePrimary)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("first use must open the primary: %v", err)
	}
}

// TestLazyOpenFallsToSiblingWithoutBlocking: a held primary must cost
// milliseconds, not a second. The pre-0.121.1 bolt.Options.Timeout was 1s and
// every losing process paid it at startup.
func TestLazyOpenFallsToSiblingWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	c := New(path)
	defer c.Close()
	start := time.Now()
	if err := c.Put("k", []byte("v")); err != nil {
		t.Fatalf("a held primary must still yield a working sibling: %v", err)
	}
	elapsed := time.Since(start)

	want := siblingPath(path, os.Getpid())
	if c.Mode() != ModeSibling || c.Path() != want || !c.Fallback() {
		t.Fatalf("mode=%q path=%q fallback=%v; want the sibling %q", c.Mode(), c.Path(), c.Fallback(), want)
	}
	// ABSOLUTE, not a multiple of LockTimeout: a revert of the constant to the
	// old 1s must fail here rather than move the bar with itself.
	if budget := 700 * time.Millisecond; elapsed > budget {
		t.Errorf("fallback took %v, want under %v — the bbolt lock Timeout is no longer short", elapsed, budget)
	}
	if v, ok := c.Get("k"); !ok || string(v) != "v" {
		t.Fatalf("sibling cache must round-trip, got %q %v", v, ok)
	}
}

// TestLazyOpenReportsANonLockFailureAsItself: a bad path is not lock contention
// and must never be absorbed by the sibling fallback, which would send the
// operator to diagnose the wrong thing for the rest of the run.
func TestLazyOpenReportsANonLockFailureAsItself(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "cache.db")
	c := New(bad)
	defer c.Close()
	if _, ok := c.Get("k"); ok {
		t.Error("Get hit against an unopenable cache")
	}
	if c.Mode() != ModeUnavailable {
		t.Errorf("Mode = %q, want %q", c.Mode(), ModeUnavailable)
	}
	err := c.OpenErr()
	if err == nil {
		t.Fatal("OpenErr = nil; an unavailable cache must name why")
	}
	if errors.Is(err, bolt.ErrTimeout) {
		t.Errorf("a missing directory was reported as lock contention: %v", err)
	}
	if n, _ := SiblingStats(bad); n != 0 {
		t.Errorf("a non-lock failure created %d sibling(s); only contention may fall back", n)
	}
}

// TestNotifyFiresOnceOnANonPrimaryResolve: the binaries print the operator note
// from this callback, and they must print it when the fallback becomes TRUE
// rather than at startup when it was merely predicted.
func TestNotifyFiresOnceOnANonPrimaryResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	var modes []Mode
	c := New(path).WithNotify(func(m Mode, _ string, _ error) { modes = append(modes, m) })
	defer c.Close()
	if len(modes) != 0 {
		t.Fatalf("notify fired at construction (%v); nothing is resolved yet", modes)
	}
	c.Get("a")
	c.Get("b")
	c.Put("c", []byte("v"))
	if len(modes) != 1 || modes[0] != ModeSibling {
		t.Fatalf("notify calls = %v, want exactly one %q", modes, ModeSibling)
	}
}

// TestNotifyStaysSilentOnThePrimary: the note means "you lost the shared cache".
// Printing it on the happy path would train the operator to ignore it.
func TestNotifyStaysSilentOnThePrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	fired := 0
	c := New(path).WithNotify(func(Mode, string, error) { fired++ })
	defer c.Close()
	c.Get("k")
	if c.Mode() != ModePrimary || fired != 0 {
		t.Fatalf("mode=%q notify fired %d time(s); want the primary, silently", c.Mode(), fired)
	}
}

// ---------------------------------------------------------------------------
// D-05: the read-only / read-through path
// ---------------------------------------------------------------------------

// TestReadOnlyOpensShareWithEachOtherButNotWithAWriter pins the bbolt locking
// semantics the whole read-through design rests on, so that a future bbolt bump
// that changes them fails here instead of silently making the design wrong.
//
// bbolt db.go, at the flock call: "The database file is locked exclusively (only
// one process can grab the lock) if !options.ReadOnly. The database file is
// locked using the shared lock (more than one process may hold a lock at the
// same time) otherwise." On Windows bolt_windows.go implements the same thing
// with LockFileEx, adding LOCKFILE_EXCLUSIVE_LOCK only for the exclusive case —
// so a shared request there still conflicts with a held exclusive one.
func TestReadOnlyOpensShareWithEachOtherButNotWithAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	r1, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("first read-only open: %v", err)
	}
	r2, err := openReadOnly(path)
	if err != nil {
		r1.Close()
		t.Fatalf("a SECOND read-only open must share the lock with the first: %v", err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	blocked, err := openReadOnly(path)
	if err == nil {
		blocked.Close()
		t.Fatal("a read-only open succeeded while a writer held the exclusive lock — " +
			"bbolt's semantics changed; the read-through fallback in resolveReaderLocked " +
			"is no longer needed and its comment is now wrong")
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		t.Errorf("read-only open under a writer failed with %v, want a lock timeout", err)
	}
}

// TestReaderNeverCreatesAFile is item (2) of D-05: a surface that only looks
// things up must not be the thing that litters the cache directory. The
// contended case is the one that matters — that is exactly when the old code
// created a sibling.
func TestReaderNeverCreatesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	before := dirNames(t, dir)

	r := NewReader(path)
	defer r.Close()
	if _, ok := r.Get("anything"); ok {
		t.Error("reader hit against a store it cannot even open")
	}
	if r.Mode() != ModeUnavailable {
		t.Errorf("Mode = %q; with the primary held and no sibling of our own, a reader has nothing to read", r.Mode())
	}
	if err := r.Put("k", []byte("v")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on a reader = %v, want ErrReadOnly", err)
	}
	if n, b := SiblingStats(path); n != 0 || b != 0 {
		t.Errorf("the read path created %d sibling(s) (%d bytes); it must never write", n, b)
	}
	if after := dirNames(t, dir); len(after) != len(before) {
		t.Errorf("directory went from %v to %v; a pure read created a file", before, after)
	}
}

// TestReaderReadsTheFreePrimary: when nothing holds the primary the reader is a
// real cache reader, not a permanent miss.
func TestReaderReadsTheFreePrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewReader(path)
	defer r.Close()
	got, ok := r.Get("k")
	if !ok || string(got) != "v" {
		t.Fatalf("reader Get = (%q, %v), want (\"v\", true)", got, ok)
	}
	if r.Mode() != ModeReadOnly || r.Path() != path {
		t.Errorf("mode=%q path=%q, want %q on the primary", r.Mode(), r.Path(), ModeReadOnly)
	}
}

// TestReaderReadsThroughToItsOwnSibling is the second half of the read-through:
// the primary is held by another writer, but this pid's own sibling is on disk
// and unheld, so a reader reaches it instead of reporting a blanket miss.
//
// "Unheld" is load-bearing and was measured, not assumed: an exclusive bbolt
// lock conflicts with a shared request from the SAME process too (LockFileEx and
// flock both scope to the handle, not the pid), so while this process's own
// writer handle is open its sibling is no more readable than the primary. The
// reachable case is the one built here — the writer handle is gone, the file is
// not. A reader that lands nowhere simply misses; see TestReaderNeverCreatesAFile.
func TestReaderReadsThroughToItsOwnSibling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	// Build this pid's sibling directly and release it, so the file survives
	// without a live exclusive hold on it.
	sib := siblingPath(path, os.Getpid())
	sdb, err := openWritable(sib, LockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewReader(path)
	defer r.Close()
	got, ok := r.Get("k")
	if !ok || string(got) != "v" {
		t.Fatalf("read-through Get = (%q, %v), want (\"v\", true) from this process's sibling", got, ok)
	}
	if r.Mode() != ModeReadOnly || r.Path() != sib {
		t.Errorf("mode=%q path=%q, want %q on %q", r.Mode(), r.Path(), ModeReadOnly, sib)
	}
	// Reading it must not have changed the census.
	if n, _ := SiblingStats(path); n != 1 {
		t.Errorf("SiblingStats = %d after a read-through, want the 1 file that was already there", n)
	}
}

// ---------------------------------------------------------------------------
// D-05: sibling cleanup on Close
// ---------------------------------------------------------------------------

// TestEmptySiblingIsRemovedOnClose is the 40-odd 32 KB files this PR is about:
// every one of them was a sibling that never took a single entry.
func TestEmptySiblingIsRemovedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	c := New(path)
	if _, ok := c.Get("nothing"); ok {
		t.Fatal("hit on an empty store")
	}
	sib := siblingPath(path, os.Getpid())
	if c.Mode() != ModeSibling {
		t.Fatalf("setup: mode=%q, want a sibling", c.Mode())
	}
	if _, err := os.Stat(sib); err != nil {
		t.Fatalf("the sibling must exist while the handle is open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sib); !os.IsNotExist(err) {
		t.Fatalf("an EMPTY sibling must be deleted on Close, stat err=%v", err)
	}
	if n, _ := SiblingStats(path); n != 0 {
		t.Errorf("SiblingStats still counts %d sibling(s) after Close", n)
	}
}

// TestNonEmptySiblingIsPromotedWhenThePrimaryIsFree: a session's hits must not
// be stranded in a file nobody else will ever read.
func TestNonEmptySiblingIsPromotedWhenThePrimaryIsFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := New(path)
	if err := c.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if c.Mode() != ModeSibling {
		t.Fatalf("setup: mode=%q, want a sibling", c.Mode())
	}
	sib := c.Path()
	// The other process exits BEFORE this one: the promotion window is open.
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sib); !os.IsNotExist(err) {
		t.Errorf("a fully promoted sibling must be removed, stat err=%v", err)
	}

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if v, ok := p.Get("k"); !ok || string(v) != "v" {
		t.Fatalf("the sibling's entry must land in the primary, got (%q, %v)", v, ok)
	}
}

// TestPromotionIsSkippedWhileThePrimaryIsHeld: ONE attempt, no waiting. Process
// exit must never block on another process's lock, and the sibling must survive
// so a later run (or the sweep) can deal with it.
func TestPromotionIsSkippedWhileThePrimaryIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	c := New(path)
	if err := c.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	sib := c.Path()
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close waited %v on a held primary; the promotion attempt must be bounded", elapsed)
	}
	if _, err := os.Stat(sib); err != nil {
		t.Errorf("a sibling that could not be promoted must be LEFT IN PLACE, stat err=%v", err)
	}
	if n, err := holder.Count(); err != nil || n != 0 {
		t.Errorf("primary Count = (%d, %v); nothing may be written to a db this process does not hold", n, err)
	}
}

// TestPromotionIsBoundedByPromotionMaxKeys: exactly the bound crosses, and an
// over-budget sibling is KEPT because it still holds the surplus. Deleting it
// would silently discard cached work.
func TestPromotionIsBoundedByPromotionMaxKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := New(path)
	if err := c.Put("seed", []byte("v")); err != nil {
		t.Fatal(err)
	}
	// One transaction: PromotionMaxKeys+ individual Puts would be that many
	// fsyncs and would make this test the slowest in the package.
	extra := PromotionMaxKeys + 49 // + the seed = PromotionMaxKeys + 50
	if err := c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		for i := 0; i < extra; i++ {
			if err := b.Put([]byte("k"+strconv.Itoa(i)), []byte("v")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sib := c.Path()
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sib); err != nil {
		t.Errorf("an over-budget sibling must be kept (it still holds the surplus), stat err=%v", err)
	}

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	n, err := p.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != PromotionMaxKeys {
		t.Errorf("promotion moved %d keys into the primary, want exactly the PromotionMaxKeys bound %d", n, PromotionMaxKeys)
	}
}

// TestCloseOfAnUnusedHandleIsANoOp: the common case after D-05 — a process that
// never needed the cache must not create anything on the way out either.
func TestCloseOfAnUnusedHandleIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	c := New(path)
	if err := c.Close(); err != nil {
		t.Fatalf("Close of an unopened handle: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("closing an unused handle created %v", names)
	}
}

// ---------------------------------------------------------------------------
// D-05: the sweep and the sibling census
// ---------------------------------------------------------------------------

// TestSweepUsesTheOneHourWindowAndRunsAtConstruction pins BOTH halves of item
// (4). The window matters on its own: at 12 h the 40-odd siblings this PR found
// had a whole working day to accumulate.
func TestSweepUsesTheOneHourWindowAndRunsAtConstruction(t *testing.T) {
	if SiblingMaxAge != time.Hour {
		t.Fatalf("SiblingMaxAge = %v, want 1h (D-05 item 4)", SiblingMaxAge)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	older := siblingPath(path, 4242)   // 90 min: stale under a 1 h window, fresh under the old 12 h one
	younger := siblingPath(path, 4343) // 30 min: another live session
	for f, age := range map[string]time.Duration{older: 90 * time.Minute, younger: 30 * time.Minute} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(f, at, at); err != nil {
			t.Fatal(err)
		}
	}

	c := New(path) // the sweep runs HERE, not at resolve: most handles never resolve
	defer c.Close()

	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Errorf("a 90-minute-old sibling must be swept under the 1h window, stat err=%v", err)
	}
	if _, err := os.Stat(younger); err != nil {
		t.Errorf("a 30-minute-old sibling belongs to a live session and must be kept: %v", err)
	}
	if c.Mode() != ModeUnopened {
		t.Errorf("the sweep resolved the handle (mode=%q); it must take no lock", c.Mode())
	}
}

// TestReaderSweepsToo: `status` is the surface an operator runs when the cache
// directory looks wrong, so it must clean up as well as report — and still
// create nothing.
func TestReaderSweepsToo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	stale := siblingPath(path, 4242)
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-SiblingMaxAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	r := NewReader(path)
	defer r.Close()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a reader must sweep stale siblings too, stat err=%v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("the reader left %v behind; it must create nothing", names)
	}
}

// TestSweepNeverTouchesFilesItDidNotName is the review finding: an operator's
// backup beside the cache ("cache.prev.db", "cache.patched.db") shares the
// "cache.p*" prefix and must survive a sweep however old it is.
func TestSweepNeverTouchesFilesItDidNotName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	keep := []string{filepath.Join(dir, "cache.prev.db"), filepath.Join(dir, "cache.patched.db"), filepath.Join(dir, "cache.p12x.db"), filepath.Join(dir, "cache.p.db")}
	stale := siblingPath(path, 777)
	old := time.Now().Add(-SiblingMaxAge - time.Hour)
	for _, f := range append(keep, stale) {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepStaleSiblings(path)
	for _, f := range keep {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed by the sweep; only <stem>.p<pid><ext> is ours", filepath.Base(f))
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the real stale sibling must be removed, stat err=%v", err)
	}
}

// TestSiblingStatsCountsOnlyOurOwnShape: the number offload_status publishes is
// the D-05 regression detector, so an operator's backup must not inflate it.
func TestSiblingStatsCountsOnlyOurOwnShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	ours := []string{siblingPath(path, 11), siblingPath(path, 22)}
	theirs := []string{filepath.Join(dir, "cache.prev.db"), filepath.Join(dir, "cache.p12x.db")}
	for _, f := range append(append([]string{}, ours...), theirs...) {
		if err := os.WriteFile(f, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, b := SiblingStats(path)
	if n != len(ours) {
		t.Errorf("SiblingStats count = %d, want %d — only <stem>.p<pid><ext> counts", n, len(ours))
	}
	if want := int64(100 * len(ours)); b != want {
		t.Errorf("SiblingStats bytes = %d, want %d", b, want)
	}
	if n, b := SiblingStats(""); n != 0 || b != 0 {
		t.Errorf("SiblingStats(\"\") = (%d, %d), want (0, 0)", n, b)
	}
}
