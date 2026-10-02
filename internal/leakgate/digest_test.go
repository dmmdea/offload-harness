package leakgate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func hmacHex(key []byte, msg string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func buildSynth(t *testing.T, pair string, allow []AllowRow, exempt []ExemptRow) (*DigestFile, []byte) {
	t.Helper()
	key := keyBytes(pair)
	df, err := BuildDigest(key, synthEntries(t), allow, exempt)
	if err != nil {
		t.Fatal(err)
	}
	return df, key
}

// TestEntryDigestIsTheSpecifiedHMAC re-derives the digest of every mode from
// the specification: d = HMAC-SHA256(key, "leakgate/2/" + code + NUL + text),
// f = HMAC-SHA256(key, "leakgate/2/f" + NUL + first word).
func TestEntryDigestIsTheSpecifiedHMAC(t *testing.T) {
	key := keyBytes("ab")
	cases := []struct {
		e    Entry
		code string
		len  int
		n    int
	}{
		{Entry{ModeSub, "zorblax"}, "s", 7, 0},
		{Entry{ModeWord, "quuxel"}, "w", 6, 0},
		{Entry{ModeExact, "8128"}, "x", 4, 0},
		{Entry{ModeSubCS, "froodToken"}, "c", 10, 0},
		{Entry{ModePhrase, "plugh xyzzy"}, "p", 0, 2},
		{Entry{ModePhrase, "k frotz plover"}, "p", 0, 3},
	}
	for _, c := range cases {
		d := EntryDigest(key, c.e)
		if want := hmacHex(key, "leakgate/2/"+c.code+"\x00"+c.e.Text); d.D != want {
			t.Errorf("%s %q: d = %s, want %s", c.e.Mode, c.e.Text, d.D, want)
		}
		if d.Mode != string(c.e.Mode) || d.Len != c.len || d.N != c.n {
			t.Errorf("%s %q: %+v", c.e.Mode, c.e.Text, d)
		}
		if c.e.Mode == ModePhrase {
			first := strings.Fields(c.e.Text)[0]
			if want := hmacHex(key, "leakgate/2/f\x00"+first); d.F != want {
				t.Errorf("phrase %q: f = %s, want %s", c.e.Text, d.F, want)
			}
		} else if d.F != "" {
			t.Errorf("%s carries a prefilter: %+v", c.e.Mode, d)
		}
	}
	// The same word in two modes gives different digests, and the case of a
	// case-sensitive entry is part of the digest.
	a := EntryDigest(key, Entry{ModeSub, "zorblax"}).D
	b := EntryDigest(key, Entry{ModeWord, "zorblax"}).D
	c := EntryDigest(key, Entry{ModeSubCS, "zorblax"}).D
	d := EntryDigest(key, Entry{ModeSubCS, "Zorblax"}).D
	if a == b || a == c || b == c || c == d {
		t.Errorf("digests collide across modes or case: %s %s %s %s", a, b, c, d)
	}
	if EntryDigest(keyBytes("cd"), Entry{ModeSub, "zorblax"}).D == a {
		t.Error("two keys give the same digest")
	}
}

func TestBuildDigestShape(t *testing.T) {
	df, key := buildSynth(t, "ab", nil, nil)
	if df.Version != DigestVersion || df.Algo != DigestAlgo {
		t.Errorf("version/algo = %d %q", df.Version, df.Algo)
	}
	if len(df.Entries) != len(synthEntries(t))+1 {
		t.Errorf("entries = %d, want the list plus the canary", len(df.Entries))
	}
	if !sort.SliceIsSorted(df.Entries, func(i, j int) bool { return df.Entries[i].D < df.Entries[j].D }) {
		t.Error("entries are not sorted by digest")
	}
	seen := map[string]bool{}
	for _, e := range df.Entries {
		if seen[e.D] {
			t.Errorf("duplicate digest %s", e.D)
		}
		seen[e.D] = true
		if len(e.D) != 64 {
			t.Errorf("digest length %d", len(e.D))
		}
	}
	canary := EntryDigest(key, Entry{ModeSub, CanaryText()}).D
	if !seen[canary] {
		t.Error("the canary is not among the entries")
	}
	if df.Allow == nil || df.Exempt == nil {
		t.Error("allow and exempt must be empty lists, not null")
	}
	if len(df.MAC) != 64 || !df.VerifyMAC(key) {
		t.Errorf("mac %q does not verify under the key it was built with", df.MAC)
	}
	if err := df.Validate(); err != nil {
		t.Errorf("a freshly built digest file must validate: %v", err)
	}
}

func TestBuildDigestIsDeterministicAndOrderIndependent(t *testing.T) {
	key := keyBytes("ab")
	es := synthEntries(t)
	a, err := BuildDigest(key, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rev := append([]Entry{}, es...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	b, err := BuildDigest(key, rev, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ma, _ := a.Marshal()
	mb, _ := b.Marshal()
	if !bytes.Equal(ma, mb) {
		t.Error("the digest file depends on the order of the list")
	}
}

func TestBuildDigestDoesNotMutateItsInputs(t *testing.T) {
	es := synthEntries(t)
	before := append([]Entry{}, es...)
	if _, err := BuildDigest(keyBytes("ab"), es, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(es, before) {
		t.Error("BuildDigest changed the caller's list")
	}
}

// TestMACCoversEverything: any edit to an entry, an allow row or an exempt row
// without the key fails verification.
func TestMACCoversEverything(t *testing.T) {
	allow := []AllowRow{{Where: "body", Path: "a.md", D: hmacHex(keyBytes("ab"), "chunk")}}
	exempt := []ExemptRow{{Path: "f.woff2", Blob: blobA, Why: "binary"}}
	base, key := buildSynth(t, "ab", allow, exempt)
	if !base.VerifyMAC(key) {
		t.Fatal("base file does not verify")
	}
	if base.VerifyMAC(keyBytes("cd")) {
		t.Error("the mac verified under the wrong key")
	}
	clone := func() *DigestFile {
		b, err := base.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		c, err := ParseDigest(b)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	edits := []struct {
		name string
		do   func(*DigestFile)
	}{
		{"an entry digest edited", func(d *DigestFile) { d.Entries[0].D = strings.Repeat("0", 64) }},
		{"an entry removed", func(d *DigestFile) { d.Entries = d.Entries[1:] }},
		{"an entry added", func(d *DigestFile) {
			d.Entries = append(d.Entries, DigestEntry{D: strings.Repeat("f", 64), Mode: "sub", Len: 5})
		}},
		{"an entry length edited", func(d *DigestFile) { d.Entries[0].Len += 1 }},
		{"an entry mode edited", func(d *DigestFile) { d.Entries[0].Mode = "word" }},
		{"a phrase prefilter edited", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "phrase" {
					d.Entries[i].F = strings.Repeat("0", 64)
					return
				}
			}
		}},
		{"an allow row edited", func(d *DigestFile) { d.Allow[0].Path = "b.md" }},
		{"an allow row added", func(d *DigestFile) {
			d.Allow = append(d.Allow, AllowRow{Where: "name", Path: "x", D: strings.Repeat("1", 64)})
		}},
		{"an allow row removed", func(d *DigestFile) { d.Allow = nil }},
		{"an exempt row edited", func(d *DigestFile) { d.Exempt[0].Blob = blobB }},
		{"an exempt row added", func(d *DigestFile) { d.Exempt = append(d.Exempt, ExemptRow{Path: "g", Blob: blobA, Why: "binary"}) }},
		{"an exempt row removed", func(d *DigestFile) { d.Exempt = nil }},
		{"an exempt reason edited", func(d *DigestFile) { d.Exempt[0].Why = "oversize" }},
	}
	for _, e := range edits {
		d := clone()
		e.do(d)
		if d.VerifyMAC(key) {
			t.Errorf("%s: the mac still verifies", e.name)
		}
	}
	// Field boundaries are part of the message: moving a character from one
	// field to the next must change the mac.
	a := &DigestFile{Version: 2, Algo: DigestAlgo, Entries: nil, Allow: []AllowRow{{Where: "body", Path: "ab", D: strings.Repeat("0", 64)}}, Exempt: []ExemptRow{}}
	b := &DigestFile{Version: 2, Algo: DigestAlgo, Entries: nil, Allow: []AllowRow{{Where: "bodya", Path: "b", D: strings.Repeat("0", 64)}}, Exempt: []ExemptRow{}}
	if a.ComputeMAC(key) == b.ComputeMAC(key) {
		t.Error("two different allow rows give the same mac")
	}
}

func TestSelectKeyRotation(t *testing.T) {
	oldKey, newKey := keyBytes("ab"), keyBytes("cd")
	df, err := BuildDigest(oldKey, synthEntries(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// built under the old key; KEY is the new one and KEY_PREV the old one.
	got, err := SelectKey(df, Keys{Key: newKey, Prev: oldKey})
	if err != nil || !bytes.Equal(got, oldKey) {
		t.Fatalf("rotation: key = %x, err = %v; want the previous key", got, err)
	}
	got, err = SelectKey(df, Keys{Key: oldKey, Prev: newKey})
	if err != nil || !bytes.Equal(got, oldKey) {
		t.Fatalf("normal run: key = %x, err = %v", got, err)
	}
	got, err = SelectKey(df, Keys{Key: oldKey})
	if err != nil || !bytes.Equal(got, oldKey) {
		t.Fatalf("no previous key: %x %v", got, err)
	}
	if _, err = SelectKey(df, Keys{Key: newKey}); !errors.Is(err, ErrMACMismatch) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = SelectKey(df, Keys{Key: newKey, Prev: keyBytes("ef")}); !errors.Is(err, ErrMACMismatch) {
		t.Fatalf("neither key: %v", err)
	}
	if _, err = SelectKey(df, Keys{Prev: oldKey}); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("a previous key alone never counts: %v", err)
	}
	if msg := ErrMACMismatch.Error(); msg != "digest file does not match this key" {
		t.Errorf("message = %q", msg)
	}
}

func TestMarshalParseRoundTrip(t *testing.T) {
	exempt := []ExemptRow{{Path: "fonts/a b.woff2", Blob: blobA, Why: "binary"}}
	allow := []AllowRow{{Where: "name", Path: "p.md", D: strings.Repeat("2", 64)}}
	df, key := buildSynth(t, "ab", allow, exempt)
	b, err := df.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte("}\n")) || bytes.Contains(b, []byte("\r")) {
		t.Errorf("marshalled file must be LF with one trailing newline: %q", b[len(b)-4:])
	}
	back, err := ParseDigest(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(df, back) || !back.VerifyMAC(key) {
		t.Errorf("round trip changed the file")
	}
	b2, _ := back.Marshal()
	if !bytes.Equal(b, b2) {
		t.Error("marshalling is not stable")
	}
	// A CRLF checkout of the same file parses identically.
	crlf := bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	back2, err := ParseDigest(crlf)
	if err != nil || !reflect.DeepEqual(df, back2) {
		t.Errorf("CRLF form: %v", err)
	}
	// Field names avoid the words the operator's secret scanners look for.
	low := strings.ToLower(string(b))
	for _, w := range []string{"key", "secret", "token", "password", "passwd", "credential"} {
		if strings.Contains(low, w) {
			t.Errorf("the digest file carries the word %q", w)
		}
	}
	// No name leaks: not one synthetic entry text is in the file.
	for _, e := range synthEntries(t) {
		if strings.Contains(low, strings.ToLower(strings.ReplaceAll(e.Text, " ", ""))) {
			t.Errorf("the digest file carries entry text %q", e.Text)
		}
	}
}

func TestParseDigestRejectsMalformedFiles(t *testing.T) {
	df, _ := buildSynth(t, "ab", nil, []ExemptRow{{Path: "f", Blob: blobA, Why: "binary"}})
	good, _ := df.Marshal()
	mutate := func(from, to string) []byte {
		if !bytes.Contains(good, []byte(from)) {
			t.Fatalf("fixture does not contain %q", from)
		}
		return bytes.Replace(good, []byte(from), []byte(to), 1)
	}
	first := df.Entries[0]
	cases := []struct {
		name string
		in   []byte
	}{
		{"not json", []byte("{")},
		{"empty", nil},
		{"unknown field", mutate(`"algo"`, `"extra": 1, "algo"`)},
		{"trailing data", append(append([]byte{}, good...), []byte("{}")...)},
		{"wrong version", mutate(`"version": 2`, `"version": 1`)},
		{"wrong algo", mutate(`"hmac-sha256"`, `"sha1"`)},
		{"mac not hex", mutate(`"mac": "`+df.MAC[:4], `"mac": "zzzz`)},
		{"mac too short", mutate(`"mac": "`+df.MAC, `"mac": "`+df.MAC[:60])},
		{"digest too short", mutate(first.D, first.D[:60])},
		{"unknown mode", mutate(`"mode": "`+first.Mode+`"`, `"mode": "fuzzy"`)},
		{"exempt blob too short", mutate(blobA, blobA[:39])},
		{"exempt reason unknown", mutate(`"binary"`, `"because"`)},
		{"no entries", []byte(`{"version": 2, "algo": "hmac-sha256", "mac": "` + df.MAC + `", "entries": [], "allow": [], "exempt": []}`)},
	}
	for _, c := range cases {
		if _, err := ParseDigest(c.in); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestValidateStructure(t *testing.T) {
	good, _ := buildSynth(t, "ab", nil, nil)
	clone := func() *DigestFile {
		b, _ := good.Marshal()
		d, err := ParseDigest(b)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	bad := []struct {
		name string
		do   func(*DigestFile)
	}{
		{"unsorted", func(d *DigestFile) { d.Entries[0], d.Entries[1] = d.Entries[1], d.Entries[0] }},
		{"duplicate", func(d *DigestFile) { d.Entries[1] = d.Entries[0] }},
		{"uppercase hex", func(d *DigestFile) { d.Entries[0].D = strings.ToUpper(d.Entries[0].D) }},
		{"window entry without a length", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "sub" {
					d.Entries[i].Len = 0
					return
				}
			}
		}},
		{"phrase without a prefilter", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "phrase" {
					d.Entries[i].F = ""
					return
				}
			}
		}},
		{"phrase of one word", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "phrase" {
					d.Entries[i].N = 1
					return
				}
			}
		}},
		{"phrase with a length", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "phrase" {
					d.Entries[i].Len = 4
					return
				}
			}
		}},
		{"window entry with a prefilter", func(d *DigestFile) {
			for i := range d.Entries {
				if d.Entries[i].Mode == "sub" {
					d.Entries[i].F = strings.Repeat("0", 64)
					return
				}
			}
		}},
		{"allow row without a place", func(d *DigestFile) { d.Allow = []AllowRow{{Where: "elsewhere", Path: "a", D: strings.Repeat("0", 64)}} }},
		{"allow row with a short digest", func(d *DigestFile) { d.Allow = []AllowRow{{Where: "body", Path: "a", D: "00"}} }},
		{"allow row without a path", func(d *DigestFile) { d.Allow = []AllowRow{{Where: "body", D: strings.Repeat("0", 64)}} }},
		{"exempt row without a path", func(d *DigestFile) { d.Exempt = []ExemptRow{{Blob: blobA, Why: "binary"}} }},
		{"duplicate exempt rows", func(d *DigestFile) {
			d.Exempt = []ExemptRow{{Path: "a", Blob: blobA, Why: "binary"}, {Path: "a", Blob: blobB, Why: "binary"}}
		}},
	}
	for _, c := range bad {
		d := clone()
		c.do(d)
		if err := d.Validate(); err == nil {
			t.Errorf("%s: validated", c.name)
		}
	}
	if err := clone().Validate(); err != nil {
		t.Errorf("an untouched file must validate: %v", err)
	}
}

func TestNewDigestMatcherRejectsABadFile(t *testing.T) {
	df, key := buildSynth(t, "ab", nil, nil)
	df.Entries[0].D = "xyz"
	if _, err := NewDigestMatcher(df, key); err == nil {
		t.Error("a digest matcher was built from an invalid file")
	}
	if _, err := NewDigestMatcher(&DigestFile{}, key); err == nil {
		t.Error("a digest matcher was built from an empty file")
	}
}

func TestGenValidationRefusesBadLists(t *testing.T) {
	key := keyBytes("ab")
	bad := []struct {
		name string
		es   []Entry
	}{
		{"sub too short", []Entry{{ModeSub, "abc"}}},
		{"word too short", []Entry{{ModeWord, "abc"}}},
		{"subcs too short", []Entry{{ModeSubCS, "abc"}}},
		{"exact too short", []Entry{{ModeExact, "ab"}}},
		{"phrase of one word", []Entry{{ModePhrase, "solo"}}},
		{"phrase of four words", []Entry{{ModePhrase, "a b c d"}}},
		{"hyphen", []Entry{{ModeSub, "ab-cd"}}},
		{"underscore", []Entry{{ModeSub, "ab_cd"}}},
		{"dot", []Entry{{ModeWord, "ab.cd"}}},
		{"non-ASCII", []Entry{{ModeSub, "caf" + string(rune(0x00E9))}}},
		{"space in a window mode", []Entry{{ModeSub, "ab cd"}}},
		{"empty text", []Entry{{ModeSub, ""}}},
		{"duplicate in a mode", []Entry{{ModeSub, "abcd"}, {ModeSub, "abcd"}}},
		{"duplicate after case folding", []Entry{{ModeWord, "abcd"}, {ModeWord, "ABCD"}}},
		{"canary supplied by the list", []Entry{{ModeSub, CanaryText()}}},
		{"unknown mode", []Entry{{Mode("fuzzy"), "abcd"}}},
		{"empty list", nil},
	}
	for _, c := range bad {
		if _, err := BuildDigest(key, c.es, nil, nil); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// The same word in two modes is fine.
	if _, err := BuildDigest(key, []Entry{{ModeSub, "abcd"}, {ModeWord, "abcd"}, {ModeSubCS, "abcd"}, {ModeExact, "abcd"}}, nil, nil); err != nil {
		t.Errorf("the same text in different modes: %v", err)
	}
	// A one-letter first word is allowed (the drive-letter rule).
	if _, err := BuildDigest(key, []Entry{{ModePhrase, "k frotz"}}, nil, nil); err != nil {
		t.Errorf("one-letter first word: %v", err)
	}
	// A bad key is refused.
	if _, err := BuildDigest([]byte("short"), synthEntries(t), nil, nil); err == nil {
		t.Error("a short key was accepted")
	}
}
