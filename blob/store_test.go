package blob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
)

func TestNewCreatesAndEmptiesSpoolDir(t *testing.T) {
	work := t.TempDir()
	spool := filepath.Join(work, spoolDirName)
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _, _ := newTestStore(t, Options{WorkDir: work})
	if got := spoolDirOf(b); got != spool {
		t.Fatalf("spool dir = %q, want %q", got, spool)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatalf("spool dir missing: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("spool dir not emptied: %v", entries)
	}
}

func TestNewRequiresWorkDir(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := New(st, Options{}, nil); err == nil {
		t.Fatal("empty WorkDir must be refused")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	want := Options{
		WorkDir:               b.opts.WorkDir,
		MaxInMemory:           64 << 20,
		AnalyzeParallelism:    2,
		AnalyzeTimeout:        15 * time.Minute,
		MaxConcurrentFinalize: max(1, runtime.NumCPU()/2),
		RecentTTL:             time.Hour,
	}
	if b.opts != want {
		t.Fatalf("defaults = %+v, want %+v", b.opts, want)
	}
	b2, _, _ := newTestStore(t, Options{MaxConcurrentFinalize: 3, AnalyzeParallelism: 1, AnalyzeTimeout: time.Second, RecentTTL: time.Minute, MaxInMemory: 1 << 20, VerifyRoundTrip: true})
	if b2.opts.MaxConcurrentFinalize != 3 || b2.opts.AnalyzeParallelism != 1 || b2.opts.AnalyzeTimeout != time.Second || b2.opts.RecentTTL != time.Minute || b2.opts.MaxInMemory != 1<<20 || !b2.opts.VerifyRoundTrip {
		t.Fatalf("explicit options overridden: %+v", b2.opts)
	}
}

func TestUnknownDigest(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	d := oci.DigestOfBytes([]byte("nope"))
	ok, err := b.Exists(d)
	if err != nil || ok {
		t.Fatalf("Exists = %v, %v; want false, nil", ok, err)
	}
	if _, err := b.Open(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open = %v, want ErrNotFound", err)
	}
	if err := b.Delete(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete = %v, want ErrNotFound", err)
	}
	if _, ok := b.TakeRecent(d); ok {
		t.Fatal("TakeRecent must be false for an unknown digest")
	}
}

func TestBuildRootAndOpen(t *testing.T) {
	b, st, _ := newTestStore(t, Options{})
	ctx := context.Background()
	data := []byte("hello raw")
	d := oci.DigestOfBytes(data)
	meta := Meta{Version: MetaVersion, Digest: d, Size: int64(len(data)), Kind: KindRaw, Format: "none", RawReason: ReasonNotTar, UploadedAt: time.Now().UTC()}

	w := st.NewWriter(ctx)
	rawKey, err := w.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.buildRoot(w, meta, map[string]key.Key{RawFile: rawKey}, key.Key{})
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(RefName(d), root); err != nil {
		t.Fatal(err)
	}

	ok, err := b.Exists(d)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	bl, err := b.Open(d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	metaEqual(t, meta, bl.Meta)
	if bl.Root() != root {
		t.Fatalf("Root = %s, want %s", bl.Root(), root)
	}
	if !bl.SupportsRange() {
		t.Fatal("raw blob must support ranges")
	}
	e, err := st.Lookup(root, MetaFile)
	if err != nil || e.Mode != store.ModeFile {
		t.Fatalf("meta.json entry: %+v, %v", e, err)
	}
	e, err = st.Lookup(root, RawFile)
	if err != nil || e.Mode != store.ModeFile {
		t.Fatalf("raw entry: %+v, %v", e, err)
	}
	if ck, err := key.Parse(e.ContentKey); err != nil || ck != rawKey {
		t.Fatalf("raw content key = %v, %v; want %s", ck, err, rawKey)
	}
	if _, err := st.Lookup(root, CompFile); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("raw root must have no comp.json: %v", err)
	}

	// A prism-shaped root lists blobs/ as a directory and sorts every name.
	w2 := st.NewWriter(ctx)
	fileKey, err := w2.PutBytes([]byte("file content"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := w2.NewDir()
	if err := blobs.AddFile("00000001", fileKey); err != nil {
		t.Fatal(err)
	}
	blobsKey, err := blobs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	compKey, err := w2.PutBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	recipeKey, err := w2.PutBytes([]byte("recipe"))
	if err != nil {
		t.Fatal(err)
	}
	indexKey, err := w2.PutBytes([]byte("[]\n"))
	if err != nil {
		t.Fatal(err)
	}
	pmeta := meta
	pmeta.Kind, pmeta.RawReason = KindPrism, ""
	root2, err := b.buildRoot(w2, pmeta, map[string]key.Key{CompFile: compKey, "recipe.bin": recipeKey, "recipe.json": indexKey}, blobsKey)
	if err != nil {
		t.Fatalf("buildRoot prism: %v", err)
	}
	if _, err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = st.Lookup(root2, "blobs")
	if err != nil || e.Mode != store.ModeDir {
		t.Fatalf("blobs entry: %+v, %v", e, err)
	}
	if ck, err := key.Parse(e.ContentKey); err != nil || ck != blobsKey {
		t.Fatalf("blobs content key = %v, %v; want %s", ck, err, blobsKey)
	}
	entries, more, err := st.ListDir(root2, "", 10)
	if err != nil || more {
		t.Fatalf("ListDir: %v, more=%v", err, more)
	}
	var names []string
	for _, e := range entries {
		names = append(names, string(e.Name))
	}
	want := []string{"blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json"}
	if len(names) != len(want) {
		t.Fatalf("root entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("root entries = %v, want %v", names, want)
		}
	}
}
