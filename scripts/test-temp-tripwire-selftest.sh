#!/usr/bin/env bash
# test-temp-tripwire-selftest.sh -- the temp-folder tripwire proves itself (GPU-I8c).
#
# Runs scripts/test-temp-tripwire.sh against the fixture module scripts/testdata/tripwirefixture and asserts, for every
# case, the verdict, the exit code and what was left on disk -- read from the disk here, not taken on the script's word:
#
#   CLEAN         ./clean    t.TempDir only                       exit 0, no folder left
#   LEAK          ./leaker   three folders never removed          exit 1, "3  tripwire-leak-*", the folder kept; --remove takes it
#   PROCESSES     ./spawner  a copy of the test binary running    exit 2 with the pid (needs a real /proc; where there is
#                            from an image inside the folder      none the case asserts the documented skip instead)
#   TESTS-FAILED  ./failing  a failing test, nothing left         exit 3, no folder left
#   REFUSED       --dir /    a folder on the system drive         exit 4, nothing created
#   usage errors, a leak beside a failing package, the name-prefix rules, and -- when pwsh is on PATH -- the PowerShell twin
#   giving the same verdicts, exit codes and leak lines on the same fixture. Also pinned (review round): a failure followed by a
#   passing package is named although the tail shows one line; a stat that fails refuses the folder; a host without a usable
#   /proc says so and does not remove a folder with entries; a temp folder removed during the run exits 70; and, on Linux, a
#   run interrupted by TERM or HUP leaves no test binary running from the folder, keeps the folder and removes its capture
#   (./hanger, a test that sleeps 40 s, is the binary).
#
# Usage: bash scripts/test-temp-tripwire-selftest.sh      (needs the Go toolchain on PATH; exits 1 if any check fails)
# TRIPWIRE_SELFTEST_FAILFAST=1 ends the run at the first failed check (a mutation check needs only the first red).
# TRIPWIRE_SELFTEST_NO_SPAWNER=1 leaves out the PROCESSES case (where the sleeper cannot be killed the run waits for it to end).
# Scratch: $TRIPWIRE_SELFTEST_DIR, else $TMPDIR, else /tmp; it should be on a data drive (the script refuses the system
# drive, and the self-test then passes --allow-system-drive for the cases that need a folder). Removed on exit.

set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tripwire="$here/test-temp-tripwire.sh"
twin="$here/test-temp-tripwire.ps1"
fixture="$here/testdata/tripwirefixture"
bash_bin=${BASH:-bash}

command -v go >/dev/null 2>&1 || {
	echo "selftest: the Go toolchain is not on PATH; the fixture is a Go module" >&2
	exit 2
}
[ -f "$tripwire" ] && [ -d "$fixture" ] || {
	echo "selftest: cannot find $tripwire or $fixture" >&2
	exit 2
}

export GOWORK=off GOTOOLCHAIN=local # the fixture is a module of its own; never resolve it through a workspace

base=${TRIPWIRE_SELFTEST_DIR:-${TMPDIR:-/tmp}}
base=$(cd "$base" && pwd -P) || exit 2
root=$(mktemp -d "${base%/}/test-temp-tripwire-selftest.XXXXXX") || exit 2
root=$(cd "$root" && pwd -P)
pids=''

# remove_tree DIR: removes a folder the self-test made, trying again for a while because an image that is still running
# cannot be deleted on Windows (the sleeper of the PROCESSES case ends by itself within a minute where nothing kills it).
remove_tree() {
	for _ in $(seq 1 45); do
		rm -rf -- "$1" 2>/dev/null
		[ -e "$1" ] || return 0
		sleep 2
	done
	printf 'selftest: could not remove %s (an image in it is still running); remove it by an explicit list
' "$1" >&2
}

cleanup() { # every sleeper the cases started goes first, then the scratch root (its own mktemp folder, nothing else)
	for p in $pids; do kill "$p" 2>/dev/null; done
	for p in $pids; do
		for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || break; sleep 0.3; done
	done
	case $root in "$base"/test-temp-tripwire-selftest.*) remove_tree "$root" ;; esac
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass=0
fail=0
skip=0
out='' rc=0

ok() { pass=$((pass + 1)); printf 'ok    %s\n' "$1"; }
bad() {
	fail=$((fail + 1))
	printf 'FAIL  %s\n' "$1"
	[ -z "${TRIPWIRE_SELFTEST_FAILFAST:-}" ] || exit 1
}
skipped() { skip=$((skip + 1)); printf 'skip  %s\n' "$1"; }

expect() { # expect WHAT GOT WANT
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}
expect_has() { # expect_has WHAT TEXT PATTERN (fixed string)
	case $2 in *"$3"*) ok "$1" ;; *) bad "$1: output lacks '$3'"; printf '%s\n' "$2" | sed 's/^/      | /' ;; esac
}
expect_lacks() {
	case $2 in *"$3"*) bad "$1: output has '$3'" ;; *) ok "$1" ;; esac
}

run() { # run ARGS... : the tripwire with ARGS; sets out and rc
	out=$("$bash_bin" "$tripwire" "$@" 2>&1)
	rc=$?
}
verdict_of() { printf '%s\n' "$out" | sed -n 's/^TRIPWIRE-VERDICT: \([A-Z-]*\).*/\1/p' | tail -n 1; }
# proc_ok: 1 where the tripwire can look at running processes (a real /proc), 0 where it says it cannot (Git Bash's emulated one)
proc_ok=1
case $(uname -s 2>/dev/null) in MINGW* | MSYS* | CYGWIN*) proc_ok=0 ;; esac
[ -r /proc/self/exe ] || proc_ok=0
# procs_under DIR [NAME]: the pids of the processes that run from an image inside DIR (Linux only); with NAME, only images of that name
procs_under() {
	local p exe
	[ "$proc_ok" = 1 ] || return 0
	for p in /proc/[0-9]*; do
		exe=$(readlink "$p/exe" 2>/dev/null) || continue
		exe=${exe% (deleted)}
		case $exe in "$1"/*) { [ -z "${2:-}" ] || [ "${exe##*/}" = "$2" ]; } && printf '%s\n' "${p#/proc/}" ;; esac
	done
}
stray_captures() { find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*.go.out' 2>/dev/null | wc -l | tr -d ' '; }
folders_in() { find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' 2>/dev/null | wc -l | tr -d ' '; }
leak_lines() { printf '%s\n' "$out" | grep -E '^test-temp-tripwire:  +[0-9]+  ' | sed 's/^test-temp-tripwire: *//'; }

# --- usage errors (no verdict line, exit 64)
run
expect "no --dir: exit" "$rc" 64
run --dir "$root/does-not-exist" --module "$fixture"
expect "--dir that does not exist: exit" "$rc" 64
run --dir "$root" --module "$fixture" --bogus
expect "unknown option: exit" "$rc" 64
run --dir "$root" --module "$root"
expect "no go.mod in --module: exit" "$rc" 64
expect "usage errors print no verdict" "$(verdict_of)" ""

# --- the system drive is refused, and nothing is created
run --dir / --module "$fixture" --pkgs ./clean
expect "system drive: exit" "$rc" 4
expect "system drive: verdict" "$(verdict_of)" REFUSED
expect "system drive: nothing was created in /" "$(find / -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' 2>/dev/null | wc -l | tr -d ' ')" 0

# --- whether the scratch root itself is on the system drive decides whether the cases below need the override
run --dir "$root" --module "$fixture" --pkgs ./clean
allow=''
if [ "$rc" = 4 ]; then
	allow=--allow-system-drive
	ok "scratch root is on the system drive: refused without the override (the cases below pass it)"
fi
common=(--dir "$root" --module "$fixture")
[ -n "$allow" ] && common+=("$allow")

# --- CLEAN
run "${common[@]}" --pkgs ./clean
expect "clean: exit" "$rc" 0
expect "clean: verdict" "$(verdict_of)" CLEAN
expect "clean: the empty folder is gone" "$(folders_in)" 0

# --- LEAK
run "${common[@]}" --pkgs ./leaker
expect "leak: exit" "$rc" 1
expect "leak: verdict" "$(verdict_of)" LEAK
expect "leak: the three leaked folders are counted under one prefix" "$(leak_lines)" "3  tripwire-leak-*"
expect "leak: the folder is kept as evidence" "$(folders_in)" 1
kept=$(find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' -type d | head -n 1)
expect "leak: the folder holds exactly the three leaked entries on disk" "$(ls -A1 "$kept" | wc -l | tr -d ' ')" 3
expect "leak: the folder that was removed by its own test is not among them" "$(ls -A1 "$kept" | grep -c 'tripwire-clean-' | tr -d ' ')" 0
cleanup_kept_all() { find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' -type d | while read -r d; do remove_tree "$d"; done; }
cleanup_kept() { case $kept in "$root"/test-temp-tripwire.*) remove_tree "$kept" ;; esac; }
cleanup_kept
run "${common[@]}" --pkgs ./leaker --remove
expect "leak with --remove: exit is still LEAK" "$rc" 1
if [ "$proc_ok" = 1 ]; then
	expect "leak with --remove: the folder is removed" "$(folders_in)" 0
else
	# no process check here: the folder is not removed while a process might still run from it
	expect "leak with --remove, no process check: the folder is kept" "$(folders_in)" 1
	cleanup_kept_all
fi

# --- the reasoned ignore list
printf '# a comment\n\ntripwire-leak-*\tthe fixture leaves these on purpose\n' >"$root/ignore.txt"
run "${common[@]}" --pkgs ./leaker --ignore-file "$root/ignore.txt"
expect "ignore list naming the leak: exit" "$rc" 0
expect "ignore list naming the leak: verdict" "$(verdict_of)" CLEAN
expect_has "ignore list naming the leak: the group is printed with its reason, not hidden" "$out" "ignored       3  tripwire-leak-*  (the fixture leaves these on purpose)"
expect_lacks "ignore list naming the leak: it is not reported as a leak" "$out" "test-temp-tripwire: left"
expect_has "ignore list naming the leak: the verdict says how many were not counted" "$out" "3 entries named by the ignore list were not counted"
if [ "$proc_ok" = 1 ]; then
	expect "ignore list naming the leak: the folder goes" "$(folders_in)" 0
else
	expect "ignore list naming the leak, no process check: the folder is kept (it holds entries)" "$(folders_in)" 1
	cleanup_kept_all
fi
printf 'something-else-*\tnot what the fixture leaves\n' >"$root/ignore-other.txt"
run "${common[@]}" --pkgs ./leaker --ignore-file "$root/ignore-other.txt"
expect "ignore list naming something else: exit" "$rc" 1
expect "ignore list naming something else: verdict" "$(verdict_of)" LEAK
cleanup_kept_all
printf 'tripwire-leak-*\n' >"$root/ignore-noreason.txt"
run "${common[@]}" --pkgs ./leaker --ignore-file "$root/ignore-noreason.txt"
expect "ignore line with no reason: exit" "$rc" 64
printf 'tripwire-leak-*\t   \n' >"$root/ignore-blank.txt"
run "${common[@]}" --pkgs ./leaker --ignore-file "$root/ignore-blank.txt"
expect "ignore line with a blank reason: exit" "$rc" 64
run "${common[@]}" --pkgs ./leaker --ignore-file "$root/no-such-file.txt"
expect "ignore file that does not exist: exit" "$rc" 64
expect "a rejected ignore list ran nothing" "$(folders_in)" 0
# the default list: scripts/test-temp-tripwire.ignore in the module, found without being named
cp -R "$fixture" "$root/fixmod" && mkdir -p "$root/fixmod/scripts" && printf 'tripwire-leak-*\tthe default list names it\n' >"$root/fixmod/scripts/test-temp-tripwire.ignore"
run --dir "$root" --module "$root/fixmod" ${allow:+"$allow"} --pkgs ./leaker
expect "default ignore list: exit" "$rc" 0
expect_has "default ignore list: the reason is printed" "$out" "(the default list names it)"
cleanup_kept_all

# --- TESTS-FAILED
run "${common[@]}" --pkgs ./failing
expect "failing: exit" "$rc" 3
expect "failing: verdict" "$(verdict_of)" TESTS-FAILED
expect_has "failing: the go test output is shown" "$out" "this test fails on purpose"
expect "failing: the empty folder is gone" "$(folders_in)" 0

# --- a leak beside a failing package is still a LEAK
run "${common[@]}" --pkgs "./leaker ./failing"
expect "leak beside a failing package: exit" "$rc" 1
expect "leak beside a failing package: verdict" "$(verdict_of)" LEAK
cleanup_kept_all

# --- a failure that is not at the end of the output is still named. go test prints one "ok" line per later package, so the last
# lines of a whole-module run hold only "ok" lines; the failing test and its message have to be reported on their own.
run "${common[@]}" --pkgs "./failing ./clean" --tail 1
expect "failure followed by a passing package, --tail 1: exit" "$rc" 3
expect "failure followed by a passing package, --tail 1: verdict" "$(verdict_of)" TESTS-FAILED
expect_has "failure followed by a passing package, --tail 1: the failing test is named" "$out" "--- FAIL: TestFails"
expect_has "failure followed by a passing package, --tail 1: its message is shown" "$out" "this test fails on purpose"
expect "failure followed by a passing package: the empty folder is gone" "$(folders_in)" 0
expect "failure followed by a passing package: no capture is left beside it" "$(stray_captures)" 0
run "${common[@]}" --pkgs ./failing --tail 0
expect_lacks "--tail 0 shows no go test output" "$out" "this test fails on purpose"
cleanup_kept_all

# --- a refusal that cannot be decided is a refusal. A host whose stat does not take -c (BSD, BusyBox) cannot say which
# filesystem holds the folder; the guard must not read that as "not the system drive".
if [ -z "$allow" ]; then
	mkdir -p "$root/stub-stat"
	printf '#!/bin/sh\nexit 1\n' >"$root/stub-stat/stat"
	chmod +x "$root/stub-stat/stat"
	out=$(PATH="$root/stub-stat:$PATH" "$bash_bin" "$tripwire" --dir "$root" --module "$fixture" --pkgs ./clean 2>&1)
	rc=$?
	expect "stat that fails: exit (refused)" "$rc" 4
	expect "stat that fails: verdict" "$(verdict_of)" REFUSED
	expect_has "stat that fails: the output says it could not tell" "$out" "cannot tell"
	expect "stat that fails: nothing was created" "$(folders_in)" 0
	out=$(PATH="$root/stub-stat:$PATH" "$bash_bin" "$tripwire" --dir "$root" --module "$fixture" --pkgs ./clean --allow-system-drive 2>&1)
	rc=$?
	expect "stat that fails, with --allow-system-drive: the run goes ahead" "$rc" 0
else
	skipped "stat that fails: the scratch root is on the system drive, so the guard is not what decides here"
fi

# --- a process check that cannot run is said, and the folder is not removed on its account. A uname that answers like Git Bash
# makes this host look like one without a usable /proc.
mkdir -p "$root/stub-uname"
printf '#!/bin/sh\necho MINGW64_NT-10.0\n' >"$root/stub-uname/uname"
chmod +x "$root/stub-uname/uname"
out=$(PATH="$root/stub-uname:$PATH" "$bash_bin" "$tripwire" "${common[@]}" --pkgs ./clean 2>&1)
rc=$?
expect "no process check, nothing left: exit" "$rc" 0
expect_has "no process check, nothing left: the verdict says the check was skipped" "$out" "process check skipped"
expect "no process check, nothing left: the empty folder is gone" "$(folders_in)" 0
out=$(PATH="$root/stub-uname:$PATH" "$bash_bin" "$tripwire" "${common[@]}" --pkgs ./leaker --remove 2>&1)
rc=$?
expect "no process check, a leak, --remove: exit" "$rc" 1
expect_has "no process check, a leak, --remove: the verdict says the check was skipped" "$out" "process check skipped"
expect "no process check, a leak, --remove: the folder is kept, because a process might run from it" "$(folders_in)" 1
cleanup_kept_all

# --- a temp folder that vanishes during the run is an error, not a clean run. A go that deletes the folder it was given stands in
# for a test that removes its own temp dir: an empty listing of a folder that is gone would read as CLEAN.
mkdir -p "$root/stub-go"
printf '#!/bin/sh\nrm -rf "$GOTMPDIR"\nexit 0\n' >"$root/stub-go/go"
chmod +x "$root/stub-go/go"
out=$(PATH="$root/stub-go:$PATH" "$bash_bin" "$tripwire" "${common[@]}" --pkgs ./clean 2>&1)
rc=$?
expect "folder removed during the run: exit" "$rc" 70
expect_has "folder removed during the run: the output says it is gone" "$out" "is gone"
expect "folder removed during the run: no verdict is printed" "$(verdict_of)" ""
expect "folder removed during the run: no capture is left beside it" "$(stray_captures)" 0

# --- PROCESSES
if [ -n "${TRIPWIRE_SELFTEST_NO_SPAWNER:-}" ]; then
	skipped "spawner: TRIPWIRE_SELFTEST_NO_SPAWNER is set (for a host where the sleeper cannot be killed and the run would wait for it to end)"
else
	run "${common[@]}" --pkgs ./spawner
	printed_pids=$(printf '%s\n' "$out" | sed -n 's/^test-temp-tripwire: process   pid \([0-9]*\) .*/\1/p')
	pids="$pids $printed_pids"
	case $(uname -s 2>/dev/null) in
	MINGW* | MSYS* | CYGWIN*)
		expect "spawner without a usable /proc: exit (a leak, not a process verdict)" "$rc" 1
		expect_has "spawner without a usable /proc: the skip is said" "$out" "process check skipped"
		;;
	*)
		if [ -r /proc/self/exe ]; then
			expect "spawner: exit" "$rc" 2
			expect "spawner: verdict" "$(verdict_of)" PROCESSES
			expect_has "spawner: the image under the folder is named" "$out" "/tripwire-sleeper"
			[ -n "$printed_pids" ] && ok "spawner: a pid was printed" || bad "spawner: no pid was printed"
			expect "spawner: the folder is kept while the process runs" "$(folders_in)" 1
			# the folder of a run is never removed while a process runs from it, whatever was asked
			for p in $printed_pids; do kill "$p" 2>/dev/null; done
			for p in $printed_pids; do
				for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do kill -0 "$p" 2>/dev/null || break; sleep 0.3; done
			done
			cleanup_kept_all
			run "${common[@]}" --pkgs ./spawner --remove
			printed_pids=$(printf '%s\n' "$out" | sed -n 's/^test-temp-tripwire: process   pid \([0-9]*\) .*/\1/p')
			pids="$pids $printed_pids"
			expect "spawner with --remove: exit" "$rc" 2
			expect "spawner with --remove: the folder is still kept while the process runs" "$(folders_in)" 1
			kept=$(find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' -type d | head -n 1)
			expect "spawner with --remove: nothing in the folder was touched (the note beside the running image is still there)" "$(ls -A1 "$kept" 2>/dev/null | grep -c '^tripwire-note.txt$' | tr -d ' ')" 1
		else
			skipped "spawner: no /proc on this host"
		fi
		;;
	esac
	for p in $printed_pids; do kill "$p" 2>/dev/null; done
	for p in $printed_pids; do
		for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do kill -0 "$p" 2>/dev/null || break; sleep 0.3; done
	done
	cleanup_kept_all
	# a sleeper the case started on a host whose process check is skipped ends by itself within its bounded sleep; the
	# scratch removal below waits for the image to be free only where the OS needs that
fi

# --- an interrupted run stops the test binaries it started, not only go. A run is stopped while the ./hanger test binary is
# running from inside the watched folder; afterwards no process may run from the scratch root, the capture file must be gone,
# the folder stays as evidence, and the exit code is the interrupt's (130). Linux only: the check reads /proc and signals the
# process group that job control gives the go test.
interrupt_case() { # interrupt_case SIGNAL
	local sig=$1 pid waited=0 alive=0 left strays
	"$bash_bin" "$tripwire" "${common[@]}" --pkgs ./hanger >"$root/interrupt.out" 2>&1 &
	pid=$!
	# both processes: the test binary and the copy of itself it starts; a stop before the copy exists proves nothing about the copy
	while [ "$waited" -lt 600 ] && [ "$(procs_under "$root" hanger.test | wc -l | tr -d ' ')" -lt 2 ]; do sleep 0.2; waited=$((waited + 1)); done
	if [ "$(procs_under "$root" hanger.test | wc -l | tr -d ' ')" -lt 2 ]; then
		bad "interrupt $sig: the hanger test binary and its copy never both ran from the folder (waited 120 s)"
		kill "$pid" 2>/dev/null
		return
	fi
	pids="$pids $(procs_under "$root" | tr '\n' ' ')" # the cleanup at the end kills any survivor
	kill -"$sig" "$pid"
	for _ in $(seq 1 200); do kill -0 "$pid" 2>/dev/null || break; sleep 0.3; done
	if kill -0 "$pid" 2>/dev/null; then
		bad "interrupt $sig: the tripwire is still running 60 s after the signal"
		kill -KILL "$pid" 2>/dev/null
	else
		wait "$pid" 2>/dev/null
		rc=$?
		expect "interrupt $sig: exit code" "$rc" 130
	fi
	out=$(cat "$root/interrupt.out")
	expect_has "interrupt $sig: it says the run was stopped" "$out" "interrupted"
	left=$(procs_under "$root" | tr '\n' ' ')
	expect "interrupt $sig: no process runs from the scratch root afterwards" "$left" ""
	expect "interrupt $sig: the folder is kept as evidence" "$(find "$root" -mindepth 1 -maxdepth 1 -type d -name 'test-temp-tripwire.*' | wc -l | tr -d ' ')" 1
	strays=$(stray_captures)
	expect "interrupt $sig: the go test capture is gone" "$strays" 0
	for p in $left; do kill -KILL "$p" 2>/dev/null; done
	cleanup_kept_all
	remove_tree "$root/interrupt.out"
}
if [ "$proc_ok" = 1 ] && [ -d "$fixture/hanger" ]; then
	interrupt_case TERM
	# a SIGHUP that was ignored when this self-test started (nohup) is ignored by every script it starts and cannot be trapped
	sig_ign=$(awk '/^SigIgn/ { print $2 }' /proc/$$/status 2>/dev/null)
	if [ -n "$sig_ign" ] && [ $((0x$sig_ign & 1)) -ne 0 ]; then
		skipped "interrupt HUP: SIGHUP is ignored in this shell (started with nohup), so the tripwire cannot trap it"
	else
		interrupt_case HUP
	fi
else
	skipped "interrupt: needs a real /proc and process groups (a Linux host); the PowerShell twin is stopped through its own self-test"
fi

# --- the name-prefix rules (the PowerShell twin is held to the same table)
table() {
	while IFS='|' read -r name want; do
		[ -n "$name" ] || continue
		expect "prefix of $name" "$("$bash_bin" "$tripwire" --name-prefix "$name" </dev/null)" "$want"
	done <<'EOF'
lo-pipeline-lease-482910377|lo-pipeline-lease-*
run-graph-12.json|run-graph-*.json
run-graph-manifest-9981.json|run-graph-manifest-*.json
abc.k3Jd9Q|abc.*
lo-x-state-aB3dE9|lo-x-state-*
plain-name|plain-name
tripwire-sleeper.exe|tripwire-sleeper.exe
tmp1|tmp*
EOF
}
table

# --- the PowerShell twin agrees on the same fixture
if command -v pwsh >/dev/null 2>&1 && [ -f "$twin" ]; then
	root_native=$root
	command -v cygpath >/dev/null 2>&1 && root_native=$(cygpath -m "$root")
	fixture_native=$fixture
	command -v cygpath >/dev/null 2>&1 && fixture_native=$(cygpath -m "$fixture")
	twin_native=$twin
	command -v cygpath >/dev/null 2>&1 && twin_native=$(cygpath -m "$twin")
	twin_allow=()
	[ -n "$allow" ] && twin_allow=(-AllowSystemDrive)
	for case_pkgs in ./clean ./leaker ./failing; do
		run "${common[@]}" --pkgs "$case_pkgs"
		sh_verdict=$(verdict_of) sh_rc=$rc sh_leaks=$(leak_lines)
		cleanup_kept_all
		out=$(pwsh -NoProfile -File "$twin_native" -Dir "$root_native" -Module "$fixture_native" -Pkgs "$case_pkgs" ${twin_allow[@]+"${twin_allow[@]}"} 2>&1)
		rc=$?
		out=$(printf '%s' "$out" | tr -d '\r')
		expect "twin agrees on $case_pkgs: exit" "$rc" "$sh_rc"
		expect "twin agrees on $case_pkgs: verdict" "$(verdict_of)" "$sh_verdict"
		expect "twin agrees on $case_pkgs: leak lines" "$(leak_lines)" "$sh_leaks"
		cleanup_kept_all
	done
	while IFS='|' read -r name want; do
		[ -n "$name" ] || continue
		expect "twin prefix of $name" "$(pwsh -NoProfile -File "$twin_native" -NamePrefix "$name" </dev/null)" "$want"
	done <<'EOF'
lo-pipeline-lease-482910377|lo-pipeline-lease-*
run-graph-12.json|run-graph-*.json
abc.k3Jd9Q|abc.*
lo-x-state-aB3dE9|lo-x-state-*
plain-name|plain-name
EOF
else
	skipped "the PowerShell twin is not on this host (pwsh not on PATH): its self-test runs there"
fi

printf '\nselftest: %d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
[ "$fail" = 0 ]
