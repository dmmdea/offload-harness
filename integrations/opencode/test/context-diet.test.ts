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
import { describe, expect, it } from "bun:test";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createHooks, DEFAULTS, type Options } from "../src/plugin.ts";

const tmpLog = () => join(mkdtempSync(join(tmpdir(), "olo-diet-")), "dispatch-log.jsonl");
const opts = (over: Partial<Options> = {}): Options => ({ ...DEFAULTS, dispatchLog: tmpLog(), ...over });
const logRows = (p: string) => {
  try {
    return readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  } catch {
    return [] as any[];
  }
};

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

});
