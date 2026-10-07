package dedup

import (
	"context"
	"sort"
)

// prepareSnapshot runs the full search pipeline against one immutable
// revision and materializes the ordered entry list:
//
//  1. filter the revision's items;
//  2. sort by the total base order (score desc, id asc);
//  3. deterministically truncate candidates over MaxCandidates;
//  4. group candidates by their global similarity cluster and keep one
//     representative per cluster;
//  5. diversity-rerank (or skip deterministically over MaxRerankItems);
//  6. truncate over MaxSnapshotItems.
//
// Pair-comparison accounting: grouping here relies on clusters that were
// maintained incrementally in the index, so query time does no quadratic
// text scanning; the comparison budget is enforced during index updates.
func (s *Service) prepareSnapshot(ctx context.Context, rev *revision, filter func(Item) bool) ([]Entry, []string) {
	var degraded []string
	addDegrade := func(reason string) {
		for _, d := range degraded {
			if d == reason {
				return
			}
		}
		degraded = append(degraded, reason)
	}

	cands := make([]*indexEntry, 0, len(rev.items))
	for _, e := range rev.items {
		if filter == nil || filter(e.item) {
			cands = append(cands, e)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].item.Score != cands[j].item.Score {
			return cands[i].item.Score > cands[j].item.Score
		}
		return cands[i].item.ID < cands[j].item.ID
	})

	maxCand := s.cfg.effectiveMaxCandidates()
	if len(cands) > maxCand {
		cands = cands[:maxCand]
		addDegrade(DegradeCandidatesTruncated)
	}

	// Group by global component; preserve first-appearance (base order)
	// and record merged ids in base order.
	groups := make(map[string]*clusterGroup)
	groupOrder := make([]string, 0)
	for _, e := range cands {
		cid := rev.componentOf(e.item.ID)
		g := groups[cid]
		if g == nil {
			g = &clusterGroup{id: cid}
			groups[cid] = g
			groupOrder = append(groupOrder, cid)
		}
		g.members = append(g.members, e)
	}

	entries := make([]Entry, 0, len(groups))
	inputs := make([]rerankInput, 0, len(groups))
	for _, cid := range groupOrder {
		if err := ctx.Err(); err != nil {
			break
		}
		g := groups[cid]
		rep := g.pickRepresentative(rev)
		var merged []string
		for _, m := range g.members {
			if m.item.ID != rep.item.ID {
				merged = append(merged, m.item.ID)
			}
		}
		sort.Strings(merged)
		entries = append(entries, Entry{
			Item:      rep.item,
			ClusterID: cid,
			MergedIDs: merged,
		})
		inputs = append(inputs, rerankInput{
			item:      rep.item,
			clusterID: cid,
			source:    rep.item.Source,
			mergedIDs: merged,
			baseIdx:   len(inputs),
		})
	}

	maxRerank := s.cfg.effectiveMaxRerankItems()
	if len(inputs) > maxRerank {
		// Keep stable base order; no reranking for oversized sets.
		addDegrade(DegradeRerankSkipped)
	} else {
		reranked := diversityRerank(inputs)
		for i, r := range reranked {
			entries[i] = Entry{
				Item:      r.item,
				ClusterID: r.clusterID,
				MergedIDs: r.mergedIDs,
			}
		}
	}

	maxSnap := s.cfg.effectiveMaxSnapshotItems()
	if len(entries) > maxSnap {
		entries = entries[:maxSnap]
		addDegrade(DegradeResultsTruncated)
	}
	return entries, degraded
}

type clusterGroup struct {
	id      string
	members []*indexEntry
}

// pickRepresentative chooses the global representative of the cluster when
// it is present in the result set; otherwise the highest-ranked member
// (members arrive in base order, so that is the first one). Keeping the
// global representative whenever possible prevents representative
// oscillation across queries and incremental membership changes.
func (g *clusterGroup) pickRepresentative(rev *revision) *indexEntry {
	for _, m := range g.members {
		if m.item.ID == g.id {
			return m
		}
	}
	return g.members[0]
}
