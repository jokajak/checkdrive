package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"
)

// A run overwrites real user data and puts it back afterwards. If the process
// is killed, the machine panics or the drive is yanked in between, the
// original contents would be gone. The journal is the insurance: every
// original block is written to a local file and flushed to disk *before* the
// device is touched, so an interrupted run can be undone later with
// `checkdrive -restore <journal>`.

const journalMagic = "CHKDRVJ1"

type journalHeader struct {
	Version    int       `json:"version"`
	Created    time.Time `json:"created"`
	Device     string    `json:"device"`
	Identifier string    `json:"identifier"`
	DeviceSize int64     `json:"device_size"`
	BlockSize  int64     `json:"block_size"`
	SampleSize int64     `json:"sample_size"`
	Seed       string    `json:"seed"`
}

type journalRecord struct {
	Offset int64
	Data   []byte
}

type journal struct {
	path string
	f    *os.File
}

func newJournal(path string, hdr journalHeader) (*journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	hdr.Version = 1
	blob, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, len(journalMagic)+4+len(blob))
	buf = append(buf, journalMagic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(blob)))
	buf = append(buf, blob...)
	if _, err := f.Write(buf); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return &journal{path: path, f: f}, nil
}

// record appends one original block and flushes it. The flush is the whole
// point, so it is not optional and not batched.
func (j *journal) record(offset int64, data []byte) error {
	if j == nil {
		return nil
	}
	buf := make([]byte, 0, 12+len(data)+4)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(offset))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(data)))
	buf = append(buf, data...)
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(data))
	if _, err := j.f.Write(buf); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *journal) close() error {
	if j == nil {
		return nil
	}
	return j.f.Close()
}

// discard removes the journal once every block is known to be back in place.
func (j *journal) discard() error {
	if j == nil {
		return nil
	}
	if err := j.f.Close(); err != nil {
		return err
	}
	return os.Remove(j.path)
}

func (j *journal) location() string {
	if j == nil {
		return ""
	}
	return j.path
}

// readJournal loads a journal for replay. A torn final record (the process
// died mid-write) is dropped rather than treated as corruption: everything
// before it is still good.
func readJournal(path string) (journalHeader, []journalRecord, error) {
	var hdr journalHeader
	f, err := os.Open(path)
	if err != nil {
		return hdr, nil, err
	}
	defer f.Close()

	magic := make([]byte, len(journalMagic))
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != journalMagic {
		return hdr, nil, fmt.Errorf("%s is not a checkdrive journal", path)
	}
	var hdrLen uint32
	if err := binary.Read(f, binary.LittleEndian, &hdrLen); err != nil {
		return hdr, nil, err
	}
	blob := make([]byte, hdrLen)
	if _, err := io.ReadFull(f, blob); err != nil {
		return hdr, nil, err
	}
	if err := json.Unmarshal(blob, &hdr); err != nil {
		return hdr, nil, err
	}

	var records []journalRecord
	for {
		var offset uint64
		var length uint32
		if err := binary.Read(f, binary.LittleEndian, &offset); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return hdr, records, err
		}
		if err := binary.Read(f, binary.LittleEndian, &length); err != nil {
			break // torn record
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(f, data); err != nil {
			break // torn record
		}
		var sum uint32
		if err := binary.Read(f, binary.LittleEndian, &sum); err != nil {
			break // torn record
		}
		if sum != crc32.ChecksumIEEE(data) {
			return hdr, records, fmt.Errorf("journal record at offset %d is corrupt", offset)
		}
		records = append(records, journalRecord{Offset: int64(offset), Data: data})
	}
	return hdr, records, nil
}
