// opencode 1.18.32 hook contracts and the offload context diet (O1-O4, 2026-09-22).
//
// Every shape below was read from the opencode 1.18.32 bundle, not from the plugin types:
// - MCP tools: tool.execute.after receives the RAW MCP result ({content:[...]}) and opencode
//   builds the model-visible text from `content` AFTER the hook, then head-truncates it.
// - Registry tools (task): trigger("tool.execute.before", ..., {args: b}) then execute(b), so only
//   an IN-PLACE change of output.args reaches the tool; a replaced output.args is ignored.
// - LLMRequestPrep.prepare joins agent prompt + env + instructions + skills into ONE system
//   element, runs experimental.chat.system.transform on it, then chat.params.
// - Instruction files are rendered as `Instructions from: <abs path>\n<file text>`.
// - Title and compaction requests carry only their agent prompt as the system text.
import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { PROTOCOL_MARKER } from "../src/protocol.ts";
import { COMPACTION_MAX_OUTPUT_TOKENS, createHooks, DEFAULTS, RECON_TOOLS, resolveOptions, TITLE_MAX_OUTPUT_TOKENS, type Options } from "../src/plugin.ts";

const tmpLog = () => join(mkdtempSync(join(tmpdir(), "olo-diet-")), "dispatch-log.jsonl");
const opts = (over: Partial<Options> = {}): Options => ({ ...DEFAULTS, dispatchLog: tmpLog(), ...over });
const logRows = (p: string) => {
  try {
    return readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  } catch {
    return [] as any[];
  }
};

// The harness MCP server's tools/list, measured 2026-09-22 (34 tools, harness 0.133.0).
const MEASURED_HARNESS_TOOLS = [
  "agent_delegate", "agent_rig", "agent_run", "offload_animate_character", "offload_ask", "offload_assess_image",
  "offload_classify", "offload_classify_image", "offload_edit_image", "offload_edit_image_generative", "offload_extract",
  "offload_extract_image", "offload_generate_audio", "offload_generate_image", "offload_generate_svg",
  "offload_generate_video", "offload_image_embed", "offload_inpaint_image", "offload_media", "offload_nim",
  "offload_object_detect", "offload_ocr", "offload_research", "offload_review_diff", "offload_run_graph",
  "offload_semantic_segment", "offload_status", "offload_summarize", "offload_transcribe", "offload_triage",
  "offload_upscale_image", "offload_video_describe", "offload_video_watch", "offload_vqa",
].map((t) => `harness_${t}`);
const RECON_EXPECTED = [
  "agent_delegate", "agent_run", "offload_ask", "offload_status", "offload_research", "offload_summarize",
  "offload_classify", "offload_extract", "offload_triage", "offload_ocr", "offload_vqa", "offload_extract_image",
].map((t) => `harness_${t}`);
const TIER1_EXPECTED = ["offload_summarize", "offload_classify", "offload_extract", "offload_triage"].map((t) => `harness_${t}`);

// ---- a replica of opencode 1.18.32's permission resolution (bundle: Wildcard.match, Permission
// fromConfig / merge / disabled). Rules are defaults, then the global config, then the agent's own
// config, in object-key order; the LAST matching rule wins; a tool whose last match is a deny with
// pattern "*" is removed from the request.
type Rule = { permission: string; pattern: string; action: string };
// Same semantics as opencode's Wildcard.match (anchored, case-insensitive, `*` = any run, `?` = one
// character, backslashes read as `/`, a trailing " *" also matches nothing), without building a
// RegExp from config text.
function globMatch(v: string, p: string): boolean {
  let i = 0, j = 0, star = -1, mark = 0;
  while (i < v.length) {
    if (j < p.length && (p[j] === "?" || p[j] === v[i])) { i++; j++; }
    else if (j < p.length && p[j] === "*") { star = j++; mark = i; }
    else if (star >= 0) { j = star + 1; i = ++mark; }
    else return false;
  }
  while (j < p.length && p[j] === "*") j++;
  return j === p.length;
}
function wildcardMatch(value: string, pattern: string): boolean {
  const v = value.replaceAll("\\", "/").toLowerCase();
  const p = pattern.replaceAll("\\", "/").toLowerCase();
  if (p.endsWith(" *") && globMatch(v, p.slice(0, -2))) return true;
  return globMatch(v, p);
}
function fromConfig(obj: Record<string, any> | undefined): Rule[] {
  const rules: Rule[] = [];
  for (const [k, v] of Object.entries(obj ?? {})) {
    if (typeof v === "string") rules.push({ permission: k, action: v, pattern: "*" });
    else for (const [p, a] of Object.entries(v as Record<string, string>)) rules.push({ permission: k, pattern: p, action: a });
  }
  return rules;
}
function enabledHarnessTools(cfg: Record<string, any>, agent: string | null): string[] {
  const ruleset: Rule[] = [{ permission: "*", action: "allow", pattern: "*" }, ...fromConfig(cfg.permission), ...(agent ? fromConfig(cfg.agent?.[agent]?.permission) : [])];
  return MEASURED_HARNESS_TOOLS.filter((t) => {
    const last = ruleset.findLast((r) => wildcardMatch(t, r.permission));
    return !(last?.pattern === "*" && last.action === "deny");
  });
}

describe("O2a: the delegate placement digest reaches the model for MCP results", () => {
  const pairJSON = '{"summary":{"succeeded":2,"infrastructure":0},"results":[{"placement":"local"},{"placement":"route=spread → fleet node (slot 2 of 2)"}]}';

  it("adds the digest as a text part of output.content, ahead of the harness JSON", async () => {
    const h = createHooks(opts());
    const out: any = { content: [{ type: "text", text: pairJSON }] }; // the raw MCP shape: no `output` field
    await h["tool.execute.after"]!({ tool: "harness_agent_delegate", sessionID: "m1", callID: "d1", args: { route: "spread", subtasks: [{}, {}] } }, out);
    expect(out.content.length).toBe(2);
    expect(out.content[0]).toEqual({ type: "text", text: expect.stringContaining("[local-offload] delegate placement: 1 local, 1 remote of 2 — pair landed.") });
    expect(out.content[1]).toEqual({ type: "text", text: pairJSON }); // the harness answer is untouched
    // opencode renders the model text by joining the text parts; the digest must be in it
    const rendered = out.content.filter((c: any) => c.type === "text").map((c: any) => c.text).join("\n\n");
    expect(rendered).toContain("pair landed");
    expect(out.output).toBeUndefined(); // no "undefined\n\n…" string on a field opencode never reads
  });

  it("an unparseable MCP result gets the explicit could-not-verify line in content", async () => {
    const h = createHooks(opts());
    const out: any = { content: [{ type: "text", text: "Error: could not reach the fleet" }] };
    await h["tool.execute.after"]!({ tool: "harness_agent_delegate", sessionID: "m2", callID: "d2", args: {} }, out);
    expect(out.content[0].text).toContain("could not verify placement");
  });

  it("the digest is placed FIRST so opencode's head truncation cannot cut it", async () => {
    const h = createHooks(opts());
    const big = '{"summary":{"infrastructure":0},"results":[{"placement":"local","output":"' + "x".repeat(60_000) + '"}]}';
    const out: any = { content: [{ type: "text", text: big }] };
    await h["tool.execute.after"]!({ tool: "harness_agent_delegate", sessionID: "m3", callID: "d3", args: {} }, out);
    const rendered = out.content.map((c: any) => c.text).join("\n\n");
    expect(rendered.slice(0, 200)).toContain("[local-offload] delegate placement: 1 local, 0 remote of 1");
  });
});

describe("O2b: the read-only task reroute takes effect on opencode 1.18.32", () => {
  const roArgs = () => ({ description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" });

  it("mutates output.args IN PLACE (opencode executes the object it passed and ignores a replacement)", async () => {
    const h = createHooks(opts());
    const args = roArgs();
    const out = { args };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "r1", callID: "c1" }, out);
    expect(out.args).toBe(args); // same object
    expect(args.subagent_type).toBe("offload");
    expect(args.prompt).toContain("[local-offload] This leg was routed");
  });

  it("an args object that cannot be changed is not recorded as rerouted and never stamped", async () => {
    const o = opts();
    const h = createHooks(o);
    const args = Object.freeze(roArgs());
    await h["tool.execute.before"]!({ tool: "task", sessionID: "r2", callID: "c2" }, { args });
    expect(args.subagent_type).toBe("general");
    const rows = logRows(o.dispatchLog);
    expect(rows.some((e) => e.event === "task_reroute")).toBe(false);
    expect(rows.some((e) => e.event === "task_reroute_skipped")).toBe(true);
    const after = { title: "", output: "result", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "r2", callID: "c2", args }, after);
    expect(after.output).toBe("result");
  });

  it("stamps 'ran on the offload seat' only when the child session's agent really is offload", async () => {
    const o = opts();
    const h = createHooks(o);
    // child created by the task tool for ANOTHER agent (e.g. the reroute did not take)
    await h["tool.execute.before"]!({ tool: "task", sessionID: "p1", callID: "c1" }, { args: roArgs() });
    await h.event!({ event: { type: "session.created", properties: { info: { id: "ch-general", parentID: "p1", agent: "general" } } } } as any);
    const wrong = { title: "", output: '<task id="ch-general" state="completed">\n<task_result>\nok\n</task_result>\n</task>', metadata: { sessionId: "ch-general" } };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "p1", callID: "c1", args: {} }, wrong);
    expect(wrong.output).not.toContain("ran on the free local");
    expect(logRows(o.dispatchLog).some((e) => e.event === "task_reroute_unconfirmed")).toBe(true);
    // the same flow with an offload child is stamped
    await h["tool.execute.before"]!({ tool: "task", sessionID: "p1", callID: "c2" }, { args: roArgs() });
    await h.event!({ event: { type: "session.created", properties: { info: { id: "ch-offload", parentID: "p1", agent: "offload" } } } } as any);
    const right = { title: "", output: '<task id="ch-offload" state="completed">\n<task_result>\nok\n</task_result>\n</task>', metadata: { sessionId: "ch-offload" } };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "p1", callID: "c2", args: {} }, right);
    expect(right.output).toContain('ran on the free local "offload" seat');
  });

  it("an unknown child (no session.created seen) is never stamped", async () => {
    const h = createHooks(opts());
    await h["tool.execute.before"]!({ tool: "task", sessionID: "p2", callID: "c1" }, { args: roArgs() });
    const after = { title: "", output: "result", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "p2", callID: "c1", args: {} }, after);
    expect(after.output).toBe("result");
  });

  it("the child's agent falls back to opencode's '(@<agent> subagent)' title when info.agent is absent", async () => {
    const h = createHooks(opts());
    await h["tool.execute.before"]!({ tool: "task", sessionID: "p3", callID: "c1" }, { args: roArgs() });
    await h.event!({ event: { type: "session.created", properties: { info: { id: "ch-t", parentID: "p3", title: "Doc sweep (@offload subagent)" } } } } as any);
    const after = { title: "", output: "ok", metadata: { sessionId: "ch-t" } };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "p3", callID: "c1", args: {} }, after);
    expect(after.output).toContain("ran on the free local");
  });

  it("a leg addressed to offload-media is never rerouted to offload", async () => {
    const h = createHooks(opts());
    const args = { ...roArgs(), subagent_type: "offload-media" };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "r5", callID: "c5" }, { args });
    expect(args.subagent_type).toBe("offload-media");
  });

  it("recon mode: a media-shaped read-only leg is not forced onto the recon-only offload agent", async () => {
    const leg = () => ({ description: "Summarize the recording", prompt: "transcribe the video at clips/intro.mp4 and summarize what is said.", subagent_type: "general" });
    const recon = createHooks(opts());
    const a = leg();
    await recon["tool.execute.before"]!({ tool: "task", sessionID: "r6", callID: "c6" }, { args: a });
    expect(a.subagent_type).toBe("general");
    const all = createHooks(opts({ offloadTools: "all" })); // control: offload holds the whole harness
    const b = leg();
    await all["tool.execute.before"]!({ tool: "task", sessionID: "r7", callID: "c7" }, { args: b });
    expect(b.subagent_type).toBe("offload");
  });
});

describe("O1: offloadTools recon | all, and the offload-media subagent", () => {
  it("defaults to recon; anything else falls back to recon; all is accepted", () => {
    expect(DEFAULTS.offloadTools).toBe("recon");
    expect(resolveOptions({ offloadTools: "bogus" } as any).offloadTools).toBe("recon");
    expect(resolveOptions({ offloadTools: "all" }).offloadTools).toBe("all");
  });

  it("the recon set is the twelve read-and-digest lanes", () => {
    expect([...RECON_TOOLS].sort()).toEqual(RECON_EXPECTED.map((t) => t.replace(/^harness_/, "")).sort());
  });

  it("recon: offload resolves to exactly the recon tools, offload-media to every other harness tool, the primary to Tier 1", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    expect(enabledHarnessTools(cfg, null)).toEqual(TIER1_EXPECTED.slice().sort((a, b) => MEASURED_HARNESS_TOOLS.indexOf(a) - MEASURED_HARNESS_TOOLS.indexOf(b)));
    const offload = enabledHarnessTools(cfg, "offload");
    const media = enabledHarnessTools(cfg, "offload-media");
    expect(offload.slice().sort()).toEqual(RECON_EXPECTED.slice().sort());
    expect(media.length).toBe(22);
    for (const t of MEASURED_HARNESS_TOOLS) expect([offload.includes(t), media.includes(t)].filter(Boolean).length).toBe(1); // never stranded, never doubled
    for (const t of ["generate_image", "generate_video", "generate_audio", "edit_image", "transcribe", "video_watch", "nim"]) expect(media).toContain(`harness_offload_${t}`);
    expect(media).toContain("harness_agent_rig");
  });

  it("a tool the harness adds later lands on offload-media, never nowhere (0.135.0 added offload_compose_video)", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    const resolveFor = (agent: string, tool: string) => {
      const rules: Rule[] = [{ permission: "*", action: "allow", pattern: "*" }, ...fromConfig(cfg.permission), ...fromConfig(cfg.agent[agent].permission)];
      const last = rules.findLast((r) => wildcardMatch(tool, r.permission));
      return !(last?.pattern === "*" && last.action === "deny");
    };
    expect(resolveFor("offload-media", "harness_offload_compose_video")).toBe(true);
    expect(resolveFor("offload", "harness_offload_compose_video")).toBe(false);
  });

  it("recon: offload-media is a read-only subagent on the offload model", async () => {
    const h = createHooks(opts({ offloadModel: "prov/seat" }));
    const cfg: any = {};
    await h.config!(cfg);
    const m = cfg.agent["offload-media"];
    expect(m.mode).toBe("subagent");
    expect(m.model).toBe("prov/seat");
    expect(m.permission).toMatchObject({ edit: "deny", bash: "deny", webfetch: "deny", external_directory: "allow" });
  });

  it("all: offload keeps the whole harness and no offload-media agent is provided", async () => {
    const h = createHooks(opts({ offloadTools: "all" }));
    const cfg: any = {};
    await h.config!(cfg);
    expect(enabledHarnessTools(cfg, "offload")).toEqual(MEASURED_HARNESS_TOOLS);
    expect(cfg.agent["offload-media"]).toBeUndefined();
  });

  it("recon also trims a user-defined offload agent, leaving every user key untouched", async () => {
    const h = createHooks(opts());
    const cfg: any = { agent: { offload: { mode: "subagent", prompt: "mine", permission: { edit: "deny", bash: "deny", webfetch: "deny", external_directory: "allow" } } } };
    await h.config!(cfg);
    expect(cfg.agent.offload.prompt).toBe("mine");
    expect(cfg.agent.offload.permission).toMatchObject({ edit: "deny", bash: "deny", webfetch: "deny", external_directory: "allow" });
    expect(enabledHarnessTools(cfg, "offload").slice().sort()).toEqual(RECON_EXPECTED.slice().sort());
  });

  it("a harness key the user set on the agent keeps its value AND wins (inserted defaults never shadow it)", async () => {
    const h = createHooks(opts());
    const cfg: any = { agent: { offload: { mode: "subagent", permission: { edit: "deny", harness_offload_generate_image: "allow" } } } };
    await h.config!(cfg);
    expect(cfg.agent.offload.permission.harness_offload_generate_image).toBe("allow");
    expect(enabledHarnessTools(cfg, "offload")).toContain("harness_offload_generate_image");
  });

  it("a user harness_* deny on the agent is never re-opened by the plugin's allows", async () => {
    const h = createHooks(opts());
    const cfg: any = { agent: { offload: { mode: "subagent", permission: { "harness_*": "deny" } } } };
    await h.config!(cfg);
    expect(cfg.agent.offload.permission["harness_*"]).toBe("deny");
    expect(enabledHarnessTools(cfg, "offload")).toEqual([]);
  });

  it("tier1: a user global allow for one harness tool keeps winning over the plugin's global deny", async () => {
    const h = createHooks(opts());
    const cfg: any = { permission: { skill: { "*": "allow" }, harness_offload_vqa: "allow" } };
    await h.config!(cfg);
    expect(enabledHarnessTools(cfg, null)).toContain("harness_offload_vqa");
    expect(Object.keys(cfg.permission)[0]).toBe("skill"); // user order kept
  });

  it("a user '*' catch-all declared first does not re-open the trimmed tools (#24335 order shape)", async () => {
    const h = createHooks(opts());
    const cfg: any = { permission: { "*": "allow" }, agent: { offload: { permission: { "*": "allow" } } } };
    await h.config!(cfg);
    expect(enabledHarnessTools(cfg, null)).toEqual(TIER1_EXPECTED.slice().sort((a, b) => MEASURED_HARNESS_TOOLS.indexOf(a) - MEASURED_HARNESS_TOOLS.indexOf(b)));
    expect(enabledHarnessTools(cfg, "offload").slice().sort()).toEqual(RECON_EXPECTED.slice().sort());
  });

  it("agent-level allows win over the root harness_* deny (#47946 shape)", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    expect(cfg.permission["harness_*"]).toBe("deny");
    expect(enabledHarnessTools(cfg, "offload")).toContain("harness_agent_delegate");
    expect(enabledHarnessTools(cfg, "offload-media")).toContain("harness_offload_generate_image");
  });

  it("a user-defined offload-media agent is kept as written (only missing harness keys are added)", async () => {
    const h = createHooks(opts());
    const cfg: any = { agent: { "offload-media": { mode: "subagent", model: "x/y", prompt: "mine", permission: { edit: "deny" } } } };
    await h.config!(cfg);
    expect(cfg.agent["offload-media"]).toMatchObject({ mode: "subagent", model: "x/y", prompt: "mine" });
    expect(cfg.agent["offload-media"].permission.edit).toBe("deny");
  });

  // harness #444 adds offload_status {section:"brief"} (fleet block + one-line verdicts, ~4.4 KB
  // instead of ~17.9 KB); an older harness ignores the argument and returns the full dump
  // (checked on 0.133.0), so the wording is right before and after #444 merges.
  it("the offload prompt and the all-mode protocol name the brief roster check", async () => {
    const { protocolText } = await import("../src/protocol.ts");
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    expect(cfg.agent.offload.prompt).toContain('harness_offload_status {section:"brief"}');
    expect(protocolText("harness", "offload", "all")).toContain('harness_offload_status {section:"brief"}');
  });

  it("the plugin's offload prompt names only tools the recon agent can call", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    const named = (cfg.agent.offload.prompt.match(/harness_[a-z_]+/g) ?? []) as string[];
    expect(named.length).toBeGreaterThan(0);
    for (const n of named) expect(RECON_EXPECTED).toContain(n);
  });

  it("tier1 + recon: the protocol routes media legs to offload-media and still names no hidden tool", async () => {
    const h = createHooks(opts());
    const out = { system: ["base"] };
    await h["experimental.chat.system.transform"]!({ sessionID: "main", model: {} as any }, out);
    expect(out.system[0]).toContain('subagent_type "offload-media"');
    const hidden = (out.system[0].match(/harness_[a-z_]+/g) ?? []).filter((n) => !TIER1_EXPECTED.includes(n));
    expect(hidden).toEqual([]);
    const task = { description: "Launch a subagent.", parameters: {} };
    await h["tool.definition"]!({ toolID: "task" }, task);
    expect(task.description).toContain('"offload-media"');
  });
});

describe("O3: child, title and compaction requests", () => {
  let cfgHome: string;
  let agentsPath: string;
  const AGENTS = "# AGENTS.md — house rules\n\n## Prime directive\nDeliver working, verified results.\n\n## Git\nNever mix accounts.\n";
  const prevXdg = process.env.XDG_CONFIG_HOME;
  beforeEach(() => {
    cfgHome = mkdtempSync(join(tmpdir(), "olo-xdg-"));
    mkdirSync(join(cfgHome, "opencode"), { recursive: true });
    agentsPath = resolve(join(cfgHome, "opencode", "AGENTS.md"));
    writeFileSync(agentsPath, AGENTS);
    process.env.XDG_CONFIG_HOME = cfgHome;
  });
  afterEach(() => {
    if (prevXdg === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = prevXdg;
  });
  // opencode 1.18.32 builds ONE element: agent prompt, env, instruction files, skills, joined by "\n"
  const ENV = "You are powered by the model named m. The exact model ID is p/m\nHere is some useful information about the environment you are running in:\n<env>\n  Working directory: /w\n</env>";
  const PROJECT = "Instructions from: /w/AGENTS.md\nproject rule: tests first\n";
  const SKILLS = "Skills provide specialized instructions and workflows for specific tasks.\nUse the skill tool to load a skill when a task matches its description.";
  const system = (prompt: string) => [[prompt, ENV, `Instructions from: ${agentsPath}\n${AGENTS}`, PROJECT, SKILLS].join("\n")];

  async function child(h: ReturnType<typeof createHooks>, id: string, agent: string) {
    await h.event!({ event: { type: "session.created", properties: { info: { id, parentID: "root", agent } } } } as any);
  }

  it("offload child: no dispatch protocol, and the global AGENTS.md segment becomes a 3-line digest", async () => {
    const h = createHooks(opts());
    await child(h, "c-off", "offload");
    const out = { system: system("You are the OFFLOAD subagent.") };
    await h["experimental.chat.system.transform"]!({ sessionID: "c-off", model: {} as any }, out);
    const s = out.system[0];
    expect(out.system.length).toBe(1);
    expect(s).not.toContain(PROTOCOL_MARKER);
    expect(s).not.toContain("Never mix accounts.");
    expect(s).not.toContain(`Instructions from: ${agentsPath}`);
    const digest = s.slice(s.indexOf("House rules (read-only digest)"), s.indexOf(PROJECT));
    expect(digest.trim().split("\n").length).toBeLessThanOrEqual(3);
    expect(digest).toMatch(/verify, then assert/i);
    expect(digest).toMatch(/quote/i);
    expect(digest).toMatch(/instructions addressed to you/i);
    // everything around the segment is intact, in order
    expect(s.startsWith("You are the OFFLOAD subagent.\n" + ENV + "\n")).toBe(true);
    expect(s.endsWith(PROJECT + "\n" + SKILLS)).toBe(true);
  });

  it("offload-media child gets the same diet", async () => {
    const h = createHooks(opts());
    await child(h, "c-med", "offload-media");
    const out = { system: system("You are the OFFLOAD-MEDIA subagent.") };
    await h["experimental.chat.system.transform"]!({ sessionID: "c-med", model: {} as any }, out);
    expect(out.system[0]).not.toContain(PROTOCOL_MARKER);
    expect(out.system[0]).not.toContain("Never mix accounts.");
  });

  it("a child of another agent (general) keeps the full rules and the protocol", async () => {
    const h = createHooks(opts());
    await child(h, "c-gen", "general");
    const out = { system: system("You are general.") };
    await h["experimental.chat.system.transform"]!({ sessionID: "c-gen", model: {} as any }, out);
    expect(out.system[0]).toContain("Never mix accounts.");
    expect(out.system[0]).toContain(PROTOCOL_MARKER);
  });

  it("the main session is unchanged: full AGENTS.md plus the protocol, one element", async () => {
    const h = createHooks(opts());
    const out = { system: system("You are opencode.") };
    await h["experimental.chat.system.transform"]!({ sessionID: "root", model: {} as any }, out);
    expect(out.system.length).toBe(1);
    expect(out.system[0]).toContain("Never mix accounts.");
    expect(out.system[0].split(PROTOCOL_MARKER).length - 1).toBe(1);
  });

  it("fails open when the segment does not match the file on disk (format or content drift)", async () => {
    const h = createHooks(opts());
    await child(h, "c-drift", "offload");
    const drifted = [["You are the OFFLOAD subagent.", ENV, `Instructions from: ${agentsPath}\n${AGENTS.replace("verified", "VERIFIED")}`, SKILLS].join("\n")];
    const out = { system: drifted.slice() };
    await h["experimental.chat.system.transform"]!({ sessionID: "c-drift", model: {} as any }, out);
    expect(out.system[0]).toBe(drifted[0]); // untouched, not truncated at a guessed boundary
    expect(h._diagnostics.systemTransform.childFailOpen).toBe(1);
  });

  it("a rules file saved with a byte-order mark still matches the text the host decoded without it", async () => {
    writeFileSync(agentsPath, String.fromCharCode(0xfeff) + AGENTS);
    const h = createHooks(opts());
    await child(h, "c-bom", "offload");
    const out = { system: system("You are the OFFLOAD subagent.") }; // the host's text carries no BOM
    await h["experimental.chat.system.transform"]!({ sessionID: "c-bom", model: {} as any }, out);
    expect(out.system[0]).not.toContain("Never mix accounts.");
    expect(out.system[0]).toContain("House rules (read-only digest)");
  });

  it("the project AGENTS.md is never touched, only the global one", async () => {
    const h = createHooks(opts());
    await child(h, "c-proj", "offload");
    const out = { system: system("You are the OFFLOAD subagent.") };
    await h["experimental.chat.system.transform"]!({ sessionID: "c-proj", model: {} as any }, out);
    expect(out.system[0]).toContain(PROJECT);
  });

  // The two prompts below are copied verbatim (first sentence) from the opencode 1.18.32 bundle.
  it("title request: nothing is injected", async () => {
    const h = createHooks(opts());
    const title = "You are a title generator. You output ONLY a thread title. Nothing else.\n\n<task>\nGenerate a brief title";
    const out = { system: [title] };
    await h["experimental.chat.system.transform"]!({ sessionID: "root", model: {} as any }, out);
    expect(out.system).toEqual([title]);
  });

  it("compaction request: nothing is injected", async () => {
    const h = createHooks(opts());
    const comp = "You are a context summarization agent. You are given a conversation between a user and an agent.";
    const out = { system: [comp] };
    await h["experimental.chat.system.transform"]!({ sessionID: "root", model: {} as any }, out);
    expect(out.system).toEqual([comp]);
  });

  it("an unrecognised system prompt fails open: the protocol is injected as before", async () => {
    const h = createHooks(opts());
    const out = { system: ["You are a titler (reworded upstream)."] };
    await h["experimental.chat.system.transform"]!({ sessionID: "root", model: {} as any }, out);
    expect(out.system[0]).toContain(PROTOCOL_MARKER);
  });
});

describe("O4: title and compaction requests think less on Qwen-family models", () => {
  const qwen = { id: "qwen3.8-27b-vllm-3card", providerID: "llamacpp", api: { id: "qwen3.8-27b-vllm-3card" } } as any;
  const gemma = { id: "gemma-4-e4b", providerID: "llamacpp", api: { id: "gemma-4-e4b" } } as any;
  const params = (over: Record<string, any> = {}) => ({ temperature: 0.5, topP: 1, topK: 0, maxOutputTokens: 32000 as number | undefined, options: {} as Record<string, any>, ...over });
  const call = async (h: ReturnType<typeof createHooks>, agent: string, model: any, out: ReturnType<typeof params>) =>
    h["chat.params"]!({ sessionID: "s", agent, model, provider: {} as any, message: {} as any }, out as any);

  it("title: thinking off and output capped", async () => {
    const h = createHooks(opts());
    const out = params();
    await call(h, "title", qwen, out);
    expect(out.options.chat_template_kwargs).toEqual({ enable_thinking: false });
    expect(out.maxOutputTokens).toBe(TITLE_MAX_OUTPUT_TOKENS);
    expect(TITLE_MAX_OUTPUT_TOKENS).toBeLessThanOrEqual(64);
  });

  // enable_thinking, not reasoning_effort: measured 2026-09-22 on a Qwen3.6 vLLM seat, the
  // template ignores reasoning_effort "low" (400 of 400 completion tokens were reasoning, same as
  // no kwarg) while enable_thinking false gives 0; Qwen3.8's template honours both.
  it("compaction: thinking off (honoured by every Qwen3.x template), never high, with room for the summary", async () => {
    const h = createHooks(opts());
    const out = params();
    await call(h, "compaction", qwen, out);
    expect(out.options.chat_template_kwargs).toEqual({ enable_thinking: false });
    expect(JSON.stringify(out.options)).not.toContain('"high"');
    expect(out.maxOutputTokens).toBe(COMPACTION_MAX_OUTPUT_TOKENS);
    // the largest measured compaction output (09-18, xhigh) was 13,774 tokens; the cap must not cut it
    expect(COMPACTION_MAX_OUTPUT_TOKENS).toBeGreaterThan(13_774);
  });

  it("other agents and non-Qwen models are untouched", async () => {
    const h = createHooks(opts());
    for (const [agent, model] of [["build", qwen], ["offload", qwen], ["title", gemma], ["compaction", gemma]] as const) {
      const out = params();
      await call(h, agent, model, out);
      expect(out).toEqual(params());
    }
  });

  it("keys the user set win: an explicit effort or thinking flag is left alone", async () => {
    const h = createHooks(opts());
    const a = params({ options: { chat_template_kwargs: { reasoning_effort: "medium" } } });
    await call(h, "compaction", qwen, a);
    expect(a.options.chat_template_kwargs).toEqual({ reasoning_effort: "medium" });
    const b = params({ options: { chat_template_kwargs: { enable_thinking: true } } });
    await call(h, "title", qwen, b);
    expect(b.options.chat_template_kwargs).toEqual({ enable_thinking: true });
  });

  it("other options survive, the user's kwargs object is not mutated, and a lower cap is never raised", async () => {
    const h = createHooks(opts());
    const userKw = { preserve_thinking: true };
    const out = params({ maxOutputTokens: 32, options: { apiKey: "local", chat_template_kwargs: userKw } });
    await call(h, "title", qwen, out);
    expect(out.options.apiKey).toBe("local");
    expect(out.options.chat_template_kwargs).toEqual({ preserve_thinking: true, enable_thinking: false });
    expect(userKw).toEqual({ preserve_thinking: true });
    expect(out.maxOutputTokens).toBe(32);
  });
});
