package dedup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Mutation kinds persisted in the write-ahead log.
const (
	mutUpsert  = "upsert"
	mutDelete  = "delete"
	mutCompact = "compact"
)

// walFileName is the single log file inside the persistence directory.
const walFileName = "wal.log"

// mutation is one durable index change. A compact entry carries the full
// surviving item set with their insertion sequences plus the edge set;
// entries after it are replayed on recovery.
type mutation struct {
	Op    string           `json:"op"`
	Seq   int64            `json:"seq"`
	Item  *Item            `json:"item,omitempty"`
	ID    string           `json:"id,omitempty"`
	Edges []clusterEdge    `json:"edges,omitempty"`
	Items []Item           `json:"items,omitempty"`
	Seqs  map[string]int64 `json:"seqs,omitempty"`
}

// wal is a durable, append-only write-ahead log backed by one JSON object
// per line. Every append is flushed to disk before the corresponding
// mutation becomes visible, so a crash leaves either the old committed
// state or the fully appended mutation on disk.
type wal struct {
	path string
	f    *os.File
	w    *bufio.Writer
	enc  *json.Encoder
}

// openWAL opens (creating the directory if needed) the log and replays it:
// it returns the highest sequence seen and the full mutation stream. The
// latest compact mutation, when present, resets state before subsequent
// entries are replayed. A missing log file means an empty log.
func openWAL(dir string) (*wal, int64, []mutation, error) {
	if dir == "" {
		return nil, 0, nil, fmt.Errorf("dedup: empty persist path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, nil, err
	}
	path := filepath.Join(dir, walFileName)

	var lastSeq int64
	var all []mutation
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var m mutation
			if err := json.Unmarshal(line, &m); err == nil {
				all = append(all, m)
			}
		}
		if err := sc.Err(); err != nil {
			f.Close()
			return nil, 0, nil, err
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, 0, nil, err
	}

	for _, m := range all {
		if m.Seq > lastSeq {
			lastSeq = m.Seq
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, 0, nil, err
	}
	w := &wal{path: path, f: f, w: bufio.NewWriter(f)}
	w.enc = json.NewEncoder(w.w)
	return w, lastSeq, all, nil
}

func lastCompactIndex(all []mutation) int {
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Op == mutCompact {
			return i
		}
	}
	return -1
}

// append writes and flushes one mutation to stable storage.
func (w *wal) append(m mutation) error {
	if w == nil {
		return nil
	}
	if err := w.enc.Encode(m); err != nil {
		return err
	}
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

func (w *wal) close() error {
	if w == nil || w.f == nil {
		return nil
	}
	if err := w.w.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// compactWAL atomically rewrites the log as one compact mutation followed
// by tail (mutations with Seq > lastSeq). The replacement is fsynced and
// renamed over the old log so that a crash during compaction never leaves
// a torn log (the temp file is either fully present or absent).
func compactWAL(dir string, snap mutation, tail []mutation) error {
	path := filepath.Join(dir, walFileName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	writeErr := enc.Encode(snap)
	for i := range tail {
		if writeErr == nil {
			writeErr = enc.Encode(tail[i])
		}
	}
	if writeErr == nil {
		writeErr = bw.Flush()
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if cerr := f.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		os.Remove(tmp)
		return writeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Sync the directory so the rename itself is durable.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

var _ io.Writer = (*os.File)(nil)
