# OpenSpec

## Purpose

Give the repository one place for spec and plan artifacts of a change: a proposal, the spec deltas, a design and a task
list, written to a fixed shape and checked by a validator. The tool is the OpenSpec CLI, installed as the newest npm release, verified against the registry's integrity value (the row in [security.md](security.md#admitted-third-party-software)). The `openspec/` tree is tracked, so the
[leak gate](leak-gate.md) reads it like every other path.

OpenSpec is a pilot. No other workstream depends on it, waits for it or names it: work outside it keeps the ADR, the pull
request body and the goal list it has today.

## What OpenSpec owns here

| OpenSpec owns | Stays as it is |
|---|---|
| the `openspec/` tree: `config.yaml`, `schemas/`, `specs/`, `changes/` | plan execution (superpowers-style: small steps, no new tests) |
| the artifacts of a change: `proposal.md`, `specs/**/spec.md`, `design.md`, `tasks.md`, and an optional `adr.md` | the ship flow: one gate pass, then a pull request; the changelog fragment; the docs update |
| `openspec validate` over those artifacts | ADRs under `docs/architecture/decisions/`, accepted by a human |

A change in this repository may use OpenSpec for its spec and plan, or not. Either way the code, the tests, the fragment
and the docs travel the same way.

## The tree

| path | what it is |
|---|---|
| `openspec/config.yaml` | `schema: harness-change`, a `context` that carries the standing invariants (quality outranks speed with no wall-time term in a pass rule, no CPU inference, idle models unload at 300 s, no daemon or scheduler, public-repo privacy with node letters only, one changelog fragment per change, ADR for a decision, no new tests and one real run as the check, the leak gate) and `rules` per artifact |
| `openspec/schemas/harness-change/` | the project-local workflow, forked from the built-in `spec-driven` with `openspec schema fork spec-driven harness-change` |
| `openspec/specs/` | the capability specs (empty until the first change is archived) |
| `openspec/changes/` | in-flight changes; `archive/` holds finished ones |

### The schema

`harness-change` keeps `proposal` -> `specs` -> `design` -> `tasks` as `spec-driven` has it and adds `adr`, an ADR draft for a
change that makes a decision. The artifact is optional by the shape of the graph: it requires `design`, and nothing requires
it, neither `tasks` nor `apply`. The OpenSpec CLI has no `optional` key in its artifact schema (an unknown key is dropped
without a warning), so the property is held by `TestOpenSpecAdrArtifactIsOptional` instead of by a flag.

What that looks like in use, checked in a scratch project against the CLI as of 2026-10-28:

- With `proposal`, `specs`, `design` and `tasks` written and no `adr.md`, `openspec instructions apply` reports `state: ready`,
  `openspec validate --all --strict` passes, and the propose flow leaves `adr` alone (it creates only the artifacts
  `applyRequires` reaches).
- `openspec status` keeps listing `adr` as `ready` (`Progress: 4/5 artifacts complete`, `Next: openspec instructions adr`) and
  `isPlanningComplete` stays `false` until an `adr.md` exists. The propose and apply flows ignore that: they follow
  `applyRequires` (here `tasks`), not that flag.
- The archive flow does not ignore it. The generated `/opsx:archive` skill warns about every artifact that is neither done
  nor skipped and asks for confirmation, so a change with no `adr.md` meets that prompt each time; the CLI has no
  `optional` flag to silence it (the key is dropped), so the friction is the price of an optional artifact being a graph shape.
  Confirm and move on; do not create an empty `adr.md` to make the prompt go away. The CLI command
  `openspec archive <name> --yes` archives without asking.
- The draft is always `status: Proposed`. A person accepts it, assigns the number and adds the index row when moving it to
  `docs/architecture/decisions/` (the architecture rules in [STYLE.md](../STYLE.md#architecture-decision-records)).

## Install

The CLI is installed by `scripts/openspec-install.sh <prefix>` (or `OPENSPEC_PREFIX`), where the prefix is a directory of its
own on a data drive. There is no default prefix. The script:

1. resolves the newest release of `@fission-ai/openspec` from the npm registry (`dist-tags.latest`) and fetches its tarball
   with `npm pack --ignore-scripts` into a scratch directory under the prefix (the scoped name always: the unscoped npm
   package `openspec` is another publisher's);
2. refuses, installing nothing, when the tarball's integrity does not match the `dist.integrity` the registry publishes for
   that release (nothing hashed lives in the repository);
3. copies `scripts/openspec/package.json` into the scratch directory, next to the verified tarball, and runs
   `npm install --ignore-scripts` there: the CLI is that file and its dependencies resolve to their newest compatible
   releases, each verified by npm against the registry's integrity value, with no lifecycle script run;
4. checks the staged CLI runs and reports a version, moves the staged `node_modules` into the prefix (replacing an earlier
   install of this CLI, refusing a `node_modules` that is something else's), removes the scratch directory and prints the four
   exports below.

npm's cache, logs and temporary files stay under the prefix, and the script touches neither the user-level npm prefix nor the
npmrc. The prefix ends up holding `node_modules`, `.npm-cache` and, if the CLI writes any, `config`. Versions are never
pinned (ADR 0099): there is no lockfile and no version or digest constant in the script; the registry is the integrity
source, `--ignore-scripts` and the validator gate are the mitigations, and a release that breaks the workflow is fixed in the
schema or the templates, not by freezing the CLI.

To uninstall, delete the prefix directory. To update, run the script again: it takes the newest release. A new admitted
source (a different package or publisher) is a new admission: change the row in `security.md`.

### Telemetry and the config home

The CLI sends usage telemetry unless `OPENSPEC_TELEMETRY=0` or `DO_NOT_TRACK=1` is set, and checks the registry for a newer
version unless `OPENSPEC_NO_UPDATE_CHECK=1`. Its global config lives under `XDG_CONFIG_HOME`, so that points under the
prefix. The install script prints the four:

```
export OPENSPEC_TELEMETRY=0
export DO_NOT_TRACK=1
export OPENSPEC_NO_UPDATE_CHECK=1
export XDG_CONFIG_HOME='<prefix>/config'
```

Set them in any shell that runs `openspec`. `scripts/openspec-install.sh` and `scripts/openspec-validate.sh` set the first
three themselves. Never run `openspec completion install` (it edits shell startup files such as `.bashrc`) and never edit
user-level config.

## The validate gate

`scripts/openspec-validate.sh` sets the three switches and runs `openspec validate --all --strict --no-interactive`, then
`openspec schema validate <name>` for each directory under `openspec/schemas/`, because `validate --all` reads specs and
changes and never the schema (a schema with a dangling `requires` passes it). It takes the binary from `OPENSPEC_BIN`, else
`openspec` on `PATH`, and its exit code is the gate's.

On an empty tree `validate --all` prints `No items found to validate.` and exits 0: green means no finding, not that there is
a spec. The gate has teeth once a change exists; in a scratch project a requirement without a scenario made it exit 1.

`TestOpenSpecTreeValidates` (root package) runs the script with the real CLI where `OPENSPEC_BIN` names it, and otherwise
skips and says so. The structural tests need no CLI and run everywhere.

**Where the gate runs.** `go test ./...` does not run the real validator unless `OPENSPEC_BIN` is set, and CI does not set it.
The gate list is the pre-pull-request list in [CONTRIBUTING.md](../../CONTRIBUTING.md#opening-a-pull-request): a change that
touches `openspec/` runs `scripts/openspec-validate.sh` there, with the CLI installed by `scripts/openspec-install.sh`.

## Generated tool files

`openspec init --tools claude,opencode --profile core` writes, besides `openspec/`, six skills and six slash commands for
each tool: `.claude/skills/openspec-*/`, `.claude/commands/opsx/` and `.opencode/` (commands and skills). They are
gitignored and regenerated by `openspec update`; a tracked copy would put tool prompt text in the public tree and drift from
the CLI version that wrote it. `TestOpenSpecGeneratedToolFilesStayUntracked` holds the ignore lines.

## Invariants and pitfalls

- The `context` and `rules` in `config.yaml` are public text. Machines appear as node letters or role phrases, never by
  name, and a tier id only as "the `<tier>` reference box"; the leak gate fails the build on a hit and has no allow list.
- A rule in `config.yaml` that is a plain YAML scalar containing `: ` parses as a mapping, and OpenSpec then drops that
  artifact's whole rule list with a warning. Quote every rule.
- `rules` keys are artifact ids. An unknown key is dropped with a warning, so `TestOpenSpecConfigRulesNameRealArtifacts` checks
  them.
- `openspec new change <name>` prints `with schema 'spec-driven'` before it creates the change, but the change's
  `.openspec.yaml` records the schema from `config.yaml` (`harness-change`). Check the `Schema:` line it prints after.
- The CLI also keeps a user-level data directory (user schemas, stores, worksets): `XDG_DATA_HOME` when set, else
  `%LOCALAPPDATA%\openspec` on Windows. The commands this workflow uses (`init`, `new change`, `status`, `validate`,
  `schema fork` and `schema validate`) did not create it in the checks against the CLI. Do not use the `store` or
  `workset` commands; if one is ever needed, point `XDG_DATA_HOME` under the prefix first.
- A change with no spec deltas fails validation unless its `.openspec.yaml` sets `skip_specs: true`.

## Testing notes

`go test -run 'TestOpenSpec|TestDocsLint' .` covers the structure, both scripts (against stand-in `openspec` and
`npm` programs, so the switches, the arguments, the exit code, the integrity refusal and the install's success path, with npm's
cache and temp folders under the prefix in the fetch and in the install whatever the caller exported, need neither the network nor the CLI) and the ignore
lines. With `OPENSPEC_BIN` set it also runs the real CLI. With `OPENSPEC_INSTALL_TEST=1` it also runs the install script for
real (npm and the network) into a scratch prefix, with home, application-data, XDG and temp folders pointed at empty folders
beside it: the test fails if any of them gains a file or if the binary does not report a version.

## Source map

| path | role |
|---|---|
| `openspec/config.yaml` | the schema choice, the invariants as `context`, the per-artifact `rules` |
| `openspec/schemas/harness-change/schema.yaml` | the forked workflow and the optional `adr` artifact |
| `openspec/schemas/harness-change/templates/` | the artifact templates, including `adr.md` |
| `scripts/openspec-install.sh` | the registry-verified install of the newest release into a caller-chosen prefix |
| `scripts/openspec/package.json` | the install project: the admitted package, newest release, no lockfile |
| `scripts/openspec-validate.sh` | the gate: strict validation plus schema validation, telemetry off |
| `openspec_tree_test.go` | the structural tests, the script tests and `TestOpenSpecTreeValidates` |
| `openspec_ignore_test.go` | the generated tool files stay untracked |
| `internal/leakgate/openspec_scope_test.go` | the leak gate reads the `openspec/` tree |
| `.gitignore` | the generated per-tool paths |

## Related docs

- [security.md](security.md#admitted-third-party-software): the admitted row (source, licence, telemetry, data location) and the
  admission checks behind it.
- [leak-gate.md](leak-gate.md): the privacy gate that scans the tree.
- [STYLE.md](../STYLE.md): documentation rules, the ADR rules and the privacy vocabulary.
