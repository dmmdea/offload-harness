<!-- Read CONTRIBUTING.md once before opening this. The rules that trip people up are summarised here. -->

## What changed and why

<!-- One focused change per pull request. Say what a user or operator sees differently and what motivated it. -->

## The one real run

<!-- A change carries no new tests. The check is one real run of the changed thing (the real command, the built binary,
     the rendered file), read back. Paste the command and its trimmed output. -->

```
```

## Checklist

- [ ] `go build ./...` and `go vet ./...` are clean.
- [ ] Published behaviour changed, so a fragment was added under `changes/unreleased/` (and `CHANGELOG.md` itself was not edited), or nothing published changed.
- [ ] Docs that describe the changed behaviour were updated in this pull request.
- [ ] No new tests were added (CONTRIBUTING.md, "Testing rule").

<!-- Removing a tracked file: CI refuses an undeclared deletion. Declare it with a line in this body or in a commit
     message that starts with the word Deletes, a colon, and the paths, as CONTRIBUTING.md shows. This comment is
     deliberately not such a line. -->
