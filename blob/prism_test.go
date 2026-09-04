package blob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	compprysm "github.com/draganm/comp-prysm"
	tarprism "github.com/draganm/tar-prism"
	"github.com/klauspost/compress/zstd"

	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
)

// prismFile is one regular file of a test archive.
type prismFile struct {
	name string
	data []byte
}

// prismFixtureFiles returns the ten regular files of the standard fixture:
// seven small text files, one 3 MiB random file, one empty file and one
// whose name needs a PAX header.
func prismFixtureFiles(t *testing.T) []prismFile {
	t.Helper()
	files := make([]prismFile, 0, 10)
	for i := 0; i < 7; i++ {
		files = append(files, prismFile{name: fmt.Sprintf("usr/lib/file-%d.txt", i), data: textBytes(1000+i*4096, int64(i))})
	}
	return append(files,
		prismFile{name: "usr/lib/big.bin", data: randomBytes(t, 3<<20)},
		prismFile{name: "usr/lib/empty", data: nil},
		prismFile{name: strings.Repeat("long-name-", 15) + ".txt", data: []byte("pax long name\n")},
	)
}

// prismSmallFiles is a fixture without the 3 MiB random file, for tests
// that push several variants and do not need chunk-spanning content.
func prismSmallFiles(t *testing.T) []prismFile {
	t.Helper()
	files := make([]prismFile, 0, 4)
	for i := 0; i < 3; i++ {
		files = append(files, prismFile{name: fmt.Sprintf("etc/conf-%d", i), data: textBytes(20000+i*1000, int64(100+i))})
	}
	return append(files, prismFile{name: "etc/random", data: randomBytes(t, 50000)})
}

// prismTar writes one directory, the given regular files, one symlink and
// one hard link with archive/tar. Names longer than 100 bytes get a PAX
// header.
func prismTar(t *testing.T, files []prismFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, data []byte) {
		t.Helper()
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %s: %v", h.Name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write %s: %v", h.Name, err)
		}
	}
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "usr/lib/", Mode: 0o755}, nil)
	for _, f := range files {
		h := &tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.data))}
		if len(f.name) > 100 {
			h.Format = tar.FormatPAX
		}
		write(h, f.data)
	}
	write(&tar.Header{Typeflag: tar.TypeSymlink, Name: "usr/lib/link", Linkname: "file-0.txt", Mode: 0o777}, nil)
	write(&tar.Header{Typeflag: tar.TypeLink, Name: "usr/lib/hard", Linkname: "usr/lib/file-1.txt", Mode: 0o644}, nil)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// prismZstd compresses data with klauspost zstd at its defaults.
func prismZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// putPrism stores data through a memory spool.
func putPrism(t *testing.T, b *Store, data []byte) *Meta {
	t.Helper()
	meta, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return meta
}

// pullPrism opens d and streams it back whole.
func pullPrism(t *testing.T, b *Store, d oci.Digest) ([]byte, *Blob) {
	t.Helper()
	bl, err := b.Open(d)
	if err != nil {
		t.Fatalf("Open(%s): %v", d, err)
	}
	return pullAll(t, bl), bl
}

func TestBlobsDirNameMatchesTarPrism(t *testing.T) {
	if blobsDirName != tarprism.BlobsDir {
		t.Fatalf("blobsDirName = %q, tarprism.BlobsDir = %q", blobsDirName, tarprism.BlobsDir)
	}
}

func TestPutGzipTarIsPrism(t *testing.T) {
	b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true})
	files := prismFixtureFiles(t)
	tarData := prismTar(t, files)
	data := gzipBytes(t, tarData, gzip.DefaultCompression)

	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want %q", meta.Kind, meta.RawReason, KindPrism)
	}
	if meta.Format != "gzip" {
		t.Errorf("format = %q, want gzip", meta.Format)
	}
	if meta.RawReason != "" {
		t.Errorf("rawReason = %q, want empty", meta.RawReason)
	}
	if meta.Entries != len(files) {
		t.Errorf("entries = %d, want %d", meta.Entries, len(files))
	}
	if want := oci.DigestOfBytes(tarData); meta.DiffID != want {
		t.Errorf("diffId = %s, want %s", meta.DiffID, want)
	}
	if meta.UncompressedSize != int64(len(tarData)) {
		t.Errorf("uncompressedSize = %d, want %d", meta.UncompressedSize, len(tarData))
	}
	if want := oci.DigestOfBytes(data); meta.Digest != want || meta.Size != int64(len(data)) {
		t.Errorf("digest/size = %s/%d, want %s/%d", meta.Digest, meta.Size, want, len(data))
	}
	if meta.Engine == "" || meta.EngineVersion == "" {
		t.Errorf("engine/version = %q/%q, want both set", meta.Engine, meta.EngineVersion)
	}
	if meta.Version != MetaVersion || meta.UploadedAt.IsZero() {
		t.Errorf("version/uploadedAt = %d/%v", meta.Version, meta.UploadedAt)
	}
	if meta.Stats.LogicalBytes < int64(len(tarData)) {
		t.Errorf("logicalBytes = %d, want at least the tar size %d", meta.Stats.LogicalBytes, len(tarData))
	}
	if meta.Stats.DiskBytes <= 0 || meta.Stats.ObjectsNew <= 0 {
		t.Errorf("diskBytes/objectsNew = %d/%d, want > 0", meta.Stats.DiskBytes, meta.Stats.ObjectsNew)
	}
	if meta.Stats.LogicalBytes != meta.Stats.NewLogicalBytes+meta.Stats.DedupedBytes {
		t.Errorf("stats do not add up: %+v", meta.Stats)
	}
	assertSpoolDirEmpty(t, b)

	ok, err := b.Exists(meta.Digest)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
	got, bl := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	if oci.DigestOfBytes(got) != meta.Digest {
		t.Fatal("pulled digest differs")
	}
	if bl.SupportsRange() {
		t.Error("prism blob reports range support")
	}
	metaEqual(t, *meta, bl.Meta)
	if s, ok := b.TakeRecent(meta.Digest); !ok || s != meta.Stats {
		t.Errorf("TakeRecent = %+v, %v; want %+v", s, ok, meta.Stats)
	}
	for _, want := range []string{"blob stored", "digest=" + meta.Digest.String(), "kind=prism", "format=gzip", "engine=" + meta.Engine, fmt.Sprintf("entries=%d", len(files)), "logical_bytes=", "deduped_bytes=", "disk_bytes=", "duration="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestPutGzipLevelsRoundTrip(t *testing.T) {
	tarData := prismTar(t, prismSmallFiles(t))
	for _, level := range []int{gzip.BestSpeed, gzip.DefaultCompression, gzip.BestCompression} {
		t.Run(fmt.Sprintf("level%d", level), func(t *testing.T) {
			b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
			data := gzipBytes(t, tarData, level)
			meta := putPrism(t, b, data)
			t.Logf("level %d: kind=%s rawReason=%q engine=%s", level, meta.Kind, meta.RawReason, meta.Engine)
			if meta.Kind != KindPrism || meta.Format != "gzip" || meta.DiffID != oci.DigestOfBytes(tarData) {
				t.Fatalf("meta = %+v, want a gzip prism", *meta)
			}
			got, _ := pullPrism(t, b, meta.Digest)
			if !bytes.Equal(got, data) {
				t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
			}
		})
	}
}

func TestPutSystemGzipTarIsPrism(t *testing.T) {
	tarData := prismTar(t, prismFixtureFiles(t))
	for _, tool := range []string{"gzip", "pigz"} {
		t.Run(tool, func(t *testing.T) {
			path, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("%s not on PATH", tool)
			}
			cmd := exec.Command(path, "-c")
			cmd.Stdin = bytes.NewReader(tarData)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			data, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s -c: %v: %s", tool, err, stderr.String())
			}
			b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
			meta := putPrism(t, b, data)
			t.Logf("%s: kind=%s rawReason=%q engine=%s version=%s", tool, meta.Kind, meta.RawReason, meta.Engine, meta.EngineVersion)
			if meta.Kind != KindPrism || meta.Format != "gzip" {
				t.Fatalf("%s output stored as kind=%s rawReason=%q, want a gzip prism", tool, meta.Kind, meta.RawReason)
			}
			if meta.DiffID != oci.DigestOfBytes(tarData) || meta.UncompressedSize != int64(len(tarData)) {
				t.Errorf("diffId/uncompressedSize = %s/%d, want %s/%d", meta.DiffID, meta.UncompressedSize, oci.DigestOfBytes(tarData), len(tarData))
			}
			got, _ := pullPrism(t, b, meta.Digest)
			if !bytes.Equal(got, data) {
				t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
			}
		})
	}
}

func TestPrismRootLayout(t *testing.T) {
	b, st, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	files := prismFixtureFiles(t)
	tarData := prismTar(t, files)
	data := gzipBytes(t, tarData, gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want prism", meta.Kind, meta.RawReason)
	}
	bl, err := b.Open(meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	root := bl.Root()

	for _, name := range []string{MetaFile, CompFile, tarprism.RecipeFile, tarprism.IndexFile} {
		e, err := st.Lookup(root, name)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", name, err)
		}
		if e.Mode != store.ModeFile {
			t.Errorf("%s mode = %o, want %o", name, e.Mode, store.ModeFile)
		}
	}
	if _, err := st.Lookup(root, RawFile); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Lookup(%s) = %v, want ErrNotFound", RawFile, err)
	}
	blobsEntry, err := st.Lookup(root, tarprism.BlobsDir)
	if err != nil {
		t.Fatalf("Lookup(blobs): %v", err)
	}
	if blobsEntry.Mode != store.ModeDir {
		t.Errorf("blobs mode = %o, want %o", blobsEntry.Mode, store.ModeDir)
	}
	entries, more, err := st.ListDir(root, "", 10)
	if err != nil || more {
		t.Fatalf("ListDir: %v, more=%v", err, more)
	}
	var names []string
	for _, e := range entries {
		names = append(names, string(e.Name))
	}
	if want := []string{"blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("root entries = %v, want %v", names, want)
	}
	blobsKey, err := st.LookupKey(root, tarprism.BlobsDir)
	if err != nil {
		t.Fatal(err)
	}

	indexKey, err := st.LookupKey(root, tarprism.IndexFile)
	if err != nil {
		t.Fatal(err)
	}
	indexData, err := st.ReadFile(indexKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(indexData, []byte("}\n")) {
		t.Errorf("recipe.json does not end with an indented object and newline: %q", indexData[max(0, len(indexData)-8):])
	}
	idx, err := tarprism.DecodeIndex(indexData)
	if err != nil {
		t.Fatalf("recipe.json: %v", err)
	}
	if encoded, _ := tarprism.EncodeIndex(idx); !bytes.Equal(encoded, indexData) {
		t.Errorf("recipe.json is not tar-prism's own encoding:\n%s", indexData)
	}
	if idx.Version != tarprism.FormatVersion || len(idx.BLAKE3) != 64 {
		t.Errorf("index version/blake3 = %d/%q", idx.Version, idx.BLAKE3)
	}
	if len(idx.Entries) != len(files) {
		t.Fatalf("index has %d entries, want %d", len(idx.Entries), len(files))
	}
	var contentTotal int64
	for i, e := range idx.Entries {
		name := fmt.Sprintf("%08d", i+1)
		if e.Blob != tarprism.BlobsDir+"/"+name {
			t.Errorf("entry %d blob = %q, want %s/%s", i, e.Blob, tarprism.BlobsDir, name)
		}
		if e.Name != files[i].name || e.Size != int64(len(files[i].data)) {
			t.Errorf("entry %d = %q/%d, want %q/%d", i, e.Name, e.Size, files[i].name, len(files[i].data))
		}
		k, err := st.LookupKey(blobsKey, name)
		if err != nil {
			t.Fatalf("LookupKey(blobs/%s): %v", name, err)
		}
		if int64(k.Length()) != e.Size {
			t.Errorf("blobs/%s length = %d, want %d", name, k.Length(), e.Size)
		}
		content, err := st.ReadFile(k)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, files[i].data) {
			t.Errorf("blobs/%s content differs from %s", name, files[i].name)
		}
		contentTotal += e.Size
	}
	if _, err := st.LookupKey(blobsKey, fmt.Sprintf("%08d", len(files)+1)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("extra blob entry: %v", err)
	}

	recipeKey, err := st.LookupKey(root, tarprism.RecipeFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(tarData)) - contentTotal; int64(recipeKey.Length()) != want {
		t.Errorf("recipe.bin length = %d, want %d (tar minus file contents)", recipeKey.Length(), want)
	}

	compKey, err := st.LookupKey(root, CompFile)
	if err != nil {
		t.Fatal(err)
	}
	compData, err := st.ReadFile(compKey)
	if err != nil {
		t.Fatal(err)
	}
	params, err := compprysm.ReadParams(bytes.NewReader(compData))
	if err != nil {
		t.Fatalf("comp.json: %v", err)
	}
	if params.Format != compprysm.FormatGzip || params.Engine != meta.Engine || params.EngineVersion != meta.EngineVersion {
		t.Errorf("comp.json = %s/%s/%s, meta = gzip/%s/%s", params.Format, params.Engine, params.EngineVersion, meta.Engine, meta.EngineVersion)
	}
	if params.Uncompressed.Size != int64(len(tarData)) || params.Compressed.Size != int64(len(data)) {
		t.Errorf("comp.json sizes = %d/%d, want %d/%d", params.Uncompressed.Size, params.Compressed.Size, len(tarData), len(data))
	}

	metaKey, err := st.LookupKey(root, MetaFile)
	if err != nil {
		t.Fatal(err)
	}
	metaData, err := st.ReadFile(metaKey)
	if err != nil {
		t.Fatal(err)
	}
	var stored Meta
	if err := json.Unmarshal(metaData, &stored); err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	metaEqual(t, *meta, stored)
}

func TestPutZstdTarRoundTrips(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	files := prismFixtureFiles(t)
	tarData := prismTar(t, files)
	data := prismZstd(t, tarData)

	meta := putPrism(t, b, data)
	t.Logf("zstd layer stored as kind=%s rawReason=%q engine=%s", meta.Kind, meta.RawReason, meta.Engine)
	switch meta.Kind {
	case KindPrism:
		if meta.Entries != len(files) || meta.DiffID != oci.DigestOfBytes(tarData) || meta.UncompressedSize != int64(len(tarData)) {
			t.Errorf("prism meta = %+v", *meta)
		}
		if meta.Engine == "" {
			t.Error("engine is empty")
		}
	case KindRaw:
		if meta.RawReason != ReasonNotReproducible {
			t.Errorf("rawReason = %q, want %q", meta.RawReason, ReasonNotReproducible)
		}
	default:
		t.Fatalf("kind = %q", meta.Kind)
	}
	if meta.Format != "zstd" {
		t.Errorf("format = %q, want zstd", meta.Format)
	}
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	assertSpoolDirEmpty(t, b)
}

// zstdHugeWindowFrame hand-builds a minimal, empty zstd frame whose window
// descriptor declares a 256 MiB window: magic, a frame header descriptor
// with every flag clear (so a window descriptor byte follows and no
// content-size field does), the window descriptor byte itself (exponent 18,
// mantissa 0), and one empty last raw block. No encoder is asked to
// actually fill such a window; this exercises exactly the frame-header
// field blob.zstdWindowSize reads.
func zstdHugeWindowFrame() []byte {
	return []byte{
		0x28, 0xb5, 0x2f, 0xfd, // zstd magic
		0x00,             // frame header descriptor: no flags set
		0x90,             // window descriptor: exponent=18 -> window = 2^28 = 256 MiB
		0x01, 0x00, 0x00, // block header: last block, raw, size 0
	}
}

// TestPutZstdHugeWindowStoresRawUnsupported covers the window-size guard in
// analyze.go: a zstd frame declaring a window past maxZstdWindow is
// classified before Analyze (and before any decompression) is attempted,
// so a hostile or oversized frame never grows a decoder window buffer past
// the configured bound.
func TestPutZstdHugeWindowStoresRawUnsupported(t *testing.T) {
	b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true})
	data := zstdHugeWindowFrame()
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonUnsupported {
		t.Fatalf("kind/reason = %q/%q, want raw/%s", meta.Kind, meta.RawReason, ReasonUnsupported)
	}
	if meta.Format != "zstd" {
		t.Errorf("format = %q, want zstd", meta.Format)
	}
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	assertSpoolDirEmpty(t, b)
	if out := logs.String(); !strings.Contains(out, "window_size=268435456") {
		t.Errorf("log lacks the declared window size:\n%s", out)
	}
}

func TestPutUncompressedTarIsPrism(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	files := prismFixtureFiles(t)
	data := prismTar(t, files)

	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want prism", meta.Kind, meta.RawReason)
	}
	if meta.Format != "none" {
		t.Errorf("format = %q, want none", meta.Format)
	}
	if meta.Engine != "" || meta.EngineVersion != "" {
		t.Errorf("engine = %q/%q, want empty for format none", meta.Engine, meta.EngineVersion)
	}
	if meta.Entries != len(files) {
		t.Errorf("entries = %d, want %d", meta.Entries, len(files))
	}
	if meta.DiffID != meta.Digest || meta.UncompressedSize != meta.Size {
		t.Errorf("diffId/uncompressedSize = %s/%d, want the blob's own %s/%d", meta.DiffID, meta.UncompressedSize, meta.Digest, meta.Size)
	}
	got, bl := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	if bl.SupportsRange() {
		t.Error("prism blob reports range support")
	}
}

// cancelAfterWriter cancels a context once n bytes have passed through it.
type cancelAfterWriter struct {
	w      *bytes.Buffer
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfterWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if c.w.Len() >= c.n {
		c.cancel()
	}
	return n, err
}

func TestPrismWriteToCancelledContext(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	data := gzipBytes(t, prismTar(t, prismFixtureFiles(t)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want prism", meta.Kind, meta.RawReason)
	}
	bl, err := b.Open(meta.Digest)
	if err != nil {
		t.Fatal(err)
	}

	// Cancelled before the first byte: nothing is written.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := bl.WriteTo(ctx, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo with cancelled context = %v, want context.Canceled", err)
	}
	if out.Len() != 0 {
		t.Errorf("cancelled pull wrote %d bytes", out.Len())
	}

	// Cancelled mid-body (the client went away): the pipeline stops short
	// of the full body and reports the cancellation, not a digest mismatch.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	cw := &cancelAfterWriter{w: &bytes.Buffer{}, n: 64 << 10, cancel: cancel}
	err = bl.WriteTo(ctx, cw)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo cancelled mid-body = %v, want context.Canceled", err)
	}
	if cw.w.Len() >= len(data) {
		t.Errorf("cancelled pull still produced the whole body (%d bytes)", cw.w.Len())
	}
	if !bytes.Equal(cw.w.Bytes(), data[:cw.w.Len()]) {
		t.Error("partial body is not a prefix of the blob")
	}

	// The blob is still fully servable afterwards.
	if got := pullAll(t, bl); !bytes.Equal(got, data) {
		t.Fatal("pull after cancellation differs")
	}
}
