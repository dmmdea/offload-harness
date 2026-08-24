import { describe, expect, it } from "bun:test";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { classifyLeg } from "../src/classify.ts";
import { appendDispatchLog, newInstrumentStats } from "../src/instrument.ts";
import { createHooks, DEFAULTS, delegateDigest, LocalOffloadPlugin, resolveOptions, taskEscalated, taskFailed, type Options } from "../src/plugin.ts";

const tmpLog = () => join(mkdtempSync(join(tmpdir(), "olo-")), "dispatch-log.jsonl");
const opts = (over: Partial<Options> = {}): Options => ({ ...DEFAULTS, dispatchLog: tmpLog(), ...over });
const lines = (p: string) => {
  try {
    return readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  } catch {
    return [] as any[]; // no writes yet → no file
  }
};

describe("classifier (H15 vocabulary port)", () => {
  const ro = [
    ["Map quota data flow in code", "Map quota data flow in code and report each consumer."],
    ["Research llama.cpp qwen3-vl bug", "Research llama.cpp qwen3-vl text bug reports in the local notes folder."],
    ["README/SKILL semantic audit", "README/SKILL semantic audit: check each doc file for drift."],
    ["Trace request flow", "Trace request flow."],
    ["Find callers", "Find callers of parseConfig."],
    ["Read merged config", "read the merged config and list every route it defines."],
    ["Summarize edits made", "summarize the edits made to the parser since Friday."],
    ["Doc sweep", "read the files under docs and list every decision and when it was made."],
  ];
  for (const [d, p] of ro) it(`read-only: ${d}`, () => expect(classifyLeg(d, p)).toBe("read-only"));
  const jd = [
    ["Roast council: Contrarian", "You are the Contrarian on an engineering design council. Find the fatal flaws. THE BRIEF: sessions spawn read-only research/recon legs; extraction sweeps went all-Claude."],
    ["Councils plural", "A panel of councils will each summarize a different codebase area."],
    ["Adversarial review", "Adversarial review of the diff: try to refute each claim."],
    ["Fix test", "Fix the failing test in delegate_test.go."],
    ["Design work", "Design a caching layer for the fleet probe."],
    ["Gerund actions", "Merging the feature branch and committing the release notes."],
  ];
  for (const [d, p] of jd) it(`judgment: ${d}`, () => expect(classifyLeg(d, p)).toBe("judgment"));
  it("network legs are never rerouted", () => expect(classifyLeg("Research the API", "Research the opencode plugin docs at https://opencode.ai/docs and summarize the hook surface.")).toBe("network"));
  it("neutral text is other", () => expect(classifyLeg("Update badges", "Update the README badges.")).toBe("other"));
});

describe("options", () => {
  it("defaults + env + explicit precedence", () => {
    process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS = JSON.stringify({ mcp: "envmcp", routeReadOnlyTasks: false });
    const o = resolveOptions({ mcp: "explicit" });
    expect(o.mcp).toBe("explicit");
    expect(o.routeReadOnlyTasks).toBe(false);
    delete process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS;
    expect(resolveOptions().mcp).toBe("harness");
  });
  it("malformed env options are ignored", () => {
    process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS = "{not json";
    expect(resolveOptions().mcp).toBe("harness");
    delete process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS;
  });
});

describe("hooks", () => {
  it("system transform injects the protocol once", async () => {
    const h = createHooks(opts());
    const out = { system: ["base"] };
    await h["experimental.chat.system.transform"]!({ model: {} as any }, out);
    await h["experimental.chat.system.transform"]!({ model: {} as any }, out);
    expect(out.system.length).toBe(2);
    expect(out.system[1]).toContain("harness_agent_delegate");
    expect(out.system[1]).toContain('route:"spread"');
  });
  it("system transform respects the option", async () => {
    const h = createHooks(opts({ systemProtocol: false }));
    const out = { system: ["base"] };
    await h["experimental.chat.system.transform"]!({ model: {} as any }, out);
    expect(out.system.length).toBe(1);
  });
  it("task definition gains the offload route", async () => {
    const h = createHooks(opts());
    const out = { description: "Launch a subagent.", parameters: {} };
    await h["tool.definition"]!({ toolID: "task" }, out);
    expect(out.description).toContain('subagent_type "offload"');
    const other = { description: "Read a file.", parameters: {} };
    await h["tool.definition"]!({ toolID: "read" }, other);
    expect(other.description).toBe("Read a file.");
  });
  it("read-only task is rerouted to the offload agent and logged", async () => {
    const o = opts();
    const h = createHooks(o);
    const out = { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "s1", callID: "c1" }, out);
    expect(out.args.subagent_type).toBe("offload");
    expect(out.args.prompt).toContain("[local-offload]");
    const ev = lines(o.dispatchLog);
    expect(ev.some((e) => e.event === "task_reroute" && e.to === "offload" && e.harness === "opencode")).toBe(true);
    const after = { title: "", output: "result", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "s1", callID: "c1", args: out.args }, after);
    expect(after.output).toContain("ran on the free local");
  });
  it("judgment task is NOT rerouted", async () => {
    const h = createHooks(opts());
    const out = { args: { description: "Adversarial review", prompt: "Adversarial review of the diff: refute each claim.", subagent_type: "general" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "s2", callID: "c2" }, out);
    expect(out.args.subagent_type).toBe("general");
  });
  it("reroute is option-gated", async () => {
    const h = createHooks(opts({ routeReadOnlyTasks: false }));
    const out = { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "s3", callID: "c3" }, out);
    expect(out.args.subagent_type).toBe("general");
  });
  it("already-offload task is left alone (no double prompt suffix)", async () => {
    const h = createHooks(opts());
    const out = { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "offload" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "s4", callID: "c4" }, out);
    expect(out.args.prompt).not.toContain("[local-offload]");
  });
  it("read-counter nudge fires at tier 12 once, silent after a delegate call", async () => {
    const o = opts();
    const h = createHooks(o);
    const outputs: string[] = [];
    for (let i = 0; i < 13; i++) {
      const out = { title: "", output: "x", metadata: {} };
      await h["tool.execute.after"]!({ tool: "read", sessionID: "s5", callID: "r" + i, args: {} }, out);
      outputs.push(out.output);
    }
    expect(outputs[11]).toContain("[offload] 12 file reads"); // fires ON the 12th read
    expect(outputs[10]).toBe("x");
    expect(outputs[12]).toBe("x"); // once per tier
    const again = { title: "", output: "x", metadata: {} };
    await h["tool.execute.after"]!({ tool: "read", sessionID: "s5", callID: "r13", args: {} }, again);
    expect(again.output).toBe("x");
    // delegate use silences future tiers
    const d = { title: "", output: '{"summary":{"infrastructure":0},"results":[{"placement":"local"},{"placement":"route=spread → lenovo (slot 2 of 2)"}]}', metadata: {} };
    await h["tool.execute.after"]!({ tool: "harness_agent_delegate", sessionID: "s5", callID: "d1", args: { route: "spread", subtasks: [{}, {}] } }, d);
    expect(d.output).toContain("pair landed");
    for (let i = 0; i < 40; i++) {
      const out = { title: "", output: "x", metadata: {} };
      await h["tool.execute.after"]!({ tool: "grep", sessionID: "s5", callID: "g" + i, args: {} }, out);
      expect(out.output).toBe("x");
    }
    const ev = lines(o.dispatchLog);
    expect(ev.some((e) => e.event === "delegate" && e.n === 2 && e.route === "spread")).toBe(true);
    expect(ev.some((e) => e.event === "nudge" && e.tier === 12)).toBe(true);
  });
  it("config hook provides agent, commands, small_model idempotently", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    expect(cfg.agent.offload.mode).toBe("subagent");
    expect(cfg.agent.offload.model).toBe("llamacpp/qwen3.8-27b");
    expect(Object.keys(cfg.command)).toEqual(["offload-recon", "offload-digest", "offload-pair"]);
    expect(cfg.command["offload-pair"].subtask).toBe(true);
    expect(cfg.small_model).toBe("llamacpp/gemma-4-e4b");
    cfg.agent.offload.model = "custom";
    cfg.small_model = "mine";
    await h.config!(cfg);
    expect(cfg.agent.offload.model).toBe("custom");
    expect(cfg.small_model).toBe("mine");
  });
  it("session event writes a heartbeat", async () => {
    const o = opts();
    const h = createHooks(o);
    await h.event!({ event: { type: "session.created", properties: { info: { id: "sess-abc" } } } as any });
    expect(lines(o.dispatchLog)[0]).toMatchObject({ event: "session", harness: "opencode", sid: "sess-abc" });
  });
  it("plugin entry returns hooks and the status tool executes", async () => {
    const hooks: any = await LocalOffloadPlugin({} as any, { dispatchLog: tmpLog() });
    expect(typeof hooks["tool.execute.before"]).toBe("function");
    const res = await hooks.tool.offload_plugin_status.execute({}, { sessionID: "none" } as any);
    expect(JSON.parse(res as string).plugin).toBe("opencode-local-offload");
  });
});

describe("review-round fixes", () => {
  it("failed rerouted task gets a FAILURE note, not a success banner, and is logged", async () => {
    const o = opts();
    const h = createHooks(o);
    const out = { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "f1", callID: "c1" }, out);
    const after = { title: "", output: "Error: Subagent failed (task_id: x): The user rejected permission to use this specific tool call.", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "f1", callID: "c1", args: out.args }, after);
    expect(after.output).toContain("FAILED on the");
    expect(after.output).not.toContain("ran on the free local");
    expect(lines(o.dispatchLog).some((e) => e.event === "task_reroute_failed")).toBe(true);
    // consumed: a second after-call for the same callID is not annotated again
    const again = { title: "", output: "x", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "f1", callID: "c1", args: out.args }, again);
    expect(again.output).toBe("x");
  });
  it("escalating rerouted task gets the needs-primary note", async () => {
    const h = createHooks(opts());
    const out = { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" } };
    await h["tool.execute.before"]!({ tool: "task", sessionID: "f2", callID: "c2" }, out);
    const after = { title: "", output: "Findings: ... This leg needs the web to confirm the upstream version.", metadata: {} };
    await h["tool.execute.after"]!({ tool: "task", sessionID: "f2", callID: "c2", args: out.args }, after);
    expect(after.output).toContain("needs the primary agent");
  });
  it("unparseable delegate output gets an explicit could-not-verify line", async () => {
    const h = createHooks(opts());
    const d = { title: "", output: "Error: could not reach the fleet", metadata: {} };
    await h["tool.execute.after"]!({ tool: "harness_agent_delegate", sessionID: "f3", callID: "d", args: { route: "spread", subtasks: [{}] } }, d);
    expect(d.output).toContain("could not verify placement");
  });
  it("small_model default is logged as config_default_applied", async () => {
    const o = opts();
    const h = createHooks(o);
    const cfg: any = {};
    await h.config!(cfg);
    expect(lines(o.dispatchLog).some((e) => e.event === "config_default_applied" && e.key === "small_model")).toBe(true);
  });
  it("child sessions get no read-counter nudge and no heartbeat", async () => {
    const o = opts();
    const h = createHooks(o);
    await h.event!({ event: { type: "session.created", properties: { info: { id: "child-1", parentID: "parent-1" } } } } as any);
    for (let i = 0; i < 15; i++) {
      const out = { title: "", output: "x", metadata: {} };
      await h["tool.execute.after"]!({ tool: "read", sessionID: "child-1", callID: "r" + i, args: {} }, out);
      expect(out.output).toBe("x");
    }
    expect(lines(o.dispatchLog).filter((e) => e.event === "session").length).toBe(0);
  });
  it("session.deleted prunes state; dispose clears everything", async () => {
    const h = createHooks(opts());
    await h.event!({ event: { type: "session.created", properties: { info: { id: "p1" } } } } as any);
    expect(h._state.has("p1")).toBe(true);
    await h.event!({ event: { type: "session.deleted", properties: { info: { id: "p1" } } } } as any);
    expect(h._state.has("p1")).toBe(false);
    await h.event!({ event: { type: "session.created", properties: { info: { id: "p2" } } } } as any);
    await h.dispose!();
    expect(h._state.size).toBe(0);
  });
  it("event hook survives a nullish argument (fail-open, no rejection)", async () => {
    const h = createHooks(opts());
    await expect(h.event!(undefined as any)).resolves.toBeUndefined();
  });
  it("malformed env options are surfaced in the status tool diagnostics", async () => {
    process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS = "{not json";
    const hooks: any = await LocalOffloadPlugin({} as any, { dispatchLog: tmpLog() });
    delete process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS;
    const res = JSON.parse((await hooks.tool.offload_plugin_status.execute({}, { sessionID: "none" } as any)) as string);
    expect(res.diagnostics.envOptionsError).toContain("OPENCODE_LOCAL_OFFLOAD_OPTIONS ignored");
    expect(typeof res.diagnostics.instrument.failures).toBe("number");
  });
  it("offload agent prompt and protocol name only prefixed, existing harness tools", async () => {
    const h = createHooks(opts());
    const cfg: any = {};
    await h.config!(cfg);
    const prompt: string = cfg.agent.offload.prompt;
    expect(prompt).toContain("harness_offload_vqa");
    expect(prompt).not.toMatch(/(^|[^_])offload_vqa/);
    const out = { system: [] as string[] };
    await h["experimental.chat.system.transform"]!({ model: {} as any }, out);
    for (const name of ["offload_status", "offload_summarize", "offload_classify", "offload_extract", "offload_triage", "offload_vqa", "offload_ocr", "offload_transcribe", "offload_extract_image", "offload_assess_image", "offload_video_describe", "offload_generate_image", "offload_edit_image", "offload_inpaint_image", "offload_upscale_image", "offload_generate_video", "offload_generate_audio", "offload_generate_svg", "offload_run_graph", "offload_media", "offload_nim", "agent_run", "agent_delegate"]) {
      expect(out.system[0]).toContain(`harness_${name}`);
    }
  });
  it("readonly_spawn n counts read-only spawns, independent of rerouting", async () => {
    const o = opts({ routeReadOnlyTasks: false });
    const h = createHooks(o);
    for (const id of ["a", "b"]) {
      await h["tool.execute.before"]!({ tool: "task", sessionID: "n1", callID: id }, { args: { description: "Doc sweep", prompt: "read the files under docs and list every decision.", subagent_type: "general" } });
    }
    const ns = lines(o.dispatchLog).filter((e) => e.event === "readonly_spawn").map((e) => e.n);
    expect(ns).toEqual([1, 2]);
  });
});

describe("delegate digest", () => {
  it("names a local-only deal", () => {
    const d = delegateDigest('prefix text {"summary":{"infrastructure":2},"results":[{"placement":"route=spread: no eligible remote — local (...)","deferred":true},{"placement":"local","deferred":true}]}');
    expect(d).toContain("2 local, 0 remote");
    expect(d).toContain("did NOT land");
    expect(d).toContain("infrastructure=2");
    expect(d).toContain("2 contract(s) deferred");
  });
  it("returns null on non-JSON", () => expect(delegateDigest("no json here")).toBeNull());
});

describe("instrument", () => {
  it("is append-only (the Claude Code hooks own rotation) and counts writes", () => {
    const p = tmpLog();
    require("node:fs").writeFileSync(p, "x".repeat(1100 * 1024) + "\n");
    const stats = newInstrumentStats();
    expect(appendDispatchLog({ event: "session", sid: "t" }, p, stats)).toBe(true);
    const raw = readFileSync(p, "utf8");
    expect(raw.length).toBeGreaterThan(1100 * 1024); // nothing truncated
    expect(raw.trim().split("\n").pop()).toContain('"event":"session"');
    expect(stats.writes).toBe(1);
  });
  it("counts and reports a write failure without throwing", () => {
    const stats = newInstrumentStats();
    // a directory path cannot be appended to as a file
    expect(appendDispatchLog({ event: "session", sid: "t" }, tmpdir(), stats)).toBe(false);
    expect(stats.failures).toBe(1);
    expect(stats.lastError).toBeTruthy();
  });
});

describe("review round 2", () => {
  it("session.idle does NOT prune counters (idle is a per-turn status)", async () => {
    const h = createHooks(opts());
    await h.event!({ event: { type: "session.created", properties: { info: { id: "i1" } } } } as any);
    for (let i = 0; i < 5; i++) await h["tool.execute.after"]!({ tool: "read", sessionID: "i1", callID: "r" + i, args: {} }, { title: "", output: "x", metadata: {} });
    await h.event!({ event: { type: "session.idle", properties: { sessionID: "i1" } } } as any);
    expect(h._state.get("i1")?.reads).toBe(5);
  });
  it("taskFailed: only opencode's failure envelope, not a quoted Error: line in a real answer", () => {
    expect(taskFailed("Error: Subagent failed (task_id: ses_x): The user rejected permission to use this specific tool call.")).toBe(true);
    expect(taskFailed("Tool execution failed: model not served")).toBe(true);
    expect(taskFailed("Error: disk full at line 42 (quoted from app.log). No other errors found in the scan.")).toBe(false);
    expect(taskFailed("No errors found; 3 warnings listed below.")).toBe(false);
  });
  it("taskEscalated: marker or first-person only, never third-person prose", () => {
    expect(taskEscalated("Findings...\n[needs-primary] the upstream version check requires the web.")).toBe(true);
    expect(taskEscalated("I cannot complete the last step read-only; it needs the primary agent to edit the file.")).toBe(true);
    expect(taskEscalated("This leg needs the web to confirm the upstream version.")).toBe(true);
    expect(taskEscalated("The orders table needs the primary index rebuilt after migration.")).toBe(false);
    expect(taskEscalated("This cron job needs to run nightly at 2am per the crontab entry found.")).toBe(false);
    expect(taskEscalated("the script needs write access to /var/log per its manifest.")).toBe(false);
  });
  it("diagnostics and instrument stats are per plugin instance", async () => {
    process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS = "{bad";
    const a: any = await LocalOffloadPlugin({} as any, { dispatchLog: tmpLog() });
    delete process.env.OPENCODE_LOCAL_OFFLOAD_OPTIONS;
    const b: any = await LocalOffloadPlugin({} as any, { dispatchLog: tmpdir() }); // log path is a dir → writes fail
    await b.event({ event: { type: "session.created", properties: { info: { id: "zz" } } } });
    const ra = JSON.parse(await a.tool.offload_plugin_status.execute({}, { sessionID: "n" }));
    const rb = JSON.parse(await b.tool.offload_plugin_status.execute({}, { sessionID: "n" }));
    expect(ra.diagnostics.envOptionsError).toContain("ignored");
    expect(rb.diagnostics.envOptionsError).toBeNull();
    expect(ra.diagnostics.instrument.failures).toBe(0);
    expect(rb.diagnostics.instrument.failures).toBe(1);
  });
});
