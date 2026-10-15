// contextbench_pyrand.go - the subset of CPython's `random.Random` the contextbench
// task generators draw from, reproduced bit for bit: the Mersenne Twister seeded the
// way `random.seed(<int>)` seeds it, and the algorithms behind `random()`,
// `getrandbits`, `choice`, `randint`, `shuffle`, `sample` and `choices`.
//
// Why: a contextbench instance is a pure function of (seed, knobs) in its Python
// original. Reproducing the generator's random stream exactly means seed 42 here is
// the same instance as seed 42 there, which is what lets a result be set beside the
// published levels. contextbench_gen_test.go pins the byte digest of the streamed
// operations against output of the original generator.
package compeval

import "math"

const (
	mtN         = 624
	mtM         = 397
	mtMatrixA   = 0x9908b0df
	mtUpperMask = 0x80000000
	mtLowerMask = 0x7fffffff
)

// pyRand is a CPython-compatible random source.
type pyRand struct {
	mt  [mtN]uint32
	idx int
}

// newPyRand seeds like `random.Random(seed)` for a non-negative int seed.
func newPyRand(seed uint64) *pyRand {
	r := &pyRand{}
	// CPython splits abs(seed) into little-endian 32-bit words (one zero word for 0).
	key := []uint32{}
	for s := seed; s > 0; s >>= 32 {
		key = append(key, uint32(s&0xffffffff))
	}
	if len(key) == 0 {
		key = []uint32{0}
	}
	r.initByArray(key)
	return r
}

func (r *pyRand) initGenrand(s uint32) {
	r.mt[0] = s
	for i := 1; i < mtN; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	r.idx = mtN
}

func (r *pyRand) initByArray(key []uint32) {
	r.initGenrand(19650218)
	i, j := 1, 0
	k := mtN
	if len(key) > k {
		k = len(key)
	}
	for ; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k = mtN - 1; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
	r.idx = mtN
}

func (r *pyRand) genrand() uint32 {
	if r.idx >= mtN {
		var kk int
		for kk = 0; kk < mtN-mtM; kk++ {
			y := (r.mt[kk] & mtUpperMask) | (r.mt[kk+1] & mtLowerMask)
			r.mt[kk] = r.mt[kk+mtM] ^ (y >> 1) ^ ((y & 1) * mtMatrixA)
		}
		for ; kk < mtN-1; kk++ {
			y := (r.mt[kk] & mtUpperMask) | (r.mt[kk+1] & mtLowerMask)
			r.mt[kk] = r.mt[kk+(mtM-mtN)] ^ (y >> 1) ^ ((y & 1) * mtMatrixA)
		}
		y := (r.mt[mtN-1] & mtUpperMask) | (r.mt[0] & mtLowerMask)
		r.mt[mtN-1] = r.mt[mtM-1] ^ (y >> 1) ^ ((y & 1) * mtMatrixA)
		r.idx = 0
	}
	y := r.mt[r.idx]
	r.idx++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

// random is `random.random()`: a 53-bit float in [0, 1).
func (r *pyRand) random() float64 {
	a := r.genrand() >> 5
	b := r.genrand() >> 6
	return (float64(a)*67108864.0 + float64(b)) * (1.0 / 9007199254740992.0)
}

// getrandbits is `getrandbits(k)` for 0 <= k <= 32.
func (r *pyRand) getrandbits(k int) uint32 {
	if k <= 0 {
		return 0
	}
	return r.genrand() >> uint(32-k)
}

// randbelow is `_randbelow_with_getrandbits(n)`; n must be positive and below 2^32.
func (r *pyRand) randbelow(n int) int {
	if n <= 0 {
		return 0
	}
	k := 0
	for v := n; v > 0; v >>= 1 {
		k++
	}
	v := int(r.getrandbits(k))
	for v >= n {
		v = int(r.getrandbits(k))
	}
	return v
}

// randint is `randint(a, b)`.
func (r *pyRand) randint(a, b int) int { return a + r.randbelow(b+1-a) }

// choiceIndex is the index `choice(seq)` picks from a sequence of length n.
func (r *pyRand) choiceIndex(n int) int { return r.randbelow(n) }

// shuffleInts is `shuffle(x)` on an int slice, in place.
func (r *pyRand) shuffleInts(x []int) {
	for i := len(x) - 1; i >= 1; i-- {
		j := r.randbelow(i + 1)
		x[i], x[j] = x[j], x[i]
	}
}

// shufflePairs is `shuffle(x)` on a slice of [2]int, in place.
func (r *pyRand) shufflePairs(x [][2]int) {
	for i := len(x) - 1; i >= 1; i-- {
		j := r.randbelow(i + 1)
		x[i], x[j] = x[j], x[i]
	}
}

// sampleIndexes is the list of population indexes `sample(population, k)` selects,
// in the order it returns them. It follows CPython's two strategies (a pool for a
// small population, a selection set for a large one).
func (r *pyRand) sampleIndexes(n, k int) []int {
	if k < 0 || k > n {
		return nil
	}
	setsize := 21
	if k > 5 {
		setsize += int(math.Pow(4, math.Ceil(math.Log(float64(k*3))/math.Log(4))))
	}
	result := make([]int, k)
	if n <= setsize {
		pool := make([]int, n)
		for i := range pool {
			pool[i] = i
		}
		for i := 0; i < k; i++ {
			j := r.randbelow(n - i)
			result[i] = pool[j]
			pool[j] = pool[n-i-1]
		}
		return result
	}
	selected := make(map[int]bool, k)
	for i := 0; i < k; i++ {
		j := r.randbelow(n)
		for selected[j] {
			j = r.randbelow(n)
		}
		selected[j] = true
		result[i] = j
	}
	return result
}

// weightedIndex is the index one draw of `choices(population, weights)` picks.
func (r *pyRand) weightedIndex(weights []float64) int {
	cum := make([]float64, len(weights))
	acc := 0.0
	for i, w := range weights {
		acc += w
		cum[i] = acc
	}
	total := cum[len(cum)-1] + 0.0
	x := r.random() * total
	lo, hi := 0, len(cum)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if x < cum[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
