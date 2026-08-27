package main

import (
	"fmt"
	"io"
)

// fakeMode is how a simulated device misbehaves above its real capacity.
type fakeMode int

const (
	fakeHonest     fakeMode = iota // stores everything it claims to
	fakeWrap                       // folds addresses back with a modulo (the classic counterfeit)
	fakeDiscard                    // silently throws away writes past the real capacity
	fakeZeros                      // accepts the writes, reads back zeros
	fakeWriteError                 // errors on writes past the real capacity
)

// fakeDevice is a blockDevice that lies in whichever way the test asks for. It
// is what lets the scan engine be exercised end to end without a USB stick.
type fakeDevice struct {
	claimed, real int64
	mode          fakeMode
	blocks        map[int64][]byte
	reopens       int
	closed        bool
}

func newFake(claimed, real int64, mode fakeMode) *fakeDevice {
	return &fakeDevice{claimed: claimed, real: real, mode: mode, blocks: map[int64][]byte{}}
}

// initialFill is the "user data" that was on the device before the run, so
// that restoring can be checked and a discarded write is distinguishable from
// a zeroed one.
func initialFill(p []byte, off int64) {
	fillPattern(p, [32]byte{'p', 'r', 'e', 'e', 'x', 'i', 's', 't'}, off)
}

func (f *fakeDevice) load(p []byte, eff int64) {
	if b, ok := f.blocks[eff]; ok {
		copy(p, b)
		return
	}
	initialFill(p, eff)
}

func (f *fakeDevice) readAt(p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > f.claimed {
		return fmt.Errorf("read at %d: %w", off, io.EOF)
	}
	beyond := off+int64(len(p)) > f.real
	switch {
	case f.mode == fakeWrap:
		f.load(p, off%f.real)
	case beyond && f.mode == fakeZeros:
		for i := range p {
			p[i] = 0
		}
	default:
		f.load(p, off)
	}
	return nil
}

func (f *fakeDevice) writeAt(p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > f.claimed {
		return fmt.Errorf("write at %d: %w", off, io.EOF)
	}
	beyond := off+int64(len(p)) > f.real
	switch {
	case f.mode == fakeWrap:
		off %= f.real
	case beyond && f.mode == fakeWriteError:
		return fmt.Errorf("write at %d: device error", off)
	case beyond:
		return nil // silently dropped
	}
	f.blocks[off] = append([]byte(nil), p...)
	return nil
}

func (f *fakeDevice) sync() error { return nil }

func (f *fakeDevice) reopen() error {
	f.reopens++
	return nil
}

func (f *fakeDevice) close() error {
	f.closed = true
	return nil
}

// intact reports whether every location the run touched holds its original
// contents again.
func (f *fakeDevice) intact(offsets []int64, size int64) error {
	buf := make([]byte, size)
	want := make([]byte, size)
	for _, off := range offsets {
		if err := f.readAt(buf, off); err != nil {
			return err
		}
		initialFill(want, off)
		if string(buf) != string(want) {
			return fmt.Errorf("offset %d was not restored", off)
		}
	}
	return nil
}
