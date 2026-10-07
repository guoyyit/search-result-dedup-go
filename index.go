package dedup

import (
	"context"
	"fmt"
	"sort"
)

// indexEntry is the stored state of one item.
type indexEntry struct {
	item Item
	seq  int64 // monotonically increasing insertion sequence
}

// revision is an immutable point-in-time view of the index. Revisions are
// copy-on-write: updates build a new revision while pinned snapshots keep
// referencing old revisions.
type revision struct {
	items    map[string]*indexEntry
	edges    []clusterEdge
	edgeSet  map[[2]string]struct{}
	seq      int64
	clusters map[string][]string // component key -> members (sorted)
	repOf    map[string]string   // item id -> component key
}

// Index is the mutable near-duplicate index. All exported mutations are
// serialized by the service; reads are safe concurrently with them and
// observe a consistent revision.
type Index struct {
	cfg     Config
	wal     *wal
	current *revision
}

func newIndex(cfg Config) *Index {
	return &Index{
		cfg:     cfg,
		current: emptyRevision(),
	}
}

func emptyRevision() *revision {
	return &revision{
		items:    map[string]*indexEntry{},
		edgeSet:  map[[2]string]struct{}{},
		clusters: map[string][]string{},
		repOf:    map[string]string{},
	}
}

func (ix *Index) currentRev() *revision { return ix.current }

// componentOf returns the component key an id belongs to in rev.
func (rev *revision) componentOf(id string) string {
	if c, ok := rev.repOf[id]; ok {
		return c
	}
	return id
}

// UpsertResult reports the outcome of an upsert, including deterministic
// degradation when the configured pairwise-comparison budget was
// exhausted (remaining candidate pairs are treated as non-similar).
type UpsertResult struct {
	Comparisons int64
	Degraded    bool
}

// upsert inserts or replaces an item and returns the mutation whose Edges
// field carries the full normalized edge set after the change. Persisting
// that set makes recovery exact without rescanning text, so resource-limit
// degradation is preserved across restart as well.
//
// Representative stability: component keys are the smallest member id;
// text changes only adjust incident edges, and added edges merely merge
// components (keeping the smallest key), so unchanged text causes no
// cluster churn.
func (ix *Index) upsert(ctx context.Context, it Item, nextSeq int64) (mutation, UpsertResult, error) {
	if it.ID == "" {
		return mutation{}, UpsertResult{}, fmt.Errorf("dedup: item id required")
	}
	rev := ix.current
	prev := rev.items[it.ID]

	items := make(map[string]*indexEntry, len(rev.items)+1)
	for k, v := range rev.items {
		items[k] = v
	}
	entry := &indexEntry{item: it, seq: nextSeq}
	if prev != nil {
		entry.seq = prev.seq // preserve insertion order across overwrites
	}
	items[it.ID] = entry

	// Candidate neighbors in a deterministic order; the budget truncates
	// this order, which keeps degradation reproducible.
	neighbors := make([]string, 0, len(items)-1)
	for id := range items {
		if id != it.ID {
			neighbors = append(neighbors, id)
		}
	}
	sort.Strings(neighbors)

	budget := ix.cfg.effectiveMaxPairComparisons()
	var used int64
	degraded := false
	incident := map[clusterEdge]struct{}{}
	for _, nid := range neighbors {
		if err := ctx.Err(); err != nil {
			return mutation{}, UpsertResult{}, err
		}
		if used >= budget {
			degraded = true
			break
		}
		used++
		other := items[nid]
		if pairSimilarity(it, other.item, ix.cfg.effectiveShingleSize()) >= ix.cfg.effectiveThreshold() {
			e := canonicalEdge(clusterEdge{A: it.ID, B: nid})
			incident[e] = struct{}{}
		}
	}

	// Keep edges not incident to this id; incident ones were recomputed.
	edges := make([]clusterEdge, 0, len(rev.edges))
	for _, e := range rev.edges {
		if e.A != it.ID && e.B != it.ID {
			edges = append(edges, e)
		}
	}
	for e := range incident {
		edges = append(edges, e)
	}
	newRev := rebuildRevision(items, edges, nextSeq)
	ix.current = newRev

	m := mutation{
		Op:    mutUpsert,
		Seq:   nextSeq,
		Item:  &it,
		Edges: append([]clusterEdge(nil), newRev.edges...),
	}
	return m, UpsertResult{Comparisons: used, Degraded: degraded}, nil
}

// delete removes an item and all incident edges. Removing a bridge member
// may split its cluster into several components.
func (ix *Index) delete(_ context.Context, id string, nextSeq int64) (mutation, bool, error) {
	if _, ok := ix.current.items[id]; !ok {
		return mutation{Op: mutDelete, Seq: nextSeq, ID: id}, false, nil
	}
	rev := ix.current
	items := make(map[string]*indexEntry, len(rev.items)-1)
	for k, v := range rev.items {
		if k != id {
			items[k] = v
		}
	}
	edges := make([]clusterEdge, 0, len(rev.edges))
	for _, e := range rev.edges {
		if e.A != id && e.B != id {
			edges = append(edges, e)
		}
	}
	newRev := rebuildRevision(items, edges, nextSeq)
	ix.current = newRev
	return mutation{
		Op:    mutDelete,
		Seq:   nextSeq,
		ID:    id,
		Edges: append([]clusterEdge(nil), newRev.edges...),
	}, true, nil
}

// apply replays one committed mutation. Edge sets are taken verbatim from
// the log, so recovery is deterministic and identical to the online path.
func (ix *Index) apply(m mutation) {
	switch m.Op {
	case mutCompact:
		ix.applyCompact(m)
		return
	case mutUpsert:
		if m.Item == nil {
			return
		}
		rev := ix.current
		seq := m.Seq
		if prev := rev.items[m.Item.ID]; prev != nil {
			seq = prev.seq
		}
		items := make(map[string]*indexEntry, len(rev.items)+1)
		for k, v := range rev.items {
			items[k] = v
		}
		items[m.Item.ID] = &indexEntry{item: *m.Item, seq: seq}
		ix.current = revisionFromEdges(items, m.Edges, m.Seq)
	case mutDelete:
		rev := ix.current
		items := make(map[string]*indexEntry, len(rev.items))
		for k, v := range rev.items {
			if k != m.ID {
				items[k] = v
			}
		}
		ix.current = revisionFromEdges(items, m.Edges, m.Seq)
	}
}

// applyCompact seeds the index from a compact snapshot.
func (ix *Index) applyCompact(m mutation) {
	entries := make(map[string]*indexEntry, len(m.Items))
	for i := range m.Items {
		it := m.Items[i]
		seq, ok := m.Seqs[it.ID]
		if !ok {
			seq = m.Seq
		}
		entries[it.ID] = &indexEntry{item: it, seq: seq}
	}
	ix.current = revisionFromEdges(entries, m.Edges, m.Seq)
}

func canonicalEdge(e clusterEdge) clusterEdge {
	if e.A < e.B {
		return e
	}
	return clusterEdge{A: e.B, B: e.A}
}

// revisionFromEdges rebuilds a revision treating edges as the complete,
// already-durable edge set (recovery path).
func revisionFromEdges(items map[string]*indexEntry, edges []clusterEdge, seq int64) *revision {
	return rebuildRevision(items, edges, seq)
}

// rebuildRevision constructs an immutable revision, normalizing edges
// (deduplicated, surviving endpoints only) and recomputing clusters.
func rebuildRevision(items map[string]*indexEntry, edges []clusterEdge, seq int64) *revision {
	ids := make(map[string]struct{}, len(items))
	for id := range items {
		ids[id] = struct{}{}
	}
	set := make(map[[2]string]struct{}, len(edges))
	norm := make([]clusterEdge, 0, len(edges))
	for _, e := range edges {
		lo, hi, ok := edgeKey(e)
		if !ok {
			continue
		}
		if _, a := ids[lo]; !a {
			continue
		}
		if _, b := ids[hi]; !b {
			continue
		}
		key := [2]string{lo, hi}
		if _, dup := set[key]; dup {
			continue
		}
		set[key] = struct{}{}
		norm = append(norm, clusterEdge{A: lo, B: hi})
	}
	sort.Slice(norm, func(i, j int) bool {
		if norm[i].A != norm[j].A {
			return norm[i].A < norm[j].A
		}
		return norm[i].B < norm[j].B
	})
	comps := connectedComponents(ids, norm)
	repOf := make(map[string]string, len(items))
	for key, members := range comps {
		for _, m := range members {
			repOf[m] = key
		}
	}
	return &revision{
		items:    items,
		edges:    norm,
		edgeSet:  set,
		seq:      seq,
		clusters: comps,
		repOf:    repOf,
	}
}
