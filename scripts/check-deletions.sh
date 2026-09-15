#!/usr/bin/env bash
# check-deletions.sh -- a deletion that nobody declared does not reach main (H-51).
#
# The problem it closes. Pull request #324 ("the composite tier") was merged with a
# tree that had never seen the two releases before it: the branch was rebuilt by
# replaying its trees onto an older base, so the merge commit's FIRST PARENT was
# 0.123.1 while its TREE was 0.122.2 plus the branch. Six files that two merged,
# tested, reviewed pull requests had added vanished in one commit -- an ADR, a
# package, three test files and a fixture -- and every gate stayed green, because
# every gate reads the tree it is handed and none of them reads what the tree LOST.
# `go build`, `go vet` and `go test` all pass on a tree that is simply missing a
# feature. Deleting a file is a decision; this makes it a DECLARED one.
#
# The check: diff BASE against HEAD, take the deletions, and require each deleted
# path to be named somewhere a human wrote -- so an intentional removal costs one
# line and an accidental one costs a red build.
#
# Two calls, because the two failure shapes are different:
#
#   * On a pull request, BASE is the base branch tip. This catches "this branch
#     removes a file that is on main".
#   * On a push to main, BASE is HEAD's FIRST PARENT. This is the one that catches
#     #324: a merge commit whose tree drops what its first parent carried. Nothing
#     else looks at that relationship, which is exactly why it went unseen.
#
# Declaring a deletion -- any ONE of these, checked in this order:
#
#   1. A `DELETIONS` file at the repo root listing one path per line (`#` comments
#      and blank lines ignored). Use this for a large, deliberate removal.
#   2. A `Deletes: <path>[, <path>...]` line in the pull request body ($PR_BODY).
#   3. A `Deletes: <path>[, <path>...]` line in any commit message in BASE..HEAD.
#
# `Deletes:` is matched case-sensitively and must start the line (an optional list
# bullet is allowed), so prose that happens to say "deletes" is never a declaration.
#
# Usage: scripts/check-deletions.sh <base-ref> [<head-ref>]   (head defaults to HEAD)
# Exit:  0 nothing deleted, or every deletion declared; 1 an undeclared deletion; 2 bad usage.

set -euo pipefail

base="${1:-}"
head_ref="${2:-HEAD}"
if [ -z "$base" ]; then
	echo "usage: scripts/check-deletions.sh <base-ref> [<head-ref>]" >&2
	exit 2
fi

if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
	echo "check-deletions: base ref '$base' is not a commit this checkout can see." >&2
	echo "  In CI that means the base branch was not fetched (checkout needs fetch-depth: 0)." >&2
	exit 2
fi

deleted="$(git diff --name-status --diff-filter=D "$base" "$head_ref" | cut -f2-)"
if [ -z "$deleted" ]; then
	echo "check-deletions: $base..$head_ref deletes no file — OK"
	exit 0
fi

# Everything a human declared, one path per line. `Deletes:` lines may be
# comma-separated; whitespace splits the rest.
declarations() {
	if [ -f DELETIONS ]; then
		sed -e 's/#.*$//' DELETIONS
	fi
	# The PR body and the commit messages, restricted to their Deletes: lines.
	{
		printf '%s\n' "${PR_BODY:-}"
		git log --format=%B "$base..$head_ref"
	} | sed -n -e 's/^[[:space:]]*[-*]\{0,1\}[[:space:]]*Deletes:[[:space:]]*//p' | tr ',' ' '
}

declared="$(declarations | tr -s '[:space:]' '\n' | sed -e 's#^\./##' -e 's#^/##' | grep -v '^$' || true)"

undeclared=""
declared_count=0
for path in $deleted; do
	if printf '%s\n' "$declared" | grep -qxF -- "$path"; then
		declared_count=$((declared_count + 1))
		continue
	fi
	undeclared="$undeclared$path
"
done

if [ -z "$undeclared" ]; then
	echo "check-deletions: $declared_count deletion(s) against $base, all declared — OK"
	exit 0
fi

{
	echo "check-deletions: FILES DELETED BUT NOT DECLARED (H-51)"
	echo
	echo "  Comparing $base -> $head_ref, these tracked files are gone and nothing says why:"
	printf '%s' "$undeclared" | sed -e 's/^/    /'
	echo
	echo "  If the removal is intended, declare it — one of:"
	echo "    * add a 'Deletes: <path>, <path>' line to the pull request body"
	echo "    * add a 'Deletes: <path>' line to a commit message on this branch"
	echo "    * list the paths in a DELETIONS file at the repo root"
	echo
	echo "  If it is NOT intended, your branch is missing work that is already on $base."
	echo "  A branch rebuilt by replaying trees onto an older base does exactly this, and"
	echo "  it is how pull request #324 dropped two merged releases (see CONTRIBUTING.md)."
	echo "  Merge $base into your branch — do not force the merge — and re-run."
} >&2
exit 1
