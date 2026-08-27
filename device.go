package main

import (
	"errors"
	"fmt"
	"strings"
)

// errUnsupported is returned by the non-darwin build of the device layer.
var errUnsupported = errors.New("checkdrive can only talk to raw devices on macOS")

// syncWithFallback retries a durability request with fsync when a device does
// not implement the stronger Darwin F_FULLFSYNC ioctl. Some USB mass-storage
// drivers return ENOTTY for that ioctl even though fsync works normally.
func syncWithFallback(fullSync, fsync func() error, unsupported error) error {
	err := fullSync()
	if errors.Is(err, unsupported) {
		return fsync()
	}
	return err
}

// blockDevice is the small surface the scan engine needs. The real
// implementation lives in device_darwin.go; tests drive a memory-backed fake.
type blockDevice interface {
	// readAt fills p from off. Both must be block aligned.
	readAt(p []byte, off int64) error
	// writeAt writes p at off. Both must be block aligned.
	writeAt(p []byte, off int64) error
	// sync forces everything written so far all the way to the media.
	sync() error
	// reopen closes and reopens the device, dropping any state the OS or the
	// driver kept about it, so a later read cannot be served from a cache.
	reopen() error
	close() error
}

// mountedVolume is a filesystem the OS currently has mounted from the target.
type mountedVolume struct {
	Device     string `json:"device"`
	MountPoint string `json:"mount_point"`
	Name       string `json:"name,omitempty"`
}

// diskInfo is what the platform layer can tell us about a disk.
type diskInfo struct {
	Identifier string          `json:"identifier"`
	Path       string          `json:"path"`
	RawPath    string          `json:"raw_path"`
	Model      string          `json:"model,omitempty"`
	Protocol   string          `json:"protocol,omitempty"`
	Internal   bool            `json:"internal"`
	Removable  bool            `json:"removable"`
	WholeDisk  bool            `json:"whole_disk"`
	SolidState bool            `json:"solid_state"`
	BlockSize  int64           `json:"block_size"`
	Size       int64           `json:"size"`
	Mounted    []mountedVolume `json:"mounted,omitempty"`
}

func (d diskInfo) describe() string {
	parts := []string{d.Identifier}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	if d.Protocol != "" {
		parts = append(parts, d.Protocol)
	}
	parts = append(parts, humanBytes(d.Size))
	if d.Internal {
		parts = append(parts, "INTERNAL")
	}
	return strings.Join(parts, " · ")
}

// humanBytes formats a byte count the way storage vendors do (decimal), since
// that is the number printed on the package we are checking.
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTP"[exp])
}
