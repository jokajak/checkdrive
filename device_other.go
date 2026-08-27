//go:build !darwin

package main

// The scan engine, the plan, the patterns and the report are portable and are
// exercised by the tests on any platform. Only the raw-device layer is macOS
// specific, so on other systems it compiles to stubs that refuse politely.

func probeDisk(string) (diskInfo, error) { return diskInfo{}, errUnsupported }

func listDisks() ([]diskInfo, error) { return nil, errUnsupported }

func unmountDisk(string) error { return errUnsupported }

func remountDisk(string) error { return errUnsupported }

func openDevice(diskInfo, int, bool) (blockDevice, error) { return nil, errUnsupported }
