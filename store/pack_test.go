package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jobs-build/amber-store-core/amberpack"
	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"
)

// packFixture is the content every pack test stages: one file that spans
// several chunks, one small file and one directory naming both.
type packFixture struct {
	big, small []byte
}

func newPackFixture(t *testing.T) packFixture {
	t.Helper()
	return packFixture{big: pseudoRandomBytes(t, 3<<20, 41), small: []byte("a small file")}
}

// stageFixture writes the fixture through w and returns the root keys.
func stageFixture(t *testing.T, w *Writer, fx packFixture) (big, small, dir key.Key) {
	t.Helper()
	var err error
	if big, err = w.PutStream(bytes.NewReader(fx.big)); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if small, err = w.PutBytes(fx.small); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	d := w.NewDir()
	if err := d.AddFile("big", big); err != nil {
		t.Fatal(err)
	}
	if err := d.AddFile("small", small); err != nil {
		t.Fatal(err)
	}
	if dir, err = d.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return big, small, dir
}

// assertDirEmpty fails when dir holds any entry: pack files are unlinked
// at creation, so nothing may ever be visible there.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d files left under %s: %v", len(entries), dir, entries)
	}
}

func TestPackWriterStagesThenAddPackStores(t *testing.T) {
	s := openWriterStore(t)
	dir := t.TempDir()
	fx := newPackFixture(t)

	pw, err := s.NewPackWriter(context.Background(), dir)
	if err != nil {
		t.Fatalf("NewPackWriter: %v", err)
	}
	if pw.Pack() != nil {
		t.Fatal("Pack() before Close is not nil")
	}
	big, small, root := stageFixture(t, pw, fx)
	st, err := pw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.LogicalBytes <= int64(len(fx.big)) || st.NewLogicalBytes != 0 || st.DiskBytes != 0 || st.ObjectsNew != 0 {
		t.Fatalf("pack writer stats = %+v, want LogicalBytes only", st)
	}
	p := pw.Pack()
	if p == nil || p.Size() == 0 {
		t.Fatalf("Pack() = %v after Close, want a non-empty pack", p)
	}
	assertDirEmpty(t, dir)
	for _, k := range []key.Key{big, small, root} {
		if has, _ := s.Has(k); has {
			t.Fatalf("%s reached the store before AddPack", k)
		}
	}

	w := s.NewWriter(context.Background())
	var reports []int64
	if err := w.AddPack(p, func(n int64) { reports = append(reports, n) }); err != nil {
		t.Fatalf("AddPack: %v", err)
	}
	st2, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("AddPack reported no progress")
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
	if last := reports[len(reports)-1]; last > p.Size() {
		t.Fatalf("progress %d exceeds the pack size %d", last, p.Size())
	}
	if got := readFileContent(t, s, big); !bytes.Equal(got, fx.big) {
		t.Error("big file read back differs")
	}
	if got := readFileContent(t, s, small); !bytes.Equal(got, fx.small) {
		t.Error("small file read back differs")
	}
	if k, err := s.LookupKey(root, "big"); err != nil || k != big {
		t.Errorf("Lookup big = %s, %v; want %s", k, err, big)
	}
	if st2.LogicalBytes != st.LogicalBytes {
		t.Errorf("AddPack LogicalBytes = %d, want the pack writer's %d", st2.LogicalBytes, st.LogicalBytes)
	}
	if st2.ObjectsNew == 0 || st2.DiskBytes == 0 || st2.NewLogicalBytes != st2.LogicalBytes {
		t.Errorf("AddPack stats = %+v, want everything new", st2)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Pack.Close: %v", err)
	}
}

func TestAddPackStatsMatchLiveWriter(t *testing.T) {
	// The same objects written live into a second store must cost the
	// same: the records are byte-identical, so keys, disk bytes and object
	// counts agree.
	fx := newPackFixture(t)

	live := openWriterStore(t)
	lw := live.NewWriter(context.Background())
	lbig, lsmall, lroot := stageFixture(t, lw, fx)
	lst, err := lw.Close()
	if err != nil {
		t.Fatal(err)
	}

	staged := openWriterStore(t)
	pw, err := staged.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pbig, psmall, proot := stageFixture(t, pw, fx)
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	w := staged.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatal(err)
	}
	pst, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pbig != lbig || psmall != lsmall || proot != lroot {
		t.Fatalf("keys differ between the pack and the live writer")
	}
	if pst != lst {
		t.Fatalf("AddPack stats %+v differ from the live writer's %+v", pst, lst)
	}
}

func TestAddPackDedupsAgainstPresentObjects(t *testing.T) {
	s := openWriterStore(t)
	fx := newPackFixture(t)
	lw := s.NewWriter(context.Background())
	stageFixture(t, lw, fx)
	first, err := lw.Close()
	if err != nil {
		t.Fatal(err)
	}

	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stageFixture(t, pw, fx)
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	w := s.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatal(err)
	}
	st, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.ObjectsNew != 0 || st.DiskBytes != 0 || st.NewLogicalBytes != 0 {
		t.Errorf("stats = %+v, want nothing new", st)
	}
	if st.ObjectsDeduped != first.ObjectsNew || st.DedupedBytes != st.LogicalBytes {
		t.Errorf("stats = %+v, want every object of the first write deduped (%d)", st, first.ObjectsNew)
	}
}

func TestPackWriterAbortAndCancelReleaseTheFile(t *testing.T) {
	s := openWriterStore(t)
	dir := t.TempDir()

	pw, err := s.NewPackWriter(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.PutBytes([]byte("staged then aborted")); err != nil {
		t.Fatal(err)
	}
	pw.Abort()
	pw.Abort() // idempotent
	if pw.Pack() != nil {
		t.Error("Pack() after Abort is not nil")
	}
	if _, err := pw.PutBytes([]byte("after abort")); err == nil {
		t.Error("PutBytes after Abort succeeded")
	}
	if _, err := pw.Close(); !errors.Is(err, errAborted) {
		t.Errorf("Close after Abort: err = %v, want errAborted", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pw2, err := s.NewPackWriter(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw2.PutBytes([]byte("before cancel")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := pw2.Close(); !errors.Is(err, context.Canceled) {
		t.Errorf("Close after cancel: err = %v, want context.Canceled", err)
	}
	if pw2.Pack() != nil {
		t.Error("Pack() after a cancelled Close is not nil")
	}
	assertDirEmpty(t, dir)
}

func TestAddPackRejectsTamperedPack(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k, err := pw.PutBytes(pseudoRandomBytes(t, 4000, 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	p := pw.Pack()
	// The last byte is the end marker; the one before it is the last
	// payload byte of the last record, so its CRC no longer matches.
	if _, err := p.f.WriteAt([]byte{0xff}, p.Size()-2); err != nil {
		t.Fatal(err)
	}
	w := s.NewWriter(context.Background())
	err = w.AddPack(p, nil)
	if !errors.Is(err, amberpack.ErrMalformed) {
		t.Fatalf("AddPack: err = %v, want amberpack.ErrMalformed", err)
	}
	if _, err := w.Close(); err == nil {
		t.Fatal("Close after a failed AddPack succeeded")
	}
	if has, _ := s.Has(k); has {
		t.Fatal("object from the tampered pack was stored")
	}
}

func TestAddPackMisuse(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.PutBytes([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	p := pw.Pack()

	other, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Abort()
	if err := other.AddPack(p, nil); err == nil {
		t.Error("AddPack on a pack writer succeeded")
	}

	w := s.NewWriter(context.Background())
	if err := w.AddPack(p, nil); err != nil {
		t.Fatalf("first AddPack: %v", err)
	}
	if err := w.AddPack(p, nil); err == nil {
		t.Error("second AddPack of the same pack succeeded")
	}
	if err := w.AddPack(nil, nil); err == nil {
		t.Error("AddPack(nil) succeeded")
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPackWriterEmitAfterCloseFails(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	obj, err := fstree.EncodeBlob([]byte("late"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pw.Emit(obj); !errors.Is(err, errWriterClosed) {
		t.Errorf("Emit after Close: err = %v, want errWriterClosed", err)
	}
	// An empty pack is a valid pack.
	w := s.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatalf("AddPack of an empty pack: %v", err)
	}
	if st, err := w.Close(); err != nil || st != (Stats{}) {
		t.Fatalf("Close = %+v, %v; want zero stats", st, err)
	}
}
