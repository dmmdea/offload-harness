---
status: Accepted
date: "2026-09-30"
---

# 0067 — The house security standard

## Context

The operator asked for a standard security framework for the harness. It was to be evaluated against
three NVIDIA offerings for what could be adopted, ported or integrated:
- DOCA (the BlueField/ConnectX SDK and its security services);
- the Open Agent Safety Platform (continuous in-silicon agent monitoring);
- OpenShell (an open-source runtime that wraps third-party agents).

The evaluation found three things.

1. **Almost none of the NVIDIA software fits this fleet.**
   - DOCA needs a BlueField or ConnectX device, and the fleet has none.
   - The platform's in-silicon monitor exists only as announcements.
   - OpenShell governs agents the operator does not control. The harness's agents already run through
     its own policy broker (ADR 0003, ADR 0036).
   - What transfers are design rules and about ten small patterns.
2. **Its own code had two defects** that a framework review catches and a feature review did not:
   - `offload_nim` could send the NVIDIA API key to any host whose URL merely contained NVIDIA's name
     (fixed in 0.143.1).
   - `--listen-trusted-network` permitted an all-interfaces bind, so a node that booted before its
     tailnet address existed served its unauthenticated endpoints everywhere (fixed in 0.144.1).
3. **Several controls exist but cannot prove themselves:**
   - the broker audit is written only on the CLI doors;
   - no test fails when a new bare HTTP client appears, although ADR 0042 forbids one on a
     caller-named host;
   - paused guards read as absent rather than paused.

A framework that adds failures gets switched off, so the standard also has to say how a control
earns the right to block work.

## Decision

The harness adopts the **house security standard** in [systems/security.md](../../systems/security.md):

1. **Seven invariants:**
   - decide below and propose above;
   - tighten-only;
   - fail loud;
   - audit, then warn, then enforce;
   - every control has a gate that can fail;
   - additive, flag-gated and reversible;
   - the public/private split.
2. **Ten layers.** L0 is the posture of the controls themselves, then L1 identity and auth through
   L9 human approval and kill. A reliability track R runs across all of them. Each layer states what
   exists, what is added, its default and its gate.
3. **AARM v1.0 R1–R9 is the checklist.** The standard records its coverage per requirement and
   states the deliberate gaps: R3 stays structural, R8 is deferred.
4. **A control is promoted only on counted data.** A new rule starts in audit mode and logs would-be
   denials. It is promoted to warn and then enforce by a pull request carrying its counts and false
   positives. No gate interrupts work before that.
5. **Every control ships with a gate that fails when the control is removed.** Twelve gates are
   named (G1–G12). G1 (the credential binding) and G2 (the bare-client lint,
   `bare_http_client_lint_test.go`, added with this ADR) ship now.
6. **Approval boundary.** No new daemon, listener, scheduler, route or off-node endpoint is built
   without the operator's yes. Such items are listed as approval-gated, not planned work.

## Consequences

- There is one document to hold a change against. A security-relevant change names its layer, its
  default and its gate.
- G2 turns ADR 0042 into a build failure: a new bare HTTP client fails the suite until it is reviewed
  and listed with its reason. The first review found one open site, `offload_nim`'s caller-named
  base. Its key is bound (0.143.1), and an audit-first base allowlist is the next change in L5.
- Promotion by counted data means some protections stay in audit mode for weeks. That is the price
  of not training the operator to disable them.
- The reliability track is a precondition. Promoting any control to enforce by default waits until
  the harness it lives in stops failing work on its own (the 15-change plan of 2026-09-29).

## Alternatives considered

- **Adopt the OpenShell runtime now.** Rejected for now. It adds a gateway daemon, container-group
  access and vendor telemetry that is on by default, and it wraps agents that are not the problem
  here. A time-boxed trial remains an approval item.
- **Adopt DOCA's services.** Impossible without the hardware; the three transferable designs are
  recorded in the standard.
- **A list of point fixes with no framework.** Rejected. Both in-house defects above sat beside
  controls that already existed, and a list has no rule for when a control may block work.

## Related

- [systems/security.md](../../systems/security.md) — the standard
- [ADR 0003](0003-policy-broker-and-capability-flags-off-by-default.md) — the policy broker and flags off by default
- [ADR 0036](0036-the-agent-lane-is-a-harnessed-environment.md) — the agent lane is a harnessed environment
- [ADR 0042](0042-no-bare-http-client-on-a-caller-named-host.md) — no bare HTTP client on a caller-named host
