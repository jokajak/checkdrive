package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// stats are the latency percentiles for one class of operation.
type stats struct {
	Count int   `json:"count"`
	MinNS int64 `json:"min_ns"`
	P50NS int64 `json:"p50_ns"`
	P90NS int64 `json:"p90_ns"`
	P99NS int64 `json:"p99_ns"`
	MaxNS int64 `json:"max_ns"`
}

func computeStats(values []int64) stats {
	v := append([]int64(nil), values...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	s := stats{Count: len(v)}
	if len(v) == 0 {
		return s
	}
	pick := func(p float64) int64 {
		idx := int(p * float64(len(v)-1))
		return v[idx]
	}
	s.MinNS, s.MaxNS = v[0], v[len(v)-1]
	s.P50NS, s.P90NS, s.P99NS = pick(0.50), pick(0.90), pick(0.99)
	return s
}

func (s stats) line(label string, sampleSize int64) string {
	if s.Count == 0 {
		return fmt.Sprintf("  %-6s no samples", label)
	}
	rate := float64(sampleSize) / (float64(s.P50NS) / 1e9) / 1e6 // MB/s at the median
	return fmt.Sprintf("  %-6s min %8s   p50 %8s   p90 %8s   max %8s   (%.1f MB/s at p50)",
		label, ms(s.MinNS), ms(s.P50NS), ms(s.P90NS), ms(s.MaxNS), rate)
}

func ms(ns int64) string {
	return fmt.Sprintf("%.2f ms", float64(ns)/1e6)
}

// Latency buckets, roughly logarithmic, mirroring the idea behind ValiDrive's
// colour map: healthy flash sits in the first few buckets and the slow
// outliers that mark a struggling controller stand out immediately.
var bucketCeilingsNS = []int64{
	500_000, 1_000_000, 2_000_000, 5_000_000, 10_000_000,
	25_000_000, 50_000_000, 100_000_000, 250_000_000,
}

var bucketColors = []int{51, 45, 39, 46, 82, 190, 226, 214, 208, 196}

var bucketGlyphs = []string{".", ":", "-", "=", "+", "*", "#", "%", "&", "@"}

func latencyBucket(ns int64) int {
	for i, ceil := range bucketCeilingsNS {
		if ns < ceil {
			return i
		}
	}
	return len(bucketCeilingsNS)
}

type painter struct{ enabled bool }

func (p painter) fg(code int, s string) string {
	if !p.enabled {
		return s
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", code, s)
}

func (p painter) bold(s string) string {
	if !p.enabled {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

const gridColumns = 24

// grid draws one cell per sample in address order, so the picture reads left
// to right, top to bottom, from the front of the device to the end of it.
func grid(w io.Writer, res *runResult, p painter, pick func(sampleResult) int64) {
	for row := 0; row*gridColumns < len(res.Samples); row++ {
		first := res.Samples[row*gridColumns]
		cells := make([]string, 0, gridColumns)
		for col := 0; col < gridColumns; col++ {
			i := row*gridColumns + col
			if i >= len(res.Samples) {
				break
			}
			s := res.Samples[i]
			switch {
			case !s.Verdict.ok():
				cells = append(cells, p.fg(196, "X"))
			case p.enabled:
				cells = append(cells, p.fg(bucketColors[latencyBucket(pick(s))], "█"))
			default:
				cells = append(cells, bucketGlyphs[latencyBucket(pick(s))])
			}
		}
		fmt.Fprintf(w, "  %10s │ %s\n", humanBytes(first.Offset), strings.Join(cells, " "))
	}
}

func legend(p painter) string {
	labels := []string{"<0.5ms", "<1ms", "<2ms", "<5ms", "<10ms", "<25ms", "<50ms", "<100ms", "<250ms", "250ms+"}
	var b strings.Builder
	b.WriteString("  legend  ")
	for i, label := range labels {
		cell := "█"
		if !p.enabled {
			cell = bucketGlyphs[i]
		}
		fmt.Fprintf(&b, "%s %s  ", p.fg(bucketColors[i], cell), label)
	}
	fmt.Fprintf(&b, "%s failed", p.fg(196, "X"))
	return b.String()
}

// overallVerdict turns the sample results into the one line that matters.
func overallVerdict(res *runResult, claimed int64) (headline string, pass bool) {
	if res.ReadOnly {
		if res.Failures > 0 {
			return fmt.Sprintf("READ ERRORS — %d of %d locations could not be read", res.Failures, len(res.Samples)), false
		}
		return fmt.Sprintf("READABLE — all %d locations read back without error (read-only survey: capacity was not verified)", len(res.Samples)), true
	}
	if res.WrapModulus > 0 {
		return fmt.Sprintf("FAIL — the device folds addresses back on themselves at about %s, so it cannot hold the %s it claims",
			humanBytes(res.WrapModulus), humanBytes(claimed)), false
	}
	if res.Failures == 0 {
		return fmt.Sprintf("PASS — all %d locations across %s stored and returned their data", len(res.Samples), humanBytes(claimed)), true
	}
	if res.ByVerdict[string(verdictReadError)]+res.ByVerdict[string(verdictWriteError)] == res.Failures {
		return fmt.Sprintf("FAIL — %d of %d locations returned I/O errors; the media is failing or disconnecting", res.Failures, len(res.Samples)), false
	}
	return fmt.Sprintf("FAIL — %d of %d locations did not hold what was written to them", res.Failures, len(res.Samples)), false
}

func writeReport(w io.Writer, info diskInfo, cfg runConfig, res *runResult, seedHex string, colorize bool) {
	p := painter{enabled: colorize}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Device      %s\n", info.describe())
	fmt.Fprintf(w, "  Capacity    %s claimed (%d bytes, %d byte blocks)\n", humanBytes(info.Size), info.Size, info.BlockSize)
	fmt.Fprintf(w, "  Sampled     %d locations of %s each, %s total\n",
		len(res.Samples), humanBytes(cfg.SampleSize), humanBytes(int64(len(res.Samples))*cfg.SampleSize))
	fmt.Fprintf(w, "  Seed        %s\n", seedHex)
	fmt.Fprintf(w, "  Elapsed     %s\n", res.Duration.Round(time.Millisecond))

	var reads, writes, verifies []int64
	for _, s := range res.Samples {
		if s.ReadNS > 0 {
			reads = append(reads, s.ReadNS)
		}
		if s.WriteNS > 0 {
			writes = append(writes, s.WriteNS)
		}
		if s.VerifyNS > 0 {
			verifies = append(verifies, s.VerifyNS)
		}
	}

	fmt.Fprintf(w, "\n  Read latency by location (front of device at the top)\n\n")
	grid(w, res, p, func(s sampleResult) int64 { return s.ReadNS })
	if !res.ReadOnly {
		fmt.Fprintf(w, "\n  Write latency by location\n\n")
		grid(w, res, p, func(s sampleResult) int64 { return s.WriteNS })
	}
	fmt.Fprintf(w, "\n%s\n\n", legend(p))

	fmt.Fprintln(w, computeStats(reads).line("read", cfg.SampleSize))
	if !res.ReadOnly {
		fmt.Fprintln(w, computeStats(writes).line("write", cfg.SampleSize))
		fmt.Fprintln(w, computeStats(verifies).line("verify", cfg.SampleSize))
	}

	headline, pass := overallVerdict(res, info.Size)
	color := 196
	if pass {
		color = 46
	}
	fmt.Fprintf(w, "\n  %s\n", p.bold(p.fg(color, headline)))

	if res.Failures > 0 {
		for _, v := range []verdict{verdictAliased, verdictZeros, verdictUnchanged, verdictCorrupt, verdictReadError, verdictWriteError} {
			if c := res.ByVerdict[string(v)]; c > 0 {
				fmt.Fprintf(w, "    %-12s %4d  %s\n", v, c, v.describe())
			}
		}
		if res.FirstFailure >= 0 {
			fmt.Fprintf(w, "    first failure at %s (offset %d)\n", humanBytes(res.FirstFailure), res.FirstFailure)
		}
		if a := firstAlias(res); a != nil {
			fmt.Fprintf(w, "    e.g. %s read back the data written to %s\n", humanBytes(a.Offset), humanBytes(a.AliasOffset))
		}
	}

	if res.WrapModulus > 0 {
		fmt.Fprintf(w, "    address wrapping: the data written at %s was found again at %s (fold every %s)\n",
			humanBytes(res.Samples[len(res.Samples)-1].Offset), humanBytes(res.WrapEcho), humanBytes(res.WrapModulus))
	}

	if !res.ReadOnly && res.EstimatedCapacity >= 0 && (res.Failures > 0 || res.WrapModulus > 0) {
		pct := 100 * float64(res.EstimatedCapacity) / float64(info.Size)
		fmt.Fprintf(w, "\n    claimed %s, actually holds about %s (%.1f%% of the claim, +/- %s)\n",
			humanBytes(info.Size), humanBytes(res.EstimatedCapacity), pct, humanBytes(cfg.SampleSize))
	}
	if res.EstimateNote != "" {
		fmt.Fprintf(w, "    note: %s\n", res.EstimateNote)
	}

	if len(res.RestoreErrors) > 0 {
		fmt.Fprintf(w, "\n  %s\n", p.bold(p.fg(196, "WARNING: original data could not be fully restored")))
		for _, e := range res.RestoreErrors {
			fmt.Fprintf(w, "    %s\n", e)
		}
	}
	fmt.Fprintln(w)
}

func firstAlias(res *runResult) *sampleResult {
	for i := range res.Samples {
		if res.Samples[i].Verdict == verdictAliased && res.Samples[i].AliasOffset >= 0 {
			return &res.Samples[i]
		}
	}
	return nil
}
