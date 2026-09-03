package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"
)

var errDirFinished = errors.New("store: dir already finished")

// Dir builds one directory object through a Writer. Entries must be added
// in strictly increasing bytewise name order, exactly as fstree.DirBuilder
// requires; every entry has UID, GID and Mtime zero and Mode ModeFile or
// ModeDir, so identical content always yields identical keys. A Dir is not
// safe for concurrent use; build sibling content concurrently, then add the
// entries from one goroutine.
type Dir struct {
	w    *Writer
	db   *fstree.DirBuilder
	last string
	n    int
	done bool
}

// NewDir starts an empty directory whose objects flow through w.
func (w *Writer) NewDir() *Dir {
	return &Dir{w: w, db: fstree.NewDirBuilder(w.ic)}
}

// AddFile adds a regular-file entry whose content is the Blob or FileNode
// under content (a PutStream result).
func (d *Dir) AddFile(name string, content key.Key) error {
	if t := content.Type(); t != key.Blob && t != key.FileNode {
		return fmt.Errorf("store: dir entry %q: content key %s is a %s, not file content", name, content, t)
	}
	return d.add(name, ModeFile, content)
}

// AddDir adds a subdirectory entry whose content is the DirLeaf or DirNode
// under content (a Dir.Finish result).
func (d *Dir) AddDir(name string, content key.Key) error {
	if t := content.Type(); t != key.DirLeaf && t != key.DirNode {
		return fmt.Errorf("store: dir entry %q: content key %s is a %s, not a directory", name, content, t)
	}
	return d.add(name, ModeDir, content)
}

func (d *Dir) add(name string, mode uint64, content key.Key) error {
	if d.done {
		return errDirFinished
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("store: invalid dir entry name %q", name)
	}
	if d.n > 0 && name <= d.last {
		return fmt.Errorf("store: dir entry %q not sorted after %q", name, d.last)
	}
	e := fstree.Entry{
		Name:       []byte(name),
		Mode:       mode,
		ContentKey: content[:],
	}
	if err := d.db.AddEntry(d.w.Emit, e); err != nil {
		return err
	}
	d.last = name
	d.n++
	return nil
}

// Finish closes the directory and returns its root key (a DirLeaf for small
// directories, including an empty one; a DirNode once the entries span more
// than one leaf). The Dir cannot be used afterwards.
func (d *Dir) Finish() (key.Key, error) {
	if d.done {
		return key.Key{}, errDirFinished
	}
	d.done = true
	return d.db.Finish(d.w.Emit)
}
