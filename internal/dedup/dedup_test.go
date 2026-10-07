package dedup

import (
	"testing"

	"github.com/guoyyit/search-result-dedup-go/internal/model"
	"github.com/guoyyit/search-result-dedup-go/internal/similarity"
)

func entry(h *similarity.Hasher, id, source, content string) Entry {
	set, _ := h.Shingles(content)
	return Entry{Doc: model.Document{ID: id, Source: source, Content: content}, Shingles: set}
}

func newTestClusterer(t *testing.T, threshold float64) (*Clusterer, *similarity.Hasher) {
	t.Helper()
	h := similarity.NewHasher(4)
	return NewClusterer(threshold, h), h
}

func TestNearDuplicateMerge(t *testing.T) {
	c, h := newTestClusterer(t, 0.6)
	if !c.Add(entry(h, "d1", "a", "the quick brown fox jumps over the lazy dog")) {
		t.Fatal("first add should succeed")
	}
	// Small edit: near duplicate.
	if !c.Add(entry(h, "d2", "a", "the quick brown fox jumpd over the lazy dog")) {
		t.Fatal("second add should succeed")
	}
	// Duplicate ID is rejected.
	if c.Add(entry(h, "d2", "a", "whatever")) {
		t.Fatal("duplicate ID add must fail")
	}
	// Unrelated document forms its own cluster.
	if !c.Add(entry(h, "d3", "a", "completely different news about stock markets today")) {
		t.Fatal("third add should succeed")
	}
	clusters := c.Clusters()
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	cl, ok := c.ClusterOf("d1")
	if !ok {
		t.Fatal("d1 missing")
	}
	if cl.Representative != "d1" {
		t.Fatalf("representative = %s, want d1 (oldest member)", cl.Representative)
	}
	if len(cl.MemberIDs) != 2 || cl.MemberIDs[0] != "d1" || cl.MemberIDs[1] != "d2" {
		t.Fatalf("members = %v, want [d1 d2]", cl.MemberIDs)
	}
	if c.Len() != 3 {
		t.Fatalf("len = %d, want 3", c.Len())
	}
}

func TestTransitiveMergeOfClusters(t *testing.T) {
	c, h := newTestClusterer(t, 0.34)
	// A and C are each similar to B but not to each other; inserting order
	// forces them into separate clusters first, then a bridge merges all.
	c.Add(entry(h, "a", "s", "alpha beta gamma delta epsilon zeta eta theta"))
	c.Add(entry(h, "c", "s", "iota kappa lambda mu alpha beta gamma delta"))
	// b shares alpha..delta with both.
	c.Add(entry(h, "b", "s", "alpha beta gamma delta eta theta iota kappa lambda"))
	clusters := c.Clusters()
	if len(clusters) != 1 {
		t.Fatalf("bridge should merge into 1 cluster, got %d: %+v", len(clusters), clusters)
	}
	if clusters[0].Representative != "a" {
		t.Fatalf("representative = %s, want oldest a", clusters[0].Representative)
	}
}

func TestClusterSplitAfterBridgeRemoval(t *testing.T) {
	c, h := newTestClusterer(t, 0.45)
	c.Add(entry(h, "a", "s", "alpha beta gamma delta epsilon one two three"))
	c.Add(entry(h, "c", "s", "four five six seven alpha beta gamma delta"))
	c.Add(entry(h, "b", "s", "alpha beta gamma delta one two three four five six seven"))
	if len(c.Clusters()) != 1 {
		t.Fatal("expected a single merged cluster before split")
	}
	if !c.Remove("b") {
		t.Fatal("remove bridge should succeed")
	}
	if c.Remove("b") {
		t.Fatal("removing again must report false")
	}
	clusters := c.Clusters()
	if len(clusters) != 2 {
		t.Fatalf("after bridge removal want 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	for _, cl := range clusters {
		if len(cl.MemberIDs) != 1 {
			t.Fatalf("cluster %s should be singleton, members=%v", cl.ID, cl.MemberIDs)
		}
	}
}

func TestRemovingNonRepresentativeKeepsRepresentative(t *testing.T) {
	c, h := newTestClusterer(t, 0.6)
	c.Add(entry(h, "d1", "s", "the quick brown fox jumps over the lazy dog"))
	c.Add(entry(h, "d2", "s", "the quick brown fox jumps over the lazy do"))
	c.Add(entry(h, "d3", "s", "the quick brown fox jumps over the lazy dg"))
	cl, _ := c.ClusterOf("d1")
	if cl.Representative != "d1" || len(cl.MemberIDs) != 3 {
		t.Fatalf("setup wrong: %+v", cl)
	}
	// Unrelated add/remove must not touch this cluster's representative.
	c.Add(entry(h, "x", "s", "totally different topic entirely here"))
	c.Remove("x")
	c.Remove("d2")
	cl, _ = c.ClusterOf("d1")
	if cl.Representative != "d1" {
		t.Fatalf("representative jittered to %s", cl.Representative)
	}
	if len(cl.MemberIDs) != 2 {
		t.Fatalf("members = %v, want [d1 d3]", cl.MemberIDs)
	}
}

func TestRemovingRepresentativeAdvancesStably(t *testing.T) {
	c, h := newTestClusterer(t, 0.6)
	c.Add(entry(h, "d1", "s", "the quick brown fox jumps over the lazy dog"))
	c.Add(entry(h, "d2", "s", "the quick brown fox jumps over the lazy do"))
	c.Remove("d1")
	cl, ok := c.ClusterOf("d2")
	if !ok {
		t.Fatal("d2 should still exist")
	}
	if cl.Representative != "d2" {
		t.Fatalf("representative = %s, want d2", cl.Representative)
	}
	if _, ok := c.ClusterOf("d1"); ok {
		t.Fatal("removed d1 should be gone")
	}
}

func TestUpsertPreservesRepresentativeOrder(t *testing.T) {
	c, h := newTestClusterer(t, 0.6)
	c.Add(entry(h, "d1", "s", "the quick brown fox jumps over the lazy dog"))
	c.Add(entry(h, "d2", "s", "the quick brown fox jumps over the lazy do"))
	// Refresh d2's content while still a near-duplicate.
	added := c.Upsert(entry(h, "d2", "s", "the quick brown fox jumps over the lazy dog!"))
	if added {
		t.Fatal("upsert of existing id should report added=false")
	}
	cl, _ := c.ClusterOf("d1")
	if cl.Representative != "d1" {
		t.Fatalf("representative changed after upsert: %s", cl.Representative)
	}
	// A new id via Upsert is an addition.
	if !c.Upsert(entry(h, "d9", "s", "unrelated unique content about space rockets")) {
		t.Fatal("upsert of new id should report added=true")
	}
}

func TestClusterIDDeterminism(t *testing.T) {
	if ClusterIDOf("abc") != "c:abc" {
		t.Fatalf("cluster id format wrong: %s", ClusterIDOf("abc"))
	}
}
