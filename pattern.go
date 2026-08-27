package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
)

// fillPattern fills buf with a keyed hash stream derived from the run seed
// and the byte offset of the sample.
//
// Two properties matter for catching counterfeit media:
//
//   - The data is bound to its address. A drive that wraps writes back onto
//     lower blocks (the usual fake-capacity trick) hands back data that is
//     valid but belongs to a different offset, which is detectable.
//   - The data is incompressible and unpredictable. A controller cannot
//     "store" the pattern cheaply, and the seed changes every run, so a
//     device cannot pass by replaying whatever was written last time.
func fillPattern(buf []byte, seed [32]byte, offset int64) {
	var scratch [48]byte
	copy(scratch[:32], seed[:])
	binary.LittleEndian.PutUint64(scratch[32:40], uint64(offset))
	for i := 0; i < len(buf); i += sha256.Size {
		binary.LittleEndian.PutUint64(scratch[40:48], uint64(i))
		sum := sha256.Sum256(scratch[:])
		copy(buf[i:], sum[:])
	}
}

// verdict is the classification of a single sample after read-back.
type verdict string

const (
	verdictOK         verdict = "ok"
	verdictUnchanged  verdict = "unchanged"
	verdictZeros      verdict = "zeros"
	verdictAliased    verdict = "aliased"
	verdictCorrupt    verdict = "corrupt"
	verdictReadError  verdict = "read-error"
	verdictWriteError verdict = "write-error"
)

func (v verdict) ok() bool { return v == verdictOK }

func (v verdict) describe() string {
	switch v {
	case verdictOK:
		return "verified"
	case verdictUnchanged:
		return "write was silently discarded (original data still there)"
	case verdictZeros:
		return "read back as all zeros"
	case verdictAliased:
		return "read back data written to a different offset (address wrapping)"
	case verdictCorrupt:
		return "read back data that matches nothing written"
	case verdictReadError:
		return "read failed"
	case verdictWriteError:
		return "write failed"
	}
	return string(v)
}

// classify decides what happened at one sample. byPattern maps the SHA-256 of
// every pattern this run wrote to the offset it was written at, which is what
// lets an aliased read name the offset it actually came from.
func classify(got, want, original []byte, byPattern map[[32]byte]int64) (verdict, int64) {
	switch {
	case bytes.Equal(got, want):
		return verdictOK, -1
	case allZero(got):
		return verdictZeros, -1
	case original != nil && bytes.Equal(got, original):
		return verdictUnchanged, -1
	}
	if src, found := byPattern[sha256Of(got)]; found {
		return verdictAliased, src
	}
	return verdictCorrupt, -1
}

// sha256Of is the fingerprint used to recognise a pattern that turned up at
// the wrong address.
func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
