// Package servingcfg reads the llama-swap config a box serves (config key serving_config_path) for
// one fact: the cards each model is pinned to.
//
// It is a leaf (standard library and YAML only) because two packages that cannot import each other
// both need the answer. internal/config arms the load fence from it (Config.ModelPins, which
// modelaffinity is armed with by Load) and internal/gpualloc places the models of a lease's unload
// list by it (LeaseScope.Locate), and gpualloc imports config. With one parser the fence and the
// unload list cannot place a model differently (registers F15 and C-86a).
//
// The file is read again when it changes. The fence runs in processes that live for days (the MCP
// server, the fleet node) and a llama-swap config is edited and reloaded under them, so Load memoises
// on the file's modification time and size and Stats the file on every call; PinsOf, which the
// fence asks on every poll of a blocked load, costs one Stat while the file is unchanged.
//
// The serving config (config key serving_config_path) is the llama-swap yaml the box actually runs.
// Nearly every llama.cpp model in it pins its cards with an env entry,
// `CUDA_VISIBLE_DEVICES=<uuid>[,<uuid>]` (GPU UUIDs, sometimes indices), and that pin is where the
// seat really runs, so it places a model when `layers` does not declare it.
//
// ONLY THE ENV PIN COUNTS. A model whose entry names no device in env (an unpinned dual-card
// `-sm layer` / `--tensor-split` entry), a `proxy:` seat (a vLLM engine, whose cards are pinned in
// its own unit and not here), a pin that is not a literal device list (a macro), an alias two
// entries claim, and a bare index when the entry does not also set CUDA_DEVICE_ORDER=PCI_BUS_ID
// (without it a CUDA index is not an nvidia-smi index, and reading it as one could place a seat on
// the wrong card) are all UNKNOWN, with the reason. Device flags in cmd are never read. Unknown is
// the answer every caller reads as "on every card".
package servingcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Pin is one model's placement as the serving config states it: its pin elements, or why it has none
// the harness can use.
type Pin struct {
	Pins []string
	Why  string
}

var pciOrderEntry = regexp.MustCompile(`(?i)^\s*CUDA_DEVICE_ORDER\s*=\s*["']?PCI_BUS_ID["']?\s*$`)

// Snapshot is one reading of the serving config: the pin of every model id and alias (lower-cased)
// and the model ids it declares. The zero value places nothing.
type Snapshot struct {
	pins map[string]Pin
	ids  []string
}

// Pin is the placement of the model id or alias name (case-insensitive). ok is false for a name the
// config does not list. The returned Pins are the caller's own copy.
func (s Snapshot) Pin(name string) (Pin, bool) {
	p, ok := s.pins[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Pin{}, false
	}
	p.Pins = append([]string(nil), p.Pins...)
	return p, true
}

// IDs lists the model ids the config declares (sorted; aliases are not ids).
func (s Snapshot) IDs() []string { return append([]string(nil), s.ids...) }

var errUnset = errors.New("serving_config_path is unset")

// Pins reads the llama-swap config at path and returns the pin of every model id and alias
// (lower-cased). It reads the file every time; Load is the memoised form. An error means no model is
// placed by it.
func Pins(path string) (map[string]Pin, error) {
	snap, err := Read(path)
	return snap.pins, err
}

// Read parses the llama-swap config at path, every time (no memo).
func Read(path string) (Snapshot, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Snapshot{}, errUnset
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	return parse(b)
}

// parsed counts the parses made, for the tests that pin the memo.
var parsed atomic.Int64

func parse(b []byte) (Snapshot, error) {
	parsed.Add(1)
	var doc struct {
		Models map[string]struct {
			Aliases []string `yaml:"aliases"`
			Env     []string `yaml:"env"`
			Proxy   string   `yaml:"proxy"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return Snapshot{}, fmt.Errorf("not parseable YAML: %w", err)
	}
	out := map[string]Pin{}
	ids := make([]string, 0, len(doc.Models))
	for id := range doc.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := doc.Models[id]
		pin := pinOf(m.Env, m.Proxy)
		for _, name := range append([]string{id}, m.Aliases...) {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if prev, dup := out[key]; dup && !samePin(prev, pin) {
				out[key] = Pin{Why: "two entries of the serving config claim the name with different pins"}
				continue
			}
			out[key] = pin
		}
	}
	return Snapshot{pins: out, ids: ids}, nil
}

func samePin(a, b Pin) bool {
	return a.Why == b.Why && strings.Join(a.Pins, ",") == strings.Join(b.Pins, ",")
}

func pinOf(env []string, proxy string) Pin {
	if strings.TrimSpace(proxy) != "" {
		return Pin{Why: "it is a proxy: seat, whose cards are pinned in its own engine and not in the serving config"}
	}
	value, found, pci := "", false, false
	for _, e := range env {
		if pciOrderEntry.MatchString(e) {
			pci = true
		}
		if k, v, ok := strings.Cut(e, "="); ok && strings.TrimSpace(k) == "CUDA_VISIBLE_DEVICES" && !found {
			value, found = strings.Trim(strings.TrimSpace(v), `"'`), true
		}
	}
	if !found {
		return Pin{Why: "its serving config entry pins no CUDA_VISIBLE_DEVICES in env"}
	}
	if value == "" || strings.ContainsAny(value, "${}") {
		return Pin{Why: "its serving config pin is not a literal device list"}
	}
	var pins []string
	for _, el := range strings.Split(value, ",") {
		el = strings.TrimSpace(el)
		if el == "" {
			return Pin{Why: "its serving config pin has a blank element"}
		}
		if isIndex(el) && !pci {
			return Pin{Why: "its serving config pin is a CUDA index and the entry does not set CUDA_DEVICE_ORDER=PCI_BUS_ID, so it is not an nvidia-smi index"}
		}
		pins = append(pins, el)
	}
	return Pin{Pins: pins}
}

func isIndex(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// racyWindow is how long a file must have been quiet before a reading of it is trusted by its
// modification time and size alone. A rewrite in the same clock tick as the read, to the same size,
// has the same (time, size) as the content that was read, and the coarsest file systems keep a
// modification time to two seconds; a reading of a file changed less than this before the read is
// therefore read again on the next call, until the file has settled (the rule git applies to its
// index).
const racyWindow = 3 * time.Second

// maxCached bounds the memo: a process asks about one serving config, tests about many.
const maxCached = 16

type cached struct {
	size  int64
	mtime time.Time
	snap  Snapshot
	err   error // a parse error, kept with the file it was made from
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cached{}
	// clock is the time a reading is judged against; tests swap it.
	clock = time.Now
)

// Load returns the serving config at path as it is now. The file is Stat-ed on every call and parsed
// again only when its modification time or size changed (or it was last changed within racyWindow of
// the reading), so a long-lived process sees a llama-swap config reload on its next call without a
// restart, and an unchanged file costs one Stat. A file that is not set, cannot be Stat-ed or cannot
// be read is an error and no snapshot: a memo is never served for a file that is gone.
func Load(path string) (Snapshot, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Snapshot{}, errUnset
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Snapshot{}, asOpenError(err)
	}
	size, mtime := fi.Size(), fi.ModTime()
	cacheMu.Lock()
	c, hit := cache[path]
	cacheMu.Unlock()
	if hit && c.size == size && c.mtime.Equal(mtime) {
		return c.snap, c.err
	}
	// The clock is read before the file, so "quiet for racyWindow" is judged at or before the read.
	readAt := clock()
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	snap, perr := parse(b)
	if mtime.After(readAt.Add(-racyWindow)) {
		return snap, perr // changed too recently to trust (time, size) as its identity: read it again next time
	}
	cacheMu.Lock()
	if len(cache) >= maxCached {
		cache = map[string]cached{}
	}
	cache[path] = cached{size: size, mtime: mtime, snap: snap, err: perr}
	cacheMu.Unlock()
	return snap, perr
}

// asOpenError words a failed Stat the way the read it stands in for would have failed ("open <path>:
// ..."), so the sentence an operator reads for an unreadable serving config is the one it always was.
func asOpenError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &fs.PathError{Op: "open", Path: pe.Path, Err: pe.Err}
	}
	return err
}

// noteLogf is where the fence says it cannot read the serving config. A variable so a test can listen
// (SetNoteLogger); the default writes to the process log like the rest of the fence's notes.
var noteLogf = log.Printf

// SetNoteLogger replaces where the unreadable-serving-config note goes and returns a function that puts
// the previous one back; nil restores the process log. A test installs its own so that the note is
// heard and the process-wide once-per-cause memory starts empty.
func SetNoteLogger(fn func(format string, args ...any)) (restore func()) {
	noteMu.Lock()
	prev := noteLogf
	if fn == nil {
		fn = log.Printf
	}
	noteLogf = fn
	noted = map[string]string{}
	noteMu.Unlock()
	return func() {
		noteMu.Lock()
		noteLogf = prev
		noted = map[string]string{}
		noteMu.Unlock()
	}
}

var (
	noteMu sync.Mutex
	// noted is, for each serving config path, the text of the error last said about it.
	noted = map[string]string{}
)

// sayUnreadable tells the operator, once per cause, that the fence cannot place a model by the serving
// config. Failing closed is the right answer and an invisible one: the refusal a blocked load prints
// names only the lease holder, so a moved, mid-edit or mistyped llama-swap config would read as the
// behaviour the fence had before it could place a seat at all. An unset path is the default and not a
// fault, so it says nothing; a set path that cannot be read, or is not YAML, is said again only when
// its cause changes or after the file was readable in between.
func sayUnreadable(path string, err error) {
	if errors.Is(err, errUnset) {
		return
	}
	path = strings.TrimSpace(path)
	cause := err.Error()
	noteMu.Lock()
	if noted[path] == cause {
		noteMu.Unlock()
		return
	}
	if _, known := noted[path]; !known && len(noted) >= maxCached {
		noted = map[string]string{}
	}
	noted[path] = cause
	logf := noteLogf
	noteMu.Unlock()
	logf("servingcfg: the serving config at %s cannot be read (%v): the load fence places no model by it, so a lease on any card fences every seat the layers do not place (said once per cause)", path, err)
}

// sayReadable forgets what was said about path once it reads, so the same fault coming back later is
// said again.
func sayReadable(path string) {
	noteMu.Lock()
	if len(noted) > 0 {
		delete(noted, strings.TrimSpace(path))
	}
	noteMu.Unlock()
}

// PinsOf is the placement the load fence uses: the pins the serving config at path gives model, as the
// caller's own copy. ok is false when the config places it nowhere (path unset or unreadable, the
// model not listed, a proxy: seat, an entry that pins nothing it can read), which every caller reads
// as "on every card".
func PinsOf(path, model string) ([]string, bool) {
	snap, err := Load(path)
	if err != nil {
		sayUnreadable(path, err)
		return nil, false
	}
	sayReadable(path)
	p, ok := snap.Pin(model)
	if !ok || len(p.Pins) == 0 {
		return nil, false
	}
	return p.Pins, true
}

// IDs lists the model ids the serving config at path declares (aliases are not ids), for the
// candidate list of a lease whose llama-swap roster could not be read. Empty when it cannot be read.
func IDs(path string) []string {
	snap, err := Load(path)
	if err != nil {
		return nil
	}
	return snap.IDs()
}
