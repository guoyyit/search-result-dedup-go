// Package service exposes snapshot-based, cursor-paginated, diversity-reranked
// search over a durable near-duplicate index.
//
// Stability model
//
//   - Every Search materializes an immutable snapshot: after near-duplicate
//     merge and diversity rerank, the exact output permutation is stored
//     under a random snapshot ID.
//   - Cursors name (snapshot ID, offset). Concurrent index additions,
//     deletions and overwrites mutate only future snapshots; pages already
//     headed by a returned cursor never reorder, repeat or lose items.
//   - Snapshots are bounded by MaxSnapshots (LRU) and SnapshotTTL. An
//     evicted/expired cursor deterministically fails with ErrSnapshotExpired,
//     inviting the caller to restart at page one.
//
// Degradation (see package limits):
//
//   - candidate scan truncated at MaxCandidates,
//   - rerank input truncated at MaxRerankItems (warning rerank_items_limited),
//   - both conditions set Page.Degraded and add machine-readable warnings,
//   - exceeding MaxConcurrentQueries fails with ErrTooManyConcurrent.
package service

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/guoyyit/search-result-dedup-go/internal/diversity"
	"github.com/guoyyit/search-result-dedup-go/internal/index"
	"github.com/guoyyit/search-result-dedup-go/internal/limits"
	"github.com/guoyyit/search-result-dedup-go/internal/model"
	"github.com/guoyyit/search-result-dedup-go/internal/similarity"
)

// Sentinel errors describing deterministic degradation outcomes.
var (
	// ErrSnapshotExpired is returned when a cursor refers to a snapshot
	// that was evicted because of retention limits or TTL.
	ErrSnapshotExpired = errors.New("snapshot expired or evicted")
	// ErrTooManyConcurrent is returned when MaxConcurrentQueries is exceeded.
	ErrTooManyConcurrent = errors.New("too many concurrent queries")
	// ErrInvalidCursor is returned for malformed/foreign cursors.
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrPageSize is returned for an out-of-range page size.
	ErrPageSize = errors.New("invalid page size")
)

// Warning names reported in Page.Warnings.
const (
	WarnCandidatesLimited = "candidates_limited"
	WarnRerankLimited     = "rerank_items_limited"
)

// Query describes a search request.
type Query struct {
	// Text matches documents by query-term overlap; empty means all docs.
	Text string
	// Sources optionally restricts matches to these sources.
	Sources []string
	// PageSize is the number of items returned for the first page.
	PageSize int
}

// Item is one output result: a cluster representative plus traceability data.
type Item struct {
	ID        string   `json:"id"`
	Source    string   `json:"source"`
	Title     string   `json:"title,omitempty"`
	Score     float64  `json:"score"`
	ClusterID string   `json:"cluster_id"`
	MergedIDs []string `json:"merged_ids,omitempty"`
}

// Page is one stable slice of an immutable query snapshot.
type Page struct {
	Items      []Item   `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
	Total      int      `json:"total"`
	SnapshotID string   `json:"snapshot_id"`
	Degraded   bool     `json:"degraded,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// scored is an internal ranking element.
type scored struct {
	view  index.RepView
	score float64
}

// snapshot is an immutable, fully reranked result permutation.
type snapshot struct {
	id        string
	createdAt time.Time
	items     []Item
	degraded  bool
	warnings  []string
	elem      *list.Element // LRU position
}

// cursor is the opaque JSON payload carried by cursors.
type cursor struct {
	SnapID string `json:"s"`
	Offset int    `json:"o"`
}

// Service coordinates index snapshots, reranking and bounded paging.
type Service struct {
	idx    *index.Index
	limits limits.Limits
	now    func() time.Time

	mu     sync.Mutex
	snaps  map[string]*snapshot
	lru    *list.List
	sem    chan struct{}
	closed bool
}

// New creates a Service over idx with the given resource limits.
func New(idx *index.Index, lim limits.Limits) *Service {
	lim = lim.Normalize()
	return &Service{
		idx:    idx,
		limits: lim,
		now:    time.Now,
		snaps:  make(map[string]*snapshot),
		lru:    list.New(),
		sem:    make(chan struct{}, lim.MaxConcurrentQueries),
	}
}

// newSnapshotID returns a random hex snapshot identifier.
func newSnapshotID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is exceptional; fall back to time-based bytes.
		now := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			b[i] = byte(now >> (uint(i) * 4))
		}
	}
	return hex.EncodeToString(b[:])
}

// acquire takes a concurrency slot or fails deterministically.
func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrTooManyConcurrent
	}
}

func (s *Service) release() { <-s.sem }

// purgeExpired removes TTL-expired snapshots (lazy, bounded work per call).
func (s *Service) purgeExpired() {
	now := s.now()
	for e := s.lru.Back(); e != nil; {
		snap := e.Value.(*snapshot)
		if now.Sub(snap.createdAt) <= s.limits.SnapshotTTL {
			return // list is newest-at-front; older tail entries come first
		}
		prev := e.Prev()
		s.lru.Remove(e)
		delete(s.snaps, snap.id)
		e = prev
	}
}

// retain registers snap and evicts LRU entries past MaxSnapshots.
func (s *Service) retain(snap *snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpired()
	snap.elem = s.lru.PushFront(snap)
	s.snaps[snap.id] = snap
	for s.lru.Len() > s.limits.MaxSnapshots {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		victim := oldest.Value.(*snapshot)
		s.lru.Remove(oldest)
		delete(s.snaps, victim.id)
	}
}

// touch moves an existing snapshot to the LRU front and re-checks its TTL.
func (s *Service) touch(id string) (*snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpired()
	snap, ok := s.snaps[id]
	if !ok {
		return nil, ErrSnapshotExpired
	}
	s.lru.MoveToFront(snap.elem)
	return snap, nil
}

// matchTerms returns the fraction [0,1] of distinct query terms present in
// the document token set. found is whether at least one term matched.
func matchTerms(queryTerms []string, docTokens map[string]struct{}) (frac float64, found bool) {
	if len(queryTerms) == 0 {
		return 1, true
	}
	hit := 0
	for _, t := range queryTerms {
		if _, ok := docTokens[t]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(queryTerms)), hit > 0
}

// buildSnapshot performs retrieval, merge-aware ranking and diversity rerank.
func (s *Service) buildSnapshot(q Query) *snapshot {
	reps := s.idx.Representatives() // deterministic: representative ID order
	queryTerms := uniqueTokens(similarity.Tokenize(q.Text))
	sourceSet := make(map[string]struct{}, len(q.Sources))
	for _, src := range q.Sources {
		sourceSet[src] = struct{}{}
	}

	var (
		matched       []scored
		scanned       int
		candidatesCut bool
	)
	for _, rv := range reps {
		if scanned >= s.limits.MaxCandidates {
			candidatesCut = true
			break
		}
		scanned++
		if len(sourceSet) > 0 {
			if _, ok := sourceSet[rv.Doc.Source]; !ok {
				continue
			}
		}
		docTokens := tokenSet(rv.Doc.Title + " " + rv.Doc.Content)
		frac, found := matchTerms(queryTerms, docTokens)
		if !found {
			continue
		}
		// Rank score: query relevance for text queries; caller score for
		// a browse (empty query). The secondary key is the caller's score,
		// then ID (applied below).
		rankScore := rv.Doc.Score
		if len(queryTerms) > 0 {
			rankScore = frac
		}
		matched = append(matched, scored{view: rv, score: rankScore})
	}

	sortMatched(matched, queryTerms)

	warnings := make([]string, 0, 2)
	if candidatesCut {
		warnings = append(warnings, WarnCandidatesLimited)
	}
	rerankCut := false
	if len(matched) > s.limits.MaxRerankItems {
		matched = matched[:s.limits.MaxRerankItems]
		rerankCut = true
		warnings = append(warnings, WarnRerankLimited)
	}

	items := make([]diversity.Item, len(matched))
	for i, m := range matched {
		items[i] = diversity.Item{
			ID:        m.view.Doc.ID,
			ClusterID: m.view.Cluster.ID,
			Source:    m.view.Doc.Source,
			Score:     m.score,
		}
	}
	order := diversity.Rerank(items)

	byID := make(map[string]scored, len(matched))
	for _, m := range matched {
		byID[m.view.Doc.ID] = m
	}
	out := make([]Item, 0, len(order))
	for _, di := range order {
		m := byID[di.ID]
		members := m.view.Cluster.MemberIDs
		merged := make([]string, 0, len(members)-1)
		for _, mid := range members {
			if mid != di.ID {
				merged = append(merged, mid)
			}
		}
		out = append(out, Item{
			ID:        di.ID,
			Source:    di.Source,
			Title:     m.view.Doc.Title,
			Score:     di.Score,
			ClusterID: di.ClusterID,
			MergedIDs: merged,
		})
	}
	return &snapshot{
		id:        newSnapshotID(),
		createdAt: s.now(),
		items:     out,
		degraded:  candidatesCut || rerankCut,
		warnings:  warnings,
	}
}

// sortMatched applies the deterministic ranking order.
func sortMatched(matched []scored, queryTerms []string) {
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.view.Doc.Score != b.view.Doc.Score {
			return a.view.Doc.Score > b.view.Doc.Score
		}
		return a.view.Doc.ID < b.view.Doc.ID
	})
}

func uniqueTokens(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := tokens[:0]
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func tokenSet(text string) map[string]struct{} {
	toks := similarity.Tokenize(text)
	set := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		set[t] = struct{}{}
	}
	return set
}

// pageOf slices a snapshot and assembles the Page value for an offset.
func pageOf(snap *snapshot, offset, size int) *Page {
	end := offset + size
	if end > len(snap.items) {
		end = len(snap.items)
	}
	page := &Page{
		Items:      append([]Item(nil), snap.items[offset:end]...),
		Total:      len(snap.items),
		SnapshotID: snap.id,
		Degraded:   snap.degraded,
		Warnings:   append([]string(nil), snap.warnings...),
	}
	if end < len(snap.items) {
		page.NextCursor = encodeCursor(cursor{SnapID: snap.id, Offset: end})
	}
	return page
}

// Search runs a query, materializing an immutable ordered snapshot and
// returning its first page.
func (s *Service) Search(ctx context.Context, q Query) (*Page, error) {
	if q.PageSize <= 0 {
		return nil, ErrPageSize
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap := s.buildSnapshot(q)
	s.retain(snap)
	return pageOf(snap, 0, q.PageSize), nil
}

// ReadPage returns the page following a cursor within the same snapshot.
func (s *Service) ReadPage(ctx context.Context, cur string, size int) (*Page, error) {
	if size <= 0 {
		return nil, ErrPageSize
	}
	c, err := decodeCursor(cur)
	if err != nil {
		return nil, err
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap, err := s.touch(c.SnapID)
	if err != nil {
		return nil, err
	}
	if c.Offset < 0 || c.Offset >= len(snap.items) {
		return nil, ErrInvalidCursor
	}
	return pageOf(snap, c.Offset, size), nil
}

func encodeCursor(c cursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, ErrInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.SnapID == "" {
		return cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// Close releases retained snapshots. The underlying index is not closed.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.snaps = make(map[string]*snapshot)
	s.lru.Init()
	return nil
}

// --- thin write pass-throughs so callers can use Service as the only API ---

// Add delegates to the underlying index.
func (s *Service) Add(doc model.Document) (bool, error) { return s.idx.Add(doc) }

// Upsert delegates to the underlying index.
func (s *Service) Upsert(doc model.Document) (bool, error) { return s.idx.Upsert(doc) }

// Delete delegates to the underlying index.
func (s *Service) Delete(id string) (bool, error) { return s.idx.Delete(id) }
