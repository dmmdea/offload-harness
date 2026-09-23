// residency.go reads llama-swap's CO-RESIDENCY rule out of the serving config
// the box actually runs (serving_config_path) and answers one question with
// llama-swap's own semantics: "if this model were requested now, which of the
// running models would llama-swap unload to make room for it?"
//
// It is a reader of the config, never an author: the harness does not decide
// what may share the cards, the operator's llama-swap.yaml does, and a guard
// that hardcoded "the cascade rungs are exclusive with the agent seat" would
// be wrong the day a set is added that runs a twin beside the seat (the
// display layer's set already does). Two routing engines exist and both are
// modelled:
//
//   - matrix (llama-swap v239+; the reference box): vars name model ids, sets
//     are expressions over them (`&` AND, `|` OR, `()` grouping, `+ref`
//     inlines another set) that expand into concrete combinations, and the
//     solver serves a request by choosing, among the combinations that contain
//     it, the one whose evictions cost least (evict_costs, default 1). A model
//     in no set runs alone.
//   - groups: swap (one member at a time), exclusive (a member unloads every
//     other group), persistent (no other group may unload this one's members);
//     a model in no group is in the default group, which swaps and is
//     exclusive. A config with neither block is all default group.
//
// Both the legacy top-level `matrix:` / `groups:` and the current
// `routing.router.settings.{matrix,groups}` (engine chosen by
// `routing.router.use`) are read.
package seatguard

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// maxCombos bounds one set's expansion. A cartesian product of alternatives
// grows multiplicatively; the reference config expands to a few dozen
// combinations, so a set past this is a config the guard refuses to reason
// about rather than one it spends seconds enumerating on a request path.
const maxCombos = 4096

const (
	engineMatrix = "matrix"
	engineGroup  = "group"
	// defaultGroup is llama-swap's name for the group every ungrouped model
	// belongs to.
	defaultGroup = "(default)"
)

// Residency is one parsed serving config's co-residency rule.
type Residency struct {
	engine string
	// canon maps a folded model id or alias to the model id.
	canon map[string]string
	// matrix engine
	sets  []concreteSet
	costs map[string]int // folded model id -> evict cost
	// group engine
	groups   map[string]groupRule // group name -> rule
	memberOf map[string]string    // folded model id -> group name
}

// concreteSet is one combination a matrix set expands to: the models that may
// run together, as sorted, de-duplicated model ids.
type concreteSet struct {
	name string
	ids  []string
}

func (s concreteSet) has(id string) bool {
	for _, x := range s.ids {
		if strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

type groupRule struct {
	swap, exclusive, persistent bool
}

// Eviction is the solver's answer for one request.
type Eviction struct {
	// Evicted is every running model llama-swap would unload, sorted. On a
	// matrix tie it is the UNION of what the tied combinations evict: the
	// guard asks "might this evict the seat", and a tie broken the other way
	// is a yes.
	Evicted []string
	// Set and Target name the combination chosen (matrix only; the first one
	// in name order on a tie), Cost what its evictions cost.
	Set    string
	Target []string
	Cost   int
	// Alone is a model that appears in no matrix set: it can only run on its
	// own, so it evicts everything.
	Alone bool
}

// Describe renders the decision in the shape of llama-swap's own matrix log
// line (`model=… set=… evict=[…] target=[…] cost=…`), so the harness's line and
// llama-swap's can be read side by side.
//
// Caveat — a genuine cost TIE. llama-swap breaks it by set definition order
// and logs the one set it picked; this package cannot see definition order
// (sets decode as a map) and deliberately UNIONS the evictions of every tied
// combination, reporting the first tied set in name order. So on a tie, set=,
// target= and evict= here can differ from llama-swap's line — evict= only ever
// by naming MORE models, never fewer (conservative on purpose: the guard asks
// "might this evict the seat"). The live reference config has such a tie:
// qwen3.8-27b is in both `interactive` and `qwen_aux`, at equal cost.
func (e Eviction) Describe(model string) string {
	if e.Alone {
		return fmt.Sprintf("model=%s set= evict=[%s] (in no set: runs alone)", model, strings.Join(e.Evicted, " "))
	}
	if e.Set == "" {
		return fmt.Sprintf("model=%s evict=[%s]", model, strings.Join(e.Evicted, " "))
	}
	return fmt.Sprintf("model=%s set=%s evict=[%s] target=[%s] cost=%d", model, e.Set, strings.Join(e.Evicted, " "), strings.Join(e.Target, " "), e.Cost)
}

// residencyDoc is the SLICE of a llama-swap config this package reads. Every
// other key is ignored (yaml.v3 decodes unknown keys silently), so a config
// that grows fields every release still parses.
type residencyDoc struct {
	Models map[string]struct {
		Aliases []string `yaml:"aliases"`
	} `yaml:"models"`
	Matrix  *matrixDoc          `yaml:"matrix"`
	Groups  map[string]groupDoc `yaml:"groups"`
	Routing struct {
		Router struct {
			Use      string `yaml:"use"`
			Settings struct {
				Matrix *matrixDoc          `yaml:"matrix"`
				Groups map[string]groupDoc `yaml:"groups"`
			} `yaml:"settings"`
		} `yaml:"router"`
	} `yaml:"routing"`
}

type matrixDoc struct {
	Vars       map[string]string `yaml:"vars"`
	EvictCosts map[string]int    `yaml:"evict_costs"`
	Sets       map[string]string `yaml:"sets"`
}

// groupDoc's swap and exclusive are pointers because llama-swap defaults BOTH
// to true: an absent key and an explicit false must stay distinguishable.
type groupDoc struct {
	Swap       *bool    `yaml:"swap"`
	Exclusive  *bool    `yaml:"exclusive"`
	Persistent bool     `yaml:"persistent"`
	Members    []string `yaml:"members"`
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ParseResidency parses a llama-swap config's co-residency rule. An error
// means the config cannot be reasoned about (not YAML, an unknown or cyclic
// set reference, a malformed expression) — the configs llama-swap itself
// refuses to load — and the caller must treat co-residency as unknown.
func ParseResidency(text []byte) (Residency, error) {
	var doc residencyDoc
	if err := yaml.Unmarshal(text, &doc); err != nil {
		return Residency{}, fmt.Errorf("serving config is not parseable YAML: %w", err)
	}
	r := Residency{canon: map[string]string{}}
	// Ids first, aliases second: an id always names its own model, even when
	// some other entry lists the same string as an alias.
	for id := range doc.Models {
		r.canon[fold(id)] = id
	}
	for id, m := range doc.Models {
		for _, a := range m.Aliases {
			if k := fold(a); k != "" {
				if _, taken := r.canon[k]; !taken {
					r.canon[k] = id
				}
			}
		}
	}

	mx, groups := doc.Matrix, doc.Groups
	if s := doc.Routing.Router.Settings; s.Matrix != nil || len(s.Groups) > 0 {
		if s.Matrix != nil {
			mx = s.Matrix
		}
		if len(s.Groups) > 0 {
			groups = s.Groups
		}
	}
	use := fold(doc.Routing.Router.Use)
	switch {
	case use == engineMatrix || (use == "" && mx != nil):
		if mx == nil {
			return Residency{}, fmt.Errorf("serving config selects the matrix router but declares no matrix block")
		}
		return r, r.buildMatrix(mx)
	case use == "" || use == engineGroup:
		r.buildGroups(groups)
		return r, nil
	}
	return Residency{}, fmt.Errorf("serving config names an unknown router %q (want group or matrix)", doc.Routing.Router.Use)
}

// Canonical resolves a model id or alias to the model id the config declares;
// a name the config does not know is returned as given.
func (r Residency) Canonical(name string) string {
	if id, ok := r.canon[fold(name)]; ok {
		return id
	}
	return strings.TrimSpace(name)
}

func (r *Residency) buildMatrix(mx *matrixDoc) error {
	r.engine = engineMatrix
	// Var names are FOLDED (case-insensitive), while llama-swap matches them
	// exactly. The divergence is deliberate and one-directional: a config
	// whose sets spell a var in another case (`GE4` for `ge4`) is one
	// llama-swap refuses to load (an unresolved name), so folding can only
	// make this reader accept a config the live server would reject — never
	// read a served config differently. Model ids fold the same way, for the
	// same reason the rest of the harness matches seat names case-insensitively.
	vars := make(map[string]string, len(mx.Vars))
	for v, id := range mx.Vars {
		vars[fold(v)] = r.Canonical(id)
	}
	// A var takes precedence when its name is also a model id (llama-swap's
	// rule); anything else resolves through the model table.
	resolve := func(name string) string {
		if id, ok := vars[fold(name)]; ok {
			return id
		}
		return r.Canonical(name)
	}
	names := make([]string, 0, len(mx.Sets))
	for n := range mx.Sets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		combos, err := expandSet(n, mx.Sets, resolve, map[string]bool{})
		if err != nil {
			return err
		}
		for _, c := range combos {
			r.sets = append(r.sets, concreteSet{name: n, ids: c})
		}
	}
	r.costs = make(map[string]int, len(mx.EvictCosts))
	for k, v := range mx.EvictCosts {
		r.costs[fold(resolve(k))] = v
	}
	return nil
}

func (r *Residency) buildGroups(groups map[string]groupDoc) {
	r.engine = engineGroup
	r.groups = map[string]groupRule{defaultGroup: {swap: true, exclusive: true}}
	r.memberOf = map[string]string{}
	for name, g := range groups {
		rule := groupRule{swap: true, exclusive: true, persistent: g.Persistent}
		if g.Swap != nil {
			rule.swap = *g.Swap
		}
		if g.Exclusive != nil {
			rule.exclusive = *g.Exclusive
		}
		r.groups[name] = rule
		for _, m := range g.Members {
			r.memberOf[fold(r.Canonical(m))] = name
		}
	}
}

func (r Residency) groupOf(id string) (string, groupRule) {
	name, ok := r.memberOf[fold(id)]
	if !ok {
		name = defaultGroup
	}
	return name, r.groups[name]
}

func (r Residency) cost(id string) int {
	if c, ok := r.costs[fold(id)]; ok {
		return c
	}
	return 1
}

// Evicts answers "which running models would llama-swap unload to serve
// requested?" running holds the ids /running lists (aliases are resolved too).
// A requested model that is already running evicts nothing: llama-swap serves
// it without consulting the router.
func (r Residency) Evicts(requested string, running []string) Eviction {
	req := r.Canonical(requested)
	var others []string
	seen := map[string]bool{}
	for _, id := range running {
		c := r.Canonical(id)
		if strings.EqualFold(c, req) {
			return Eviction{}
		}
		if k := fold(c); c != "" && !seen[k] {
			seen[k] = true
			others = append(others, c)
		}
	}
	if r.engine == engineMatrix {
		return r.matrixEvicts(req, others)
	}
	return r.groupEvicts(req, others)
}

func (r Residency) matrixEvicts(req string, others []string) Eviction {
	var best []concreteSet
	bestCost := -1
	for _, s := range r.sets {
		if !s.has(req) {
			continue
		}
		cost := 0
		for _, o := range others {
			if !s.has(o) {
				cost += r.cost(o)
			}
		}
		switch {
		case bestCost < 0 || cost < bestCost:
			best, bestCost = []concreteSet{s}, cost
		case cost == bestCost:
			best = append(best, s)
		}
	}
	if len(best) == 0 {
		return Eviction{Evicted: sorted(others), Alone: true}
	}
	evicted := map[string]string{}
	for _, s := range best {
		for _, o := range others {
			if !s.has(o) {
				evicted[fold(o)] = o
			}
		}
	}
	out := make([]string, 0, len(evicted))
	for _, o := range evicted {
		out = append(out, o)
	}
	return Eviction{Evicted: sorted(out), Set: best[0].name, Target: best[0].ids, Cost: bestCost}
}

func (r Residency) groupEvicts(req string, others []string) Eviction {
	gname, g := r.groupOf(req)
	var out []string
	for _, o := range others {
		oname, og := r.groupOf(o)
		switch {
		case oname == gname:
			if g.swap {
				out = append(out, o)
			}
		case g.exclusive && !og.persistent:
			out = append(out, o)
		}
	}
	return Eviction{Evicted: sorted(out)}
}

func sorted(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// expandSet expands one named set into its concrete combinations.
func expandSet(name string, sets map[string]string, resolve func(string) string, visiting map[string]bool) ([][]string, error) {
	expr, ok := sets[name]
	if !ok {
		return nil, fmt.Errorf("matrix set %q is referenced but not defined", name)
	}
	if visiting[name] {
		return nil, fmt.Errorf("matrix set %q references itself", name)
	}
	visiting[name] = true
	defer delete(visiting, name)
	toks, err := tokenize(expr)
	if err != nil {
		return nil, fmt.Errorf("matrix set %q: %w", name, err)
	}
	p := &exprParser{toks: toks, set: name, resolve: resolve, ref: func(ref string) ([][]string, error) {
		return expandSet(ref, sets, resolve, visiting)
	}}
	out, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("matrix set %q: unexpected %q", name, p.toks[p.pos])
	}
	return out, nil
}

// tokenize splits a set expression into the operators & | ( ) and names; a
// name may carry the + reference prefix.
func tokenize(expr string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, c := range expr {
		switch {
		case unicode.IsSpace(c):
			flush()
		case c == '&' || c == '|' || c == '(' || c == ')':
			flush()
			toks = append(toks, string(c))
		case c == '+':
			flush()
			cur.WriteRune(c)
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	for _, t := range toks {
		if t == "+" {
			return nil, fmt.Errorf("a + with no set name after it")
		}
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	return toks, nil
}

// exprParser is a recursive-descent parser over the grammar
//
//	or   := and ('|' and)*
//	and  := term ('&' term)*
//	term := name | '+' set | '(' or ')'
//
// that returns each sub-expression already expanded into combinations.
type exprParser struct {
	toks    []string
	pos     int
	set     string
	resolve func(string) string
	ref     func(string) ([][]string, error)
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) or() ([][]string, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek() == "|" {
		p.pos++
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = append(left, right...)
		if len(left) > maxCombos {
			return nil, fmt.Errorf("matrix set %q expands to more than %d combinations", p.set, maxCombos)
		}
	}
	return left, nil
}

func (p *exprParser) and() ([][]string, error) {
	left, err := p.term()
	if err != nil {
		return nil, err
	}
	for p.peek() == "&" {
		p.pos++
		right, err := p.term()
		if err != nil {
			return nil, err
		}
		if len(left)*len(right) > maxCombos {
			return nil, fmt.Errorf("matrix set %q expands to more than %d combinations", p.set, maxCombos)
		}
		prod := make([][]string, 0, len(left)*len(right))
		for _, a := range left {
			for _, b := range right {
				prod = append(prod, union(a, b))
			}
		}
		left = prod
	}
	return left, nil
}

func (p *exprParser) term() ([][]string, error) {
	t := p.peek()
	switch {
	case t == "":
		return nil, fmt.Errorf("matrix set %q: the expression ends where a model or set was expected", p.set)
	case t == "(":
		p.pos++
		v, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("matrix set %q: unclosed parenthesis", p.set)
		}
		p.pos++
		return v, nil
	case t == ")" || t == "&" || t == "|":
		return nil, fmt.Errorf("matrix set %q: unexpected %q where a model or set was expected", p.set, t)
	case strings.HasPrefix(t, "+"):
		p.pos++
		return p.ref(t[1:])
	}
	p.pos++
	return [][]string{{p.resolve(t)}}, nil
}

// union merges two combinations into one sorted, de-duplicated combination.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if k := fold(x); !seen[k] {
			seen[k] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
