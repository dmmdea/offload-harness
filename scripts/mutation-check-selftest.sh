#!/usr/bin/env bash
# mutation-check-selftest.sh -- the mutation harness proves itself (H-17).
#
# Runs scripts/mutation-check.sh against the tiny fixture module in scripts/testdata/mutfixture, on a COPY in a temp
# dir (the tracked fixture is never mutated), and asserts for every case the verdict, the exit code and that the
# mutated file came back byte for byte -- checked here with cmp against a snapshot taken before the run, not on the
# harness's word.
#
# The three verdicts the harness exists to tell apart, on the LF fixture:
#   RED          a mutation the tests catch                          Clamp: `v < lo` -> `v > lo`
#   SURVIVED     a mutation the tests do not catch (a real gap)      Band: `case n < 10:` -> `case n < 5:` (the "small" band is untested)
#   BAD-MUTANT   a mutation that does not compile (NOT a kill)       Clamp: `return lo` -> `return undefinedName`
# Then the traps that made the old proof scripts lie: a CRLF copy (the same mutations, the line endings must
# survive), whole-line and marker mutations, a mixed-ending file, an unterminated last line, a mutation that cannot
# land (text absent, ambiguous, a no-op, a multi-line pattern), an unmutated tree whose tests already fail, and a
# SIGTERM during the mutant's test run (the file must be restored, and the wait must not outlast the signal).
#
# Usage: bash scripts/mutation-check-selftest.sh          (needs the Go toolchain on PATH; exits 1 if any check fails)
# Scratch: $MUTATION_CHECK_TMPDIR, else $TMPDIR, else /tmp; removed on exit.

set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
harness="$here/mutation-check.sh"
bash_bin=${BASH:-bash} # run the harness through bash, so a checkout without the exec bit still works
fixture="$here/testdata/mutfixture"

command -v go >/dev/null 2>&1 || {
	echo "selftest: the Go toolchain is not on PATH; the fixture is a Go module" >&2
	exit 2
}
[ -f "$harness" ] && [ -d "$fixture" ] || {
	echo "selftest: cannot find $harness or $fixture" >&2
	exit 2
}

export GOWORK=off GOTOOLCHAIN=local # the fixture is a module of its own; never resolve it through a workspace

base=${MUTATION_CHECK_TMPDIR:-${TMPDIR:-/tmp}}
root=$(mktemp -d "${base%/}/mutation-check-selftest.XXXXXX") || exit 2
trap 'rm -rf -- "$root"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass=0
fail=0
skip=0
n=0
TAB=$(printf '\t')

sha_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum <"$1" | cut -d' ' -f1
	else
		shasum -a 256 <"$1" | cut -d' ' -f1
	fi
}

expect() { # expect WHAT GOT WANT
	if [ "$2" = "$3" ]; then
		pass=$((pass + 1))
		printf 'selftest:   ok    %s (%s)\n' "$1" "$2"
	else
		fail=$((fail + 1))
		printf 'selftest:   FAIL  %s: got [%s], want [%s]\n' "$1" "$2" "$3"
	fi
}

to_crlf() { # to_crlf FILE: rewrite with CRLF endings using bash builtins only, so the bytes are exact everywhere
	local f=$1 line
	tr -d '\r' <"$f" >"$f.tmp"
	while IFS= read -r line || [ -n "$line" ]; do
		printf '%s\r\n' "$line"
	done <"$f.tmp" >"$f"
	rm -f -- "$f.tmp"
}

to_lf() {
	tr -d '\r' <"$1" >"$1.tmp" && cat "$1.tmp" >"$1" && rm -f -- "$1.tmp"
}

# case_run NAME WANT_VERDICT WANT_EXIT WATCHED SHOW -- HARNESS_ARGS...
#   SHOW=full prints the harness's whole report (the headline cases); SHOW=brief drops the backup line and log tails.
case_run() {
	local name=$1 want_v=$2 want_c=$3 watched=$4 show=$5 out snap rc verdict
	shift 6
	n=$((n + 1))
	out="$root/case$n.out"
	snap="$root/case$n.snap"
	cp -p -- "$watched" "$snap"
	printf '\nselftest: ===== case %s: %s\n' "$n" "$name"
	"$bash_bin" "$harness" "$@" >"$out" 2>&1
	rc=$?
	if [ "$show" = full ]; then
		grep -v '^mutation-check: backup' "$out"
	else
		grep -v -e '^mutation-check: backup' -e '^mutation-check:           | ' "$out"
	fi
	verdict=$(grep '^MUTATION-VERDICT:' "$out" | tail -n 1 | cut -d' ' -f2)
	expect "verdict" "${verdict:-none}" "$want_v"
	expect "exit code" "$rc" "$want_c"
	if cmp -s "$snap" "$watched"; then
		expect "$(basename "$watched") restored byte for byte (cmp against the pre-run snapshot)" yes yes
	else
		expect "$(basename "$watched") restored byte for byte (cmp against the pre-run snapshot)" DIFFERS yes
	fi
	LAST_OUT=$out
}

# ---------------------------------------------------------------------------------------------- fixtures

lf="$root/lf"
crlf="$root/crlf"
cp -r "$fixture" "$lf"
for f in "$lf"/*; do to_lf "$f"; done # a Windows checkout may have turned the fixture into CRLF; start from LF
cp -r "$lf" "$crlf"
for f in "$crlf"/*.go; do to_crlf "$f"; done
printf 'selftest: fixture copies in %s (lf: %s CR bytes, crlf: %s CR bytes in clamp.go)\n' "$root" \
	"$(tr -cd '\r' <"$lf/clamp.go" | wc -c | tr -d ' ')" "$(tr -cd '\r' <"$crlf/clamp.go" | wc -c | tr -d ' ')"

# the tracked fixture must come out of the self-test exactly as it went in
fixture_sum=$(cat "$fixture"/* | sha256sum | cut -d' ' -f1)

# ------------------------------------------------------------------------- the three headline verdicts

case_run "RED: the tests catch Clamp's lower bound flipped" RED 0 "$lf/clamp.go" full -- \
	--dir "$lf" --file clamp.go --old 'v < lo' --new 'v > lo' --pkg .

case_run "SURVIVED: nothing tests Band's \"small\" band, so moving its upper edge goes unnoticed" SURVIVED 1 "$lf/clamp.go" full -- \
	--dir "$lf" --file clamp.go --old 'case n < 10:' --new 'case n < 5:' --pkg .

case_run "BAD-MUTANT: an undefined name does not compile, so it is NOT a kill" BAD-MUTANT 2 "$lf/clamp.go" full -- \
	--dir "$lf" --file clamp.go --old 'return lo' --new 'return undefinedName' --pkg .

# ------------------------------------------------------------------------- marker mode (whole line, or inside it)

case_run "RED, marker mode: the whole line is replaced and keeps its indentation" RED 0 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --marker 'case n == 0:' --new 'case n == 1:' --pkg . --run TestBand
if grep -q -F "+ ${TAB}case n == 1:" "$LAST_OUT"; then
	expect "the replaced line kept its tab indentation" kept kept
else
	expect "the replaced line kept its tab indentation" lost kept
fi

case_run "RED, marker picks the line and --old narrows inside it ('lo' alone is ambiguous in the file)" RED 0 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --marker 'return lo' --old 'lo' --new 'hi' --pkg . --run TestClamp

# ------------------------------------------------------------------------- the CRLF file (the old fake pass)

case_run "RED on a CRLF file: the same mutation, and the CRLF endings must come back" RED 0 "$crlf/clamp.go" brief -- \
	--dir "$crlf" --file clamp.go --old 'v < lo' --new 'v > lo' --pkg .

case_run "RED on a CRLF file, whole-line marker mutation" RED 0 "$crlf/clamp.go" brief -- \
	--dir "$crlf" --file clamp.go --marker 'case n == 0:' --new 'case n == 1:' --pkg . --run TestBand

crlf_probe='[ $(( $(tr -cd "\r" < clamp.go | wc -c) )) -eq $(( $(wc -l < clamp.go) )) ]'
case_run "CRLF stays CRLF on the mutated line (the probe passes only if every line of the MUTANT ends in CRLF)" SURVIVED 1 "$crlf/clamp.go" brief -- \
	--dir "$crlf" --file clamp.go --old 'v < lo' --new 'v > lo' --compile true --test "$crlf_probe"

case_run "the same probe on the LF copy fails its control arm, so the probe can tell the two apart" CONTROL-FAILED 3 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --old 'v < lo' --new 'v > lo' --compile true --test "$crlf_probe"

# ------------------------------------------------------------------------- line-ending shapes outside Go

shapes="$root/shapes"
mkdir "$shapes"

printf 'alpha=1\nbeta=2\ngamma=3' >"$shapes/unterminated.txt"
last_unterminated='[ "$(tail -c 1 unterminated.txt | od -An -tx1 | tr -d " \n")" != 0a ] && [ $(( $(wc -l < unterminated.txt) )) -eq 2 ]'
case_run "an unterminated last line stays unterminated (probe: no final newline, 2 LFs, in both arms)" SURVIVED 1 "$shapes/unterminated.txt" brief -- \
	--dir "$shapes" --file unterminated.txt --old 'gamma=3' --new 'gamma=4' --compile true --test "$last_unterminated"

printf 'alpha=1\r\nbeta=2\ngamma=3\r\n' >"$shapes/mixed.txt"
two_crs='[ $(( $(tr -cd "\r" < mixed.txt | wc -c) )) -eq 2 ]'
case_run "a mixed-ending file: the LF line stays LF, the CRLF lines stay CRLF (probe: exactly 2 CRs in both arms)" SURVIVED 1 "$shapes/mixed.txt" brief -- \
	--dir "$shapes" --file mixed.txt --old 'beta=2' --new 'beta=3' --compile true --test "$two_crs"

# ------------------------------------------------------------------------- a mutation that cannot land

case_run "NOT-APPLIED: the text is not in the file" NOT-APPLIED 4 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --old 'no such text anywhere' --new 'x' --pkg .

case_run "NOT-APPLIED: the text matches several lines, so the harness will not guess" NOT-APPLIED 4 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --old 'return' --new 'x' --pkg .

case_run "NOT-APPLIED: a no-op mutation changes nothing" NOT-APPLIED 4 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --old 'v < lo' --new 'v < lo' --pkg .

case_run "NOT-APPLIED: the marker line does not hold the --old text" NOT-APPLIED 4 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --marker 'return lo' --old 'hi' --new 'x' --pkg .

# a multi-line pattern is refused outright: it is the shape that silently misses a CRLF file
n=$((n + 1))
printf '\nselftest: ===== case %s: a multi-line --old is a usage error, not a pattern that quietly misses\n' "$n"
snap="$root/case$n.snap"
cp -p -- "$lf/clamp.go" "$snap"
"$bash_bin" "$harness" --dir "$lf" --file clamp.go --old "$(printf 'v < lo {\n\t\treturn lo')" --new 'x' --pkg . >"$root/case$n.out" 2>&1
rc=$?
sed -e 's/^/    /' "$root/case$n.out"
expect "exit code" "$rc" 64
expect "no verdict is printed" "$(grep -c '^MUTATION-VERDICT:' "$root/case$n.out")" 0
cmp -s "$snap" "$lf/clamp.go" && expect "clamp.go untouched" yes yes || expect "clamp.go untouched" DIFFERS yes

# ------------------------------------------------------------------------- the control arm

broken="$root/broken"
cp -r "$lf" "$broken"
cat >"$broken/zz_always_fails_test.go" <<'EOF'
package mutfixture

import "testing"

func TestAlwaysFails(t *testing.T) { t.Fatal("this tree is red before any mutation") }
EOF
case_run "CONTROL-FAILED: the tests already fail without the mutation, so nothing is attributed to one" CONTROL-FAILED 3 "$broken/clamp.go" brief -- \
	--dir "$broken" --file clamp.go --old 'v < lo' --new 'v > lo' --pkg .
expect "the mutation was never applied" "$(grep -c '^mutation-check: applied' "$LAST_OUT")" 0

case_run "CONTROL-FAILED: a mistyped --run runs no test, and go test still exits 0 (else every mutant scores SURVIVED)" CONTROL-FAILED 3 "$lf/clamp.go" brief -- \
	--dir "$lf" --file clamp.go --old 'v < lo' --new 'v > lo' --pkg . --run TestNoSuchTest
expect "the mutation was never applied" "$(grep -c '^mutation-check: applied' "$LAST_OUT")" 0

# ------------------------------------------------------------------------- a signal during the mutant's tests

nap() { sleep 0.1 2>/dev/null || sleep 1; }

# signal_ignored SIG: 0 when a bash started from here would have SIG ignored on entry (then it can never trap it)
signal_ignored() {
	local out
	out=$("$bash_bin" -c "trap 'echo handled; exit 0' $1; kill -$1 \$\$; sleep 1; echo ignored" 2>/dev/null)
	[ "$out" != handled ]
}

# signal_case SIG WANT_EXIT: stop the harness with SIG while the mutant's test command sleeps. The file must be
# restored byte for byte, the harness must not wait the sleep out, and the sleeping child must be dead too.
signal_case() {
	local sig=$1 want=$2 dir="$root/sig-$1" hpid i started sleeper t0 rc
	if signal_ignored "$sig"; then
		skip=$((skip + 1))
		printf '\nselftest:   skip  SIG%s: this shell started with it ignored (a background job of a script, nohup, some CI runners), and bash cannot trap a signal that was ignored on entry; run the self-test in the foreground to cover it\n' "$sig"
		return
	fi
	mkdir "$dir"
	printf 'state=ok\n' >"$dir/target.txt"
	cp -p -- "$dir/target.txt" "$dir/target.snap"
	n=$((n + 1))
	printf '\nselftest: ===== case %s: SIG%s while the mutant is under test\n' "$n" "$sig"
	t0=$SECONDS
	"$bash_bin" "$harness" --dir "$dir" --file target.txt --old ok --new hang --compile true \
		--test 'if grep -q hang target.txt; then sleep 120 & echo $! > sleeper.pid; touch started; wait; fi' \
		>"$dir/out.txt" 2>&1 &
	hpid=$!
	started=0
	for i in $(seq 1 300); do
		if [ -f "$dir/started" ]; then
			started=1
			break
		fi
		nap
	done
	expect "the mutant's test command was running (the mutation was on disk: $(grep -c hang "$dir/target.txt") line)" "$started" 1
	kill -"$sig" "$hpid" 2>/dev/null
	wait "$hpid"
	rc=$?
	grep -v -e '^mutation-check: backup' "$dir/out.txt"
	expect "exit code is 128+$sig" "$rc" "$want"
	expect "verdict" "$(grep '^MUTATION-VERDICT:' "$dir/out.txt" | tail -n 1 | cut -d' ' -f2)" INTERRUPTED
	if cmp -s "$dir/target.snap" "$dir/target.txt"; then
		expect "target.txt restored byte for byte after the signal" yes yes
	else
		expect "target.txt restored byte for byte after the signal" DIFFERS yes
	fi
	sleeper=$(cat "$dir/sleeper.pid" 2>/dev/null)
	if [ -n "$sleeper" ] && kill -0 "$sleeper" 2>/dev/null; then
		expect "the sleeping grandchild of the test command was stopped too" alive dead
		kill "$sleeper" 2>/dev/null
	else
		expect "the sleeping grandchild of the test command was stopped too" dead dead
	fi
	expect "the 120 s sleep was cut short ($((SECONDS - t0)) s in all)" "$([ $((SECONDS - t0)) -lt 60 ] && echo yes || echo no)" yes
}

set -m # a background job then keeps the default SIGINT disposition (a plain non-interactive shell starts it ignoring SIGINT)
signal_case TERM 143
signal_case INT 130
signal_case HUP 129
set +m

# ------------------------------------------------------------------------- a restore that cannot succeed

rf="$root/rf"
mkdir "$rf"
printf 'x=1\n' >"$rf/rf.txt"
cp -p -- "$rf/rf.txt" "$rf/rf.snap"
n=$((n + 1))
printf '\nselftest: ===== case %s: RESTORE-FAILED: the test command replaces the file with a directory, so the restore cannot succeed\n' "$n"
"$bash_bin" "$harness" --dir "$rf" --file rf.txt --old 'x=1' --new 'x=2' --compile true \
	--test 'if grep -q x=2 rf.txt; then rm -f rf.txt; mkdir rf.txt; fi' >"$rf/out.txt" 2>&1
rc=$?
grep -v -e '^mutation-check: backup' "$rf/out.txt"
expect "verdict" "$(grep '^MUTATION-VERDICT:' "$rf/out.txt" | tail -n 1 | cut -d' ' -f2)" RESTORE-FAILED
expect "exit code" "$rc" 5
kept=$(sed -n 's/^mutation-check: restore   the pristine copy is kept at: //p' "$rf/out.txt")
if [ -n "$kept" ] && cmp -s "$rf/rf.snap" "$kept"; then
	expect "the pristine copy it names holds the original bytes" yes yes
else
	expect "the pristine copy it names holds the original bytes" no yes
fi
# the kept copy is the harness's own temp dir (named mutation-check.XXXXXX); it is the one thing a failed restore leaves
case $(basename "$(dirname "$kept")") in
mutation-check.*) rm -rf -- "$(dirname "$kept")" ;;
esac

# ---------------------------------------------------------------------------------------------- summary

expect "the tracked fixture was never touched" "$(cat "$fixture"/* | sha256sum | cut -d' ' -f1)" "$fixture_sum"

printf '\nselftest: %s checks, %s failed, %s skipped\n' "$((pass + fail))" "$fail" "$skip"
if [ "$fail" -eq 0 ]; then
	echo "selftest: PASS"
	exit 0
fi
echo "selftest: FAIL"
exit 1
