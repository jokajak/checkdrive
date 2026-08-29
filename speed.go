package main

import (
	"fmt"
	"time"
)

// The read-speed survey is the other half of what GRC's tooling does for a
// drive. Where the capacity check asks "does this device store what it claims
// to?", ReadSpeed (https://www.grc.com/readspeed.htm) asks "how fast does it
// hand data back, and does that hold up across the whole platter or die?".
//
// The answer is not one number. A drive that reads at 500 MB/s at the front
// and 40 MB/s at the back is telling you something - an SMR hard disk, a
// nearly full SSD whose SLC cache is exhausted, a USB stick whose controller
// falls over on the high addresses, or a counterfeit that is emulating flash
// it does not have. So the survey times sequential reads at a handful of
// evenly spaced zones - the beginning, the middle, the end and the quarter
// points in between - and reports each one separately.
//
// Nothing here writes, so this mode is safe on media whose contents matter.

type speedConfig struct {
	Zones     int   // how many places across the device to time
	ZoneBytes int64 // bytes read sequentially at each one
	ChunkSize int64 // size of a single read request
	BlockSize int64
	Start     int64
	End       int64
	Progress  func(phase string, done, total int)
	// Now is the clock the timings come from. Tests replace it; production
	// leaves it nil and gets time.Now.
	Now func() time.Time
}

// speedZone is one place on the device that gets timed.
type speedZone struct {
	Index    int
	Offset   int64
	Position float64 // 0 at the front of the device, 1 at the end
	Length   int64
}

// speedPlan is the zone layout a config works out to once it has been fitted
// to the device: sizes are rounded to whole transfers, and a device too small
// for the requested layout gets shorter zones, or fewer of them.
type speedPlan struct {
	Zones     []speedZone
	ChunkSize int64
	ZoneBytes int64
}

type zoneResult struct {
	Index       int     `json:"index"`
	Offset      int64   `json:"offset"`
	Position    float64 `json:"position"`
	Bytes       int64   `json:"bytes"`
	DurationNS  int64   `json:"duration_ns"`
	BytesPerSec float64 `json:"bytes_per_sec"`
	Transfers   stats   `json:"transfer_latency"`
	Err         string  `json:"error,omitempty"`
}

// label names the zone the way ValiDrive's sibling does: by how far across the
// device it sits.
func (z zoneResult) label() string { return fmt.Sprintf("%.0f%%", z.Position*100) }

func (z zoneResult) ok() bool { return z.Err == "" && z.Bytes > 0 }

type speedResult struct {
	Started     time.Time     `json:"started"`
	Duration    time.Duration `json:"duration_ns"`
	ChunkSize   int64         `json:"chunk_size"`
	ZoneBytes   int64         `json:"zone_bytes"`
	BytesRead   int64         `json:"bytes_read"`
	Zones       []zoneResult  `json:"zones"`
	Slowest     float64       `json:"slowest_bytes_per_sec"`
	SlowestZone int           `json:"slowest_zone"`
	Fastest     float64       `json:"fastest_bytes_per_sec"`
	FastestZone int           `json:"fastest_zone"`
	Overall     float64       `json:"overall_bytes_per_sec"`
	Errors      int           `json:"errors"`
}

// beginning, middle and end are the three numbers the survey exists to
// produce; the quarter points are context for them.
func (r *speedResult) beginning() *zoneResult { return pickZone(r, 0) }

func (r *speedResult) middle() *zoneResult { return pickZone(r, 0.5) }

func (r *speedResult) end() *zoneResult { return pickZone(r, 1) }

// pickZone returns the zone sitting closest to a given position on the device.
func pickZone(r *speedResult, position float64) *zoneResult {
	if len(r.Zones) == 0 {
		return nil
	}
	best := 0
	for i := range r.Zones {
		if abs(r.Zones[i].Position-position) < abs(r.Zones[best].Position-position) {
			best = i
		}
	}
	return &r.Zones[best]
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// buildSpeedPlan spreads the zones evenly from the front of the usable span to
// the very end of it, so the first zone starts at the beginning of the device
// and the last one finishes at the last addressable block. Everything is
// rounded to whole transfers, and the zones are shrunk - or dropped - rather
// than allowed to overlap on a device too small to hold them all.
func buildSpeedPlan(start, end, zoneBytes, chunkSize, blockSize int64, count int) (speedPlan, error) {
	switch {
	case blockSize <= 0:
		return speedPlan{}, fmt.Errorf("block size must be positive, got %d", blockSize)
	case chunkSize <= 0:
		return speedPlan{}, fmt.Errorf("transfer size must be positive, got %d", chunkSize)
	case zoneBytes <= 0:
		return speedPlan{}, fmt.Errorf("zone length must be positive, got %d", zoneBytes)
	case count <= 0:
		return speedPlan{}, fmt.Errorf("zone count must be positive, got %d", count)
	}

	start = alignUp(start, blockSize)
	end = alignDown(end, blockSize)
	span := end - start
	if span < blockSize {
		return speedPlan{}, fmt.Errorf("device span %s is too small to time a read", humanBytes(span))
	}

	chunk := alignUp(chunkSize, blockSize)
	if chunk > span {
		chunk = alignDown(span, blockSize)
	}

	// Each zone gets at most an equal share of the device, so no two of them
	// ever read the same blocks - a second read of the same address would be
	// timing a cache, not the media.
	length := alignUp(zoneBytes, chunk)
	if share := alignDown(span/int64(count), chunk); share < length {
		length = share
	}
	if length < chunk {
		length = chunk
		if fits := int(span / chunk); fits < count {
			count = fits
		}
	}

	last := alignDown(end-length, blockSize)
	zones := make([]speedZone, count)
	for i := range zones {
		offset, position := start, 0.0
		if count > 1 {
			offset = alignDown(start+(last-start)*int64(i)/int64(count-1), blockSize)
			position = float64(i) / float64(count-1)
		}
		zones[i] = speedZone{Index: i, Offset: offset, Position: position, Length: length}
	}
	return speedPlan{Zones: zones, ChunkSize: chunk, ZoneBytes: length}, nil
}

// runSpeedSurvey times a sequential read at each zone. The device is reopened
// between zones so that nothing the OS learned during one zone can serve the
// next, and each transfer is timed individually so a zone that stalls
// occasionally is distinguishable from one that is uniformly slow.
func runSpeedSurvey(dev blockDevice, cfg speedConfig) (*speedResult, error) {
	plan, err := buildSpeedPlan(cfg.Start, cfg.End, cfg.ZoneBytes, cfg.ChunkSize, cfg.BlockSize, cfg.Zones)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	progress := cfg.Progress
	if progress == nil {
		progress = func(string, int, int) {}
	}

	res := &speedResult{
		Started:     now(),
		ChunkSize:   plan.ChunkSize,
		ZoneBytes:   plan.ZoneBytes,
		Zones:       make([]zoneResult, len(plan.Zones)),
		SlowestZone: -1,
		FastestZone: -1,
	}

	buf := make([]byte, plan.ChunkSize)
	perZone := int(plan.ZoneBytes / plan.ChunkSize)
	total := len(plan.Zones) * perZone
	done := 0
	var totalNS int64

	for _, z := range plan.Zones {
		zr := zoneResult{Index: z.Index, Offset: z.Offset, Position: z.Position}
		if err := dev.reopen(); err != nil {
			return nil, fmt.Errorf("reopening device: %w", err)
		}
		latencies := make([]int64, 0, perZone)
		for c := 0; c < perZone; c++ {
			progress("speed", done, total)
			done++
			started := now()
			err := dev.readAt(buf, z.Offset+int64(c)*plan.ChunkSize)
			elapsed := now().Sub(started).Nanoseconds()
			if err != nil {
				zr.Err = err.Error()
				break
			}
			latencies = append(latencies, elapsed)
			zr.Bytes += plan.ChunkSize
			zr.DurationNS += elapsed
		}
		zr.Transfers = computeStats(latencies)
		if zr.DurationNS > 0 {
			zr.BytesPerSec = float64(zr.Bytes) / (float64(zr.DurationNS) / 1e9)
		}
		if zr.Err != "" {
			res.Errors++
		}
		res.BytesRead += zr.Bytes
		totalNS += zr.DurationNS
		res.Zones[z.Index] = zr
	}
	progress("speed", total, total)

	res.Duration = now().Sub(res.Started)
	summarizeSpeed(res, totalNS)
	return res, nil
}

// summarizeSpeed fills in the headline numbers. The overall rate is total
// bytes over total time rather than the mean of the per-zone rates, so a slow
// zone weighs what it actually cost.
func summarizeSpeed(res *speedResult, totalNS int64) {
	for i, z := range res.Zones {
		if !z.ok() {
			continue
		}
		if res.SlowestZone < 0 || z.BytesPerSec < res.Slowest {
			res.Slowest, res.SlowestZone = z.BytesPerSec, i
		}
		if res.FastestZone < 0 || z.BytesPerSec > res.Fastest {
			res.Fastest, res.FastestZone = z.BytesPerSec, i
		}
	}
	if totalNS > 0 {
		res.Overall = float64(res.BytesRead) / (float64(totalNS) / 1e9)
	}
}

// speedVerdict is the one line that sums the survey up.
func speedVerdict(res *speedResult) (headline string, ok bool) {
	if res.Errors > 0 {
		return fmt.Sprintf("READ ERRORS — %d of %d zones could not be read to the end", res.Errors, len(res.Zones)), false
	}
	if res.FastestZone < 0 {
		return "NO DATA — no zone was timed", false
	}
	spread := ""
	if res.Slowest > 0 {
		if ratio := res.Fastest / res.Slowest; ratio >= 2 {
			spread = fmt.Sprintf("; the %s zone is %.1fx slower than the %s zone",
				res.Zones[res.SlowestZone].label(), ratio, res.Zones[res.FastestZone].label())
		}
	}
	return fmt.Sprintf("%s overall across %d zones%s", humanRate(res.Overall), len(res.Zones), spread), true
}
