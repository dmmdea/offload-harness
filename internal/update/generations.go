package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// Directory components keep generations beside the live path:
//
//	<live>                       a junction (Windows) or symlink (Linux) to <live>-versions/<id>
//	<live>-versions/<id>/        write-once after promotion
//	<live>-versions/.staging/    download + extract + self-check, deleted on every outcome
//	<live>-versions/<id>.meta.json

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// hosted components are run by llama-swap, so a promotion also has to get them reloaded.
func hosted(def *compDef) bool { return def.Name == Llama || def.Name == Whisper }

// unloadHosted asks llama-swap to unload (with drain) the models that run from live, so the next load uses the
// build the live path points at now; why names that build in the note ("the new build", "the previous build"). A
// keep-set seat is left alone and named. No llama-swap answering means nothing is running from it.
func (e *Env) unloadHosted(ctx context.Context, def *compDef, live, why string) ([]string, error) {
	if !hosted(def) || e.Endpoint == "" {
		return nil, nil
	}
	un, left, err := Consumer{Endpoint: e.Endpoint}.UnloadOn(ctx, live, false, 5*time.Minute)
	if err != nil {
		if errors.Is(err, llamaswap.ErrUnreachable) {
			return nil, nil
		}
		return nil, err
	}
	var notes []string
	if len(un) > 0 {
		notes = append(notes, "unloaded for "+why+": "+strings.Join(un, ", "))
	}
	if len(left) > 0 {
		notes = append(notes, "keep-set seat(s) left on the build they loaded with until their next load: "+strings.Join(left, ", "))
	}
	return notes, nil
}

func (e *Env) cleanStaging(vroot string) {
	_ = os.RemoveAll(filepath.Join(vroot, ".staging"))
	_ = os.MkdirAll(filepath.Join(vroot, ".staging"), 0o755)
}

func (e *Env) applyDir(ctx context.Context, def *compDef, st *State, cand *Candidate, live string, pre *preGen) result {
	vroot := versionsRoot(live, false)
	if err := os.MkdirAll(vroot, 0o755); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	e.cleanStaging(vroot)
	stage := filepath.Join(vroot, ".staging", cand.ID+"."+randHex())
	gen := filepath.Join(stage, "gen")
	defer func() { _ = os.RemoveAll(stage) }()

	var fe *fetched
	var err error
	if pre != nil {
		// The installer staged this generation behind a running engine and its meta file carries the publisher's digests.
		e.logf("%s: promoting %s, staged by the installer (no download)", def.Name, cand.ID)
		fe = &fetched{size: pre.Size, attestation: "the generation's meta file carries the publisher's digests (verified by the installer)"}
		gen = pre.Dir
	} else if def.Rule == ruleNPM {
		e.logf("%s: staging %s", def.Name, cand.ID)
		fe = &fetched{attestation: "npm registry integrity (verified by npm)"}
		err = e.stageNPM(ctx, def, live, cand, gen)
		if err != nil {
			return result{"error", "stage: " + err.Error(), ExitError, nil}
		}
	} else {
		e.logf("%s: staging %s", def.Name, cand.ID)
		fe, err = e.fetch(ctx, cand, filepath.Join(stage, "dl"))
		if err != nil {
			return result{"error", "download or digest check failed, nothing installed: " + err.Error(), ExitError, nil}
		}
		if err = e.extractAll(fe.files, gen, stage); err != nil {
			return e.rejected(st, cand, "extract: "+err.Error())
		}
	}
	notes, err := SelfCheck(ctx, def, gen, e.yamlPath(), live)
	if err == nil && def.Rule == ruleNPM {
		if id, _, _ := Probe(ctx, def, gen); id != cand.ID {
			err = fmt.Errorf("self-check: the staged package is %q, not %s", id, cand.ID)
		}
	}
	if err != nil {
		return e.rejected(st, cand, err.Error())
	}
	var prevDir string
	if st.Current != nil {
		prevDir = st.Current.Path
	}
	g := e.Gate.Run(ctx, GateInput{Component: def.Name, Candidate: cand, StagedDir: gen, PreviousDir: prevDir, Live: live})
	switch g.Verdict {
	case GateReject:
		return e.rejected(st, cand, "gate: "+g.Reason)
	case GateInconclusive:
		return result{"inconclusive", "gate could not decide, nothing changed; will retry: " + g.Reason, ExitOK, notes}
	}
	notes = append(notes, gateNotes(e.Gate.Name(), g)...)

	// A live path that is a real directory is migrated into the layout first.
	oldTarget, isLink := readLink(live)
	if !isLink && exists(live) {
		if un, uerr := e.unloadHosted(ctx, def, live, "the migration of "+live); uerr != nil {
			return result{"inconclusive", "a seat on " + live + " is busy or could not be unloaded; nothing changed: " + uerr.Error(), ExitOK, notes}
		} else {
			notes = append(notes, un...)
		}
		dir, id, merr := e.migrate(ctx, def, st, live, vroot)
		if merr != nil {
			return result{"error", "migrating " + live + " into " + vroot + ": " + merr.Error(), ExitError, notes}
		}
		oldTarget = dir
		notes = append(notes, "migrated the real directory "+live+" to "+dir+" ("+id+")")
	}
	if st.Current != nil && oldTarget != "" && (st.Current.Path == live || st.Current.Path == "") {
		st.Current.Path = oldTarget
	}

	finalDir := filepath.Join(vroot, cand.ID)
	if pre != nil {
		finalDir = pre.Dir // already in place, write-once
	} else {
		if exists(finalDir) {
			if finalDir == oldTarget {
				return result{"error", finalDir + " already is the live generation", ExitError, notes}
			}
			_ = os.RemoveAll(finalDir)
		}
		if err := os.Rename(gen, finalDir); err != nil {
			return result{"error", "promoting the staged build: " + err.Error(), ExitError, notes}
		}
	}
	now := e.Now()
	asset := make([]string, 0, len(cand.Assets))
	for _, a := range cand.Assets {
		asset = append(asset, a.Name)
	}
	if pre == nil { // a staged generation keeps the meta file its installer wrote
		writeMeta(vroot, meta{ID: cand.ID, Source: cand.Source, Assets: cand.Assets, Attestation: fe.attestation, Size: fe.size,
			InstalledAt: nowStamp(now), Checks: notes, Gate: e.Gate.Name(), GateNumbers: g.Numbers})
	}

	back := func(reason string) result {
		out := result{"rolled-back", reason, ExitRejected, notes}
		if oldTarget != "" {
			if ferr := flipLink(live, oldTarget); ferr != nil {
				out.action, out.exit = "error", ExitError
				out.detail = reason + "; re-pointing " + live + " at the previous build ALSO failed: " + ferr.Error()
				return out
			}
		} else {
			_ = os.Remove(live)
		}
		_, _ = e.unloadHosted(ctx, def, live, "the previous build") // before the delete: a seat that loaded the new build holds its files
		_ = os.RemoveAll(finalDir)
		_ = os.Remove(filepath.Join(vroot, filepath.Base(finalDir)+".meta.json"))
		e.reject(st, cand.ID, reason)
		return out
	}

	if ferr := flipLink(live, finalDir); ferr != nil {
		return result{"error", "flipping " + live + ": " + ferr.Error(), ExitError, notes}
	}
	un, uerr := e.unloadHosted(ctx, def, live, "the new build")
	if uerr != nil {
		return back("reloading the consumer failed after the flip: " + uerr.Error())
	}
	notes = append(notes, un...)
	if id, ok, detail := Probe(ctx, def, live); !ok {
		return back("the live path does not start after the flip: " + detail)
	} else if id != "" && def.IDProbe != nil && cmpID(id, cand.ID) != 0 {
		return back(fmt.Sprintf("the live path reports %s after the flip, expected %s", id, cand.ID))
	}
	if def.Name == Llama && e.Config.Gate.On() {
		wnote, werr := e.WatchFirstLoad(ctx, live)
		if werr != nil {
			return back("post-flip load failed: " + werr.Error())
		}
		if wnote != "" {
			notes = append(notes, wnote)
		}
	}

	newGen := &Generation{ID: cand.ID, Path: finalDir, Digest: cand.Assets[0].Digest, Size: fe.size, Source: cand.Source,
		InstalledAt: nowStamp(now), Asset: strings.Join(asset, ",")}
	if st.Current != nil && oldTarget != "" {
		st.Previous = st.Current
	}
	st.Current = newGen
	e.prune(vroot, st)
	verb := "updated"
	if st.Previous == nil {
		verb = "installed"
	}
	return result{verb, fmt.Sprintf("%s now points at %s", live, finalDir), ExitOK, notes}
}

func (e *Env) rejected(st *State, cand *Candidate, reason string) result {
	e.reject(st, cand.ID, reason)
	return result{"rejected", reason, ExitRejected, nil}
}

// migrate renames a real live directory into <vroot>/<id> and puts the junction or symlink where it was.
func (e *Env) migrate(ctx context.Context, def *compDef, st *State, live, vroot string) (dir, id string, err error) {
	if st.Current != nil && !strings.HasPrefix(st.Current.ID, "unknown") {
		id = st.Current.ID
	}
	if id == "" {
		if pid, ok, _ := Probe(ctx, def, live); ok && pid != "" {
			id = pid
		}
	}
	if id == "" {
		id = "unknown-" + e.Now().Format("20060102")
	}
	dir = filepath.Join(vroot, id)
	if exists(dir) {
		dir += "-migrated-" + randHex()
	}
	var rerr error
	for i := 0; i < 3; i++ {
		if rerr = os.Rename(live, dir); rerr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if rerr != nil {
		return "", "", fmt.Errorf("%w (is something still running from it?)", rerr)
	}
	if lerr := makeLink(live, dir); lerr != nil {
		if back := os.Rename(dir, live); back != nil {
			return "", "", fmt.Errorf("%v; and moving it back failed: %v", lerr, back)
		}
		return "", "", lerr
	}
	writeMeta(vroot, meta{ID: id, Source: "migrated from the live directory", InstalledAt: nowStamp(e.Now())})
	return dir, id, nil
}

// prune deletes every generation that is neither current nor previous, and the staging area.
func (e *Env) prune(vroot string, st *State) {
	keep := map[string]bool{".staging": true}
	for _, g := range []*Generation{st.Current, st.Previous} {
		if g != nil && filepath.Dir(g.Path) == filepath.Clean(vroot) {
			keep[filepath.Base(g.Path)] = true
			keep[g.ID+".meta.json"] = true
			keep[filepath.Base(g.Path)+".meta.json"] = true
		}
	}
	ents, err := os.ReadDir(vroot)
	if err != nil {
		return
	}
	for _, en := range ents {
		if keep[en.Name()] {
			continue
		}
		if !en.IsDir() && !strings.HasSuffix(en.Name(), ".meta.json") {
			continue // never delete a file this package did not make
		}
		if rerr := os.RemoveAll(filepath.Join(vroot, en.Name())); rerr != nil {
			e.logf("prune: %s is in use and stays until the next update (%v)", en.Name(), rerr)
		}
	}
	_ = os.RemoveAll(filepath.Join(vroot, ".staging"))
}

// extractAll unpacks each archive on its own, lifts a single wrapper directory, and merges them into gen.
func (e *Env) extractAll(files []string, gen, stage string) error {
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return err
	}
	for i, f := range files {
		tmp := filepath.Join(stage, fmt.Sprintf("x%d", i))
		if err := Extract(f, tmp); err != nil {
			return err
		}
		if err := hoistSingleDir(tmp); err != nil {
			return err
		}
		if err := mergeInto(tmp, gen); err != nil {
			return err
		}
	}
	return nil
}

func mergeInto(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, en := range ents {
		s, d := filepath.Join(src, en.Name()), filepath.Join(dst, en.Name())
		if di, derr := os.Lstat(d); derr == nil {
			if di.IsDir() && en.IsDir() {
				if err := mergeInto(s, d); err != nil {
					return err
				}
				continue
			}
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
		if err := os.Rename(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(t, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cerr := io.Copy(out, in)
		if e := out.Close(); cerr == nil {
			cerr = e
		}
		return cerr
	})
}

// stageNPM builds a hyperframes generation: the live package.json with the dependency set to the resolved version,
// npm install without install scripts (npm verifies the registry's integrity), and the live browser cache carried over.
func (e *Env) stageNPM(ctx context.Context, def *compDef, live string, cand *Candidate, gen string) error {
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return err
	}
	pj := map[string]any{"name": "offload-harness-" + def.Name, "version": "0.0.0", "private": true}
	if b, err := os.ReadFile(filepath.Join(live, "package.json")); err == nil {
		_ = json.Unmarshal(b, &pj)
	}
	deps, _ := pj["dependencies"].(map[string]any)
	if deps == nil {
		deps = map[string]any{}
	}
	deps[def.Repo] = cand.ID
	pj["dependencies"] = deps
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(gen, "package.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return errors.New("npm is not on PATH")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := winexec.CommandContext(cctx, npm, "install", "--ignore-scripts", "--no-audit", "--no-fund", "--no-package-lock")
	cmd.Dir = gen
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install: %v: %s", err, truncate(strings.TrimSpace(string(out)), 300))
	}
	if st, err := os.Stat(filepath.Join(live, "home")); err == nil && st.IsDir() {
		if err := copyTree(filepath.Join(live, "home"), filepath.Join(gen, "home")); err != nil {
			return fmt.Errorf("carrying the browser cache over: %w", err)
		}
	}
	return nil
}

// rollback returns a component to its previous generation, on the operator's word (not a failure: nothing is rejected).
func (e *Env) rollback(ctx context.Context, def *compDef, man *Manifest, opts Options) Report {
	st := man.State(def.Name)
	live := e.livePath(def, opts)
	rep := Report{Component: def.Name, Live: live, Installed: installedID(st), Action: "rollback"}
	if st.Previous == nil {
		rep.Action, rep.Detail, rep.exit = "error", "no previous generation is recorded; nothing to roll back to", ExitError
		return rep
	}
	rep.Previous = st.Previous.ID
	var res result
	if def.Single {
		res = e.rollbackSingle(ctx, def, st, live)
	} else {
		res = e.rollbackDir(ctx, def, st, live)
	}
	rep.Action, rep.Detail, rep.exit, rep.Notes = res.action, res.detail, res.exit, res.notes
	if st.Current != nil {
		rep.Installed = verbose(st.Current.ID, rep.Installed)
	}
	if st.Previous != nil {
		rep.Previous = st.Previous.ID
	}
	return rep
}

func (e *Env) rollbackDir(ctx context.Context, def *compDef, st *State, live string) result {
	prev, cur := st.Previous, st.Current
	if !exists(prev.Path) {
		return result{"error", "the previous generation " + prev.Path + " is gone", ExitError, nil}
	}
	oldTarget, _ := readLink(live)
	if err := flipLink(live, prev.Path); err != nil {
		return result{"error", err.Error(), ExitError, nil}
	}
	notes, uerr := e.unloadHosted(ctx, def, live, "the previous build")
	if uerr != nil {
		notes = append(notes, "reload: "+uerr.Error())
	}
	if _, ok, detail := Probe(ctx, def, live); !ok {
		if oldTarget != "" {
			_ = flipLink(live, oldTarget)
		}
		return result{"error", "the previous generation does not start (" + detail + "); left on the current one", ExitError, notes}
	}
	if cur != nil && oldTarget != "" {
		cur.Path = oldTarget
	}
	st.Current, st.Previous, st.ETag = prev, cur, ""
	return result{"rollback", fmt.Sprintf("%s now points at %s", live, prev.Path), ExitOK, notes}
}

// gateNotes is the gate's line for the table and, when it measured anything, its numbers.
func gateNotes(name string, g GateResult) []string {
	out := []string{"gate: " + name + ": " + g.Reason}
	if len(g.Numbers) > 0 {
		keys := make([]string, 0, len(g.Numbers))
		for k := range g.Numbers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%g", k, g.Numbers[k]))
		}
		out = append(out, "gate numbers: "+strings.Join(parts, " "))
	}
	return out
}
