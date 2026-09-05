package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/jobs-build/amber-store-core/amberpack"
	"github.com/jobs-build/amber-store-core/packstore"
)

// Pack is a staged pack file: the objects a pack Writer received, encoded
// as records in amberpack's wire format, in a temp file that was unlinked
// as soon as it was created (the descriptor keeps it alive; a crash leaves
// nothing behind). A live Writer's AddPack inserts it into the store, at
// most once; Close releases it.
type Pack struct {
	f    *os.File
	size int64
	used bool
}

// Size is the pack file's length in bytes.
func (p *Pack) Size() int64 { return p.size }

// Close releases the file.
func (p *Pack) Close() error { return p.f.Close() }

// NewPackWriter starts a Writer that stages its objects in a pack file
// under dir instead of the store (spec "Store package"): a caller that
// does not yet know whether it will keep them, such as blob's speculative
// decompose, builds exactly what a live Writer would build, and a live
// Writer's AddPack later inserts the pack with the usual dedup,
// verification and accounting, or Close drops it. Objects are encoded on
// writers() goroutines and appended in whatever order they finish; the
// order of records in a pack carries no meaning. Close writes the pack's
// end marker and returns Stats holding LogicalBytes only, since dedup is
// unknown until AddPack; the pack is then available from Pack. Abort, a
// cancelled context or a failed Close release the file.
func (s *Store) NewPackWriter(ctx context.Context, dir string) (*Writer, error) {
	f, err := os.CreateTemp(dir, "pack-*")
	if err != nil {
		return nil, fmt.Errorf("store: creating pack file: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: unlinking pack file: %w", err)
	}
	w := s.newWriter(ctx)
	w.pack = &Pack{f: f}
	go w.runPack()
	return w, nil
}

// Pack returns the staged pack after a successful Close, and nil before
// Close, after Abort, or when Close reported an error (the file is
// released then).
func (w *Writer) Pack() *Pack {
	if !w.sealed {
		return nil
	}
	return w.pack
}

// runPack is the pack backend's goroutine: it records stage's outcome and
// signals done, exactly as run does for WriteParallel.
func (w *Writer) runPack() {
	defer close(w.done)
	w.werr = w.stage()
}

// stage drains the item channel into the pack file until the channel is
// closed or the context ends. writers() goroutines encode; a mutex
// serializes their appends and the accounting. The first failure cancels
// the others and is returned; the file is left as it is for settle to
// release.
func (w *Writer) stage() error {
	pw := amberpack.NewWriter(w.pack.f)
	ctx, stop := context.WithCancelCause(w.ctx)
	defer stop(nil)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for range writers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var it item
				select {
				case <-ctx.Done():
					return
				case i, ok := <-w.ch:
					if !ok {
						return
					}
					it = i
				}
				rec, err := amberpack.EncodeRecord(it.obj.Key, it.obj.Data)
				if err != nil {
					stop(err)
					return
				}
				mu.Lock()
				err = pw.AddRecord(rec)
				if err == nil {
					w.logical += it.logical
				}
				mu.Unlock()
				if err != nil {
					stop(fmt.Errorf("store: writing pack: %w", err))
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("store: finishing pack: %w", err)
	}
	fi, err := w.pack.f.Stat()
	if err != nil {
		return fmt.Errorf("store: pack size: %w", err)
	}
	w.pack.size = fi.Size()
	return nil
}

// AddPack offers every record of p to the store through the live Writer's
// pipeline, so dedup, verification and accounting are exactly what a
// freshly built object gets and Stats keep their meaning, with a record's
// uncompressed length counted as its logical bytes. progress, when not
// nil, is called after each record with the pack bytes read so far. A
// malformed pack fails with an error wrapping amberpack.ErrMalformed and
// poisons the Writer, so Close reports it too. Only a live Writer accepts
// a pack, and a pack only once.
func (w *Writer) AddPack(p *Pack, progress func(read int64)) error {
	if w.pack != nil {
		return errors.New("store: AddPack on a pack writer")
	}
	if p == nil {
		return errors.New("store: AddPack: nil pack")
	}
	if p.used {
		return errors.New("store: pack already added")
	}
	p.used = true
	if _, err := p.f.Seek(0, io.SeekStart); err != nil {
		return w.fail(fmt.Errorf("store: rewinding pack: %w", err))
	}
	cr := &countingReader{r: p.f}
	for raw, err := range amberpack.NewReader(cr).Records() {
		if err != nil {
			return w.fail(fmt.Errorf("store: reading pack: %w", err))
		}
		it := item{obj: packstore.Object{Key: raw.Key, Record: raw.Bytes}, logical: int64(raw.Ulen)}
		if err := w.emit(it); err != nil {
			return err
		}
		if progress != nil {
			progress(cr.n)
		}
	}
	return nil
}

// fail poisons the Writer with err and returns it: later Emit calls and
// Close report it.
func (w *Writer) fail(err error) error {
	w.cancel(err)
	return err
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
