---
status: Accepted
date: "2026-10-02"
---

# ADR 0071 — A fleet-token holder may send a whole composition project to render; the template door stays as it was

## Context

[ADR 0059](0059-external-cli-media-tool-runs-cpu-class-pinned-env-scrubbed.md) decision 6 made the fleet door for the
composition lane template-only: `compose-video` over `/fleet/dispatch` names a vetted template and typed variables, and
`html` and `project_dir` are accepted only from the local MCP and CLI doors. Two facts drove that: a composition is
trusted code (HyperFrames' Chrome runs it with `--no-sandbox` and without site isolation), and media dispatch carries no
token ([ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md) gates the agent lane and leaves media dispatch tokenless so
deployed media clients keep working).

Some machines have no composition lane at all: a 2-core laptop has no room for a Chrome worker beside its desktop, and an
arm64 single-board computer has no chrome-headless-shell build. They can author compositions and run the ffmpeg-only parts
of an editing workflow, but every render, including a full HyperFrames project from the operator's teaching kit, has to
happen on a node that has the lane. Template renders can already be sent; a project cannot.

What a project can do on a render node was read in HyperFrames 0.8.114 before deciding:

- At render time the page is served by HyperFrames' own file server, which answers only paths inside the project
  directory or its compiled copy (`isPathInside`) on 127.0.0.1. Chrome keeps the same-origin policy (no
  `--disable-web-security`, no file-access flags), so page JavaScript cannot read `file://`.
- At compile time HyperFrames copies an asset the markup references outside the project (by a relative or an absolute path)
  into its output, downloads remote `<script src>`, media and Google Fonts CSS. The copy is a way to read a file off the
  render node and receive it in the video.
- Page JavaScript can make network requests at render time, to the internet or to any host the render node can reach.

## Decision

1. **A new door, `POST /fleet/compose-project` (task type `compose-project`).** It accepts a gzip-compressed tar of a
   project (base64 in a JSON body with its sha256) plus the composition, format, quality, fps, resolution, workers,
   `strict` and snapshot options, and renders it through the same pipeline and runner as every composition.
2. **Closed unless the node opts in, and never open without a token.** The door is advertised and admitted only when
   `fleet_compose_projects` is true, `fleet_auth_token` is set and the compose route is bound
   (`config.ComposeProjectsAdmissible`); a tokenless node refuses it even on loopback. The bearer is checked before any of
   the body is read. The bundle is capped by `fleet_compose_bundle_max_mb` (default 64 MiB, compressed).
3. **The bundle is extracted into a fresh directory and confined to it** (`internal/composebundle`): regular files and
   directories only; no absolute path, drive letter, UNC path, `..`, backslash, NUL, colon, Windows device name or trailing
   dot in a member name; caps of 4,096 files, 256 MiB per file and 512 MiB in total counted on the bytes written. Every
   reference the project's `.html`/`.htm`/`.svg`/`.css` files make (`src`, `href`, `poster`, `data`, `background`,
   `srcset`, `xlink:href`, `data-composition-src`/`-file`, CSS `url()` and `@import`, after decoding HTML character
   references and CSS escapes) must resolve inside the project or be `data:`/`#`; a root-relative or absolute path, a
   `file:` or other non-web URL, and an http(s) URL to a loopback, private, link-local, shared-range (100.64/10, which the tailnet uses), `.ts.net`,
   `.local`, `.internal` or dotless host are refused, as are a `<base>` element and an iframe `srcdoc`. The directory is
   removed when the job ends and on every refusal. The client runs the same check before sending, to fail early.
4. **The template door does not change.** `compose-video` over `/fleet/dispatch` stays tokenless and template-only.

## Consequences

- **Accepted by the operator, 2026-10-02:** a holder of the fleet token can run its own JavaScript in headless Chrome on a
  render node. Page JavaScript can still make network requests at render time, including to hosts on the tailnet, and draw
  what it receives into the video; the static check covers references in markup, not requests built in script. The fleet
  token is held only by the operator's machines. A follow-up may make bundle renders hermetic on Linux nodes (the runner
  setting a dead proxy for Chrome), adopted only once a test proves Chrome honours it.
- A thin client (no compose lane) can render templates and whole projects through the harness; `offload_compose_video`
  places the work with `route` (local, auto, remote), auto choosing a remote node when this box has no lane.
- The door is per node and off by default, so enabling it is a deliberate configuration change on each render node.

## Alternatives considered

- **SSH the project to a render node and run the CLI there.** No new harness surface, but it bypasses the harness's
  placement, ledger and gates, needs SSH keys from every thin client to every render node, and the operator asked for the
  delegation to go through the harness.
- **Keep the door template-only.** Thin clients then render only the seven vetted templates; kit projects would have to
  be rendered from a session on a render node. Rejected by the operator in favour of this door.

## Amendment 1 (2026-10-03): the check follows the compiler, not the file extension

A review against the real compiler (HyperFrames 0.8.114) found three ways past decision 3's check, and the compiler copied an
outside file for each: an attribute after `/` (`<img/src=...>`, which HTML allows); a `data-composition-src` file with
another extension, which was never scanned; and a reference in a nested file that stays inside from its own directory but
climbs out from the project root, which is where the compiler resolves any reference that does not start with `../` (it
rewrites only those relative to their composition). Reading the compiler showed two more: it reads a
`data-composition-src` file by any path, unconfined, and it fetches a `<script src>` on any host, so a numeric loopback a
browser accepts (`127.1`, `0x7f.0.0.1`) reached a node's own services. The check now:

- reads references in raw text wherever they are, an attribute name following whitespace, a quote, `<` or `/`, so it does
  not depend on parsing HTML the way the compiler's parser does (a script's `name = "/x"` can be refused; the message
  names the line);
- reads every file a composition attribute names as a composition, whatever its extension, and follows the names it
  makes;
- refuses a reference that climbs out of the project from its own file or, unless it starts with `../`, from the
  project root; and UNC and protocol-relative references;
- reads a numeric host the way a browser does (decimal, octal and hex parts, one to four of them) before judging it, and
  refuses one no browser accepts and any non-ASCII host name;
- counts every bundle entry, directories included, against a cap of its own, and bounds the decompressed stream as a whole.

The door also masks its jobs from tokenless polls and feeds, as every token-gated lane now does (`gatedJob`, for dispatch,
the project door and queue claims alike); is exempt from `fleet_max_concurrent_jobs` like `compose-video`; gives a token
holder a 15-minute read and write window, since the server's blanket 30 seconds cut a large bundle on an ordinary link;
answers a failure that is the node's own (a disk that filled while unpacking) with 500, not "refused"; and fleet-serve
sweeps orphaned project trees at startup.

Accepted with decision 3's residual: the check is static and reads markup as text, so a difference between how it and
the compiler's parser read a document could still let a reference through. The fleet token is the boundary; the check
narrows what a mistake, or a leaked token, could read.
