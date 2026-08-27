package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestFillPatternIsBoundToItsAddress(t *testing.T) {
	seed := testSeed(20)
	a := make([]byte, 4096)
	b := make([]byte, 4096)
	fillPattern(a, seed, 0)
	fillPattern(b, seed, 4096)
	if bytes.Equal(a, b) {
		t.Fatal("two offsets produced the same pattern, so a wrapped read would look correct")
	}

	again := make([]byte, 4096)
	fillPattern(again, seed, 0)
	if !bytes.Equal(a, again) {
		t.Error("fillPattern is not deterministic")
	}

	other := make([]byte, 4096)
	fillPattern(other, testSeed(21), 0)
	if bytes.Equal(a, other) {
		t.Error("a different seed produced the same pattern, so a device could replay the last run")
	}
}

func TestFillPatternHasNoRepeats(t *testing.T) {
	buf := make([]byte, 4096)
	fillPattern(buf, testSeed(22), 1<<30)
	seen := map[string]bool{}
	for i := 0; i+32 <= len(buf); i += 32 {
		chunk := string(buf[i : i+32])
		if seen[chunk] {
			t.Fatalf("chunk at %d repeats; the pattern is compressible", i)
		}
		seen[chunk] = true
	}
	if allZero(buf) {
		t.Fatal("pattern is all zeros")
	}
}

func TestFillPatternPartialTail(t *testing.T) {
	buf := make([]byte, 100) // not a multiple of the hash size
	fillPattern(buf, testSeed(23), 512)
	if allZero(buf[96:]) {
		t.Error("the tail of the buffer was left unwritten")
	}
}

func TestClassify(t *testing.T) {
	seed := testSeed(24)
	want := make([]byte, 512)
	fillPattern(want, seed, 4096)
	original := make([]byte, 512)
	initialFill(original, 4096)
	elsewhere := make([]byte, 512)
	fillPattern(elsewhere, seed, 1<<30)
	index := map[[32]byte]int64{sha256Of(want): 4096, sha256Of(elsewhere): 1 << 30}

	cases := []struct {
		name string
		got  []byte
		want verdict
	}{
		{"exact match", want, verdictOK},
		{"all zeros", make([]byte, 512), verdictZeros},
		{"write dropped", original, verdictUnchanged},
		{"wrapped from elsewhere", elsewhere, verdictAliased},
		{"garbage", bytes.Repeat([]byte{0xAB}, 512), verdictCorrupt},
	}
	for _, tc := range cases {
		got, alias := classify(tc.got, want, original, index)
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
		if tc.want == verdictAliased && alias != 1<<30 {
			t.Errorf("%s: alias offset %d, want %d", tc.name, alias, int64(1)<<30)
		}
	}
}

func TestVerdictsDescribeThemselves(t *testing.T) {
	for _, v := range []verdict{verdictOK, verdictZeros, verdictUnchanged, verdictAliased, verdictCorrupt, verdictReadError, verdictWriteError} {
		if strings.TrimSpace(v.describe()) == "" {
			t.Errorf("verdict %q has no description", v)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:              "0 B",
		999:            "999 B",
		1_000:          "1.00 kB",
		64_000_000_000: "64.00 GB",
		1 << 30:        "1.07 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
