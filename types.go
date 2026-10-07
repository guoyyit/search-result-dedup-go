// Package dedup provides a local near-duplicate merging and diversity
// reranking service for search result sets.
package dedup

import (
	"errors"
	"time"
)

// Item is a single indexable search result.
type Item struct {
	ID        string
	Source    string
	Title     string
	Content   string
	Score     float64
	UpdatedAt time.Time
}

// Entry is one returned result: the representative item of a cluster plus
// the IDs of the items that were merged into it within the result set.
type Entry struct {
	Item      Item
	ClusterID string
	MergedIDs []string
}

// Cursor is an opaque pagination cursor bound to an immutable snapshot.
type Cursor struct {
	SnapshotID string `json:"sid"`
	Offset     int    `json:"off"`
}

// Page is one page of search results.
type Page struct {
	Entries    []Entry
	NextCursor *Cursor
	HasMore    bool
	// Degraded lists deterministic degradation reasons triggered by
	// resource limits (see Degrade* constants).
	Degraded []string
}

// Degradation reasons.
const (
	DegradeCandidatesTruncated = "candidates_truncated"
	DegradeSimilarityAborted   = "similarity_scan_aborted"
	DegradeRerankSkipped       = "rerank_skipped"
	DegradeResultsTruncated    = "results_truncated"
)

// Config configures similarity, reranking, pagination, persistence and
// resource limits. Zero values are replaced by sane defaults in New.
type Config struct {
	// SimilarityThreshold in [0,1]; pairs at or above it are merged.
	SimilarityThreshold float64
	// ShingleSize is the word n-gram size used for Jaccard similarity.
	ShingleSize int

	// MaxCandidates bounds how many matched items enter the merge pipeline.
	// When exceeded, only the top MaxCandidates by (score desc, id asc)
	// are used and DegradeCandidatesTruncated is reported.
	MaxCandidates int
	// MaxPairComparisons bounds pairwise similarity comparisons per query.
	// When exceeded, remaining pairs are treated as non-similar and
	// DegradeSimilarityAborted is reported.
	MaxPairComparisons int64
	// MaxRerankItems bounds the number of entries that are diversity
	// reranked; larger result sets keep their stable base order and
	// DegradeRerankSkipped is reported.
	MaxRerankItems int
	// MaxSnapshotItems bounds the materialized size of one snapshot.
	// Beyond it results are deterministically truncated and
	// DegradeResultsTruncated is reported.
	MaxSnapshotItems int
	// MaxSnapshots bounds how many pinned snapshots the service retains.
	MaxSnapshots int
	// SnapshotTTL bounds how long a pinned snapshot (and its cursors)
	// stays valid.
	SnapshotTTL time.Duration

	// PersistPath is the directory for the write-ahead log and snapshots.
	// Empty disables persistence.
	PersistPath string

	// Clock is overridable in tests; defaults to time.Now.
	Clock func() time.Time
}

var (
	// ErrInvalidCursor is returned for malformed/unknown cursors.
	ErrInvalidCursor = errors.New("dedup: invalid cursor")
	// ErrSnapshotExpired is returned when a cursor references a snapshot
	// that expired or was evicted; callers must restart the search.
	ErrSnapshotExpired = errors.New("dedup: snapshot expired")
	// ErrServiceClosed is returned after Close.
	ErrServiceClosed = errors.New("dedup: service closed")
)
