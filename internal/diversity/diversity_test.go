package diversity

import (
	"fmt"
	"testing"
)

func TestRerankNoLossAndDeterministic(t *testing.T) {
	items := []Item{
		{ID: "1", ClusterID: "c1", Source: "a", Score: 10},
		{ID: "2", ClusterID: "c2", Source: "a", Score: 9},
		{ID: "3", ClusterID: "c1", Source: "b", Score: 8},
		{ID: "4", ClusterID: "c3", Source: "a", Score: 7},
	}
	out := Rerank(items)
	if len(out) != len(items) {
		t.Fatalf("lost items: %d -> %d", len(items), len(out))
	}
	counts := map[string]int{}
	for _, it := range out {
		counts[it.ID]++
	}
	for _, it := range items {
		if counts[it.ID] != 1 {
			t.Fatalf("item %s appears %d times", it.ID, counts[it.ID])
		}
	}
	out2 := Rerank(items)
	for i := range out {
		if out[i] != out2[i] {
			t.Fatalf("rerank not deterministic at %d: %v vs %v", i, out, out2)
		}
	}
}

func TestRerankAdjacentClustersDiffer(t *testing.T) {
	// One dominant cluster with enough other clusters to separate: when
	// possible, adjacent items must never share a cluster.
	items := []Item{
		{ID: "a1", ClusterID: "A", Source: "x", Score: 100},
		{ID: "a2", ClusterID: "A", Source: "y", Score: 99},
		{ID: "b1", ClusterID: "B", Source: "x", Score: 50},
		{ID: "c1", ClusterID: "C", Source: "x", Score: 40},
	}
	out := Rerank(items)
	for i := 1; i < len(out); i++ {
		if out[i].ClusterID == out[i-1].ClusterID {
			t.Fatalf("adjacent items share cluster at %d: %s,%s", i, out[i-1].ID, out[i].ID)
		}
	}
	// Highest score first.
	if out[0].ID != "a1" {
		t.Fatalf("expected highest score first, got %s", out[0].ID)
	}
}

func TestRerankSourceFallback(t *testing.T) {
	// Only two clusters but multiple sources: adjacency must still differ in
	// cluster OR source at every boundary.
	items := []Item{
		{ID: "a1", ClusterID: "A", Source: "x", Score: 10},
		{ID: "a2", ClusterID: "A", Source: "y", Score: 9},
		{ID: "a3", ClusterID: "A", Source: "z", Score: 8},
		{ID: "b1", ClusterID: "B", Source: "x", Score: 7},
	}
	out := Rerank(items)
	for i := 1; i < len(out); i++ {
		if out[i].ClusterID == out[i-1].ClusterID && out[i].Source == out[i-1].Source {
			t.Fatalf("adjacent items identical in cluster and source at %d", i)
		}
	}
}

func TestRerankSingleClusterSingleSource(t *testing.T) {
	// Worst case: same cluster and source everywhere; nothing is dropped and
	// order is the deterministic score/ID order.
	items := []Item{
		{ID: "2", ClusterID: "A", Source: "x", Score: 5},
		{ID: "1", ClusterID: "A", Source: "x", Score: 5},
		{ID: "3", ClusterID: "A", Source: "x", Score: 10},
	}
	out := Rerank(items)
	if len(out) != 3 {
		t.Fatalf("expected 3 items, got %d", len(out))
	}
	if out[0].ID != "3" || out[1].ID != "1" || out[2].ID != "2" {
		t.Fatalf("unexpected order: %s %s %s", out[0].ID, out[1].ID, out[2].ID)
	}
	if Rerank(nil) != nil {
		t.Fatal("nil input should produce nil")
	}
}

func TestRerankManyClusters(t *testing.T) {
	// 50 items spread over 10 clusters: no adjacent equal clusters and all
	// items retained.
	var items []Item
	for i := 0; i < 50; i++ {
		items = append(items, Item{
			ID:        fmt.Sprintf("d%02d", i),
			ClusterID: fmt.Sprintf("c%d", i%10),
			Source:    "s",
			Score:     float64(50 - i),
		})
	}
	out := Rerank(items)
	if len(out) != 50 {
		t.Fatalf("expected 50 items got %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].ClusterID == out[i-1].ClusterID {
			t.Fatalf("adjacent cluster collision at %d", i)
		}
	}
}
