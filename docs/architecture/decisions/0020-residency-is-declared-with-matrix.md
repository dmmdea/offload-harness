---
status: Accepted
date: "2026-07-28"
---

# Residency is declared with `matrix:`, not `groups:`

## Context

Every serving template declared model residency with llama-swap's `groups:` block. Two
things are wrong with that, and only one of them is cosmetic.

**Upstream moved.** llama-swap replaced `groups:` with `matrix:` in v202; a config must
use one or the other, never both. The installers pin v236, and the oldest binary in this
fleet is v208 — every one of them is past the change.

**The semantics did not hold.** On the workstation node, under `groups:`, an
`exclusive: true` group evicted a `persistent: true` group anyway — verified live on
2026-07-26, not inferred. While the evicting model was loaded, `bge-reranker-v2-m3`
could not start at all: a direct probe of its port timed out and a rerank through
llama-swap returned HTTP 000 after 90 s, so the memory stack **silently fell back to
dense-only ordering** even though its admission thresholds are calibrated against the
reranker's raw logits. Unloading the other model restored it in 3.3 s, which is what
pinned the cause. `persistent` did not do what its name promises.

That node was hand-migrated to `matrix:` the same day. The repo templates were not — so
every tier still rendered the topology that had just been proven unsafe.

Worse, three of them rendered something strictly worse than the workstation's:

| template | what it declared | consequence |
|---|---|---|
| `win-cuda`, `win-vulkan`, `win-cpu` | `offload-family`: `swap: true` + **`exclusive: true`**, with `embeddinggemma` in **no group at all** | an exclusive group unloads everything outside itself, so loading any chat model evicted the embedder |
| `win-cuda-resident` | **no `groups:` block whatsoever** | the ALL-RESIDENT tiers (`blackwell-32/48/72`) never actually declared that they are all-resident; residency was left to llama-swap's default for ungrouped models rather than stated by the file whose entire premise is "no swap group" |
| `linux-cuda` | `heavy` swap:true/exclusive:false + `support` swap:false | the only one exposed to none of the above — it never used `exclusive: true` |

## Decision

**All six templates declare residency with `matrix:`. Sets are the valid CONCURRENT
COMBINATIONS, and residents appear in every set.**

That last clause is the whole point. Under `groups:` "do not evict the memory stack" was
a priority the solver was free to reinterpret, and did. Under `matrix:` it is structural:
if the embedder is a member of every set, there is no valid combination in which it is
absent, so no request can be satisfied by evicting it. `evict_costs` makes it maximally
expensive to stop on top of that, as a second line rather than the first.

Verified against the real binary before writing any of it — none of these are in the
upstream README:

- `matrix` **must** define at least one var (`matrix: matrix must define at least one var`).
- A var key **must be alphanumeric and 1–8 characters** — `embeddinggemma` is rejected
  outright. A seat's own name can therefore never be its var key.
- A set expression may reference **only var ids**, never a model id
  (`set "interactive": unknown var ID "offload-e4b"`).

Because a var key cannot be the model name, `internal/servingtmpl` derives one from the
seat KIND (`vis`, `stt`) — stable and unique by construction, since a tier may declare at
most one seat per kind.

Set membership for the optional 26B and for media seats is a TOKEN, not an expression
edit: `__M26_ALT__` / `__M26_AND__` and `__SEATS_SWAPPABLE__` / `__SEATS_RESIDENT__`. The
operator differs by template — a swappable member is one more ALTERNATIVE (`|`) in a
mutually-exclusive set, a resident member is one more CO-RESIDENT (`&`) — and only the
template knows which it means. Editing an expression from the outside would have to
guess.

## Consequences

- **Nothing changes on any running node.** These templates are rendered at INSTALL time.
  The workstation's live `C:\llama-swap\llama-swap.yaml` and the Linux node's
  `etc/config.yaml` are hand-maintained; the repo contains no reference to either path
  and never writes them. **Rendering a template over a live hand-tuned config is the one
  way this change could disturb the memory stack — do not do it without diffing first.**
- Fresh installs of the three `offload-family` tiers stop evicting the embedder.
- The all-resident tiers now state their premise instead of inheriting a default.
- A **llama-swap >= v202 floor** is now real for rendered configs. `install.ps1` pins
  v236. `install.sh` does not install llama-swap at all — the operator provides it — so
  the floor is documented in each template header rather than enforced in code.
- `TestNoTemplateStillDeclaresLegacyGroups` fails CI if any template regresses to
  `groups:` or ships no `matrix:` at all.
- Every rendered template was accepted by a real binary before merge: all five Windows
  templates on v242, and the Linux template on **v208** — the oldest in the fleet, chosen
  deliberately as the floor test — which served the full roster including both media
  seats.

## Alternatives rejected

- **Leave `groups:` alone since the Linux template is not exposed.** It is not exposed
  *today*, by the accident of never having used `exclusive: true`. The failure is one
  template edit away, and three sibling templates were already in it.
- **Keep `groups:` and add `persistent: true`.** Measured not to work: that is precisely
  the flag that failed to protect the memory stack on 2026-07-26.
- **Edit set expressions programmatically instead of using tokens.** The renderer would
  have to infer whether a member joins with `|` or `&` — a property of the tier's
  intent that lives in the template, not in the seat.
