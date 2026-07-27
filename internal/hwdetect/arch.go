package hwdetect

import "regexp"

// ArchFromName maps a GPU product name to the architecture the tier bands key on.
// A straight port of detect.ps1's Get-Arch, rule order included — the order IS the
// logic (an "RTX PRO 5000 Blackwell" must not fall through to the RTX-50xx rule it
// does not match, and a future non-Blackwell "RTX PRO" generation needs a new rule
// ABOVE that one).
func ArchFromName(name string) string {
	if name == "" {
		return "none"
	}
	for _, r := range archRules {
		if r.re.MatchString(name) {
			return r.arch
		}
	}
	return "other"
}

var archRules = []struct {
	re   *regexp.Regexp
	arch string
}{
	// NVIDIA consumer generations — match the 4-digit model so "RTX 3070" -> ampere.
	{regexp.MustCompile(`(?i)RTX\s*50\d{2}`), "blackwell"},
	{regexp.MustCompile(`(?i)RTX\s*50\b`), "blackwell"},
	// RTX PRO Blackwell workstation cards: "RTX PRO" breaks the RTX-50xx match, so the
	// explicit Blackwell suffix comes first, then the "RTX PRO NNNN" branding — which
	// IS the Blackwell pro generation (pre-Blackwell pro cards were "RTX A6000" /
	// "RTX 6000 Ada Generation" and do not match).
	{regexp.MustCompile(`(?i)\bBlackwell\b`), "blackwell"},
	{regexp.MustCompile(`(?i)RTX\s+PRO\s+\d{4}`), "blackwell"},
	{regexp.MustCompile(`(?i)RTX\s*40\d{2}`), "ada"},
	{regexp.MustCompile(`(?i)RTX\s*40\b`), "ada"},
	{regexp.MustCompile(`(?i)RTX\s*30\d{2}`), "ampere"},
	{regexp.MustCompile(`(?i)RTX\s*30\b`), "ampere"},
	// Tesla / data-center Volta.
	{regexp.MustCompile(`(?i)\bV100\b`), "volta"},
	// AMD RDNA3: 780M/760M iGPU (Phoenix) and 7000-series discrete.
	{regexp.MustCompile(`(?i)\b7\d{2}M\b`), "rdna3"},
	{regexp.MustCompile(`(?i)RX\s*7\d{3}`), "rdna3"},
	{regexp.MustCompile(`(?i)RDNA\s*3`), "rdna3"},
	// AMD GCN / Vega (older iGPU + discrete). "Vega 7" is a Ryzen APU iGPU.
	{regexp.MustCompile(`(?i)\bVega\b`), "gcn"},
	{regexp.MustCompile(`(?i)\bGCN\b`), "gcn"},
	// A recognised vendor whose generation we do not classify.
	{regexp.MustCompile(`(?i)NVIDIA|GeForce|Quadro|Tesla|AMD|Radeon`), "other"},
}

// VendorFromName reports the GPU vendor a product name belongs to. "none" when the
// name is empty or unrecognised — never a guess, since the vendor selects the whole
// serving backend.
func VendorFromName(name string) string {
	switch {
	case name == "":
		return "none"
	case regexp.MustCompile(`(?i)NVIDIA|GeForce|Quadro|Tesla`).MatchString(name):
		return "nvidia"
	case regexp.MustCompile(`(?i)\bAMD\b|Radeon`).MatchString(name):
		return "amd"
	}
	return "none"
}
