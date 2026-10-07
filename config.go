package dedup

import "time"

// Default resource and similarity parameters.
const (
	defaultThreshold       = 0.85
	defaultShingleSize     = 3
	defaultMaxCandidates   = 10_000
	defaultMaxPairCompares = 5_000_000
	defaultMaxRerankItems  = 10_000
	defaultMaxSnapshot     = 100_000
	defaultMaxSnapshots    = 64
	defaultSnapshotTTL     = 30 * time.Minute
)

func (cfg Config) effectiveThreshold() float64 {
	if cfg.SimilarityThreshold > 0 {
		if cfg.SimilarityThreshold > 1 {
			return 1
		}
		return cfg.SimilarityThreshold
	}
	return defaultThreshold
}

func (cfg Config) effectiveShingleSize() int {
	if cfg.ShingleSize > 0 {
		return cfg.ShingleSize
	}
	return defaultShingleSize
}

func (cfg Config) effectiveMaxCandidates() int {
	if cfg.MaxCandidates > 0 {
		return cfg.MaxCandidates
	}
	return defaultMaxCandidates
}

func (cfg Config) effectiveMaxPairComparisons() int64 {
	if cfg.MaxPairComparisons > 0 {
		return cfg.MaxPairComparisons
	}
	return defaultMaxPairCompares
}

func (cfg Config) effectiveMaxRerankItems() int {
	if cfg.MaxRerankItems > 0 {
		return cfg.MaxRerankItems
	}
	return defaultMaxRerankItems
}

func (cfg Config) effectiveMaxSnapshotItems() int {
	if cfg.MaxSnapshotItems > 0 {
		return cfg.MaxSnapshotItems
	}
	return defaultMaxSnapshot
}

func (cfg Config) effectiveMaxSnapshots() int {
	if cfg.MaxSnapshots > 0 {
		return cfg.MaxSnapshots
	}
	return defaultMaxSnapshots
}

func (cfg Config) effectiveSnapshotTTL() time.Duration {
	if cfg.SnapshotTTL > 0 {
		return cfg.SnapshotTTL
	}
	return defaultSnapshotTTL
}

func (cfg Config) clock() func() time.Time {
	if cfg.Clock != nil {
		return cfg.Clock
	}
	return time.Now
}
