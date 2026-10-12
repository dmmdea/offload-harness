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
#   giving the same verdicts, exit codes and leak lines on the same fixture.
#
# Usage: bash scripts/test-temp-tripwire-selftest.sh      (needs the Go toolchain on PATH; exits 1 if any check fails)
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
bad() { fail=$((fail + 1)); printf 'FAIL  %s\n' "$1"; }
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
cleanup_kept() { case $kept in "$root"/test-temp-tripwire.*) remove_tree "$kept" ;; esac; }
cleanup_kept
run "${common[@]}" --pkgs ./leaker --remove
expect "leak with --remove: exit is still LEAK" "$rc" 1
expect "leak with --remove: the folder is removed" "$(folders_in)" 0

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
cleanup_kept_all() { find "$root" -mindepth 1 -maxdepth 1 -name 'test-temp-tripwire.*' -type d | while read -r d; do remove_tree "$d"; done; }
cleanup_kept_all

# --- PROCESSES
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
		out=$(printf '%s' "$out" | tr -d '')
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
