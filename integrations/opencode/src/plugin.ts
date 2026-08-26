// opencode-local-offload — full local-offload harness support inside opencode.
//
// What Claude Code gets from ~/.claude/rules/local-offload.md + hooks H14/H15, opencode gets
// here — and more, because opencode's plugin surface offers a PLAN-TIME lever Claude Code
// lacks: the system-prompt transform lands in the very generation that composes a
// dispatch, where a PreToolUse hook is already too late (measured 2026-08-23).
//
// Hooks (all fail-open; a plugin error must never break a session):
//   experimental.chat.system.transform  the three-lane dispatch protocol + tool map, every turn
//   tool.definition                     the built-in `task` description names the offload route
//   tool.execute.before (task)          FORCING FUNCTION: read-only-shaped subagent legs are
//                                       rerouted to the `offload` subagent (option-gated)
//   tool.execute.after                  H14 read-counter nudge; delegate placement digest;
//                                       "ran on the offload seat" note on rerouted tasks
//   config                              idempotently provides the offload agent + commands,
//                                       small_model default, so the plugin alone brings parity
//   event                               session heartbeat into the cross-harness dispatch log
//   tool.offload_plugin_status          load proof + doctor
import type { Hooks, Plugin, PluginInput } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { classifyLeg, READ_TOOLS, type LegClass } from "./classify.ts";
import { appendDispatchLog, DEFAULT_LOG, newInstrumentStats, type InstrumentStats } from "./instrument.ts";
import { protocolText, taskDescriptionAddendum } from "./protocol.ts";

export const VERSION = "0.1.0";

export type Options = {
  /** MCP server name the harness is registered under in opencode.jsonc (tool prefix). */
  mcp: string;
  /** Name of the bundled read-only subagent pinned to a local seat. */
  offloadAgent: string;
  /** Model for the offload subagent (provider/model). */
  offloadModel: string;
  /** Default small_model applied when the config has none. */
  smallModel: string;
  /** Reroute read-only-shaped `task` calls to the offload subagent. */
  routeReadOnlyTasks: boolean;
  /** Inject the dispatch protocol into every turn's system prompt. */
  systemProtocol: boolean;
  /** H14-style read-counter nudges. */
  nudges: boolean;
  readNudgeTiers: number[];
  /** Cross-harness dispatch instrument path. */
  dispatchLog: string;
};

export const DEFAULTS: Options = {
  mcp: "harness",
  offloadAgent: "offload",
  offloadModel: "llamacpp/qwen3.8-27b",
  smallModel: "llamacpp/gemma-4-e4b",
  routeReadOnlyTasks: true,
  systemProtocol: true,
  nudges: true,
  readNudgeTiers: [12, 40],
  dispatchLog: DEFAULT_LOG,
};

// Options arrive either from the config `plugin: [[name, {...}]]` form or, for a
// plugins-dir install (no options channel), from OPENCODE_LOCAL_OFFLOAD_OPTIONS (JSON).
// Diagnostics the status tool reports — PER INSTANCE (opencode may load a plugin more than
// once in a process; a shared singleton would report one instance's failures as another's).
export type Diagnostics = { envOptionsError: string | null; smallModelDefaulted: boolean; instrument: InstrumentStats };
export function newDiagnostics(): Diagnostics {
  return { envOptionsError: null, smallModelDefaulted: false, instrument: newInstrumentStats() };
}

// A malformed env option string must be visible, not silently replaced by defaults (it is
// the ONLY options channel for a plugins-dir install).
export function resolveOptions(raw?: Record<string, unknown>, diag?: Diagnostics): Options {
  let env: Record<string, unknown> = {};
  const s = process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS;
  if (s) {
    try {
      env = JSON.parse(s);
    } catch (e) {
      const msg = `OPENCODE_LOCAL_OFFLOAD_OPTIONS ignored: ${(e as Error)?.message ?? e}`;
      if (diag) diag.envOptionsError = msg;
      warn("resolveOptions", msg);
    }
  }
  const merged = { ...DEFAULTS, ...env, ...(raw ?? {}) } as Options;
  if (!Array.isArray(merged.readNudgeTiers) || merged.readNudgeTiers.length === 0) merged.readNudgeTiers = DEFAULTS.readNudgeTiers;
  return merged;
}

type SessionState = {
  reads: number;
  readOnlySpawns: number;
  nudged: Set<number>;
  rerouted: Set<string>; // callIDs rerouted to the offload agent; consumed by tool.execute.after
  delegateCalls: number;
};

// Bounded state: a long-lived opencode server sees many sessions. Sessions are dropped on
// session.deleted events (NOT idle — idle is a transient per-turn status) and by an
// insertion-order cap; rerouted callIDs are consumed by the matching tool.execute.after.
const MAX_SESSIONS = 500;

export function offloadAgentDefinition(o: Options) {
  return {
    description: "Free local read-only specialist: reconnaissance, doc sweeps, digests, extraction over LOCAL files using the local-offload harness tools. Never edits, never runs commands, never browses.",
    mode: "subagent",
    model: o.offloadModel,
    prompt: [
      "You are the OFFLOAD subagent: a read-only reconnaissance and digest specialist running on a free local seat.",
      `Use the ${o.mcp}_* harness tools for bulk work: ${o.mcp}_offload_ask (question + paths, the harness writes the whole contract) the moment you have NAMED FILES and one bounded question, ${o.mcp}_agent_delegate (route:"spread", 2+ contracts with context_paths + output_schema + content acceptance) for multi-file legs, ${o.mcp}_agent_run for one bounded leg, the ${o.mcp}_offload_summarize / ${o.mcp}_offload_classify / ${o.mcp}_offload_extract / ${o.mcp}_offload_triage cascade for mechanical text, ${o.mcp}_offload_ocr / ${o.mcp}_offload_vqa / ${o.mcp}_offload_extract_image for images.`,
      "Read files with your own read/glob/grep tools when a leg is small. Hand the harness NAMED FILES, never a search problem.",
      "Return structured findings with exact file paths and line references. Quote, do not paraphrase, identifiers.",
      "If a leg needs the web, writes, or a judgment call (review, design, architecture), start a line with the exact marker [needs-primary] saying what is needed, and return what you could establish read-only; the primary agent owns those legs.",
      "You never edit files, never run shell commands, never publish.",
    ].join("\n"),
    // Read-only by construction; reading OUTSIDE the project directory is this agent's whole
    // purpose (recon over local files anywhere), so the external_directory gate is opened for
    // it alone — the primary agent keeps opencode's default ask.
    permission: { edit: "deny", bash: "deny", webfetch: "deny", external_directory: "allow" },
  };
}

export function offloadCommands(o: Options) {
  const a = o.offloadAgent;
  const m = o.mcp;
  return {
    "offload-recon": {
      description: "Free local reconnaissance over local files (offload subagent)",
      agent: a,
      subtask: true,
      template: `Reconnaissance over the LOCAL files for: $ARGUMENTS\n\nUse ${m}_agent_run for one bounded leg or ${m}_agent_delegate route:"spread" for 2+ legs, always with NAMED files (context_paths / paths in the goal). Report findings with file paths and line references. Read-only.`,
    },
    "offload-digest": {
      description: "Digest a set of documents on the free local fleet (offload subagent)",
      agent: a,
      subtask: true,
      template: `Digest these documents: $ARGUMENTS\n\nSplit them into contracts of at most 6 named files each and run ONE ${m}_agent_delegate call with route:"spread" (context_paths, a flat output_schema, content acceptance). Merge the results into one digest with per-file sources; state coverage (which contracts deferred or failed acceptance).`,
    },
    "offload-pair": {
      description: "Dispatch two read-only legs as the local+fleet spread pair (offload subagent)",
      agent: a,
      subtask: true,
      template: `Dispatch as a two-contract ${m}_agent_delegate route:"spread" pair (subtask 0 runs on the local seat, subtask 1 on the first fleet node): $ARGUMENTS\n\nEach contract: self-contained goal, context_paths, flat output_schema, acceptance testing CONTENT. After the call, verify results[].placement shows one local + one remote; if the deal went local-only, say why (reason field). Summarize both answers.`,
    },
  };
}

// Pure hook logic exported for tests; the plugin function wires it to opencode.
export function createHooks(o: Options, diagnostics: Diagnostics = newDiagnostics()): Hooks & { _state: Map<string, SessionState>; _diagnostics: Diagnostics } {
  const sessions = new Map<string, SessionState>();
  const log = (ev: Parameters<typeof appendDispatchLog>[0]) => appendDispatchLog(ev, o.dispatchLog, diagnostics.instrument);
  // Subagent (child) sessions: no heartbeat, no read-counter nudges (a nudge telling the
  // offload seat to offload to itself is noise in a small model's context and a false
  // under-use row in the shared log).
  const children = new Set<string>();
  const st = (sid: string): SessionState => {
    let s = sessions.get(sid);
    if (!s) {
      s = { reads: 0, readOnlySpawns: 0, nudged: new Set(), rerouted: new Set(), delegateCalls: 0 };
      sessions.set(sid, s);
      while (sessions.size > MAX_SESSIONS) {
        const oldest = sessions.keys().next().value;
        if (oldest === undefined) break;
        sessions.delete(oldest);
        children.delete(oldest);
      }
    }
    return s;
  };
  const delegateTool = `${o.mcp}_agent_delegate`;

  const hooks: Hooks & { _state: Map<string, SessionState>; _diagnostics: Diagnostics } = {
    _state: sessions,
    _diagnostics: diagnostics,

    config: async (config) => {
      try {
        const c = config as unknown as Record<string, any>;
        c.agent ??= {};
        if (!c.agent[o.offloadAgent]) c.agent[o.offloadAgent] = offloadAgentDefinition(o);
        c.command ??= {};
        for (const [name, def] of Object.entries(offloadCommands(o))) {
          if (!c.command[name]) c.command[name] = def;
        }
        if (!c.small_model && o.smallModel) {
          c.small_model = o.smallModel;
          diagnostics.smallModelDefaulted = true;
          log({ event: "config_default_applied", key: "small_model", value: o.smallModel });
        }
      } catch (e) {
        warn("config hook", e);
      }
    },

    dispose: async () => {
      sessions.clear();
      children.clear();
    },

    // No parameter destructuring: a nullish argument must land in the catch, not become a
    // rejected promise opencode would surface.
    event: async (arg) => {
      try {
        const ev = (arg as { event?: unknown } | undefined)?.event as { type?: string; properties?: { info?: { id?: string; parentID?: string }; sessionID?: string } } | undefined;
        // Prune ONLY on deletion: "idle" is a transient per-turn status (idle → busy every
        // turn), and pruning there would wipe the read counters after every turn.
        if (ev?.type === "session.deleted") {
          const sid = ev.properties?.info?.id ?? ev.properties?.sessionID;
          if (sid) {
            sessions.delete(sid);
            children.delete(sid);
          }
          return;
        }
        if (ev?.type === "session.created") {
          const sid = ev.properties?.info?.id ?? ev.properties?.sessionID ?? "unknown";
          // Subagent (child) sessions carry a parentID; only top-level sessions count as a
          // heartbeat so the weekly read's denominator is not inflated by every task call.
          if (ev.properties?.info?.parentID) {
            children.add(sid);
            return;
          }
          if (!sessions.has(sid)) {
            st(sid);
            log({ event: "session", sid });
          }
        }
      } catch (e) {
        warn("event hook", e);
      }
    },

    "experimental.chat.system.transform": async (_input, output) => {
      try {
        if (!o.systemProtocol) return;
        const text = protocolText(o.mcp, o.offloadAgent);
        if (!output.system.some((s) => s.includes("three-lane dispatch (house protocol"))) output.system.push(text);
      } catch (e) {
        warn("system.transform hook", e);
      }
    },

    "tool.definition": async (input, output) => {
      try {
        if (input.toolID === "task" && !output.description.includes("OFFLOAD ROUTE:")) {
          output.description += taskDescriptionAddendum(o.offloadAgent, o.mcp);
        }
      } catch (e) {
        warn("tool.definition hook", e);
      }
    },

    "tool.execute.before": async (input, output) => {
      try {
        if (input.tool !== "task") return;
        // Copy before mutating: a frozen or shared args object must not turn a reroute into
        // a thrown (and therefore silently skipped) hook.
        const args = { ...((output.args ?? {}) as Record<string, any>) };
        const current = String(args.subagent_type ?? args.agent ?? "");
        const cls: LegClass = classifyLeg(args.description, args.prompt);
        const s = st(input.sessionID);
        if (cls === "read-only") {
          s.readOnlySpawns++;
          log({ event: "readonly_spawn", sid: input.sessionID, n: s.readOnlySpawns, desc: String(args.description ?? "").slice(0, 80), target: current || "default" });
        }
        if (!o.routeReadOnlyTasks || cls !== "read-only" || current === o.offloadAgent) return;
        // The forcing function: route the leg to the free local seat.
        if ("subagent_type" in args || !("agent" in args)) args.subagent_type = o.offloadAgent;
        if ("agent" in args) args.agent = o.offloadAgent;
        args.prompt = `${String(args.prompt ?? "")}\n\n[local-offload] This leg was routed to the free local offload seat because it is read-only over local files. Use the ${o.mcp}_* harness tools; if it turns out to need the web, writes, or a judgment call, say so and return what you could establish read-only.`;
        output.args = args;
        s.rerouted.add(input.callID);
        log({ event: "task_reroute", sid: input.sessionID, from: current || "default", to: o.offloadAgent, desc: String(args.description ?? "").slice(0, 80) });
      } catch (e) {
        warn("tool.execute.before hook", e);
      }
    },

    "tool.execute.after": async (input, output) => {
      try {
        const s = st(input.sessionID);
        if (input.tool === "task" && s.rerouted.has(input.callID)) {
          s.rerouted.delete(input.callID); // consumed
          const text = String(output.output ?? "");
          if (taskFailed(text)) {
            // Never stamp a failure with a success banner: say what happened and what to do.
            output.output += `\n\n[local-offload] The rerouted leg FAILED on the "${o.offloadAgent}" seat (see the error above). Re-run it on the default agent, or check that agent "${o.offloadAgent}" exists and its model is served.`;
            log({ event: "task_reroute_failed", sid: input.sessionID, desc: String(input.args?.description ?? "").slice(0, 80) });
          } else if (taskEscalated(text)) {
            output.output += `\n\n[local-offload] This leg ran on the free local "${o.offloadAgent}" seat and reports it needs the primary agent for part of the work (web, writes, or a judgment call) — see its note above.`;
          } else {
            output.output += `\n\n[local-offload] This leg ran on the free local "${o.offloadAgent}" seat (rerouted: read-only over local files).`;
          }
          return;
        }
        if (input.tool === delegateTool) {
          s.delegateCalls++;
          log({ event: "delegate", sid: input.sessionID, n: Array.isArray(input.args?.subtasks) ? input.args.subtasks.length : -1, route: String(input.args?.route ?? "auto") });
          const digest = delegateDigest(String(output.output ?? ""));
          // The verification step must be visibly present or visibly impossible — never absent.
          output.output += `\n\n${digest ?? "[local-offload] could not verify placement: the delegate output did not parse as the harness result JSON — read results[].placement and summary.infrastructure yourself before trusting the answers."}`;
          return;
        }
        if (children.has(input.sessionID)) return; // subagent context: no meter, no nudge
        if (!o.nudges || !READ_TOOLS.has(input.tool)) return;
        s.reads++;
        if (s.delegateCalls > 0) return; // the session already fans out — no nag
        const tier = o.readNudgeTiers.filter((t) => s.reads >= t).pop();
        if (tier && !s.nudged.has(tier)) {
          s.nudged.add(tier);
          log({ event: "nudge", sid: input.sessionID, tier, reads: s.reads });
          output.output += `\n\n[offload] ${s.reads} file reads this session, local-offload unused. Bounded read-and-reason legs (repo recon, doc sweep, log scan, classify/extract/OCR) run for free on the local seat: ${o.mcp}_offload_ask (cheapest — just a question plus the paths you were about to open), ${o.mcp}_agent_run for one leg that must find its own files, ${o.mcp}_agent_delegate route:"spread" for 2+, or task subagent_type "${o.offloadAgent}". Ignore if every read feeds your own judgment.`;
        }
      } catch (e) {
        warn("tool.execute.after hook", e);
      }
    },

    tool: {
      offload_plugin_status: tool({
        description: "Report the opencode-local-offload plugin's version, active options, per-session counters (reads, reroutes, delegate calls) and whether the harness MCP prefix is configured. Call this to confirm the plugin is loaded.",
        args: {},
        async execute(_args, ctx) {
          const s = sessions.get(ctx.sessionID);
          return JSON.stringify(
            {
              plugin: "opencode-local-offload",
              version: VERSION,
              options: { ...o, dispatchLog: o.dispatchLog },
              session: s ? { reads: s.reads, rerouted: s.rerouted.size, delegateCalls: s.delegateCalls, nudged: [...s.nudged] } : null,
              hooks: ["experimental.chat.system.transform", "tool.definition", "tool.execute.before", "tool.execute.after", "config", "event"],
              diagnostics: { ...diagnostics, instrument: { ...diagnostics.instrument } },
            },
            null,
            2,
          );
        },
      }),
    },
  };
  return hooks;
}

// Reads a harness agent_delegate result and states whether the local+server pair landed —
// the verification step the protocol demands, done for the model so it cannot skip it.
export function delegateDigest(raw: string): string | null {
  try {
    const start = raw.indexOf("{");
    if (start < 0) return null;
    const parsed = JSON.parse(raw.slice(start)) as { summary?: Record<string, number>; results?: Array<{ placement?: string; deferred?: boolean; reason?: string; retried_on?: string; failed?: boolean }> };
    const results = parsed.results ?? [];
    if (results.length === 0) return null;
    const local = results.filter((r) => /local/i.test(r.placement ?? "")).length;
    const remote = results.length - local;
    const infra = parsed.summary?.infrastructure ?? 0;
    const deferred = results.filter((r) => r.deferred).length;
    const lines = [`[local-offload] delegate placement: ${local} local, ${remote} remote of ${results.length}` + (results.length >= 2 && remote === 0 ? " — the pair did NOT land (all local); read results[].reason: missing output_schema, over-size, or node down." : results.length >= 2 ? " — pair landed." : ".")];
    if (infra > 0) lines.push(`[local-offload] summary.infrastructure=${infra}: a node is broken/misconfigured — fix the stack, do not read this as model failure.`);
    if (deferred > 0) lines.push(`[local-offload] ${deferred} contract(s) deferred — do those legs yourself; a defer is normal.`);
    return lines.join("\n");
  } catch {
    return null;
  }
}

// opencode surfaces a failed subagent with a specific envelope ("Error: Subagent failed
// (task_id: …): …", permission rejections, tool execution failures). Only that envelope
// counts: a successful log-analysis leg legitimately begins its answer with a quoted
// "Error: …" line and must not be stamped FAILED.
export function taskFailed(text: string): boolean {
  return /\bsubagent failed\b|\bthe user rejected permission\b|\btool execution failed\b|^\s*error:\s*(task|subagent|agent)\b/i.test(text);
}
// Escalation is detected from the agent's OWN voice only: the exact marker its prompt tells
// it to emit, or first-person phrasing. Third-person prose about other systems ("the cron
// job needs to run nightly", "the table needs the primary index") must never match.
export const ESCALATION_MARKER = "[needs-primary]";
export function taskEscalated(text: string): boolean {
  if (text.includes(ESCALATION_MARKER)) return true;
  return /\b(I|this leg|this seat|this subagent)\s+(need|needs|cannot|can't|could not|couldn't)\s+[^.\n]{0,80}\b(web|network|internet|primary agent|judgment|write access|edit|run commands?)\b/i.test(text);
}

function warn(where: string, e: unknown) {
  try {
    process.stderr.write(`[opencode-local-offload] ${where} failed open: ${(e as Error)?.message ?? e}\n`);
  } catch {
    /* ignore */
  }
}

export const LocalOffloadPlugin: Plugin = async (_input: PluginInput, options?: Record<string, unknown>) => {
  const diagnostics = newDiagnostics();
  const o = resolveOptions(options, diagnostics);
  return createHooks(o, diagnostics);
};

export default LocalOffloadPlugin;
