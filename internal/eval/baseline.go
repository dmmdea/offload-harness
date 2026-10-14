package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
)

// The frozen baseline is the gate's pre-registration: the mode, the primary metric and its
// margin, the judge, the efficiency metrics a cut must improve, the sample floor, the bootstrap
// settings, the held-out ids and the declared task list, written ONCE before the first
// candidate run and hashed. A verdict names the hash it was computed under, and the gate
// refuses an input whose hash is not the baseline's, so the rules cannot be moved after the
// numbers are seen. The shape copies compaction-eval's tokens ratchet (freeze once, check
// reproduces), with one more rule: the file is the canonical bytes, byte for byte.

// GateVersion is the version of the rules this package implements. It rides in the baseline,
// in every result and in the era marker. Version 2: unusable candidate attempts enter at the
// worst score and an unfinished one's recorded cost is spent, the judge is frozen, bootstrap_b and
// min_pairs have floors, a declared efficiency metric that got worse vetoes (read from two
// resolved pairs), the margin is bounded by the class width, and the primary is read on the score
// form of the t interval. Version 2 has never been released (the gate is new in the change that
// carries it, and no version 2 baseline exists outside tests), so these rules landed in it
// rather than in a version 3 nobody ran.
const GateVersion = "2"

// Modes.
const (
	ModeSuperiority    = "superiority"
	ModeNonInferiority = "non_inferiority"
)

// Primary metric classes. The class fixes the score scale and the default margin.
const (
	ClassUnitInterval = "unit_interval" // scores in [0,1]; default margin 0.05
	ClassJudge010     = "judge_0_10"    // scores in [0,10]; default margin 0.5
	ClassCustom       = "custom"        // any scale; non-inferiority must carry its own margin
)

// Efficiency metric classes. Tokens, money and resident memory only: INV-5 keeps every
// wall-time term (seconds, latency, wall, duration, time) out of a pass rule, so no class names
// one and freeze refuses any other word.
const (
	EffTokens    = "tokens"
	EffMoney     = "money"
	EffMemoryGiB = "memory_gib"
)

// Defaults filled by FreezeBaseline when the pre-registration leaves a field out.
const (
	DefaultMinPairs         = 16
	DefaultBootstrapB       = 10000
	DefaultSeed       int64 = 1
	// DefaultMaxExcludedShare is the share of attempts or declared tasks that may be unusable
	// before the run is INCONCLUSIVE (the usable-denominator rule).
	DefaultMaxExcludedShare = 0.20
	// Default margins for non-inferiority.
	DefaultMarginUnitInterval = 0.05
	DefaultMarginJudge010     = 0.5
)

// Floors that validate enforces. min_pairs: the primary interval's false-ADOPT rate was measured
// from 16 tasks up (61 of the 63 null cells of the calibration grid, 16 to 80 tasks, are at most
// 3.6% against a nominal 2.5%; the other two, a strong incumbent with a handful of disagreeing
// tasks at 80 tasks, are 4.8% and 8.2%, and the same regime reaches 17.7% at 62 tasks for a
// candidate that loses 5% of the incumbent's passes; at most 4% is tested end to end on 16 to 24
// tasks), so 16 is the smallest sample the gate makes a claim about. bootstrap_b: the paired bootstrap reads the efficiency metrics, and an
// interval read from a handful of resamples is mostly noise: on the cost nulls of the study at 16
// tasks a metric falsely improved in 8% to 13% of runs at B = 10 and in 42% to 50% at B = 1,
// against 3.5% to 5.9% at B = 2000, which is the B the existing callers of BootstrapDeltaMean use.
// The figures are TestCalibrationStudy's and TestEfficiencyCalibrationStudy's (gate_sim_test.go).
const (
	MinBootstrapB = 2000
	MinMinPairs   = 16
)

// Primary is the metric the verdict is about.
type Primary struct {
	Name  string `json:"name"`
	Class string `json:"class"`
	// Margin is the non-inferiority margin m (> 0); 0 in superiority mode, where it is ignored.
	Margin         float64 `json:"margin"`
	HigherIsBetter bool    `json:"higher_is_better"`
	// Range is [lo, hi], the closed score range of a custom primary class; required for it and
	// absent for the others, whose range the class fixes.
	Range []float64 `json:"range,omitempty"`
}

// scoreBounds is the closed range a pass or task_fail score of this primary must lie in. A class
// without a valid range reports bounded false.
func (p Primary) scoreBounds() (lo, hi float64, bounded bool) {
	switch p.Class {
	case ClassUnitInterval:
		return 0, 1, true
	case ClassJudge010:
		return 0, 10, true
	case ClassCustom:
		if len(p.Range) == 2 {
			return p.Range[0], p.Range[1], true
		}
	}
	return 0, 0, false
}

// worst is the bad end of the primary's score range given its direction: the score an attempt
// that did not produce one, or that cheated, enters a task's mean at.
func (p Primary) worst() float64 {
	lo, hi, _ := p.scoreBounds()
	if p.HigherIsBetter {
		return lo
	}
	return hi
}

// EfficiencyMetric is a cost a non-inferior candidate must improve to be adopted.
type EfficiencyMetric struct {
	Name          string `json:"name"`
	Class         string `json:"class"`
	LowerIsBetter bool   `json:"lower_is_better"`
}

// Baseline is the frozen pre-registration. Field order is the canonical order.
type Baseline struct {
	GateVersion      string             `json:"gate_version"`
	Mode             string             `json:"mode"`
	Primary          Primary            `json:"primary"`
	Judge            Judge              `json:"judge"`
	Efficiency       []EfficiencyMetric `json:"efficiency"`
	MinPairs         int                `json:"min_pairs"`
	BootstrapB       int                `json:"bootstrap_b"`
	Seed             int64              `json:"seed"`
	MaxExcludedShare float64            `json:"max_excluded_share"`
	HeldOut          []string           `json:"held_out"`
	CorpusHash       string             `json:"corpus_hash"`
	// Tasks is the declared task list: the denominator of the excluded share.
	Tasks []string `json:"tasks"`
	// Incumbent is the frozen incumbent run (a baseline measurement of the current cascade freezes it here); optional.
	Incumbent *Arm `json:"incumbent,omitempty"`
}

// PreReg is what the operator writes before freezing. Optional fields are pointers (or zero)
// so that "left out" and "explicitly zero" differ: a left-out field takes its default, an
// explicit unsupported value is refused.
type PreReg struct {
	Mode             string             `json:"mode"`
	Primary          PreRegPrimary      `json:"primary"`
	Judge            Judge              `json:"judge"`
	Efficiency       []PreRegEfficiency `json:"efficiency"`
	MinPairs         int                `json:"min_pairs"`
	BootstrapB       int                `json:"bootstrap_b"`
	Seed             *int64             `json:"seed"`
	MaxExcludedShare *float64           `json:"max_excluded_share"`
	HeldOut          []string           `json:"held_out"`
	Tasks            []string           `json:"tasks"`
}

// PreRegPrimary: Margin left out takes the class default; HigherIsBetter left out is true (the
// only supported value for now).
type PreRegPrimary struct {
	Name           string    `json:"name"`
	Class          string    `json:"class"`
	Margin         *float64  `json:"margin"`
	HigherIsBetter *bool     `json:"higher_is_better"`
	Range          []float64 `json:"range,omitempty"`
}

// PreRegEfficiency: LowerIsBetter left out is true.
type PreRegEfficiency struct {
	Name          string `json:"name"`
	Class         string `json:"class"`
	LowerIsBetter *bool  `json:"lower_is_better"`
}

// canonicalBaseline is the file format: indented JSON, LF only, one trailing newline.
func canonicalBaseline(b Baseline) ([]byte, error) {
	normalizeBaseline(&b)
	j, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(j, '\n'), nil
}

// BaselineHash is the hex SHA-256 of the baseline file's bytes.
func BaselineHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// normalizeBaseline makes absent lists empty ones, so freeze never writes `null` and check
// never accepts a file that says it.
func normalizeBaseline(b *Baseline) {
	if b.Efficiency == nil {
		b.Efficiency = []EfficiencyMetric{}
	}
	if b.HeldOut == nil {
		b.HeldOut = []string{}
	}
	if b.Tasks == nil {
		b.Tasks = []string{}
	}
	if b.Incumbent != nil {
		a := cloneArm(*b.Incumbent)
		b.Incumbent = &a
	}
}

// FreezeBaseline validates a pre-registration, fills its defaults, sorts the task lists and the
// incumbent's tasks, and returns the baseline, its canonical bytes and their hash. Freezing the
// same inputs twice returns identical bytes. The corpus is hashed, never stored: a corpus is
// harvested redacted, hash-stamped, machine-local and never committed, and freeze and check
// reproduce from the hash.
func FreezeBaseline(p PreReg, corpus []byte, incumbent *Arm) (Baseline, []byte, string, error) {
	if len(corpus) == 0 {
		return Baseline{}, nil, "", refusef("freeze refused: the corpus is empty (the pre-registration must name the corpus it measures)")
	}
	b := Baseline{
		GateVersion:      GateVersion,
		Mode:             p.Mode,
		Judge:            p.Judge,
		MinPairs:         p.MinPairs,
		BootstrapB:       p.BootstrapB,
		Seed:             DefaultSeed,
		MaxExcludedShare: DefaultMaxExcludedShare,
		CorpusHash:       BaselineHash(corpus),
		Tasks:            append([]string{}, p.Tasks...),
		HeldOut:          append([]string{}, p.HeldOut...),
	}
	if b.MinPairs == 0 {
		b.MinPairs = DefaultMinPairs
	}
	if b.BootstrapB == 0 {
		b.BootstrapB = DefaultBootstrapB
	}
	if p.Seed != nil {
		b.Seed = *p.Seed
	}
	if p.MaxExcludedShare != nil {
		b.MaxExcludedShare = *p.MaxExcludedShare
	}
	sort.Strings(b.Tasks)
	sort.Strings(b.HeldOut)

	b.Primary = Primary{Name: p.Primary.Name, Class: p.Primary.Class, HigherIsBetter: true}
	if len(p.Primary.Range) > 0 {
		b.Primary.Range = append([]float64{}, p.Primary.Range...)
	}
	if p.Primary.HigherIsBetter != nil {
		b.Primary.HigherIsBetter = *p.Primary.HigherIsBetter
	}
	switch p.Mode {
	case ModeSuperiority:
		b.Primary.Margin = 0 // ignored in superiority mode, recorded as 0
	case ModeNonInferiority:
		switch {
		case p.Primary.Margin != nil:
			b.Primary.Margin = *p.Primary.Margin
		case p.Primary.Class == ClassUnitInterval:
			b.Primary.Margin = DefaultMarginUnitInterval
		case p.Primary.Class == ClassJudge010:
			b.Primary.Margin = DefaultMarginJudge010
		case p.Primary.Class == ClassCustom:
			return Baseline{}, nil, "", refusef("freeze refused: primary class %q must carry its own margin (non_inferiority has no default for it)", ClassCustom)
		}
	}
	b.Efficiency = make([]EfficiencyMetric, 0, len(p.Efficiency))
	for _, e := range p.Efficiency {
		m := EfficiencyMetric{Name: e.Name, Class: e.Class, LowerIsBetter: true}
		if e.LowerIsBetter != nil {
			m.LowerIsBetter = *e.LowerIsBetter
		}
		b.Efficiency = append(b.Efficiency, m)
	}
	if incumbent != nil {
		a := sortedArm(*incumbent)
		b.Incumbent = &a
	}
	if err := b.validate(); err != nil {
		return Baseline{}, nil, "", err
	}
	raw, err := canonicalBaseline(b)
	if err != nil {
		return Baseline{}, nil, "", err
	}
	normalizeBaseline(&b)
	return b, raw, BaselineHash(raw), nil
}

// versionRefusal is the refusal for a baseline of another gate version. Its fields and its canonical
// bytes are those of another gate, so the form of the file says nothing useful about it, and the
// only remedy is to write the pre-registration again.
func versionRefusal(got string) error {
	return refusef("baseline refused: gate_version %q, this gate implements %q: a baseline frozen under another version (a version 1 file, for one) must be re-frozen under the current version, because its rules and its canonical bytes are those of another gate", got, GateVersion)
}

// CheckBaseline parses a baseline file, re-marshals it canonically and refuses when the bytes
// differ from what it was given: the file is exactly what freeze wrote, or it is not a
// baseline. It also re-validates every rule freeze applies, so a hand-built file that happens
// to be canonical is still refused. It returns the baseline and the hash of the raw bytes.
//
// The version is read first, before the document is decoded strictly or compared with its
// canonical form: a baseline of another version (one frozen by the version 1 gate has no judge key,
// so its bytes are not this gate's canonical form) is refused for its version, which is the thing
// the operator can act on.
func CheckBaseline(raw []byte) (Baseline, string, error) {
	var probe struct {
		GateVersion *string `json:"gate_version"`
	}
	if json.NewDecoder(bytes.NewReader(raw)).Decode(&probe) == nil && probe.GateVersion != nil && *probe.GateVersion != GateVersion {
		return Baseline{}, "", versionRefusal(*probe.GateVersion)
	}
	var b Baseline
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Baseline{}, "", refusef("baseline refused: not a baseline document: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Baseline{}, "", refusef("baseline refused: trailing data after the baseline document")
	}
	canon, err := canonicalBaseline(b)
	if err != nil {
		return Baseline{}, "", refusef("baseline refused: %v", err)
	}
	if !bytes.Equal(canon, raw) {
		return Baseline{}, "", refusef("baseline is not in canonical form: edited by hand? (re-freeze to write a new pre-registration; a changed baseline has a new hash)")
	}
	normalizeBaseline(&b)
	if err := b.validate(); err != nil {
		return Baseline{}, "", err
	}
	return b, BaselineHash(raw), nil
}

// validate refuses a baseline the gate cannot honour. Freeze runs it before writing and check
// runs it after parsing, so there is one set of rules.
func (b *Baseline) validate() error {
	if b.GateVersion != GateVersion {
		return versionRefusal(b.GateVersion)
	}
	switch b.Mode {
	case ModeSuperiority, ModeNonInferiority:
	default:
		return refusef("baseline refused: mode %q is not %q or %q", b.Mode, ModeSuperiority, ModeNonInferiority)
	}
	if b.Primary.Name == "" {
		return refusef("baseline refused: primary.name is empty")
	}
	if t := timeTerm(b.Primary.Name); t != "" {
		return refusef("baseline refused: primary name %q carries the wall-time term %q (INV-5: no pass rule, verdict or ranking carries a wall-time term)", b.Primary.Name, t)
	}
	switch b.Primary.Class {
	case ClassUnitInterval, ClassJudge010, ClassCustom:
	default:
		return refusef("baseline refused: primary class %q is not %q, %q or %q", b.Primary.Class, ClassUnitInterval, ClassJudge010, ClassCustom)
	}
	if !b.Primary.HigherIsBetter {
		return refusef("baseline refused: primary.higher_is_better must be true (lower-is-better primary scores are not supported yet)")
	}
	if b.Primary.Class == ClassCustom {
		r := b.Primary.Range
		if len(r) != 2 || !finite(r[0]) || !finite(r[1]) || !(r[1] > r[0]) || !finite(r[1]-r[0]) {
			return refusef("baseline refused: primary class %q needs a range [lo, hi] of two finite numbers with hi above lo and a finite width (the worst score of an unfinished attempt is its bad end)", ClassCustom)
		}
	} else if len(b.Primary.Range) > 0 {
		return refusef("baseline refused: primary.range is only for class %q; %q has a fixed range", ClassCustom, b.Primary.Class)
	}
	switch b.Judge.Kind {
	case "deterministic", "model":
	default:
		return refusef("baseline refused: judge kind %q is not \"deterministic\" or \"model\" (the judge is part of the pre-registration)", b.Judge.Kind)
	}
	if b.Judge.Identity == "" {
		return refusef("baseline refused: judge identity is empty (the pre-registration must name the scorer)")
	}
	switch b.Mode {
	case ModeSuperiority:
		if b.Primary.Margin != 0 {
			return refusef("baseline refused: superiority ignores the margin, so it is recorded as 0, not %g", b.Primary.Margin)
		}
	case ModeNonInferiority:
		if !finite(b.Primary.Margin) || b.Primary.Margin <= 0 {
			return refusef("baseline refused: non_inferiority needs a margin above 0, got %g", b.Primary.Margin)
		}
		lo, hi, _ := b.Primary.scoreBounds()
		if width := hi - lo; b.Primary.Margin >= width {
			return refusef("baseline refused: margin %g is not below the width %g of the %s score range: a margin that wide lets a candidate that fails every task through", b.Primary.Margin, width, b.Primary.Class)
		}
		if len(b.Efficiency) == 0 {
			return refusef("freeze refused: non_inferiority with no efficiency metric (a non-inferiority cut must name what it saves)")
		}
	}
	seenEff := map[string]struct{}{}
	for _, e := range b.Efficiency {
		switch e.Class {
		case EffTokens, EffMoney, EffMemoryGiB:
		default:
			return refusef("freeze refused: efficiency class %q is not allowed: only %q, %q and %q are (INV-5: no pass rule, verdict or ranking carries a wall-time term; seconds, latency, wall, duration and time are never an efficiency class)", e.Class, EffTokens, EffMoney, EffMemoryGiB)
		}
		if e.Name == "" {
			return refusef("baseline refused: an efficiency metric has an empty name")
		}
		if t := timeTerm(e.Name); t != "" {
			return refusef("freeze refused: efficiency metric %q carries the wall-time term %q (INV-5: no pass rule, verdict or ranking carries a wall-time term)", e.Name, t)
		}
		if _, dup := seenEff[e.Name]; dup {
			return refusef("baseline refused: efficiency metric %q is declared twice", e.Name)
		}
		seenEff[e.Name] = struct{}{}
	}
	if b.MinPairs < MinMinPairs {
		return refusef("baseline refused: min_pairs %d is below the floor %d (the primary interval's false-ADOPT rate was measured from %d tasks up and nothing below that is claimed)", b.MinPairs, MinMinPairs, MinMinPairs)
	}
	if b.BootstrapB < MinBootstrapB {
		return refusef("baseline refused: bootstrap_b %d is below the floor %d (the efficiency intervals are read from the resamples, and a few of them are mostly noise: on null cost data a metric falsely improved in 8%% to 13%% of runs at B = 10 and in 42%% to 50%% at B = 1)", b.BootstrapB, MinBootstrapB)
	}
	if !finite(b.MaxExcludedShare) || b.MaxExcludedShare < 0 || b.MaxExcludedShare > 1 {
		return refusef("baseline refused: max_excluded_share %g is outside [0,1]", b.MaxExcludedShare)
	}
	if len(b.CorpusHash) != 64 || !isLowerHex(b.CorpusHash) {
		return refusef("baseline refused: corpus_hash is not a 64-character lower-case hex SHA-256")
	}
	if len(b.Tasks) == 0 {
		return refusef("baseline refused: tasks is empty (the declared task list is the denominator of the excluded share)")
	}
	declared := make(map[string]struct{}, len(b.Tasks))
	for i, id := range b.Tasks {
		if id == "" {
			return refusef("baseline refused: tasks holds an empty id")
		}
		if i > 0 && id <= b.Tasks[i-1] {
			return refusef("baseline refused: tasks must be sorted and unique (%q after %q): not the form freeze writes", id, b.Tasks[i-1])
		}
		declared[id] = struct{}{}
	}
	for i, id := range b.HeldOut {
		if _, ok := declared[id]; !ok {
			return refusef("baseline refused: held_out id %q is not in tasks", id)
		}
		if i > 0 && id <= b.HeldOut[i-1] {
			return refusef("baseline refused: held_out must be sorted and unique (%q after %q): not the form freeze writes", id, b.HeldOut[i-1])
		}
	}
	if len(b.HeldOut) >= len(b.Tasks) {
		return refusef("baseline refused: every declared task is held out, none is left for the main comparison")
	}
	if left := len(b.Tasks) - len(b.HeldOut); b.MinPairs > left {
		return refusef("baseline refused: min_pairs %d is above the %d tasks left once the held-out ones are set aside, so no run could reach it", b.MinPairs, left)
	}
	if b.Incumbent != nil {
		if !stampsComplete(b.Incumbent.Stamps) {
			return refusef("baseline refused: the frozen incumbent's stamps are incomplete (harness_build_sha256, seat_config_sha256 and engine_build are all required)")
		}
		for i := 1; i < len(b.Incumbent.Tasks); i++ {
			if b.Incumbent.Tasks[i].ID <= b.Incumbent.Tasks[i-1].ID {
				return refusef("baseline refused: the incumbent's tasks must be sorted by id and unique (%q after %q): not the form freeze writes", b.Incumbent.Tasks[i].ID, b.Incumbent.Tasks[i-1].ID)
			}
		}
		if err := validateArm("incumbent", *b.Incumbent, declared, b.Primary); err != nil {
			return err
		}
	}
	return nil
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// nonHeldOut returns the declared tasks that are not held out, in sorted order.
func (b *Baseline) nonHeldOut() []string {
	held := make(map[string]struct{}, len(b.HeldOut))
	for _, id := range b.HeldOut {
		held[id] = struct{}{}
	}
	out := make([]string, 0, len(b.Tasks))
	for _, id := range b.Tasks {
		if _, h := held[id]; !h {
			out = append(out, id)
		}
	}
	return out
}
