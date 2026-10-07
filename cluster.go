package dedup

import "sort"

// unionFind is a simple disjoint-set structure keyed by item ids.
type unionFind struct {
	parent map[string]string
	rank   map[string]int
}

func newUnionFind(ids []string) *unionFind {
	uf := &unionFind{
		parent: make(map[string]string, len(ids)),
		rank:   make(map[string]int, len(ids)),
	}
	for _, id := range ids {
		uf.parent[id] = id
	}
	return uf
}

func (uf *unionFind) find(x string) string {
	p := uf.parent[x]
	if p == x {
		return x
	}
	root := uf.find(p)
	uf.parent[x] = root
	return root
}

// union merges the components of a and b and reports whether they were
// previously separate. Unknown ids are registered on demand.
func (uf *unionFind) union(a, b string) bool {
	if _, ok := uf.parent[a]; !ok {
		uf.parent[a] = a
	}
	if _, ok := uf.parent[b]; !ok {
		uf.parent[b] = b
	}
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return false
	}
	switch {
	case uf.rank[ra] < uf.rank[rb]:
		ra, rb = rb, ra
	case uf.rank[ra] == uf.rank[rb]:
		uf.rank[ra]++
	}
	uf.parent[rb] = ra
	return true
}

// clusterEdge records that two items are known-similar at the configured
// threshold. Edges are stored in the index so that deletions can split
// clusters without re-scanning item text.
type clusterEdge struct {
	A string `json:"a"`
	B string `json:"b"`
}

// edgeKey returns the canonical (undirected, self-loop-free) form of an
// edge plus whether it is valid.
func edgeKey(e clusterEdge) (lo, hi string, ok bool) {
	if e.A == "" || e.B == "" || e.A == e.B {
		return "", "", false
	}
	if e.A < e.B {
		return e.A, e.B, true
	}
	return e.B, e.A, true
}

// connectedComponents recomputes clusters from surviving ids and stored
// similarity edges. Component keys are the lexicographically smallest
// surviving member id, which keeps a cluster's identity stable across
// membership changes that do not remove that member; removing the key
// member deterministically renames the component to its new smallest id.
func connectedComponents(ids map[string]struct{}, edges []clusterEdge) map[string][]string {
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sort.Strings(idList)
	uf := newUnionFind(idList)
	for _, e := range edges {
		if _, ok := ids[e.A]; !ok {
			continue
		}
		if _, ok := ids[e.B]; !ok {
			continue
		}
		uf.union(e.A, e.B)
	}
	comps := make(map[string][]string)
	groups := make(map[string][]string)
	for _, id := range idList {
		root := uf.find(id)
		groups[root] = append(groups[root], id)
	}
	for _, members := range groups {
		// idList was sorted and append order follows it, so members[0]
		// is the lexicographically smallest member.
		key := members[0]
		comps[key] = members
	}
	return comps
}
