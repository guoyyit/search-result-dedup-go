package similarity

import (
	"math"
	"testing"
)

func TestShinglesAndJaccard(t *testing.T) {
	h := NewHasher(5)

	a, n := h.Shingles("the quick brown fox jumps")
	if n == 0 || len(a) == 0 {
		t.Fatalf("expected shingles, got set=%v n=%d", a, n)
	}
	// Identical text => 1.0.
	b, _ := h.Shingles("the quick brown fox jumps")
	if got := Jaccard(a, b); got != 1.0 {
		t.Fatalf("identical text similarity = %v, want 1", got)
	}
	// Completely disjoint text => 0.
	z, _ := h.Shingles("zzzz qqqqq xxxxx vvvvv bbbbb")
	if got := Jaccard(a, z); got != 0.0 {
		t.Fatalf("disjoint text similarity = %v, want 0", got)
	}
	// Near-duplicate (small edit) should be more similar than unrelated text.
	sim, _ := h.Shingles("the quick brown fox jumpd")
	near := Jaccard(a, sim)
	far := Jaccard(a, z)
	if !(near > far) {
		t.Fatalf("expected near(%v) > far(%v)", near, far)
	}
	// Similarity must be symmetric.
	if Jaccard(a, sim) != Jaccard(sim, a) {
		t.Fatalf("jaccard not symmetric")
	}
	// Range check.
	for _, x := range []map[uint64]struct{}{a, b, z, sim} {
		if v := Jaccard(a, x); v < 0 || v > 1 || math.IsNaN(v) {
			t.Fatalf("jaccard out of range: %v", v)
		}
	}
}

func TestShinglesNormalization(t *testing.T) {
	h := NewHasher(5)
	// Case and punctuation differences must normalize to the same set.
	a, _ := h.Shingles("Hello, World! FOO bar baz qux")
	b, _ := h.Shingles("hello world foo bar baz qux")
	if Jaccard(a, b) != 1.0 {
		t.Fatalf("normalized texts should be identical, got %v", Jaccard(a, b))
	}
}

func TestShortText(t *testing.T) {
	h := NewHasher(5)
	set, n := h.Shingles("ab") // shorter than K
	if n != 1 || len(set) != 1 {
		t.Fatalf("short text should yield 1 shingle, got n=%d set=%d", n, len(set))
	}
	empty, n := h.Shingles("!!!")
	if n != 0 || len(empty) != 0 {
		t.Fatalf("punctuation-only text should yield no shingles")
	}
	if got := Jaccard(empty, map[uint64]struct{}{}); got != 1 {
		t.Fatalf("two empty sets should be equal, got %v", got)
	}
	if got := Jaccard(set, empty); got != 0 {
		t.Fatalf("empty vs non-empty should be 0, got %v", got)
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("Hello, World! foo123")
	want := []string{"hello", "world", "foo123"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
