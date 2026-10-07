// Package dedup performs near-duplicate clustering over indexed documents.
//
// Clustering semantics:
//
//   - Two entries are linked when the Jaccard similarity of their k-shingle
//     sets is >= the configured threshold.
//   - A cluster is a connected component of the "near-duplicate" graph:
//     a new entry similar to members of several clusters merges them all.
//   - The representative of a cluster is its earliest-inserted member
//     (insertion order, ties broken by ID). This choice makes the
//     representative immune to ordinary additions and to deletion of
//     non-representative members; it only changes when the representative
//     itself is removed or a deletion splits the cluster, which is the
//     minimum jitter possible for a deterministic representative rule.
//   - Removing a member recomputes connected components inside its former
//     cluster, so clusters split correctly when the bridging entry goes.
package dedup

import (
	"sort"

	"github.com/guoyyit/search-result-dedup-go/internal/model"
	"github.com/guoyyit/search-result-dedup-go/internal/similarity"
)

// Entry is a document paired with its precomputed shingle fingerprint.
type Entry struct {
	Doc      model.Document
	Shingles map[uint64]struct{}
	// order is the cluster-wide insertion sequence used for representative
	// stability. Lower values are older.
	order int
}

// Cluster is a group of near-duplicate entries with a single representative.
type Cluster struct {
	// ID is the deterministic cluster identifier (derived from its representative).
	ID string `json:"id"`
	// Representative is the doc ID of the entry kept for the cluster.
	Representative string `json:"representative"`
	// MemberIDs contains all doc IDs in the cluster, sorted ascending.
	MemberIDs []string `json:"member_ids"`
}

// ClusterIDPrefix namespaces cluster identifiers derived from doc IDs.
const ClusterIDPrefix = "c:"

// ClusterIDOf returns the deterministic cluster ID for a representative.
func ClusterIDOf(representativeID string) string {
	return ClusterIDPrefix + representativeID
}

// Clusterer incrementally maintains near-duplicate clusters.
type Clusterer struct {
	threshold float64
	hasher    *similarity.Hasher
	entries   map[string]*Entry
	parent    map[string]string
	seq       int
}

// NewClusterer creates an empty Clusterer. threshold is clamped into (0,1];
// non-positive or out-of-range values become 1 (identical-shingle match).
func NewClusterer(threshold float64, hasher *similarity.Hasher) *Clusterer {
	if threshold <= 0 || threshold > 1 {
		threshold = 1
	}
	return &Clusterer{
		threshold: threshold,
		hasher:    hasher,
		entries:   make(map[string]*Entry),
		parent:    make(map[string]string),
	}
}

// Hasher returns the configured shingle hasher.
func (c *Clusterer) Hasher() *similarity.Hasher { return c.hasher }

// find returns the current union-find root of id, with path compression.
func (c *Clusterer) find(id string) string {
	root := id
	for c.parent[root] != root {
		root = c.parent[root]
	}
	for c.parent[id] != root {
		next := c.parent[id]
		c.parent[id] = root
		id = next
	}
	return root
}

// union merges the components of a and b.
func (c *Clusterer) union(a, b string) {
	ra, rb := c.find(a), c.find(b)
	if ra == rb {
		return
	}
	// Attach the component whose oldest member is younger under the older
	// one so roots tend to stay on stable representatives. Roots carry no
	// semantic meaning (representatives are derived on read), but this keeps
	// internal structure predictable.
	oldest := func(root string) (*Entry, string) {
		var best *Entry
		bestID := root
		for id := range c.entries {
			if c.find(id) == root {
				e := c.entries[id]
				if best == nil || e.order < best.order || (e.order == best.order && id < bestID) {
					best, bestID = e, id
				}
			}
		}
		return best, bestID
	}
	ea, _ := oldest(ra)
	eb, _ := oldest(rb)
	if eb.order < ea.order || (eb.order == ea.order && rb < ra) {
		ra, rb = rb, ra
	}
	c.parent[rb] = ra
}

// Add inserts an entry and unions it with every near-duplicate neighbour.
// It returns false if the ID already existed (no change).
//
// When the new entry is similar to members of several existing clusters,
// all those clusters are merged into one (transitive clustering).
func (c *Clusterer) Add(e Entry) bool {
	if _, exists := c.entries[e.Doc.ID]; exists {
		return false
	}
	if e.order == 0 {
		c.seq++
		e.order = c.seq
	} else if e.order > c.seq {
		c.seq = e.order
	}
	c.entries[e.Doc.ID] = &e
	c.parent[e.Doc.ID] = e.Doc.ID

	// Find every distinct component containing a near-duplicate member.
	var roots []string
	seen := make(map[string]bool)
	for id, other := range c.entries {
		if id == e.Doc.ID {
			continue
		}
		if similarity.Jaccard(e.Shingles, other.Shingles) >= c.threshold {
			r := c.find(id)
			if !seen[r] {
				seen[r] = true
				roots = append(roots, r)
			}
		}
	}
	for _, r := range roots {
		c.union(e.Doc.ID, r)
	}
	return true
}

// componentMembers returns the member IDs of the component owning id.
func (c *Clusterer) componentMembers(id string) []string {
	root := c.find(id)
	var members []string
	for mid := range c.entries {
		if c.find(mid) == root {
			members = append(members, mid)
		}
	}
	sort.Strings(members)
	return members
}

// rebuild resets union-find roots for the given member set and re-derives
// edges from pairwise threshold comparisons. It is used by Remove and Upsert.
func (c *Clusterer) rebuild(members []string) {
	for _, m := range members {
		c.parent[m] = m
	}
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			a, b := members[i], members[j]
			if similarity.Jaccard(c.entries[a].Shingles, c.entries[b].Shingles) >= c.threshold {
				ra, rb := c.find(a), c.find(b)
				if ra != rb {
					// Attach under the older representative root for stability.
					if c.entries[rb].order < c.entries[ra].order ||
						(c.entries[rb].order == c.entries[ra].order && rb < ra) {
						c.parent[ra] = rb
					} else {
						c.parent[rb] = ra
					}
				}
			}
		}
	}
}

// Upsert inserts a new entry (same semantics as Add) or replaces an existing
// entry's document and shingles in place. The existing insertion order is
// preserved on replacement so the cluster representative does not jitter
// when a member's content is refreshed.
//
// After replacement the former cluster is rebuilt (allowing splits), and the
// replaced entry is linked against any outside near-duplicates (allowing
// merges). added reports whether the ID was previously absent.
func (c *Clusterer) Upsert(e Entry) (added bool) {
	if old, ok := c.entries[e.Doc.ID]; !ok {
		c.Add(e)
		return true
	} else {
		affected := c.componentMembers(e.Doc.ID)
		e.order = old.order
		c.entries[e.Doc.ID] = &e
		// Rebuild the old component: unchanged entries cannot have gained
		// edges to each other, so pairwise rebuild is sufficient there and
		// may split it.
		c.rebuild(affected)
		// Only the replaced entry can have gained edges to other components.
		for id, other := range c.entries {
			if id == e.Doc.ID {
				continue
			}
			if c.find(id) == c.find(e.Doc.ID) {
				continue
			}
			if similarity.Jaccard(e.Shingles, other.Shingles) >= c.threshold {
				c.union(e.Doc.ID, id)
			}
		}
		return false
	}
}

// Remove deletes an entry and splits its former cluster when necessary.
// It returns false if the ID was not present.
//
// The remaining members are re-clustered from scratch (connected components
// of the pairwise similarity graph restricted to that cluster); unrelated
// clusters are never touched.
func (c *Clusterer) Remove(id string) bool {
	if _, ok := c.entries[id]; !ok {
		return false
	}
	members := c.componentMembers(id)
	delete(c.entries, id)
	delete(c.parent, id)

	remaining := make([]string, 0, len(members))
	for _, m := range members {
		if m != id {
			remaining = append(remaining, m)
		}
	}
	c.rebuild(remaining)
	return true
}

// representative returns the earliest-inserted member ID of a component.
func (c *Clusterer) representative(root string) string {
	rep := ""
	var repOrder int
	for id, e := range c.entries {
		if c.find(id) != root {
			continue
		}
		if rep == "" || e.order < repOrder || (e.order == repOrder && id < rep) {
			rep, repOrder = id, e.order
		}
	}
	return rep
}

// Get returns a copy of the stored entry.
func (c *Clusterer) Get(id string) (Entry, bool) {
	e, ok := c.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// clusterForRoot builds the Cluster value for one union-find root.
func (c *Clusterer) clusterForRoot(root string) Cluster {
	members := c.componentMembers(root)
	rep := c.representative(root)
	return Cluster{
		ID:             ClusterIDOf(rep),
		Representative: rep,
		MemberIDs:      members,
	}
}

// Clusters returns all current clusters sorted by representative ID.
func (c *Clusterer) Clusters() []Cluster {
	roots := make(map[string]bool)
	for id := range c.entries {
		roots[c.find(id)] = true
	}
	out := make([]Cluster, 0, len(roots))
	for r := range roots {
		out = append(out, c.clusterForRoot(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Representative < out[j].Representative })
	return out
}

// ClusterOf returns the cluster containing the given doc ID.
func (c *Clusterer) ClusterOf(id string) (Cluster, bool) {
	if _, ok := c.entries[id]; !ok {
		return Cluster{}, false
	}
	return c.clusterForRoot(c.find(id)), true
}

// Len returns the number of stored entries.
func (c *Clusterer) Len() int { return len(c.entries) }
