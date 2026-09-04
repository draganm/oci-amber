package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	compprysm "github.com/draganm/comp-prysm"

	"github.com/draganm/oci-amber/upload"
)

// decision is the outcome of pass one: how the blob will be stored.
type decision struct {
	kind   Kind
	reason RawReason         // set for KindRaw
	params *compprysm.Params // set for KindPrism
	format string            // "gzip" | "zstd" | "none", always set
}

// tarHeaderSize is one tar block; the first block of a tar is a header.
const tarHeaderSize = 512

// spoolDir is the directory comp-prysm spills its decompressed spool to.
func (b *Store) spoolDir() string { return filepath.Join(b.opts.WorkDir, spoolDirName) }

// analyze runs comp-prysm's first pass under the analyze deadline and
// classifies the result (spec finalization steps 4 and 5). It returns an
// error only for failures that must fail the upload: the request context
// ended, an I/O error, an unexpected comp-prysm error. Every fallback case
// is a raw decision carrying its reason.
func (b *Store) analyze(ctx context.Context, sp *upload.Spool) (decision, error) {
	if err := ctx.Err(); err != nil {
		return decision{}, fmt.Errorf("blob: analyze: %w", err)
	}
	r, err := sp.Open()
	if err != nil {
		return decision{}, fmt.Errorf("blob: opening spool: %w", err)
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}

	// Detect first so a raw fallback still records the container format.
	f, err := compprysm.Detect(r)
	if err != nil {
		return decision{}, fmt.Errorf("blob: detecting format: %w", err)
	}
	format := string(f)

	// A compressed blob whose first decompressed block is not a tar header
	// can never become a prism, so decide it here rather than after a full
	// engine search and a pass two that tar-prism is bound to reject (spec
	// "Blob finalization" step 5). The probe decompresses one 512-byte
	// block through klauspost; nothing is spooled.
	if f != compprysm.FormatNone {
		ok, err := startsWithCompressedTarHeader(r, f)
		if err == nil && !ok {
			return decision{kind: KindRaw, reason: ReasonNotTar, format: format}, nil
		}
		// A probe that could not read the stream decides nothing: Analyze
		// runs and reports a corrupt or unsupported stream with its own
		// reason, or fails the upload on a spool I/O error.
	}

	actx, cancel := context.WithTimeout(ctx, b.opts.AnalyzeTimeout)
	defer cancel()
	params, err := compprysm.Analyze(actx, r, &compprysm.Options{
		TempDir:     b.spoolDir(),
		MaxInMemory: b.opts.MaxInMemory,
		Parallelism: b.opts.AnalyzeParallelism,
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return decision{}, fmt.Errorf("blob: analyze: %w", ctx.Err())
		case errors.Is(err, context.DeadlineExceeded):
			// Only the child deadline can have expired here.
			return decision{kind: KindRaw, reason: ReasonAnalyzeTimeout, format: format}, nil
		case errors.Is(err, compprysm.ErrNotReproducible):
			return decision{kind: KindRaw, reason: ReasonNotReproducible, format: format}, nil
		case errors.Is(err, compprysm.ErrUnsupported):
			return decision{kind: KindRaw, reason: ReasonUnsupported, format: format}, nil
		case errors.Is(err, compprysm.ErrCorrupt):
			return decision{kind: KindRaw, reason: ReasonCorrupt, format: format}, nil
		default:
			return decision{}, fmt.Errorf("blob: analyze: %w", err)
		}
	}
	// Analyze does not observe ctx on uncompressed input.
	if err := ctx.Err(); err != nil {
		return decision{}, fmt.Errorf("blob: analyze: %w", err)
	}
	format = string(params.Format)
	if params.Format == compprysm.FormatNone {
		ok, err := startsWithTarHeader(r)
		if err != nil {
			return decision{}, fmt.Errorf("blob: reading tar header: %w", err)
		}
		if !ok {
			return decision{kind: KindRaw, reason: ReasonNotTar, format: format}, nil
		}
	}
	return decision{kind: KindPrism, params: params, format: format}, nil
}

// startsWithCompressedTarHeader reports whether the first 512 decompressed
// bytes of r are a tar header block. It rewinds r and reads through a
// klauspost decompressor for f, which pulls only as much of the compressed
// stream as that one block needs; nothing is written to disk. Analyze
// rewinds r itself (its Detect does), so the position left behind does not
// matter. A stream with fewer than 512 decompressed bytes is not a tar; any
// other failure returns an error and decides nothing.
func startsWithCompressedTarHeader(r io.ReadSeeker, f compprysm.Format) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	dec, err := newDecompressor(f, r)
	if err != nil {
		return false, err
	}
	defer dec.Close()
	var hdr [tarHeaderSize]byte
	if _, err := io.ReadFull(dec, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return isTarHeader(hdr[:]), nil
}

// startsWithTarHeader reports whether r begins with a tar header block
// carrying a valid checksum.
func startsWithTarHeader(r io.ReadSeeker) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	var hdr [tarHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return isTarHeader(hdr[:]), nil
}

// isTarHeader verifies the checksum of a 512-byte tar header: the sum of
// all bytes with the checksum field (148..155) taken as spaces, accepting
// the signed variant some old archivers wrote.
func isTarHeader(b []byte) bool {
	if len(b) < tarHeaderSize {
		return false
	}
	var unsigned, signed int64
	for i, c := range b[:tarHeaderSize] {
		if i >= 148 && i < 156 {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}
	field := strings.Trim(string(b[148:156]), " \x00")
	if field == "" {
		return false
	}
	want, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	return want == unsigned || want == signed
}
