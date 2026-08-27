package main

import (
	"errors"
	"testing"
)

func TestSyncWithFallbackForUnsupportedIoctl(t *testing.T) {
	unsupported := errors.New("unsupported ioctl")
	fallbackCalls := 0
	err := syncWithFallback(func() error { return unsupported }, func() error {
		fallbackCalls++
		return nil
	}, unsupported)
	if err != nil {
		t.Fatalf("syncWithFallback returned %v", err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback called %d times, want 1", fallbackCalls)
	}
}

func TestSyncWithFallbackPreservesOtherErrors(t *testing.T) {
	want := errors.New("device disappeared")
	unsupported := errors.New("unsupported ioctl")
	fallbackCalls := 0
	err := syncWithFallback(func() error { return want }, func() error {
		fallbackCalls++
		return nil
	}, unsupported)
	if !errors.Is(err, want) {
		t.Fatalf("syncWithFallback returned %v, want %v", err, want)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback called %d times, want 0", fallbackCalls)
	}
}
