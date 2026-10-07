package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guoyyit/search-result-dedup-go/internal/model"
)

func TestAppendAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recs := []Record{
		{Op: OpAdd, Doc: model.Document{ID: "1", Content: "one", Source: "a"}},
		{Op: OpAdd, Doc: model.Document{ID: "2", Content: "two", Source: "b"}},
		{Op: OpDelete, Doc: model.Document{ID: "1"}},
	}
	for _, r := range recs {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []Record
	if _, err := l2.Replay(func(r Record) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Fatalf("replayed %d, want %d", len(got), len(recs))
	}
	for i, r := range recs {
		if got[i].Op != r.Op || got[i].Doc.ID != r.Doc.ID || got[i].Doc.Content != r.Doc.Content {
			t.Fatalf("record %d mismatch: %+v vs %+v", i, got[i], r)
		}
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverTruncatesTornTailAndAppendsWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	committed := []Record{
		{Op: OpAdd, Doc: model.Document{ID: "a", Content: "alpha"}},
		{Op: OpAdd, Doc: model.Document{ID: "b", Content: "beta"}},
	}
	for _, r := range committed {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	// Simulate a crash mid-append: append a partial frame directly.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	partial := append(append([]byte{}, frameMagic...), 0, 0, 0, 50, 1, 2, 3)
	if _, err := f.Write(partial); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Recovery must surface the committed prefix and truncate torn bytes.
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []Record
	if err := l2.Recover(func(r Record) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("recover torn tail must not fail, got %v", err)
	}
	if len(got) != 2 || got[0].Doc.ID != "a" || got[1].Doc.ID != "b" {
		t.Fatalf("committed records mismatch: %+v", got)
	}

	// After recovery, new appends land right after the intact prefix and
	// replay sees the full sequence.
	if err := l2.Append(Record{Op: OpAdd, Doc: model.Document{ID: "c", Content: "gamma"}}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	l3, _ := Open(path)
	var got3 []Record
	if err := l3.Recover(func(r Record) error { got3 = append(got3, r); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if len(got3) != len(want) {
		t.Fatalf("after recovery+append want %v, got %+v", want, got3)
	}
	for i, id := range want {
		if got3[i].Doc.ID != id {
			t.Fatalf("record %d = %s, want %s", i, got3[i].Doc.ID, id)
		}
	}
	l3.Close()
}

func TestReplayBadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	if err := os.WriteFile(path, []byte("XXXXgarbagegarbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Replay(func(Record) error { return nil }); err != ErrCorrupt {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}
