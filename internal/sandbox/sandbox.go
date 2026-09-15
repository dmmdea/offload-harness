// Package sandbox builds the P4 OS-level cage for running an untrusted command
// (a future broad-shell/exec tool) confined so it CANNOT bypass the agent's
// app-level cages. On Linux it self-restricts a re-exec'd worker with: a fresh
// UNPRIVILEGED user+network namespace (severs ALL egress, including the loopback
// model/memory services), Landlock filesystem confinement (RO system + RO
// worktree + RW scratch, inherited by every child and irreversible), and a
// seccomp denylist + no_new_privs. The cage is built and adversarially PROVEN
// before any shell capability is granted (cage-before-capability). Non-Linux
// builds get a stub that reports unavailable and refuses to run (fail-closed).
//
// The whole path is rootless: an unprivileged user namespace grants the
// namespaced privilege to create the net namespace; Landlock and seccomp are
// unprivileged by design. No sudo, no firewall, no package installs.
package sandbox

import (
	"encoding/json"
	"strings"
)

// workerEnv is the sentinel environment variable. When it is set on a re-exec of
// this binary, the process is the sandboxed WORKER: it applies the cage and
// execs Spec.Argv. IsWorker()/RunWorkerFromEnv() MUST be called at the very top
// of main() (and TestMain) so the worker path runs before any normal startup.
const workerEnv = "OFFLOAD_SANDBOX_WORKER"

// refusalMarker prefixes the worker's stderr when the CAGE ITSELF refuses to run
// (setup failure: ABI floor unmet, seccomp/Landlock install failed, …), so the
// caller can distinguish a refusal from a real command's non-zero exit (both can
// surface as exit 126/127). Run() sets Result.Refused when it sees this prefix.
const refusalMarker = "[[SANDBOX-REFUSED]] "

// NetABIFloor is the Landlock ABI the cage's NETWORK guarantee needs: the
// rule set is landlock.V4 (paths + TCP bind/connect handling), and with FS
// rules but no ConnectTCP/BindTCP rule every TCP bind and connect is denied
// — ONLY on a kernel whose Landlock ABI is >= 4 (Linux 6.7). Below that,
// BestEffort() applies the path rules and silently drops the network ones
// (register H-27: the shape GHSA-vv6c-69r6-chg9 made worse in go-landlock
// v0.9.0, fixed upstream in v0.10.0 which this repo took in 0.104.0). The
// callers used to ask for ABI floor 1, which let a 6.2-era kernel run the
// cage with the network half of a shipped guarantee missing. Run raises any
// lower request to this floor; a caller that truly wants a path-only cage
// has no such mode — the guarantee is one thing, not two.
const NetABIFloor = 4

// EffectiveABIFloor is the floor Run enforces for a requested one: never
// below NetABIFloor.
func EffectiveABIFloor(requested int) int {
	if requested < NetABIFloor {
		return NetABIFloor
	}
	return requested
}

// Spec is the cage configuration the parent hands to the re-exec'd worker
// (serialized into workerEnv). All paths must be absolute.
type Spec struct {
	Argv             []string `json:"argv"`              // command to run INSIDE the cage, e.g. ["/bin/sh","-c",cmd]
	Worktree         string   `json:"worktree"`          // the worker's working directory; read-only UNLESS WorktreeWritable
	WorktreeWritable bool     `json:"worktree_writable"` // grant RW (not RO) on Worktree — for a shell that must build/edit in place
	Scratch          string   `json:"scratch"`           // a writable scratch dir (always RW); also HOME/TMPDIR inside the cage
	ReadDirs         []string `json:"read_dirs"`         // extra read-only dirs (the system dirs below are always added)
	ReadFiles        []string `json:"read_files"`        // specific read-only (+executable) files to grant, e.g. a static helper binary
	ABIFloor         int      `json:"abi_floor"`         // minimum achieved Landlock ABI required, else fail-closed; raised to NetABIFloor by Run (0.115.22, H-27)

	// AllowedExecutables is the base-name allowlist of programs the caged command
	// may launch. WINDOWS ENFORCES it: before spawning, the resolved program's base
	// name (case-insensitive, trailing ".exe" dropped) must be in this list, else
	// Run returns Result{Refused:true} WITHOUT launching. The LINUX cage IGNORES it
	// — there the OS-level containment (namespaces + Landlock + seccomp) is the
	// boundary, not an executable name filter. An empty list on Windows refuses
	// every command (fail-closed).
	AllowedExecutables []string `json:"allowed_executables"`
}

// Result is the outcome of a caged run.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Signal   int  // terminating signal number if the command was killed (e.g. SIGSYS=31 from a seccomp KILL), else 0
	Refused  bool // true if the CAGE refused to run (setup failed) — distinct from the command running and exiting non-zero
}

// defaultReadDirs are the read-only system directories a minimal command needs
// to load its interpreter and shared libraries. NOTE: deliberately excludes
// /home, /root, /proc, /sys, /dev, /tmp, /mnt — Landlock default-denies anything
// not granted, so secrets (~/.ssh, ~/.claude, the memory store) are unreadable.
var defaultReadDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/etc"}

// --- the caged child's environment (shared contract, per-platform lists) ------
//
// A caged process gets an EXPLICIT environment, never the parent's. The parent
// here is the harness: its environment carries GITHUB_TOKEN, MEM0_API_KEY, the
// fleet auth token and whatever else a shell exported, and handing that block to
// an untrusted command is a credential disclosure that no filesystem or network
// containment can undo afterwards. Windows passed nil lpEnvironment to
// CreateProcess until 0.117.7, which means exactly "inherit the caller's block";
// Linux has always built its three entries by hand.
//
// The set has two halves, and the split is the rule:
//
//   - DERIVED (derivedEnvNames): a home and a temp dir, whose VALUES come from
//     Spec.Scratch and never from the parent. This is what keeps the child out of
//     the operator's real profile and %TEMP%.
//   - INHERITED (the platform's envPassthrough): the few variables whose values
//     only the parent OS can supply and that a process needs in order to START.
//     Linux needs none of them (its PATH is a fixed system list); Windows cannot
//     synthesise SystemRoot or PATH, so those are copied by name.
//
// Anything not named is dropped. Adding a name is a decision with a reason, which
// is the property inheritance destroyed.

// Each platform file declares the four lists this contract is made of:
//
//	envFixed        entries whose VALUE is a constant of the cage (Linux's system PATH)
//	envPassthrough  names COPIED BY NAME from the parent (Windows' SystemRoot, PATH, ...)
//	envHomeNames    names set to the run's scratch dir, the child's "home"
//	envTempNames    names set to the run's scratch dir, the child's temp
//
// envFixed + envPassthrough is where the two platforms genuinely differ: a Linux
// cage can name its own PATH, a Windows one cannot synthesise SystemRoot or the
// system PATH and must copy them. Everything else is shared here.

// EnvAllowlist is every variable name a caged child can receive on this
// platform. Exported so the guarantee's test can assert the CLOSED set instead
// of restating it — a test that re-declares the list proves only that it can
// copy the list.
func EnvAllowlist() []string {
	out := make([]string, 0, len(envFixed)+len(envPassthrough)+len(envHomeNames)+len(envTempNames))
	for _, e := range envFixed {
		if k, _, ok := strings.Cut(e, "="); ok {
			out = append(out, k)
		}
	}
	out = append(out, envPassthrough...)
	out = append(out, envHomeNames...)
	out = append(out, envTempNames...)
	return out
}

// cageEnv builds the caged command's whole environment. The child sees this and
// nothing else.
//
// Home and temp point at the run's SCRATCH dir, never at the operator's real
// profile or %TEMP%: the cage grants scratch as writable, so a child that writes
// where its env tells it to stays inside the cage instead of being refused (or,
// on Windows, quietly writing into a directory the low-IL token happens to allow).
// A Spec with no Scratch falls back to the worktree — never to the parent's value,
// which is the inheritance this exists to remove.
func cageEnv(spec Spec, environ []string) []string {
	dir := spec.Scratch
	if dir == "" {
		dir = spec.Worktree
	}
	env := append([]string(nil), envFixed...)
	env = append(env, inheritedEnv(envPassthrough, environ)...)
	for _, n := range envHomeNames {
		env = append(env, n+"="+dir)
	}
	for _, n := range envTempNames {
		env = append(env, n+"="+dir)
	}
	return env
}

// inheritedEnv copies ONLY the named variables out of the parent environment,
// matching names case-insensitively (Windows env names are case-insensitive, and
// a case-sensitive match there is a silently empty allowlist) and emitting the
// allowlist's own spelling. A name the parent does not set is simply absent — an
// empty-string entry is not the same as unset, and some Windows APIs treat it
// differently.
func inheritedEnv(names []string, environ []string) []string {
	if len(names) == 0 {
		return nil
	}
	have := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue // a leading-"=" entry is Windows' per-drive cwd record, never inherited
		}
		have[strings.ToUpper(k)] = v
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := have[strings.ToUpper(n)]; ok && v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

func (s Spec) encode() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func decodeSpec(s string) (Spec, error) {
	var sp Spec
	err := json.Unmarshal([]byte(s), &sp)
	return sp, err
}
