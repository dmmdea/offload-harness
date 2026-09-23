// thinking.go — per-call chat-template control. llama.cpp's `--jinja` mode
// renders the model's OWN chat template, and a THINKING template (Qwen3-class)
// emits its answer into `reasoning_content`, not `content`.
package llamaclient

// GenOption is a per-call knob on Generate. It is a variadic option rather
// than a tenth positional parameter or a `GenerateNoThink` twin for two
// reasons: every existing call site compiles and serializes byte-identically
// (an unset option writes nothing to the body), and this package already
// carries three near-duplicate Generate methods — a second axis of boolean
// variants would multiply them, while options compose.
type GenOption func(*genOpts)

// genOpts is the resolved per-call option set. Its zero value is the historical
// behavior, so an absent option can never change a request.
type genOpts struct {
	noThinking bool
	// jsonSchema is the JSON Schema a vLLM seat is constrained by. nil (the
	// zero value) means nothing was asked for and the request carries no
	// structured-output field at all — the historical shape.
	jsonSchema map[string]any
	// localBusy, when set, is the caller's own reason the call must not load
	// on this box (WithLocalBusy). It changes WHERE the call goes, never the
	// body, so it is not part of RenderKey.
	localBusy string
}

// WithLocalBusy tells the send that THIS box must not serve the call, for the
// given reason: the call rides its cascade lane (cascade_remote_lanes) when
// the lane verifiably serves the same model, exactly as it would under a held
// GPU lease or a busy seat — the reason is what the lane's serve-log line
// prints. It exists for the cascade seat guard (internal/seatguard): a rung
// whose load would EVICT a loaded vLLM seat, where the seat is idle and so is
// not "busy" to either lane gate. The residency check still applies — a lane
// that does not serve the model is never taken, and the call stays local — and
// a client with no lanes ignores the option. An empty reason is no option.
func WithLocalBusy(reason string) GenOption {
	return func(o *genOpts) { o.localBusy = reason }
}

// WithoutThinking asks the server to render the seat's chat template in
// NON-thinking mode (`chat_template_kwargs: {"enable_thinking": false}`).
// Use it for calls that are a mechanical shape transformation rather than a
// reasoning step — see repackStructured, where thinking is not merely wasted
// but actively destroys the answer.
func WithoutThinking() GenOption {
	return func(o *genOpts) { o.noThinking = true }
}

// WithJSONSchema constrains the answer with vLLM's OWN structured-output
// field — `structured_outputs: {"json": <schema>}` — and suppresses the raw
// GBNF `grammar` member for that call.
//
// It exists because `grammar` is llama.cpp's request field and vLLM's request
// model ALLOWS unknown extras: a vLLM seat behind llama-swap accepts the key,
// discards it, and answers unconstrained. Measured over nine days of the
// delegation log (register D-129): 1,018 structured re-packs on vLLM seats
// against 506 on every other seat, and 3-attempt exhaustion at 19.7 % against
// 12.6 %. ADR 0048 makes vLLM a first-class engine, so it gets the field it
// actually reads rather than the one it throws away.
//
// `structured_outputs` is vLLM's CURRENT name for this (the `guided_json` /
// `guided_*` family is deprecated); `response_format` stays unused on every
// engine, as ADR 0002 decided and its 2026-09-18 amendment restates. A nil or
// empty schema is ignored, so a caller that could not build one falls back to
// whatever grammar it passed.
func WithJSONSchema(schema map[string]any) GenOption {
	return func(o *genOpts) {
		if len(schema) > 0 {
			o.jsonSchema = schema
		}
	}
}

// RenderKey names the render an option set asks for, for cache keys: "" for
// the historical (option-free) render and "nothink" for WithoutThinking. Two
// calls on the SAME model with different renders produce different answers —
// a thinking seat's grammar answer vs its non-thinking one — so a cache that
// keys on the model alone would serve one render's answer to the other's
// caller (0.115.18, reviewer finding on PR #302).
func RenderKey(opts ...GenOption) string {
	if applyGenOptions(opts).noThinking {
		return "nothink"
	}
	return ""
}

// applyGenOptions folds the variadic options into one resolved set.
func applyGenOptions(opts []GenOption) genOpts {
	var o genOpts
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// chatTemplateKwargs is llama.cpp's `chat_template_kwargs` passthrough: the
// server injects these keys into the Jinja chat template's render context.
// `enable_thinking` is the key Qwen3-class templates read to decide whether to
// open a reasoning block. It is a POINTER field on the request struct with
// `omitempty`, so a call that does not ask for it emits no key at all.
type chatTemplateKwargs struct {
	EnableThinking bool `json:"enable_thinking"`
}

// templateKwargs returns the request-body value for this option set, or nil
// when nothing was asked for (the omitted-entirely path).
func (o genOpts) templateKwargs() *chatTemplateKwargs {
	if !o.noThinking {
		return nil
	}
	return &chatTemplateKwargs{EnableThinking: false}
}

// structuredOutputs is vLLM's structured-output request object. Only the
// `json` arm is modelled: the harness compiles its own JSON Schema
// (gbnf.JSONSchema), so the `regex`, `choice` and `grammar` (xgrammar EBNF —
// NOT GBNF) arms have no caller here. `json` is NOT omitempty: an arm-less
// structured_outputs object is a request error, and the POINTER field on the
// request struct is what makes the whole object absent when unasked.
type structuredOutputs struct {
	JSON map[string]any `json:"json"`
}

// structured returns the request-body value for this option set, or nil when
// no schema was asked for — the path every llama.cpp call takes, where the
// key is absent from the body entirely.
func (o genOpts) structured() *structuredOutputs {
	if len(o.jsonSchema) == 0 {
		return nil
	}
	return &structuredOutputs{JSON: o.jsonSchema}
}

// grammarFor is the ONE place the two constraint fields are arbitrated: a
// call that carries a JSON schema is bound for a vLLM seat, which ignores
// `grammar`, so the grammar is DROPPED rather than sent alongside. Sending
// both would leave the misleading key on the wire for every future reader of
// a captured request to reason about (register D-129).
func (o genOpts) grammarFor(grammar string) string {
	if len(o.jsonSchema) > 0 {
		return ""
	}
	return grammar
}
