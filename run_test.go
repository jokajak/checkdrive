package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func scanConfig(claimed int64, seedByte byte) runConfig {
	return runConfig{
		Samples:    256,
		SampleSize: 4 * kiB,
		BlockSize:  512,
		Start:      miB,
		End:        claimed,
		Seed:       testSeed(seedByte),
		Estimate:   true,
	}
}

func sampleOffsets(res *runResult) []int64 {
	out := make([]int64, len(res.Samples))
	for i, s := range res.Samples {
		out[i] = s.Offset
	}
	return out
}

func TestScanHonestDevicePasses(t *testing.T) {
	dev := newFake(8*giB, 8*giB, fakeHonest)
	cfg := scanConfig(8*giB, 10)

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failures != 0 {
		t.Fatalf("honest device reported %d failures: %v", res.Failures, res.ByVerdict)
	}
	if res.WrapModulus != 0 {
		t.Errorf("honest device flagged as wrapping at %d", res.WrapModulus)
	}
	if _, pass := overallVerdict(res, cfg.End); !pass {
		t.Error("honest device did not pass")
	}
	if res.EstimatedCapacity != cfg.End {
		t.Errorf("estimated capacity %d, want %d", res.EstimatedCapacity, cfg.End)
	}
	if err := dev.intact(sampleOffsets(res), cfg.SampleSize); err != nil {
		t.Errorf("original data was not put back: %v", err)
	}
	if dev.reopens == 0 {
		t.Error("device was never reopened, so reads could have come from cache")
	}
}

// The headline case: a stick that says 64 GB and holds 8 GB, folding every
// address back on itself.
func TestScanWrapAroundCounterfeit(t *testing.T) {
	const real = 8 * giB
	dev := newFake(64*giB, real, fakeWrap)
	cfg := scanConfig(64*giB, 11)

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failures == 0 {
		t.Fatal("a wrap-around counterfeit passed the scan")
	}
	if res.ByVerdict[string(verdictAliased)] == 0 {
		t.Errorf("expected aliased locations, got %v", res.ByVerdict)
	}
	if res.WrapModulus != real {
		t.Errorf("wrap modulus %s, want %s", humanBytes(res.WrapModulus), humanBytes(real))
	}
	if res.EstimatedCapacity != real {
		t.Errorf("estimated capacity %s, want %s", humanBytes(res.EstimatedCapacity), humanBytes(real))
	}
	headline, pass := overallVerdict(res, cfg.End)
	if pass || !strings.Contains(headline, "folds addresses") {
		t.Errorf("unexpected verdict: %q", headline)
	}
}

// A wrap whose modulus is not a multiple of the probe stride: the lattice
// cannot catch it, so this exercises the dedicated wrap sweep.
func TestScanWrapAtOddModulus(t *testing.T) {
	const real = 7 * 1_000_000_000
	dev := newFake(64*giB, real, fakeWrap)

	res, err := runScan(dev, scanConfig(64*giB, 12))
	if err != nil {
		t.Fatal(err)
	}
	if res.WrapModulus != real {
		t.Fatalf("wrap modulus %s, want %s", humanBytes(res.WrapModulus), humanBytes(real))
	}
	if _, pass := overallVerdict(res, 64*giB); pass {
		t.Error("a wrapping device passed")
	}
}

func TestScanDiscardedWritesEstimateCapacity(t *testing.T) {
	const real = 4 * giB
	dev := newFake(32*giB, real, fakeDiscard)
	cfg := scanConfig(32*giB, 13)

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ByVerdict[string(verdictUnchanged)] == 0 {
		t.Errorf("expected discarded writes to show as unchanged, got %v", res.ByVerdict)
	}
	if delta := res.EstimatedCapacity - real; delta > cfg.SampleSize || delta < -cfg.SampleSize {
		t.Errorf("estimated capacity %s, want %s (within one sample)",
			humanBytes(res.EstimatedCapacity), humanBytes(real))
	}
	if res.FirstFailure < real {
		t.Errorf("first failure reported at %s, below the real capacity %s",
			humanBytes(res.FirstFailure), humanBytes(real))
	}
}

func TestScanZeroReadbackEstimateCapacity(t *testing.T) {
	const real = 2 * giB
	dev := newFake(16*giB, real, fakeZeros)
	cfg := scanConfig(16*giB, 14)

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ByVerdict[string(verdictZeros)] == 0 {
		t.Errorf("expected zero read-backs, got %v", res.ByVerdict)
	}
	if delta := res.EstimatedCapacity - real; delta > cfg.SampleSize || delta < -cfg.SampleSize {
		t.Errorf("estimated capacity %s, want %s", humanBytes(res.EstimatedCapacity), humanBytes(real))
	}
	// Everything below the real capacity must have been put back.
	var below []int64
	for _, off := range sampleOffsets(res) {
		if off+cfg.SampleSize <= real {
			below = append(below, off)
		}
	}
	if err := dev.intact(below, cfg.SampleSize); err != nil {
		t.Errorf("original data was not put back: %v", err)
	}
}

func TestScanWriteErrorsAreReported(t *testing.T) {
	dev := newFake(16*giB, 2*giB, fakeWriteError)
	res, err := runScan(dev, scanConfig(16*giB, 15))
	if err != nil {
		t.Fatal(err)
	}
	if res.ByVerdict[string(verdictWriteError)] == 0 {
		t.Errorf("expected write errors, got %v", res.ByVerdict)
	}
	if _, pass := overallVerdict(res, 16*giB); pass {
		t.Error("a device that rejects writes passed")
	}
}

func TestScanReadOnlyNeverWrites(t *testing.T) {
	dev := newFake(8*giB, 8*giB, fakeHonest)
	cfg := scanConfig(8*giB, 16)
	cfg.ReadOnly = true

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.blocks) != 0 {
		t.Errorf("read-only run wrote %d blocks", len(dev.blocks))
	}
	if res.Failures != 0 {
		t.Errorf("read-only run reported failures: %v", res.ByVerdict)
	}
	headline, pass := overallVerdict(res, cfg.End)
	if !pass || !strings.Contains(headline, "read-only") {
		t.Errorf("unexpected read-only verdict: %q", headline)
	}
}

// Every block the run is about to overwrite has to reach the journal first,
// so that an interrupted run can still be undone.
func TestScanJournalsOriginalsBeforeWriting(t *testing.T) {
	dev := newFake(8*giB, 8*giB, fakeHonest)
	cfg := scanConfig(8*giB, 17)
	path := filepath.Join(t.TempDir(), "run.journal")
	j, err := newJournal(path, journalHeader{Device: "/dev/disk9", SampleSize: cfg.SampleSize})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Journal = j

	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}

	_, records, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(res.Samples) {
		t.Fatalf("journal holds %d blocks for %d samples", len(records), len(res.Samples))
	}
	want := make([]byte, cfg.SampleSize)
	for _, r := range records {
		initialFill(want, r.Offset)
		if string(r.Data) != string(want) {
			t.Fatalf("journal record for offset %d is not the original data", r.Offset)
		}
	}
}
