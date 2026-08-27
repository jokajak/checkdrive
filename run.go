package main

import (
	"bytes"
	"fmt"
	"sort"
	"time"
)

// runConfig is everything the scan engine needs. It is deliberately free of
// flags, terminals and devices so the engine can be driven by a fake in tests.
type runConfig struct {
	Samples    int
	SampleSize int64
	BlockSize  int64
	Start      int64
	End        int64
	Seed       [32]byte
	ReadOnly   bool
	Estimate   bool
	Journal    *journal
	Progress   func(phase string, done, total int)
}

type sampleResult struct {
	Index       int     `json:"index"`
	Offset      int64   `json:"offset"`
	Verdict     verdict `json:"verdict"`
	ReadNS      int64   `json:"read_ns"`
	WriteNS     int64   `json:"write_ns"`
	VerifyNS    int64   `json:"verify_ns"`
	AliasOffset int64   `json:"alias_offset,omitempty"`
	Restored    bool    `json:"restored"`
	Err         string  `json:"error,omitempty"`
}

type runResult struct {
	Started           time.Time      `json:"started"`
	Duration          time.Duration  `json:"duration_ns"`
	SampleSize        int64          `json:"sample_size"`
	ReadOnly          bool           `json:"read_only"`
	Samples           []sampleResult `json:"samples"`
	Failures          int            `json:"failures"`
	ByVerdict         map[string]int `json:"by_verdict"`
	FirstFailure      int64          `json:"first_failure_offset"`
	EstimatedCapacity int64          `json:"estimated_capacity"`
	EstimateNote      string         `json:"estimate_note,omitempty"`
	WrapModulus       int64          `json:"wrap_modulus"`
	WrapEcho          int64          `json:"wrap_echo_offset"`
	WrapProbes        int            `json:"wrap_probes"`
	RestoreErrors     []string       `json:"restore_errors,omitempty"`
	Probes            int            `json:"capacity_probes"`
}

const maxProbes = 48

// runScan executes the whole check:
//
//  1. read and journal the original contents of every sample
//  2. write a unique, address-bound pattern to each one
//  3. flush to the media, reopen the device, and read every sample back in a
//     shuffled order so neither the OS nor the drive's own cache can serve a
//     read from RAM
//  4. put the original contents back
//  5. if anything failed, binary-search the boundary to estimate what the
//     device really holds
func runScan(dev blockDevice, cfg runConfig) (*runResult, error) {
	plan, err := buildPlan(cfg.Start, cfg.End, cfg.SampleSize, cfg.BlockSize, cfg.Samples, rngFromSeed(cfg.Seed, "plan"))
	if err != nil {
		return nil, err
	}
	n := len(plan)

	res := &runResult{
		Started:           time.Now(),
		SampleSize:        cfg.SampleSize,
		ReadOnly:          cfg.ReadOnly,
		Samples:           make([]sampleResult, n),
		ByVerdict:         map[string]int{},
		FirstFailure:      -1,
		EstimatedCapacity: -1,
	}
	progress := cfg.Progress
	if progress == nil {
		progress = func(string, int, int) {}
	}

	expected := make([][]byte, n)
	originals := make([][]byte, n)
	written := make([]bool, n)
	byPattern := make(map[[32]byte]int64, n)
	for i, s := range plan {
		res.Samples[i] = sampleResult{Index: i, Offset: s.Offset, AliasOffset: -1}
		buf := make([]byte, cfg.SampleSize)
		fillPattern(buf, cfg.Seed, s.Offset)
		expected[i] = buf
		byPattern[sha256Of(buf)] = s.Offset
	}

	// Phase 1 - read the originals, and get them safely into the journal
	// before a single byte of the device is modified.
	for i, s := range plan {
		progress("read", i, n)
		buf := make([]byte, cfg.SampleSize)
		started := time.Now()
		err := dev.readAt(buf, s.Offset)
		res.Samples[i].ReadNS = time.Since(started).Nanoseconds()
		if err != nil {
			res.Samples[i].Verdict = verdictReadError
			res.Samples[i].Err = err.Error()
			continue
		}
		originals[i] = buf
		if err := cfg.Journal.record(s.Offset, buf); err != nil {
			return nil, fmt.Errorf("writing journal: %w", err)
		}
	}
	progress("read", n, n)

	if cfg.ReadOnly {
		for i := range res.Samples {
			if res.Samples[i].Verdict == "" {
				res.Samples[i].Verdict = verdictOK
			}
		}
		finalize(res)
		return res, nil
	}

	// Phase 2 - write the patterns, then force them to the media.
	for i, s := range plan {
		progress("write", i, n)
		if res.Samples[i].Verdict != "" {
			continue // never write where the original could not be saved
		}
		started := time.Now()
		err := dev.writeAt(expected[i], s.Offset)
		res.Samples[i].WriteNS = time.Since(started).Nanoseconds()
		written[i] = true
		if err != nil {
			res.Samples[i].Verdict = verdictWriteError
			res.Samples[i].Err = err.Error()
		}
	}
	progress("write", n, n)
	if err := dev.sync(); err != nil {
		// Writes may already have reached the device even when its flush request
		// fails. Make a best-effort restoration before returning; the caller will
		// still retain the journal because durability could not be established.
		restoreOriginals(dev, plan, res, originals, written, progress)
		return nil, fmt.Errorf("flushing writes: %w", err)
	}

	// Phase 3 - verify. Reopening drops anything the OS held, and the
	// shuffled order means the drive's own cache cannot cover for media that
	// never received the data.
	if err := dev.reopen(); err != nil {
		return nil, fmt.Errorf("reopening device: %w", err)
	}
	order := rngFromSeed(cfg.Seed, "verify").Perm(n)
	got := make([]byte, cfg.SampleSize)
	for done, i := range order {
		progress("verify", done, n)
		if res.Samples[i].Verdict != "" {
			continue
		}
		started := time.Now()
		err := dev.readAt(got, plan[i].Offset)
		res.Samples[i].VerifyNS = time.Since(started).Nanoseconds()
		if err != nil {
			res.Samples[i].Verdict = verdictReadError
			res.Samples[i].Err = err.Error()
			continue
		}
		v, alias := classify(got, expected[i], originals[i], byPattern)
		res.Samples[i].Verdict = v
		res.Samples[i].AliasOffset = alias
	}
	progress("verify", n, n)

	// Phase 3b - hunt for address wrapping while the patterns are still on
	// the media, which has to happen before the restore below undoes them.
	progress("wrapscan", 0, 1)
	detectWrap(dev, cfg, plan, res)
	progress("wrapscan", 1, 1)

	// Phase 4 - put everything back.
	restoreOriginals(dev, plan, res, originals, written, progress)

	finalize(res)

	if cfg.Estimate && res.Failures > 0 {
		estimateCapacity(dev, cfg, plan, res, progress)
	} else if res.Failures == 0 {
		res.EstimatedCapacity = cfg.End
		if res.WrapModulus > 0 {
			res.EstimatedCapacity = res.WrapModulus
			res.EstimateNote = "capacity taken from the wrap modulus; the sampled locations themselves all read back correctly"
		}
	}
	return res, nil
}

func restoreOriginals(dev blockDevice, plan []Sample, res *runResult, originals [][]byte, written []bool, progress func(string, int, int)) {
	for i, s := range plan {
		progress("restore", i, len(plan))
		if !written[i] || originals[i] == nil {
			continue
		}
		if err := dev.writeAt(originals[i], s.Offset); err != nil {
			res.RestoreErrors = append(res.RestoreErrors,
				fmt.Sprintf("offset %d (%s): %v", s.Offset, humanBytes(s.Offset), err))
			continue
		}
		res.Samples[i].Restored = true
	}
	progress("restore", len(plan), len(plan))
	if err := dev.sync(); err != nil {
		res.RestoreErrors = append(res.RestoreErrors, fmt.Sprintf("final flush: %v", err))
	}
}

func finalize(res *runResult) {
	res.Duration = time.Since(res.Started)
	for _, s := range res.Samples {
		res.ByVerdict[string(s.Verdict)]++
		if !s.Verdict.ok() {
			res.Failures++
			if res.FirstFailure < 0 || s.Offset < res.FirstFailure {
				res.FirstFailure = s.Offset
			}
		}
	}
}

// estimateCapacity narrows down where the device stops storing data. The
// samples already bracket the boundary; a binary search between the highest
// good sample and the lowest bad one finds it to within one sample.
func estimateCapacity(dev blockDevice, cfg runConfig, plan []Sample, res *runResult, progress func(string, int, int)) {
	lastGood, firstBad := int64(-1), int64(-1)
	for _, s := range res.Samples {
		if s.Verdict.ok() {
			if s.Offset > lastGood {
				lastGood = s.Offset
			}
		} else if firstBad < 0 || s.Offset < firstBad {
			firstBad = s.Offset
		}
	}
	if firstBad < 0 {
		res.EstimatedCapacity = cfg.End
		return
	}
	// A binary search cannot be trusted on a device that wraps addresses: a
	// lone probe writes and reads the same folded block, so every address
	// looks fine. Where wrapping was seen, the fold point is the answer.
	if res.WrapModulus > 0 {
		res.EstimatedCapacity = res.WrapModulus
		res.EstimateNote = "capacity taken from the address fold point rather than a boundary search"
		return
	}
	if lastGood > firstBad {
		res.EstimateNote = "good and bad locations are interleaved, so there is no single capacity boundary; " +
			"this looks like failing or damaged media rather than a simple capacity lie"
		return
	}

	if res.ByVerdict[string(verdictAliased)] > 0 {
		res.EstimateNote = fmt.Sprintf("the device reuses physical blocks for different addresses but the fold point "+
			"could not be identified; usable capacity is at most %s", humanBytes(firstBad))
		return
	}

	start := alignUp(cfg.Start, cfg.SampleSize)
	slotOf := func(off int64) int64 { return (off - start) / cfg.SampleSize }
	lo, hi := int64(-1), slotOf(firstBad) // lo is known good, hi is known bad
	if lastGood >= 0 {
		lo = slotOf(lastGood)
	}

	decoy := plan[0].Offset
	restoreFailed := false
	probe := func(off int64) bool {
		orig := make([]byte, cfg.SampleSize)
		if err := dev.readAt(orig, off); err != nil {
			return false
		}
		if err := cfg.Journal.record(off, orig); err != nil {
			return false
		}
		want := make([]byte, cfg.SampleSize)
		fillPattern(want, cfg.Seed, off)
		writeErr := dev.writeAt(want, off)
		_ = dev.sync()
		_ = dev.reopen()

		var readErr error
		got := make([]byte, cfg.SampleSize)
		if writeErr == nil {
			if decoy != off { // push the drive's own cache along
				_ = dev.readAt(make([]byte, cfg.SampleSize), decoy)
			}
			readErr = dev.readAt(got, off)
		}

		// Always put the original back, whatever happened above. A failed restore
		// must be visible to the caller so the undo journal is retained.
		if err := dev.writeAt(orig, off); err != nil {
			restoreFailed = true
			res.RestoreErrors = append(res.RestoreErrors,
				fmt.Sprintf("capacity probe offset %d (%s): %v", off, humanBytes(off), err))
		} else if err := dev.sync(); err != nil {
			restoreFailed = true
			res.RestoreErrors = append(res.RestoreErrors,
				fmt.Sprintf("capacity probe flush at offset %d (%s): %v", off, humanBytes(off), err))
		}
		return writeErr == nil && readErr == nil && bytes.Equal(got, want)
	}

	for hi-lo > 1 && res.Probes < maxProbes && !restoreFailed {
		mid := lo + (hi-lo)/2
		progress("estimate", res.Probes, maxProbes)
		res.Probes++
		if probe(start + mid*cfg.SampleSize) {
			lo = mid
		} else {
			hi = mid
		}
	}
	progress("estimate", res.Probes, res.Probes)

	res.EstimatedCapacity = start + (lo+1)*cfg.SampleSize
	if lo < 0 {
		res.EstimatedCapacity = start
	}
	if hi-lo > 1 {
		reason := fmt.Sprintf("stopped after %d probes", maxProbes)
		if restoreFailed {
			reason = "stopped because a capacity probe could not be safely restored"
		}
		res.EstimateNote = fmt.Sprintf("%s; the boundary is somewhere between %s and %s",
			reason, humanBytes(start+(lo+1)*cfg.SampleSize), humanBytes(start+hi*cfg.SampleSize))
	}
}

// detectWrap looks for the signature of a modulo-wrapping counterfeit that the
// lattice can miss: a real capacity that is not a multiple of the probe
// stride. The pattern written to the highest sample is unique to that address,
// so if a device folds addresses at some modulus M, that pattern is physically
// sitting at highest%M. Trying a list of plausible moduli costs one 4 KB read
// each and needs no writes at all.
func detectWrap(dev blockDevice, cfg runConfig, plan []Sample, res *runResult) {
	high := plan[len(plan)-1]
	switch res.Samples[high.Index].Verdict {
	case verdictWriteError, verdictReadError:
		return // nothing of ours is up there to find
	}

	want := make([]byte, cfg.SampleSize)
	fillPattern(want, cfg.Seed, high.Offset)
	got := make([]byte, cfg.SampleSize)

	for _, m := range wrapCandidates(cfg.End) {
		echo := high.Offset % m
		if echo == high.Offset || echo%cfg.BlockSize != 0 {
			continue // no fold, or an address the raw device would reject
		}
		res.WrapProbes++
		if err := dev.readAt(got, echo); err != nil {
			continue
		}
		if bytes.Equal(got, want) {
			res.WrapModulus, res.WrapEcho = m, echo
			return
		}
	}
}

// wrapCandidates lists the capacities a counterfeit plausibly wraps at:
// powers of two, whole binary gigabytes, and whole decimal gigabytes, which
// between them cover what fake flash actually reports.
func wrapCandidates(claimed int64) []int64 {
	const maxCandidates = 1024
	seen := map[int64]bool{}
	add := func(v int64) {
		if v >= 1<<20 && v <= claimed {
			seen[v] = true
		}
	}
	for m := int64(1) << 20; m > 0 && m <= claimed; m <<= 1 {
		add(m)
	}
	for k := int64(1); k <= 512; k++ {
		add(k << 30)
		add(k * 1_000_000_000)
		add(k << 20)
	}
	out := make([]int64, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > maxCandidates {
		out = out[len(out)-maxCandidates:]
	}
	return out
}
