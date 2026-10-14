# Changelog fragments

A change to this repository adds **one new file** under `changes/unreleased/` and never edits `CHANGELOG.md`.
Two branches add two files, so they cannot conflict on the changelog, and the release step moves the files under
the new version heading with one command that cannot overwrite a heading (it did, twice, when entries were typed
into `CHANGELOG.md` by hand).

What this does not remove: two *releases* still conflict on `VERSION`, `internal/buildinfo/buildinfo.go`,
`.printing-press.json` and the top of `CHANGELOG.md`. Only the feature-versus-feature conflict and the heading
overwrite are gone.

## Adding a fragment

`changes/unreleased/<slug>.md`, where the slug is the lower-case branch name with `/` written as `-`
(`feat/0187-recipe-fingerprint` becomes `feat-0187-recipe-fingerprint.md`). A branch with two separate entries
adds `<slug>-2.md`, or puts two `###` sections in one file.

The body is **exactly the text that will appear in `CHANGELOG.md`**, written the way the entries there are written:
what changed, why, how it was proved (the red-first note, the mutations that were run), where the details are.

```markdown
### Added - one line that says what changed

- **What it does.** ...
- **Why.** ...
- **Tests.** ...
```

The rules, all checked by `go run ./cmd/changelog check` and by `go test` (the root package, through the same code):

- The file name is lower case and matches `[a-z0-9][a-z0-9._-]*.md`. `README.md` is the one file in the directory
  that is not a fragment; every other entry that is not a well-formed fragment is an error, because a skipped
  fragment is a lost entry.
- The first non-blank line starts with `### ` and names the kind and the change: `### Added - ...`,
  `### Changed - ...`, `### Fixed - ...`, `### Removed - ...` or `### Security - ...`. (Opening with `###` is a rule
  the old changelog did not have: a loose bullet would be glued under the previous fragment's section when the
  fragments are joined. The kind words are a convention, see below.)
- Only `###` and deeper headings. **No line may start with `# ` or `## `**, even inside a code fence: a `## [x.y.z]`
  line in a fragment is the forged release heading the assembler exists to prevent.
- No line may carry a release date between two separators (the shape ` - YYYY-MM-DD - `, or the same with em
  dashes): only a release heading does, and `check` refuses such a line anywhere else in `CHANGELOG.md`. A fragment
  that held one would be assembled, deleted, and then make the changelog fail its own check. Copying the release
  heading's style into a `### Fixed` title, or quoting a release heading in a bullet, is the usual cause: write the
  date with a word before it (`on 2026-10-14`) instead.
- Valid UTF-8, no control character except tab and line feed, and a carriage return only as half of a CR LF pair.
  A lone carriage return ends a line in Markdown, so a `## [x.y.z]` line could hide behind one from every rule
  above.
- Nothing else in `changes/` but this `README.md` and the `unreleased/` directory, spelled exactly so. A fragment
  saved one level up (`changes/<slug>.md`), or in a directory spelled `Unreleased/` (a case-insensitive checkout
  reads it, Linux does not), is read by nothing and would never reach `CHANGELOG.md`, so `check` names it.

Conventions that no code checks: the kind words above, no version number in a heading (the release gives the
version, the fragment does not know it), and keeping machine names, personal names, local paths and LAN addresses out,
as in every tracked file (use `node-a`, `192.0.2.x`, `GPU-00000000-...`); the repository's leak gate reads fragments
like any other tracked file.

`CHANGELOG.md` keeps an **empty `## [Unreleased]` block**. `go test` fails if anything is written there, and says
where the entry goes instead: `changes/unreleased/`. Only the root `CHANGELOG.md` is read or edited; the
`tools/*/CHANGELOG.md` files are vendored ledgers of printed CLIs and are never touched.

The prose gates that read `CHANGELOG.md` for retired wording (and for a required sentence) read it with the
fragments spliced in, so a fragment is checked the day it is written.

## Releasing

The version is the `VERSION` file's, so the order is **bump first, assemble last**.

1. Merge the feature branches into the release branch (or, for a direct pull request, finish your change), then
   merge `origin/main`.
2. Set the version in the three carriers: `VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json`.
3. Assemble:

   ```sh
   go run ./cmd/changelog assemble -title "<one line for the heading>"
   ```

   `-title` is required (the existing headings are a one-line list of the section titles). `-date YYYY-MM-DD`
   defaults to today. `-dry-run` prints the section and writes and deletes nothing. The command reads and checks
   everything first, writes `CHANGELOG.md` through a temporary file and a rename, deletes each consumed fragment
   (retrying, because Windows can hold a file for a moment), and prints one line:

   ```
   Deletes: changes/unreleased/a.md, changes/unreleased/b.md
   ```

   The new section is `## [VERSION] - DATE - TITLE`, under an `## [Unreleased]` heading that is left empty.
   Fragments come in file-name order (byte order, so `a-10.md` sorts before `a-2.md`; put a numeric prefix on a
   name to force an order), each separated by exactly one blank line. Whatever an old-style branch left under
   `## [Unreleased]` is moved to the top of the new section, not dropped. The result is checked against the old
   file before it is written: everything from the old top release heading to the end is byte for byte what it was,
   and the line endings are the file's own.
4. Commit. Put the printed `Deletes:` line in the commit message **only if** the fragments reached `main`
   earlier, through a fragment-only pull request that did not bump the version: then this commit deletes files
   `main` has, and `scripts/check-deletions.sh` wants the deletion declared. A fragment that is added and consumed
   inside the same pull request, or on the release branch, is no deletion against `main` and needs no line.
5. `go test -count=1 ./...`, unpiped.

Assemble is the last step, because it writes the top of `CHANGELOG.md`: a `git merge origin/main` after it
conflicts in exactly the hunk the tool wrote, and the same goes for renumbering the version after it.

### Recovery

If `main` moves after the assembly (another release shipped first, so the version has to be renumbered):

```sh
git revert --no-edit <the release commit>     # the fragments, the old changelog and the old version come back
git merge origin/main
# renumber VERSION, internal/buildinfo/buildinfo.go and .printing-press.json, then assemble again
```

For a release commit that was never pushed, `git reset --soft HEAD~1` followed by
`git restore --source=HEAD --staged --worktree -- CHANGELOG.md changes VERSION internal/buildinfo/buildinfo.go .printing-press.json`
does the same without a revert commit.

If the assembly wrote `CHANGELOG.md` but could not delete a fragment, it exits 1 and names the file. Delete it
by hand. Until you do, `check` and `go test` refuse it as already in `CHANGELOG.md` (a fragment whose text is
already in the changelog is a duplicate entry waiting for the next release).

### Branches written the old way

A branch that already typed its entry under `## [Unreleased]` goes red on the empty-block rule once it merges
this layout. The fix is mechanical: cut the text out of `CHANGELOG.md`, make it a fragment, and leave the block
empty. (If the release happens first, the assembler absorbs the text from `## [Unreleased]`, so nothing is lost
either way.) A stale branch that carries its own release heading still conflicts by hand: this layout cannot
help a heading that two branches both wrote.
