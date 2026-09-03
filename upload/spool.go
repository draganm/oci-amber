// Package upload holds in-progress blob uploads. A Session buffers the bytes
// of one upload in memory and spills them to a file in the work directory
// once they exceed a threshold, keeping a running sha256 over everything
// received. A Manager owns the sessions of one registry process and expires
// the ones that go idle. A Spool is the snapshot a finished upload hands to
// the blob store.
package upload

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/draganm/oci-amber/oci"
)

// ReaderAtSeeker is the view of an upload that blob finalization consumes: a
// seekable stream that can also be read at arbitrary offsets, so comp-prysm
// can evaluate candidate engines in parallel over one open spool.
type ReaderAtSeeker interface {
	io.ReadSeeker
	io.ReaderAt
}

var errSpoolRemoved = errors.New("upload: spool removed")

// Spool is a snapshot of an upload's bytes, backed either by a byte slice or
// by a file under the manager's directory. Its size and digest are fixed
// when the snapshot is taken; bytes appended to the session afterwards are
// not visible through it.
type Spool struct {
	size    int64
	digest  oci.Digest
	mem     []byte
	path    string
	touch   func()
	removed bool
}

// NewMemorySpool returns a Spool over data. The slice is not copied.
func NewMemorySpool(data []byte) *Spool {
	return &Spool{size: int64(len(data)), digest: oci.DigestOfBytes(data), mem: data}
}

// Size is the number of bytes in the spool.
func (sp *Spool) Size() int64 { return sp.size }

// Digest is the sha256 digest of the spool's bytes.
func (sp *Spool) Digest() oci.Digest { return sp.digest }

// Open returns a reader positioned at offset 0. For a memory spool it is a
// *bytes.Reader; for a file spool it is backed by a freshly opened *os.File
// and implements io.Closer, which the caller must close. Open may be called
// several times and the readers' ReadAt may be used concurrently.
func (sp *Spool) Open() (ReaderAtSeeker, error) {
	if sp.removed {
		return nil, errSpoolRemoved
	}
	if sp.touch != nil {
		sp.touch()
	}
	if sp.path == "" {
		return bytes.NewReader(sp.mem), nil
	}
	f, err := os.Open(sp.path)
	if err != nil {
		return nil, err
	}
	return &fileReader{SectionReader: io.NewSectionReader(f, 0, sp.size), f: f}, nil
}

// Remove deletes the backing file, if any, and drops the reference to the
// memory buffer. It is idempotent and succeeds when the file is already
// gone. Open fails after Remove.
func (sp *Spool) Remove() error {
	sp.removed = true
	sp.mem = nil
	if sp.path == "" {
		return nil
	}
	if err := os.Remove(sp.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// fileReader restricts an open file to the spool's size so the reader is a
// true snapshot even if the session's file grows afterwards.
type fileReader struct {
	*io.SectionReader
	f *os.File
}

// Close closes the underlying file.
func (r *fileReader) Close() error { return r.f.Close() }
