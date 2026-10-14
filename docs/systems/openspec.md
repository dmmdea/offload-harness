# OpenSpec

## Purpose

Give the repository one place for spec and plan artifacts of a change: a proposal, the spec deltas, a design and a task
list, written to a fixed shape and checked by a validator. The tool is the OpenSpec CLI, installed from a hash-pinned
tarball (the row in [security.md](security.md#admitted-third-party-software)). The `openspec/` tree is tracked, so the
[leak gate](leak-gate.md) reads it like every other path.

OpenSpec is a pilot. No other workstream depends on it, waits for it or names it: work outside it keeps the ADR, the pull
request body and the goal list it has today.

## What OpenSpec owns here

| OpenSpec owns | Stays as it is |
|---|---|
| the `openspec/` tree: `config.yaml`, `schemas/`, `specs/`, `changes/` | plan execution (superpowers-style: red-first tests, small steps) |
| the artifacts of a change: `proposal.md`, `specs/**/spec.md`, `design.md`, `tasks.md`, and an optional `adr.md` | the ship flow: one gate pass, then a pull request; the changelog fragment; the docs update |
| `openspec validate` over those artifacts | ADRs under `docs/architecture/decisions/`, accepted by a human |

A change in this repository may use OpenSpec for its spec and plan, or not. Either way the code, the tests, the fragment
and the docs travel the same way.

## The tree

| path | what it is |
|---|---|
| `openspec/config.yaml` | `schema: harness-change`, a `context` that carries the standing invariants (quality outranks speed with no wall-time term in a pass rule, no CPU inference, idle models unload at 300 s, no daemon or scheduler, public-repo privacy with node letters only, one changelog fragment per change, ADR for a decision, red-first tests, the leak gate) and `rules` per artifact |
| `openspec/schemas/harness-change/` | the project-local workflow, forked from the built-in `spec-driven` with `openspec schema fork spec-driven harness-change` |
| `openspec/specs/` | the capability specs (empty until the first change is archived) |
| `openspec/changes/` | in-flight changes; `archive/` holds finished ones |

### The schema

`harness-change` keeps `proposal` -> `specs` -> `design` -> `tasks` as `spec-driven` has it and adds `adr`, an ADR draft for a
change that makes a decision. The artifact is optional by the shape of the graph: it requires `design`, and nothing requires
it, neither `tasks` nor `apply`. OpenSpec 1.14.1 has no `optional` key in its artifact schema (an unknown key is dropped
without a warning), so the property is held by `TestOpenSpecAdrArtifactIsOptional` instead of by a flag.

What that looks like in use, checked in a scratch project against the pinned CLI:

- With `proposal`, `specs`, `design` and `tasks` written and no `adr.md`, `openspec instructions apply` reports `state: ready`,
  `openspec validate --all --strict` passes, and the propose flow leaves `adr` alone (it creates only the artifacts
  `applyRequires` reaches).
- `openspec status` keeps listing `adr` as `ready` and `isPlanningComplete` stays `false` until an `adr.md` exists. That is
  display only: the propose and apply flows follow `applyRequires` (here `tasks`), not that flag.
- The draft is always `status: Proposed`. A person accepts it, assigns the number and adds the index row when moving it to
  `docs/architecture/decisions/` (the architecture rules in [STYLE.md](../STYLE.md#architecture-decision-records)).

## Install

The CLI is installed by `scripts/openspec-install.sh <prefix>` (or `OPENSPEC_PREFIX`), where the prefix is a directory on a
data drive. There is no default prefix. The script:

1. fetches the tarball with `npm pack @fission-ai/openspec@1.14.1 --ignore-scripts` into a scratch directory under the
   prefix (the scoped name always: the unscoped npm package `openspec` is another publisher's);
2. refuses, installing nothing, when the tarball's sha256 is not the pinned one (the constants in the script must equal the
   admitted row, which `TestOpenSpecInstallScriptPinsWhatSecurityMdAdmitted` checks);
3. installs from that file with `npm install --prefix <prefix> <tarball> --ignore-scripts --no-save`;
4. checks the installed CLI reports the pinned version, removes the scratch directory and prints the four exports below.

npm's cache, logs and temporary files stay under the prefix, and the script touches neither the user-level npm prefix nor the
npmrc. `--no-save` is deliberate: the tarball lives in the scratch directory, and a `package.json` entry pointing into it would
break every later npm command in the prefix. The sha256 pins the openspec tarball itself; its dependencies are resolved by
npm from the registry at install time under npm's own integrity checks and are not pinned by this script.

To uninstall, delete the prefix directory. A new version is a new admission: change the table row, the two constants and the
test fixture together.

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

## Generated tool files

`openspec init --tools claude,opencode --profile core` writes, besides `openspec/`, six skills and six slash commands for
each tool: `.claude/skills/openspec-*/`, `.claude/commands/opsx/` and `.opencode/` (commands and skills). They are
gitignored and regenerated by `openspec update`; a tracked copy would put tool prompt text in the public tree and drift from
the CLI version that wrote it. `TestOpenSpecGeneratedToolFilesStayUntracked` pins the ignore lines.

## Invariants and pitfalls

- The `context` and `rules` in `config.yaml` are public text. Machines appear as tier ids or node letters, never by name; the
  leak gate fails the build on a hit and has no allow list.
- A rule in `config.yaml` that is a plain YAML scalar containing `: ` parses as a mapping, and OpenSpec then drops that
  artifact's whole rule list with a warning. Quote every rule.
- `rules` keys are artifact ids. An unknown key is dropped with a warning, so `TestOpenSpecConfigRulesNameRealArtifacts` checks
  them.
- `openspec new change <name>` prints `with schema 'spec-driven'` before it creates the change, but the change's
  `.openspec.yaml` records the schema from `config.yaml` (`harness-change`). Check the `Schema:` line it prints after.
- The CLI also keeps a user-level data directory (user schemas, stores, worksets): `XDG_DATA_HOME` when set, else
  `%LOCALAPPDATA%\openspec` on Windows. The commands this workflow uses (`init`, `new change`, `status`, `validate`,
  `schema fork` and `schema validate`) did not create it in the checks against the pinned CLI. Do not use the `store` or
  `workset` commands; if one is ever needed, point `XDG_DATA_HOME` under the prefix first.
- A change with no spec deltas fails validation unless its `.openspec.yaml` sets `skip_specs: true`.

## Testing notes

`go test -run 'TestOpenSpec|TestDocsLint' .` covers the structure, both scripts (against stand-in `openspec` and `npm`
programs, so the switches, the arguments, the exit code and the hash refusal need neither the network nor the CLI) and the
ignore lines. With `OPENSPEC_BIN` set it also runs the real CLI.

## Source map

| path | role |
|---|---|
| `openspec/config.yaml` | the schema choice, the invariants as `context`, the per-artifact `rules` |
| `openspec/schemas/harness-change/schema.yaml` | the forked workflow and the optional `adr` artifact |
| `openspec/schemas/harness-change/templates/` | the artifact templates, including `adr.md` |
| `scripts/openspec-install.sh` | the pinned, hash-verified install into a caller-chosen prefix |
| `scripts/openspec-validate.sh` | the gate: strict validation plus schema validation, telemetry off |
| `openspec_tree_test.go` | the structural tests, the script tests and `TestOpenSpecTreeValidates` |
| `openspec_ignore_test.go` | the generated tool files stay untracked |
| `internal/leakgate/openspec_scope_test.go` | the leak gate reads the `openspec/` tree |
| `.gitignore` | the generated per-tool paths |

## Related docs

- [security.md](security.md#admitted-third-party-software): the admitted row (version, sha256, telemetry, data location) and the
  admission checks behind it.
- [leak-gate.md](leak-gate.md): the privacy gate that scans the tree.
- [STYLE.md](../STYLE.md): documentation rules, the ADR rules and the privacy vocabulary.
