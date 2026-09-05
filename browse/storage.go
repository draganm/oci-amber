package browse

import (
	"fmt"
	"strings"
	"time"

	tarprism "github.com/draganm/tar-prism"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/draganm/oci-amber/blob"
	"github.com/draganm/oci-amber/rootfs"
	"github.com/draganm/oci-amber/store"
	"github.com/draganm/oci-amber/tui"
)

// fileNode is a regular file of the storage tree.
type fileNode struct {
	st     *store.Store
	name   string
	key    key.Key
	labels []KV
}

func (n *fileNode) Crumb() string { return n.name }

func (n *fileNode) Open() (*File, error) {
	if t := n.key.Type(); t != key.Blob && t != key.FileNode {
		return nil, fmt.Errorf("browse: %s is a %s, not file content", n.name, t)
	}
	st, k := n.st, n.key
	return &File{Name: n.name, Size: int64(k.Length()), Key: k, Labels: n.labels, Open: func() *store.Reader { return st.NewReader(k) }}, nil
}

// keyInfo are the info lines every storage row carries.
func keyInfo(name string, k key.Key) []KV {
	return []KV{
		{"name", name},
		{"key", k.String()},
		{"type", k.Type().String()},
		{"length", tui.FormatCount(int64(k.Length())) + " bytes"},
	}
}

// listDir decodes the entries of the directory object dir in name order.
func listDir(st *store.Store, dir key.Key) ([]rootfs.Entry, error) {
	entries, _, err := rootfs.NewFS(st, dir).List("", "", 0)
	return entries, err
}

// entryRow renders a rootfs entry's ls -l columns and info; the caller
// sets Child.
func entryRow(e rootfs.Entry) Row {
	mtime := time.Unix(0, e.Mtime).UTC()
	r := Row{Name: e.Name, IsDir: e.IsDir(), Meta: &RowMeta{Mode: e.Mode, UID: e.UID, GID: e.GID, Mtime: mtime, Target: e.Target}}
	info := []KV{
		{"name", e.Name},
		{"type", e.TypeName()},
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
		{"mtime", mtime.Format(time.RFC3339)},
	}
	switch e.Type() {
	case store.TypeReg:
		r.Size, r.HasSize = e.Size, true
		info = append(info, KV{"size", tui.FormatCount(e.Size) + " bytes"}, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeDir:
		info = append(info, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeLink:
		info = append(info, KV{"target", e.Target})
	case store.TypeChar, store.TypeBlock:
		info = append(info, KV{"device", fmt.Sprintf("%d, %d", e.Rdev[0], e.Rdev[1])})
	}
	r.Info = info
	return r
}

// entryLabels are the viewer status facts of a rootfs file.
func entryLabels(e rootfs.Entry) []KV {
	return []KV{
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
	}
}

// amberDirNode is a directory of the storage tree the browser has no
// special knowledge of: rootfs/ and everything under it, or an entry a
// future layout adds. Rows carry the entry's own metadata; symlinks are
// shown, not followed.
type amberDirNode struct {
	st     *store.Store
	name   string
	dir    key.Key
	labels []KV // inherited by files: the image or layer they belong to
}

func (n *amberDirNode) Crumb() string { return n.name }

func (n *amberDirNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s: %w", n.name, err)
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		r := entryRow(e)
		switch e.Type() {
		case store.TypeDir:
			r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: n.labels}
		case store.TypeReg:
			r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: append(entryLabels(e), n.labels...)}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// blobKind summarizes how a blob is stored: "prism gzip go-flate",
// "prism none", "raw not-tar".
func blobKind(m blob.Meta) string {
	if m.Kind == blob.KindRaw {
		return "raw " + string(m.RawReason)
	}
	s := "prism " + m.Format
	if m.Engine != "" {
		s += " " + m.Engine
	}
	return s
}

// blobLabels are the viewer status facts of a blob's parts.
func blobLabels(m blob.Meta) []KV {
	return []KV{{"blob", shortRef(m.Digest)}, {"kind", blobKind(m)}}
}

// partDetails describes the fixed entries of a blob root.
var partDetails = map[string]string{
	blob.MetaFile:       "blob metadata",
	blob.CompFile:       "zrecipe compression parameters",
	tarprism.IndexFile:  "tar-prism index: where each blob splices into the recipe",
	tarprism.RecipeFile: "tar recipe: every byte that is not file content",
	blob.RawFile:        "the blob's bytes, verbatim",
}

// blobRootNode is a blob root: the parts of a prism, or meta.json and the
// verbatim bytes of a raw blob.
type blobRootNode struct {
	st *store.Store
	bl *blob.Blob
}

func (n *blobRootNode) Crumb() string { return shortRef(n.bl.Meta.Digest) }

func (n *blobRootNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.bl.Root())
	if err != nil {
		return nil, fmt.Errorf("browse: listing blob root %s: %w", n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Name == tarprism.BlobsDir && e.IsDir():
			rows = append(rows, Row{
				Name: e.Name, Detail: plural(n.bl.Meta.Entries, "file"), IsDir: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &prismBlobsNode{st: n.st, bl: n.bl, dir: e.Content},
			})
		case e.IsRegular():
			rows = append(rows, Row{
				Name: e.Name, Detail: partDetails[e.Name], Size: e.Size, HasSize: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.st, name: e.Name, key: e.Content, labels: labels},
			})
		default:
			r := entryRow(e)
			if e.IsDir() {
				r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: labels}
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// prismBlobsNode is the blobs/ directory of a prism: one numbered file per
// regular file of the layer's tar, annotated with the tar entry's name
// from recipe.json.
type prismBlobsNode struct {
	st  *store.Store
	bl  *blob.Blob
	dir key.Key
}

func (n *prismBlobsNode) Crumb() string { return tarprism.BlobsDir }

func (n *prismBlobsNode) List() ([]Row, error) {
	prism, err := n.bl.Prism()
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	idx, err := prism.Index()
	if err != nil {
		return nil, fmt.Errorf("browse: reading %s of %s: %w", tarprism.IndexFile, n.bl.Meta.Digest, err)
	}
	byBlob := make(map[string]tarprism.Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		if name, ok := strings.CutPrefix(e.Blob, tarprism.BlobsDir+"/"); ok {
			byBlob[name] = e
		}
	}
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s of %s: %w", tarprism.BlobsDir, n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		if !e.IsRegular() {
			rows = append(rows, entryRow(e))
			continue
		}
		r := Row{Name: e.Name, Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content)}
		fileLabels := labels
		if te, ok := byBlob[e.Name]; ok {
			r.Detail = te.Name
			r.Info = append(r.Info, KV{"tar entry", te.Name}, KV{"recipe offset", tui.FormatCount(te.Offset)})
			fileLabels = append([]KV{{"file", te.Name}}, labels...)
		}
		r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: fileLabels}
		rows = append(rows, r)
	}
	return rows, nil
}
