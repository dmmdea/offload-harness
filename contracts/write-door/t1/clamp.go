package clamp

// Clamp returns v limited to the closed range [lo, hi].
// Values below lo return lo; values above hi return hi.
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return lo
	}
	return v
}
