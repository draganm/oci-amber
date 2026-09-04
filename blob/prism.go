package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	tarprism "github.com/draganm/tar-prism"
	zrecipe "github.com/draganm/zrecipe"
	"github.com/jobs-build/amber-store-core/key"
	kpgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"lukechampine.com/blake3"

	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
	"github.com/draganm/oci-amber/upload"
)

// prismResult is what pass two leaves in the store before the blob root is
// built: the keys of recipe.bin, recipe.json, blobs/ and comp.json, and the
// facts that go into meta.json.
type prismResult struct {
	recipe, index, blobs, comp key.Key
	entries                    int
	diffID                     oci.Digest
	uncompressedSize           int64
}

// decomposeError reports that the decompressed stream could not be taken
// apart: tar-prism rejected it, the decompressor failed, or the BLAKE3
// digest or length differs from what pass one recorded. The blob is stored
// raw with ReasonDecomposeFailed; the upload does not fail.
type decomposeError struct{ err error }

func (e *decomposeError) Error() string { return "decompose: " + e.err.Error() }
func (e *decomposeError) Unwrap() error { return e.err }

// sinkError marks an amber write failure inside the sink. The upload fails.
type sinkError struct{ err error }

func (e *sinkError) Error() string { return "amber sink: " + e.err.Error() }
func (e *sinkError) Unwrap() error { return e.err }

// readError marks an I/O failure on the upload spool. The upload fails.
type readError struct{ err error }

func (e *readError) Error() string { return "spool: " + e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// rawFallback is returned by finalizePrism when the spec downgrades the
// blob to raw instead of failing the upload: reason is decompose-failed or
// roundtrip-failed, err is what went wrong.
type rawFallback struct {
	reason RawReason
	err    error
}

func (e *rawFallback) Error() string { return string(e.reason) + ": " + e.err.Error() }
func (e *rawFallback) Unwrap() error { return e.err }

// spoolReader tags errors coming from the upload spool so they can be told
// apart from tar-prism and decompressor errors after DecomposeTo returns.
// klauspost's gzip and zstd readers, tar-prism's bufio and io.TeeReader all
// pass the underlying reader's error through unchanged.
type spoolReader struct{ r io.Reader }

func (s *spoolReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &readError{err}
	}
	return n, err
}

// byteCounter counts the bytes read through it.
type byteCounter struct {
	r io.Reader
	n int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// newDecompressor returns a reader over the decompressed form of r for the
// format pass one detected: klauspost gzip, klauspost zstd bounded like
// zrecipe's own first pass, or r itself for "none". Closing it does not
// close r.
//
// The zstd decoder's max window matches maxZstdWindow (analyze.go): analyze
// already turns away any frame that declares a bigger window before this is
// ever called, so the bound here is a second, independent guard against
// this decoder ever growing a window buffer past what the store accepts.
func newDecompressor(format zrecipe.Format, r io.Reader) (io.ReadCloser, error) {
	switch format {
	case zrecipe.FormatGzip:
		zr, err := kpgzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zr, nil
	case zrecipe.FormatZstd:
		dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxZstdWindow))
		if err != nil {
			return nil, err
		}
		return dec.IOReadCloser(), nil
	case zrecipe.FormatNone:
		return io.NopCloser(r), nil
	default:
		return nil, fmt.Errorf("blob: unknown format %q", format)
	}
}

// amberSink implements tarprism.Sink over a store.Writer. The recipe streams
// through a pipe into PutStream on its own goroutine; each blob is chunked
// straight from tar-prism's reader; the index is stored exactly as
// tar-prism's own recipe.json (EncodeIndex).
type amberSink struct {
	w        *store.Writer
	blobs    *store.Dir
	recipe   *recipeWriter
	index    key.Key
	hasIndex bool
	entries  int
}

func newAmberSink(w *store.Writer) *amberSink {
	return &amberSink{w: w, blobs: w.NewDir()}
}

// closeRecipe closes the recipe writer if tar-prism ever requested one via
// Recipe(). recipeWriter.Close is idempotent (guarded by sync.Once), so
// ingestPrism can defer this right after newAmberSink and still let finish()
// close the recipe again on the success path: a panic or an early return
// from a failed DecomposeTo can never leave the PutStream goroutine feeding
// recipe.bin blocked on the pipe waiting for a Close that never comes.
func (s *amberSink) closeRecipe() {
	if s.recipe != nil {
		s.recipe.Close()
	}
}

// Recipe starts the recipe goroutine and hands tar-prism the pipe writer.
func (s *amberSink) Recipe() (io.WriteCloser, error) {
	if s.recipe != nil {
		return nil, &sinkError{errors.New("recipe requested twice")}
	}
	pr, pw := io.Pipe()
	rw := &recipeWriter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(rw.done)
		k, err := s.w.PutStream(pr)
		if err != nil {
			rw.err = err
			// Fail tar-prism's next recipe write with the store's error
			// instead of blocking it on a pipe nobody reads.
			pr.CloseWithError(err)
			return
		}
		rw.key = k
	}()
	s.recipe = rw
	return rw, nil
}

// Blob stores exactly entry.Size bytes of r as blobs/%08d.
func (s *amberSink) Blob(index int, entry tarprism.Entry, r io.Reader) error {
	name := fmt.Sprintf("%08d", index+1)
	if entry.Blob != tarprism.BlobsDir+"/"+name {
		return &sinkError{fmt.Errorf("entry %d (%s): unexpected blob path %q", index, entry.Name, entry.Blob)}
	}
	if entry.Size < 0 {
		return &sinkError{fmt.Errorf("entry %d (%s): negative size %d", index, entry.Name, entry.Size)}
	}
	k, err := s.w.PutStream(io.LimitReader(r, entry.Size))
	if err != nil {
		var re *readError
		if errors.As(err, &re) {
			return err
		}
		return &sinkError{err}
	}
	if err := s.blobs.AddFile(name, k); err != nil {
		return &sinkError{err}
	}
	return nil
}

// Index stores recipe.json.
func (s *amberSink) Index(idx *tarprism.Index) error {
	data, err := tarprism.EncodeIndex(idx)
	if err != nil {
		return &sinkError{err}
	}
	k, err := s.w.PutBytes(data)
	if err != nil {
		return &sinkError{err}
	}
	s.index = k
	s.hasIndex = true
	s.entries = len(idx.Entries)
	return nil
}

// finish waits for the recipe, checks that tar-prism delivered an index and
// closes the blobs directory.
func (s *amberSink) finish() (recipe, blobs key.Key, err error) {
	if s.recipe == nil {
		return key.Key{}, key.Key{}, &decomposeError{errors.New("tar-prism did not request a recipe")}
	}
	if err := s.recipe.Close(); err != nil {
		return key.Key{}, key.Key{}, err
	}
	if !s.hasIndex {
		return key.Key{}, key.Key{}, &decomposeError{errors.New("tar-prism did not deliver an index")}
	}
	blobs, err = s.blobs.Finish()
	if err != nil {
		return key.Key{}, key.Key{}, &sinkError{err}
	}
	return s.recipe.key, blobs, nil
}

// recipeWriter is the writer end of the recipe pipe. Close closes the pipe
// and waits for the PutStream goroutine; it is idempotent, which matters
// because DecomposeTo closes the recipe on every failure path and finish
// closes it again.
type recipeWriter struct {
	pw   *io.PipeWriter
	done chan struct{}
	once sync.Once
	key  key.Key
	err  error
}

func (rw *recipeWriter) Write(p []byte) (int, error) {
	n, err := rw.pw.Write(p)
	if err != nil {
		return n, &sinkError{err}
	}
	return n, nil
}

func (rw *recipeWriter) Close() error {
	rw.once.Do(func() {
		rw.pw.Close()
		<-rw.done
	})
	if rw.err != nil {
		return &sinkError{rw.err}
	}
	return nil
}

// ingestPrism runs pass two: reopen the spool at offset 0, decompress it
// with klauspost, hash the stream with BLAKE3 and sha256, take the tar
// apart into the store and write comp.json. It returns a *decomposeError
// when the blob should fall back to raw and any other error when the upload
// must fail.
func (b *Store) ingestPrism(ctx context.Context, w *store.Writer, sp *upload.Spool, params *zrecipe.Params) (prismResult, error) {
	src, err := sp.Open()
	if err != nil {
		return prismResult{}, fmt.Errorf("blob: opening spool: %w", err)
	}
	if c, ok := src.(io.Closer); ok {
		defer c.Close()
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return prismResult{}, fmt.Errorf("blob: rewinding spool: %w", err)
	}
	rd := b.observeReader(sp.Digest(), src)

	dec, err := newDecompressor(params.Format, &spoolReader{r: rd})
	if err != nil {
		var re *readError
		if errors.As(err, &re) {
			return prismResult{}, err
		}
		return prismResult{}, &decomposeError{err}
	}
	defer dec.Close()

	b3 := blake3.New(32, nil)
	s256 := sha256.New()
	counter := &byteCounter{r: io.TeeReader(dec, io.MultiWriter(b3, s256))}

	sink := newAmberSink(w)
	defer sink.closeRecipe()
	if err := tarprism.DecomposeTo(counter, sink); err != nil {
		return prismResult{}, classifyDecomposeError(ctx, err)
	}
	recipe, blobs, err := sink.finish()
	if err != nil {
		return prismResult{}, err
	}
	if got := hex.EncodeToString(b3.Sum(nil)); got != params.Uncompressed.Blake3 || counter.n != params.Uncompressed.Size {
		return prismResult{}, &decomposeError{fmt.Errorf("decompressed stream is %s/%d, pass one recorded %s/%d", got, counter.n, params.Uncompressed.Blake3, params.Uncompressed.Size)}
	}
	var comp bytes.Buffer
	if err := params.Write(&comp); err != nil {
		return prismResult{}, fmt.Errorf("blob: encoding %s: %w", CompFile, err)
	}
	compKey, err := w.PutBytes(comp.Bytes())
	if err != nil {
		return prismResult{}, fmt.Errorf("blob: storing %s: %w", CompFile, err)
	}
	return prismResult{
		recipe:           recipe,
		index:            sink.index,
		blobs:            blobs,
		comp:             compKey,
		entries:          sink.entries,
		diffID:           oci.DigestFromSum(s256.Sum(nil)),
		uncompressedSize: counter.n,
	}, nil
}

// classifyDecomposeError decides whether a DecomposeTo failure fails the
// upload (request context done, spool I/O, amber write) or downgrades the
// blob to raw (anything else: tar-prism rejecting the archive, the
// decompressor failing).
func classifyDecomposeError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var se *sinkError
	if errors.As(err, &se) {
		return err
	}
	var re *readError
	if errors.As(err, &re) {
		return err
	}
	return &decomposeError{err}
}

// finalizePrism runs pass two through its own accounting writer and, when
// VerifyRoundTrip is set, the pull pipeline over the fresh objects. It
// returns a *rawFallback when the spec stores the blob raw instead
// (decompose-failed, roundtrip-failed; both logged at error level), the
// context's error when the request went away, and any other error when the
// upload must fail. Objects written before a failure are left to GC.
func (b *Store) finalizePrism(ctx context.Context, sp *upload.Spool, params *zrecipe.Params, d oci.Digest) (prismResult, store.Stats, error) {
	w := b.st.NewWriter(ctx)
	b.observeStage(d, StageDecompose)
	res, err := b.ingestPrism(ctx, w, sp, params)
	if err != nil {
		w.Abort()
		if cerr := ctx.Err(); cerr != nil {
			return prismResult{}, store.Stats{}, cerr
		}
		var de *decomposeError
		if errors.As(err, &de) {
			b.log.Error("decompose failed, storing raw", "digest", d, "format", params.Format, "engine", params.Engine, "error", err)
			return prismResult{}, store.Stats{}, &rawFallback{reason: ReasonDecomposeFailed, err: err}
		}
		return prismResult{}, store.Stats{}, err
	}
	stats, err := w.Close()
	if err != nil {
		return prismResult{}, store.Stats{}, err
	}
	if b.opts.VerifyRoundTrip {
		b.observeStage(d, StageVerify)
		// Read comp.json back from the store rather than reusing the
		// in-memory params pass one produced: the check must exercise
		// exactly the bytes a real pull will read (spec I5).
		storedParams, err := b.readCompParams(res.comp)
		if err != nil {
			return prismResult{}, store.Stats{}, fmt.Errorf("blob: reading back %s for round-trip check: %w", CompFile, err)
		}
		src := &Prism{st: b.st, recipe: res.recipe, index: res.index, blobs: res.blobs}
		if err := roundTripCheck(ctx, b, src, storedParams, d); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return prismResult{}, store.Stats{}, cerr
			}
			b.log.Error("round-trip verification failed, storing raw", "digest", d, "format", params.Format, "engine", params.Engine, "engine_version", params.EngineVersion, "error", err)
			return prismResult{}, store.Stats{}, &rawFallback{reason: ReasonRoundTripFailed, err: err}
		}
	}
	return res, stats, nil
}

// roundTripCheck runs the pull pipeline over freshly ingested prism objects
// into a sha256 and compares the result with the OCI digest. It is a
// variable so tests can force a failure or count calls.
var roundTripCheck = func(ctx context.Context, b *Store, src *Prism, params *zrecipe.Params, want oci.Digest) error {
	h := sha256.New()
	if err := composeRecompress(ctx, b.observeWriter(want, h), src, params); err != nil {
		return err
	}
	if got := oci.DigestFromSum(h.Sum(nil)); got != want {
		return fmt.Errorf("round trip produced %s, want %s", got, want)
	}
	return nil
}

// Prism is one prism blob's parts as tar-prism's Source: recipe.bin and
// every blob are streamed with store.Reader, recipe.json is read whole.
// BlobKey exposes a file's content key so that a tree can reference it
// without reading it.
type Prism struct {
	st     *store.Store
	recipe key.Key
	index  key.Key
	blobs  key.Key
}

func (s *Prism) Index() (*tarprism.Index, error) {
	data, err := s.st.ReadFile(s.index)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", tarprism.IndexFile, err)
	}
	return tarprism.DecodeIndex(data)
}

func (s *Prism) Recipe() (io.ReadCloser, error) {
	return s.st.NewReader(s.recipe), nil
}

// BlobKey returns the content key of entry's blob, checking that the stored
// length matches the index.
func (s *Prism) BlobKey(index int, entry tarprism.Entry) (key.Key, error) {
	name, ok := strings.CutPrefix(entry.Blob, tarprism.BlobsDir+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return key.Key{}, fmt.Errorf("entry %d (%s): blob path %q is not directly under %s/", index, entry.Name, entry.Blob, tarprism.BlobsDir)
	}
	k, err := s.st.LookupKey(s.blobs, name)
	if err != nil {
		return key.Key{}, fmt.Errorf("entry %d (%s): %w", index, entry.Name, err)
	}
	if int64(k.Length()) != entry.Size {
		return key.Key{}, fmt.Errorf("entry %d (%s): blob %s is %d bytes, index says %d", index, entry.Name, entry.Blob, k.Length(), entry.Size)
	}
	return k, nil
}

func (s *Prism) Blob(index int, entry tarprism.Entry) (io.ReadCloser, error) {
	k, err := s.BlobKey(index, entry)
	if err != nil {
		return nil, err
	}
	return s.st.NewReader(k), nil
}

// openSource resolves the prism files of a blob root.
func (b *Store) openSource(root key.Key) (*Prism, error) {
	src := &Prism{st: b.st}
	var err error
	if src.recipe, err = b.st.LookupKey(root, tarprism.RecipeFile); err != nil {
		return nil, fmt.Errorf("blob: %s: %w", tarprism.RecipeFile, err)
	}
	if src.index, err = b.st.LookupKey(root, tarprism.IndexFile); err != nil {
		return nil, fmt.Errorf("blob: %s: %w", tarprism.IndexFile, err)
	}
	if src.blobs, err = b.st.LookupKey(root, blobsDirName); err != nil {
		return nil, fmt.Errorf("blob: %s: %w", blobsDirName, err)
	}
	return src, nil
}

// readParams reads comp.json of a prism root.
func (b *Store) readParams(root key.Key) (*zrecipe.Params, error) {
	k, err := b.st.LookupKey(root, CompFile)
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w", CompFile, err)
	}
	return b.readCompParams(k)
}

// readCompParams reads and decodes a comp.json object already stored under
// key k, the same way a pull does. finalizePrism uses it directly (k is
// res.comp, not yet under a root) so the round-trip check exercises exactly
// the bytes a real pull will read instead of the in-memory params pass one
// produced.
func (b *Store) readCompParams(k key.Key) (*zrecipe.Params, error) {
	data, err := b.st.ReadFile(k)
	if err != nil {
		return nil, fmt.Errorf("blob: reading %s: %w", CompFile, err)
	}
	params, err := zrecipe.ReadParams(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w", CompFile, err)
	}
	return params, nil
}

// composePipeSlots is how many writes the composer may be ahead of the
// recompressor. tar-prism writes through a 1 MiB buffer, so this is about
// 8 MiB in flight per compose, enough for the two to overlap fully: on a
// 228 MiB layer the lockstep of an unbuffered pipe cost 5.1 s against 3.8 s
// with this queue.
const composePipeSlots = 8

// composeRecompress streams the prism through tar-prism and zrecipe into
// w: ComposeFrom runs on a goroutine feeding a queued pipe that Recompress
// reads, so composing (store reads) and recompressing (CPU) overlap.
// Nothing touches the disk. Once ctx is done the pipe is closed at once, so
// a client that went away does not make Recompress compose and drain the
// whole layer for its digest check; the context's error is then reported.
func composeRecompress(ctx context.Context, w io.Writer, src *Prism, params *zrecipe.Params) error {
	p := newPipe(composePipeSlots)
	composed := make(chan error, 1)
	go func() {
		err := tarprism.ComposeFrom(src, p)
		p.CloseWrite(err)
		composed <- err
	}()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.CloseRead()
		case <-stop:
		}
	}()
	rerr := zrecipe.Recompress(ctx, params, p, w, &zrecipe.RecompressOptions{AllowVersionMismatch: true})
	close(stop)
	// Unblock the composer if Recompress stopped early, then collect it.
	p.CloseRead()
	cerr := <-composed
	if rerr != nil || cerr != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("blob: pull cancelled: %w", err)
		}
	}
	switch {
	case cerr != nil && !errors.Is(cerr, io.ErrClosedPipe):
		if rerr != nil {
			return fmt.Errorf("blob: compose: %w (recompress: %v)", cerr, rerr)
		}
		return fmt.Errorf("blob: compose: %w", cerr)
	case rerr != nil:
		return fmt.Errorf("blob: recompress: %w", rerr)
	}
	return nil
}

// writePrism rebuilds the original blob bytes of a prism root into w.
func (b *Store) writePrism(ctx context.Context, w io.Writer, root key.Key, params *zrecipe.Params) error {
	src, err := b.openSource(root)
	if err != nil {
		return err
	}
	return composeRecompress(ctx, w, src, params)
}
