package main

import (
	"strings"
	"testing"
)

func TestComputeStats(t *testing.T) {
	var values []int64
	for i := int64(1); i <= 100; i++ {
		values = append(values, i*1_000_000)
	}
	s := computeStats(values)
	if s.Count != 100 || s.MinNS != 1_000_000 || s.MaxNS != 100_000_000 {
		t.Fatalf("unexpected stats: %+v", s)
	}
	if s.P50NS != 50_000_000 || s.P90NS != 90_000_000 {
		t.Errorf("percentiles off: p50=%d p90=%d", s.P50NS, s.P90NS)
	}
	if empty := computeStats(nil); empty.Count != 0 {
		t.Error("empty input should give an empty summary")
	}
}

func TestLatencyBuckets(t *testing.T) {
	cases := map[int64]int{
		100_000:       0,
		999_999:       1,
		3_000_000:     3,
		30_000_000:    6,
		2_000_000_000: len(bucketCeilingsNS),
	}
	for ns, want := range cases {
		if got := latencyBucket(ns); got != want {
			t.Errorf("latencyBucket(%d) = %d, want %d", ns, got, want)
		}
	}
	if len(bucketColors) != len(bucketCeilingsNS)+1 || len(bucketGlyphs) != len(bucketColors) {
		t.Error("bucket colours and glyphs must cover every bucket")
	}
}

func TestWriteReportPass(t *testing.T) {
	dev := newFake(8*giB, 8*giB, fakeHonest)
	cfg := scanConfig(8*giB, 30)
	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	info := diskInfo{Identifier: "disk9", Model: "Test Media", Protocol: "USB", Size: cfg.End, BlockSize: 512}
	writeReport(&out, info, cfg, res, "deadbeef", false)
	got := out.String()

	for _, want := range []string{"disk9", "Test Media", "PASS", "legend", "read ", "write "} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("report emitted ANSI escapes with colour disabled")
	}
}

func TestWriteReportCounterfeit(t *testing.T) {
	dev := newFake(64*giB, 8*giB, fakeWrap)
	cfg := scanConfig(64*giB, 31)
	res, err := runScan(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	info := diskInfo{Identifier: "disk9", Size: cfg.End, BlockSize: 512}
	writeReport(&out, info, cfg, res, "deadbeef", true)
	got := out.String()

	for _, want := range []string{"FAIL", "folds addresses", "address wrapping", "actually holds about"} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\x1b[") {
		t.Error("report emitted no ANSI escapes with colour enabled")
	}
}
