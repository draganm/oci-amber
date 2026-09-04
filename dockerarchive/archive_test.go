package dockerarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/draganm/oci-amber/dockerarchive/archivetest"
	"github.com/draganm/oci-amber/oci"
)

func openBuilder(t *testing.T, b *archivetest.Builder) *Archive {
	t.Helper()
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestOpenIndexesBlobsAndReadsTopFiles(t *testing.T) {
	b := archivetest.New()
	layer := []byte("layer bytes that are long enough to matter")
	img := b.AddImage([]byte(`{"os":"linux"}`), []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar", Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes([]byte(`{"os":"linux"}`)), RepoTags: []string{"app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	a := openBuilder(t, b)

	if len(a.Index.Manifests) != 1 || a.Index.Manifests[0].Digest != img.Digest {
		t.Fatalf("index = %+v", a.Index)
	}
	if len(a.Legacy) != 1 || a.Legacy[0].RepoTags[0] != "app:v1" || a.Legacy[0].Config != "blobs/sha256/"+oci.DigestOfBytes([]byte(`{"os":"linux"}`)).Hex() {
		t.Fatalf("legacy = %+v", a.Legacy)
	}
	ld := oci.DigestOfBytes(layer)
	if !a.Has(ld) {
		t.Fatal("layer not indexed")
	}
	if n, ok := a.Size(ld); !ok || n != int64(len(layer)) {
		t.Fatalf("Size = %d,%v", n, ok)
	}
	sec, err := a.Section(ld)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(sec)
	if string(got) != string(layer) {
		t.Fatalf("section read %q", got)
	}
	body, err := a.ReadBlob(img.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if oci.DigestOfBytes(body) != img.Digest {
		t.Fatal("ReadBlob returned other bytes")
	}
	if a.Has(oci.DigestOfBytes([]byte("nope"))) {
		t.Fatal("Has reports an absent blob")
	}
	if _, err := a.Section(oci.DigestOfBytes([]byte("nope"))); !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("Section of an absent blob: %v", err)
	}
}

func TestOpenRejectsMissingIndex(t *testing.T) {
	b := archivetest.New()
	b.AddBlob([]byte("x"))
	b.NoIndex()
	path, _ := b.WriteFile(t.TempDir(), "legacy.tar")
	_, err := Open(path)
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
	if !strings.Contains(err.Error(), "25") {
		t.Fatalf("message should point at Docker 25+: %v", err)
	}
}

func TestReadBlobVerifiesDigest(t *testing.T) {
	b := archivetest.New()
	wrong := oci.DigestOfBytes([]byte("what the name claims"))
	b.AddBlobAs(wrong, []byte("what is actually there"))
	b.Top()
	a := openBuilder(t, b)
	if _, err := a.ReadBlob(wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ReadBlob on a corrupt blob: %v", err)
	}
	var seen []int64
	err := a.Verify(context.Background(), wrong, func(n int64) { seen = append(seen, n) })
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Verify on a corrupt blob: %v", err)
	}
	if len(seen) == 0 || seen[len(seen)-1] != int64(len("what is actually there")) {
		t.Fatalf("Verify progress = %v", seen)
	}
}

func TestVerifyAcceptsGoodBlobAndObeysContext(t *testing.T) {
	b := archivetest.New()
	d := b.AddBlob([]byte("good bytes"))
	b.Top()
	a := openBuilder(t, b)
	if err := a.Verify(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Verify(ctx, d, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Verify: %v", err)
	}
}

func TestOpenIgnoresUnrelatedEntriesAndDotSlash(t *testing.T) {
	// Some tar writers prefix names with "./"; the reader must normalise.
	b := archivetest.New()
	d := b.AddBlob([]byte("payload"))
	b.Top()
	raw := b.Bytes()
	rewritten := rewriteNames(t, raw, func(name string) string { return "./" + name })
	path := writeTemp(t, rewritten)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.Has(d) {
		t.Fatal("./-prefixed blob not indexed")
	}
}

// rewriteNames re-encodes a tar with every entry name mapped through f.
func rewriteNames(t *testing.T, data []byte, f func(string) string) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h.Name = f(h.Name)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return out.Bytes()
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
