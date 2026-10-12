#!/usr/bin/env bash
# test-temp-tripwire.sh -- run the Go tests with the OS temp dir pointed at a fresh folder, then report what is left (GPU-I8c).
#
# The problem it closes. A test that makes a file or folder in the OS temp dir and never removes it adds one more every run.
# That filled a system drive twice (a stand-in test binary left 22,434 folders and about 70 GB in under two hours), and the
# static lint (tempdir_lint_test.go, GPU-I8) cannot see a temp file made by PRODUCTION code that a test calls (F79's inline
# graph JSON), or a started copy of the test binary that exits before its cleanup. This script sees what is actually left in
# the temp folder (and only there: see "What it cannot see" below): it sets TMP, TEMP, TMPDIR and GOTMPDIR to a folder it creates
# empty, runs go test, and looks in the folder afterwards.
# PowerShell twin: scripts/test-temp-tripwire.ps1 (same options in PascalCase, same labels, verdicts and exit codes).
# Self-test: scripts/test-temp-tripwire-selftest.sh over the fixture module scripts/testdata/tripwirefixture.
#
# Usage (run it from the module root, or pass --module):
#   test-temp-tripwire.sh --dir PARENT [--pkgs "PKG ..."] [options]
#
#   --dir PARENT          a folder that exists and is NOT on the system drive; the script makes a fresh, empty
#                         test-temp-tripwire.XXXXXX folder inside it and points the temp dir there. Required: there is no
#                         default, because the OS default temp dir is the system drive, which is what this guards.
#   --pkgs "PKG ..."      the packages, space separated (default ./...). A whole-module run loads the machine: run it
#                         niced on a spare host (nice -n 19 bash scripts/test-temp-tripwire.sh ...), or pass a subset.
#   --module DIR          the Go module to test (default: the current directory)
#   --go-args "ARG ..."   extra go test arguments, space separated, e.g. "-p 1 -short" (-count=1 is always passed). A whole-module
#                         run needs "-timeout 45m": internal/pipeline takes about ten minutes, go test's default timeout, and a test
#                         binary that times out is killed before it cleans up, so its temp state would read as a leak
#   --ignore-file FILE    the reasoned ignore list (default: scripts/test-temp-tripwire.ignore in the module, when it exists):
#                         one entry per line, "NAME-PREFIX<TAB>reason", # comments and blank lines allowed. An entry left in the
#                         temp folder whose prefix is listed is not a leak; it is printed with its reason, never silently. A line
#                         with no reason is a usage error. For the leaver that is legitimate (a lock folder that must outlive a run).
#   --allow-system-drive  accept a PARENT on the system drive. Without it the script refuses and creates nothing.
#   --remove              remove the folder at the end of a LEAK run too (a CLEAN run always removes its empty folder). Never
#                         done while a process still runs from it.
#   --tail N              how much go test output to show when it fails: the last N lines, and ahead of them (when the output is
#                         longer than N) the lines that name what failed -- each "--- FAIL" and "panic:" with the indented lines under
#                         it, and each "FAIL pkg" line -- because go test prints one "ok" line per later package and the tail alone
#                         holds nothing else on a whole-module run (default 20; 0 = none)
#   --verbose             show the full go test output
#   --name-prefix NAME    print the name prefix a leaked entry NAME is counted under, and exit (a check, no run)
#   --help                this text
#   Only the "--opt value" spelling is accepted.
#
# Verdicts. The last line of the output is always "TRIPWIRE-VERDICT: <verdict> ..." and the exit code follows:
#   CLEAN         0   go test passed and the folder is empty (or holds only entries the ignore list names, each printed with
#                     its reason): nothing was left
#   LEAK          1   the folder holds entries; they are listed by name prefix with their count ("lo-x-state-*")
#   PROCESSES     2   a process still runs from an image inside the folder (listed with its pid); it wins over LEAK,
#                     the leaked entries are listed as well
#   TESTS-FAILED  3   go test failed and the folder is empty: the tests that did not run were not watched
#   REFUSED       4   PARENT is on the system drive, or the host cannot tell (a stat that takes no -c, no SystemDrive), and
#                     --allow-system-drive was not given: nothing was created or run
#   (bad arguments exit 64 and print no verdict; a temp folder that vanished during the run exits 70 and prints none, because what was
#   left in it cannot be read)
# A LEAK or PROCESSES run keeps the folder (it is the evidence) and prints its path; remove it by an explicit list, or
# run with --remove. When go test fails and the folder is not empty, the verdict is still LEAK or PROCESSES.
# An interrupted run (INT, TERM or HUP) stops go test's whole process group -- go and every test binary it started, INT first so
# go test cleans its own work, then TERM, then KILL, each given a bounded time -- names any process that still runs from the
# folder, keeps the folder, removes its own capture and exits 130. It prints no verdict line.
#
# Platform notes. The process check reads /proc/PID/exe, so it needs Linux; where there is no /proc, or only an
# emulated one that lists just its own processes (Git Bash on Windows), the check is skipped, the output says so, and the
# verdict line carries "process check skipped": use the PowerShell twin there. Without the check a folder that holds entries is
# not removed, not even with --remove, because a process might still run from it. "System drive" means the filesystem that
# holds / (same device), or, where SystemDrive is set (Windows), the drive letter it names; where neither can be decided the
# script refuses, as it does for the system drive. A legitimate leaver is never ignored silently: fix the test, or name its
# prefix and reason in scripts/test-temp-tripwire.ignore (the Usage block above).
# What it cannot see. Only a child that inherits TMP, TEMP and TMPDIR writes into the watched folder. A child started with a
# scrubbed environment (the browse sidecar gets an allowlisted one) or a test that names a fixed directory (/dev/shm) writes
# elsewhere, and a CLEAN verdict says nothing about that; the lint's relaunch-proof test is what covers the scrubbed copy.

set -u
set -m # job control: go test runs in its own process group, so an interrupt can stop it AND the test binaries it started

PROG=test-temp-tripwire

EXIT_CLEAN=0
EXIT_LEAK=1
EXIT_PROCESSES=2
EXIT_TESTS_FAILED=3
EXIT_REFUSED=4
EXIT_VANISHED=70
EXIT_USAGE=64

parent='' pkgs='./...' module='' goargs='' allow_system=0 remove=0 tail_n=20 verbose=0 ignore_file='' ignore_explicit=0
unset name_prefix

say() { printf '%s: %-9s %s\n' "$PROG" "$1" "$2"; }

usage() { # the Usage and Verdicts blocks of the header above, without the comment markers
	sed -n '/^# Usage (run it/,/^# Platform notes/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//'
}

# prefixes: reads names, one per line, and prints each with its random tail replaced by *, so a thousand leaked folders are
# one line. The rules are the ones the PowerShell twin applies (no regex intervals: mawk has none).
#   lo-pipeline-lease-482910377 -> lo-pipeline-lease-*      run-graph-12.json -> run-graph-*.json
#   abc.k3Jd9Q                  -> abc.*                    plain-name         -> plain-name
prefixes() {
	awk '{
		name = $0; ext = ""
		if (match(name, /\.[A-Za-z0-9]+$/) && RSTART > 1 && length(substr(name, RSTART)) <= 6 && substr(name, RSTART - 1, 1) ~ /[0-9]/) { ext = substr(name, RSTART); name = substr(name, 1, RSTART - 1) }
		n = match(name, /[-_.][A-Za-z0-9]+$/)
		if (n) { tok = substr(name, n + 1); if (length(tok) >= 6 && tok ~ /[0-9]/) { print substr(name, 1, n) "*" ext; next } }
		if (match(name, /[0-9]+$/)) { print substr(name, 1, RSTART - 1) "*" ext; next }
		print name ext
	}'
}

# failure_digest FILE: the lines of go test output that name what failed -- every "--- FAIL", "panic:", "fatal error:" and
# "running tests:" line with the indented lines under it, and every "FAIL pkg" line -- at most 40. The tail cannot do this on a
# whole-module run: go test prints one "ok" line per package after the failing one.
failure_digest() {
	awk -v max=40 '
		function emit(l) { if (n < max) print "  ! " l; else if (n == max) print "  ! ... more in the run: --verbose shows all of it"; n++ }
		/^(--- FAIL|panic: |fatal error: |running tests:)/ { cont = 1; emit($0); next }
		/^FAIL[ \t]/ { cont = 0; emit($0); next }
		/^[ \t]/ && cont { emit($0); next }
		{ cont = 0 }
	' "$1"
}

die_usage() {
	printf '%s: usage error: %s\n' "$PROG" "$1" >&2
	printf "%s: run '%s --help' for the options\n" "$PROG" "$0" >&2
	exit "$EXIT_USAGE"
}

while [ $# -gt 0 ]; do
	case $1 in
	--dir | --pkgs | --module | --go-args | --tail | --name-prefix | --ignore-file)
		[ $# -ge 2 ] || die_usage "$1 needs a value"
		case $1 in
		--dir) parent=$2 ;;
		--pkgs) pkgs=$2 ;;
		--module) module=$2 ;;
		--go-args) goargs=$2 ;;
		--tail) tail_n=$2 ;;
		--name-prefix) name_prefix=$2 ;;
		--ignore-file) ignore_file=$2 ignore_explicit=1 ;;
		esac
		shift 2
		;;
	--allow-system-drive)
		allow_system=1
		shift
		;;
	--remove)
		remove=1
		shift
		;;
	--verbose)
		verbose=1
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*) die_usage "unknown option $1" ;;
	esac
done

if [ -n "${name_prefix+x}" ]; then # a check, not a run
	printf '%s\n' "$name_prefix" | prefixes
	exit 0
fi

[ -n "$parent" ] || die_usage "--dir is required"
[ -d "$parent" ] || die_usage "--dir '$parent' is not an existing folder"
case $tail_n in '' | *[!0-9]*) die_usage "--tail needs a number" ;; esac
[ -n "$pkgs" ] || die_usage "--pkgs is empty"
if [ -n "$module" ]; then
	[ -d "$module" ] || die_usage "--module '$module' is not an existing folder"
else
	module=$(pwd)
fi
[ -f "$module/go.mod" ] || die_usage "no go.mod in '$module': run from the module root or pass --module"
command -v go >/dev/null 2>&1 || die_usage "the Go toolchain is not on PATH"

# the reasoned ignore list: read and checked before anything runs, so a malformed list never costs a run
if [ "$ignore_explicit" = 0 ] && [ -f "$module/scripts/test-temp-tripwire.ignore" ]; then
	ignore_file="$module/scripts/test-temp-tripwire.ignore"
fi
if [ -n "$ignore_file" ]; then
	[ -f "$ignore_file" ] || die_usage "--ignore-file '$ignore_file' is not an existing file"
	bad=$(tr -d '\r' <"$ignore_file" | awk -F'\t' '/^[ \t]*(#|$)/ { next } { l = $1; r = $2; gsub(/^[ \t]+|[ \t]+$/, "", l); gsub(/^[ \t]+|[ \t]+$/, "", r); if (NF < 2 || l == "" || r == "") print NR ": " $0 }' | head -n 1)
	[ -z "$bad" ] || die_usage "line $bad of the ignore list has no name prefix, a tab and a reason"
fi

parent=$(cd "$parent" && pwd -P) || die_usage "cannot enter --dir"

# ------------------------------------------------------------------------------------------- system drive

on_system_drive() { # on_system_drive PATH: 0 when PATH is on the system drive, 1 when it is not, 2 when this host cannot tell
	local p=$1 want dev_p dev_root
	if [ -n "${SYSTEMDRIVE:-${SystemDrive:-}}" ] && command -v cygpath >/dev/null 2>&1; then
		want=$(printf '%s' "${SYSTEMDRIVE:-$SystemDrive}" | tr 'A-Z' 'a-z')
		case $(cygpath -m "$p" | tr 'A-Z' 'a-z') in "$want"/* | "$want") return 0 ;; esac
	fi
	dev_p=$(stat -c %d "$p" 2>/dev/null) && dev_root=$(stat -c %d / 2>/dev/null) && [ -n "$dev_p" ] && [ -n "$dev_root" ] || return 2
	[ "$dev_p" = "$dev_root" ]
}

sys_rc=0
on_system_drive "$parent" || sys_rc=$?
if [ "$allow_system" = 0 ] && [ "$sys_rc" != 1 ]; then
	if [ "$sys_rc" = 0 ]; then
		say refused "'$parent' is on the system drive: the temp dir of a run is not pointed there, because a leak would fill it; give --dir on a data drive, or --allow-system-drive to accept it"
		echo "TRIPWIRE-VERDICT: REFUSED ($parent is on the system drive; nothing was created or run)"
	else
		say refused "this host cannot tell whether '$parent' is on the system drive (stat -c %d failed and no SystemDrive names it): give --dir on a data drive and --allow-system-drive to accept the risk"
		echo "TRIPWIRE-VERDICT: REFUSED (cannot tell whether $parent is on the system drive; nothing was created or run)"
	fi
	exit "$EXIT_REFUSED"
fi


# ------------------------------------------------------------------------------------------------- the run

folder=$(mktemp -d "${parent%/}/test-temp-tripwire.XXXXXX") || {
	echo "$PROG: could not create a folder in $parent" >&2
	exit "$EXIT_USAGE"
}
folder=$(cd "$folder" && pwd -P)
say folder "$folder"

procs=0 proc_check=1

# count_processes: counts, and names, the processes that run from an image inside $folder. Sets procs and proc_check (0 when the
# check cannot run here).
count_processes() {
	local p exe
	procs=0 proc_check=1
	case $(uname -s 2>/dev/null) in MINGW* | MSYS* | CYGWIN*) proc_check=0 ;; esac # their /proc lists only their own processes
	if [ "$proc_check" = 1 ] && [ -r /proc/self/exe ]; then
		for p in /proc/[0-9]*; do
			exe=$(readlink "$p/exe" 2>/dev/null) || continue
			exe=${exe% (deleted)}
			case $exe in
			"$folder"/*)
				say process "pid ${p#/proc/}  $exe"
				procs=$((procs + 1))
				;;
			esac
		done
	else
		proc_check=0
		say note "no usable /proc here: the process check is skipped; use the PowerShell twin on this host"
	fi
}

# group_alive PID: 0 while the process group PID leads (or, where groups cannot be addressed, the process PID) has a member
group_alive() { kill -0 -- "-$1" 2>/dev/null || kill -0 "$1" 2>/dev/null; }

# stop_group PID: stops the process group job control gave go test -- go and every test binary it started. INT first, which is
# what a Ctrl-C would have sent, so go test ends its test binaries and cleans its own work; then TERM; then KILL. Each step gets
# a bounded time (10 s, 5 s, 5 s), and the next one runs only when a member is still there.
stop_group() {
	local sig i limit
	for sig in INT TERM KILL; do
		group_alive "$1" || return 0
		kill -"$sig" -- "-$1" 2>/dev/null || kill -"$sig" "$1" 2>/dev/null
		limit=25
		[ "$sig" = INT ] && limit=50
		for ((i = 0; i < limit; i++)); do
			group_alive "$1" || return 0
			sleep 0.2
		done
	done
}

# The run is interrupted (INT, TERM, HUP): go test is stopped together with the test binaries it started, the folder is kept as
# evidence with the processes that still run from it named, and the script's own capture goes.
out="$folder.go.out"
child=''
# drop_capture: the go test capture is the script's own file beside the folder and always goes
drop_capture() { case $out in "${parent%/}"/test-temp-tripwire.*.go.out) rm -f -- "$out" ;; esac; }
on_signal() {
	trap '' INT TERM HUP # a second signal must not cut the cleanup short
	[ -n "$child" ] && stop_group "$child"
	child=''
	say interrupted "the run was stopped; the folder is $folder (kept as evidence; remove it by an explicit list when it has been read)"
	count_processes
	[ "$procs" != 0 ] && say interrupted "$procs process(es) still run from images inside the folder"
	drop_capture
	exit 130
}
trap on_signal INT TERM HUP

# shellcheck disable=SC2086 # --pkgs and --go-args are space-separated lists on purpose
start=$(date +%s)
(
	cd "$module" || exit 125
	exec env TMP="$folder" TEMP="$folder" TMPDIR="$folder" GOTMPDIR="$folder" go test -count=1 $goargs $pkgs
) >"$out" 2>&1 </dev/null &
child=$!
wait "$child"
rc=$?
child=''
trap - INT TERM HUP
secs=$(($(date +%s) - start))
say "go test" "$pkgs (exit $rc, ${secs}s)"
if [ "$verbose" = 1 ]; then
	cat "$out"
elif [ "$rc" != 0 ] && [ "$tail_n" != 0 ]; then
	if [ "$(wc -l <"$out" | tr -d ' ')" -gt "$tail_n" ]; then # the tail does not show it all: name what failed first
		digest=$(failure_digest "$out")
		if [ -n "$digest" ]; then
			say failed "what go test reported as failed (the last $tail_n lines follow)"
			printf '%s\n' "$digest"
		fi
	fi
	tail -n "$tail_n" "$out" | sed 's/^/  | /'
fi

# ------------------------------------------------------------------------------------------ what is left

# A folder that is gone was removed by something during the run (a test that removes its own temp dir, say): an empty listing of it
# would read as CLEAN, so it is an error.
if [ ! -d "$folder" ]; then
	printf '%s: the temp folder %s is gone: something removed it during the run, so what was left cannot be read\n' "$PROG" "$folder" >&2
	drop_capture
	exit "$EXIT_VANISHED"
fi

entries=$(cd "$folder" && ls -A1 | sort)
n_total=0
[ -n "$entries" ] && n_total=$(printf '%s\n' "$entries" | wc -l | tr -d ' ')

# Group by name prefix, then split the groups the reasoned ignore list names (printed with their reason) from the leaks.
# A line of $split is "L<TAB>count<TAB>prefix" for a leak and "I<TAB>count<TAB>prefix<TAB>reason" for an ignored group.
n_entries=0 n_prefixes=0 n_ignored=0
if [ "$n_total" != 0 ]; then
	summary=$(printf '%s\n' "$entries" | prefixes | sort | uniq -c | sort -k1,1nr -k2)
	split=$(printf '%s\n' "$summary" | awk -v list="$ignore_file" '
		BEGIN {
			FS = "\t"
			while (list != "" && (getline line < list) > 0) {
				sub(/\r$/, "", line)
				if (line ~ /^[ \t]*(#|$)/) continue
				split(line, f, "\t"); label = f[1]; reason = f[2]
				gsub(/^[ \t]+|[ \t]+$/, "", label); gsub(/^[ \t]+|[ \t]+$/, "", reason)
				why[label] = reason
			}
			FS = " "
		}
		{ c = $1; sub(/^ *[0-9]+ /, ""); if ($0 in why) print "I\t" c "\t" $0 "\t" why[$0]; else print "L\t" c "\t" $0 }')
	n_entries=$(printf '%s\n' "$split" | awk -F'\t' '$1 == "L" { n += $2 } END { print n + 0 }')
	n_ignored=$(printf '%s\n' "$split" | awk -F'\t' '$1 == "I" { n += $2 } END { print n + 0 }')
	n_prefixes=$(printf '%s\n' "$split" | awk -F'\t' '$1 == "L" { n++ } END { print n + 0 }')
	if [ "$n_entries" != 0 ]; then
		say left "$n_entries entries"
		printf '%s\n' "$split" | awk -F'\t' -v prog="$PROG" '$1 == "L" { printf "%s:   %5s  %s\n", prog, $2, $3 }'
	fi
	printf '%s\n' "$split" | awk -F'\t' -v prog="$PROG" '$1 == "I" { printf "%s: ignored   %5s  %s  (%s)\n", prog, $2, $3, $4 }'
fi


count_processes

skip_note=''
[ "$proc_check" = 0 ] && skip_note=', process check skipped'

verdict='' code=0
if [ "$procs" != 0 ]; then
	verdict="PROCESSES ($procs process(es) run from images inside $folder; $n_entries entries left in $n_prefixes name prefix(es)$skip_note)"
	code=$EXIT_PROCESSES
elif [ "$n_entries" != 0 ]; then
	verdict="LEAK ($n_entries entries left in $n_prefixes name prefix(es) under $folder$skip_note)"
	code=$EXIT_LEAK
elif [ "$rc" != 0 ]; then
	verdict="TESTS-FAILED (go test exited $rc and nothing was left: the tests that did not run were not watched$skip_note)"
	code=$EXIT_TESTS_FAILED
else
	ignored_note=''
	[ "$n_ignored" != 0 ] && ignored_note="; $n_ignored entries named by the ignore list were not counted"
	verdict="CLEAN (go test passed and nothing was left in the temp folder$ignored_note$skip_note)"
	code=$EXIT_CLEAN
fi

# The empty folder of a run that left nothing goes. A folder with entries is the evidence: kept unless --remove, and never
# removed while a process runs from it -- nor when the process check could not run, because one might. The go test capture is
# always the script's own file and always goes.
remove_ok=0
if [ "$procs" = 0 ]; then
	if [ "$n_total" = 0 ]; then
		remove_ok=1
	elif [ "$proc_check" = 1 ] && { [ "$n_entries" = 0 ] || [ "$remove" = 1 ]; }; then
		remove_ok=1
	fi
fi
if [ "$remove_ok" = 1 ]; then
	case $folder in
	"${parent%/}"/test-temp-tripwire.*) rm -rf -- "$folder" ;;
	esac
elif [ "$procs" = 0 ] && [ "$proc_check" = 0 ]; then
	say kept "$folder (the process check could not run, so a process might still run from it; remove it by an explicit list when it has been read)"
else
	say kept "$folder (remove it by an explicit list when it has been read)"
fi
drop_capture

echo "TRIPWIRE-VERDICT: $verdict"
exit "$code"
