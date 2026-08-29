// Command checkdrive verifies that a USB stick, SD card or external SSD really
// holds the capacity it advertises, on macOS.
//
// It is a work-alike of GRC's ValiDrive (Windows only): it spot-checks
// locations spread across the whole address space of the device, writing a
// unique pattern to each one and reading it back through the raw character
// device so nothing can be answered from a cache, then puts the original data
// back. Counterfeit media - which reports a large capacity but contains a
// small amount of flash - fails those checks above its real size.
//
// It also has a read-only speed mode (-speed), in the spirit of GRC's
// ReadSpeed: sequential read rates timed at the beginning, the middle and the
// end of the device, plus the quarter points in between, because where a drive
// is slow says as much as how slow it is.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// version is replaced with the release tag by GoReleaser. Keeping a useful
// fallback makes binaries built directly from a checkout easy to identify.
var version = "devel"

type options struct {
	device     string
	list       bool
	samples    int
	sampleSize string
	skipStart  string
	seed       string
	readOnly   bool
	noEstimate bool
	speed      bool
	speedZones int
	speedLen   string
	speedXfer  string
	unmount    bool
	remount    bool
	force      bool
	yes        bool
	asJSON     bool
	noColor    bool
	journal    string
	restore    string
	quiet      bool
	showVer    bool
}

func main() {
	if err := run(); err != nil {
		var fail failedCheck
		if errors.As(err, &fail) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "checkdrive: %v\n", err)
		os.Exit(2)
	}
}

// failedCheck marks "the tool worked, the device did not", which is exit 1
// rather than exit 2 so scripts can tell the two apart.
type failedCheck struct{}

func (failedCheck) Error() string { return "device failed verification" }

func run() error {
	var o options
	flag.StringVar(&o.device, "device", "", "device to check, e.g. disk4, /dev/disk4 or /dev/rdisk4")
	flag.BoolVar(&o.list, "list", false, "list attached disks and exit")
	flag.IntVar(&o.samples, "samples", 576, "number of locations to test across the device")
	flag.StringVar(&o.sampleSize, "sample-size", "4k", "bytes written and verified at each location (k/M suffixes allowed)")
	flag.StringVar(&o.skipStart, "skip-start", "1M", "leave this much of the front of the device untouched, to keep the partition table out of harm's way")
	flag.StringVar(&o.seed, "seed", "", "hex seed for the test patterns and sample placement (default: random each run)")
	flag.BoolVar(&o.readOnly, "read-only", false, "only read and time the locations; never write (does not verify capacity)")
	flag.BoolVar(&o.noEstimate, "no-estimate", false, "skip the binary search for the real capacity after a failure")
	flag.BoolVar(&o.speed, "speed", false, "measure sequential read speed at the beginning, middle and end of the device instead of checking capacity (never writes)")
	flag.IntVar(&o.speedZones, "speed-zones", 5, "number of evenly spaced places to time during -speed")
	flag.StringVar(&o.speedLen, "speed-length", "32M", "bytes read sequentially at each -speed zone")
	flag.StringVar(&o.speedXfer, "speed-transfer", "1M", "size of each read request during -speed")
	flag.BoolVar(&o.unmount, "unmount", false, "unmount the device's volumes first (macOS refuses raw writes while they are mounted)")
	flag.BoolVar(&o.remount, "remount", false, "mount the volumes again when the run finishes")
	flag.BoolVar(&o.force, "force", false, "allow internal, system or partition devices (dangerous)")
	flag.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	flag.BoolVar(&o.asJSON, "json", false, "emit machine-readable JSON instead of the report")
	flag.BoolVar(&o.noColor, "no-color", false, "disable ANSI colour")
	flag.StringVar(&o.journal, "journal", "", "path for the undo journal (default: a file in the temp directory; \"none\" disables it)")
	flag.StringVar(&o.restore, "restore", "", "replay an undo journal to put original data back, then exit")
	flag.BoolVar(&o.quiet, "quiet", false, "suppress progress output")
	flag.BoolVar(&o.showVer, "version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	switch {
	case o.showVer:
		fmt.Printf("checkdrive %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case o.restore != "":
		return doRestore(o)
	case o.list:
		return doList(o)
	case o.device == "":
		usage()
		return errors.New("no -device given")
	case o.speed:
		return doSpeed(o)
	}
	return doCheck(o)
}

func usage() {
	fmt.Fprintf(os.Stderr, `checkdrive %s - verify that a drive holds the capacity it claims (macOS)

Usage:
  sudo checkdrive -list
  sudo checkdrive -device disk4 -unmount
  sudo checkdrive -device disk4 -read-only
  sudo checkdrive -device disk4 -speed
  sudo checkdrive -restore /var/folders/.../checkdrive-disk4-....journal

The check is non-destructive: the original contents of every location it
touches are read first, saved to an undo journal, and written back at the end.
It still writes to the device, so use it on media whose contents you can
afford to lose, and never on a disk you cannot unmount.

-speed is a separate, read-only mode: it times sequential reads at the
beginning, the middle and the end of the device and writes nothing at all. It
measures transfer rate; it does not verify capacity.

Exit status: 0 the device verified, 1 the device failed, 2 checkdrive errored.

Options:
`, version)
	flag.PrintDefaults()
}

func doList(o options) error {
	disks, err := listDisks()
	if err != nil {
		return err
	}
	if o.asJSON {
		return json.NewEncoder(os.Stdout).Encode(disks)
	}
	fmt.Println()
	for _, d := range disks {
		fmt.Printf("  %-8s %10s  %-8s %s\n", d.Identifier, humanBytes(d.Size), d.Protocol, d.Model)
		flags := []string{}
		if d.Internal {
			flags = append(flags, "internal")
		}
		if d.Removable {
			flags = append(flags, "removable")
		}
		if d.SolidState {
			flags = append(flags, "ssd")
		}
		if len(flags) > 0 {
			fmt.Printf("           %s\n", strings.Join(flags, ", "))
		}
		for _, m := range d.Mounted {
			fmt.Printf("           mounted: %s on %s\n", m.Device, m.MountPoint)
		}
	}
	fmt.Println()
	return nil
}

func doCheck(o options) error {
	sampleSize, err := parseSize(o.sampleSize)
	if err != nil {
		return fmt.Errorf("-sample-size: %w", err)
	}
	skipStart, err := parseSize(o.skipStart)
	if err != nil {
		return fmt.Errorf("-skip-start: %w", err)
	}
	seed, seedHex, err := parseSeed(o.seed)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}

	info, err := probeDisk(o.device)
	if err != nil {
		return err
	}
	if info.Size <= 0 {
		return fmt.Errorf("%s reports no size; is the media present?", info.Identifier)
	}
	if sampleSize%info.BlockSize != 0 {
		sampleSize = alignUp(sampleSize, info.BlockSize)
	}

	if err := checkSafety(info, o); err != nil {
		return err
	}
	if o.unmount && len(info.Mounted) > 0 && !o.readOnly {
		if err := unmountDisk(info.Identifier); err != nil {
			return err
		}
		if info, err = probeDisk(info.Identifier); err != nil {
			return err
		}
		if err := checkSafety(info, o); err != nil {
			return fmt.Errorf("target changed after unmount: %w", err)
		}
		if len(info.Mounted) > 0 {
			return fmt.Errorf("%s still has mounted volumes after unmount", info.Identifier)
		}
	}
	if err := confirm(info, o); err != nil {
		return err
	}

	cfg := runConfig{
		Samples:    o.samples,
		SampleSize: sampleSize,
		BlockSize:  info.BlockSize,
		Start:      skipStart,
		End:        info.Size,
		Seed:       seed,
		ReadOnly:   o.readOnly,
		Estimate:   !o.noEstimate,
		Progress:   progressPrinter(o),
	}

	if !o.readOnly && o.journal != "none" {
		path := o.journal
		if path == "" {
			path = filepath.Join(os.TempDir(), fmt.Sprintf("checkdrive-%s-%s.journal",
				info.Identifier, time.Now().UTC().Format("20060102T150405Z")))
		}
		j, err := newJournal(path, journalHeader{
			Created:    time.Now().UTC(),
			Device:     info.Path,
			Identifier: info.Identifier,
			DeviceSize: info.Size,
			BlockSize:  info.BlockSize,
			SampleSize: sampleSize,
			Seed:       seedHex,
		})
		if err != nil {
			return err
		}
		cfg.Journal = j
		if !o.asJSON {
			fmt.Fprintf(os.Stderr, "undo journal: %s\n", path)
		}
	}

	dev, err := openDevice(info, int(sampleSize), o.readOnly)
	if err != nil {
		_ = cfg.Journal.close()
		return err
	}

	res, runErr := runScan(dev, cfg)
	_ = dev.close()
	if cfg.Progress != nil {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if runErr != nil {
		_ = cfg.Journal.close()
		if where := cfg.Journal.location(); where != "" {
			return fmt.Errorf("%w (undo journal kept at %s)", runErr, where)
		}
		return runErr
	}

	if len(res.RestoreErrors) == 0 {
		if err := cfg.Journal.discard(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove undo journal: %v\n", err)
		}
	} else if where := cfg.Journal.location(); where != "" {
		_ = cfg.Journal.close()
		fmt.Fprintf(os.Stderr, "undo journal kept at %s - replay it with -restore\n", where)
	}

	if o.remount {
		if err := remountDisk(info.Identifier); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remount %s: %v\n", info.Identifier, err)
		}
	}

	_, pass := overallVerdict(res, info.Size)
	if o.asJSON {
		if err := emitJSON(info, cfg, res, seedHex, pass); err != nil {
			return err
		}
	} else {
		writeReport(os.Stdout, info, cfg, res, seedHex, colorEnabled(o))
	}
	if !pass {
		return failedCheck{}
	}
	return nil
}

// doSpeed runs the read-speed survey: how fast the device hands data back at
// the beginning, the middle, the end, and the quarter points in between. It is
// a work-alike of GRC's ReadSpeed, and the read-only sibling of the capacity
// check - it never writes, so it needs no journal, no unmount and no
// confirmation.
func doSpeed(o options) error {
	zoneBytes, err := parseSize(o.speedLen)
	if err != nil {
		return fmt.Errorf("-speed-length: %w", err)
	}
	transfer, err := parseSize(o.speedXfer)
	if err != nil {
		return fmt.Errorf("-speed-transfer: %w", err)
	}
	if o.speedZones <= 0 {
		return errors.New("-speed-zones must be at least 1")
	}
	if err := requireRoot(); err != nil {
		return err
	}

	info, err := probeDisk(o.device)
	if err != nil {
		return err
	}
	if info.Size <= 0 {
		return fmt.Errorf("%s reports no size; is the media present?", info.Identifier)
	}

	// Nothing is written, so mounted volumes are not in the way and there is
	// nothing to confirm. The internal-disk and whole-disk rules still apply.
	o.readOnly = true
	if err := checkSafety(info, o); err != nil {
		return err
	}

	cfg := speedConfig{
		Zones:     o.speedZones,
		ZoneBytes: zoneBytes,
		ChunkSize: transfer,
		BlockSize: info.BlockSize,
		// Unlike the capacity check this reads from the very front of the
		// device: -skip-start protects the partition table from writes, and
		// there are none here.
		Start:    0,
		End:      info.Size,
		Progress: progressPrinter(o),
	}

	dev, err := openDevice(info, int(alignUp(transfer, info.BlockSize)), true)
	if err != nil {
		return err
	}
	res, err := runSpeedSurvey(dev, cfg)
	_ = dev.close()
	if cfg.Progress != nil {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err != nil {
		return err
	}

	if o.asJSON {
		if err := emitSpeedJSON(info, res); err != nil {
			return err
		}
	} else {
		writeSpeedReport(os.Stdout, info, res, colorEnabled(o))
	}
	if res.Errors > 0 {
		return failedCheck{}
	}
	return nil
}

func doRestore(o options) error {
	hdr, records, err := readJournal(o.restore)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return fmt.Errorf("%s contains no saved blocks", o.restore)
	}
	if err := requireRoot(); err != nil {
		return err
	}

	info, err := probeDisk(hdr.Identifier)
	if err != nil {
		return err
	}
	if info.Size != hdr.DeviceSize && !o.force {
		return fmt.Errorf("%s is %s but the journal was taken from a %s device; pass -force if you are sure",
			info.Identifier, humanBytes(info.Size), humanBytes(hdr.DeviceSize))
	}

	fmt.Fprintf(os.Stderr, "restoring %d blocks saved %s from %s onto %s\n",
		len(records), hdr.Created.Local().Format(time.RFC3339), hdr.Device, info.describe())

	if err := checkSafety(info, o); err != nil {
		return err
	}
	if o.unmount && len(info.Mounted) > 0 {
		if err := unmountDisk(info.Identifier); err != nil {
			return err
		}
		if info, err = probeDisk(info.Identifier); err != nil {
			return err
		}
		if info.Size != hdr.DeviceSize && !o.force {
			return fmt.Errorf("target changed after unmount: %s is %s but the journal expects %s",
				info.Identifier, humanBytes(info.Size), humanBytes(hdr.DeviceSize))
		}
		if err := checkSafety(info, o); err != nil {
			return fmt.Errorf("target changed after unmount: %w", err)
		}
		if len(info.Mounted) > 0 {
			return fmt.Errorf("%s still has mounted volumes after unmount", info.Identifier)
		}
	}
	if err := confirm(info, o); err != nil {
		return err
	}

	maxLen := 0
	for _, r := range records {
		if len(r.Data) > maxLen {
			maxLen = len(r.Data)
		}
	}
	dev, err := openDevice(info, maxLen, false)
	if err != nil {
		return err
	}
	defer dev.close()

	var failures int
	for _, r := range records {
		if err := dev.writeAt(r.Data, r.Offset); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "  offset %d: %v\n", r.Offset, err)
		}
	}
	if err := dev.sync(); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d blocks could not be written back", failures, len(records))
	}
	fmt.Fprintf(os.Stderr, "restored %d blocks; the journal at %s can now be deleted\n", len(records), o.restore)
	return nil
}

// checkSafety refuses the targets a person would regret: the boot disk, an
// internal drive, or a single partition rather than the whole device.
func checkSafety(info diskInfo, o options) error {
	if info.Internal && !o.force {
		return fmt.Errorf("%s is an internal disk; checkdrive writes to it, so it refuses without -force", info.Identifier)
	}
	if !info.WholeDisk && !o.force {
		return fmt.Errorf("%s is a partition, not a whole disk; pass the whole disk (e.g. disk4) or use -force", info.Identifier)
	}
	if len(info.Mounted) > 0 && !o.readOnly && !o.unmount {
		var b strings.Builder
		fmt.Fprintf(&b, "%s still has mounted volumes and macOS will refuse raw writes:\n", info.Identifier)
		for _, m := range info.Mounted {
			fmt.Fprintf(&b, "    %s on %s\n", m.Device, m.MountPoint)
		}
		fmt.Fprintf(&b, "  pass -unmount, or run: diskutil unmountDisk %s", info.Path)
		return errors.New(b.String())
	}
	return nil
}

func confirm(info diskInfo, o options) error {
	if o.yes || o.readOnly {
		return nil // nothing is written, so there is nothing to confirm
	}
	fmt.Fprintf(os.Stderr, "\nAbout to write to %s.\nEvery location it touches is saved first and written back afterwards,\n"+
		"but a crash mid-run leaves that data only in the undo journal.\n\nType %q to continue: ",
		info.describe(), info.Identifier)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != info.Identifier {
		return errors.New("aborted")
	}
	return nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("raw device access needs root: re-run with sudo")
	}
	return nil
}

func progressPrinter(o options) func(string, int, int) {
	if o.quiet || o.asJSON {
		return nil
	}
	last := time.Now().Add(-time.Second)
	return func(phase string, done, total int) {
		if done < total && time.Since(last) < 100*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Fprintf(os.Stderr, "\r\033[K  %-8s %4d/%-4d", phase, done, total)
	}
}

func colorEnabled(o options) bool {
	if o.noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	trimmed := strings.TrimSuffix(strings.TrimSuffix(s, "iB"), "B")
	switch last := trimmed[len(trimmed)-1]; last {
	case 'k', 'K':
		mult, trimmed = 1<<10, trimmed[:len(trimmed)-1]
	case 'm', 'M':
		mult, trimmed = 1<<20, trimmed[:len(trimmed)-1]
	case 'g', 'G':
		mult, trimmed = 1<<30, trimmed[:len(trimmed)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(trimmed), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return n * mult, nil
}

func parseSeed(s string) ([32]byte, string, error) {
	var seed [32]byte
	if s == "" {
		if _, err := rand.Read(seed[:]); err != nil {
			return seed, "", err
		}
		return seed, hex.EncodeToString(seed[:]), nil
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return seed, "", fmt.Errorf("-seed must be hex: %w", err)
	}
	if len(raw) == 0 {
		return seed, "", errors.New("-seed is empty")
	}
	copy(seed[:], raw)
	return seed, hex.EncodeToString(seed[:]), nil
}

type jsonReport struct {
	Tool              string     `json:"tool"`
	Version           string     `json:"version"`
	Device            diskInfo   `json:"device"`
	Seed              string     `json:"seed"`
	Samples           int        `json:"samples"`
	SampleSize        int64      `json:"sample_size"`
	SkipStart         int64      `json:"skip_start"`
	Pass              bool       `json:"pass"`
	Headline          string     `json:"headline"`
	ClaimedCapacity   int64      `json:"claimed_capacity"`
	EstimatedCapacity int64      `json:"estimated_capacity"`
	ReadStats         stats      `json:"read_stats"`
	WriteStats        stats      `json:"write_stats"`
	VerifyStats       stats      `json:"verify_stats"`
	Result            *runResult `json:"result"`
}

func emitJSON(info diskInfo, cfg runConfig, res *runResult, seedHex string, pass bool) error {
	headline, _ := overallVerdict(res, info.Size)
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
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonReport{
		Tool:              "checkdrive",
		Version:           version,
		Device:            info,
		Seed:              seedHex,
		Samples:           len(res.Samples),
		SampleSize:        cfg.SampleSize,
		SkipStart:         cfg.Start,
		Pass:              pass,
		Headline:          headline,
		ClaimedCapacity:   info.Size,
		EstimatedCapacity: res.EstimatedCapacity,
		ReadStats:         computeStats(reads),
		WriteStats:        computeStats(writes),
		VerifyStats:       computeStats(verifies),
		Result:            res,
	})
}

type jsonSpeedReport struct {
	Tool         string       `json:"tool"`
	Version      string       `json:"version"`
	Mode         string       `json:"mode"`
	Device       diskInfo     `json:"device"`
	Zones        int          `json:"zones"`
	ZoneBytes    int64        `json:"zone_bytes"`
	TransferSize int64        `json:"transfer_size"`
	Headline     string       `json:"headline"`
	OK           bool         `json:"ok"`
	Result       *speedResult `json:"result"`
}

func emitSpeedJSON(info diskInfo, res *speedResult) error {
	headline, ok := speedVerdict(res)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonSpeedReport{
		Tool:         "checkdrive",
		Version:      version,
		Mode:         "speed",
		Device:       info,
		Zones:        len(res.Zones),
		ZoneBytes:    res.ZoneBytes,
		TransferSize: res.ChunkSize,
		Headline:     headline,
		OK:           ok,
		Result:       res,
	})
}
