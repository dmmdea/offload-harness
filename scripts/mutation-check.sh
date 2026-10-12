#!/usr/bin/env bash
# mutation-check.sh -- prove a test CAN fail: apply one single-line mutation, run the tests, restore the file (H-17).
#
# The problem it closes. A proof that "this test catches that bug" used to be a throw-away script: apply a
# mutation, run the tests, count ANY failure as RED. Two ways that lied:
#   * a mutant that does not COMPILE also fails the test command, and was scored as a killed mutant;
#   * a mutation that never landed (an LF pattern on a CRLF file) changed nothing, and the unchanged tree's
#     result was reported as the mutant's.
# This is the one harness for that proof. PowerShell twin: scripts/mutation-check.ps1 (same options, same
# verdicts, same exit codes). Self-test: scripts/mutation-check-selftest.sh.
#
# What a run does, in this order:
#   1. prepares the mutated copy in a temp dir and asserts the mutation LANDS: the bytes differ, they differ
#      beyond line endings, exactly one line differs, and every byte outside that line is identical. Nothing
#      has been run and the file has not been touched yet, so a bad mutation fails in milliseconds.
#   2. CONTROL arm: the unmutated tree is compiled and tested. Both must pass, or nothing can be proven. With the
#      default go test command a control run that executed no test at all (a mistyped --run) is refused too:
#      it would otherwise score every mutant SURVIVED.
#   3. writes the mutation into the file and checks the bytes on disk are the prepared ones.
#   4. COMPILES first (go test -c on the package, go vet for several, or your --compile command). A build
#      failure is BAD-MUTANT, never RED: a mutant that does not build killed nothing.
#   5. runs the tests: they fail = RED (the mutant was killed), they pass = SURVIVED (a gap in the tests).
#   6. restores the file byte for byte and asserts it (cmp against the pristine copy, plus a digest compare),
#      on every path: normal end, an unexpected abort, SIGINT, SIGTERM and SIGHUP. The step being run is
#      killed first, so a signal is never deferred until a long test run ends.
#
# Usage (run it from the module root, or pass --dir):
#   mutation-check.sh --file PATH (--old TEXT | --marker TEXT) --new TEXT (--pkg PATTERN | --test CMD) [options]
#
#   --file PATH      the source file to mutate; a relative path resolves against --dir
#   --old TEXT       exact text to replace; it must occur once on one line
#   --marker TEXT    picks the ONE line that contains TEXT. With --old, --old is replaced inside that line;
#                    without --old, the WHOLE line is replaced (its indentation and line ending are kept)
#   --new TEXT       the replacement; --new '' deletes the --old text
#   --pkg PATTERN    Go package pattern. Default compile: go test -c -o <tmp> PATTERN (one package), or
#                    go vet PATTERN (a "..." pattern or several, since go test -c takes one). Default tests:
#                    go test -count=1 [-run RE] PATTERN
#   --run RE         with --pkg: go test -run RE
#   --test CMD       the test command, run with bash -c; exit 0 means the tests pass. Overrides the default
#   --compile CMD    the command that must succeed on the mutant before the tests run (bash -c). Required
#                    when there is no --pkg. For other languages: a syntax check, a build, a type check
#   --dir DIR        working directory for everything (default: the current directory)
#   --tail N         lines of command output to show on a failure (default 20; 0 = none)
#   --verbose        show the full output of every step
#   --help           this text
#   Only the "--opt value" spelling is accepted (not --opt=value). --old, --marker and --new must be one line.
#
# Verdicts. The last line of the output is always "MUTATION-VERDICT: <verdict> ..." and the exit code follows:
#   RED              0   the mutant compiled and the tests failed: the tests catch this mutation
#   SURVIVED         1   the mutant compiled and the tests passed: the tests do NOT catch it
#   BAD-MUTANT       2   the mutant does not compile: not a kill, the proof is void; pick another mutation
#   CONTROL-FAILED   3   the unmutated tree failed to compile or test: nothing can be attributed to a mutation
#   NOT-APPLIED      4   the mutation did not land (text absent, ambiguous, a no-op, an unwritable file)
#   RESTORE-FAILED   5   the file is NOT back to its original bytes; the message names the pristine copy
#   INTERRUPTED  128+n   a signal ended the run; the file was restored first (130 INT, 143 TERM, 129 HUP)
#   (bad arguments exit 64 and print no verdict)
#
# Platform notes. Line endings are handled byte-exact: on Git Bash for Windows, sed, awk, grep and $(...) drop the
# CR of a CRLF line, so the original bytes are only ever read by head, tail, cat, cp, cmp, tr and od (all exact),
# the search runs on a CR-stripped copy, and the mutated file is spliced as head + new line + tail with the
# line's own terminator (CRLF, LF, or none on an unterminated last line). The temp dir is
# $MUTATION_CHECK_TMPDIR, else $TMPDIR, else /tmp. If a run is killed with SIGKILL nothing can restore the file;
# the pristine copy's path is printed at the start so it can be put back by hand. A shell started with a signal
# ignored (a background job of a script has SIGINT ignored, nohup ignores SIGHUP) cannot trap it; that is bash's
# rule, not a choice here, and Ctrl-C in a terminal is unaffected. Do not run two instances against the same file.

set -u
set -m # job control: each step runs in its own process group, so a signal can stop the step AND its children

PROG=mutation-check
BASH_BIN=${BASH:-bash}

EXIT_RED=0
EXIT_SURVIVED=1
EXIT_BAD_MUTANT=2
EXIT_CONTROL_FAILED=3
EXIT_NOT_APPLIED=4
EXIT_RESTORE_FAILED=5
EXIT_USAGE=64

file='' old='' marker='' new=''
have_old=0 have_marker=0 have_new=0
pkg='' run_re='' test_cmd='' compile_cmd='' workdir=''
show_lines=20
verbose=0

work=''        # temp dir holding the pristine copy and every log
orig=''        # the pristine copy of the file
keep_work=0    # 1 when the pristine copy must outlive the run (restore failed)
sum_before=''  # digest of the file before the run
file_abs=''    # the file's absolute path, for the recovery hints
child=''       # pid of the step being run
finished=0
prepared=0     # 1 once the mutated copy exists, so the result line can quote the mutation
RC=0 SECS=0 RESTORE_OK=1 RESTORE_NOTE=''
N=0 content='' newcontent='' eol='' term=''

# ---------------------------------------------------------------------------------------------------- output

say() { printf '%s: %-9s %s\n' "$PROG" "$1" "$2"; }

usage() { # the Usage and Verdicts blocks of the header above, without the comment markers
	sed -n '/^# Usage (run it/,/^# Platform notes/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//'
}

die_usage() {
	printf '%s: usage error: %s\n' "$PROG" "$1" >&2
	printf "%s: run '%s --help' for the options\n" "$PROG" "$0" >&2
	exit "$EXIT_USAGE"
}

# ------------------------------------------------------------------------------------------------- arguments

while [ $# -gt 0 ]; do
	case $1 in
	--file | --old | --marker | --new | --pkg | --run | --test | --compile | --dir | --tail)
		[ $# -ge 2 ] || die_usage "$1 needs a value"
		case $1 in
		--file) file=$2 ;;
		--old) old=$2 have_old=1 ;;
		--marker) marker=$2 have_marker=1 ;;
		--new) new=$2 have_new=1 ;;
		--pkg) pkg=$2 ;;
		--run) run_re=$2 ;;
		--test) test_cmd=$2 ;;
		--compile) compile_cmd=$2 ;;
		--dir) workdir=$2 ;;
		--tail) show_lines=$2 ;;
		esac
		shift 2
		;;
	--verbose)
		verbose=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die_usage "unknown argument: $1 (options take a separate value: --opt value)" ;;
	esac
done

[ -n "$file" ] || die_usage "--file is required"
[ "$have_new" -eq 1 ] || die_usage "--new is required (use --new '' to delete the --old text)"
if [ "$have_old" -eq 0 ] && [ "$have_marker" -eq 0 ]; then
	die_usage "one of --old or --marker is required"
fi
if [ "$have_old" -eq 1 ] && [ -z "$old" ]; then die_usage "--old must not be empty"; fi
if [ "$have_marker" -eq 1 ] && [ -z "$marker" ]; then die_usage "--marker must not be empty"; fi
case $old$marker$new in
*$'\n'* | *$'\r'*)
	die_usage "--old, --marker and --new must each be ONE line (no CR or LF): a mutation is a single-line replacement, and a multi-line pattern is how an LF pattern silently misses a CRLF file"
	;;
esac
case $show_lines in
'' | *[!0-9]*) die_usage "--tail needs a number" ;;
esac
if [ -z "$pkg" ] && [ -z "$test_cmd" ]; then die_usage "give --pkg (Go) or --test: what runs the tests?"; fi
if [ -z "$pkg" ] && [ -z "$compile_cmd" ]; then
	die_usage "give --compile when there is no --pkg: the compile step is what tells BAD-MUTANT from RED"
fi
if [ -n "$run_re" ] && { [ -z "$pkg" ] || [ -n "$test_cmd" ]; }; then
	die_usage "--run only applies to the default test command (--pkg without --test)"
fi

if [ -n "$workdir" ]; then
	cd -- "$workdir" 2>/dev/null || die_usage "--dir not found: $workdir"
fi
[ -f "$file" ] || die_usage "file not found: $file (a relative path resolves against --dir, default the current directory)"
[ -r "$file" ] && [ -w "$file" ] || die_usage "the file is not both readable and writable: $file"

for tool in head tail cat cp cmp tr od wc diff grep cut mktemp sleep; do
	command -v "$tool" >/dev/null 2>&1 || die_usage "a required tool is missing from PATH: $tool"
done

# ------------------------------------------------------------------------------------------------- helpers

sha_of() { # sha_of FILE: digest of the bytes (sha256 when a tool exists, else cksum)
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum <"$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 <"$1" | cut -d' ' -f1
	else
		cksum <"$1" | cut -d' ' -f1,2 | tr ' ' '-'
	fi
}

short() { printf '%s' "$1" | cut -c1-12; }

count_bytes() { # count_bytes CHAR FILE: how many CHAR bytes the file holds (tr is byte exact)
	echo $(($(tr -cd "$1" <"$2" | wc -c)))
}

describe_eol() { # describe_eol FILE: "LF", "CRLF" or "mixed", and whether the last line is unterminated
	local lf cr style last
	lf=$(($(wc -l <"$1")))
	cr=$(count_bytes '\r' "$1")
	if [ "$cr" -eq 0 ]; then
		style=LF
	elif [ "$cr" -eq "$lf" ]; then
		style=CRLF
	else
		style="mixed ($cr CR, $lf LF)"
	fi
	if [ -s "$1" ]; then
		last=$(tail -c 1 "$1" | od -An -tx1 | tr -d ' \n')
		[ "$last" = 0a ] || style="$style, no final newline"
	fi
	printf '%s' "$style"
}

show_log() { # show_log LOG head|tail|all: the log, CR-stripped, indented so it reads as part of the report
	local log=$1 how=$2
	{
		if [ "$verbose" -eq 1 ] || [ "$how" = all ]; then
			tr -d '\r' <"$log"
		elif [ "$show_lines" -gt 0 ]; then
			if [ "$how" = head ]; then
				tr -d '\r' <"$log" | head -n "$show_lines"
			else
				tr -d '\r' <"$log" | tail -n "$show_lines"
			fi
		fi
	} | while IFS= read -r line || [ -n "$line" ]; do
		printf '%s:           | %s\n' "$PROG" "$line"
	done
}

nap() { sleep 0.2 2>/dev/null || sleep 1; } # a fifth of a second where sleep takes fractions

stop_step() { # stop_step PID: end the running step and its children; bounded, because the restore must not wait on it
	local pid=$1 i
	kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null
	for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
		kill -0 "$pid" 2>/dev/null || return 0
		nap
	done
	kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null
	for i in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		nap
	done
	return 0
}

run_logged() { # run_logged LOG CMD ARGS...: run in the background and wait, so a trapped signal fires at once
	local log=$1 t0=$SECONDS
	shift
	"$@" >"$log" 2>&1 </dev/null &
	child=$!
	{ wait "$child"; } 2>/dev/null
	RC=$?
	child=''
	SECS=$((SECONDS - t0))
}

go_single_pkg() { # 0 when --pkg names exactly one package (go test -c takes one)
	case $pkg in
	*'...'* | *' '* | *$'\t'*) return 1 ;;
	esac
	return 0
}

step_compile() { # step_compile LOG
	if [ -n "$compile_cmd" ]; then
		run_logged "$1" "$BASH_BIN" -c "$compile_cmd"
	elif go_single_pkg; then
		run_logged "$1" go test -c -o "$work/compiled.test" "$pkg"
	else
		# shellcheck disable=SC2086 # several package patterns are meant to be split
		run_logged "$1" go vet $pkg
	fi
}

step_tests() { # step_tests LOG
	if [ -n "$test_cmd" ]; then
		run_logged "$1" "$BASH_BIN" -c "$test_cmd"
	elif [ -n "$run_re" ]; then
		# shellcheck disable=SC2086
		run_logged "$1" go test -count=1 -run "$run_re" $pkg
	else
		# shellcheck disable=SC2086
		run_logged "$1" go test -count=1 $pkg
	fi
}

describe_compile() {
	if [ -n "$compile_cmd" ]; then
		printf '%s' "$compile_cmd"
	elif go_single_pkg; then
		printf 'go test -c %s' "$pkg"
	else
		printf 'go vet %s' "$pkg"
	fi
}

describe_tests() {
	if [ -n "$test_cmd" ]; then
		printf '%s' "$test_cmd"
	elif [ -n "$run_re" ]; then
		printf 'go test -count=1 -run %s %s' "$run_re" "$pkg"
	else
		printf 'go test -count=1 %s' "$pkg"
	fi
}

# ------------------------------------------------------------------------------- restore, finish, cleanup

restore_original() { # puts the pristine bytes back and proves it; sets RESTORE_OK and RESTORE_NOTE
	local i after
	RESTORE_OK=0
	RESTORE_NOTE=''
	if [ -z "$orig" ] || [ ! -f "$orig" ]; then
		RESTORE_OK=1
		RESTORE_NOTE='no pristine copy was taken, so the file was never touched'
		return
	fi
	for i in 1 2 3 4 5; do
		if ! cmp -s "$orig" "$file" 2>/dev/null; then
			cp -p -- "$orig" "$file" 2>/dev/null || cat "$orig" >"$file" 2>/dev/null
		fi
		if cmp -s "$orig" "$file" 2>/dev/null; then
			RESTORE_OK=1
			break
		fi
		sleep 1 # a scanner or an editor can hold the file for a moment on Windows
	done
	if [ "$RESTORE_OK" -eq 1 ]; then
		after=$(sha_of "$file")
		if [ "$after" = "$sum_before" ]; then
			RESTORE_NOTE="byte for byte: cmp equal, digest $(short "$after") == $(short "$sum_before")"
		else
			RESTORE_OK=0
			RESTORE_NOTE="cmp says equal but the digests differ ($(short "$after") vs $(short "$sum_before"))"
		fi
	else
		RESTORE_NOTE="cmp still differs after 5 attempts"
	fi
}

finish() { # finish VERDICT CODE WHY: the only normal way out
	local verdict=$1 code=$2 why=$3
	if [ "$finished" -eq 1 ]; then exit "$code"; fi
	finished=1
	trap '' INT TERM HUP # a second Ctrl-C must not interrupt the restore
	if [ -n "$child" ]; then
		stop_step "$child"
		child=''
	fi
	restore_original
	if [ "$RESTORE_OK" -eq 1 ]; then
		say restore "OK, $RESTORE_NOTE"
	else
		keep_work=1
		say restore "FAILED: $RESTORE_NOTE"
		say restore "the pristine copy is kept at: $orig"
		say restore "put it back by hand: cp -p '$orig' '$file_abs'"
		verdict=RESTORE-FAILED
		code=$EXIT_RESTORE_FAILED
		why="the file is not back to its original bytes"
	fi
	if [ "$prepared" -eq 1 ]; then
		say result "$verdict -- $file line $N: '$content' -> '$newcontent'"
	fi
	printf 'MUTATION-VERDICT: %s (%s)\n' "$verdict" "$why"
	exit "$code"
}

on_exit() {
	if [ "$finished" -eq 0 ]; then
		finished=1
		trap '' INT TERM HUP
		restore_original
		if [ "$RESTORE_OK" -eq 1 ]; then
			say restore "OK after an unexpected abort: $RESTORE_NOTE"
		else
			keep_work=1
			say restore "FAILED after an unexpected abort: $RESTORE_NOTE; pristine copy: $orig"
		fi
	fi
	if [ "$keep_work" -eq 0 ] && [ -n "$work" ] && [ -d "$work" ]; then
		rm -rf -- "$work"
	fi
}

on_signal() { # on_signal NAME CODE
	say interrupt "received SIG$1: stopping the running step and restoring $file"
	finish INTERRUPTED "$2" "SIG$1 ended the run before a verdict; the file was restored first"
}

fail_not_applied() {
	say mutation "NOT APPLIED: $1"
	finish NOT-APPLIED "$EXIT_NOT_APPLIED" "$1"
}

# ------------------------------------------------------------------------------------------------- set up

tmp_base=${MUTATION_CHECK_TMPDIR:-${TMPDIR:-/tmp}}
work=$(mktemp -d "${tmp_base%/}/mutation-check.XXXXXX") || die_usage "cannot create a temp dir under $tmp_base"
trap on_exit EXIT
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap 'on_signal HUP 129' HUP

orig="$work/original"
cp -p -- "$file" "$orig" 2>/dev/null || cat -- "$file" >"$orig"
if ! cmp -s "$file" "$orig"; then
	orig='' # no trustworthy copy: refuse to go on rather than risk a file we cannot put back
	finished=1
	die_usage "cannot take an exact copy of $file"
fi
sum_before=$(sha_of "$orig")
file_abs=$(cd -- "$(dirname -- "$file")" 2>/dev/null && pwd)/$(basename -- "$file") # for the recovery hints: valid from any directory

say file "$file ($(describe_eol "$orig"))"
say backup "pristine copy $orig (digest $(short "$sum_before")); if this run is killed hard, put it back with: cp -p '$orig' '$file_abs'"

# ------------------------------------------------------------------------- 1. prepare and prove it lands

norm="$work/original.norm"
tr -d '\r' <"$orig" >"$norm"
if ! tr -d '\000' <"$orig" | cmp -s - "$orig"; then
	fail_not_applied "the file holds NUL bytes: it is not a text source file"
fi

if [ "$have_marker" -eq 1 ]; then
	sel=$marker sel_name=--marker
else
	sel=$old sel_name=--old
fi
hits=$(LC_ALL=C grep -n -F -- "$sel" "$norm" | LC_ALL=C cut -d: -f1 | tr '\n' ' ')
hits=${hits% }
# shellcheck disable=SC2086
set -- $hits
if [ $# -eq 0 ]; then fail_not_applied "the $sel_name text is not in $file: $sel"; fi
if [ $# -gt 1 ]; then
	fail_not_applied "the $sel_name text matches $# lines ($hits): make it unique, or pick the line with --marker"
fi
N=$1
content=$(head -n "$N" "$norm" | tail -n 1)

if [ "$have_old" -eq 1 ]; then
	cnt=0
	rest=$content
	while :; do
		case $rest in
		*"$old"*)
			cnt=$((cnt + 1))
			rest=${rest#*"$old"}
			;;
		*) break ;;
		esac
	done
	if [ "$cnt" -eq 0 ]; then fail_not_applied "line $N (picked by --marker) does not contain the --old text: $old"; fi
	if [ "$cnt" -gt 1 ]; then fail_not_applied "the --old text occurs $cnt times on line $N: make it unique"; fi
	newcontent=${content%%"$old"*}$new${content#*"$old"}
else
	indent=${content%%[![:space:]]*}
	newcontent=$indent$new
fi
if [ "$newcontent" = "$content" ]; then fail_not_applied "the mutation is a no-op: line $N would not change"; fi

# the line's own terminator, read from the ORIGINAL bytes (never through sed, awk or $(...))
head -n "$N" "$orig" | tail -n 1 >"$work/line.orig"
last2=$(tail -c 2 "$work/line.orig" | od -An -tx1 | tr -d ' \n')
case $last2 in
*0d0a) eol=$'\r\n' term=CRLF allowed_cr=1 ;;
*0a) eol=$'\n' term=LF allowed_cr=0 ;;
*0d) eol=$'\r' term='CR (unterminated last line)' allowed_cr=1 ;;
*) eol='' term='none (unterminated last line)' allowed_cr=0 ;;
esac
if [ "$(count_bytes '\r' "$work/line.orig")" -gt "$allowed_cr" ]; then
	fail_not_applied "line $N holds a CR that is not its line ending: this harness mutates ordinary text lines"
fi

mutated="$work/mutated"
{
	head -n $((N - 1)) "$orig"
	printf '%s' "$newcontent"
	printf '%s' "$eol"
	tail -n +$((N + 1)) "$orig"
} >"$mutated"

prepared=1
say mutation "$file line $N (line ending kept: $term)"
say '' "- $content"
say '' "+ $newcontent"

# the landing assertions, on the prepared copy, before anything runs
if cmp -s "$orig" "$mutated"; then fail_not_applied "the mutated copy is byte-identical to the original"; fi
mutnorm="$work/mutated.norm"
tr -d '\r' <"$mutated" >"$mutnorm"
if cmp -s "$norm" "$mutnorm"; then fail_not_applied "only line endings differ: the mutation did not land"; fi
dlines=$(diff "$norm" "$mutnorm")
removed=$(printf '%s\n' "$dlines" | grep -c '^<' || true)
added=$(printf '%s\n' "$dlines" | grep -c '^>' || true)
if [ "$removed" -ne 1 ] || [ "$added" -ne 1 ]; then
	fail_not_applied "expected exactly one line to differ, found $removed removed and $added added"
fi
head -n $((N - 1)) "$orig" >"$work/a"
head -n $((N - 1)) "$mutated" >"$work/b"
if ! cmp -s "$work/a" "$work/b"; then fail_not_applied "bytes before line $N changed"; fi
tail -n +$((N + 1)) "$orig" >"$work/a"
tail -n +$((N + 1)) "$mutated" >"$work/b"
if ! cmp -s "$work/a" "$work/b"; then fail_not_applied "bytes after line $N changed"; fi
say landed "yes: exactly 1 line differs (line $N), every other byte is identical, line endings compared normalised"

# --------------------------------------------------------------------------------------- 2. control arm

say control "unmutated tree: compile, then tests; both must pass"
say '' "compile: $(describe_compile)"
say '' "tests:   $(describe_tests)"
step_compile "$work/control.compile.log"
if [ "$RC" -ne 0 ]; then
	say control "the compile step FAILED on the unmutated tree (exit $RC, ${SECS}s)"
	show_log "$work/control.compile.log" head
	finish CONTROL-FAILED "$EXIT_CONTROL_FAILED" "the unmutated tree does not compile with the compile command, so no result can be attributed to the mutation"
fi
[ "$verbose" -eq 1 ] && show_log "$work/control.compile.log" all
step_tests "$work/control.tests.log"
if [ "$RC" -ne 0 ]; then
	say control "the tests FAILED on the unmutated tree (exit $RC, ${SECS}s)"
	show_log "$work/control.tests.log" tail
	finish CONTROL-FAILED "$EXIT_CONTROL_FAILED" "the tests already fail without the mutation, so a failure with it proves nothing"
fi
[ "$verbose" -eq 1 ] && show_log "$work/control.tests.log" all
# go test exits 0 when a mistyped --run matches nothing ("ok  pkg  0.01s [no tests to run]") or the package has no tests
# ("?  pkg  [no test files]"): a control arm that ran nothing would score every mutant SURVIVED
if [ -z "$test_cmd" ] && ! tr -d '\r' <"$work/control.tests.log" | grep -E '^ok[[:space:]]' | grep -v -F '[no tests to run]' | grep -q .; then
	say control "go test exited 0 but ran no test: a mistyped --run, or a package without tests"
	show_log "$work/control.tests.log" tail
	finish CONTROL-FAILED "$EXIT_CONTROL_FAILED" "the control run executed no tests, so every mutant would score SURVIVED; fix --pkg or --run"
fi
say control "PASS: the unmutated tree compiles and its tests pass (${SECS}s for the tests)"

# ------------------------------------------------------------------------------- 3. write the mutation

if ! cat "$mutated" >"$file" 2>/dev/null; then
	fail_not_applied "cannot write $file"
fi
if ! cmp -s "$mutated" "$file"; then fail_not_applied "the bytes on disk are not the prepared mutation"; fi
say applied "the mutation is on disk ($(short "$(sha_of "$file")") now, $(short "$sum_before") before)"

# -------------------------------------------------------------------------------- 4. compile the mutant

step_compile "$work/mutant.compile.log"
if [ "$RC" -ne 0 ]; then
	say compile "FAILED on the mutant (exit $RC): it does not build, so this is NOT a kill"
	show_log "$work/mutant.compile.log" head
	finish BAD-MUTANT "$EXIT_BAD_MUTANT" "the mutant does not compile, so a test failure here would prove nothing; choose a mutation that builds"
fi
[ "$verbose" -eq 1 ] && show_log "$work/mutant.compile.log" all
say compile "ok: the mutant builds (${SECS}s)"

# ----------------------------------------------------------------------------------- 5. run the tests

step_tests "$work/mutant.tests.log"
if [ "$RC" -eq 0 ]; then
	[ "$verbose" -eq 1 ] && show_log "$work/mutant.tests.log" all
	say tests "PASSED on the mutant (${SECS}s)"
	finish SURVIVED "$EXIT_SURVIVED" "the mutant compiles and the tests pass: they do not catch this mutation"
fi
say tests "FAILED on the mutant (exit $RC, ${SECS}s)"
show_log "$work/mutant.tests.log" tail
finish RED "$EXIT_RED" "the mutant compiles and the tests fail: the tests catch this mutation"
