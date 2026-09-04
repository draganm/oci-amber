package blob

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	zrecipe "github.com/draganm/zrecipe"
	cpformat "github.com/draganm/zrecipe/format"

	"github.com/draganm/oci-amber/upload"
)

// decision is the outcome of pass one: how the blob will be stored.
type decision struct {
	kind   Kind
	reason RawReason       // set for KindRaw
	params *zrecipe.Params // set for KindPrism
	format string          // "gzip" | "zstd" | "none", always set
}

// tarHeaderSize is one tar block; the first block of a tar is a header.
const tarHeaderSize = 512

// maxZstdWindow is the largest zstd window this store will decompress. Both
// this check and newDecompressor's zstd.WithDecoderMaxWindow enforce it, so
// a frame that declares a bigger window is never decompressed at all.
const maxZstdWindow = 128 << 20

// spoolDir is the directory zrecipe spills its decompressed spool to.
func (b *Store) spoolDir() string { return filepath.Join(b.opts.WorkDir, spoolDirName) }

// analyze runs zrecipe's first pass under the analyze deadline and
// classifies the result (spec finalization steps 4 and 5). It returns an
// error only for failures that must fail the upload: the request context
// ended, an I/O error, an unexpected zrecipe error. Every fallback case
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
	r = b.observeReader(sp.Digest(), r)

	// Detect first so a raw fallback still records the container format.
	f, err := zrecipe.Detect(r)
	if err != nil {
		return decision{}, fmt.Errorf("blob: detecting format: %w", err)
	}
	format := string(f)

	// A zstd frame that declares a window past what this store will ever
	// decompress is decided from its header alone, before anything is
	// decompressed: parsing costs a handful of bytes, where letting
	// newDecompressor's bounded decoder discover the same fact mid-probe or
	// mid-Analyze would mean it already started reading compressed data
	// under a window budget it cannot honor.
	if f == zrecipe.FormatZstd {
		windowSize, err := zstdWindowSize(r)
		if err == nil && windowSize > maxZstdWindow {
			b.log.Info("zstd window exceeds limit, storing raw",
				"window_size", windowSize, "limit", int64(maxZstdWindow))
			return decision{kind: KindRaw, reason: ReasonUnsupported, format: format}, nil
		}
		// A header that cannot be parsed decides nothing: Analyze runs and
		// reports its own corrupt/unsupported classification.
	}

	// A compressed blob whose first decompressed block is not a tar header
	// can never become a prism, so decide it here rather than after a full
	// engine search and a pass two that tar-prism is bound to reject (spec
	// "Blob finalization" step 5). The probe decompresses one 512-byte
	// block through klauspost; nothing is spooled.
	if f != zrecipe.FormatNone {
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
	params, err := zrecipe.Analyze(actx, r, &zrecipe.Options{
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
		case errors.Is(err, zrecipe.ErrNotReproducible):
			return decision{kind: KindRaw, reason: ReasonNotReproducible, format: format}, nil
		case errors.Is(err, zrecipe.ErrUnsupported):
			return decision{kind: KindRaw, reason: ReasonUnsupported, format: format}, nil
		case errors.Is(err, zrecipe.ErrCorrupt):
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
	if params.Format == zrecipe.FormatNone {
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
// bytes of r are a tar header block or the stream is an empty archive (see
// probeTar). It rewinds r and reads through a klauspost decompressor for f,
// which pulls only as much of the compressed stream as the probe needs;
// nothing is written to disk. Analyze rewinds r itself (its Detect does),
// so the position left behind does not matter. A stream with fewer than 512
// decompressed bytes is not a tar; any other failure returns an error and
// decides nothing.
func startsWithCompressedTarHeader(r io.ReadSeeker, f zrecipe.Format) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	dec, err := newDecompressor(f, r)
	if err != nil {
		return false, err
	}
	defer dec.Close()
	return probeTar(dec)
}

// zstdWindowSize parses the zstd frame header of r and returns its declared
// window size, without decompressing anything. It rewinds r first; Analyze
// and the other probes rewind it themselves before use, so the position
// left behind does not matter.
func zstdWindowSize(r io.ReadSeeker) (uint64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	h, err := cpformat.ParseZstdFrameHeader(bufio.NewReader(r))
	if err != nil {
		return 0, err
	}
	return h.WindowSize, nil
}

// startsWithTarHeader reports whether r begins with a tar header block
// carrying a valid checksum or is an empty archive (see probeTar).
func startsWithTarHeader(r io.ReadSeeker) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	return probeTar(r)
}

// emptyArchiveMax is the longest all-zero stream accepted as an empty tar
// archive: two zero blocks end an archive and GNU tar pads to its 10 KiB
// record size.
const emptyArchiveMax = 10240

// probeTar reports whether r starts like a tar archive: a header block with
// a valid checksum, or an empty archive, which is nothing but zero blocks
// and at most emptyArchiveMax bytes long. A stream shorter than one block
// is not a tar; a read error decides nothing.
func probeTar(r io.Reader) (bool, error) {
	var hdr [tarHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	if isTarHeader(hdr[:]) {
		return true, nil
	}
	if !bytes.Equal(hdr[:], make([]byte, tarHeaderSize)) {
		return false, nil
	}
	rest, err := io.ReadAll(io.LimitReader(r, emptyArchiveMax-tarHeaderSize+1))
	if err != nil {
		return false, err
	}
	if len(rest) > emptyArchiveMax-tarHeaderSize {
		return false, nil
	}
	for _, c := range rest {
		if c != 0 {
			return false, nil
		}
	}
	return true, nil
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
