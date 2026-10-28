// Package update is the `local-offload update` verb: it resolves each harness component's newest complete build
// from its publisher, verifies the publisher's digest, stages it, promotes it as a write-once generation behind
// the component's existing live path, keeps the previous generation, rolls back on any failure and records
// everything in installed.json (schema 2). Run with nothing new upstream it is one conditional request.
//
// The engine builds on internal/nodeswap for the single-file component (llama-swap): hash verify, idle wait, backup
// by rename, restart, verify, automatic rollback. Directory components (llama, whisper, sdcpp, hyperframes) get
// the generation layout in generations.go. Docs: docs/systems/update.md.
package update

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/floors"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// Options is one invocation of the verb.
type Options struct {
	Components  []string // already expanded ("all" -> AllComponents)
	Channel     string   // "", "nightly" or "release"
	DryRun      bool
	ResolveOnly bool
	Rollback    bool // roll the (single) component back to its previous generation
	Live        string
	// Explicit is true when the operator named the component (--component X): only then may the verb install a
	// component the node does not have. "all" updates what the node runs and installs nothing.
	Explicit bool
}

// Env is everything the verb needs from the node. NewEnv builds the real one.
type Env struct {
	Home     string
	Config   FileConfig
	Endpoint string // the node's llama-swap, "" when none is configured
	Floors   floors.Set
	HTTP     *http.Client
	GitHub   string
	NPM      string
	Gate     Gate
	Deps     nodeswap.Deps
	Out      io.Writer
	Logf     func(format string, args ...any)
	Now      func() time.Time

	restart map[string]RestartSpec // the restart mechanism prepareRestart settled on, per component
}

// NewEnv builds the real environment: publisher hosts reached through the public-only dial guard, the default
// smoke gate (OfflineGate when update.gate.enabled is false), nodeswap's real deps.
func NewEnv(home string, cfg FileConfig, endpoint string, floorsJSON []byte, out io.Writer, logf func(string, ...any)) (*Env, error) {
	fl, err := floors.Parse(floorsJSON)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: netguard.PublicTransport(nil)}
	env := &Env{Home: home, Config: cfg, Endpoint: endpoint, Floors: fl, HTTP: hc,
		GitHub: "https://api.github.com", NPM: "https://registry.npmjs.org",
		Gate: OfflineGate{}, Deps: nodeswap.DefaultDeps(), Out: out, Logf: logf, Now: time.Now}
	if cfg.Gate.On() {
		// The smoke gate measures llama only; every other component passes it untouched.
		self, _ := os.Executable()
		env.Gate = &SmokeGate{Self: self, YAML: env.yamlPath(), Cfg: cfg.Gate, Logf: logf}
	}
	return env, nil
}

// Report is one component's result.
type Report struct {
	Component string     `json:"component"`
	Installed string     `json:"installed"`
	Resolved  string     `json:"resolved,omitempty"`
	Action    string     `json:"action"`
	Detail    string     `json:"detail,omitempty"`
	Previous  string     `json:"previous,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
	Notes     []string   `json:"notes,omitempty"`
	Live      string     `json:"live,omitempty"`
	exit      int
}

// Exit codes: 0 no-op, updated, skipped or inconclusive; 1 rejected or rolled back; 2 error.
const (
	ExitOK       = 0
	ExitRejected = 1
	ExitError    = 2
)

func (e *Env) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

// livePath is where a component's live build is: --live (one component), config.json update.paths, the live path
// installed.json recorded the last time the verb worked on the component, else <home>/<component>.
func (e *Env) livePath(def *compDef, st *State, opts Options) string {
	if opts.Live != "" && len(opts.Components) == 1 {
		return filepath.Clean(opts.Live)
	}
	if p := e.Config.Paths[def.Name]; p != "" {
		return filepath.Clean(p)
	}
	if st != nil {
		if st.Live != "" {
			return filepath.Clean(st.Live)
		}
		// A record written before `live` existed: a single-file component in the file layout recorded the live
		// file itself as its current generation (a generation directory is never a live path).
		if def.Single && st.Current != nil && st.Current.Path != "" {
			if fi, err := os.Stat(st.Current.Path); err == nil && !fi.IsDir() {
				return filepath.Clean(st.Current.Path)
			}
		}
	}
	return filepath.Join(e.Home, def.liveRel())
}

// LivePath is where a component's live build is (the --live flag, config.json update.paths, the recorded live path,
// else <home>/<component>).
func (e *Env) LivePath(name string, opts Options) string {
	def := defs[name]
	if def == nil {
		return ""
	}
	var st *State
	if man, err := LoadManifest(filepath.Join(e.Home, "installed.json")); err == nil {
		st = man.State(name)
	}
	return e.livePath(def, st, opts)
}

func (e *Env) yamlPath() string {
	if e.Config.LlamaSwapYAML != "" {
		return e.Config.LlamaSwapYAML
	}
	return filepath.Join(e.Home, "llama-swap.yaml")
}

func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cmpID compares two upstream ids; an "unknown-<date>" id (a build nobody could name) is older than any real one.
func cmpID(a, b string) int {
	ua, ub := strings.HasPrefix(a, "unknown"), strings.HasPrefix(b, "unknown")
	switch {
	case ua && ub:
		return 0
	case ua:
		return -1
	case ub:
		return 1
	}
	return floors.Compare(a, b)
}

// Run walks the components. It returns every report and the process exit code.
func (e *Env) Run(ctx context.Context, opts Options) ([]Report, int) {
	manPath := filepath.Join(e.Home, "installed.json")
	man, err := LoadManifest(manPath)
	if err != nil {
		return []Report{{Component: "-", Action: "error", Detail: err.Error()}}, ExitError
	}
	channel := opts.Channel
	if channel == "" {
		channel = e.Config.Channel
	}
	if channel != "release" {
		channel = "nightly"
	}
	mutating := !opts.DryRun && !opts.ResolveOnly
	var lock *Lock
	if mutating {
		l, busy, holder, lerr := AcquireLock(e.Home)
		if lerr != nil {
			return []Report{{Component: "-", Action: "error", Detail: "update.lock: " + lerr.Error()}}, ExitError
		}
		if busy {
			return []Report{{Component: "-", Action: "busy", Detail: "another update holds " + filepath.Join(e.Home, "update.lock") + " (" + holder + "); nothing to do"}}, ExitOK
		}
		lock = l
		defer lock.Release()
	}
	rs := &Resolver{HTTP: e.HTTP, GitHub: e.GitHub, NPM: e.NPM, Token: GitHubToken(), Floors: e.Floors,
		Channel: channel, Log: e.logf}
	variant := "" // detected on first need: llama-swap and npm components have none, and a no-op should not start nvidia-smi

	var reports []Report
	exit := ExitOK
	for _, name := range opts.Components {
		def := defs[name]
		if def == nil {
			reports = append(reports, Report{Component: name, Action: "error", Detail: "unknown component", exit: ExitError})
			exit = max(exit, ExitError)
			continue
		}
		if def.Name != LlamaSwap && def.Rule != ruleNPM {
			if variant == "" {
				variant = DetectVariant(man.Backend())
			}
			rs.Variant = variant
		}
		var rep Report
		switch {
		case opts.ResolveOnly:
			rep = e.resolveOnly(ctx, def, rs, man)
		case opts.Rollback:
			rep = e.rollback(ctx, def, man, opts)
		default:
			rep = e.one(ctx, def, rs, man, opts)
		}
		reports = append(reports, rep)
		exit = max(exit, rep.exit)
		if mutating && rep.Action != "unsupported" && rep.Action != "not-installed" {
			if serr := man.Save(); serr != nil {
				reports = append(reports, Report{Component: name, Action: "error", Detail: "writing installed.json: " + serr.Error(), exit: ExitError})
				exit = max(exit, ExitError)
			}
		}
	}
	return reports, exit
}

func (e *Env) resolveOnly(ctx context.Context, def *compDef, rs *Resolver, man *Manifest) Report {
	rep := Report{Component: def.Name, Installed: installedID(man.State(def.Name)), Action: "resolve"}
	c, _, _, err := rs.Resolve(ctx, def, "")
	if err != nil {
		return failReport(rep, err)
	}
	rep.Resolved, rep.Candidate = c.ID, c
	return rep
}

func failReport(rep Report, err error) Report {
	switch {
	case errors.Is(err, ErrUnsupported):
		rep.Action, rep.Detail, rep.exit = "unsupported", err.Error(), ExitOK
	case errors.Is(err, ErrBelowFloor):
		rep.Action, rep.Detail, rep.exit = "error", err.Error(), ExitError
	default:
		rep.Action, rep.Detail, rep.exit = "error", err.Error(), ExitError
	}
	return rep
}

func installedID(st *State) string {
	if st.Current == nil {
		return "none"
	}
	return st.Current.ID
}

// one is resolve -> floor -> compare -> (stage -> promote) for one component.
func (e *Env) one(ctx context.Context, def *compDef, rs *Resolver, man *Manifest, opts Options) Report {
	st := man.State(def.Name)
	live := e.livePath(def, st, opts)
	rep := Report{Component: def.Name, Live: live}
	// Updates move what a node runs. A component with no record and nothing at its live path is installed only when
	// the operator names it: "all" must never put an engine on a node that does not use one.
	if st.Current == nil && !opts.Explicit {
		if _, err := os.Stat(live); err != nil {
			rep.Installed, rep.Action, rep.exit = "none", "not-installed", ExitOK
			rep.Detail = fmt.Sprintf("not on this node (no record, nothing at %s); `update --component %s` installs it, update.paths.%s points at a build elsewhere", live, def.Name, def.Name)
			return rep
		}
	}
	if def.Single {
		if _, err := os.Stat(live); err != nil {
			rep.Installed, rep.Action, rep.exit = "none", "error", ExitError
			rep.Detail = fmt.Sprintf("%s is not there; point update.paths.%s in config.json (or --live) at the live file", live, def.Name)
			return rep
		}
	}
	// The live path's own generation settles what is installed (the installer's meta file beside it).
	e.reconcileLive(def, st, live)
	// Bootstrap: a box with no manifest entry is read from the live build itself.
	if st.Current == nil {
		if _, err := os.Stat(live); err == nil {
			id, ok, detail := Probe(ctx, def, liveProbeDir(def, live))
			if !ok {
				e.logf("%s: the live build did not answer its version probe (%s); recorded as unknown", def.Name, detail)
			}
			if id == "" {
				id = "unknown-" + e.Now().Format("20060102")
			}
			st.Current = &Generation{ID: id, Path: live, Source: "probed", InstalledAt: nowStamp(e.Now())}
		}
	}
	if _, err := os.Stat(live); err == nil {
		st.Live = live // the next run finds the component here with no flag and no config
	}
	rep.Installed = installedID(st)
	if st.Previous != nil {
		rep.Previous = st.Previous.ID
	}
	st.Floor = e.Floors.Floor(def.Upstream)

	// A conditional request is allowed only when what is installed already clears the floor and the last decision was final.
	etag := st.ETag
	if opts.DryRun || st.Current == nil || st.Staged != "" {
		etag = "" // a generation the installer staged waits for a decision, whatever the publisher says
	} else if below, _ := e.Floors.Below(def.Upstream, st.Current.ID); below {
		etag = ""
	}
	cand, newETag, notModified, err := rs.Resolve(ctx, def, etag)
	if err != nil {
		return failReport(rep, err)
	}
	st.CheckedAt = nowStamp(e.Now())
	if notModified {
		rep.Action, rep.Resolved, rep.Detail = "none", rep.Installed, "no change (304)"
		return rep
	}
	rep.Resolved, rep.Candidate, rep.Notes = cand.ID, cand, cand.Notes
	switch {
	case st.isRejected(cand.ID):
		st.ETag = newETag
		rep.Action, rep.Detail = "none", "candidate "+cand.ID+" was rejected before ("+rejectReason(st, cand.ID)+"); waiting for a newer build"
		return rep
	case st.Current != nil && cmpID(cand.ID, st.Current.ID) == 0:
		st.ETag = newETag
		st.Staged = "" // the newest build is what the live path serves; whatever the installer staged is older
		rep.Action, rep.Detail = "none", "up to date"
		return rep
	case st.Current != nil && cmpID(cand.ID, st.Current.ID) < 0:
		st.ETag = newETag
		rep.Action, rep.Detail = "none", "the installed build is newer than the newest published one; never downgraded"
		return rep
	}
	verb := "update"
	if st.Current == nil {
		verb = "install"
	}
	pre, staleNote := e.stagedGen(def, st, cand, live)
	if staleNote != "" {
		rep.Notes = append(rep.Notes, staleNote)
	}
	if def.Single {
		rep.Notes = append(rep.Notes, "layout: "+e.layoutOf(def, live).Why)
	}
	if opts.DryRun {
		rep.Action = verb
		return rep
	}
	var res result
	if def.Single {
		res = e.applySingle(ctx, def, st, cand, live, pre)
	} else {
		res = e.applyDir(ctx, def, st, cand, live, pre)
	}
	rep.Action, rep.Detail, rep.exit = res.action, res.detail, res.exit
	rep.Notes = append(rep.Notes, res.notes...)
	switch res.action {
	case "updated", "installed", "rejected", "rolled-back":
		st.ETag = newETag // the decision for this tag is final
		if res.action == "updated" || res.action == "installed" {
			st.Staged = "" // promoted (or superseded by a newer build): the staged generation is current or pruned
		}
	default:
		st.ETag = ""
	}
	if st.Current != nil {
		rep.Installed = verbose(st.Current.ID, rep.Installed)
	}
	if st.Previous != nil {
		rep.Previous = st.Previous.ID
	}
	return rep
}

func verbose(now, was string) string {
	if now == was {
		return now
	}
	return was + " -> " + now
}

func rejectReason(st *State, id string) string {
	for _, r := range st.Rejected {
		if r.ID == id {
			return truncate(r.Reason, 120)
		}
	}
	return ""
}

// liveProbeDir is the directory Probe runs in: the live directory, or the folder of a live file.
func liveProbeDir(def *compDef, live string) string {
	if def.Single {
		return filepath.Dir(live)
	}
	return live
}

// result is what applying one candidate came to.
type result struct {
	action string
	detail string
	exit   int
	notes  []string
}

// attestationNote is the report line for a fetched build's attestation; "" when nothing was fetched.
func attestationNote(fe *fetched) string {
	if fe == nil || fe.attestation == "" {
		return ""
	}
	return "attestation: " + fe.attestation
}

// attestationRatchet compares the candidate's attestation with the generation now serving: a publisher whose last
// build was verified does not silently become unattested. It returns reject when the candidate's publisher no
// longer publishes one ("none published": a substituted asset reads exactly like that), inconclusive when the check
// could not run this time (gh missing or signed out), and "" when the candidate is as attested as the current build.
func (e *Env) attestationRatchet(st *State, vroot string, fe *fetched) (reject, inconclusive string) {
	if fe == nil || st == nil || st.Current == nil || fe.attestation == "verified" {
		return "", ""
	}
	if !strings.HasPrefix(fe.attestation, "none published") && !strings.HasPrefix(fe.attestation, "skipped") {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(vroot, st.Current.ID+".meta.json"))
	if err != nil {
		return "", ""
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var m struct {
		Attestation string `json:"attestation"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Attestation != "verified" {
		return "", ""
	}
	why := fmt.Sprintf("the serving build %s was attested by its publisher and this one is not (%s)", st.Current.ID, fe.attestation)
	if strings.HasPrefix(fe.attestation, "skipped") {
		return "", why + "; sign gh in on this node and retry"
	}
	return why, ""
}

func (e *Env) reject(st *State, id, reason string) {
	st.Rejected = append(st.Rejected, Rejected{ID: id, Reason: reason, TS: nowStamp(e.Now())})
	if len(st.Rejected) > 20 {
		st.Rejected = st.Rejected[len(st.Rejected)-20:]
	}
}

// meta is <id>.meta.json beside a generation. tag, digests and sizes are the installer's own meta fields (the same
// file layout): the installer reads the live generation's tag as the truth of what is installed, and reuses a
// generation whose digests match the publisher's without a download.
type meta struct {
	ID          string             `json:"id"`
	Tag         string             `json:"tag,omitempty"`
	Digests     []string           `json:"digests,omitempty"`
	Sizes       []int64            `json:"sizes,omitempty"`
	Source      string             `json:"source"`
	Assets      []Asset            `json:"assets"`
	Attestation string             `json:"attestation,omitempty"`
	Size        int64              `json:"size"`
	InstalledAt string             `json:"installed_at"`
	Checks      []string           `json:"checks,omitempty"`
	Gate        string             `json:"gate,omitempty"`
	GateNumbers map[string]float64 `json:"gate_numbers,omitempty"`
}

func writeMeta(vroot string, m meta) {
	if m.Tag == "" {
		m.Tag = m.ID
	}
	if m.Digests == nil {
		for _, a := range m.Assets {
			m.Digests = append(m.Digests, a.Digest)
			m.Sizes = append(m.Sizes, a.Size)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(vroot, m.ID+".meta.json"), append(b, '\n'), 0o644)
	}
}

// fetched is a candidate downloaded, verified and (for directories) extracted.
type fetched struct {
	files       []string
	size        int64
	attestation string
	notes       []string
}

// fetch downloads every asset of the candidate into dl, verifies each against the publisher's digest, and runs the
// attestation check. A mismatch is an error and the bytes are gone.
func (e *Env) fetch(ctx context.Context, cand *Candidate, dl string) (*fetched, error) {
	f := &fetched{}
	var sums map[string]string
	for _, a := range cand.Assets {
		e.logf("  fetching %s (%d MiB)", a.Name, a.Size>>20)
		p, n, err := Download(ctx, e.HTTP, a, dl, e.logf)
		if err != nil {
			return nil, err
		}
		f.files, f.size = append(f.files, p), f.size+n
		if sums == nil {
			sums = e.checksums(ctx, cand)
		}
		if want, ok := sums[a.Name]; ok && !strings.EqualFold(want, strings.TrimPrefix(a.Digest, "sha256:")) {
			return nil, fmt.Errorf("%s: the publisher's checksums file says %s but its API digest says %s", a.Name, want, strings.TrimPrefix(a.Digest, "sha256:"))
		}
		att, aerr := Attest(ctx, cand.Source, p)
		if aerr != nil {
			return nil, aerr
		}
		f.attestation = att
	}
	return f, nil
}

// checksums reads the candidate's checksums asset (llama-swap publishes one) as name -> sha256; empty when none.
func (e *Env) checksums(ctx context.Context, cand *Candidate) map[string]string {
	out := map[string]string{}
	if cand.Checksums == nil {
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cand.Checksums.URL, nil)
	if err != nil {
		return out
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, line := range strings.Split(string(b), "\n") {
		fs := strings.Fields(line)
		if len(fs) == 2 && len(fs[0]) == 64 {
			out[strings.TrimPrefix(fs[1], "*")] = fs[0]
		}
	}
	return out
}
