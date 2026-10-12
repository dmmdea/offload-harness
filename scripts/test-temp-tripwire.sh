#!/usr/bin/env bash
# test-temp-tripwire.sh -- run the Go tests with the OS temp dir pointed at a fresh folder, then report what is left (GPU-I8c).
#
# The problem it closes. A test that makes a file or folder in the OS temp dir and never removes it adds one more every run.
# That filled a system drive twice (a stand-in test binary left 22,434 folders and about 70 GB in under two hours), and the
# static lint (tempdir_lint_test.go, GPU-I8) cannot see a temp file made by PRODUCTION code that a test calls (F79's inline
# graph JSON), or a started copy of the test binary that exits before its cleanup. This script sees what is actually left:
# it sets TMP, TEMP, TMPDIR and GOTMPDIR to a folder it creates empty, runs go test, and looks in the folder afterwards.
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
#   --go-args "ARG ..."   extra go test arguments, space separated, e.g. "-p 1 -short" (-count=1 is always passed)
#   --ignore-file FILE    the reasoned ignore list (default: scripts/test-temp-tripwire.ignore in the module, when it exists):
#                         one entry per line, "NAME-PREFIX<TAB>reason", # comments and blank lines allowed. An entry left in the
#                         temp folder whose prefix is listed is not a leak; it is printed with its reason, never silently. A line
#                         with no reason is a usage error. For the leaver that is legitimate (a lock folder that must outlive a run).
#   --allow-system-drive  accept a PARENT on the system drive. Without it the script refuses and creates nothing.
#   --remove              remove the folder at the end of a LEAK run too (a CLEAN run always removes its empty folder). Never
#                         done while a process still runs from it.
#   --tail N              lines of go test output to show when it fails (default 20; 0 = none)
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
#   REFUSED       4   PARENT is on the system drive and --allow-system-drive was not given: nothing was created or run
#   (bad arguments exit 64 and print no verdict)
# A LEAK or PROCESSES run keeps the folder (it is the evidence) and prints its path; remove it by an explicit list, or
# run with --remove. When go test fails and the folder is not empty, the verdict is still LEAK or PROCESSES.
#
# Platform notes. The process check reads /proc/PID/exe, so it needs Linux; where there is no /proc, or only an
# emulated one that lists just its own processes (Git Bash on Windows), the check is skipped, the output says so, and the
# verdict line carries "process check skipped": use the PowerShell twin there. "System drive" means the filesystem that holds / (same device), or, where SystemDrive is set (Windows), the
# drive letter it names. A legitimate leaver is never ignored silently: fix the test, or list the file in the allowlist of
# tempdir_lint_test.go with its reason.

set -u

PROG=test-temp-tripwire

EXIT_CLEAN=0
EXIT_LEAK=1
EXIT_PROCESSES=2
EXIT_TESTS_FAILED=3
EXIT_REFUSED=4
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

on_system_drive() { # on_system_drive PATH: 0 when PATH is on the system drive
	local p=$1 want dev_p dev_root
	if [ -n "${SYSTEMDRIVE:-${SystemDrive:-}}" ] && command -v cygpath >/dev/null 2>&1; then
		want=$(printf '%s' "${SYSTEMDRIVE:-$SystemDrive}" | tr 'A-Z' 'a-z')
		case $(cygpath -m "$p" | tr 'A-Z' 'a-z') in "$want"/* | "$want") return 0 ;; esac
	fi
	dev_p=$(stat -c %d "$p" 2>/dev/null) || return 1
	dev_root=$(stat -c %d / 2>/dev/null) || return 1
	[ -n "$dev_p" ] && [ "$dev_p" = "$dev_root" ]
}

if [ "$allow_system" = 0 ] && on_system_drive "$parent"; then
	say refused "'$parent' is on the system drive: the temp dir of a run is not pointed there, because a leak would fill it; give --dir on a data drive, or --allow-system-drive to accept it"
	echo "TRIPWIRE-VERDICT: REFUSED ($parent is on the system drive; nothing was created or run)"
	exit "$EXIT_REFUSED"
fi

# ------------------------------------------------------------------------------------------------- the run

folder=$(mktemp -d "${parent%/}/test-temp-tripwire.XXXXXX") || {
	echo "$PROG: could not create a folder in $parent" >&2
	exit "$EXIT_USAGE"
}
folder=$(cd "$folder" && pwd -P)
say folder "$folder"

# go test runs in its own process group so a signal to this script reaches it, and the folder is reported on every exit.
out="$folder.go.out"
child=''
on_signal() {
	[ -n "$child" ] && kill -TERM "$child" 2>/dev/null
	say interrupted "the run was stopped; the folder is $folder"
	exit 130
}
trap on_signal INT TERM HUP

# shellcheck disable=SC2086 # --pkgs and --go-args are space-separated lists on purpose
start=$(date +%s)
(
	cd "$module" || exit 125
	exec env TMP="$folder" TEMP="$folder" TMPDIR="$folder" GOTMPDIR="$folder" go test -count=1 $goargs $pkgs
) >"$out" 2>&1 &
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
	tail -n "$tail_n" "$out" | sed 's/^/  | /'
fi

# ------------------------------------------------------------------------------------------ what is left

entries=$(cd "$folder" && ls -A1 2>/dev/null | sort)
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

# The empty folder of a run that left nothing goes. A folder with entries is the evidence: kept unless --remove, and
# never removed while a process runs from it. The go test capture is always the script's own file and always goes.
if [ "$procs" = 0 ] && { [ "$n_entries" = 0 ] || [ "$remove" = 1 ]; }; then
	case $folder in
	"${parent%/}"/test-temp-tripwire.*) rm -rf -- "$folder" ;;
	esac
else
	say kept "$folder (remove it by an explicit list when it has been read)"
fi
case $out in "${parent%/}"/test-temp-tripwire.*.go.out) rm -f -- "$out" ;; esac

echo "TRIPWIRE-VERDICT: $verdict"
exit "$code"
