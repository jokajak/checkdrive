# checkdrive

A macOS work-alike of [GRC's ValiDrive](https://www.grc.com/validrive.htm): it checks whether a
USB stick, SD card or external SSD actually holds the capacity it advertises, and shows how fast
it responds across its whole address space. ValiDrive is Windows-only; this is a small Go program
that does the same job on a Mac.

Counterfeit flash is common: a controller reports 512 GB to the host while the device contains
32 GB of flash, and the firmware papers over the gap — folding addresses back with a modulo,
silently swallowing writes, or handing back zeros. The drive formats fine, copies files fine, and
loses them weeks later. checkdrive finds that in a couple of minutes without filling the device.

## Credit where it is due

**The idea is not mine — it is Steve Gibson's.** Spot-checking a drive at locations spread
across its whole address space instead of filling it, and mapping the per-location response
times, is the design he published as [ValiDrive](https://www.grc.com/validrive.htm), given away
free at [grc.com](https://www.grc.com/). ValiDrive is the original and the reference, and all
credit for the concept — and for drawing attention to how widespread counterfeit flash is —
belongs to him and to GRC.

checkdrive exists for one reason: ValiDrive is Windows-only and I use a Mac. It is an
independent implementation written from the published description of the approach; no code,
text or assets from ValiDrive are used, and it is not affiliated with, endorsed by, or
supported by GRC. **If you are on Windows, use ValiDrive instead** — it is the real thing, and
it is free.

## What it does

1. Picks a few hundred locations spread across the whole device.
2. Reads and saves the original contents of each one, into an undo journal on the local disk.
3. Writes a unique, incompressible pattern to each location — one that is *bound to the address*
   it lives at.
4. Flushes to the media, reopens the device, and reads every location back in a shuffled order.
5. Puts the original contents back.
6. If anything failed, binary-searches for the boundary and reports the capacity the device
   really has.

The check is non-destructive by design, but it does write to the device. Use it on media whose
contents you can afford to lose.

## Why it can trust what it reads

- **Raw character device.** All I/O goes through `/dev/rdiskN`, which bypasses the unified buffer
  cache, so a read comes from the media rather than from RAM holding what was just written.
  checkdrive requests Darwin's `F_FULLFSYNC`, which asks the drive to flush its own cache too. USB
  drivers that do not support that ioctl are flushed with `fsync` instead.
- **Write everything, then verify everything.** Verification happens after the whole write pass,
  through a reopened file descriptor and in a shuffled order, so a small on-drive cache cannot
  cover for flash that never received the data.
- **Address-bound patterns.** Each location gets a keyed hash stream derived from the run seed and
  its byte offset, so data returned from the wrong address is recognisable as such — and the run
  seed changes every time, so a device cannot pass by replaying last run's contents.
- **A power-of-two probe lattice.** This is the subtle one. Against a drive that folds addresses
  with a modulo, *independently random probe locations do not work*: each probe writes its pattern
  to some physical block and reads the same pattern straight back. The lie only surfaces when two
  probes share a physical block, so that the later write destroys the earlier one. Probes are
  therefore placed on a power-of-two lattice (with a random per-run phase), which guarantees
  collisions for any real capacity that is a multiple of the stride — and counterfeit capacities
  are essentially always round powers of two.
- **An explicit wrap sweep.** For a fold point that is *not* a multiple of the lattice stride, the
  pattern written to the highest location is looked for at `highest % M` for a list of plausible
  capacities `M`. One 4 KB read each, no writes, and it pins the fold point exactly.

## Installing

```sh
go install github.com/jokajak/checkdrive@latest    # into $(go env GOPATH)/bin
```

Or from a checkout:

```sh
git clone https://github.com/jokajak/checkdrive && cd checkdrive
go build -o checkdrive .
go test ./...
```

There are no third-party dependencies, so it cross-compiles from any platform — you do not
need a Mac to build it:

```sh
GOOS=darwin GOARCH=arm64 go build -o checkdrive-darwin-arm64 .
```

## Using it

Raw device access needs root, and macOS refuses raw writes while the device's volumes are mounted.

```sh
sudo ./checkdrive -list                          # what is attached
sudo ./checkdrive -device disk4 -unmount         # the full check
sudo ./checkdrive -device disk4 -read-only       # latency survey, never writes
sudo ./checkdrive -device disk4 -unmount -json   # machine-readable
sudo ./checkdrive -restore /var/folders/.../checkdrive-disk4-....journal
```

Useful flags:

| Flag | Default | What it does |
| --- | --- | --- |
| `-device` | — | `disk4`, `/dev/disk4` or `/dev/rdisk4` |
| `-samples` | `576` | requested probe count (rounded to fit the lattice) |
| `-sample-size` | `4k` | bytes written and verified per location |
| `-skip-start` | `1M` | leaves the partition table alone |
| `-seed` | random | hex seed; reproduces probe placement and patterns |
| `-read-only` | off | read and time only — does not verify capacity |
| `-unmount` / `-remount` | off | unmount volumes first / mount them again after |
| `-journal` | temp file | undo journal path; `none` disables it |
| `-restore` | — | replay an undo journal and exit |
| `-force` | off | allow internal or partition devices |
| `-yes` | off | skip the confirmation prompt |

Exit status is `0` if the device verified, `1` if it failed, `2` if checkdrive itself errored.

## Reading the output

Each cell of the map is one location, in address order, coloured by latency; `X` marks a location
that did not return what was written. The verdicts are:

| Verdict | Meaning |
| --- | --- |
| `ok` | the location returned exactly what was written to it |
| `aliased` | it returned data written to a *different* address — the device is folding addresses |
| `zeros` | it read back as all zeros |
| `unchanged` | the original data is still there; the write was silently discarded |
| `corrupt` | it returned something that matches nothing this run wrote |
| `read-error` / `write-error` | the I/O itself failed |

## If a run is interrupted

Every original block is flushed to the undo journal before the device is touched. If the process
is killed or the drive is yanked mid-run, replay it:

```sh
sudo ./checkdrive -restore /path/to/checkdrive-disk4-….journal
```

The journal is deleted automatically when a run restores everything itself, and kept (with the
path printed) when it does not.

## Limits

- **macOS only.** The scan engine is portable and unit-tested on any platform, but the device
  layer is `diskutil` plus `/dev/rdiskN`.
- **It is a spot check, not a full-surface test.** It samples a few hundred locations, which is
  what makes it take minutes instead of hours. A device that fails only in the regions between
  probes will pass. For an exhaustive verdict, fill the whole device with
  [`f3`](https://github.com/AltraMayor/f3) (`brew install f3`) or badblocks-style tooling, and use
  checkdrive as the fast first pass.
- **Timings are indicative.** They come from single 4 KB transactions, so they characterise
  responsiveness and latency outliers, not sequential throughput.
- It reports what the device does *now*. Media that fails under sustained writes or after a power
  cycle needs a longer soak test.

## Design notes

[`docs/design.md`](docs/design.md) covers why the checks are built the way they are — in
particular why randomly placed probes cannot detect the most common counterfeit, and what
replaces them.

## License

MIT — see [LICENSE](LICENSE). This covers this implementation only; ValiDrive is a separate
proprietary product and a trademark of its authors, and nothing here is derived from it. See
[Credit where it is due](#credit-where-it-is-due).
