package dedup

import (
	"strings"
	"unicode"
)

// tokenize splits text into lowercased unicode word tokens. Consecutive
// letters, digits and ideographs are treated as word characters; every
// other character acts as a separator. CJK text therefore tokenizes one
// rune per word, which keeps the shingling meaningful for such languages.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// shingles returns the set of word n-grams contained in text. Each shingle
// is the space-joined form of n consecutive tokens. When a text contains
// fewer than n tokens, its full token sequence forms a single shingle, so
// short texts can still be compared meaningfully.
func shingles(text string, n int) map[string]struct{} {
	toks := tokenize(text)
	if n < 1 {
		n = 1
	}
	set := make(map[string]struct{}, len(toks))
	if len(toks) == 0 {
		return set
	}
	if len(toks) < n {
		set[strings.Join(toks, " ")] = struct{}{}
		return set
	}
	var b strings.Builder
	for i := 0; i+n <= len(toks); i++ {
		b.Reset()
		for j := 0; j < n; j++ {
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(toks[i+j])
		}
		set[b.String()] = struct{}{}
	}
	return set
}

// jaccard returns |a∩b| / |a∪b|. Two empty sets are defined to be
// identical (1.0); one empty and one non-empty set are disjoint (0.0).
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	var inter, union int
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		union++
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union += len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// itemShingles computes the shingle set of an item. Title and content are
// combined: title tokens are effectively given higher weight by being
// adjacent to content tokens (they share the token stream).
func itemShingles(it Item, n int) map[string]struct{} {
	if it.Title == "" {
		return shingles(it.Content, n)
	}
	if it.Content == "" {
		return shingles(it.Title, n)
	}
	return shingles(it.Title+" "+it.Content, n)
}

// pairSimilarity computes the similarity of two items as the Jaccard
// coefficient of their word shingle sets. The value lies in [0,1]; a pair
// with similarity >= Config.SimilarityThreshold is considered a duplicate.
//
// Note that text-level similarity is symmetric and transitive per cluster
// (see cluster.go): A~B and B~C merges all three even if A~C itself stays
// below the threshold.
func pairSimilarity(x, y Item, shingleSize int) float64 {
	return jaccard(itemShingles(x, shingleSize), itemShingles(y, shingleSize))
}
