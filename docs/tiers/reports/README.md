# Checked-in capability reports

Real output from real machines, produced by `local-offload report` and committed verbatim except
for identity scrubbing: hostnames, usernames, and machine-specific paths are replaced with
placeholders (`<node-a>`, `C:\Users\<user>`, `<repo-root>`) per the repo privacy policy
([STYLE.md](../../STYLE.md)). They are **examples to compare against**, never a claim about your
hardware — hardware, drivers and bindings differ per box.

| file | machine | tier | notes |
|---|---|---|---|
| [blackwell-16-node-b.md](blackwell-16-node-b.md) | `<node-b>` workstation (RTX 5060 Ti 16 GB, Win 11) — **HISTORICAL, 2026-07-27** | `blackwell-16` | full ComfyUI stack; 11/11 media routes CONFIGURED. ⚠️ this box gained a second card (RTX 5070 Ti 16 GB) on **2026-08-02** and now runs **`blackwell-2x16`, ~32 GB total VRAM**. This report predates that and is kept as a point-in-time record — do NOT read it as the box's current hardware. |
| [ampere-8-node-a.md](ampere-8-node-a.md) | `<node-a>` laptop (RTX 3070 8 GB, Win 11) | `ampere-8` | after binding `voicegen_script`; render tree co-located with the binary |
| [ampere-6-node-c.md](ampere-6-node-c.md) | `<node-c>` mini-PC (RTX 3050 6 GB, Ubuntu) | `ampere-6` | Linux node; `sdcpp` image engine, ComfyUI bound |

All three show `hardware tier: UNKNOWN`. None of them was installed by `install.ps1` into the default
`$OFFLOAD_HOME`, which is the only place the manifest is looked for today — a box installed the
normal way names its tier. That gap belongs to the installer workstream, and the reports say so
rather than hiding it.

To add yours: run `local-offload report --out <tier>-<node-label>.md` and drop it here — scrub your
hostname/username first (use a generic node label, not the machine name). The filename prefix is how
[the generator](../../../cmd/gentiers) attaches it to the right tier page.
