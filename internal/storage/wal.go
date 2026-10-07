// Package storage implements a small append-only write-ahead log used to
// reconstruct the committed index after a crash.
//
// On-disk frame layout (all integers big-endian):
//
//	+---------+---------+---------+------------------+
//	| magic 4B| len 4B  | crc 4B  | JSON payload lenB |
//	+---------+---------+---------+------------------+
//
// magic is the ASCII tag "DWL1". crc is CRC-32/IEEE over the payload.
// Every Append is followed by fsync, so any frame whose bytes are present
// and whose CRC matches is committed; a torn trailing frame left by a crash
// fails validation and is deterministically ignored during replay.
package storage

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/guoyyit/search-result-dedup-go/internal/model"
)

// frameMagic marks the start of a WAL frame.
var frameMagic = []byte("DWL1")

// Op identifies a WAL record kind.
type Op uint8

const (
	OpUnknown Op = 0
	// OpAdd inserts or overwrites the document carrying its ID.
	OpAdd Op = 1
	// OpDelete removes the document carrying its ID.
	OpDelete Op = 2
)

// Record is one committed index mutation.
type Record struct {
	Op  Op             `json:"op"`
	Doc model.Document `json:"doc"`
}

// ErrCorrupt indicates a frame failed validation outside the torn tail.
var ErrCorrupt = errors.New("storage: corrupt wal frame")

// Log is an append-only, fsynced mutation log.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// Open opens (creating parent directory and file if needed) the WAL at path.
func Open(path string) (*Log, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{path: path, f: f}, nil
}

// encodeFrame serializes one record into its self-describing frame.
func encodeFrame(rec Record) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, 12+len(payload))
	frame = append(frame, frameMagic...)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	frame = append(frame, lenBuf[:]...)
	binary.BigEndian.PutUint32(lenBuf[:], crc32.ChecksumIEEE(payload))
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, payload...)
	return frame, nil
}

// Append durably appends a single record (write + fsync).
func (l *Log) Append(rec Record) error {
	frame, err := encodeFrame(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(frame); err != nil {
		return err
	}
	return l.f.Sync()
}

// Replay feeds every intact record, in commit order, to fn.
//
// Replay stops cleanly at a truncated/invalid trailing frame (the expected
// shape of a torn write after a crash). Any other read error is returned.
// The returned offset is the byte position immediately after the last
// intact frame, i.e. the safe end-of-log for appends.
func (l *Log) Replay(fn func(Record) error) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	defer func() { _, _ = l.f.Seek(0, io.SeekEnd) }()

	r := l.f
	var header [12]byte
	var validEnd int64
	for {
		frameStart, err := l.f.Seek(0, io.SeekCurrent)
		if err != nil {
			return validEnd, err
		}
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return validEnd, nil // clean end of log or torn frame header
			}
			return validEnd, err
		}
		if string(header[0:4]) != string(frameMagic) {
			return validEnd, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(header[4:8])
		wantCRC := binary.BigEndian.Uint32(header[8:12])
		// Cap payload size defensively: 16 MiB is far above any record we
		// ever write, but bounds memory when reading garbage lengths.
		if n > 16<<20 {
			return validEnd, ErrCorrupt
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return validEnd, nil // torn frame body: ignore the uncommitted tail
			}
			return validEnd, err
		}
		if crc32.ChecksumIEEE(payload) != wantCRC {
			return validEnd, nil // torn frame: stop replay at last committed record
		}
		var rec Record
		if err := json.Unmarshal(payload, &rec); err != nil {
			return validEnd, ErrCorrupt
		}
		if rec.Op != OpAdd && rec.Op != OpDelete {
			return validEnd, ErrCorrupt
		}
		if err := fn(rec); err != nil {
			return validEnd, err
		}
		validEnd = frameStart + 12 + int64(n)
	}
}

// Recover replays committed records and truncates any torn trailing bytes
// left by a crash, so subsequent appends extend a clean log. It must be
// called at most once, before any Append.
func (l *Log) Recover(fn func(Record) error) error {
	validEnd, err := l.Replay(fn)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Truncate(validEnd); err != nil {
		return err
	}
	_, err = l.f.Seek(validEnd, io.SeekStart)
	return err
}

// Close releases the underlying file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
