package leakgate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
)

// alnum marks the bytes a run is made of: ASCII letters and digits.
var alnum = func() (t [256]bool) {
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
		t[c-'a'+'A'] = true
	}
	return t
}()

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isUpper(c byte) bool  { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool  { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

// entryRef is what a table hit resolves to.
type entryRef struct {
	mode Mode
	text string // plaintext matcher only
	id   string // digest matcher only: first four bytes of the entry digest, hex
}

// Matcher is the compiled entry list, either plaintext (NewPlainMatcher) or
// keyed (NewDigestMatcher). It is read-only after construction and safe for
// concurrent use; the mutable scan state lives in a Scanner.
type Matcher struct {
	key       []byte // nil for a plaintext matcher
	sets      [numSets]map[string]*entryRef
	firsts    map[string]struct{} // phrase first words (lower case, or their keyed prefilter)
	lens      [numSets][]int      // sorted entry lengths of sub, word, exact and subcs entries
	phraseN   [4]bool             // a phrase of n words exists
	maxPhrase int
}

func newMatcher(key []byte) *Matcher {
	m := &Matcher{key: key, firsts: map[string]struct{}{}}
	for i := range m.sets {
		m.sets[i] = map[string]*entryRef{}
	}
	return m
}

func (m *Matcher) addLen(set, n int) {
	for _, l := range m.lens[set] {
		if l == n {
			return
		}
	}
	m.lens[set] = append(m.lens[set], n)
	sort.Ints(m.lens[set])
}

// NewPlainMatcher compiles a plaintext list (used by the local scanner, the
// parity check and the tests). Entries are normalized and validated.
func NewPlainMatcher(es []Entry) (*Matcher, error) {
	es, err := prepare(es, nil)
	if err != nil {
		return nil, err
	}
	m := newMatcher(nil)
	for _, e := range es {
		set := e.Mode.index()
		m.sets[set][e.Text] = &entryRef{mode: e.Mode, text: e.Text}
		if e.Mode == ModePhrase {
			words := bytes.Fields([]byte(e.Text))
			m.firsts[string(words[0])] = struct{}{}
			m.addPhrase(len(words))
		} else {
			m.addLen(set, len(e.Text))
		}
	}
	return m, nil
}

func (m *Matcher) addPhrase(n int) {
	m.phraseN[n] = true
	if n > m.maxPhrase {
		m.maxPhrase = n
	}
}

// NewDigestMatcher compiles a digest file with its key. The caller has already
// chosen the key that verifies the file's MAC (SelectKey); this checks the
// file's structure and builds the tables from the digests alone.
func NewDigestMatcher(df *DigestFile, key []byte) (*Matcher, error) {
	if df == nil {
		return nil, errors.New("no digest file")
	}
	if len(key) != 32 {
		return nil, errors.New("digest matcher needs a 32-byte key")
	}
	if err := df.Validate(); err != nil {
		return nil, err
	}
	m := newMatcher(append([]byte(nil), key...))
	for _, e := range df.Entries {
		raw, err := hex.DecodeString(e.D)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("digest entry is not 64 hex characters")
		}
		mode := Mode(e.Mode)
		set := mode.index()
		m.sets[set][string(raw)] = &entryRef{mode: mode, id: e.D[:8]}
		if mode == ModePhrase {
			f, err := hex.DecodeString(e.F)
			if err != nil || len(f) != 32 {
				return nil, fmt.Errorf("phrase prefilter is not 64 hex characters")
			}
			m.firsts[string(f)] = struct{}{}
			m.addPhrase(e.N)
		} else {
			m.addLen(set, e.Len)
		}
	}
	return m, nil
}

// SelfTest scans a synthetic buffer holding the canary and fails when the
// matcher does not flag it: a blind matcher must fail loudly wherever a scan
// runs, even where the tree gate skips.
func (m *Matcher) SelfTest() error {
	findings, _ := m.ScanText("canary", []byte("x "+CanaryText()+" y\n"))
	for _, f := range findings {
		if f.Line == 1 && f.Mode == string(ModeSub) {
			return nil
		}
	}
	return errors.New("the matcher does not flag the canary entry: the gate is blind")
}

// hit is a finding before it gets its path.
type hit struct {
	line  int
	col   int
	ref   *entryRef
	chunk string // lower-case [A-Za-z0-9_.-] chunk around the hit (allow rows only)
}

type runHit struct {
	off int
	ref *entryRef
}

// runInfo is everything known about one run, independent of where it sits: the
// hits of the window modes and whether it can start a phrase. Cached per
// scanner by the run's bytes, so the work is proportional to distinct runs.
type runInfo struct {
	lower string
	hits  []runHit
	first bool
}

// Scanner is the per-goroutine scan state of a Matcher.
type Scanner struct {
	m      *Matcher
	mac    hash.Hash
	sum    [32]byte
	cache  map[string]*runInfo
	runs   []runRef
	acc    []byte
	lowBuf []byte
	cuts   []bool
}

type runRef struct {
	start, end int
	info       *runInfo
}

// NewScanner returns a scanner of m. A Scanner is not safe for concurrent use.
func (m *Matcher) NewScanner() *Scanner {
	s := &Scanner{m: m, cache: map[string]*runInfo{}}
	if m.key != nil {
		s.mac = hmac.New(sha256.New, m.key)
	}
	return s
}

var (
	macPrefixes = func() (p [numSets][]byte) {
		for i, c := range setCodes {
			p[i] = []byte("leakgate/2/" + string(c) + "\x00")
		}
		return p
	}()
	prefixFirst = []byte("leakgate/2/f\x00")
	prefixAllow = []byte("leakgate/2/a\x00")
)

// find resolves a window (already in the mode's form: lower case, or as
// written for subcs) to its entry, if any.
func (s *Scanner) find(set int, text []byte) *entryRef {
	if s.m.key == nil {
		return s.m.sets[set][string(text)]
	}
	s.mac.Reset()
	s.mac.Write(macPrefixes[set])
	s.mac.Write(text)
	sum := s.mac.Sum(s.sum[:0])
	return s.m.sets[set][string(sum)]
}

func (s *Scanner) isFirst(low []byte) bool {
	if s.m.key == nil {
		_, ok := s.m.firsts[string(low)]
		return ok
	}
	s.mac.Reset()
	s.mac.Write(prefixFirst)
	s.mac.Write(low)
	sum := s.mac.Sum(s.sum[:0])
	_, ok := s.m.firsts[string(sum)]
	return ok
}

// allowDigest is the raw keyed digest of an allow-row chunk.
func (s *Scanner) allowDigest(chunk string) string {
	s.mac.Reset()
	s.mac.Write(prefixAllow)
	s.mac.Write([]byte(chunk))
	return string(s.mac.Sum(s.sum[:0]))
}

// camelCuts marks the positions inside a run where a camel or digit boundary
// sits: before an upper-case letter that follows a lower-case letter or a
// digit, and before the last upper-case letter of an upper-case run that a
// lower-case letter follows. Position 0 and len(run) are always cuts.
func (s *Scanner) camelCuts(run []byte) []bool {
	n := len(run)
	if cap(s.cuts) < n+1 {
		s.cuts = make([]bool, n+1)
	}
	cut := s.cuts[:n+1]
	for i := range cut {
		cut[i] = false
	}
	cut[0], cut[n] = true, true
	for i := 1; i < n; i++ {
		cur, prev := run[i], run[i-1]
		if isUpper(cur) && (isLower(prev) || isDigit(prev)) {
			cut[i] = true
		} else if isUpper(cur) && isUpper(prev) && i+1 < n && isLower(run[i+1]) {
			cut[i] = true
		}
	}
	return cut
}

func (s *Scanner) computeRun(run []byte) *runInfo {
	m := s.m
	n := len(run)
	low := append(s.lowBuf[:0], run...)
	for i, c := range low {
		if isUpper(c) {
			low[i] = c + 32
		}
	}
	s.lowBuf = low
	ri := &runInfo{lower: string(low)}
	for _, L := range m.lens[setSub] {
		if L > n {
			break
		}
		for i := 0; i+L <= n; i++ {
			if ref := s.find(setSub, low[i:i+L]); ref != nil {
				ri.hits = append(ri.hits, runHit{i, ref})
			}
		}
	}
	if len(m.lens[setWord]) > 0 {
		var cut []bool
		for _, L := range m.lens[setWord] {
			if L > n {
				break
			}
			for i := 0; i+L <= n; i++ {
				if cut == nil {
					cut = s.camelCuts(run)
				}
				left := i == 0 || cut[i] || !isLetter(run[i-1])
				right := i+L == n || cut[i+L] || !isLetter(run[i+L])
				if !left || !right {
					continue
				}
				if ref := s.find(setWord, low[i:i+L]); ref != nil {
					ri.hits = append(ri.hits, runHit{i, ref})
				}
			}
		}
	}
	for _, L := range m.lens[setExact] {
		if L == n {
			if ref := s.find(setExact, low); ref != nil {
				ri.hits = append(ri.hits, runHit{0, ref})
			}
			break
		}
	}
	for _, L := range m.lens[setSubCS] {
		if L > n {
			break
		}
		for i := 0; i+L <= n; i++ {
			if ref := s.find(setSubCS, run[i:i+L]); ref != nil {
				ri.hits = append(ri.hits, runHit{i, ref})
			}
		}
	}
	if m.maxPhrase > 0 {
		ri.first = s.isFirst(low)
	}
	return ri
}

const maxCache = 1 << 20

func (s *Scanner) runInfo(run []byte) *runInfo {
	if ri, ok := s.cache[string(run)]; ok {
		return ri
	}
	ri := s.computeRun(run)
	if len(s.cache) >= maxCache {
		s.cache = map[string]*runInfo{}
	}
	s.cache[string(run)] = ri
	return ri
}

// chunkAround returns the lower-case chunk of [A-Za-z0-9_.-] around byte col
// (0-based) of line.
func chunkAround(line []byte, col int) string {
	in := func(c byte) bool { return alnum[c] || c == '_' || c == '.' || c == '-' }
	lo, hi := col, col
	for lo > 0 && in(line[lo-1]) {
		lo--
	}
	for hi < len(line) && in(line[hi]) {
		hi++
	}
	b := append([]byte(nil), line[lo:hi]...)
	for i, c := range b {
		if isUpper(c) {
			b[i] = c + 32
		}
	}
	return string(b)
}

// scanStream tokenizes one stream (already folded) line by line and appends
// the hits and the lines of the runs over the limit.
func (s *Scanner) scanStream(stream []byte, wantChunks bool, hits []hit, long []int) ([]hit, []int) {
	line := 1
	pos := 0
	for {
		end := bytes.IndexByte(stream[pos:], '\n')
		last := end < 0
		if last {
			end = len(stream)
		} else {
			end += pos
		}
		hits, long = s.scanLine(stream[pos:end], line, wantChunks, hits, long)
		if last {
			return hits, long
		}
		pos = end + 1
		line++
	}
}

func (s *Scanner) scanLine(ln []byte, lineNo int, wantChunks bool, hits []hit, long []int) ([]hit, []int) {
	s.runs = s.runs[:0]
	for i := 0; i < len(ln); {
		if !alnum[ln[i]] {
			i++
			continue
		}
		j := i + 1
		for j < len(ln) && alnum[ln[j]] {
			j++
		}
		var info *runInfo
		if j-i > maxRun {
			long = append(long, lineNo)
		} else {
			info = s.runInfo(ln[i:j])
			for _, h := range info.hits {
				ht := hit{line: lineNo, col: i + h.off + 1, ref: h.ref}
				if wantChunks {
					ht.chunk = chunkAround(ln, i+h.off)
				}
				hits = append(hits, ht)
			}
		}
		s.runs = append(s.runs, runRef{i, j, info})
		i = j
	}
	if s.m.maxPhrase == 0 || len(s.runs) < 2 {
		return hits, long
	}
	for k := range s.runs {
		rk := s.runs[k]
		if rk.info == nil || !rk.info.first {
			continue
		}
		if rk.end-rk.start == 1 {
			// A one-letter first word is a drive letter: it counts only before a
			// colon or after /mnt/. Everything else (a format verb, a stray
			// letter) is not a path.
			ok := rk.end < len(ln) && ln[rk.end] == ':'
			if !ok && rk.start >= 5 && string(ln[rk.start-5:rk.start]) == "/mnt/" {
				ok = true
			}
			if !ok {
				continue
			}
		}
		acc := append(s.acc[:0], rk.info.lower...)
		for j := k + 1; j < len(s.runs) && j < k+s.m.maxPhrase; j++ {
			rj := s.runs[j]
			if rj.info == nil {
				break
			}
			acc = append(acc, ' ')
			acc = append(acc, rj.info.lower...)
			if !s.m.phraseN[j-k+1] {
				continue
			}
			if ref := s.find(setPhrase, acc); ref != nil {
				ht := hit{line: lineNo, col: rk.start + 1, ref: ref}
				if wantChunks {
					ht.chunk = chunkAround(ln, rk.start)
				}
				hits = append(hits, ht)
			}
		}
		s.acc = acc
	}
	return hits, long
}

// scanText runs pass A (the fold) and pass B (the escape pass) over text that
// is already UTF-8 and returns the unioned hits and the lines of runs over the
// limit. The fold never adds or removes a newline, and pass B blanks in place,
// so a line number is a line of the original and every column is a column of
// the folded stream.
func (s *Scanner) scanText(data []byte, wantChunks bool) ([]hit, []int) {
	folded := data
	if NeedsFold(data) {
		folded = Fold(data)
	}
	hits, long := s.scanStream(folded, wantChunks, nil, nil)
	if blanked, changed := BlankEscapes(folded); changed {
		type key struct {
			line, col int
			ref       *entryRef
		}
		seen := make(map[key]struct{}, len(hits))
		for _, h := range hits {
			seen[key{h.line, h.col, h.ref}] = struct{}{}
		}
		extra, _ := s.scanStream(blanked, wantChunks, nil, nil)
		for _, h := range extra {
			if _, ok := seen[key{h.line, h.col, h.ref}]; !ok {
				hits = append(hits, h)
			}
		}
	}
	return hits, long
}

func (s *Scanner) toFinding(h hit, path string, name bool) Finding {
	return Finding{Path: path, Line: h.line, Col: h.col, Mode: string(h.ref.mode), ID: h.ref.id, Text: h.ref.text, Name: name}
}

// ScanText scans one text body (UTF-8; the caller has already decoded UTF-16 or
// extracted PNG text) and returns the findings under path and the line of every
// run over 96 characters.
func (m *Matcher) ScanText(path string, data []byte) ([]Finding, []int) {
	s := m.NewScanner()
	hits, long := s.scanText(data, false)
	out := make([]Finding, 0, len(hits))
	for _, h := range hits {
		out = append(out, s.toFinding(h, path, false))
	}
	sortFindings(out)
	return out, long
}

// ScanName scans a tracked path as one line; the findings are name findings.
func (m *Matcher) ScanName(path string) []Finding {
	s := m.NewScanner()
	hits, _ := s.scanText(nameBytes(path), false)
	out := make([]Finding, 0, len(hits))
	for _, h := range hits {
		out = append(out, s.toFinding(h, path, true))
	}
	sortFindings(out)
	return out
}

// nameBytes is a path as one line: a newline inside a path is a separator.
func nameBytes(path string) []byte {
	b := []byte(path)
	for i, c := range b {
		if c == '\n' {
			b[i] = ' '
		}
	}
	return b
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if pa, pb := a.DisplayPath(), b.DisplayPath(); pa != pb {
			return pa < pb
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.Label() < b.Label()
	})
}
