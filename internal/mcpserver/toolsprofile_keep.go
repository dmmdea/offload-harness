package mcpserver

// toolsprofile_keep.go: the reviewed keep-list of the compact tools/list profile.
//
// Compact keeps the opening sentence of every description and, beyond it, only the sentences named here, in the tools named
// with them, verbatim and in the order the description has them. Each entry was read by a person and kept because a caller
// that did not see it could do harm or be misled: a prohibition, a trust or privacy boundary, a cost rule, a limit the schema
// cannot express, the deferred-and-do-it-yourself protocol, a warning that a result is not a verdict. An entry carries the
// sentences it cannot be read without (With) and says why it is kept (Why).
//
// What is NOT here is dropped, and every dropped sentence is accounted for: the review gate
// (TestEveryConstraintSentenceIsKeptOrDroppedWithReason) reads the live full descriptions of every surface (each accelerator alone
// too, local and fleet) and fails for any sentence outside the opening ones, whatever words it holds, that is neither kept by this
// list nor on the dropped list in toolsprofile_dropped_test.go (one row per sentence, with a reason class). So adding a sentence to a
// description means adding it to one of the two lists, and rewording a listed sentence means updating its entry: the entry is the
// exact sentence, because a changed rule has to be read again. A tool with no entry keeps only its opening sentences.
//
// Two kinds of sentence cannot be dropped: an acceptance-authoring rule (acceptanceRule in the gate test), and an enum value's
// explanation unless it is on the short dropped-enum list (toolsprofile_enum_test.go). The deferred:true protocol of every lane is
// kept by an entry per phrasing, and a test compares the count of the literal between the full and the compact list.
//
// An entry need not carry a rule word: a sentence a reviewer wants in the compact list can be named whatever it says.

// compactKeepEntry names a sentence compact keeps, in each of the tools listed. The sentence is what splitSentences produces for
// it: a description of the tool itself, or of any parameter of it at any depth.
type compactKeepEntry struct {
	Tools    []string
	Sentence string
	With     []string // referent sentences kept with it, in the same tools
	Why      string
}

var compactKeep = []compactKeepEntry{
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "NOT the door for single-shot mechanical text: one document + one summarize/classify/extract/triage question goes to the offload_* cascade tool first (seconds; a contract costs a 20-200 s seat run) — this door is for multi-document read-and-reason with context docs, a schema and acceptance checks (register A-102).",
		Why:      "door choice: not for single-shot mechanical text",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Each subtask is a contract {goal, context docs, output_schema, acceptance} run by an autonomous read-only agent loop on THIS box or on a fleet node over the operator's tailnet (never cloud).",
		Why:      "delegation runs read-only and never in the cloud",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "SIZE CONTRACTS FROM THE LIVE CEILING, never from a remembered or written figure: offload_status {section:\"brief\"} reports each seat's agent_ctx_tokens.",
		Why:      "size contracts from the live ceiling, never from a remembered figure",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "results[].acceptance_lint (warn-only, the run still happened) flags acceptance that verifies less than it looks: PARROT-PASSABLE (every content check also matches the goal text, so an echoed question passes as verified and the retry never fires), UNGROUNDED (a contains:/regex: matching nothing in the contract's own context docs — fails right answers), or SHAPE-ONLY (nonempty:/min_items: alone — passes garbage).",
		With:     []string{"When present, fix the acceptance (anchor >=1 contains:/regex: to content that appears only in the docs) before reusing the contract."},
		Why:      "acceptance rule, keep-only: what acceptance_lint flags (PARROT-PASSABLE, UNGROUNDED, SHAPE-ONLY: a lone nonempty: passes garbage), so acceptance is written to test content that is not in the goal",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Non-zero means a node is broken or misconfigured, so do NOT read those subtasks as work the local stack honestly could not do — and when NOTHING succeeded the call comes back flagged as an error, with this same JSON body intact (a PARTIAL result, where some subtasks delivered, is a successful call: read summary.failed / lost_to_stack and each result's failed / defer_class / reason for what is missing).",
		With:     []string{"summary.infrastructure counts the results whose story is a broken STACK rather than the work: defers whose defer_class is infrastructure|config, plus a local placement taken while every configured remote failed its health probe."},
		Why:      "do not misread infrastructure defers as the stack's honest limit; the sentence names summary.infrastructure, so the sentence that defines it comes with it",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "WRITE DOOR (0.122.0): add write_root to a subtask to hand a seat a small IMPLEMENTATION leg — it gets write_file/edit_file inside that directory of the NODE'S OWN copy of your inlined docs, and the result carries diff + diff_files, a unified patch YOU review and apply (the harness never applies it; that is what makes handing a 4B an edit safe).",
		Why:      "write door: the harness never applies the diff",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Caps: 8 files, 64 KiB written, 192 KiB of diff, and no delete/shell/run/network — past a cap nothing is published at all.",
		Why:      "the write door's caps, and what it never grants",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Verify it with diff_touches:<path-prefix> and diff_max_files:<n>, which read the write set rather than the prose; neither says the change is CORRECT, so read the diff.",
		Why:      "the write door's acceptance checks read the write set; neither says the change is correct",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "On any refusal before placement it returns deferred:true with a reason and you do the work yourself.",
		Why:      "a refusal before placement is deferred:true and the caller does the work itself",
	},
	{
		Tools:    []string{"agent_delegate", "offload_research"},
		Sentence: "privacy: the material may not leave this box (credentials, unreleased or brand-isolated data); route local only.",
		Why:      "pin_reason vocabulary: privacy, the material may not leave this box",
	},
	{
		Tools:    []string{"agent_delegate", "offload_research"},
		Sentence: "locality: the work needs something only this box has (a local service, files the contract cannot carry inline, this box's own seat as the thing under comparison); route local only.",
		Why:      "pin_reason vocabulary: locality",
	},
	{
		Tools:    []string{"agent_delegate", "offload_research"},
		Sentence: "measurement: the call measures a specific place (a benchmark, a bake, a seat A/B); route local or remote.",
		Why:      "pin_reason vocabulary: measurement, the call measures a specific place",
	},
	{
		Tools:    []string{"agent_delegate", "offload_research"},
		Sentence: "operator: the human operator typed this placement in this conversation (a subagent, workflow stage or script never has it, and a claimed reason is still bounded by the pinned seat's line); route local or remote.",
		Why:      "pin_reason vocabulary: operator, the operator named this placement",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "1 = urgent (claimed first on every node).",
		Why:      "scheduling band 1 = urgent: the one priority value compact left unexplained, next to the kept -1 and 0",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "A call can only narrow the fleet, never add a node: a URL outside delegate_remotes is refused and never dialled, and a box with no delegate_remotes accepts none.",
		Why:      "trust boundary: a call can narrow the fleet and never add a node",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Admitted only with route local, this node's agent_allow_browse, a configured lane and browse_hosts; judged unattended (publish/send-class controls always refused), audited.",
		Why:      "browse admission, and publish/send-class controls are always refused",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "REQUIRED for any remote placement",
		Why:      "a conditional requirement the schema's required list cannot express",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "The result carries diff + diff_files, a unified patch (git apply -p1) that YOU review and apply; the harness never applies it.",
		Why:      "the harness never applies the patch; the caller reviews and applies it",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Grants create+overwrite only: no delete, no shell, no run, no network.",
		Why:      "write_root grants create and overwrite only",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Omit for read-only work, which is everything else",
		Why:      "omit write_root for read-only work (least privilege)",
	},
	{
		Tools:    []string{"agent_rig"},
		Sentence: "Reads the corpus files only; never calls a seat or the cloud.",
		Why:      "reads files only; never calls a seat or the cloud",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "It is READ-ONLY: it cannot write files, run commands, or touch the network.",
		Why:      "read-only: cannot write files, run commands or touch the network",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "Local route only; needs agent_allow_browse on this box, a configured lane and browse_hosts.",
		Why:      "browse is local-route only and needs its lane configured",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "Judged unattended: publish/send-class controls are always refused.",
		Why:      "publish/send-class controls are always refused",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "A `model` this box does not serve is looked up on the fleet (register D-125): with no route, or route remote, it runs as ONE contract through the delegator on a node whose agent seat is that model (the response names node, placement, seat and executed_on, plus node_reported only when the node's own result named itself by another id, carries no sizing of this box, and notes read_root and judge, which do not travel), and a name no node runs is refused with the roster sentence and where the fleet looked (defer_class config when every node answered, infrastructure when one did not); a model on route auto, spread or queue is refused, because they place by capacity and not by seat name, and route local never asks the fleet.",
		Why:      "a model this box does not serve is placed on a fleet node as ONE contract through the delegator: the goal and setup_actions leave the box (read_root and judge do not travel), and a name no node runs is refused",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "A model this box does not serve is looked up on the fleet (register D-125): with no route, or route remote, it runs on a node whose agent seat is that model, else it is refused naming where the fleet looked; route local never asks the fleet",
		Why:      "the model parameter's own statement of the same remote placement: with no route or route remote the run goes to a node whose agent seat is that model, else it is refused naming where the fleet looked; route local never asks the fleet",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "Never gates anything.",
		Why:      "the audit never gates anything (advisory)",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "auto (default; or the box's agent_thinking): every step thinks, and an EMPTY final is re-issued once with thinking off at 4x the step budget, then the run stops as reasoning_starved/empty (a defer, never an empty answer).",
		Why:      "enum-value line of agent_run.thinking: the three values are kept together, so none is explained and the others not",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "on: never send the kwarg",
		With:     []string{"off: every planner call renders in non-thinking mode (chat_template_kwargs enable_thinking:false) — use for grounded extraction on a thinking seat that spends its budget in the think block."},
		Why:      "enum-value line of agent_run.thinking: on never sends the kwarg; its sibling off line is kept with it",
	},
	{
		Tools:    []string{"offload_animate_character", "offload_generate_audio", "offload_generate_video"},
		Sentence: "The named node is the only candidate: this machine's own lane is not used (even when it has one), no other node is tried, and route local with a node is refused.",
		Why:      "a named node is the only candidate: no fallback to the local lane or another node",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "NOT this tool: unbounded exploration with no file list (use agent_run — it searches for its own files); anything that writes or runs (this lane is read-only); a multi-part job needing several contracts (agent_delegate); and above all anything whose answer is a JUDGEMENT rather than a fact the files state — security or credential review, an architecture decision, or the final does-it-actually-work verification.",
		Why:      "what offload_ask is not for: judgement, security or credential review, the final does-it-work check",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "A free seat reports what the files say; it does not own a call you are accountable for.",
		Why:      "a free seat reports what the files say; it does not own a judgement call",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "verified is a CITATION check, not a correctness verdict: it asks whether the published answer quoted one of a few distinctive tokens mined from these files, never whether the answer is right.",
		Why:      "verified is a citation check, not a correctness verdict",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "It REFUSES (deferred:true) when the files hold no token distinctive enough to ground the check, rather than handing back an answer nothing verified.",
		Why:      "ask refuses (deferred:true) rather than hand back an answer nothing verified",
	},
	{
		Tools:    []string{"offload_assess_image", "offload_ocr", "offload_vqa"},
		Sentence: "The image is read on THIS box under vision_max_image_bytes and travels with the job; meta.node / meta.placement say where it ran",
		Why:      "the image is read on this box and travels with the job to the fleet node that runs it: the sentence that says the bytes leave this box (route defaults to local, so only auto and remote move them)",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "SAFETY: controls labelled publish / send / post / delete / pay / buy / checkout / subscribe / confirm / sign out (and similar) are removed before the model sees them and re-checked at execution, so a run ends `denied` rather than clicking one; allow_labels lifts that for named labels on THIS attended door only.",
		Why:      "browse SAFETY: publish/send-class controls are removed and a run ends denied",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "allow_hosts pins the run to hosts (subdomains included): a click on an off-list link or form target is refused, and a page that navigates off-list by itself ends the run.",
		Why:      "allow_hosts pins the run to hosts",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "Never pass pages whose text should not leave this machine (the page text goes to the decision endpoint).",
		Why:      "never pass pages whose text should not leave this machine",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "DONE is the decision model's claim, not proof: verify the outcome yourself.",
		Why:      "DONE is the decision model's claim, not proof",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "Typed decisions come from the LOOPBACK decision endpoint the operator configured (browse_decision_url — the harness holds no key; that local service may itself front a cloud decision model, and the visible page text and element labels are sent to it); field values are written by the LOCAL seat under a grammar.",
		Why:      "the visible page text and the element labels are sent to the decision endpoint, which may itself front a cloud model: why pages whose text must not leave the machine are never passed",
	},
	{
		Tools:    []string{"offload_classify"},
		Sentence: "THE FIRST DOOR for one text + one label set (register A-102): seconds on the entry rung, automatic climb on a low decision margin; never write an agent_delegate contract for a single classification.",
		Why:      "door choice: never write an agent_delegate contract for a single classification",
	},
	{
		Tools:    []string{"offload_compose_video"},
		Sentence: "html and project_dir are TRUSTED CODE ONLY: HyperFrames' Chrome runs without a sandbox, so never pass third-party pages.",
		Why:      "html and project_dir are TRUSTED CODE ONLY; never pass third-party pages",
	},
	{
		Tools:    []string{"offload_compose_video"},
		Sentence: "route places the render: auto (default) runs here when this machine has the composition lane and otherwise on a fleet node; a fleet node renders a template through its template door and html or project_dir as a confined project bundle through its token-gated compose-project door (ADR 0071), and the video and snapshots are fetched back into this machine's media dir (or out).",
		Why:      "the default route (auto) renders on a fleet node when this machine has no composition lane, and an html or project_dir composition then goes there as a project bundle: the one place the caller's files leave the box on this tool",
	},
	{
		Tools:    []string{"offload_depth"},
		Sentence: "Shot analysis / parallax previews, not a production depth pass.",
		Why:      "depth output is a preview, not a production depth pass",
	},
	{
		Tools:    []string{"offload_edit_image"},
		Sentence: "ops is an ARRAY applied in order in one call: crop{x,y,width,height}, resize{width and/or height, keep_aspect}, convert{format png|jpg|webp}, composite{overlay,x,y,opacity}, text{text,x,y,size,color,font,anchor}, mask_boxes{boxes,pad?,feather?,invert?} (REPLACES the working image with a white-on-black inpaint mask at its size — ready for offload_inpaint_image), grade{levels{black,white,gamma}?,curve{points[[in,out],...]}?,wb{mode:gray_world|scale,r,g,b}?,luminance_only?} (tone/color grade — everything composes into ONE LUT per channel, single quantize, no banding; alpha untouched), lut_cube{path,strength?} (.cube 3D LUT look at strength 0-1), perspective_composite{overlay,quad:[[x,y]x4]} (warp the overlay into the destination quad — UL,UR,LR,LL winding — and alpha-composite: mockup placement), finish{sharpen{radius,percent,threshold}?,median 3|5?} (delivery sharpening, defaults tuned for post-AI-upscale web output — MUST be the LAST op, after any resize: sharpening before a resize is undone by resampling), flatten_design{} (FIRST op only: opens a .xcf/.psd via GIMP, flattens it, returns its layer list), and instantiate_design{set_text{LayerName:new copy},replace_image{LayerName:image path}} (FIRST op only: GIMP layered-template factory — sets named text layers' copy, swaps named pixel layers for new images at the same offsets, flattens; the remaining ops then run on the result — a one-call brand-variant factory).",
		Why:      "the ops grammar: the only place the op names and their arguments are listed, with the ordering rules (MUST be the LAST op; FIRST op only)",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "QUALITY-FIRST: renders can take many minutes — that is intended; do not lower steps/resolution to speed things up unless the caller explicitly asks for a draft.",
		Why:      "quality-first: do not lower steps or resolution unless the caller asks for a draft",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "On any failure (unknown family, transparent on a family without alpha, render error) it returns deferred:true — then generate the image another way.",
		Why:      "on any failure the tool returns deferred:true and the caller generates the image another way",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "Only a qwen-image-2.1 family supports it; default false = opaque RGB",
		Why:      "transparent is supported by one family only",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "mq-...: the ticket the harness is holding for this request, which starts on the first matching lane that frees anywhere; do NOT call again to wait for it (a repeat of the identical request attaches to the same ticket), and send the SAME request with waiter_token=mq-... only to collect the result.",
		Why:      "the ticket collect protocol: do NOT call again to wait; re-send the same request with the token only to collect",
	},
	{
		Tools:    []string{"offload_generate_video"},
		Sentence: "The graph family comes from THIS machine's videogen_family binding (the 2x16 reference box is bound to LTX-2.5, which generates joint AAC audio); pass model ONLY to override that deliberately — no cloud, runs on the local GPU.",
		Why:      "pass model ONLY to override the machine's family deliberately; no cloud",
	},
	{
		Tools:    []string{"offload_generate_video"},
		Sentence: "QUALITY-FIRST DEFAULT: the native two-stage recipe (no distill LoRA, 20 steps, cfg 3.5, the model's official negative) — a render takes tens of minutes and that is intended; set fast=true ONLY when the caller explicitly wants a draft (8-step lightx2v distill — visibly weaker motion).",
		Why:      "quality-first default: set fast=true ONLY when the caller explicitly wants a draft",
	},
	{
		Tools:    []string{"offload_generate_video"},
		Sentence: "The default is the native quality recipe — only set when the caller explicitly accepts draft quality",
		Why:      "fast is draft quality: only set when the caller explicitly accepts it",
	},
	{
		Tools:    []string{"offload_generate_video"},
		Sentence: "ltx25 (LTX-2.5 22B distilled, joint AV) | h3 (MiniMax-H3 joint AV — the T4-verdict opt-in: strongest prompt adherence/multi-shot direction and audio design, ~2x LTX wall; still is OPTIONAL for h3 — omit it for t2v storyboard direction) | wan (Wan 2.2 14B two-stage) | hunyuan (needs Hunyuan 1.5 files).",
		Why:      "the only place the accepted model family names are listed",
	},
	{
		Tools:    []string{"offload_inpaint_image"},
		Sentence: "NOTE: diffusion cannot write specific legible text — inpaint-to-clean, then add real type with offload_edit_image's text op.",
		Why:      "diffusion cannot write legible text: inpaint to clean, then add type with the text op",
	},
	{
		Tools:    []string{"offload_nim"},
		Sentence: "This is the ONLY cloud/remote tool on this server — every other offload_* tool runs on the LOCAL models (see offload_status for that roster).",
		Why:      "offload_nim is the only cloud tool on this server",
	},
	{
		Tools:    []string{"offload_nim"},
		Sentence: "It is OPT-IN (the hosted endpoint needs NVIDIA_API_KEY in the server env; a self-hosted NIM via base is keyless): use it deliberately for a stronger model than the local cascade, NOT for routine grunt work.",
		Why:      "offload_nim is opt-in and not for routine grunt work",
	},
	{
		Tools:    []string{"offload_ocr"},
		Sentence: "Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself.",
		Why:      "the lane defers (deferred:true) when it cannot answer confidently, and the caller does the work itself",
	},
	{
		Tools:    []string{"offload_research"},
		Sentence: "The seats never touch the network; your context never pays for the pages.",
		Why:      "the seats never touch the network",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "HOW TO USE THE RESULT: findings are TRIAGE INPUT, not verdicts.",
		Why:      "findings are triage input, not verdicts; the header of the next sentence",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "Read the flagged lines yourself and decide — never apply a finding unread, and treat a `severe` label from a small local model as a prompt to look, not as proof anything is wrong.",
		Why:      "read the flagged lines yourself; never apply a finding unread",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "Equally, an EMPTY findings list means this reviewer found nothing; it is not a verification that the change works.",
		Why:      "an empty findings list is not a verification that the change works",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "ADVISORY ONLY: this lane never gates a merge and never substitutes for the final does-it-actually-work check, which stays yours — as do security review, architecture judgement, and any call you are accountable for.",
		Why:      "ADVISORY ONLY: the review lane never gates a merge and never replaces the final check",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "OUTCOME AND EVIDENCE: every result, early refusals included, carries outcome (PASS | FINDINGS | GATE_FAILED | NOT_RUN | STALE) and evidence binding it to the exact diff judged: diff_sha256, diff_bytes (a refusal before any diff was read carries diff_bytes 0 and no fingerprint), base_sha?/head_sha?",
		With:     []string{"(pass them if you have them: echoed, never resolved), gates (the lane's own diff-size and conflict-markers, then your gate_receipts [{name, status pass|fail, detail?, head_sha?}], attached BEFORE the seat runs), seat (the seat's receipt), recheck_sha256 (the diff fingerprint checked again right before returning: a diff_path is re-read through the same confined reader, but for an inline diff the check compares the string with itself, so it can never detect movement), head_unbound?"},
		Why:      "the outcome words a caller must read (PASS, FINDINGS, GATE_FAILED, NOT_RUN, STALE) and that the evidence binds the result to the exact diff judged; the second sentence is the rest of the evidence list, including that an inline diff's recheck compares the string with itself and can never detect movement",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "THE LANE RUNS NO LINTS OR TESTS ITSELF: gate_receipts are results you ran and hand over, and the lane cannot verify them.",
		Why:      "the lane runs no lints or tests itself: gate_receipts are the caller's own results, and the lane cannot verify them",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "When you pass head_sha, a receipt for another commit, or one that names no commit, cannot support a PASS.",
		Why:      "what binds a PASS: with head_sha, a receipt for another commit, or one that names no commit, cannot support it",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "A review with no head_sha is tied to no commit: head_unbound is true, a receipt that names a commit is unbound (so a clean seat gives GATE_FAILED), and a clean PASS carries outcome_reason no head_sha: gates not tied to a commit.",
		Why:      "a review with no head_sha is tied to no commit: head_unbound, a commit-named receipt is unbound (GATE_FAILED), and a clean PASS says its gates are not tied to a commit",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "Even with head_sha nothing ties the head to the diff bytes: the fingerprint binds the bytes, the head is your word.",
		Why:      "head_sha is the caller's word: the fingerprint binds the diff bytes, nothing binds the head to them",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "PASS needs a seat that said NONE, a diff fingerprint with a second check that agrees, a diff inside the ceiling, green gates for the reviewed commit and an unchanged diff.",
		Why:      "the full list of what a PASS needs: a seat that said NONE, an agreeing second fingerprint check, a diff inside the ceiling, green gates for the reviewed commit, an unchanged diff",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "NOT_RUN means no clean seat reviewed it (every result with deferred:true, an early refusal, an oversize diff, a refused binding, evidence with no diff fingerprint, and a run the node filed as a deferral whose answer the lane read as a clean NONE) and is never a PASS; findings read from such a deferred run stay FINDINGS; STALE means the diff changed during the review, so re-run it; GATE_FAILED is a clean seat beside a red, wrong-commit or unbound gate.",
		Why:      "defines NOT_RUN (never a PASS), STALE (re-run) and GATE_FAILED: the outcome words a caller acts on",
	},
	{
		Tools:    []string{"offload_run_graph"},
		Sentence: "On ANY failure it returns deferred:true with a typed reason (SATISFIER_UNAVAILABLE, VENV_INCOHERENT, SATISFIER_SPAWN_FAILED [a provisioning subprocess failed to START, retried once — transient/retryable, NOT a venv problem], NODE_CLASS_MISSING, PREFLIGHT_MISSING_INPUTS, MODEL_SHA_MISMATCH, GPU_BUSY, TIMEOUT, ...) — it NEVER falls back to cloud; then run the graph another way.",
		Why:      "on any failure run_graph returns deferred:true and never falls back to cloud",
	},
	{
		Tools:    []string{"offload_run_graph"},
		Sentence: "mq-...: the ticket the harness is holding for this graph, which starts on the first matching lane that frees anywhere; do NOT call again to wait for it (a repeat of the identical graph attaches to the same ticket), and send the SAME request with waiter_token=mq-... only to collect the result.",
		Why:      "the ticket collect protocol: do NOT call again to wait; re-send the same request with the token only to collect",
	},
	{
		Tools:    []string{"offload_run_template"},
		Sentence: "A run is allowed only for a non-API template with no custom node pack and a permissive licence class: a paid-API template returns deferred:true with class api_paid and never falls back to cloud, and a template with a blocked, conditional, non-commercial or unlisted licence is refused with its class.",
		Why:      "what a run is allowed for: a paid-API template is deferred with its class and never falls back to cloud, and a licence that is not permissive is refused with its class",
	},
	{
		Tools:    []string{"offload_run_template"},
		Sentence: "It never downloads a model: a run with a model file or an input file missing is refused with the exact list.",
		Why:      "a run never fetches a model: a missing file is a refusal with the exact list, not a download",
	},
	{
		Tools:    []string{"offload_run_template"},
		Sentence: "A prompt embedding the harness cannot verify (a file of the name in another case, more than one file that could answer it, or a file that answers another name the prompt may give) is refused with the class embedding_unverified, and nothing is run on a guess.",
		Why:      "a node that holds an embedding in a way the harness cannot verify is refused under embedding_unverified, a class of its own that is not a missing file: a caller that never saw it would read the refusal as one and go to fetch a file",
	},
	{
		Tools:    []string{"offload_run_template"},
		Sentence: "Every run stays on this machine's lane, and a template that reads input files is never sent to the fleet.",
		Why:      "where a run goes: this machine only, and a template that reads input files is never sent to the fleet",
	},
	{
		Tools:    []string{"offload_status"},
		Sentence: "Every offload_* tool except offload_nim and offload_browse runs LOCAL — the GPU roster or a listed accelerator (free, on-box, no cloud); offload_nim is the only remote MODEL surface, and offload_browse (opt-in, present only when configured) drives the operator's own local browser with typed choices from a loopback decision endpoint the operator runs.",
		Why:      "which tools run local and which leave the box: offload_nim is the only remote model surface",
	},
	{
		Tools:    []string{"offload_status"},
		Sentence: "Trust it over any rules file or written figure.",
		Why:      "the live roster outranks any rules file or written figure",
	},
	{
		Tools:    []string{"offload_status"},
		Sentence: "idle_agent_nodes > 0 means paid-for capacity is sitting unused.",
		Why:      "idle_agent_nodes above zero means paid-for capacity is sitting unused",
	},
	{
		Tools:    []string{"offload_status"},
		Sentence: "Read verdict before concluding anything from \"held\"; a held card is queued behind with queue_with, never refused.",
		Why:      "read verdict before concluding anything from held: a held card is queued behind with queue_with, never refused",
	},
	{
		Tools:    []string{"offload_summarize"},
		Sentence: "Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself.",
		Why:      "the lane defers (deferred:true) when it cannot answer confidently, and the caller does the work itself",
	},
	{
		Tools:    []string{"offload_text_embed"},
		Sentence: "space=siglip2 (768-d) pairs only with the siglip2 image side used by offload_zero_shot.",
		Why:      "siglip2 text vectors pair only with the siglip2 image side",
	},
	{
		Tools:    []string{"offload_transcribe"},
		Sentence: "If it can't transcribe confidently it returns deferred:true and you should handle the audio yourself.",
		Why:      "the lane defers (deferred:true) when it cannot answer confidently, and the caller does the work itself",
	},
	{
		Tools:    []string{"offload_transcribe"},
		Sentence: "route (default auto) places the work: this box's whisper when it is free, a fleet node's when a render or an exclusive hold would keep it waiting (every node serves the same whisper family, so that costs no quality; the audio is sent as 16 kHz mono Opus and the transcript files are written HERE, on this box); local pins it to this box, remote forces a fleet node.",
		Why:      "the default route (auto) sends the audio, as 16 kHz mono Opus, to a fleet node when this box's whisper is held: the one default that moves the caller's bytes, and how to pin it (local)",
	},
	{
		Tools:    []string{"offload_forecast"},
		Sentence: "If the forecast can't be run it returns deferred:true with a reason (a bad input names what to fix) and you should handle the series yourself.",
		Why:      "the lane defers (deferred:true) when it cannot run the forecast, and the caller handles the series itself",
	},
	{
		Tools:    []string{"offload_forecast"},
		Sentence: "A result of more than 2,000 numbers is written as JSON to out_path (default <media_dir>/forecast-<sha8>.json) and the answer keeps shape, quantile_levels, result_path and the first and last 8 median steps of each series.",
		Why:      "a large answer carries no median or quantiles inline: the full numbers are only in the file at result_path, and a caller that reads the inline answer as complete would forecast from 16 steps",
	},
	{
		Tools:    []string{"offload_forecast"},
		Sentence: "route (default auto) runs it on this box's forecast model when it has one and its card is free, and on a fleet node's otherwise (every node serves the same model, so that costs no quality; the series travels with the job and the result file is written HERE, on this box); local pins it to this box, remote forces a fleet node.",
		Why:      "the default route (auto) sends the series to a fleet node when this box has no forecast model or its card is held: the one default that moves the caller's data, and how to pin it (local)",
	},
	{
		Tools:    []string{"offload_forecast"},
		Sentence: "remote: force a fleet node; with none eligible it defers with defer_class capacity (config when no delegate_remotes are configured) and never touches the local GPU.",
		Why:      "route value remote: with no eligible node the lane defers and never touches the local GPU",
	},
	{
		Tools:    []string{"offload_triage"},
		Sentence: "THE FIRST DOOR for one text + one yes/no question (register A-102): seconds on the entry rung, automatic climb on a low decision margin; never write an agent_delegate contract for a single check.",
		Why:      "door choice: never write an agent_delegate contract for a single check",
	},
	{
		Tools:    []string{"offload_upscale_image"},
		Sentence: "It SYNTHESIZES plausible detail, so it is an enlargement tool, not a faithful photo restore; for an exact resample with no invented detail use offload_edit_image's resize op (CPU, free).",
		Why:      "upscale synthesizes detail: an enlargement tool, not a faithful restore",
	},
	{
		Tools:    []string{"offload_video_describe"},
		Sentence: "Returns {answer} (which notes what the relevant frames show); if it can't answer confidently it returns deferred:true and you should watch the video yourself.",
		Why:      "the lane defers (deferred:true) when it cannot answer confidently, and the caller does the work itself",
	},
	{
		Tools:    []string{"offload_video_watch"},
		Sentence: "Use this instead of offload_video_describe whenever the video is longer than a few seconds or the answer depends on something not in the first seconds (shot list, continuity, defects at a given time, on-screen text over time).",
		Why:      "use video_watch, not video_describe, for longer videos or later-in-the-video answers",
	},
	{
		Tools:    []string{"offload_vqa"},
		Sentence: "Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself.",
		Why:      "the lane defers (deferred:true) when it cannot answer confidently, and the caller does the work itself",
	},
	// The deferral protocol, one entry per phrasing: a lane that cannot do the work returns deferred:true (or defers) and the caller does it
	// another way. TestCompactKeepsTheDeferralProtocolOfEveryTool compares every description's count of deferred:true between the lists.
	{
		Tools:    []string{"agent_run"},
		Sentence: "Returns {output, steps, stop_reason, tools, model}; on any failure it returns deferred:true with a reason and you do the task yourself.",
		Why:      "the deferral protocol: on any failure the lane returns deferred:true and the caller does the task itself",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "THE WALL IS A HARD DEADLINE on this door: a run still working when timeout_sec ends is cut and deferred (the reason says so), and a slow seat (the 27B) needs far more than the default for a many-step run.",
		Why:      "a run still working when timeout_sec ends is cut and deferred: the caller sizes the wall (a slow seat needs far more than the default)",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "Slow seats (the 27B) defer on short walls: size it from wall_estimate_sec",
		Why:      "slow seats defer on short walls: size timeout_sec from wall_estimate_sec",
	},
	{
		Tools:    []string{"offload_animate_character", "offload_generate_video"},
		Sentence: "On any failure it returns deferred:true — then make the clip another way.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "On any failure it returns deferred:true with a reason and you read the files yourself.",
		Why:      "the deferral protocol: on any failure the lane returns deferred:true and the caller reads the files itself",
	},
	{
		Tools:    []string{"offload_assess_image"},
		Sentence: "Returns the object or deferred:true.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "On any failure it returns deferred:true with a typed reason (NOT_CONFIGURED, BUSY, BAD_INPUT, CLI_MISSING, BROWSER_UNAVAILABLE, DECISION_UNAVAILABLE, TEXT_UNAVAILABLE, HOST_NOT_ALLOWED, DENIED, blocked, budget, TIMEOUT, RUNNER_FAILED) and, where the run started, the run so far as partial.",
		Why:      "the deferral protocol: on any failure the run returns deferred:true with a typed reason (and the run so far as partial)",
	},
	{
		Tools:    []string{"offload_classify"},
		Sentence: "Returns {label, confidence}; low-confidence results are deferred back to you.",
		Why:      "the deferral protocol: a low-confidence classification is deferred back to the caller",
	},
	{
		Tools:    []string{"offload_compose_video"},
		Sentence: "On any failure it returns deferred:true with a typed reason (BAD_INPUT, LINT_ERRORS, CHECK_FAILED, RENDER_FAILED, BROWSER_MISSING, FFMPEG_MISSING, CLI_MISSING, SPAWN_EBUSY, DISK_HEADROOM, TIMEOUT, or no composition route on this machine).",
		Why:      "the deferral protocol: on any failure the render returns deferred:true with a typed reason",
	},
	{
		Tools:    []string{"offload_edit_image"},
		Sentence: "On any failure (engine absent, bad op, tool error) it returns deferred:true — then edit the image another way.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_edit_image_generative"},
		Sentence: "On any failure (no edit binding on this machine, unknown family, images on a single-image family, more than 10 images, missing file, preset on a 2.1 family, render error) it returns deferred:true.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_extract", "offload_extract_image"},
		Sentence: "Returns the extracted object or defers.",
		Why:      "the deferral protocol: the lane returns the object or defers",
	},
	{
		Tools:    []string{"offload_generate_audio"},
		Sentence: "On any failure (GPU busy, no route, worker error, timeout) it returns deferred:true — then synthesize it another way.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_inpaint_image"},
		Sentence: "On any failure (no SDXL-class inpaint binding on this machine, missing files, render error) it returns deferred:true.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_media"},
		Sentence: "On any failure (ffmpeg absent, bad args) it returns deferred:true — then do it another way.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	{
		Tools:    []string{"offload_nim"},
		Sentence: "Returns {model, content, reasoning_content, tokens_in, tokens_out, truncated}; on any failure (no key, endpoint down, bad model) it returns deferred:true with a reason and you handle the prompt yourself.",
		Why:      "the deferral protocol: on any failure the tool returns deferred:true and the caller handles the prompt itself",
	},
	{
		Tools:    []string{"offload_review_diff"},
		Sentence: "On any failure it returns deferred:true with a reason and you review the diff yourself.",
		Why:      "the deferral protocol: on any failure the lane returns deferred:true and the caller reviews the diff itself",
	},
	{
		Tools:    []string{"offload_transcribe"},
		Sentence: "remote: force a fleet node; with none eligible it returns deferred:true with defer_class capacity (config when no delegate_remotes are configured) and never touches the local GPU.",
		Why:      "route value remote: with no eligible node the lane returns deferred:true and never touches the local GPU",
	},
	{
		Tools:    []string{"offload_triage"},
		Sentence: "Returns {decision, reason} or defers.",
		Why:      "the deferral protocol: the lane returns the decision or defers",
	},
	{
		Tools:    []string{"offload_upscale_image"},
		Sentence: "On any failure (no upscale binding on this machine, missing file, half-given or out-of-range size, bad scale/method, render error, size mismatch) it returns deferred:true.",
		Why:      "the deferral protocol: the lane hands the work back as deferred:true and the caller does it another way",
	},
	// Sentences found by the review of every non-opening sentence of every description (the gate reads them all): rules and warnings a
	// caller must not be without that no keyword list had named. Same standard as above: kept verbatim, with the reason.
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "A list here REPLACES the roster for this call, so a one-node list confines every subtask of the call to that node.",
		Why:      "a list in remotes REPLACES the roster, so a one-node list is how a call is confined to one node",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "The grant pins the whole call to this box: its pin_reason is locality, implied when you give none",
		Why:      "the allow_browse grant pins the whole call to this box (pin_reason locality, implied): a consequence of setting it on one subtask",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Use for digest / extract / summarize-shaped contracts where the measured fast seat is adequate (blind coverage 4.65 vs the default's 8.53 — judgment and coverage contracts stay on the default).",
		Why:      "which contracts the fast layer is for: digest, extract and summarize shapes only, judgment and coverage contracts stay on the default (measured)",
	},
	{
		Tools:    []string{"agent_run"},
		Sentence: "NOTE the advertised set may be NARROWED: a box can set a default agent_profile (small-seat tiers do, because an un-narrowed tool list measurably collapses a small planner), and a narrowed profile such as \"research\" drops search_files and the whole offload_* cascade.",
		Why:      "the advertised tool set may be narrowed by the box's agent_profile: do not assume the offload cascade is available inside the run",
	},
	{
		Tools:    []string{"offload_animate_character", "offload_edit_image_generative", "offload_generate_audio", "offload_generate_video", "offload_inpaint_image", "offload_upscale_image"},
		Sentence: "Re-send the SAME request with it within 10 minutes to resume that place with the arrival time it had; omit it on a first call.",
		Why:      "how to resume a queued place: re-send the SAME request with the waiter_token within 10 minutes; omit it on a first call",
	},
	{
		Tools:    []string{"offload_browse"},
		Sentence: "A sidecar (browser-use jev-ultrafast over browser-harness/CDP) opens a background tab in the operator's already-running, logged-in Chromium browser, observes the page as an indexed element table, and executes one CLICK / TYPE_TEXT / SELECT / SCROLL / WAIT per decision.",
		Why:      "the run happens in the operator's already-running, logged-in browser: the privilege boundary behind the SAFETY and allow_hosts rules",
	},
	{
		Tools:    []string{"offload_edit_image_generative"},
		Sentence: "Note it re-renders everything, so fine detail outside the intended change will shift — prefer inpaint when a mask is possible.",
		Why:      "a generative edit re-renders everything, so fine detail outside the intended change shifts: prefer inpaint when a mask is possible",
	},
	{
		Tools:    []string{"offload_edit_image_generative"},
		Sentence: "Address them in the prompt as <image2>..<image10>; image is <image1>",
		Why:      "how a multi-reference edit addresses its images in the prompt: image is <image1>, images are <image2>..<image10>",
	},
	{
		Tools:    []string{"offload_edit_image_generative"},
		Sentence: "Prefer switching preset over setting steps/cfg by hand: half-overriding the pairing renders successfully and looks wrong",
		Why:      "prefer switching preset over setting steps/cfg by hand: half-overriding the pairing renders successfully and looks wrong",
	},
	{
		Tools:    []string{"offload_enhance_low_light"},
		Sentence: "Preview-grade.",
		Why:      "the enhancement is preview-grade, not a production pass (the same warning depth carries)",
	},
	{
		Tools:    []string{"offload_extract"},
		Sentence: "THE FIRST DOOR for one text + one schema (register A-102): seconds on the entry rung, grounding-checked, automatic climb on failure; agent_delegate is for extraction that needs reading across several documents.",
		Why:      "door choice: the first door for one text and one schema; agent_delegate is for extraction across several documents",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "prompt is required (prose sentences beat tag lists on DiT models; quoted text renders as literal text); optional: negative (active on models served with real CFG), width/height (default = the model's native resolution), steps, seed, out.",
		Why:      "how to write the prompt: prose beats tag lists on DiT models, and quoted text renders as literal text",
	},
	{
		Tools:    []string{"offload_generate_image"},
		Sentence: "Caveat: unpaired quote marks used as inch marks can pair into unintended spans and force the (safe) raw-prompt fallback — spell out inches when a prompt also quotes text.",
		Why:      "unpaired quote marks used as inch marks can pair into spans and force the raw-prompt fallback: spell out inches when a prompt also quotes text",
	},
	{
		Tools:    []string{"offload_generate_image", "offload_run_graph"},
		Sentence: "The job goes on when the call returns; keep it under your client's tool-call timeout.",
		Why:      "the job goes on when the call returns: keep wait_s under the client's tool-call timeout",
	},
	{
		Tools:    []string{"offload_generate_image", "offload_run_graph"},
		Sentence: "tk-...: a place in this machine's own line; re-send the SAME request with it within 10 minutes to resume that place, and the call brings the place into the cluster queue.",
		Why:      "how to resume the tk- place: re-send the SAME request with the token within 10 minutes",
	},
	{
		Tools:    []string{"offload_inpaint_image"},
		Sentence: "Values well below 1.0 can produce muted/gray fill on the stock VAEEncodeForInpaint path — prefer 1.0 unless you know the tradeoff",
		Why:      "denoise well below 1.0 can produce muted or gray fill on the stock inpaint path: prefer 1.0 unless the tradeoff is known",
	},
	{
		Tools:    []string{"offload_research"},
		Sentence: "Caps: 12 URLs per call, 2 MiB fetched / 96 KiB text per page, 30 s per fetch.",
		Why:      "research caps: 12 URLs per call, 2 MiB fetched and 96 KiB text per page, 30 s per fetch",
	},
	{
		Tools:    []string{"offload_status"},
		Sentence: "To size or place a contract, pass section:\"brief\" (fleet + one-line gpu_lease/local verdicts + per-card gpu_cards + process, the stale-image signal); section:<block> returns one block.",
		Why:      "how to size or place a contract: pass section:\"brief\"; section:<block> returns one block",
	},
	{
		Tools:    []string{"offload_summarize"},
		Sentence: "THE FIRST DOOR for one text + one mechanical question (register A-102): seconds on the entry rung, automatic climb to the escalation and reasoning rungs on a margin, schema or grounding failure; agent_delegate is for multi-document read-and-reason, and a contract whose goal is to summarize one file is the wrong door.",
		Why:      "door choice: the first door for one text and one mechanical question; agent_delegate is for multi-document read-and-reason, and a contract to summarize one file is the wrong door",
	},
	{
		Tools:    []string{"offload_video_watch"},
		Sentence: "Slower than video_describe (one vision call per window); scale window_sec/fps for very long files.",
		Why:      "video_watch costs one vision call per window, so scale window_sec and fps for very long files",
	},
	// The accelerator tables (acceltools.go). A box that lists a device as a FLEET accelerator advertises its tools as forwarders, and
	// registerAccelTools appends the forwarding note to each description, after an opening sentence that says "LOCAL ... on-box": the
	// note is the sentence that says the call goes to another node and the image bytes travel with it. The note names the device, so
	// there is one entry per device, each listing the tools that device's table has.
	{
		Tools:    []string{"offload_face_detect", "offload_face_embed", "offload_object_detect", "offload_person_embed", "offload_depth", "offload_enhance_low_light", "offload_image_embed", "offload_pose", "offload_segment", "offload_zero_shot"},
		Sentence: "[FLEET: this box has no hailo-8l — the call is forwarded to the fleet node that does; image_path is read HERE and its bytes travel with the job (cap 8 MiB); the result carries placement{node,wall_ms}]",
		Why:      "the forwarding note of a fleet accelerator: the call is forwarded to another node and the image bytes travel with it, which the opening sentence (on-box, free) does not say",
	},
	{
		Tools:    []string{"offload_text_embed"},
		Sentence: "[FLEET: this box has no hailo-8l — the call is forwarded to the fleet node that does; text travels with the job; the result carries placement{node,wall_ms}]",
		Why:      "the forwarding note of a fleet accelerator, for the one tool that takes text and no image: the call is forwarded to another node and the text travels with it, which the opening sentence (on-box, free) does not say",
	},
	{
		Tools:    []string{"offload_classify_image", "offload_object_detect", "offload_semantic_segment", "offload_image_embed"},
		Sentence: "[FLEET: this box has no coral-edgetpu — the call is forwarded to the fleet node that does; image_path is read HERE and its bytes travel with the job (cap 8 MiB); the result carries placement{node,wall_ms}]",
		Why:      "the forwarding note of a fleet accelerator: the call is forwarded to another node and the image bytes travel with it, which the opening sentence (on-box, free) does not say",
	},
	{
		Tools:    []string{"offload_classify_image", "offload_object_detect", "offload_image_embed"},
		Sentence: "[FLEET: this box has no rknpu — the call is forwarded to the fleet node that does; image_path is read HERE and its bytes travel with the job (cap 8 MiB); the result carries placement{node,wall_ms}]",
		Why:      "the forwarding note of a fleet accelerator: the call is forwarded to another node and the image bytes travel with it, which the opening sentence (on-box, free) does not say",
	},
	// offload_image_embed is a name all three devices share; a box that lists only the Coral or only the RKNPU advertises that
	// device's own description, and the combined surfaces never show it (the shared-name rule gives the name to the first device).
	{
		Tools:    []string{"offload_image_embed"},
		Sentence: "space=efficientnet-edgetpu-s — NOT a CLIP space: there is no text tower to pair it with (no offload_text_embed / offload_zero_shot on this device).",
		Why:      "Coral image vectors are in a space with no text tower: they pair with no offload_text_embed or offload_zero_shot vector",
	},
	{
		Tools:    []string{"offload_image_embed"},
		Sentence: "space names the embedding space — NOT the Hailo's tinyclip and NOT the Coral's efficientnet-edgetpu-s, so vectors from different devices are never comparable, even at the same dimension.",
		Why:      "vectors from different devices' embedding spaces are never comparable, even at the same dimension (the sibling of the siglip2 pairing rule)",
	},
	// Found by the enum check once it read the tool-level description and the prose and list shapes of an explanation (TestCompactExplainsEveryEnumValueTheFullListExplains):
	// the sentence that says what a value of an enum parameter is or does, which the compact list had dropped while keeping the value. Kept verbatim.
	{
		Tools:    []string{"agent_run"},
		Sentence: "Prefer \"build\" for reading and reasoning over a codebase; \"general\" advertises everything.",
		Why:      "which profile to choose: \"build\" for reading and reasoning over a codebase, \"general\" advertises everything (the only place the profile values are explained)",
	},
	{
		Tools:    []string{"offload_classify_image"},
		Sentence: "domain=imagenet (default; EfficientNet-EdgeTPU-S, 1000 ImageNet classes) or birds|insects|plants (MobileNet v2 iNat).",
		Why:      "what each domain value is: imagenet (the default, 1000 ImageNet classes) or birds|insects|plants; the only place the domain values are explained",
	},
	{
		Tools:    []string{"offload_compose_video"},
		Sentence: "quality defaults to this machine's compose_quality (high = libx264 slow CRF 15).",
		Why:      "that quality defaults to the machine's compose_quality and what high is (libx264 slow CRF 15); the only place the quality values are explained",
	},
	{
		Tools:    []string{"offload_zero_shot"},
		Sentence: "space=tinyclip (default) or siglip2 (stronger: 73.0 vs 67.8 top-1).",
		Why:      "what the two embedding spaces are: tinyclip is the default, siglip2 is the stronger one (73.0 vs 67.8 top-1); the choice the space enum offers",
	},
	{
		Tools:    []string{"offload_edit_image_generative"},
		Sentence: "On the default (Qwen-Image-Edit 2511) binding the working canvas follows the source within 0.9-2.0 MP (a smaller source is scaled up to ~0.9 MP, a larger one down to 2 MP, unless the machine pins gen_edit_megapixels); preset trades speed for fidelity there: lightning8 (default, ~4x faster) or full.",
		Why:      "which preset is the default and what it costs: lightning8 (default, ~4x faster) or full, on the 2511 binding, and the canvas the edit renders at; the preset parameter lists full | lightning8 | lightning4 with no enum, and this is the only sentence that says which one is the default",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "results[].ran_on says where a subtask ran: \"local\" is this box's own seat, \"node\" a fleet node, and the key is absent when no seat ran it (a defer, a shed, a refusal); read it, not the word \"local\" in results[].placement, which is the reason in words and can mention a busy local seat on a result a node ran.",
		Why:      "result reading that stops a misreading of where the data went: ran_on says where a subtask ran (\"local\" this box's own seat, \"node\" a fleet node, absent when no seat ran it), and the word local in results[].placement is the reason in words and can mention a busy local seat on a result a node ran; the enum check also found the route value local quoted here",
	},
	// The same check, once it also read a parameter whose description lists its values with no enum keyword (kind of offload_generate_svg, preset of offload_edit_image_generative): the per-kind spec examples. Kept verbatim.
	{
		Tools:    []string{"offload_generate_svg"},
		Sentence: "Examples — gauge: {\"value\":72,\"max\":100,\"label\":\"Purity\",\"unit\":\"%\"}; comparison-bar: {\"items\":[{\"label\":\"A\",\"value\":10},{\"label\":\"B\",\"value\":20}],\"highlight\":1}; chromatogram: {\"peaks\":[{\"rt\":2.5,\"height\":80,\"label\":\"API\"}]}; icon: {\"name\":\"check\",\"color\":\"#22c55e\"}.",
		Why:      "what spec each kind takes: the spec parameter is free-form JSON and these examples (gauge, comparison-bar, chromatogram, icon) are the only place that shows the fields of each kind",
	},
	// Result-reading sentences that stop a caller from misreading a result it already holds (TestCompactKeepsTheResultReadingSentencesThatStopAMisreading). They were dropped as result shape; each is kept whole, in the source's own words.
	{
		Tools:    []string{"offload_transcribe"},
		Sentence: "Returns {gist (preview), language, duration_sec, num_segments, segments[{id,start,end,text}] (timestamped spans — pull only the ones you need), srt_path, text_path, json_path} plus, when the transcript carries word times (segments[].words), words_reliable and words_drift_sec: words_reliable false means the first word of some segment starts 0.5 s or more away from its segment (up to words_drift_sec seconds), so do not cut audio at word boundaries from those times.",
		Why:      "the transcribe result reading: words_reliable false means the first word of some segment starts 0.5 s or more away from its segment, so a caller must not cut audio at word boundaries from those times; the sentence is kept whole with the result shape it qualifies",
	},
	{
		Tools:    []string{"offload_research"},
		Sentence: "Read summary FIRST; a PARTIAL result (some pages digested, some failed) is a successful call — the error flag is set only when nothing succeeded — so read summary.failed and each result's reason for what is missing; the same whole-call deadline as agent_delegate applies (the page fetch included): at it the finished digests come back and the unfinished pages are `call deadline reached` budget defers; a source with `skipped` produced no result (fetch failed, non-public host, unsupported content type).",
		Why:      "a PARTIAL research result (some pages digested, some failed) is a successful call, the error flag is set only when nothing succeeded, and summary.failed and each result's reason say what is missing; the whole-call deadline defers the unfinished pages",
	},
	{
		Tools:    []string{"offload_research"},
		Sentence: "Two text sources are read as well as pages, both fetched here with no login and no cookies: an RSS or Atom feed (its items' titles, links, dates and summaries become the text; item links are not followed, list them yourself to read them) and, only when the operator has set ytdlp_path, a YouTube watch, youtu.be, shorts, live or embed URL (its subtitles, not its audio; sub_langs picks the languages; a video with none, or a yt-dlp failure, is a skipped source whose error_kind says which; a source with cleanup_error read fine but left its scratch folder behind).",
		Why:      "the two text sources guide a call and bound what is read: a feed's item links are not followed (list them yourself to read them), a YouTube URL is read as subtitles only when the operator has set ytdlp_path, both are fetched with no login and no cookies, and a video with no subtitles or a yt-dlp failure is a skipped source whose error_kind says which while a source with cleanup_error was read fine",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "On verified:false read acceptance_failures (which names the check that did not match) and then the evidence — it is a prompt to look, not proof the answer is wrong, and a question whose subject is a short or question-named identifier can leave nothing anchorable at all.",
		Why:      "on verified:false read acceptance_failures and the evidence: it is a prompt to look, not proof the answer is wrong",
	},
	{
		Tools:    []string{"offload_ask"},
		Sentence: "A REPEAT DOES NOT RE-ROLL THE SEAT: an identical call after a verified:false answer returns that same unverified answer again, cache_hit:true, with no new seat run, and that answer already includes the one cross-seat retry a failed local first attempt gets when no route was named (retried_on, retry_note) — to get another attempt, change the question, which is a different key and always a fresh run.",
		Why:      "an identical call after a verified:false answer returns that same unverified answer (cache_hit:true) with no new seat run; a second attempt needs a changed question",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "defer_class \"contract\" is YOUR contract, not a box: no output_schema for a remote placement, past the origin hop, or bigger than any node's advertised context — rewrite the contract and retry.",
		Why:      "a defer_class contract defer blames the caller's contract and not a box: no output_schema for a remote placement, past the origin hop, or bigger than any node's context; rewrite the contract and retry",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "The whole call has a deadline below the MCP client's abort (agent_call_deadline_sec, default 1,500 s, counted from arrival): at it the finished subtasks' results come back and every unfinished one is a budget defer whose reason opens `call deadline reached; N unfinished` — a result shape, not a failure; re-issue those subtasks in a smaller call.",
		Why:      "the call deadline: the finished subtasks' results come back and every unfinished one is a budget defer whose reason opens call deadline reached, a result shape and not a failure; the caller re-issues those subtasks in a smaller call",
	},
	{
		Tools:    []string{"agent_delegate"},
		Sentence: "Set lint_only:true to preview without dispatching: the call returns each subtask's acceptance_lint and the placement the deal would give it right now, and runs no model, takes no lease and starts no job.",
		Why:      "lint_only is the way to check acceptance and see the placement for free: without it a caller pays a full run to learn what a lint costs nothing to say",
	},
}

// compactKeepByTool is the keep-list indexed by tool name: the set of sentences compact keeps beyond the opening ones.
var compactKeepByTool = buildCompactKeep(compactKeep)

func buildCompactKeep(list []compactKeepEntry) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, e := range list {
		for _, tool := range e.Tools {
			if out[tool] == nil {
				out[tool] = map[string]bool{}
			}
			out[tool][e.Sentence] = true
			for _, w := range e.With {
				out[tool][w] = true
			}
		}
	}
	return out
}
