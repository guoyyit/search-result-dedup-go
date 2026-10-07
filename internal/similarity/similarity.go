// Package similarity computes text similarity used by near-duplicate clustering.
//
// Similarity semantics:
//
//   - Text is normalized (Unicode lower-casing, punctuation/whitespace
//     collapsed) and sliced into overlapping k-shingles of K runes.
//   - Each shingle is fingerprinted with FNV-1a (64 bit).
//   - Two documents are compared by the Jaccard index of their shingle
//     sets: |A ∩ B| / |A ∪ B|, ranging 0 (no overlap) to 1 (identical).
//
// Rune-based shingles work for both whitespace-delimited languages and
// scripts such as CJK that do not separate words with spaces.
package similarity

import (
	"hash/fnv"
	"strings"
	"unicode"
)

// DefaultK is the shingle size used when none is configured.
const DefaultK = 5

// Hasher turns text into deterministic k-shingle fingerprint sets.
type Hasher struct {
	// K is the shingle size in runes; values < 1 are treated as DefaultK.
	K int
}

// NewHasher creates a Hasher for the given shingle size.
func NewHasher(k int) *Hasher {
	if k < 1 {
		k = DefaultK
	}
	return &Hasher{K: k}
}

// normalizeRunes lower-cases text and maps every non-letter/non-digit rune
// to a single collapsing space. The result has no leading/trailing space.
func normalizeRunes(text string) []rune {
	var out []rune
	prevSpace := true
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out = append(out, unicode.ToLower(r))
			prevSpace = false
		default:
			if !prevSpace {
				out = append(out, ' ')
			}
			prevSpace = true
		}
	}
	if len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return out
}

// hashShingle fingerprints one rune window with FNV-1a 64.
func hashShingle(window []rune) uint64 {
	h := fnv.New64a()
	for _, r := range window {
		var buf [4]byte
		n := utf8EncodeRune(buf[:], r)
		_, _ = h.Write(buf[:n])
	}
	return h.Sum64()
}

// utf8EncodeRune is a tiny allocation-free utf8.EncodeRune replacement.
func utf8EncodeRune(p []byte, r rune) int {
	switch {
	case r < 0x80:
		p[0] = byte(r)
		return 1
	case r < 0x800:
		p[0] = 0xC0 | byte(r>>6)
		p[1] = 0x80 | byte(r&0x3F)
		return 2
	case r < 0x10000:
		p[0] = 0xE0 | byte(r>>12)
		p[1] = 0x80 | byte((r>>6)&0x3F)
		p[2] = 0x80 | byte(r&0x3F)
		return 3
	default:
		p[0] = 0xF0 | byte(r>>18)
		p[1] = 0x80 | byte((r>>12)&0x3F)
		p[2] = 0x80 | byte((r>>6)&0x3F)
		p[3] = 0x80 | byte(r&0x3F)
		return 4
	}
}

// Shingles returns the deduplicated set of k-shingle fingerprints for text.
//
// If the normalized text is shorter than K runes, the whole text is used
// as a single shingle so that short documents remain comparable. Empty
// (or punctuation-only) text yields an empty set.
func (h *Hasher) Shingles(text string) (map[uint64]struct{}, int) {
	runes := normalizeRunes(text)
	if len(runes) == 0 {
		return map[uint64]struct{}{}, 0
	}
	set := make(map[uint64]struct{})
	if len(runes) < h.K {
		set[hashShingle(runes)] = struct{}{}
		return set, 1
	}
	for i := 0; i+h.K <= len(runes); i++ {
		set[hashShingle(runes[i:i+h.K])] = struct{}{}
	}
	return set, len(runes) - h.K + 1
}

// Jaccard returns the Jaccard similarity of two shingle sets in [0,1].
//
// Two empty sets are considered identical (similarity 1); an empty and a
// non-empty set have similarity 0.
func Jaccard(a, b map[uint64]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	inter := 0
	for k := range small {
		if _, ok := large[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Tokenize splits text into lowercase normalized word tokens. It is used
// for query-term matching, not for similarity.
func Tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
