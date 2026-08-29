package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeClock is the clock the survey is timed against in tests, so the
// measured rates are exactly what the simulated device delivers.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// timedDevice is a simulated drive whose read speed depends on where the read
// lands - the shape the survey exists to find. It advances the fake clock by
// however long the transfer "took".
type timedDevice struct {
	*fakeDevice
	clock       *fakeClock
	bytesPerSec func(off int64) float64
	failFrom    int64 // reads at or above this offset fail (0 disables)
}

func newTimedDevice(size int64, rate func(off int64) float64) *timedDevice {
	return &timedDevice{
		fakeDevice:  newFake(size, size, fakeHonest),
		clock:       &fakeClock{t: time.Unix(0, 0)},
		bytesPerSec: rate,
	}
}

func (d *timedDevice) readAt(p []byte, off int64) error {
	if d.failFrom > 0 && off >= d.failFrom {
		return fmt.Errorf("read at %d: simulated device error", off)
	}
	d.clock.t = d.clock.t.Add(time.Duration(float64(len(p)) / d.bytesPerSec(off) * float64(time.Second)))
	return d.fakeDevice.readAt(p, off)
}

// The simulated device is 64 MiB and the survey uses five 256 KiB zones, so
// the third zone starts just below the halfway mark. slowFrom sits between the
// second and third zones, which keeps a "slow at the back" device from having
// a zone that is half fast and half slow.
const speedTestSize = 64 * miB

const slowFrom = speedTestSize * 3 / 8

func constantRate(bytesPerSec float64) func(int64) float64 {
	return func(int64) float64 { return bytesPerSec }
}

func speedTestConfig(dev *timedDevice, size int64) speedConfig {
	return speedConfig{
		Zones:     5,
		ZoneBytes: 256 * kiB,
		ChunkSize: 64 * kiB,
		BlockSize: 512,
		Start:     0,
		End:       size,
		Now:       dev.clock.now,
	}
}

func TestBuildSpeedPlanSpansWholeDevice(t *testing.T) {
	const (
		size   = 64 * giB
		length = 32 * miB
		chunk  = miB
	)
	plan, err := buildSpeedPlan(0, size, length, chunk, 512, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Zones) != 5 {
		t.Fatalf("got %d zones, want 5", len(plan.Zones))
	}
	if plan.ZoneBytes != length || plan.ChunkSize != chunk {
		t.Errorf("plan sizes %d/%d, want %d/%d", plan.ZoneBytes, plan.ChunkSize, length, chunk)
	}
	if first := plan.Zones[0].Offset; first != 0 {
		t.Errorf("first zone starts at %s, want the front of the device", humanBytes(first))
	}
	if last := plan.Zones[4]; last.Offset+plan.ZoneBytes != size {
		t.Errorf("last zone ends at %s, want the end of the device %s",
			humanBytes(last.Offset+plan.ZoneBytes), humanBytes(size))
	}
	for i, z := range plan.Zones {
		if z.Offset%512 != 0 {
			t.Errorf("zone %d offset %d is not block aligned", i, z.Offset)
		}
		if i > 0 {
			prev := plan.Zones[i-1]
			if z.Offset < prev.Offset+plan.ZoneBytes {
				t.Errorf("zone %d at %s overlaps zone %d at %s", i, humanBytes(z.Offset), i-1, humanBytes(prev.Offset))
			}
		}
	}
	for i, want := range []float64{0, 0.25, 0.5, 0.75, 1} {
		if plan.Zones[i].Position != want {
			t.Errorf("zone %d position %.2f, want %.2f", i, plan.Zones[i].Position, want)
		}
	}
}

// A device far too small for the requested layout gets shorter zones, and
// then fewer of them, rather than zones that read each other's blocks.
func TestBuildSpeedPlanShrinksToFitSmallDevice(t *testing.T) {
	plan, err := buildSpeedPlan(0, 3*miB, 32*miB, miB, 512, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Zones) != 3 {
		t.Fatalf("got %d zones on a 3 MiB device, want 3", len(plan.Zones))
	}
	if plan.ZoneBytes != miB {
		t.Errorf("zone length %s, want one whole transfer", humanBytes(plan.ZoneBytes))
	}
	for i, z := range plan.Zones {
		if z.Offset+plan.ZoneBytes > 3*miB {
			t.Errorf("zone %d runs past the end of the device", i)
		}
	}
}

func TestBuildSpeedPlanRejectsImpossibleRequests(t *testing.T) {
	cases := map[string]struct {
		start, end, length, chunk, block int64
		zones                            int
	}{
		"zero block size":  {0, giB, miB, miB, 0, 5},
		"zero zones":       {0, giB, miB, miB, 512, 0},
		"zero transfer":    {0, giB, miB, 0, 512, 5},
		"zero length":      {0, giB, 0, miB, 512, 5},
		"device too small": {0, 256, miB, miB, 512, 5},
	}
	for name, c := range cases {
		if _, err := buildSpeedPlan(c.start, c.end, c.length, c.chunk, c.block, c.zones); err == nil {
			t.Errorf("%s: buildSpeedPlan accepted the request", name)
		}
	}
}

func TestSpeedSurveyMeasuresEachZoneSeparately(t *testing.T) {
	const size = speedTestSize
	// A drive that falls off a cliff part way up its address space - an SMR
	// disk, an exhausted SLC cache, or a controller that is struggling up
	// there. The cliff sits between two zones so no zone straddles it.
	dev := newTimedDevice(size, func(off int64) float64 {
		if off >= slowFrom {
			return 25e6
		}
		return 100e6
	})
	res, err := runSpeedSurvey(dev, speedTestConfig(dev, size))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Zones) != 5 || res.Errors != 0 {
		t.Fatalf("got %d zones and %d errors, want 5 and 0", len(res.Zones), res.Errors)
	}

	want := []float64{100e6, 100e6, 25e6, 25e6, 25e6}
	for i, z := range res.Zones {
		if !z.ok() {
			t.Fatalf("zone %d failed: %s", i, z.Err)
		}
		if z.Bytes != res.ZoneBytes {
			t.Errorf("zone %d read %s, want %s", i, humanBytes(z.Bytes), humanBytes(res.ZoneBytes))
		}
		if delta := z.BytesPerSec - want[i]; delta > want[i]/100 || delta < -want[i]/100 {
			t.Errorf("zone %d measured %s, want %s", i, humanRate(z.BytesPerSec), humanRate(want[i]))
		}
	}
	if res.Zones[res.FastestZone].Position != 0 && res.Zones[res.FastestZone].Position != 0.25 {
		t.Errorf("fastest zone is %s, expected one at the front", res.Zones[res.FastestZone].label())
	}
	if res.Slowest > res.Overall || res.Overall > res.Fastest {
		t.Errorf("overall %s is not between slowest %s and fastest %s",
			humanRate(res.Overall), humanRate(res.Slowest), humanRate(res.Fastest))
	}
	if res.BytesRead != int64(len(res.Zones))*res.ZoneBytes {
		t.Errorf("read %s in total, want %s", humanBytes(res.BytesRead), humanBytes(int64(len(res.Zones))*res.ZoneBytes))
	}

	beginning, middle, end := res.beginning(), res.middle(), res.end()
	if beginning.Position != 0 || middle.Position != 0.5 || end.Position != 1 {
		t.Errorf("beginning/middle/end picked zones at %.2f/%.2f/%.2f", beginning.Position, middle.Position, end.Position)
	}
	if !(beginning.BytesPerSec > end.BytesPerSec) {
		t.Errorf("beginning %s should be faster than the end %s", humanRate(beginning.BytesPerSec), humanRate(end.BytesPerSec))
	}
	headline, ok := speedVerdict(res)
	if !ok || !strings.Contains(headline, "slower than") {
		t.Errorf("unexpected verdict for a device with a 4x spread: %q", headline)
	}
}

// Each zone has to be timed through a freshly opened device, or a zone could
// be served from what the last one left in a cache.
func TestSpeedSurveyReopensBetweenZones(t *testing.T) {
	const size = speedTestSize
	dev := newTimedDevice(size, constantRate(50e6))
	res, err := runSpeedSurvey(dev, speedTestConfig(dev, size))
	if err != nil {
		t.Fatal(err)
	}
	if dev.reopens < len(res.Zones) {
		t.Errorf("device reopened %d times for %d zones", dev.reopens, len(res.Zones))
	}
	if _, ok := speedVerdict(res); !ok {
		t.Error("a healthy device did not pass the survey")
	}
}

// A drive that stops answering part way up its address space is the honest
// failure, and the survey has to report it rather than time out or panic.
func TestSpeedSurveyReportsReadErrors(t *testing.T) {
	const size = speedTestSize
	dev := newTimedDevice(size, constantRate(50e6))
	dev.failFrom = slowFrom
	res, err := runSpeedSurvey(dev, speedTestConfig(dev, size))
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors == 0 {
		t.Fatal("a device that fails on its back half passed the survey")
	}
	for _, z := range res.Zones {
		if z.Offset >= slowFrom && z.ok() {
			t.Errorf("zone at %s reported success despite failing reads", humanBytes(z.Offset))
		}
		if z.Offset < slowFrom && !z.ok() {
			t.Errorf("zone at %s failed but should have been readable: %s", humanBytes(z.Offset), z.Err)
		}
	}
	headline, ok := speedVerdict(res)
	if ok || !strings.Contains(headline, "READ ERRORS") {
		t.Errorf("unexpected verdict: %q", headline)
	}
}

func TestSpeedSurveyNeverWrites(t *testing.T) {
	const size = speedTestSize
	dev := newTimedDevice(size, constantRate(50e6))
	if _, err := runSpeedSurvey(dev, speedTestConfig(dev, size)); err != nil {
		t.Fatal(err)
	}
	if len(dev.blocks) != 0 {
		t.Errorf("the read-speed survey wrote %d blocks", len(dev.blocks))
	}
}

func TestSpeedSurveyHandlesASingleZone(t *testing.T) {
	const size = speedTestSize
	dev := newTimedDevice(size, constantRate(50e6))
	cfg := speedTestConfig(dev, size)
	cfg.Zones = 1
	res, err := runSpeedSurvey(dev, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Zones) != 1 {
		t.Fatalf("got %d zones, want 1", len(res.Zones))
	}
	if res.beginning() != res.middle() || res.middle() != res.end() {
		t.Error("with one zone, beginning, middle and end must be that zone")
	}
}

func TestWriteSpeedReport(t *testing.T) {
	const size = speedTestSize
	dev := newTimedDevice(size, func(off int64) float64 {
		if off >= slowFrom {
			return 20e6
		}
		return 90e6
	})
	res, err := runSpeedSurvey(dev, speedTestConfig(dev, size))
	if err != nil {
		t.Fatal(err)
	}
	info := diskInfo{Identifier: "disk9", Model: "Test Media", Protocol: "USB", Size: size, BlockSize: 512}

	var plain strings.Builder
	writeSpeedReport(&plain, info, res, false)
	got := plain.String()
	for _, want := range []string{"disk9", "Test Media", "read speed", "beginning", "middle", "end", "MB/s", "0%", "50%", "100%", "read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("speed report is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("speed report emitted ANSI escapes with colour disabled")
	}

	var colored strings.Builder
	writeSpeedReport(&colored, info, res, true)
	if !strings.Contains(colored.String(), "\x1b[") {
		t.Error("speed report emitted no ANSI escapes with colour enabled")
	}
}

func TestHumanRate(t *testing.T) {
	cases := map[float64]string{0: "n/a", -1: "n/a", 1e6: "1.0 MB/s", 523_000_000: "523.0 MB/s"}
	for in, want := range cases {
		if got := humanRate(in); got != want {
			t.Errorf("humanRate(%v) = %q, want %q", in, got, want)
		}
	}
}
