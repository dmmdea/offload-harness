package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// layout.go is the one place that decides how a component's live path is organised, and the one place that
// flips it (docs/systems/update.md, "Layout").
//
//	layoutGens  the live directory is a junction (Windows) or symlink to <live>-versions/<id>/, the layout the
//	            installer makes for every engine. Directory components always use it. llama-swap uses it when
//	            <home>/llama-swap is such a link (or is the plain directory the installer would migrate).
//	layoutFile  a hand layout: llama-swap is one file in an ordinary directory (<dir>/llama-swap.exe), swapped by
//	            rename with the previous build kept as <file>.bak-<id>.
//
// Both layouts share the drain, the daemon stop, the restart and the verification (single.go, nodeswap).

type layoutKind int

const (
	layoutFile layoutKind = iota
	layoutGens
)

// layout is a component's decided layout.
type layout struct {
	Kind  layoutKind
	Dir   string // the live directory: the link itself for layoutGens
	Vroot string // where generations, their meta files and the staging area live
	Why   string // one line for the report
}

// layoutOf decides the layout of a component's live path.
func (e *Env) layoutOf(def *compDef, live string) layout {
	if !def.Single {
		return layout{Kind: layoutGens, Dir: live, Vroot: versionsRoot(live, false), Why: "generations behind " + live}
	}
	dir := filepath.Dir(live)
	vroot := versionsRoot(dir, false) // <dir>-versions, the installer's
	if t, ok := resolveLink(dir); ok {
		if isVersionsDir(filepath.Dir(t)) {
			return layout{Kind: layoutGens, Dir: dir, Vroot: filepath.Dir(t), Why: dir + " is a link into " + filepath.Dir(t) + " (the installer's layout)"}
		}
		return layout{Kind: layoutFile, Dir: dir, Vroot: versionsRoot(live, true), Why: dir + " is a link outside any -versions directory: the file is swapped in place"}
	}
	if filepath.Clean(live) == filepath.Join(e.Home, def.liveRel()) {
		return layout{Kind: layoutGens, Dir: dir, Vroot: vroot, Why: dir + " is the install home's llama-swap directory: migrated to " + vroot + " on the first swap"}
	}
	return layout{Kind: layoutFile, Dir: dir, Vroot: versionsRoot(live, true), Why: live + " is a hand layout: the file is swapped by rename, the previous build kept as " + filepath.Base(live) + ".bak-<id>"}
}

func isVersionsDir(p string) bool { return strings.HasSuffix(filepath.Base(p), "-versions") }

// resolveLink is where a junction or symlink at path points, as an absolute path; ok is false for anything else.
func resolveLink(path string) (string, bool) {
	t, ok := readLink(path)
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(path), t)
	}
	return filepath.Clean(t), true
}

// genMeta is the part of a generation's <id>.meta.json the verb reads: the verb's own, and the installer's.
type genMeta struct {
	ID          string   `json:"id"`
	Tag         string   `json:"tag"`
	Digests     []string `json:"digests"`
	Sizes       []int64  `json:"sizes"`
	Source      string   `json:"source"`
	InstalledAt string   `json:"installed_at"`
}

func readGenMeta(genDir string) (genMeta, bool) {
	var m genMeta
	b, err := os.ReadFile(genDir + ".meta.json")
	if err != nil {
		return m, false
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	if json.Unmarshal(b, &m) != nil {
		return m, false
	}
	if m.Tag == "" {
		m.Tag = m.ID
	}
	return m, m.Tag != ""
}

// reconcileLive makes the live path's own generation the truth of what is installed: when the live link points at
// <versions>/<g> and that generation's meta file names a tag the record does not, the record follows the link (an
// installer run that moved the junction and stopped before it wrote its record, or one that recorded something
// else). The record it replaces becomes the previous generation when its directory is still there.
func (e *Env) reconcileLive(def *compDef, st *State, live string) {
	lay := e.layoutOf(def, live)
	if lay.Kind != layoutGens {
		return
	}
	t, ok := resolveLink(lay.Dir)
	if !ok || !isVersionsDir(filepath.Dir(t)) {
		return
	}
	m, ok := readGenMeta(t)
	if !ok || (st.Current != nil && st.Current.ID == m.Tag) {
		if ok && st.Current != nil && st.Current.Path == "" {
			st.Current.Path = t
		}
		return
	}
	old := st.Current
	cur := &Generation{ID: m.Tag, Path: t, Source: m.Source, InstalledAt: m.InstalledAt}
	if len(m.Digests) > 0 {
		cur.Digest = m.Digests[0]
	}
	if len(m.Sizes) > 0 {
		cur.Size = m.Sizes[0]
	}
	st.Current = cur
	if old != nil && !strings.HasPrefix(old.ID, "unknown") {
		prevDir := filepath.Join(filepath.Dir(t), old.ID)
		if exists(prevDir) {
			old.Path = prevDir
			st.Previous = old
		}
	}
	e.logf("%s: the live path serves %s (its meta file); the record said %s", def.Name, m.Tag, installedID(&State{Current: old}))
}

// preGen is a generation the installer left on disk (components.<key>.staged) that the verb may promote without a
// download: its meta file's digests equal the publisher's for the newest build.
type preGen struct {
	ID   string // the directory name under the versions root
	Dir  string
	Size int64
}

// stagedGen returns the installer's staged generation when it is exactly the candidate, else nil with the reason
// in note. The files on disk are not re-hashed (the archive is gone after extraction); the generation is trusted
// by its meta file, whose digests the installer took from the publisher, equal to the candidate's.
func (e *Env) stagedGen(def *compDef, st *State, cand *Candidate, live string) (pre *preGen, note string) {
	if st.Staged == "" || def.Rule == ruleNPM {
		return nil, ""
	}
	lay := e.layoutOf(def, live)
	if lay.Kind != layoutGens {
		return nil, "the installer's staged generation " + st.Staged + " is ignored: " + lay.Why
	}
	dir := filepath.Join(lay.Vroot, st.Staged)
	if !exists(dir) {
		st.Staged = ""
		return nil, ""
	}
	m, ok := readGenMeta(dir)
	if !ok {
		return nil, "the staged generation " + st.Staged + " has no readable meta file; downloading " + cand.ID + " instead"
	}
	if cmpID(m.Tag, cand.ID) != 0 {
		return nil, fmt.Sprintf("the staged generation %s (%s) is not the newest build (%s); it is ignored and will be pruned", st.Staged, m.Tag, cand.ID)
	}
	if !sameDigests(m.Digests, cand.Assets) {
		return nil, "the staged generation " + st.Staged + " carries other digests than the publisher's for " + cand.ID + "; downloading it instead"
	}
	if findFile(dir, def.exeName()) == "" && findFile(dir, def.Binary) == "" {
		return nil, "the staged generation " + st.Staged + " holds no " + def.exeName() + "; downloading " + cand.ID + " instead"
	}
	var size int64
	for _, s := range m.Sizes {
		size += s
	}
	return &preGen{ID: st.Staged, Dir: dir, Size: size}, "the installer's staged generation " + st.Staged + " is the newest build: promoted without a download"
}

func sameDigests(have []string, want []Asset) bool {
	if len(have) != len(want) || len(have) == 0 {
		return false
	}
	set := map[string]int{}
	for _, d := range have {
		set[normDigest(d)]++
	}
	for _, a := range want {
		d := normDigest(a.Digest)
		if d == "" || set[d] == 0 {
			return false
		}
		set[d]--
	}
	return true
}

func normDigest(d string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "sha256:"))
}

// genLink is the link flip of one llama-swap generation change, in the shape nodeswap.Run drives: stop the daemon,
// Flip(true), restart, verify; on any failure Flip(false), restart, verify. A live directory that is still a plain
// directory is migrated (renamed into the versions root as the previous generation) at the first flip, when the
// daemon no longer holds it.
type genLink struct {
	dir, exe  string
	vroot     string
	priorID   string
	oldTarget string // where the link pointed; "" = dir is a plain directory, migrated at the first flip
	newDir    string
	migrated  string // where the plain directory went
	g         *nodeswap.Generation
}

func newGenLink(dir, exeName, vroot, priorID, oldTarget, newDir string) *genLink {
	l := &genLink{dir: dir, exe: exeName, vroot: vroot, priorID: priorID, oldTarget: oldTarget, newDir: newDir}
	old := dir
	if oldTarget != "" {
		old = oldTarget
	}
	l.g = &nodeswap.Generation{OldExe: filepath.Join(old, exeName), NewExe: filepath.Join(newDir, exeName), Flip: l.flip}
	return l
}

// PriorDir is the directory that held the previous build (where the link pointed, or where the plain directory went).
func (l *genLink) PriorDir() string {
	if l.migrated != "" {
		return l.migrated
	}
	return l.oldTarget
}

func (l *genLink) flip(toNew bool) error {
	if toNew {
		if l.oldTarget == "" && l.migrated == "" {
			return l.migrate()
		}
		return flipLink(l.dir, l.newDir)
	}
	switch {
	case l.migrated != "":
		return flipLink(l.dir, l.migrated)
	case l.oldTarget != "":
		return flipLink(l.dir, l.oldTarget)
	}
	return nil // the plain directory was never moved
}

func (l *genLink) migrate() error {
	id := l.priorID
	if id == "" {
		id = "unknown-" + time.Now().Format("20060102")
	}
	dst := filepath.Join(l.vroot, id)
	if exists(dst) {
		dst += "-migrated-" + randHex()
	}
	if err := os.MkdirAll(l.vroot, 0o755); err != nil {
		return err
	}
	var rerr error
	for i := 0; i < 3; i++ {
		if rerr = os.Rename(l.dir, dst); rerr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if rerr != nil {
		return fmt.Errorf("moving %s into %s: %w (is something still running from it?)", l.dir, l.vroot, rerr)
	}
	if lerr := makeLink(l.dir, l.newDir); lerr != nil {
		if back := os.Rename(dst, l.dir); back != nil {
			return fmt.Errorf("%w; and moving %s back failed: %w", lerr, dst, back)
		}
		return lerr
	}
	l.migrated = dst
	l.g.OldExe = filepath.Join(dst, l.exe)
	return nil
}
