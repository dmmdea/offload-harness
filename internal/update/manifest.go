package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Generation is one installed build of a component.
type Generation struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Digest      string `json:"digest,omitempty"` // the publisher's digest of the archive that produced it
	Size        int64  `json:"size,omitempty"`
	Source      string `json:"source,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
	Asset       string `json:"asset,omitempty"`
	// FileSHA256 is the hash of the installed file itself (single-file components), which is what a
	// rollback can verify without the archive.
	FileSHA256 string `json:"file_sha256,omitempty"`
}

// Rejected is a candidate that failed and is never retried.
type Rejected struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	TS     string `json:"ts"`
}

// State is installed.json schema 2 for one component.
type State struct {
	Current   *Generation `json:"current,omitempty"`
	Previous  *Generation `json:"previous,omitempty"`
	Rejected  []Rejected  `json:"rejected,omitempty"`
	ETag      string      `json:"etag,omitempty"`
	CheckedAt string      `json:"checked_at,omitempty"`
	Floor     string      `json:"floor,omitempty"`
	// Dest is the installer's local name for the component, carried through untouched.
	Dest string `json:"dest,omitempty"`
	// Live is the live path the verb last worked on (a junction, symlink or file): a later run with no --live and no
	// update.paths finds the component there.
	Live string `json:"live,omitempty"`
	// Staged is the generation the installer left on disk, behind a running engine, for the verb to promote
	// (<live>-versions/<Staged>); the verb clears it once it has promoted, or superseded it with a newer build.
	Staged string `json:"staged,omitempty"`

	// stagedFrom are the installer records (other keys of the components map) that carried Staged; they are
	// rewritten without it once Staged is cleared.
	stagedFrom []string
}

func (s *State) isRejected(id string) bool {
	for _, r := range s.Rejected {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Manifest is the installer's installed.json. Every top-level key this package does not own (profile,
// backend, accelerators, ...) is kept verbatim: the fleet node reads them.
type Manifest struct {
	Path string
	top  map[string]json.RawMessage
	// comps holds the components this package manages; legacy holds every other entry of a schema 1
	// "components" map (the installer's flat strings), written back untouched.
	comps  map[string]*State
	legacy map[string]json.RawMessage
}

// LoadManifest reads installed.json. A missing file is an empty manifest; schema 1 (flat components) is read
// tolerantly: an entry that is not a schema 2 object is legacy and survives a write unchanged.
func LoadManifest(path string) (*Manifest, error) {
	m := &Manifest{Path: path, top: map[string]json.RawMessage{}, comps: map[string]*State{}, legacy: map[string]json.RawMessage{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:] // the installer writes UTF-8 with a BOM
	}
	if err := json.Unmarshal(b, &m.top); err != nil {
		return nil, fmt.Errorf("installed.json: %w", err)
	}
	var comps map[string]json.RawMessage
	if raw, ok := m.top["components"]; ok {
		_ = json.Unmarshal(raw, &comps) // not an object: treated as no components
	}
	for name, raw := range comps {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil && hasKey(probe, "current", "previous", "etag", "checked_at", "rejected") {
			st := &State{}
			if err := json.Unmarshal(raw, st); err == nil {
				m.comps[name] = st
				continue
			}
		}
		m.legacy[name] = raw
	}
	for _, name := range AllComponents {
		m.adoptInstaller(name)
	}
	return m, nil
}

// installerRecord is one component entry as the installer writes it (schema 2, flat): no `current`.
type installerRecord struct {
	Version     string `json:"version"`
	Digest      string `json:"digest"`
	Size        int64  `json:"size"`
	Source      string `json:"source"`
	InstalledAt string `json:"installed_at"`
	Dest        string `json:"dest"`
	Staged      string `json:"staged"`
}

// isInstallerKey reports whether an entry of the components map is one of the installer's keys for a component:
// the installer names engines by build family (llama-vulkan, llama-cuda13, sdcpp-vulkan, ...), the verb by
// component. The CUDA runtime zip (llama-cudart*) rides with the llama build and is not a record of its own.
func isInstallerKey(component, key string) bool {
	switch component {
	case Llama:
		return key == Llama || (strings.HasPrefix(key, "llama-") && key != LlamaSwap && !strings.Contains(key, "cudart"))
	case SDCpp:
		return key == SDCpp || strings.HasPrefix(key, "sdcpp-")
	case Whisper:
		return key == Whisper || strings.HasPrefix(key, "whisper-")
	}
	return key == component
}

// adoptInstaller reads the installer's flat record as the component's current generation. A record under the
// component's own key (llama-swap, hyperframes) becomes the verb's entry and is rewritten in the verb's shape on
// the next Save; records under a build-family key (llama-cuda13) stay as the installer wrote them and only seed the
// verb's entry when it has none. `dest` and `staged` are read either way: the installer can stage a new generation
// behind a verb-managed entry. The live path's own meta file settles disagreements (see reconcileLive).
func (m *Manifest) adoptInstaller(name string) {
	var keys []string
	recs := map[string]installerRecord{}
	for key, raw := range m.legacy {
		if !isInstallerKey(name, key) {
			continue
		}
		var r installerRecord
		if json.Unmarshal(raw, &r) != nil || r.Version == "" {
			continue // a schema 1 bare string, or something else: read by probing the live build instead
		}
		keys = append(keys, key)
		recs[key] = r
	}
	if len(keys) == 0 {
		return
	}
	sort.Slice(keys, func(i, j int) bool { // the component's own key first, then by name
		if (keys[i] == name) != (keys[j] == name) {
			return keys[i] == name
		}
		return keys[i] < keys[j]
	})
	st := m.comps[name]
	fresh := st == nil
	if fresh {
		r := recs[keys[0]]
		st = &State{Current: &Generation{ID: r.Version, Digest: r.Digest, Size: r.Size, Source: r.Source, InstalledAt: r.InstalledAt}}
		m.comps[name] = st
	}
	for _, k := range keys {
		r := recs[k]
		if st.Dest == "" {
			st.Dest = r.Dest
		}
		if r.Staged != "" {
			if st.Staged == "" {
				st.Staged = r.Staged
			}
			if k != name {
				st.stagedFrom = append(st.stagedFrom, k)
			}
		}
	}
	if fresh && keys[0] == name {
		delete(m.legacy, name) // rewritten in the verb's shape
	}
}

// clearStagedRecords rewrites the installer records that carried `staged` for a component whose Staged is now empty.
func (m *Manifest) clearStagedRecords() {
	for _, st := range m.comps {
		if st.Staged != "" {
			continue
		}
		for _, k := range st.stagedFrom {
			var obj map[string]json.RawMessage
			if json.Unmarshal(m.legacy[k], &obj) != nil {
				continue
			}
			delete(obj, "staged")
			if b, err := json.Marshal(obj); err == nil {
				m.legacy[k] = b
			}
		}
		st.stagedFrom = nil
	}
}

func hasKey(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// State returns the component's state, creating an empty one on first use.
func (m *Manifest) State(name string) *State {
	if s, ok := m.comps[name]; ok {
		return s
	}
	s := &State{}
	m.comps[name] = s
	return s
}

// Has reports whether a schema 2 state exists for the component.
func (m *Manifest) Has(name string) bool { _, ok := m.comps[name]; return ok }

// Backend is the serving backend the installer recorded ("cuda", "vulkan", "cpu", ...), "" when unknown.
func (m *Manifest) Backend() string {
	var s string
	_ = json.Unmarshal(m.top["backend"], &s)
	return s
}

// Save writes the manifest atomically (tmp + rename), schema 2.
func (m *Manifest) Save() error {
	top := map[string]json.RawMessage{}
	for k, v := range m.top {
		top[k] = v
	}
	top["schema"] = json.RawMessage("2")
	m.clearStagedRecords()
	comps := map[string]json.RawMessage{}
	for k, v := range m.legacy {
		comps[k] = v
	}
	for k, v := range m.comps {
		if v.Current == nil && v.Previous == nil && len(v.Rejected) == 0 && v.ETag == "" && v.CheckedAt == "" {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		comps[k] = b
	}
	cb, err := json.Marshal(comps)
	if err != nil {
		return err
	}
	top["components"] = cb
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", m.Path, os.Getpid())
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func nowStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
