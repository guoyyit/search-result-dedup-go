// Package index maintains the searchable document set, its near-duplicate
// clusters and a durable write-ahead log for crash recovery.
//
// Durability and recovery
//
//   - Every Add/Delete/Upsert is appended to an fsynced WAL before the
//     in-memory state is observable as changed.
//   - Open replays committed records in order: OpAdd applies as Upsert,
//     OpDelete removes. Torn trailing records are ignored by the WAL layer.
//   - Because replay order, shingle hashing, threshold comparisons and
//     representative selection are all deterministic, a recovered index is
//     byte-for-byte equivalent to the pre-crash one.
//
// Representative stability
//
//   - Additions never change an existing cluster's representative.
//   - Overwrites preserve the member's insertion order, so a content refresh
//     leaves the representative unchanged whenever clustering still allows it.
//   - Deletions only disturb the cluster that contained the removed member.
package index

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/guoyyit/search-result-dedup-go/internal/dedup"
	"github.com/guoyyit/search-result-dedup-go/internal/model"
	"github.com/guoyyit/search-result-dedup-go/internal/similarity"
	"github.com/guoyyit/search-result-dedup-go/internal/storage"
)

// WALName is the on-disk log file name inside Config.DataDir.
const WALName = "wal.log"

// Config configures an Index.
type Config struct {
	// DataDir is the directory holding the write-ahead log.
	DataDir string
	// SimilarityThreshold is the Jaccard threshold in (0,1]. Zero means 1.
	SimilarityThreshold float64
	// ShingleK is the shingle size in runes. Zero means the hasher default.
	ShingleK int
	// MaxContentBytes, when > 0, rejects documents whose UTF-8 content is
	// larger than the limit.
	MaxContentBytes int
}

// ErrContentTooLarge is returned when a document exceeds MaxContentBytes.
var ErrContentTooLarge = errors.New("index: document content exceeds MaxContentBytes")

// ErrEmptyID is returned for documents without an ID.
var ErrEmptyID = errors.New("index: document id is empty")

// RepView is one cluster representative together with its cluster metadata.
type RepView struct {
	Doc     model.Document
	Cluster dedup.Cluster
}

// Index is the durable incremental near-duplicate index.
type Index struct {
	mu        sync.RWMutex
	clusterer *dedup.Clusterer
	hasher    *similarity.Hasher
	log       *storage.Log
	maxBytes  int
}

// Open opens or creates an index and replays the committed WAL.
func Open(cfg Config) (*Index, error) {
	hasher := similarity.NewHasher(cfg.ShingleK)
	cl := dedup.NewClusterer(cfg.SimilarityThreshold, hasher)
	log, err := storage.Open(filepath.Join(cfg.DataDir, WALName))
	if err != nil {
		return nil, err
	}
	ix := &Index{
		clusterer: cl,
		hasher:    hasher,
		log:       log,
		maxBytes:  cfg.MaxContentBytes,
	}
	// Recover is single-threaded at open; it applies committed records and
	// truncates torn trailing bytes left by a crash.
	if err := log.Recover(func(rec storage.Record) error {
		switch rec.Op {
		case storage.OpAdd:
			ix.applyUpsert(rec.Doc)
		case storage.OpDelete:
			ix.clusterer.Remove(rec.Doc.ID)
		}
		return nil
	}); err != nil {
		_ = log.Close()
		return nil, err
	}
	return ix, nil
}

// shingle computes the fingerprint set of a document.
func (ix *Index) shingle(doc model.Document) map[uint64]struct{} {
	set, _ := ix.hasher.Shingles(doc.Content)
	return set
}

// applyUpsert mutates only in-memory state (no WAL).
func (ix *Index) applyUpsert(doc model.Document) {
	ix.clusterer.Upsert(dedup.Entry{Doc: doc, Shingles: ix.shingle(doc)})
}

// validate checks a document before accepting it.
func (ix *Index) validate(doc model.Document) error {
	if doc.ID == "" {
		return ErrEmptyID
	}
	if ix.maxBytes > 0 && len(doc.Content) > ix.maxBytes {
		return ErrContentTooLarge
	}
	return nil
}

// Add inserts a new document. It returns false without change if the ID exists.
func (ix *Index) Add(doc model.Document) (bool, error) {
	if err := ix.validate(doc); err != nil {
		return false, err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, ok := ix.clusterer.Get(doc.ID); ok {
		return false, nil
	}
	if err := ix.log.Append(storage.Record{Op: storage.OpAdd, Doc: doc}); err != nil {
		return false, err
	}
	ix.applyUpsert(doc)
	return true, nil
}

// Delete removes a document. It reports whether the ID existed.
func (ix *Index) Delete(id string) (bool, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, ok := ix.clusterer.Get(id); !ok {
		return false, nil
	}
	doc, _ := ix.clusterer.Get(id)
	if err := ix.log.Append(storage.Record{Op: storage.OpDelete, Doc: doc.Doc}); err != nil {
		return false, err
	}
	ix.clusterer.Remove(id)
	return true, nil
}

// Upsert inserts or overwrites a document, keeping clustering consistent.
// changed reports whether the stored content actually changed; an unchanged
// document is a no-op and appends nothing to the WAL.
func (ix *Index) Upsert(doc model.Document) (changed bool, err error) {
	if err := ix.validate(doc); err != nil {
		return false, err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if old, ok := ix.clusterer.Get(doc.ID); ok &&
		old.Doc.Source == doc.Source &&
		old.Doc.Title == doc.Title &&
		old.Doc.Content == doc.Content &&
		old.Doc.Score == doc.Score {
		return false, nil
	}
	if err := ix.log.Append(storage.Record{Op: storage.OpAdd, Doc: doc}); err != nil {
		return false, err
	}
	ix.applyUpsert(doc)
	return true, nil
}

// Get returns a stored document.
func (ix *Index) Get(id string) (model.Document, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.clusterer.Get(id)
	if !ok {
		return model.Document{}, false
	}
	return e.Doc, true
}

// Len returns the number of stored documents.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.clusterer.Len()
}

// Representatives returns one RepView per current cluster, ordered by doc ID.
// The slice is freshly allocated and safe to retain by callers.
func (ix *Index) Representatives() []RepView {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	clusters := ix.clusterer.Clusters()
	out := make([]RepView, 0, len(clusters))
	for _, cl := range clusters {
		e, ok := ix.clusterer.Get(cl.Representative)
		if !ok {
			continue
		}
		out = append(out, RepView{Doc: e.Doc, Cluster: cl})
	}
	return out
}

// Close flushes and closes the WAL.
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.log.Close()
}
