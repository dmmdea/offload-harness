package leakgate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The digest file is the committed form of the list: keyed hashes only.
//
//	{ "version": 2, "algo": "hmac-sha256", "mac": "<64 hex>",
//	  "entries": [ {"d": "<64 hex>", "mode": "sub",    "len": 5},
//	               {"d": "<64 hex>", "mode": "phrase", "n": 2, "f": "<64 hex>"}, ... ],
//	  "allow":   [ {"where": "body", "path": "...", "d": "<64 hex>"} ],
//	  "exempt":  [ {"path": "...", "blob": "<40 hex>", "why": "binary"} ] }
//
// d = HMAC-SHA256(key, "leakgate/2/" + code + NUL + normalized text), so the same
// word in two modes has two digests; a phrase also carries the prefilter
// f = HMAC-SHA256(key, "leakgate/2/f" + NUL + first word) so the scanner hashes
// a phrase window only after its first word matched. mac proves that the key and
// the file belong together and makes the list tamper-evident: it covers every
// entry, allow row and exempt row.

// Version and algorithm of the digest file format.
const (
	DigestVersion = 2
	DigestAlgo    = "hmac-sha256"
)

// DigestEntry is one hashed entry. Len is the entry length (window modes), N
// the word count and F the first-word prefilter (phrases).
type DigestEntry struct {
	D    string `json:"d"`
	Mode string `json:"mode"`
	Len  int    `json:"len,omitempty"`
	N    int    `json:"n,omitempty"`
	F    string `json:"f,omitempty"`
}

// AllowRow allows one finding: Where is "name" or "body", D the keyed digest
// (AllowDigest) of the lower-case chunk of [A-Za-z0-9_.-] around the finding.
// No row ships today.
type AllowRow struct {
	Where string `json:"where"`
	Path  string `json:"path"`
	D     string `json:"d"`
}

// ExemptRow exempts one file from one fail-closed rule, bound to the file's git
// blob id so editing the file un-exempts it.
type ExemptRow struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
	Why  string `json:"why"`
}

// The reasons an exempt row may give.
const (
	WhyBinary    = "binary"
	WhyOversize  = "oversize"
	WhyLongRun   = "longrun"
	WhySymlink   = "symlink"
	WhySubmodule = "submodule"
)

func validWhy(s string) bool {
	switch s {
	case WhyBinary, WhyOversize, WhyLongRun, WhySymlink, WhySubmodule:
		return true
	}
	return false
}

// DigestFile is the parsed digest file.
type DigestFile struct {
	Version int           `json:"version"`
	Algo    string        `json:"algo"`
	MAC     string        `json:"mac"`
	Entries []DigestEntry `json:"entries"`
	Allow   []AllowRow    `json:"allow"`
	Exempt  []ExemptRow   `json:"exempt"`
}

func hmacSum(key []byte, prefix []byte, text []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(prefix)
	h.Write(text)
	return h.Sum(nil)
}

// EntryDigest hashes one normalized entry.
func EntryDigest(key []byte, e Entry) DigestEntry {
	set := e.Mode.index()
	if set < 0 {
		return DigestEntry{}
	}
	d := DigestEntry{
		D:    hex.EncodeToString(hmacSum(key, macPrefixes[set], []byte(e.Text))),
		Mode: string(e.Mode),
	}
	if e.Mode == ModePhrase {
		words := strings.Fields(e.Text)
		d.N = len(words)
		d.F = hex.EncodeToString(hmacSum(key, prefixFirst, []byte(words[0])))
	} else {
		d.Len = len(e.Text)
	}
	return d
}

// AllowDigest is the keyed digest of an allow-row chunk (lower case).
func AllowDigest(key []byte, chunk string) string {
	return hex.EncodeToString(hmacSum(key, prefixAllow, []byte(chunk)))
}

// BuildDigest hashes a plaintext list (plus the canary, which it always appends)
// under key and returns the signed digest file. The list is validated; allow and
// exempt rows are copied, sorted and validated.
func BuildDigest(key []byte, es []Entry, allow []AllowRow, exempt []ExemptRow) (*DigestFile, error) {
	if len(key) != 32 {
		return nil, errors.New("the gate key must be 32 bytes")
	}
	if len(es) == 0 {
		return nil, errors.New("no entries")
	}
	all, err := prepare(WithCanary(es), nil)
	if err != nil {
		return nil, err
	}
	df := &DigestFile{
		Version: DigestVersion,
		Algo:    DigestAlgo,
		Entries: make([]DigestEntry, 0, len(all)),
		Allow:   append([]AllowRow{}, allow...),
		Exempt:  append([]ExemptRow{}, exempt...),
	}
	for _, e := range all {
		df.Entries = append(df.Entries, EntryDigest(key, e))
	}
	sort.Slice(df.Entries, func(i, j int) bool { return df.Entries[i].D < df.Entries[j].D })
	sort.Slice(df.Allow, func(i, j int) bool { return allowLess(df.Allow[i], df.Allow[j]) })
	sort.Slice(df.Exempt, func(i, j int) bool { return exemptLess(df.Exempt[i], df.Exempt[j]) })
	df.MAC = strings.Repeat("0", 64) // a placeholder so Validate sees a well-formed file
	if err := df.Validate(); err != nil {
		return nil, err
	}
	df.MAC = df.ComputeMAC(key)
	return df, nil
}

func allowLess(a, b AllowRow) bool {
	if a.Where != b.Where {
		return a.Where < b.Where
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.D < b.D
}

func exemptLess(a, b ExemptRow) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Why < b.Why
}

// canonical is the text the MAC covers. Every field is length-prefixed, so no
// two different files have the same text, and every row carries a tag.
func (df *DigestFile) canonical() []byte {
	var b bytes.Buffer
	b.WriteString("leakgate/2")
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	field("v")
	field(strconv.Itoa(df.Version))
	field(df.Algo)
	for _, e := range df.Entries {
		field("e")
		field(e.Mode)
		field(e.D)
		field(strconv.Itoa(e.Len))
		field(strconv.Itoa(e.N))
		field(e.F)
	}
	for _, a := range df.Allow {
		field("a")
		field(a.Where)
		field(a.Path)
		field(a.D)
	}
	for _, x := range df.Exempt {
		field("x")
		field(x.Path)
		field(x.Blob)
		field(x.Why)
	}
	return b.Bytes()
}

// ComputeMAC is HMAC-SHA256(key, "leakgate/2" + the canonical text of the
// entries, allow rows and exempt rows), as hex.
func (df *DigestFile) ComputeMAC(key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(df.canonical())
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyMAC reports whether the file's mac is the one key computes.
func (df *DigestFile) VerifyMAC(key []byte) bool {
	got, err := hex.DecodeString(df.MAC)
	if err != nil || len(got) != 32 {
		return false
	}
	h := hmac.New(sha256.New, key)
	h.Write(df.canonical())
	return hmac.Equal(got, h.Sum(nil))
}

// SelectKey picks the key that verifies the file: the current key, else the
// previous one (a rotation in flight). No key is ErrKeyMissing; neither
// verifying is ErrMACMismatch (the message says nothing about either key).
func SelectKey(df *DigestFile, k Keys) ([]byte, error) {
	if k.Key == nil {
		return nil, ErrKeyMissing
	}
	if df.VerifyMAC(k.Key) {
		return k.Key, nil
	}
	if k.Prev != nil && df.VerifyMAC(k.Prev) {
		return k.Prev, nil
	}
	return nil, ErrMACMismatch
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Validate checks the structure of the file without a key: version and
// algorithm, hex lengths, entries sorted by digest and unique, per-mode field
// rules, and sorted unique allow and exempt rows with known places and reasons.
func (df *DigestFile) Validate() error {
	if df.Version != DigestVersion {
		return fmt.Errorf("digest file version %d is not %d", df.Version, DigestVersion)
	}
	if df.Algo != DigestAlgo {
		return fmt.Errorf("digest file algorithm is not %s", DigestAlgo)
	}
	if !isLowerHex(df.MAC, 64) {
		return errors.New("digest file mac is not 64 lower-case hex characters")
	}
	if len(df.Entries) == 0 {
		return errors.New("digest file has no entries")
	}
	for i, e := range df.Entries {
		if !isLowerHex(e.D, 64) {
			return fmt.Errorf("entry %d: digest is not 64 lower-case hex characters", i)
		}
		if i > 0 && df.Entries[i-1].D >= e.D {
			return fmt.Errorf("entry %d: entries are not sorted and unique", i)
		}
		mode := Mode(e.Mode)
		if mode.index() < 0 {
			return fmt.Errorf("entry %d: unknown mode", i)
		}
		if mode == ModePhrase {
			if e.N < 2 || e.N > 3 {
				return fmt.Errorf("entry %d: a phrase has two or three words", i)
			}
			if !isLowerHex(e.F, 64) {
				return fmt.Errorf("entry %d: phrase prefilter is not 64 lower-case hex characters", i)
			}
			if e.Len != 0 {
				return fmt.Errorf("entry %d: a phrase carries no length", i)
			}
		} else {
			if e.Len < 1 || e.Len > maxEntryLen {
				return fmt.Errorf("entry %d: length out of range", i)
			}
			if e.N != 0 || e.F != "" {
				return fmt.Errorf("entry %d: only a phrase carries a word count or a prefilter", i)
			}
		}
	}
	for i, a := range df.Allow {
		if a.Where != "name" && a.Where != "body" {
			return fmt.Errorf("allow row %d: where is neither name nor body", i)
		}
		if a.Path == "" {
			return fmt.Errorf("allow row %d: no path", i)
		}
		if !isLowerHex(a.D, 64) {
			return fmt.Errorf("allow row %d: digest is not 64 lower-case hex characters", i)
		}
		if i > 0 && !allowLess(df.Allow[i-1], a) {
			return fmt.Errorf("allow row %d: rows are not sorted and unique", i)
		}
	}
	for i, x := range df.Exempt {
		if x.Path == "" {
			return fmt.Errorf("exempt row %d: no path", i)
		}
		if !isLowerHex(x.Blob, 40) {
			return fmt.Errorf("exempt row %d: blob is not 40 lower-case hex characters", i)
		}
		if !validWhy(x.Why) {
			return fmt.Errorf("exempt row %d: unknown reason", i)
		}
		if i > 0 && !exemptLess(df.Exempt[i-1], x) {
			return fmt.Errorf("exempt row %d: rows are not sorted and unique by path and reason", i)
		}
	}
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Marshal is the committed form: LF line endings, one row per line, one
// trailing newline, byte-for-byte reproducible.
func (df *DigestFile) Marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"version\": %d,\n", df.Version)
	fmt.Fprintf(&b, "  \"algo\": %s,\n", jsonString(df.Algo))
	fmt.Fprintf(&b, "  \"mac\": %s,\n", jsonString(df.MAC))
	rows := func(name string, n int, row func(i int) string, last bool) {
		fmt.Fprintf(&b, "  %s: [", jsonString(name))
		if n > 0 {
			b.WriteString("\n")
			for i := 0; i < n; i++ {
				b.WriteString("    " + row(i))
				if i < n-1 {
					b.WriteString(",")
				}
				b.WriteString("\n")
			}
			b.WriteString("  ")
		}
		b.WriteString("]")
		if !last {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	rows("entries", len(df.Entries), func(i int) string {
		e := df.Entries[i]
		s := fmt.Sprintf("{\"d\": %s, \"mode\": %s", jsonString(e.D), jsonString(e.Mode))
		if e.Len != 0 {
			s += fmt.Sprintf(", \"len\": %d", e.Len)
		}
		if e.N != 0 {
			s += fmt.Sprintf(", \"n\": %d", e.N)
		}
		if e.F != "" {
			s += ", \"f\": " + jsonString(e.F)
		}
		return s + "}"
	}, false)
	rows("allow", len(df.Allow), func(i int) string {
		a := df.Allow[i]
		return fmt.Sprintf("{\"where\": %s, \"path\": %s, \"d\": %s}", jsonString(a.Where), jsonString(a.Path), jsonString(a.D))
	}, false)
	rows("exempt", len(df.Exempt), func(i int) string {
		x := df.Exempt[i]
		return fmt.Sprintf("{\"path\": %s, \"blob\": %s, \"why\": %s}", jsonString(x.Path), jsonString(x.Blob), jsonString(x.Why))
	}, true)
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// ParseDigest reads a digest file strictly (unknown fields, trailing data and
// every structural defect are errors). It does not verify the mac: that needs
// the key (SelectKey).
func ParseDigest(b []byte) (*DigestFile, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var df DigestFile
	if err := dec.Decode(&df); err != nil {
		return nil, fmt.Errorf("digest file: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("digest file: data after the top-level value")
	}
	if df.Entries == nil {
		df.Entries = []DigestEntry{}
	}
	if df.Allow == nil {
		df.Allow = []AllowRow{}
	}
	if df.Exempt == nil {
		df.Exempt = []ExemptRow{}
	}
	if err := df.Validate(); err != nil {
		return nil, err
	}
	return &df, nil
}
