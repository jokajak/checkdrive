package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	hdr := journalHeader{
		Created:    time.Now().UTC().Truncate(time.Second),
		Device:     "/dev/disk9",
		Identifier: "disk9",
		DeviceSize: 64 * giB,
		BlockSize:  512,
		SampleSize: 4096,
		Seed:       "abcd",
	}
	j, err := newJournal(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[int64][]byte{
		0:        make([]byte, 4096),
		1 << 20:  append([]byte("hello"), make([]byte, 4091)...),
		64 << 20: append([]byte("world"), make([]byte, 4091)...),
	}
	for off, data := range blocks {
		if err := j.record(off, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}

	got, records, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identifier != hdr.Identifier || got.DeviceSize != hdr.DeviceSize || got.Version != 1 {
		t.Errorf("header round trip mismatch: %+v", got)
	}
	if len(records) != len(blocks) {
		t.Fatalf("got %d records, want %d", len(records), len(blocks))
	}
	for _, r := range records {
		if string(r.Data) != string(blocks[r.Offset]) {
			t.Errorf("record at %d does not match what was written", r.Offset)
		}
	}
}

// A run that dies mid-write leaves a partial record. Everything before it is
// still usable and must be replayable.
func TestJournalToleratesTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	j, err := newJournal(path, journalHeader{
		Identifier: "disk9", DeviceSize: 1 << 20, BlockSize: 1, SampleSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := j.record(int64(i)*4096, []byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	j.close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-9); err != nil {
		t.Fatal(err)
	}

	_, records, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Errorf("got %d intact records, want 3", len(records))
	}
}

func TestJournalRejectsForeignFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-journal")
	if err := os.WriteFile(path, []byte("just some bytes here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readJournal(path); err == nil {
		t.Error("expected an error for a file that is not a journal")
	}
}

func TestJournalDiscardRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j")
	j, err := newJournal(path, journalHeader{Identifier: "disk9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("discard left the journal behind")
	}
}

func TestNilJournalIsANoOp(t *testing.T) {
	var j *journal
	if err := j.record(0, []byte("x")); err != nil {
		t.Errorf("record on a disabled journal: %v", err)
	}
	if err := j.close(); err != nil {
		t.Errorf("close on a disabled journal: %v", err)
	}
	if err := j.discard(); err != nil {
		t.Errorf("discard on a disabled journal: %v", err)
	}
}
