### Fixed - two ADR files get short names so a clone checks out on Windows without core.longpaths

- **What changed.** ADR 0076 and ADR 0082 were renamed from 143 and 145 characters to `0076-a-calls-width-is-sized-from-its-deal.md` and `0082-a-media-call-overflows-to-the-fleet.md`; the titles inside are unchanged, every link to them (ADRs, glossary, operator guide, fleet-node page, the decisions index and one archived changelog link) follows, and the README's Windows note now says the longest tracked path is about 130 characters.
- **Why.** The two long names pushed a clone under an ordinary path past the 260-character Windows limit, so the checkout failed with `Filename too long` unless `core.longpaths` was set.
- **Check.** `git ls-files | awk '{ print length($0) }' | sort -rn | head -1` prints 130; a grep for the old names returns nothing; the ADR, docs and deletion checks pass.
