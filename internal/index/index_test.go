package index

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/guoyyit/search-result-dedup-go/internal/model"
)

func openTestIndex(t *testing.T, threshold float64) *Index {
	t.Helper()
	ix, err := Open(Config{
		DataDir:             t.TempDir(),
		SimilarityThreshold: threshold,
		ShingleK:            4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func doc(id, src, content string, score float64) model.Document {
	return model.Document{ID: id, Source: src, Title: id, Content: content, Score: score}
}

func reps(ix *Index) []string {
	rvs := ix.Representatives()
	out := make([]string, len(rvs))
	for i, rv := range rvs {
		out[i] = rv.Doc.ID
	}
	return out
}

func TestAddDeleteUpsertBasic(t *testing.T) {
	ix := openTestIndex(t, 0.6)
	added, err := ix.Add(doc("d1", "a", "the quick brown fox jumps over the lazy dog", 1))
	if err != nil || !added {
		t.Fatalf("add d1: added=%v err=%v", added, err)
	}
	if added, _ := ix.Add(doc("d1", "a", "duplicate id content", 1)); added {
		t.Fatal("duplicate add must report false")
	}
	changed, err := ix.Upsert(doc("d1", "a", "the quick brown fox jumps over the lazy dog", 1))
	if err != nil || changed {
		t.Fatalf("identical upsert must be no-op: changed=%v err=%v", changed, err)
	}
	changed, err = ix.Upsert(doc("d1", "a", "the quick brown fox jumps over the lazy dog!!", 1))
	if err != nil || !changed {
		t.Fatalf("changed-content upsert: changed=%v err=%v", changed, err)
	}
	if got, ok := ix.Get("d1"); !ok || got.Content != "the quick brown fox jumps over the lazy dog!!" {
		t.Fatalf("get after upsert wrong: %+v ok=%v", got, ok)
	}
	if removed, _ := ix.Delete("missing"); removed {
		t.Fatal("deleting missing id must report false")
	}
	if removed, _ := ix.Delete("d1"); !removed {
		t.Fatal("delete d1 must report true")
	}
	if ix.Len() != 0 {
		t.Fatalf("len after delete = %d", ix.Len())
	}
}

func TestRepresentativesAreMerged(t *testing.T) {
	ix := openTestIndex(t, 0.7)
	ix.Add(doc("d1", "a", "the quick brown fox jumps over the lazy dog", 10))
	ix.Add(doc("d2", "b", "the quick brown fox jumps over the lazy do", 9))
	ix.Add(doc("d3", "a", "completely different news about finance markets", 8))
	rvs := ix.Representatives()
	if len(rvs) != 2 {
		t.Fatalf("want 2 reps, got %d: %v", len(rvs), reps(ix))
	}
	// Representatives are ordered by doc ID.
	if rvs[0].Doc.ID != "d1" {
		t.Fatalf("first rep = %s, want d1", rvs[0].Doc.ID)
	}
	var merged []string
	for _, m := range rvs[0].Cluster.MemberIDs {
		if m != "d1" {
			merged = append(merged, m)
		}
	}
	if len(merged) != 1 || merged[0] != "d2" {
		t.Fatalf("merged ids = %v, want [d2]", merged)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	ix := openTestIndex(t, 0.99)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("g%d-d%d", g, i)
				content := fmt.Sprintf("group %d unique document number %d about topic %d", g, i, i%5)
				if _, err := ix.Upsert(doc(id, "src", content, float64(i))); err != nil {
					t.Errorf("upsert: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if ix.Len() != 200 {
		t.Fatalf("len = %d, want 200", ix.Len())
	}
	// Re-reading reps concurrently with more writes must stay consistent.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = ix.Representatives()
			}
		}
	}()
	var wg2 sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg2.Add(1)
		go func(g int) {
			defer wg2.Done()
			for i := 0; i < 25; i++ {
				_, _ = ix.Upsert(doc(fmt.Sprintf("g%d-d%d", g, i), "src",
					fmt.Sprintf("group %d updated content %d different wording now", g, i), 1))
			}
		}(g)
	}
	wg2.Wait()
	close(done)
}

func TestContentSizeLimit(t *testing.T) {
	dir := t.TempDir()
	ix, err := Open(Config{
		DataDir:             dir,
		SimilarityThreshold: 0.8,
		ShingleK:            4,
		MaxContentBytes:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	big := doc("big", "s", "this content is definitely longer than ten bytes", 1)
	if _, err := ix.Add(big); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("want ErrContentTooLarge, got %v", err)
	}
	if _, err := ix.Upsert(big); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("upsert want ErrContentTooLarge, got %v", err)
	}
	if ix.Len() != 0 {
		t.Fatal("oversized doc must not be indexed")
	}
	if _, err := ix.Add(doc("ok", "s", "short", 1)); err != nil {
		t.Fatalf("small doc should be accepted: %v", err)
	}
	if _, err := ix.Add(doc("", "s", "x", 1)); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("want ErrEmptyID, got %v", err)
	}
}

func TestRecoveryEquivalence(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir, SimilarityThreshold: 0.55, ShingleK: 4}

	mutate := func(ix *Index) {
		ix.Add(doc("d1", "a", "the quick brown fox jumps over the lazy dog today", 10))
		ix.Add(doc("d2", "b", "the quick brown fox jumps over the lazy do today", 9))
		ix.Add(doc("d3", "a", "a completely different story about stock markets", 8))
		ix.Add(doc("d4", "a", "the quick brown fox jumps over lazy dogs today yes", 7))
		ix.Delete("d3")
		ix.Upsert(doc("d2", "b", "the quick brown fox jumps over the lazy dog today now", 9))
		ix.Add(doc("d5", "c", "stock market news and finance report quarterly earnings", 6))
	}

	ix1, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mutate(ix1)
	before := ix1.Representatives()
	if err := ix1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: replay must reconstruct the same docs and clusters.
	ix2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := ix2.Representatives()
	if !repViewsEqual(before, after) {
		t.Fatalf("recovery mismatch:\nbefore=%v\nafter =%v", dumpReps(before), dumpReps(after))
	}
	if ix2.Len() != 4 {
		t.Fatalf("recovered len = %d, want 4", ix2.Len())
	}

	// And a third reopen must be stable too (idempotent recovery).
	ix2.Close()
	ix3, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ix3.Close()
	third := ix3.Representatives()
	if !repViewsEqual(after, third) {
		t.Fatalf("second recovery mismatch:\n%v\n%v", dumpReps(after), dumpReps(third))
	}
}

func repViewsEqual(a, b []RepView) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Doc != b[i].Doc {
			return false
		}
		if a[i].Cluster.ID != b[i].Cluster.ID ||
			a[i].Cluster.Representative != b[i].Cluster.Representative ||
			len(a[i].Cluster.MemberIDs) != len(b[i].Cluster.MemberIDs) {
			return false
		}
		for j := range a[i].Cluster.MemberIDs {
			if a[i].Cluster.MemberIDs[j] != b[i].Cluster.MemberIDs[j] {
				return false
			}
		}
	}
	return true
}

func dumpReps(rs []RepView) []string {
	var out []string
	for _, rv := range rs {
		out = append(out, fmt.Sprintf("%s(%v)", rv.Doc.ID, rv.Cluster.MemberIDs))
	}
	return out
}
