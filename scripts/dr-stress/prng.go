package main

// Xorshift64Star is a fast, non-cryptographic PRNG suitable for workload
// distribution.  Each worker gets its own instance seeded deterministically
// from --seed + workerID so runs are reproducible.
type Xorshift64Star struct {
	state uint64
}

// NewXorshift64Star creates a new PRNG.  seed must be non-zero.
func NewXorshift64Star(seed uint64) *Xorshift64Star {
	if seed == 0 {
		seed = 1 // xorshift state must never be zero
	}
	return &Xorshift64Star{state: seed}
}

// Next returns the next pseudo-random uint64.
func (x *Xorshift64Star) Next() uint64 {
	x.state ^= x.state >> 12
	x.state ^= x.state << 25
	x.state ^= x.state >> 27
	return x.state * 0x2545F4914F6CDD1D
}

// Intn returns a pseudo-random int in [0, n).  Panics if n <= 0.
func (x *Xorshift64Star) Intn(n int) int {
	if n <= 0 {
		panic("Xorshift64Star.Intn: n must be > 0")
	}
	return int(x.Next() % uint64(n))
}

// WorkerSeed derives a deterministic per-worker seed from a base seed and
// worker ID using a simple mixing function (splitmix64 finalizer).
func WorkerSeed(baseSeed uint64, workerID int) uint64 {
	z := baseSeed + uint64(workerID)*0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z = z ^ (z >> 31)
	if z == 0 {
		z = 1
	}
	return z
}
