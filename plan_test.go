package main

import (
	"testing"
)

const (
	kiB = int64(1) << 10
	miB = int64(1) << 20
	giB = int64(1) << 30
)

func testSeed(b byte) [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = b + byte(i)
	}
	return s
}

func offsetsOf(samples []Sample) []int64 {
	out := make([]int64, len(samples))
	for i, s := range samples {
		out[i] = s.Offset
	}
	return out
}

func TestBuildPlanShape(t *testing.T) {
	const (
		claimed    = 64 * giB
		start      = miB
		sampleSize = 4 * kiB
		blockSize  = 512
	)
	plan, err := buildPlan(start, claimed, sampleSize, blockSize, 576, rngFromSeed(testSeed(1), "plan"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) < 400 || len(plan) > 700 {
		t.Errorf("got %d samples, want roughly 576", len(plan))
	}
	for i, s := range plan {
		if s.Index != i {
			t.Errorf("sample %d has index %d", i, s.Index)
		}
		if s.Offset%sampleSize != 0 {
			t.Errorf("sample %d at %d is not sample-size aligned", i, s.Offset)
		}
		if s.Offset < start || s.Offset+sampleSize > claimed {
			t.Errorf("sample %d at %d is outside [%d,%d)", i, s.Offset, start, claimed)
		}
		if i > 0 && s.Offset-plan[i-1].Offset < sampleSize {
			t.Errorf("samples %d and %d overlap", i-1, i)
		}
	}
	if last := plan[len(plan)-1].Offset; last+sampleSize > claimed || claimed-last > 2*sampleSize {
		t.Errorf("last sample at %d does not cover the end of the device", last)
	}
}

// The whole point of the lattice: probes must collide on the same physical
// block when a device folds addresses, otherwise a wrapping counterfeit reads
// back its own patterns and passes.
func TestBuildPlanCollidesUnderWrap(t *testing.T) {
	const (
		claimed    = 64 * giB
		sampleSize = 4 * kiB
	)
	plan, err := buildPlan(miB, claimed, sampleSize, 512, 576, rngFromSeed(testSeed(2), "plan"))
	if err != nil {
		t.Fatal(err)
	}
	for _, real := range []int64{2 * giB, 4 * giB, 8 * giB, 16 * giB, 32 * giB} {
		seen := map[int64]bool{}
		collisions := 0
		for _, s := range plan {
			eff := s.Offset % real
			if seen[eff] {
				collisions++
			}
			seen[eff] = true
		}
		if collisions == 0 {
			t.Errorf("no probe collisions for a device that really holds %s: a wrap would go unnoticed", humanBytes(real))
		}
	}
}

func TestBuildPlanDeterministicPerSeed(t *testing.T) {
	a, err := buildPlan(miB, 32*giB, 4*kiB, 512, 300, rngFromSeed(testSeed(3), "plan"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildPlan(miB, 32*giB, 4*kiB, 512, 300, rngFromSeed(testSeed(3), "plan"))
	c, _ := buildPlan(miB, 32*giB, 4*kiB, 512, 300, rngFromSeed(testSeed(4), "plan"))
	if len(a) != len(b) || offsetsOf(a)[0] != offsetsOf(b)[0] {
		t.Error("same seed produced a different plan")
	}
	if offsetsOf(a)[0] == offsetsOf(c)[0] && len(a) > 1 {
		t.Error("different seeds produced the same phase; the lattice is not being slid")
	}
}

func TestBuildPlanTinyAndInvalid(t *testing.T) {
	if _, err := buildPlan(0, 2*kiB, 4*kiB, 512, 10, rngFromSeed(testSeed(5), "plan")); err == nil {
		t.Error("expected an error for a device smaller than one sample")
	}
	if _, err := buildPlan(0, giB, 4097, 512, 10, rngFromSeed(testSeed(5), "plan")); err == nil {
		t.Error("expected an error for a sample size that is not a multiple of the block size")
	}
	plan, err := buildPlan(0, 64*kiB, 4*kiB, 512, 1000, rngFromSeed(testSeed(5), "plan"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) > 16 {
		t.Errorf("got %d samples in a 64 kiB span, want at most 16", len(plan))
	}
}

func TestChooseStrideIsPowerOfTwo(t *testing.T) {
	for _, span := range []int64{giB, 7 * giB, 64 * giB, 977 * giB} {
		got := chooseStride(span, 4*kiB, 576)
		if !isPow2(got) {
			t.Errorf("stride for a %s span is %d, which is not a power of two", humanBytes(span), got)
		}
		if n := span/got + 1; n < 100 || n > 1200 {
			t.Errorf("stride for a %s span yields %d samples", humanBytes(span), n)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"512": 512, "4k": 4096, "4K": 4096, "1M": miB, "1MiB": miB, "2G": 2 * giB, "0": 0}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "banana", "-5", "4Q"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) should have failed", in)
		}
	}
}
