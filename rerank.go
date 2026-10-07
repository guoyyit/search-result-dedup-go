package dedup

import "sort"

// rerankInput is one item prepared for diversity reranking.
type rerankInput struct {
	item      Item
	clusterID string
	source    string
	mergedIDs []string
	baseIdx   int
}

// diversityRerank deterministically reorders ranked inputs so that
// adjacent entries come from different clusters; when that is impossible
// (one cluster holds all remaining entries), a different source is
// preferred. No input is dropped.
//
// Stability contract: inputs are expected in a total, stable base order
// (score desc, id asc). The reordering is a pure function of that order
// and of cluster/source membership, so identical inputs always produce
// identical output and a query whose clusters did not change keeps the
// same sequence.
//
// Complexity is O(n*k) where k is the number of clusters; callers bound n
// via Config.MaxRerankItems.
func diversityRerank(in []rerankInput) []rerankInput {
	// Preserve stable per-cluster queues in base order.
	order := make([]string, 0)
	queues := make(map[string][]rerankInput)
	posOf := make(map[string]int)
	for _, v := range in {
		q, ok := queues[v.clusterID]
		if !ok {
			posOf[v.clusterID] = len(order)
			order = append(order, v.clusterID)
		}
		queues[v.clusterID] = append(q, v)
	}
	sort.SliceStable(order, func(i, j int) bool {
		// The earliest appearance in base order decides cluster priority.
		return posOf[order[i]] < posOf[order[j]]
	})

	out := make([]rerankInput, 0, len(in))
	lastCluster := ""
	lastSource := ""
	remaining := len(in)
	for remaining > 0 {
		best := -1
		bestFront := -1 // base index of best cluster's front
		// 1) Earliest-front cluster differing from the previous one.
		for i, cid := range order {
			q := queues[cid]
			if len(q) == 0 || cid == lastCluster {
				continue
			}
			idx := q[0].baseIdx
			if best == -1 || idx < bestFront {
				best, bestFront = i, idx
			}
		}
		if best == -1 {
			// 2) Only the previous cluster remains: prefer a different
			//    source within it.
			q := queues[lastCluster]
			pick := -1
			for i := range q {
				if q[i].source != "" && q[i].source != lastSource {
					pick = i
					break
				}
			}
			if pick == -1 {
				pick = 0
			}
			v := q[pick]
			queues[lastCluster] = append(q[:pick], q[pick+1:]...)
			out = append(out, v)
			lastSource = v.source
			remaining--
			continue
		}
		cid := order[best]
		v := queues[cid][0]
		queues[cid] = queues[cid][1:]
		out = append(out, v)
		lastCluster = cid
		lastSource = v.source
		remaining--
	}
	return out
}
