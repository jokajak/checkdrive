package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
)

// Sample is one location on the device that a run reads, overwrites, reads
// back and restores.
type Sample struct {
	Index  int
	Offset int64
}

// buildPlan lays the samples out on a power-of-two lattice: a stride that is a
// power of two, a single random phase for the whole run, and one extra sample
// pinned to the last addressable block.
//
// The lattice is the part that catches the most common counterfeit: a drive
// that reports, say, 64 GB while holding 8 GB of flash and quietly folding
// every address back with a modulo. Independently random probe locations do
// *not* catch that. Each probe would write its pattern to some physical block
// and read the same pattern straight back from it - correct data, wrong place,
// and no way to tell. The lie only shows up when two probes land on the same
// physical block, because then the second write destroys the first, and the
// first probe reads back a pattern stamped with somebody else's address.
//
// With every offset at start+phase+i*stride, two probes collide exactly when
// their index distance is a multiple of realCapacity/stride - which is
// guaranteed for any real capacity that is a multiple of the stride, and
// counterfeit capacities are essentially always round powers of two. The
// per-run phase and the per-run pattern seed keep the actual addresses and
// contents unpredictable, so a device cannot memorise last run's probes.
//
// A wrap whose modulus is not a multiple of the stride still slips through the
// lattice; detectWrap covers that case separately.
func buildPlan(start, end, sampleSize, blockSize int64, count int, rng *rand.Rand) ([]Sample, error) {
	switch {
	case blockSize <= 0:
		return nil, fmt.Errorf("block size must be positive, got %d", blockSize)
	case sampleSize <= 0 || sampleSize%blockSize != 0:
		return nil, fmt.Errorf("sample size %d must be a positive multiple of the %d byte block size", sampleSize, blockSize)
	case count <= 0:
		return nil, fmt.Errorf("sample count must be positive, got %d", count)
	}

	start = alignUp(start, sampleSize)
	if end-sampleSize < start {
		return nil, fmt.Errorf("device span %s is too small for even one %s sample",
			humanBytes(end-start), humanBytes(sampleSize))
	}
	last := alignDown(end-sampleSize, sampleSize)
	if last < start {
		return nil, fmt.Errorf("device span %s is too small for even one %s sample",
			humanBytes(end-start), humanBytes(sampleSize))
	}

	span := last - start
	stride := chooseStride(span, sampleSize, count)
	n := int(span/stride) + 1

	// Slide the whole lattice by a random, still aligned, amount so the run
	// does not probe the same addresses as the last one.
	phase := int64(0)
	if maxPhase := span - int64(n-1)*stride; maxPhase >= sampleSize {
		phase = alignDown(rng.Int64N(maxPhase+1), sampleSize)
	}

	samples := make([]Sample, 0, n+1)
	for i := range n {
		samples = append(samples, Sample{Offset: start + phase + int64(i)*stride})
	}

	// Counterfeit media nearly always breaks at the top of its claimed
	// capacity, so the very last block is never left to chance.
	tail := samples[len(samples)-1].Offset
	switch {
	case last-tail >= sampleSize:
		samples = append(samples, Sample{Offset: last})
	case last > tail:
		samples[len(samples)-1].Offset = last
	}

	for i := range samples {
		samples[i].Index = i
	}
	return samples, nil
}

// chooseStride picks the power-of-two stride that lands closest to the
// requested sample count. A power of two is what makes probes collide under a
// modulo-wrapping fake, so it is preferred over hitting the count exactly; if
// the sample size is not itself a power of two the lattice property is
// impossible and the stride simply divides the span evenly.
func chooseStride(span, sampleSize int64, count int) int64 {
	if span <= 0 || count <= 1 {
		return sampleSize
	}
	target := span / int64(count)
	if !isPow2(sampleSize) {
		if stride := alignUp(target, sampleSize); stride > sampleSize {
			return stride
		}
		return sampleSize
	}

	lo := sampleSize
	for lo <= target/2 {
		lo *= 2
	}
	hi := lo * 2
	distance := func(stride int64) int {
		d := int(span/stride) + 1 - count
		if d < 0 {
			return -d
		}
		return d
	}
	if distance(hi) < distance(lo) {
		return hi
	}
	return lo
}

func isPow2(v int64) bool { return v > 0 && v&(v-1) == 0 }

func alignUp(v, to int64) int64 {
	if r := v % to; r != 0 {
		return v + (to - r)
	}
	return v
}

func alignDown(v, to int64) int64 {
	return v - v%to
}

// rngFromSeed derives an independent deterministic stream from the run seed,
// so that -seed reproduces both the sample placement and the verify order.
func rngFromSeed(seed [32]byte, purpose string) *rand.Rand {
	derived := sha256.Sum256(append(seed[:], purpose...))
	return rand.New(rand.NewChaCha8(derived))
}
