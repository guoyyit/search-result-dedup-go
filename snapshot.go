package dedup

import "time"

// snapshot is an immutable, fully materialized query result bound to an
// index revision. Cursors reference snapshots by id, so paging through a
// query never observes concurrent index updates: once a page head has
// been returned, every later page of that snapshot stays identical.
type snapshot struct {
	id        string
	createdAt time.Time
	rev       *revision
	entries   []Entry
	degraded  []string
}

// entriesPage returns entries[off:off+limit] and whether more remain.
func (sn *snapshot) entriesPage(off, limit int) ([]Entry, bool) {
	if off < 0 || off > len(sn.entries) {
		return nil, false
	}
	end := off + limit
	if end > len(sn.entries) {
		end = len(sn.entries)
	}
	return sn.entries[off:end], end < len(sn.entries)
}
