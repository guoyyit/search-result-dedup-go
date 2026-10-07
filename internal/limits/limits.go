// Package limits defines bounded resource usage and deterministic
// degradation behaviour for the service.
//
// Degradation policy
//
//   - MaxCandidates: retrieval stops after this many candidates; later
//     candidates (in deterministic retrieval order) are not considered.
//   - MaxRerankItems: at most this many representatives enter the
//     diversity reranker; excess items are cut by score desc, ID asc
//     before reranking. The response carries Degraded=true plus a warning.
//   - MaxConcurrentQueries: once reached, new requests fail immediately
//     with ErrTooManyConcurrent instead of queueing (no unbounded goroutines).
//   - MaxContentBytes: oversized document writes fail deterministically
//     (they are never indexed).
//   - MaxSnapshots / SnapshotTTL: page snapshots are an LRU+TTL cache;
//     evicted cursors fail with ErrSnapshotExpired, telling the caller to
//     restart paging from page one.
package limits

import "time"

// Limits caps CPU/memory/concurrency usage of query processing.
// Zero fields are replaced by Default values via Normalize.
type Limits struct {
	// MaxCandidates bounds the number of retrieved raw candidates
	// considered for a single query.
	MaxCandidates int
	// MaxRerankItems bounds the number of items handed to diversity reranking.
	MaxRerankItems int
	// MaxConcurrentQueries bounds simultaneously executing queries.
	MaxConcurrentQueries int
	// MaxContentBytes bounds the accepted byte size of one document content.
	MaxContentBytes int
	// MaxSnapshots bounds retained query snapshots.
	MaxSnapshots int
	// SnapshotTTL bounds how long a paging snapshot stays readable.
	SnapshotTTL time.Duration
}

// Default returns conservative production-ish defaults.
func Default() Limits {
	return Limits{
		MaxCandidates:        10_000,
		MaxRerankItems:       2_000,
		MaxConcurrentQueries: 64,
		MaxContentBytes:      1 << 20, // 1 MiB
		MaxSnapshots:         1024,
		SnapshotTTL:          30 * time.Minute,
	}
}

// Normalize replaces zero/negative fields with defaults and returns l.
func (l Limits) Normalize() Limits {
	d := Default()
	if l.MaxCandidates <= 0 {
		l.MaxCandidates = d.MaxCandidates
	}
	if l.MaxRerankItems <= 0 {
		l.MaxRerankItems = d.MaxRerankItems
	}
	if l.MaxConcurrentQueries <= 0 {
		l.MaxConcurrentQueries = d.MaxConcurrentQueries
	}
	if l.MaxContentBytes <= 0 {
		l.MaxContentBytes = d.MaxContentBytes
	}
	if l.MaxSnapshots <= 0 {
		l.MaxSnapshots = d.MaxSnapshots
	}
	if l.SnapshotTTL <= 0 {
		l.SnapshotTTL = d.SnapshotTTL
	}
	return l
}
