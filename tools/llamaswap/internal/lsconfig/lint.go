// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severity orders lint output and decides the exit code. Only SevError makes
// `config lint` exit non-zero; warnings are findings, not failures, because a
// lint that fails a build on a judgment call stops being run.
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warning"
	SevInfo  Severity = "info"
	// SevSkipped is an HONEST no-op: a check that does not apply to this seat
	// (a llama-server flag check on a whisper seat) or cannot run here (dead
	// model detection without local history). Emitting it is the point —
	// silence would read as a pass.
	SevSkipped Severity = "skipped"
)

// Finding is one lint result.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Model    string   `json:"model,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
	Detail   string   `json:"detail,omitempty"`
	SeatKind SeatKind `json:"seat_kind,omitempty"`
}

// LintReport is the full result set plus the counts callers act on.
type LintReport struct {
	Path          string    `json:"path"`
	Sha256        string    `json:"sha256"`
	SchemaVersion string    `json:"schema_version"`
	Findings      []Finding `json:"findings"`
	Errors        int       `json:"errors"`
	Warnings      int       `json:"warnings"`
	Infos         int       `json:"infos"`
	Skipped       int       `json:"skipped"`
	Models        int       `json:"models"`
	// NonLlamaServerSeats names the seats that took the escape hatch, so the
	// reader can see WHICH seats had checks skipped rather than wondering.
	NonLlamaServerSeats []string `json:"non_llama_server_seats,omitempty"`
}

// LintSchemaVersion versions the --json findings shape.
const LintSchemaVersion = "lint/1"

// LintOptions controls the environment-dependent checks.
type LintOptions struct {
	// CheckListeners enables the LIVE listener probe for hardcoded ports.
	// Off by default in tests; on for the real command.
	CheckListeners bool
	// ListenerProbe reports whether a TCP port on 127.0.0.1 already has a
	// listener. Defaults to a real net.Listen probe.
	ListenerProbe func(port int) (busy bool, err error)
	// Stat resolves referenced model/binary files. Defaults to os.Stat.
	Stat func(string) (os.FileInfo, error)
	// LookupEnv resolves ${env.VAR}. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// HaveLocalHistory tells the dead-model check whether a mirror exists.
	// False today on every deployment: the check reports SKIPPED rather than
	// guessing which seats are unused.
	HaveLocalHistory bool
}

func (o LintOptions) stat() func(string) (os.FileInfo, error) {
	if o.Stat != nil {
		return o.Stat
	}
	return os.Stat
}

func (o LintOptions) probe() func(int) (bool, error) {
	if o.ListenerProbe != nil {
		return o.ListenerProbe
	}
	return probeListener
}

// probeListener reports whether 127.0.0.1:port already has a listener. Bound
// to the loopback IPv4 literal, never "localhost": name resolution can hand
// back ::1 first and stall for seconds before falling back, which has produced
// bogus "port free" answers on this class of box.
func probeListener(port int) (bool, error) {
	d := net.Dialer{Timeout: 400 * time.Millisecond}
	conn, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	return false, nil
}

// Lint runs the semantic check catalog over a parsed config.
//
// Every llama-server-specific check consults SeatKind first. A seat running
// whisper-server (or any other non-llama-server binary) is skipped with an
// explicit SevSkipped note instead of being reported as missing -ngl, missing
// a context size, or pointing at a non-GGUF weights file. That escape hatch is
// load-bearing: one noisy false positive on a legitimate seat and the operator
// stops running lint at all.
func Lint(f *File, opts LintOptions) *LintReport {
	rep := &LintReport{Path: f.Path, Sha256: f.Sha256, SchemaVersion: LintSchemaVersion, Models: len(f.Models)}
	add := func(fd Finding) { rep.Findings = append(rep.Findings, fd) }

	for _, m := range f.Models {
		if m.Seat == SeatNonLlamaServer {
			rep.NonLlamaServerSeats = append(rep.NonLlamaServerSeats, m.ID)
		}
	}

	lintMacros(f, opts, add)
	lintAliases(f, add)
	lintPorts(f, opts, add)
	lintTTL(f, add)
	lintFiles(f, opts, add)
	lintBinaries(f, opts, add)
	lintRouting(f, add)
	lintProfilesSelectors(f, add)
	lintAPIKeys(f, add)
	lintStore(f, add)
	lintDeadModels(f, opts, add)

	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if sevRank(a.Severity) != sevRank(b.Severity) {
			return sevRank(a.Severity) < sevRank(b.Severity)
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Check < b.Check
	})
	for _, fd := range rep.Findings {
		switch fd.Severity {
		case SevError:
			rep.Errors++
		case SevWarn:
			rep.Warnings++
		case SevInfo:
			rep.Infos++
		case SevSkipped:
			rep.Skipped++
		}
	}
	return rep
}

func sevRank(s Severity) int {
	switch s {
	case SevError:
		return 0
	case SevWarn:
		return 1
	case SevInfo:
		return 2
	}
	return 3
}

// ---------------------------------------------------------------- macros

func lintMacros(f *File, opts LintOptions, add func(Finding)) {
	exp := NewExpander(f.Macros, opts.LookupEnv)

	for _, name := range f.MacroOrder {
		if ReservedMacros[name] {
			add(Finding{
				Check: "macro.reserved", Severity: SevError, Line: f.MacroLine[name],
				Message: fmt.Sprintf("macro %q shadows a reserved name — llama-swap substitutes ${PID}, ${PORT} and ${MODEL_ID} itself at spawn time and rejects a config that redefines them", name),
			})
		}
	}
	for _, cyc := range exp.MacroCycles() {
		add(Finding{Check: "macro.cycle", Severity: SevError, Message: "macro cycle: " + cyc})
	}
	for _, e := range f.ExpandErrors {
		add(Finding{Check: "macro.expand", Severity: SevError, Message: e})
	}

	used := map[string]bool{}
	envUnset := map[string][]string{}
	undefined := map[string][]string{}
	var lineOf = map[string]int{}

	consider := func(owner string, line int, s string) {
		for _, ref := range ReferencedMacros(s) {
			if ReservedMacros[ref] {
				continue
			}
			if strings.HasPrefix(ref, EnvMacroPrefix) {
				varName := strings.TrimPrefix(ref, EnvMacroPrefix)
				lookup := opts.LookupEnv
				if lookup == nil {
					lookup = os.LookupEnv
				}
				if _, ok := lookup(varName); !ok {
					envUnset[varName] = append(envUnset[varName], owner)
					if _, seen := lineOf[varName]; !seen {
						lineOf[varName] = line
					}
				}
				continue
			}
			if _, ok := f.Macros[ref]; ok {
				used[ref] = true
				continue
			}
			undefined[ref] = append(undefined[ref], owner)
			if _, seen := lineOf[ref]; !seen {
				lineOf[ref] = line
			}
		}
	}
	// A macro referenced only from another macro still counts as used.
	for _, name := range f.MacroOrder {
		consider("macros."+name, f.MacroLine[name], f.Macros[name])
	}
	for _, m := range f.Models {
		consider(m.ID, m.KeyLine, m.CmdRaw)
		consider(m.ID, m.KeyLine, m.CmdStop)
		consider(m.ID, m.KeyLine, m.Proxy)
		for _, e := range m.Env {
			consider(m.ID, m.KeyLine, e)
		}
	}

	for _, name := range SortedKeys(undefined) {
		add(Finding{
			Check: "macro.undefined", Severity: SevError, Line: lineOf[name],
			Message: fmt.Sprintf("${%s} is referenced by %s but never declared in macros: — llama-swap leaves the literal token in the spawned command line", name, strings.Join(dedupe(undefined[name]), ", ")),
		})
	}
	for _, name := range SortedKeys(envUnset) {
		add(Finding{
			Check: "macro.env-unset", Severity: SevWarn, Line: lineOf[name],
			Message: fmt.Sprintf("${env.%s} is unset in THIS process environment (referenced by %s)", name, strings.Join(dedupe(envUnset[name]), ", ")),
			Detail:  "the llama-swap service may run under a different account (a SYSTEM scheduled task or a systemd unit does not inherit an interactive shell's environment), so this is a warning, not a verdict",
		})
	}
	for _, name := range f.MacroOrder {
		if used[name] || ReservedMacros[name] {
			continue
		}
		add(Finding{
			Check: "macro.unused", Severity: SevWarn, Line: f.MacroLine[name],
			Message: fmt.Sprintf("macro %q is declared but never referenced", name),
		})
	}
}

// ---------------------------------------------------------------- aliases

func lintAliases(f *File, add func(Finding)) {
	owner := map[string][]string{}
	lineOf := map[string]int{}
	for _, m := range f.Models {
		for _, a := range m.Aliases {
			owner[a] = append(owner[a], m.ID)
			if _, ok := lineOf[a]; !ok {
				lineOf[a] = m.KeyLine
			}
		}
	}
	for _, a := range SortedKeys(owner) {
		if len(owner[a]) > 1 {
			add(Finding{
				Check: "alias.duplicate", Severity: SevError, Line: lineOf[a],
				Message: fmt.Sprintf("alias %q is claimed by %d models (%s) — the router resolves it to exactly one and the others become unreachable by that name", a, len(owner[a]), strings.Join(owner[a], ", ")),
			})
			continue
		}
		if m, ok := f.ModelIndex[a]; ok && m.ID != owner[a][0] {
			add(Finding{
				Check: "alias.shadows-id", Severity: SevError, Line: lineOf[a], Model: owner[a][0],
				Message: fmt.Sprintf("alias %q on model %q is the model ID of another seat (line %d) — one of the two is unreachable", a, owner[a][0], m.KeyLine),
			})
		}
	}
}

// ---------------------------------------------------------------- ports

func lintPorts(f *File, opts LintOptions, add func(Finding)) {
	span := f.StartPort
	for _, m := range f.Models {
		spec := ParseCmd(m.CmdExpanded)
		fl, ok := spec.GetAny("--port")
		if !ok || len(fl.Values) == 0 {
			continue
		}
		val := fl.Values[0]
		// ${PORT} is the CORRECT spelling: it tells llama-swap to assign the
		// port from startPort. Never flag it.
		if strings.Contains(val, "${") {
			continue
		}
		port, err := strconv.Atoi(val)
		if err != nil {
			add(Finding{
				Check: "port.unparsable", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("--port %q is neither a number nor ${PORT}", val),
			})
			continue
		}
		add(Finding{
			Check: "port.hardcoded", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
			Message: fmt.Sprintf("seat pins --port %d instead of ${PORT}; llama-swap cannot manage the assignment and two seats pinned to one port silently fight", port),
		})
		if span > 0 && port >= span && port < span+len(f.Models)+16 {
			add(Finding{
				Check: "port.span-collision", Severity: SevError, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("hardcoded --port %d falls inside the startPort span (%d..%d) llama-swap assigns from", port, span, span+len(f.Models)+15),
			})
		}
		if opts.CheckListeners {
			busy, err := opts.probe()(port)
			switch {
			case err != nil:
				add(Finding{
					Check: "port.listener-unknown", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
					Message: fmt.Sprintf("could not probe 127.0.0.1:%d: %v", port, err),
				})
			case busy:
				add(Finding{
					Check: "port.listener-busy", Severity: SevError, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
					Message: fmt.Sprintf("127.0.0.1:%d already has a listener on THIS host — this seat will fail to bind at spawn", port),
				})
			}
		}
	}

	for _, m := range f.Models {
		if m.Proxy == "" || span <= 0 {
			continue
		}
		port, ok := portFromURL(m.Proxy)
		if !ok {
			continue
		}
		if port >= span && port < span+len(f.Models)+16 {
			continue
		}
		add(Finding{
			Check: "port.proxy-outside-span", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
			Message: fmt.Sprintf("proxy: targets port %d, outside the startPort span (%d..%d) — correct for a genuinely external upstream, a bug if this seat is supposed to be llama-swap-managed", port, span, span+len(f.Models)+15),
		})
	}
}

func portFromURL(u string) (int, bool) {
	idx := strings.LastIndex(u, ":")
	if idx < 0 {
		return 0, false
	}
	rest := u[idx+1:]
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 {
		rest = rest[:slash]
	}
	p, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return p, true
}

// ---------------------------------------------------------------- ttl

func lintTTL(f *File, add func(Finding)) {
	globalSet := f.GlobalTTL != nil && *f.GlobalTTL != 0
	for _, m := range f.Models {
		if m.TTL == nil {
			if globalSet {
				continue
			}
			add(Finding{
				Check: "ttl.unset", Severity: SevInfo, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: "no ttl and no globalTTL — this seat stays resident until something evicts it",
			})
			continue
		}
		switch {
		case *m.TTL == 0 && globalSet:
			add(Finding{
				Check: "ttl.zero-ambiguous", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("ttl: 0 with globalTTL: %d set — 0 means \"no TTL, never auto-unload\", NOT \"inherit the global\"; drop the key entirely to inherit", *f.GlobalTTL),
			})
		case *m.TTL == 0:
			add(Finding{
				Check: "ttl.zero", Severity: SevInfo, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: "ttl: 0 means never auto-unload (it is not \"unload immediately\")",
			})
		case *m.TTL < 0:
			add(Finding{
				Check: "ttl.keep-resident", Severity: SevInfo, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("ttl: %d marks this seat keep-resident; note the live API reports ttl:0 for it, so NEVER derive a keep-set from the server's ttl field — read it from the config", *m.TTL),
			})
		}
	}
}

// ---------------------------------------------------------------- files

func lintFiles(f *File, opts LintOptions, add func(Finding)) {
	stat := opts.stat()
	for _, m := range f.Models {
		spec := ParseCmd(m.CmdExpanded)
		flags := FileFlagsFor(m.Seat, m.Binary)
		checked := 0
		for _, fl := range spec.Flags {
			if !containsString(flags, fl.Name) || len(fl.Values) == 0 {
				continue
			}
			path := fl.Values[0]
			if strings.Contains(path, "${") {
				add(Finding{
					Check: "file.unresolved", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
					Message: fmt.Sprintf("%s %s still contains an unexpanded macro; cannot check whether the file exists", fl.Name, path),
				})
				continue
			}
			checked++
			if _, err := stat(path); err != nil {
				add(Finding{
					Check: "file.missing", Severity: SevError, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
					Message: fmt.Sprintf("%s %s does not exist — this seat cannot start", fl.Name, path),
					Detail:  err.Error(),
				})
			}
		}
		if m.Seat != SeatLlamaServer && checked > 0 {
			add(Finding{
				Check: "file.gguf-shape", Severity: SevSkipped, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("GGUF header checks skipped: seat runs %s, not llama-server (its weights are a %s file with a different container format)", BinaryBase(m.Binary), strings.TrimPrefix(filepath.Ext(firstFileValue(spec, flags)), ".")),
			})
		}
		if ctxFlags := ContextFlagsFor(m.Seat); ctxFlags == nil {
			add(Finding{
				Check: "ctx.window", Severity: SevSkipped, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("context-window checks skipped: %s has no llama.cpp KV window to size", BinaryBase(m.Binary)),
			})
		} else if _, ok := spec.GetAny(ctxFlags...); !ok {
			add(Finding{
				Check: "ctx.unset", Severity: SevInfo, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: "no -c/--ctx-size: llama-server falls back to the GGUF's n_ctx_train, which may be far larger than the KV budget allows",
			})
		}
	}
}

func firstFileValue(spec CmdSpec, flags []string) string {
	for _, fl := range spec.Flags {
		if containsString(flags, fl.Name) && len(fl.Values) > 0 {
			return fl.Values[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------- binaries

func lintBinaries(f *File, opts LintOptions, add func(Finding)) {
	stat := opts.stat()
	byBinary := map[string][]string{}
	for _, m := range f.Models {
		if m.Binary == "" {
			add(Finding{
				Check: "binary.unparsable", Severity: SevError, Model: m.ID, Line: m.KeyLine,
				Message: "cmd does not start with a runnable binary",
			})
			continue
		}
		if strings.Contains(m.Binary, "${") {
			add(Finding{
				Check: "binary.unresolved", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("binary path %s still contains an unexpanded macro", m.Binary),
			})
			continue
		}
		if _, err := stat(m.Binary); err != nil {
			add(Finding{
				Check: "binary.missing", Severity: SevError, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: fmt.Sprintf("inference binary %s does not exist", m.Binary),
				Detail:  err.Error(),
			})
		}
		// Build drift is only meaningful WITHIN a seat kind. A whisper seat
		// pointing at whisper-server.exe while every llama seat points at
		// llama-server.exe is correct by construction, not drift.
		if m.Seat == SeatLlamaServer {
			byBinary[m.Binary] = append(byBinary[m.Binary], m.ID)
		}
	}
	if len(byBinary) > 1 {
		var parts []string
		for _, b := range SortedKeys(byBinary) {
			parts = append(parts, fmt.Sprintf("%s (%s)", b, strings.Join(byBinary[b], ", ")))
		}
		add(Finding{
			Check: "binary.build-drift", Severity: SevWarn,
			Message: fmt.Sprintf("llama-server seats point at %d different binaries: %s", len(byBinary), strings.Join(parts, " | ")),
			Detail:  "deliberate when one seat needs a fork or a pinned build; a silent half-finished upgrade otherwise. Compare build_info per seat before benchmarking across them",
		})
	}
}

// ---------------------------------------------------------------- routing

func lintRouting(f *File, add func(Finding)) {
	hasMatrix := f.Matrix != nil
	hasGroups := len(f.Groups) > 0
	if hasMatrix && hasGroups {
		add(Finding{
			Check: "routing.both", Severity: SevError, Line: f.TopKeyLine["matrix"],
			Message: "config declares BOTH groups: and matrix: — llama-swap accepts one routing style, not both",
		})
	}
	if !hasMatrix && !hasGroups {
		add(Finding{
			Check: "routing.none", Severity: SevInfo,
			Message: "no groups: or matrix: — every model is mutually exclusive with every other, so any request evicts whatever is loaded (including keep-resident seats)",
		})
	}

	if hasMatrix {
		mx := f.Matrix
		for _, v := range SortedKeys(mx.Vars) {
			target := mx.Vars[v]
			if _, ok := f.ModelIndex[target]; !ok {
				add(Finding{
					Check: "matrix.var-unresolved", Severity: SevError, Line: mx.VarLines[v],
					Message: fmt.Sprintf("matrix var %q maps to %q, which is not a model ID in this config", v, target),
				})
			}
		}
		// THE gotcha: evict_costs is keyed by VAR ID, not model ID. llama-swap
		// rejects the whole config with `evict_costs: unknown var ID "<id>"`
		// and refuses to start — a config on this deployment was rejected for
		// exactly this, which is why the check exists.
		for _, k := range SortedKeys(mx.EvictCosts) {
			if _, ok := mx.Vars[k]; ok {
				continue
			}
			msg := fmt.Sprintf("evict_costs key %q is not a matrix var ID", k)
			if _, isModel := f.ModelIndex[k]; isModel {
				var suggest string
				for _, v := range SortedKeys(mx.Vars) {
					if mx.Vars[v] == k {
						suggest = v
						break
					}
				}
				msg += fmt.Sprintf(" — it is a MODEL id. evict_costs is keyed by VAR id; llama-swap rejects the config outright with `evict_costs: unknown var ID %q` and the service will not start", k)
				if suggest != "" {
					msg += fmt.Sprintf(". Use %q", suggest)
				}
			}
			add(Finding{Check: "matrix.evict-cost-key", Severity: SevError, Line: mx.CostLines[k], Message: msg})
		}
		for _, k := range SortedKeys(mx.EvictCosts) {
			if mx.EvictCosts[k] <= 0 {
				add(Finding{
					Check: "matrix.evict-cost-value", Severity: SevError, Line: mx.CostLines[k],
					Message: fmt.Sprintf("evict_costs[%s] = %d; values must be positive integers", k, mx.EvictCosts[k]),
				})
			}
		}
		for _, name := range SortedKeys(mx.Sets) {
			for _, ref := range setReferences(mx.Sets[name]) {
				if strings.HasPrefix(ref, "+") {
					if _, ok := mx.Sets[strings.TrimPrefix(ref, "+")]; !ok {
						add(Finding{
							Check: "matrix.set-unresolved", Severity: SevError, Line: mx.SetLines[name],
							Message: fmt.Sprintf("set %q includes %q, which is not another set", name, ref),
						})
					}
					continue
				}
				if _, ok := mx.Vars[ref]; !ok {
					add(Finding{
						Check: "matrix.set-unresolved", Severity: SevError, Line: mx.SetLines[name],
						Message: fmt.Sprintf("set %q references %q, which is not a matrix var (sets use var IDs, not model IDs)", name, ref),
					})
				}
			}
		}
		seated := map[string]bool{}
		for _, target := range mx.Vars {
			seated[target] = true
		}
		for _, m := range f.Models {
			if m.Unlisted || seated[m.ID] {
				continue
			}
			add(Finding{
				Check: "matrix.model-unmapped", Severity: SevWarn, Model: m.ID, Line: m.KeyLine, SeatKind: m.Seat,
				Message: "model has no matrix var, so the solver has no set that permits it alongside anything else",
			})
		}
	}

	for _, g := range f.Groups {
		for _, mem := range g.Members {
			if _, ok := f.ModelIndex[mem]; !ok {
				add(Finding{
					Check: "groups.member-unresolved", Severity: SevError, Line: g.Line,
					Message: fmt.Sprintf("group %q lists member %q, which is not a model ID", g.Name, mem),
				})
			}
		}
		if g.Exclusive != nil && *g.Exclusive && g.Persistent != nil && *g.Persistent {
			add(Finding{
				Check: "groups.exclusive-persistent", Severity: SevWarn, Line: g.Line,
				Message: fmt.Sprintf("group %q is both exclusive and persistent; an exclusive group elsewhere still evicts it (observed live on this class of deployment) — matrix sets express co-residency without that hole", g.Name),
			})
		}
	}

	for _, p := range f.HookPreload {
		if _, ok := f.Resolve(p); !ok {
			add(Finding{
				Check: "hooks.preload-unresolved", Severity: SevError, Line: f.TopKeyLine["hooks"],
				Message: fmt.Sprintf("hooks.on_startup.preload lists %q, which resolves to no model or alias", p),
			})
		}
	}
	if len(f.HookPreload) > 1 {
		add(Finding{
			Check: "hooks.preload-vram", Severity: SevWarn, Line: f.TopKeyLine["hooks"],
			Message: fmt.Sprintf("hooks.on_startup.preload loads %d models at boot; they must all fit in VRAM simultaneously or the last ones fail to start with no request to blame", len(f.HookPreload)),
		})
	}
}

// setReferences extracts identifiers from a matrix set expression like
// "+residents & (qc | oss | g31)".
func setReferences(expr string) []string {
	var out []string
	var cur strings.Builder
	plus := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		s := cur.String()
		if plus {
			s = "+" + s
		}
		out = append(out, s)
		cur.Reset()
		plus = false
	}
	for _, r := range expr {
		switch {
		case r == '+' && cur.Len() == 0:
			plus = true
		case r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// ------------------------------------------------- profiles and selectors

func lintProfilesSelectors(f *File, add func(Finding)) {
	for _, name := range SortedKeys(f.Profiles) {
		for _, from := range SortedKeys(f.Profiles[name]) {
			to := f.Profiles[name][from]
			if _, ok := f.Resolve(to); !ok {
				add(Finding{
					Check: "profiles.target-unresolved", Severity: SevError, Line: f.TopKeyLine["profiles"],
					Message: fmt.Sprintf("profile %q rewrites %q -> %q, and %q resolves to no model or alias", name, from, to, to),
				})
			}
		}
	}
	for _, name := range SortedKeys(f.Selectors) {
		if _, clash := f.ModelIndex[name]; clash {
			add(Finding{
				Check: "selectors.shadows-model", Severity: SevError, Line: f.TopKeyLine["selectors"],
				Message: fmt.Sprintf("selector %q has the same name as a real model; the selector wins and the model becomes unreachable by its own ID", name),
			})
		}
		for _, target := range selectorTargets(f.Selectors[name]) {
			if _, ok := f.Resolve(target); !ok {
				add(Finding{
					Check: "selectors.target-unresolved", Severity: SevError, Line: f.TopKeyLine["selectors"],
					Message: fmt.Sprintf("selector %q targets %q, which resolves to no model or alias", name, target),
				})
			}
		}
	}
}

// selectorTargets pulls model-name-shaped strings out of a selector value
// without hardcoding a selector grammar this build may not have seen.
func selectorTargets(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = append(out, t)
	case []any:
		for _, e := range t {
			out = append(out, selectorTargets(e)...)
		}
	case map[string]any:
		for _, k := range SortedKeys(t) {
			if k == "model" || k == "target" || k == "default" || k == "fallback" {
				out = append(out, selectorTargets(t[k])...)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- apiKeys

func lintAPIKeys(f *File, add func(Finding)) {
	if f.APIKeyCount == 0 {
		add(Finding{
			Check: "apikeys.absent", Severity: SevInfo, Line: f.TopKeyLine["apiKeys"],
			Message: "apiKeys is empty or absent — inference endpoints accept unauthenticated requests. Correct for a loopback-only listener; a hole the moment the listener binds 0.0.0.0",
		})
		return
	}
	// Never print a key, not even truncated, not even in --json. The count and
	// the line are everything an operator needs, and a lint report gets pasted
	// into chat logs and issue trackers.
	add(Finding{
		Check: "apikeys.present", Severity: SevInfo, Line: f.TopKeyLine["apiKeys"],
		Message: fmt.Sprintf("%d API key(s) configured (values deliberately not printed)", f.APIKeyCount),
	})
}

// ---------------------------------------------------------------- store

func lintStore(f *File, add func(Finding)) {
	if f.StorePath != "" {
		return
	}
	add(Finding{
		Check: "store.path-unset", Severity: SevInfo, Line: f.TopKeyLine["store"],
		Message: "store.path is unset — llama-swap v236+ can persist activity to SQLite across restarts. Without it, /api/metrics/activity is an in-memory ring buffer that empties on every restart",
	})
}

// ---------------------------------------------------------------- dead models

func lintDeadModels(f *File, opts LintOptions, add func(Finding)) {
	if !opts.HaveLocalHistory {
		add(Finding{
			Check: "model.dead", Severity: SevSkipped,
			Message: "dead-model detection skipped: requires local history",
			Detail:  "deciding a seat is unused needs a request history that outlives the proxy's in-memory ring buffer. Until the local mirror has accumulated it, a 'never used' verdict would only mean 'not used since the last restart'",
		})
		return
	}
	add(Finding{
		Check: "model.dead", Severity: SevSkipped,
		Message: "dead-model detection not implemented against the local mirror yet",
	})
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
