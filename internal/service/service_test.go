package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/search-result-dedup-go/internal/index"
	"github.com/guoyyit/search-result-dedup-go/internal/limits"
	"github.com/guoyyit/search-result-dedup-go/internal/model"
)

func newSvc(t *testing.T, dir string, threshold float64, lim limits.Limits) (*Service, *index.Index) {
	t.Helper()
	ix, err := index.Open(index.Config{
		DataDir:             dir,
		SimilarityThreshold: threshold,
		ShingleK:            4,
		MaxContentBytes:     lim.MaxContentBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return New(ix, lim), ix
}

func mdoc(id, src, content string, score float64) model.Document {
	return model.Document{ID: id, Source: src, Title: id, Content: content, Score: score}
}

// nearDuplicateTexts returns n near-duplicate texts for base sentence.
func nearDuplicateTexts(base string, n int) []string {
	out := []string{base}
	for i := 1; i < n; i++ {
		out = append(out, fmt.Sprintf("%s v%d", base, i))
	}
	return out
}

func TestSearchMergesAndTraces(t *testing.T) {
	lim := limits.Default()
	svc, ix := newSvc(t, t.TempDir(), 0.6, lim)
	base := "the quick brown fox jumps over the lazy dog on monday morning"
	for i, text := range nearDuplicateTexts(base, 4) {
		id := fmt.Sprintf("d%d", i)
		if _, err := ix.Add(mdoc(id, "web", text, float64(10-i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Add(mdoc("other", "news", "a totally different article about finance and stocks today", 5)); err != nil {
		t.Fatal(err)
	}

	page, err := svc.Search(context.Background(), Query{Text: "", PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2 (one merged cluster + one other)", page.Total)
	}
	var rep Item
	var other Item
	for _, it := range page.Items {
		if it.ID == "d0" {
			rep = it
		}
		if it.ID == "other" {
			other = it
		}
	}
	if rep.ID == "" || other.ID == "" {
		t.Fatalf("expected d0 and other, got %+v", page.Items)
	}
	if len(rep.MergedIDs) != 3 {
		t.Fatalf("merged ids = %v, want d1 d2 d3", rep.MergedIDs)
	}
	seen := map[string]bool{}
	for _, m := range rep.MergedIDs {
		seen[m] = true
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		if !seen[id] {
			t.Fatalf("merged ids missing %s: %v", id, rep.MergedIDs)
		}
	}
}

func TestPaginationStableUnderConcurrentUpdates(t *testing.T) {
	lim := limits.Default()
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)

	// 50 distinct documents.
	for i := 0; i < 50; i++ {
		if _, err := ix.Add(mdoc(fmt.Sprintf("doc%03d", i), fmt.Sprintf("src%d", i%3),
			fmt.Sprintf("unique article number %d covers topic %d details here", i, i%7),
			float64(50-i))); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	page1, err := svc.Search(ctx, Query{Text: "", PageSize: 7})
	if err != nil {
		t.Fatal(err)
	}
	if page1.Total != 50 {
		t.Fatalf("total = %d, want 50", page1.Total)
	}

	// Hammer the index while paging through the frozen snapshot.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("new%d-%d", g, i)
				_, _ = ix.Upsert(mdoc(id, "srcX",
					fmt.Sprintf("freshly added concurrent document %d stream %d words", i, g), 100))
				if i%3 == 0 && i > 0 {
					_, _ = ix.Delete(id)
				}
				i++
			}
		}(g)
	}

	collected := append([]Item(nil), page1.Items...)
	cursor := page1.NextCursor
	pageCount := 1
	for cursor != "" {
		time.Sleep(2 * time.Millisecond)
		p, err := svc.ReadPage(ctx, cursor, 7)
		if err != nil {
			t.Fatalf("read page %d: %v", pageCount+1, err)
		}
		collected = append(collected, p.Items...)
		if p.Total != 50 {
			t.Fatalf("snapshot total changed to %d on page %d", p.Total, pageCount+1)
		}
		cursor = p.NextCursor
		pageCount++
	}
	close(stop)
	wg.Wait()

	if len(collected) != 50 {
		t.Fatalf("collected %d items, want 50", len(collected))
	}
	ids := map[string]int{}
	for _, it := range collected {
		ids[it.ID]++
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("item %s returned %d times", id, n)
		}
	}
	for i := 0; i < 50; i++ {
		if ids[fmt.Sprintf("doc%03d", i)] != 1 {
			t.Fatalf("missing document %03d", i)
		}
	}
	if len(page1.Items) > 1 {
		if page1.Items[0].ClusterID == page1.Items[1].ClusterID {
			// With distinct docs each has its own cluster, so this must hold.
			t.Fatal("adjacent page items share cluster")
		}
	}
}

func TestInvalidCursorAndPageSize(t *testing.T) {
	lim := limits.Default()
	svc, _ := newSvc(t, t.TempDir(), 0.9, lim)
	ctx := context.Background()
	if _, err := svc.Search(ctx, Query{PageSize: 0}); !errors.Is(err, ErrPageSize) {
		t.Fatalf("want ErrPageSize, got %v", err)
	}
	if _, err := svc.ReadPage(ctx, "!!!not-base64!!!", 5); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("want ErrInvalidCursor, got %v", err)
	}
	if _, err := svc.ReadPage(ctx, "eyJzIjoieCJ9", 0); !errors.Is(err, ErrPageSize) {
		t.Fatalf("want ErrPageSize, got %v", err)
	}
}

func TestSnapshotTTLExpiry(t *testing.T) {
	lim := limits.Default()
	lim.MaxSnapshots = 4
	lim.SnapshotTTL = time.Minute
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clock }

	for i, content := range []string{
		"alpha document content word word word",
		"beta document content word word word",
		"gamma document content word word word",
	} {
		ix.Add(mdoc(fmt.Sprintf("d%d", i), "s", content, 1))
	}
	p, err := svc.Search(context.Background(), Query{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.NextCursor == "" {
		t.Fatal("expected a cursor")
	}
	// Advance past TTL.
	clock = clock.Add(2 * time.Minute)
	if _, err := svc.ReadPage(context.Background(), p.NextCursor, 1); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("want ErrSnapshotExpired after TTL, got %v", err)
	}
}

func TestSnapshotLRUEviction(t *testing.T) {
	lim := limits.Default()
	lim.MaxSnapshots = 2
	lim.SnapshotTTL = time.Hour
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	for i, content := range []string{
		"alpha content here distinct words x",
		"beta content here distinct words y",
		"gamma content here distinct words z",
	} {
		ix.Add(mdoc(fmt.Sprintf("d%d", i), "s", content, 1))
	}

	first, _ := svc.Search(context.Background(), Query{PageSize: 1})
	second, _ := svc.Search(context.Background(), Query{PageSize: 1})
	// Touching the first snapshot makes it MRU; third insertion must then
	// evict the untouched second snapshot, not the first.
	if _, err := svc.ReadPage(context.Background(), first.NextCursor, 1); err != nil {
		t.Fatal(err)
	}
	svc.Search(context.Background(), Query{PageSize: 1})
	// Untouched oldest snapshot must be gone.
	if _, err := svc.ReadPage(context.Background(), second.NextCursor, 1); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("untouched LRU snapshot should be evicted, got %v", err)
	}
	// Recently touched snapshot must survive.
	if _, err := svc.ReadPage(context.Background(), first.NextCursor, 1); err != nil {
		t.Fatalf("LRU-recent snapshot must survive, got %v", err)
	}
}

func TestTooManyConcurrentQueries(t *testing.T) {
	lim := limits.Default()
	lim.MaxConcurrentQueries = 1
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	for i := 0; i < 10; i++ {
		ix.Add(mdoc(fmt.Sprintf("d%d", i), "s",
			fmt.Sprintf("distinct content block %d with words about topic %d", i, i), 1))
	}
	// Occupy the single slot by parking on a blocked send; issue queries
	// through a helper since acquire slots are internal.
	svc.sem <- struct{}{}
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.Search(context.Background(), Query{PageSize: 5})
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTooManyConcurrent) {
			t.Fatalf("want ErrTooManyConcurrent, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query blocked instead of failing fast")
	}
	<-svc.sem
	if _, err := svc.Search(context.Background(), Query{PageSize: 5}); err != nil {
		t.Fatalf("after slot release query should succeed, got %v", err)
	}
}

func TestRerankLimitDegradesDeterministically(t *testing.T) {
	lim := limits.Default()
	lim.MaxCandidates = 1000
	lim.MaxRerankItems = 5
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	for i := 0; i < 20; i++ {
		ix.Add(mdoc(fmt.Sprintf("d%02d", i), "s",
			fmt.Sprintf("entirely unique content number %d topic word %d distinct", i, i),
			float64(20-i)))
	}
	p, err := svc.Search(context.Background(), Query{PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Degraded || p.Total != 5 {
		t.Fatalf("want degraded snapshot of 5, got degraded=%v total=%d", p.Degraded, p.Total)
	}
	found := false
	for _, w := range p.Warnings {
		if w == WarnRerankLimited {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %s", p.Warnings, WarnRerankLimited)
	}
	// Highest scores survive the deterministic cut, and no duplicates
	// across pages.
	got := map[string]bool{}
	cursor := ""
	first := true
	for {
		var pg *Page
		if first {
			pg, err = svc.Search(context.Background(), Query{PageSize: 3})
			if err != nil {
				t.Fatal(err)
			}
			first = false
		} else {
			pg, err = svc.ReadPage(context.Background(), cursor, 3)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, it := range pg.Items {
			if got[it.ID] {
				t.Fatalf("duplicate across pages: %s", it.ID)
			}
			got[it.ID] = true
		}
		cursor = pg.NextCursor
		if cursor == "" {
			break
		}
	}
	for i := 0; i < 5; i++ {
		if !got[fmt.Sprintf("d%02d", i)] {
			t.Fatalf("top-5 item d%02d missing after cutoff", i)
		}
	}
}

func TestCandidateLimit(t *testing.T) {
	lim := limits.Default()
	lim.MaxCandidates = 10
	lim.MaxRerankItems = 1000
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	for i := 0; i < 50; i++ {
		ix.Add(mdoc(fmt.Sprintf("d%03d", i), "s",
			fmt.Sprintf("separate unique text entry %d about theme %d specifics", i, i), 1))
	}
	p, err := svc.Search(context.Background(), Query{PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Degraded || p.Total != 10 {
		t.Fatalf("want degraded 10-item page, got degraded=%v total=%d", p.Degraded, p.Total)
	}
	hasWarn := false
	for _, w := range p.Warnings {
		if w == WarnCandidatesLimited {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Fatalf("missing %s warning: %v", WarnCandidatesLimited, p.Warnings)
	}
}

func TestRecoverySameQueryOutput(t *testing.T) {
	dir := t.TempDir()
	threshold := 0.6
	lim := limits.Default()

	seed := func(ix *index.Index) {
		texts := []string{
			"breaking news: local team wins championship game last night in final seconds",
			"breaking news: local team wins championship game last night in final second",
			"weather forecast: heavy rain expected throughout the weekend ahead stay home",
			"weather forecast: heavy rains expected throughout the weekend ahead stay homes",
			"technology release: new processor doubles performance while cutting power draw",
			"market update: stocks closed mixed as investors awaited the earnings season",
		}
		for i, text := range texts {
			if _, err := ix.Add(mdoc(fmt.Sprintf("doc%d", i), fmt.Sprintf("src%d", i%2), text, float64(100-i))); err != nil {
				t.Fatal(err)
			}
		}
	}

	query := Query{Text: "", PageSize: 2}
	run := func() []Item {
		ix, err := index.Open(index.Config{DataDir: dir, SimilarityThreshold: threshold, ShingleK: 4})
		if err != nil {
			t.Fatal(err)
		}
		s := New(ix, lim)
		var all []Item
		p, err := s.Search(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p.Items...)
		for p.NextCursor != "" {
			p, err = s.ReadPage(context.Background(), p.NextCursor, 2)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, p.Items...)
		}
		s.Close()
		ix.Close()
		return all
	}

	ix0, err := index.Open(index.Config{DataDir: dir, SimilarityThreshold: threshold, ShingleK: 4})
	if err != nil {
		t.Fatal(err)
	}
	seed(ix0)
	ix0.Close()

	first := run()
	second := run()
	if len(first) != len(second) {
		t.Fatalf("recovered run length differs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.ID != b.ID || a.ClusterID != b.ClusterID || a.Source != b.Source ||
			len(a.MergedIDs) != len(b.MergedIDs) {
			t.Fatalf("position %d differs after recovery:\n%+v\n%+v", i, a, b)
		}
		for j := range a.MergedIDs {
			if a.MergedIDs[j] != b.MergedIDs[j] {
				t.Fatalf("merged ids at %d differ: %v vs %v", i, a.MergedIDs, b.MergedIDs)
			}
		}
	}
}

func TestTextQueryAndSourceFilter(t *testing.T) {
	lim := limits.Default()
	svc, ix := newSvc(t, t.TempDir(), 0.99, lim)
	ix.Add(mdoc("a", "web", "rust programming language guide for systems developers", 5))
	ix.Add(mdoc("b", "web", "go programming language tutorial for backend developers", 5))
	ix.Add(mdoc("c", "news", "cooking pasta: an italian recipe with tomato sauce", 5))

	p, err := svc.Search(context.Background(), Query{Text: "programming", PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 2 {
		t.Fatalf("text query total = %d, want 2", p.Total)
	}
	p, err = svc.Search(context.Background(), Query{Text: "programming", Sources: []string{"news"}, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 0 {
		t.Fatalf("source-filtered total = %d, want 0", p.Total)
	}
	p, err = svc.Search(context.Background(), Query{Text: "programming language", Sources: []string{"web"}, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 2 {
		t.Fatalf("multi-term total = %d, want 2", p.Total)
	}
}
