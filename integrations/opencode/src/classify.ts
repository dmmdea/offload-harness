// Read-only-leg classifier — a verbatim port of the Claude Code H15 hook's measured
// vocabularies (nudge-delegate.js, N11 rebuild 2026-08-24: 17/17 read-only catch, 0/12
// judgment false positives on the fixture corpus). READ_ONLY is tested against the HEAD of
// the leg (description + first 600 chars of the prompt) so quoted brief material deep in a
// prompt cannot false-positive it; JUDGMENT is tested against the FULL text — any judgment
// signal anywhere disqualifies (the fail-safe direction: a missed reroute costs cloud
// tokens, a wrong reroute costs answer quality).
export const READ_ONLY =
  /\b(recon|reconnaissance|sweep|digest|summari[sz]e|summary of|extract|inventory|enumerate|catalog|survey|research|investigate|explore|audit|trace|analy[sz]e|assess (the|current|which|what|how)|map\b|list (every|all|the)|find (all|every|where|which|the|callers|usages|references)|which files|grep|search (the|for)|read (the|all|every|through)|locate|look (through|for|at each|at every)|confirm [^.]{0,40}(status|state|version|exists))\b/i;

// Action verbs keep base + gerund forms only: past participles are stative artifact
// descriptions ("read the merged config") and verb plurals collide with the identical
// noun ("summarize the edits made") — both deliberately excluded.
export const JUDGMENT =
  /\b(critiqu\w*|adversar\w*|refut\w*|rebut\w*|verdicts?|judg\w*|roast\w*|councils?|personas?|red[- ]team\w*|role-?play\w*|act as (a|an|the)|you are the \w+ on|implement|edit(ing)?|write (the )?(code|fix|patch)|fix(ing)?|refactor(ing)?|commit(ting)?|deploy(ing)?|merge|merging|push(ing)?|test(s)? (pass|fail)|debug(ging)?|decid\w*|choos(e|ing) between|(design|plan) (a|an|the|this|new|our|my)\b|review (of )?(this|the|my|our|each|every)? ?(diff|code|pr|patch|change|changes|branch|commit|implementation|design|plan|proposal))\b/i;

// Legs that need what the local loop lacks — network, or the primary's own tools —
// are never rerouted regardless of read-only shape (the named structural exception).
export const NEEDS_NETWORK =
  /\b(web ?search|webfetch|fetch (the )?(url|page|site|docs?)|browse|online|internet|https?:\/\/|github\.com|npm view|api docs)\b/i;

export const HEAD_CHARS = 600;

export type LegClass = "read-only" | "judgment" | "network" | "other";

export function classifyLeg(description: string | undefined, prompt: string | undefined): LegClass {
  const full = `${description ?? ""} ${prompt ?? ""}`;
  if (JUDGMENT.test(full)) return "judgment";
  if (NEEDS_NETWORK.test(full)) return "network";
  if (READ_ONLY.test(full.slice(0, HEAD_CHARS))) return "read-only";
  return "other";
}

// Built-in opencode tools whose calls count as "reading" for the H14-style meter.
export const READ_TOOLS = new Set(["read", "glob", "grep", "list", "ls", "webfetch"]);
