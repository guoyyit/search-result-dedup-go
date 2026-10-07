// Command dedupd runs the near-duplicate search service.
//
// It is intentionally a thin host: configuration is taken from flags and a
// smoke index round-trip is performed at startup. Embed internal/service in
// an HTTP/RPC layer appropriate for your environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/guoyyit/search-result-dedup-go/internal/index"
	"github.com/guoyyit/search-result-dedup-go/internal/limits"
	"github.com/guoyyit/search-result-dedup-go/internal/model"
	"github.com/guoyyit/search-result-dedup-go/internal/service"
)

func main() {
	dataDir := flag.String("data-dir", "./data", "directory for the write-ahead log")
	threshold := flag.Float64("threshold", 0.8, "near-duplicate Jaccard threshold in (0,1]")
	shingleK := flag.Int("shingle-k", 5, "shingle size in runes")
	maxQueries := flag.Int("max-concurrent-queries", 64, "maximum simultaneous queries")
	pageSize := flag.Int("page-size", 10, "page size for the startup smoke query")
	flag.Parse()

	lim := limits.Default()
	lim.MaxConcurrentQueries = *maxQueries

	idx, err := index.Open(index.Config{
		DataDir:             *dataDir,
		SimilarityThreshold: *threshold,
		ShingleK:            *shingleK,
		MaxContentBytes:     lim.MaxContentBytes,
	})
	if err != nil {
		log.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	svc := service.New(idx, lim)
	defer svc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page, err := svc.Search(ctx, service.Query{Text: "", PageSize: *pageSize})
	if err != nil {
		log.Fatalf("startup smoke query failed: %v", err)
	}
	fmt.Printf("dedupd ready: %d documents, %d snapshot items\n",
		idx.Len(), page.Total)

	// Keep the program's public surface small but referenced: show one doc
	// type so future embedding starts from these primitives.
	_ = model.Document{}
}
