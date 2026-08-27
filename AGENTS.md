# checkdrive repository instructions

## Project

- This is a Go 1.24 project: `github.com/jokajak/checkdrive`.
- Keep the dependency policy standard-library-only unless a dependency is clearly justified.
- The project targets macOS device access, especially `darwin/arm64` and `darwin/amd64`.
- Preserve the distinction between the portable scan engine and the macOS device layer:
  - `device_darwin.go` contains macOS-specific device access.
  - `device_other.go` provides non-macOS stubs.
  - The scan engine communicates through the `blockDevice` interface.

## Safety

- Treat raw-device I/O as destructive even though checkdrive is designed to restore original contents.
- Do not weaken confirmation, unmounting, journaling, restore, partition-table protection, or read-only safeguards without an explicit requirement.
- Do not add tests that require a physical disk or root privileges.
- Keep counterfeit-device behavior covered by simulated-device tests.

## Changes

- Read the relevant implementation and tests before modifying behavior.
- Follow existing Go style and keep changes focused.
- Update `README.md` when user-visible commands, flags, output, safety behavior, or limitations change.
- Update `docs/design.md` when implementation decisions or detection logic change.
- Do not commit generated binaries or unrelated worktree changes.

## Verification

Run the narrowest relevant checks while iterating. Before considering a code change complete, run:

```sh
gofmt -w <changed-go-files>
go vet ./...
go test ./...
```

For platform or release-related changes, also verify both macOS builds:

```sh
GOOS=darwin GOARCH=arm64 go build -o /dev/null .
GOOS=darwin GOARCH=amd64 go build -o /dev/null .
```

Never claim a change works without reporting the verification actually performed.
