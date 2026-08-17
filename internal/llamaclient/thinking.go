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
}

// WithoutThinking asks the server to render the seat's chat template in
// NON-thinking mode (`chat_template_kwargs: {"enable_thinking": false}`).
// Use it for calls that are a mechanical shape transformation rather than a
// reasoning step — see repackStructured, where thinking is not merely wasted
// but actively destroys the answer.
func WithoutThinking() GenOption {
	return func(o *genOpts) { o.noThinking = true }
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
