package dedup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service is the entry point: it owns the index, persistence and
// snapshot/pagination bookkeeping. A Service is safe for concurrent use.
type Service struct {
	cfg   Config
	index *Index

	mu        sync.RWMutex
	nextSeq   int64
	snapshots map[string]*snapshot
	snapshotQ []string // insertion order for eviction
	closed    bool

	now func() time.Time
}

// New creates a Service and replays persisted state from cfg.PersistPath
// when configured. Replay is deterministic: cluster structures are fully
// rebuilt from the same mutation stream, so the recovered index is
// byte-for-byte equivalent to one built in process.
func New(cfg Config) (*Service, error) {
	s := &Service{
		cfg:       cfg,
		index:     newIndex(cfg),
		snapshots: map[string]*snapshot{},
		now:       cfg.clock(),
	}
	if cfg.PersistPath != "" {
		w, lastSeq, muts, err := openWAL(cfg.PersistPath)
		if err != nil {
			return nil, err
		}
		s.index.wal = w
		s.nextSeq = lastSeq
		for _, m := range muts {
			s.index.apply(m)
			if m.Seq > s.nextSeq {
				s.nextSeq = m.Seq
			}
		}
	}
	return s, nil
}

// Close flushes and closes persistence.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	s.closed = true
	if s.index.wal != nil {
		return s.index.wal.close()
	}
	return nil
}

// Upsert adds or replaces an item durably. The mutation is fsynced to the
// WAL before it becomes visible to queries.
func (s *Service) Upsert(ctx context.Context, it Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	seq := s.nextSeq + 1
	m, _, err := s.index.upsert(ctx, it, seq)
	if err != nil {
		return err
	}
	if s.index.wal != nil {
		if err := s.index.wal.append(m); err != nil {
			return err
		}
	}
	s.nextSeq = seq
	s.expireSnapshotsLocked()
	return nil
}

// Delete removes an item durably. Deleting a missing id is a no-op but is
// still recorded so recovery observes the same end state.
func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	seq := s.nextSeq + 1
	m, _, err := s.index.delete(ctx, id, seq)
	if err != nil {
		return err
	}
	if s.index.wal != nil {
		if err := s.index.wal.append(m); err != nil {
			return err
		}
	}
	s.nextSeq = seq
	s.expireSnapshotsLocked()
	return nil
}

// Query executes a query. When cur is nil a new snapshot is built against
// the current revision and the first page returned; later pages pass the
// previous page's NextCursor. Pages of one cursor chain always come from
// the same immutable snapshot, so concurrent Upsert/Delete calls cannot
// duplicate, reorder or lose entries after the first page head.
func (s *Service) Query(ctx context.Context, filter func(Item) bool, pageSize int, cur *Cursor) (*Page, error) {
	if pageSize <= 0 {
		return nil, fmt.Errorf("dedup: pageSize must be positive")
	}

	// Continuation: resolve the pinned snapshot under a read lock, then
	// page out of its immutable entries without holding the lock.
	if cur != nil {
		s.mu.RLock()
		if s.closed {
			s.mu.RUnlock()
			return nil, ErrServiceClosed
		}
		sn, ok := s.snapshots[cur.SnapshotID]
		s.mu.RUnlock()
		if !ok || cur.Offset < 0 {
			return nil, ErrInvalidCursor
		}
		if s.now().Sub(sn.createdAt) > s.cfg.effectiveSnapshotTTL() {
			return nil, ErrSnapshotExpired
		}
		if cur.Offset > len(sn.entries) {
			return nil, ErrInvalidCursor
		}
		return s.pageOf(sn, cur.Offset, pageSize), nil
	}

	// First page: read the current revision pointer under RLock, run the
	// (read-only) pipeline against the immutable revision, then register
	// the materialized snapshot under a write lock.
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrServiceClosed
	}
	rev := s.index.currentRev()
	s.mu.RUnlock()

	entries, degraded := s.prepareSnapshot(ctx, rev, filter)
	sn := &snapshot{
		id:        newSnapshotID(),
		createdAt: s.now(),
		rev:       rev,
		entries:   entries,
		degraded:  degraded,
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrServiceClosed
	}
	s.registerSnapshotLocked(sn)
	s.mu.Unlock()
	return s.pageOf(sn, 0, pageSize), nil
}

// QueryAll returns the full materialized query (subject to resource
// limits) together with any degradation reasons.
func (s *Service) QueryAll(ctx context.Context, filter func(Item) bool) ([]Entry, []string, error) {
	page, err := s.Query(ctx, filter, s.cfg.effectiveMaxSnapshotItems(), nil)
	if err != nil {
		return nil, nil, err
	}
	return page.Entries, page.Degraded, nil
}

func (s *Service) pageOf(sn *snapshot, off, pageSize int) *Page {
	page, hasMore := sn.entriesPage(off, pageSize)
	out := &Page{
		Entries:  page,
		HasMore:  hasMore,
		Degraded: append([]string(nil), sn.degraded...),
	}
	if hasMore {
		out.NextCursor = &Cursor{SnapshotID: sn.id, Offset: off + len(page)}
	}
	return out
}

func (s *Service) registerSnapshotLocked(sn *snapshot) {
	s.snapshots[sn.id] = sn
	s.snapshotQ = append(s.snapshotQ, sn.id)
	s.expireSnapshotsLocked()
	max := s.cfg.effectiveMaxSnapshots()
	for len(s.snapshotQ) > max {
		old := s.snapshotQ[0]
		s.snapshotQ = s.snapshotQ[1:]
		delete(s.snapshots, old)
	}
}

func (s *Service) expireSnapshotsLocked() {
	ttl := s.cfg.effectiveSnapshotTTL()
	cutoff := s.now().Add(-ttl)
	kept := s.snapshotQ[:0]
	for _, id := range s.snapshotQ {
		sn := s.snapshots[id]
		if sn != nil && sn.createdAt.Before(cutoff) {
			delete(s.snapshots, id)
			continue
		}
		kept = append(kept, id)
	}
	s.snapshotQ = kept
}

// snapshotItems builds the persistence payload from a revision.
func snapshotPayload(rev *revision) ([]Item, map[string]int64) {
	ids := make([]string, 0, len(rev.items))
	for id := range rev.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]Item, 0, len(ids))
	seqs := make(map[string]int64, len(ids))
	for _, id := range ids {
		e := rev.items[id]
		items = append(items, e.item)
		seqs[id] = e.seq
	}
	return items, seqs
}

// Compact writes a full snapshot into the WAL and truncates obsolete log
// prefix. Already-registered query snapshots are unaffected.
func (s *Service) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.index.wal == nil {
		return nil
	}
	rev := s.index.currentRev()
	items, seqs := snapshotPayload(rev)
	snap := mutation{
		Op:    mutCompact,
		Seq:   s.nextSeq,
		Edges: append([]clusterEdge(nil), rev.edges...),
		Items: items,
		Seqs:  seqs,
	}
	tail := []mutation{} // all state is inside the compact payload
	if err := compactWAL(s.cfg.PersistPath, snap, tail); err != nil {
		return err
	}
	// Reopen the appender on the rewritten log.
	if err := s.index.wal.close(); err != nil {
		return err
	}
	w, _, _, err := openWAL(s.cfg.PersistPath)
	if err != nil {
		return err
	}
	s.index.wal = w
	return nil
}

func newSnapshotID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("snap-%d", time.Now().UnixNano())
	}
	return "snap-" + hex.EncodeToString(b[:])
}
