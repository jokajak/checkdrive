# Design notes

Why checkdrive is built the way it is. The README covers what it does; this covers the
decisions behind it, and in particular the one that is easy to get wrong.

## The problem

Counterfeit flash is routine in the removable-storage supply chain: a controller reports
512 GB to the host while the device holds 32 GB, and the firmware papers over the gap.
It does that in one of four ways, and they need different detection:

- **wrapping** — addresses are folded back with a modulo, so writing high overwrites low;
- **discarding** — writes above the real capacity are accepted and thrown away;
- **zeroing** — reads above the real capacity return zeros regardless of what was written;
- **erroring** — the device rejects the I/O outright, which is the honest failure and the
  easy case.

The device formats, mounts and copies files without complaint in every one of these cases.
The data goes missing later.

Steve Gibson's [ValiDrive](https://www.grc.com/validrive.htm) is the well-known tool for this,
and the source of the whole approach taken here: spot-check locations spread across the full
address space rather than filling the device, and map the per-location response times. It is
Windows-only. The alternatives on a Mac were a Windows VM, `f3`/`h2testw`-style full-surface
fills (hours per device, and they wear the flash), or a native spot-checker built on the same
idea. checkdrive is the third — an independent implementation of Gibson's approach, with no
code or content taken from ValiDrive.

## Implementation decisions

- **Go, standard library only.** No cgo and no third-party modules, so it cross-compiles to
  `darwin/arm64` and `darwin/amd64` from any platform with no module downloads, and CI can
  vet, test and build it on a Linux runner. Rust would have done the job equally well; Go
  wins on `os/exec` plus `encoding/json` for the `diskutil` plumbing.
- **`diskutil` for device metadata, not IOKit.** `diskutil info -plist` piped through
  `plutil -convert json` gives block size, total size, bus protocol, internal/removable and
  mount state with zero dependencies. IOKit would need cgo for no additional information.
- **`/dev/rdiskN` for I/O.** The raw character device bypasses the unified buffer cache,
  which is what makes a read-back meaningful. `os.File.Sync` on Darwin issues `F_FULLFSYNC`,
  so the drive's own write cache gets flushed too, without hand-rolling `fcntl`.
- **Write everything, then verify everything.** Verification runs after the whole write
  pass, through a reopened file descriptor and in a shuffled order, so a small on-drive
  cache cannot cover for flash that never received the data.
- **Platform split at one seam.** Everything except the raw-device layer is portable and
  unit tested; `device_darwin.go` holds the macOS specifics and `device_other.go` stubs them
  out. The scan engine talks to a `blockDevice` interface, so the tests drive simulated
  counterfeits — wrapping, discarding, zero-returning and erroring — end to end on any
  platform.

## The part that needed thought: catching a wrap-around fake

The obvious design — probe a few hundred pseudo-random locations, write a pattern, read it
back — **does not detect a wrapping device**. Each probe writes its pattern to some physical
block and reads that same pattern straight back: correct data, wrong place, and no way to
tell from a single probe. Independent random placement almost never puts two probes on the
same physical block, so nothing collides and the fake passes.

Two mechanisms close that:

1. **A power-of-two probe lattice.** Offsets are `start + phase + i*stride` with `stride` a
   power of two and one random phase per run. Two probes then collide exactly when their
   index distance is a multiple of `realCapacity/stride`, which is guaranteed whenever the
   real capacity is a multiple of the stride — and counterfeit capacities are essentially
   always round powers of two. The later write destroys the earlier one, and the earlier
   probe reads back a pattern stamped with somebody else's address. The per-run phase and
   pattern seed keep the actual addresses and contents unpredictable between runs.
2. **A wrap sweep.** For a fold point that is *not* a multiple of the stride, the pattern
   written to the highest location is looked for at `highest % M` for a list of plausible
   capacities `M` (powers of two, whole binary gigabytes, whole decimal gigabytes). One 4 KB
   read each, no writes, and a hit pins the fold point exactly.

The same reasoning kills the naive capacity estimate: a binary search that writes and reads
one address at a time is fooled by a wrapping device for exactly the same reason. So the
search is only used for devices that discard, zero or error above their real capacity; where
wrapping is detected, the fold point *is* the capacity.

## The read-speed survey

`-speed` is a second, read-only mode, modelled on Gibson's
[ReadSpeed](https://www.grc.com/readspeed.htm) the way the default mode is modelled on
ValiDrive. It answers a different question — how fast does this device hand data back, and does
that hold up across its whole address space? — and the decisions behind it are:

- **Zones, not one number.** Five evenly spaced zones by default: the first begins at the very
  front of the device, the last ends at its last addressable block, and the rest are spread
  between them, so the beginning, the middle and the end are always measured. A single
  whole-drive figure hides the shape that matters — an SMR disk, an exhausted SLC cache, a
  controller that struggles on high addresses, or a counterfeit emulating flash it does not
  have all show up as a profile across the address space rather than as one slow average.
- **Sequential within a zone, and a fresh descriptor between them.** Each zone reads
  `-speed-length` bytes in `-speed-transfer` sized requests from consecutive addresses, which
  is what "transfer rate" means; the device is reopened between zones so a zone cannot be
  served from what the previous one left in a cache. `/dev/rdiskN` already keeps the unified
  buffer cache out of it.
- **Every transfer timed individually.** The per-zone rate comes from the summed transfer
  times, and the slowest single transfer in each zone is reported next to it, so a zone that
  stalls occasionally is distinguishable from one that is uniformly slow. The overall figure is
  total bytes over total time, so a slow zone weighs what it actually cost rather than being
  averaged away.
- **Zones never overlap.** The requested zone length is capped at an equal share of the device
  and rounded to whole transfers; on a device too small for the requested layout the zones
  shrink, and then there are fewer of them. Re-reading the same blocks would be timing a cache.
- **Read-only, all the way down.** The device is opened `O_RDONLY`, so there is no journal, no
  unmount requirement and no confirmation prompt. The internal-disk and whole-disk refusals are
  kept anyway, for consistency with the rest of the tool. It does read from offset 0 — the
  `-skip-start` guard exists to keep writes away from the partition table, and there are none.
- **It does not verify capacity.** A wrapping counterfeit reads back quickly and happily; only
  the default mode catches that, and the report says so explicitly.

## Safety

The tool writes to a device, so the failure modes were designed for before the features:

- Original contents are read and flushed to an undo journal on the local disk **before**
  anything is overwritten; `-restore` replays that journal, and it tolerates a torn tail
  from a run that died mid-write. It is deleted only after a run puts everything back itself.
- Internal disks and bare partitions are refused without `-force`; mounted volumes are
  refused unless `-unmount` is passed; the target's identifier has to be typed to confirm
  unless `-yes`.
- The first megabyte is skipped by default, so an interrupted run cannot take the partition
  table with it.
- `-read-only` gives a latency survey with no writes at all.

## Deliberate limits

It is a spot check, not a full-surface test. A device that fails only in the regions between
probes will pass, so [`f3`](https://github.com/AltraMayor/f3) remains the exhaustive answer
and this is the fast first pass. The default mode's timings come from single 4 KB transactions,
so they characterise responsiveness and latency outliers rather than sequential throughput;
`-speed` measures throughput, but only at a handful of zones, so it is a profile of the drive
rather than a full-surface benchmark.
