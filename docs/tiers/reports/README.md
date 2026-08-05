# Checked-in capability reports

Real output from real machines, produced by `local-offload report` and committed verbatim. They are
**examples to compare against**, never a claim about your hardware — hardware, drivers and bindings
differ per box.

| file | machine | tier | notes |
|---|---|---|---|
| [blackwell-16-qube.md](blackwell-16-qube.md) | Qube (RTX 5060 Ti 16 GB, Win 11) — **HISTORICAL, 2026-07-27** | `blackwell-16` | full ComfyUI stack; 11/11 media routes CONFIGURED. ⚠️ Qube gained a second card (RTX 5070 Ti 16 GB) on **2026-08-02** and now runs **`blackwell-2x16`, ~32 GB total VRAM**. This report predates that and is kept as a point-in-time record — do NOT read it as Qube's current hardware. |
| [ampere-8-aorus15p-xd.md](ampere-8-aorus15p-xd.md) | Aorus 15P-XD (RTX 3070 8 GB, Win 11) | `ampere-8` | after binding `voicegen_script`; render tree co-located with the binary |
| [ampere-6-lenovo-m720q.md](ampere-6-lenovo-m720q.md) | Lenovo M720q (RTX 3050 6 GB, Ubuntu) | `ampere-6` | Linux node; `sdcpp` image engine, ComfyUI bound |

All three show `hardware tier: UNKNOWN`. None of them was installed by `install.ps1` into the default
`$OFFLOAD_HOME`, which is the only place the manifest is looked for today — a box installed the
normal way names its tier. That gap belongs to the installer workstream, and the reports say so
rather than hiding it.

To add yours: run `local-offload report --out <tier>-<hostname>.md` and drop it here. The filename
prefix is how [the generator](../../../cmd/gentiers) attaches it to the right tier page.
