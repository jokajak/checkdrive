//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

const (
	diskutilPath = "/usr/sbin/diskutil"
	plutilPath   = "/usr/bin/plutil"
)

var identifierRE = regexp.MustCompile(`^disk[0-9]+(s[0-9]+)*$`)

// normalizeIdentifier accepts disk4, /dev/disk4 or /dev/rdisk4 and returns the
// BSD identifier diskutil expects.
func normalizeIdentifier(s string) (string, error) {
	id := strings.TrimSpace(s)
	id = strings.TrimPrefix(id, "/dev/")
	id = strings.TrimPrefix(id, "r")
	if !identifierRE.MatchString(id) {
		return "", fmt.Errorf("%q does not look like a disk identifier (want disk4, /dev/disk4 or /dev/rdisk4)", s)
	}
	return id, nil
}

// flexBool tolerates the several shapes diskutil has used over the years for
// what is conceptually a boolean.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case bool:
		*b = flexBool(t)
	case float64:
		*b = t != 0
	case string:
		switch strings.ToLower(t) {
		case "yes", "true", "removable", "external":
			*b = true
		case "no", "false", "fixed", "internal":
			*b = false
		default:
			return fmt.Errorf("unrecognized boolean value %q", t)
		}
	default:
		return fmt.Errorf("cannot decode boolean from %T", v)
	}
	return nil
}

type duInfo struct {
	DeviceIdentifier    string    `json:"DeviceIdentifier"`
	DeviceNode          string    `json:"DeviceNode"`
	DeviceBlockSize     int64     `json:"DeviceBlockSize"`
	TotalSize           int64     `json:"TotalSize"`
	Size                int64     `json:"Size"`
	Internal            *flexBool `json:"Internal"`
	Ejectable           flexBool  `json:"Ejectable"`
	RemovableMedia      flexBool  `json:"RemovableMedia"`
	WholeDisk           *flexBool `json:"WholeDisk"`
	SolidState          flexBool  `json:"SolidState"`
	BusProtocol         string    `json:"BusProtocol"`
	MediaName           string    `json:"MediaName"`
	IORegistryEntryName string    `json:"IORegistryEntryName"`
	MountPoint          string    `json:"MountPoint"`
	VolumeName          string    `json:"VolumeName"`
}

type duPartition struct {
	DeviceIdentifier string `json:"DeviceIdentifier"`
	MountPoint       string `json:"MountPoint"`
	VolumeName       string `json:"VolumeName"`
}

type duList struct {
	AllDisksAndPartitions []struct {
		DeviceIdentifier string        `json:"DeviceIdentifier"`
		MountPoint       string        `json:"MountPoint"`
		VolumeName       string        `json:"VolumeName"`
		Partitions       []duPartition `json:"Partitions"`
		APFSVolumes      []duPartition `json:"APFSVolumes"`
	} `json:"AllDisksAndPartitions"`
}

// plistJSON runs a command that emits an XML property list and decodes it into
// out by way of plutil, which keeps this dependency-free: no plist library, and
// diskutil is the same interface Disk Utility itself sits on.
func plistJSON(out any, name string, args ...string) error {
	raw, err := runCommand(name, args...)
	if err != nil {
		return err
	}
	// Never resolve privileged helpers through the caller's PATH. checkdrive is
	// normally run with sudo, and executing a user-selected binary as root would
	// turn an otherwise harmless environment setting into code execution.
	conv := exec.Command(plutilPath, "-convert", "json", "-o", "-", "-")
	conv.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	conv.Stderr = &stderr
	js, err := conv.Output()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", plutilPath, err, strings.TrimSpace(stderr.String()))
	}
	return json.Unmarshal(js, out)
}

func runCommand(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

// probeDisk asks diskutil about one disk and folds in the mount state of every
// volume that currently lives on it.
func probeDisk(target string) (diskInfo, error) {
	id, err := normalizeIdentifier(target)
	if err != nil {
		return diskInfo{}, err
	}

	var info duInfo
	if err := plistJSON(&info, diskutilPath, "info", "-plist", id); err != nil {
		return diskInfo{}, err
	}
	if info.DeviceIdentifier != id {
		return diskInfo{}, fmt.Errorf("%s reported unexpected identifier %q for %q", diskutilPath, info.DeviceIdentifier, id)
	}
	// These two fields drive the checks that protect the system disk. Treat a
	// missing field as an incompatible diskutil response, never as a safe false.
	if info.Internal == nil || info.WholeDisk == nil {
		return diskInfo{}, fmt.Errorf("%s returned incomplete safety metadata for %q", diskutilPath, id)
	}

	size := info.TotalSize
	if size == 0 {
		size = info.Size
	}
	model := info.MediaName
	if model == "" {
		model = info.IORegistryEntryName
	}

	d := diskInfo{
		Identifier: id,
		Path:       "/dev/" + id,
		RawPath:    "/dev/r" + id,
		Model:      strings.TrimSpace(model),
		Protocol:   info.BusProtocol,
		Internal:   bool(*info.Internal),
		Removable:  bool(info.RemovableMedia) || bool(info.Ejectable),
		WholeDisk:  bool(*info.WholeDisk),
		SolidState: bool(info.SolidState),
		BlockSize:  info.DeviceBlockSize,
		Size:       size,
	}
	if d.BlockSize == 0 {
		d.BlockSize = 512
	}
	if info.MountPoint != "" {
		d.Mounted = append(d.Mounted, mountedVolume{Device: d.Path, MountPoint: info.MountPoint, Name: info.VolumeName})
	}

	mounts, err := mountedOn(id)
	if err != nil {
		return diskInfo{}, err
	}
	d.Mounted = append(d.Mounted, mounts...)
	return d, nil
}

// mountedOn returns every mounted volume that belongs to the given whole disk.
func mountedOn(id string) ([]mountedVolume, error) {
	var list duList
	if err := plistJSON(&list, diskutilPath, "list", "-plist", id); err != nil {
		return nil, err
	}
	var out []mountedVolume
	for _, disk := range list.AllDisksAndPartitions {
		for _, part := range append(append([]duPartition{}, disk.Partitions...), disk.APFSVolumes...) {
			if part.MountPoint != "" {
				out = append(out, mountedVolume{
					Device:     "/dev/" + part.DeviceIdentifier,
					MountPoint: part.MountPoint,
					Name:       part.VolumeName,
				})
			}
		}
	}
	return out, nil
}

// listDisks enumerates whole disks, external ones first.
func listDisks() ([]diskInfo, error) {
	var list duList
	if err := plistJSON(&list, diskutilPath, "list", "-plist"); err != nil {
		return nil, err
	}
	var out []diskInfo
	for _, disk := range list.AllDisksAndPartitions {
		info, err := probeDisk(disk.DeviceIdentifier)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// unmountDisk detaches every filesystem on the disk. macOS refuses writes to a
// raw device that still has mounted volumes, so this is a prerequisite, not a
// convenience.
func unmountDisk(id string) error {
	_, err := runCommand(diskutilPath, "unmountDisk", "/dev/"+id)
	return err
}

// remountDisk is best effort: it is only ever used to put things back after a
// successful run.
func remountDisk(id string) error {
	_, err := runCommand(diskutilPath, "mountDisk", "/dev/"+id)
	return err
}

// rawDevice is the character-device (/dev/rdiskN) implementation of
// blockDevice. The character device is what makes the whole check meaningful:
// it bypasses the unified buffer cache, so a read really does come from the
// media instead of from RAM that still holds what we just wrote.
type rawDevice struct {
	path     string
	readOnly bool
	f        *os.File
	buf      []byte // page-aligned, big enough for one sample
}

func openDevice(info diskInfo, bufSize int, readOnly bool) (blockDevice, error) {
	d := &rawDevice{path: info.RawPath, readOnly: readOnly}
	buf, err := alignedBuffer(bufSize)
	if err != nil {
		return nil, err
	}
	d.buf = buf
	if err := d.open(); err != nil {
		_ = syscall.Munmap(d.buf)
		return nil, err
	}
	return d, nil
}

func (d *rawDevice) open() error {
	flags := os.O_RDWR
	if d.readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(d.path, flags, 0)
	if err != nil {
		return annotateOpenError(d.path, err)
	}
	d.f = f
	return nil
}

func annotateOpenError(path string, err error) error {
	switch {
	case errors.Is(err, syscall.EBUSY):
		return fmt.Errorf("%s is busy: unmount its volumes first (diskutil unmountDisk %s, or pass -unmount): %w",
			path, strings.Replace(path, "/dev/r", "/dev/", 1), err)
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EPERM):
		return fmt.Errorf("%s: permission denied: run checkdrive with sudo: %w", path, err)
	}
	return fmt.Errorf("open %s: %w", path, err)
}

// alignedBuffer returns a page-aligned scratch buffer. Raw device I/O on
// Darwin wants aligned transfers, and an anonymous mapping is aligned by
// construction (and never moved by the garbage collector).
func alignedBuffer(n int) ([]byte, error) {
	page := os.Getpagesize()
	size := ((n + page - 1) / page) * page
	if size == 0 {
		size = page
	}
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

func (d *rawDevice) readAt(p []byte, off int64) error {
	b := d.buf[:len(p)]
	if _, err := d.f.ReadAt(b, off); err != nil {
		return err
	}
	copy(p, b)
	return nil
}

func (d *rawDevice) writeAt(p []byte, off int64) error {
	if d.readOnly {
		return errors.New("device opened read-only")
	}
	b := d.buf[:len(p)]
	copy(b, p)
	_, err := d.f.WriteAt(b, off)
	return err
}

// sync first issues F_FULLFSYNC (that is what os.File.Sync does on Darwin),
// which asks the drive to flush its own write cache rather than merely handing
// data to the driver. Some USB raw-device drivers do not implement that ioctl;
// for those, fall back to fsync rather than rejecting an otherwise usable
// device.
func (d *rawDevice) sync() error {
	if d.readOnly {
		return nil
	}
	return syncWithFallback(d.f.Sync, func() error {
		return syscall.Fsync(int(d.f.Fd()))
	}, syscall.ENOTTY)
}

func (d *rawDevice) reopen() error {
	if err := d.f.Close(); err != nil {
		return err
	}
	return d.open()
}

func (d *rawDevice) close() error {
	err := d.f.Close()
	if d.buf != nil {
		_ = syscall.Munmap(d.buf)
		d.buf = nil
	}
	return err
}
