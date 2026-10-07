// Package diversity deterministically reorders result items so that adjacent
// positions come from different clusters (preferred) or, failing that, from
// different sources.
//
// Reranking semantics:
//
//   - Items are grouped by ClusterID; each group keeps its own internal order
//     (score descending, ID ascending).
//
//   - At every position the available group heads are partitioned into tiers:
//
//     tier 0: cluster differs from the previously emitted item
//     tier 1: same cluster, but a different source
//     tier 2: same cluster and same source (used only when nothing else
//     is available, so no item is ever dropped)
//
//   - Within a tier the head with the highest score wins; ties are broken
//     by item ID. The first position uses a virtual previous item, so the
//     highest-scoring item starts the page.
//
// Because every choice is keyed only on (score, ID, cluster, source), the
// permutation is a pure function of the input: identical inputs always
// produce byte-identical output.
package diversity

import "sort"

// Item is a rerankable result element.
type Item struct {
	// ID is the document ID.
	ID string
	// ClusterID is the near-duplicate cluster the item belongs to.
	ClusterID string
	// Source is the upstream source of the item.
	Source string
	// Score is the retrieval score; higher is ranked first within a constraint.
	Score float64
}

// headScore / headID comparisons live in small helpers for readability.
type group struct {
	items []Item
}

// Rerank returns a deterministic diversity-ordered permutation of items.
// No item is dropped: len(result) == len(items) and the multisets are equal.
// A nil/empty input yields nil.
func Rerank(items []Item) []Item {
	if len(items) == 0 {
		return nil
	}

	// Stable sort by score desc, ID asc, then group. This establishes each
	// item's internal priority without mutating the caller's slice.
	ordered := make([]Item, len(items))
	copy(ordered, items)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		return ordered[i].ID < ordered[j].ID
	})

	groupOrder := make([]string, 0)
	groups := make(map[string]*group)
	for _, it := range ordered {
		g, ok := groups[it.ClusterID]
		if !ok {
			g = &group{}
			groups[it.ClusterID] = g
			groupOrder = append(groupOrder, it.ClusterID)
		}
		g.items = append(g.items, it)
	}
	// Deterministic group iteration order: by first head ID.
	sort.Slice(groupOrder, func(i, j int) bool {
		return groups[groupOrder[i]].items[0].ID < groups[groupOrder[j]].items[0].ID
	})

	out := make([]Item, 0, len(ordered))
	prev := Item{} // zero Item: empty cluster/source never matches real items
	havePrev := false

	for remaining := len(ordered); remaining > 0; remaining-- {
		bestIdx := -1
		bestTier := 3
		for i, cid := range groupOrder {
			g := groups[cid]
			if len(g.items) == 0 {
				continue
			}
			head := g.items[0]
			tier := 2
			if !havePrev || head.ClusterID != prev.ClusterID {
				tier = 0
			} else if head.Source != prev.Source {
				tier = 1
			}
			choose := tier < bestTier
			if tier == bestTier && bestIdx >= 0 {
				bestHead := groups[groupOrder[bestIdx]].items[0]
				if head.Score > bestHead.Score || (head.Score == bestHead.Score && head.ID < bestHead.ID) {
					choose = true
				}
			}
			if choose {
				bestTier, bestIdx = tier, i
			}
		}
		chosen := groups[groupOrder[bestIdx]]
		prev = chosen.items[0]
		havePrev = true
		out = append(out, prev)
		chosen.items = chosen.items[1:]
	}
	return out
}
