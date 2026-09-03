package blob

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
)

var (
	// ErrNotFound reports a digest with no blob reference.
	ErrNotFound = errors.New("blob: not found")
	// ErrDigestMismatch reports served bytes whose sha256 differs from the
	// blob's digest. They have already been written; the caller must abort
	// the response rather than finish it.
	ErrDigestMismatch = errors.New("blob: served bytes do not match digest")

	// errPrismUnavailable is returned by the prism arms of Put and
	// Blob.WriteTo until Task 8 (blob/prism.go) fills them in. Nothing in
	// this task stores a prism, so only a hand-built root reaches it.
	errPrismUnavailable = errors.New("blob: prism path not implemented yet")
)

// blobsDirName is the prism blobs directory inside a blob root; it equals
// tar-prism's BlobsDir.
const blobsDirName = "blobs"

// Options configures a Store. Zero values take the defaults of the
// corresponding `oci-amber serve` flags; VerifyRoundTrip must be set
// explicitly (the CLI defaults it to true). WorkDir is required.
type Options struct {
	WorkDir               string
	MaxInMemory           int64
	AnalyzeParallelism    int
	AnalyzeTimeout        time.Duration
	MaxConcurrentFinalize int
	VerifyRoundTrip       bool
	RecentTTL             time.Duration
}

// Store puts and serves OCI blobs in an amber store: prisms for
// reproducible compressed tars, verbatim bytes for everything else.
type Store struct {
	st       *store.Store
	opts     Options
	log      *slog.Logger
	finalize chan struct{} // finalize slots
	digests  keyedMutex    // one finalization per digest at a time
	recentMu sync.Mutex
	recent   map[oci.Digest]recentEntry
}

// recentEntry is one row of the recent-uploads table.
type recentEntry struct {
	stats store.Stats
	at    time.Time
}

// New returns a Store over st. It empties and recreates <WorkDir>/spool,
// the directory comp-prysm spills to.
func New(st *store.Store, opts Options, log *slog.Logger) (*Store, error) {
	if st == nil {
		return nil, errors.New("blob: nil store")
	}
	if opts.WorkDir == "" {
		return nil, errors.New("blob: WorkDir is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxInMemory <= 0 {
		opts.MaxInMemory = 64 << 20
	}
	if opts.AnalyzeParallelism <= 0 {
		opts.AnalyzeParallelism = 2
	}
	if opts.AnalyzeTimeout <= 0 {
		opts.AnalyzeTimeout = 15 * time.Minute
	}
	if opts.MaxConcurrentFinalize <= 0 {
		opts.MaxConcurrentFinalize = max(1, runtime.NumCPU()/2)
	}
	if opts.RecentTTL <= 0 {
		opts.RecentTTL = time.Hour
	}
	spool := filepath.Join(opts.WorkDir, spoolDirName)
	if err := os.RemoveAll(spool); err != nil {
		return nil, fmt.Errorf("blob: emptying %s: %w", spool, err)
	}
	if err := os.MkdirAll(spool, 0o755); err != nil {
		return nil, fmt.Errorf("blob: creating %s: %w", spool, err)
	}
	return &Store{
		st:       st,
		opts:     opts,
		log:      log,
		finalize: make(chan struct{}, opts.MaxConcurrentFinalize),
		recent:   make(map[oci.Digest]recentEntry),
	}, nil
}

// Exists reports whether oci/blob/<d> is published.
func (b *Store) Exists(d oci.Digest) (bool, error) {
	_, err := b.st.Resolve(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Open resolves oci/blob/<d> and reads its meta.json (a few hundred
// bytes); nothing else is fetched.
func (b *Store) Open(d oci.Digest) (*Blob, error) {
	root, err := b.st.Resolve(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	meta, err := b.readMeta(root)
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w", d, err)
	}
	if meta.Digest != d {
		return nil, fmt.Errorf("blob: %s: %s names %s", d, MetaFile, meta.Digest)
	}
	return &Blob{Meta: meta, root: root, store: b}, nil
}

// readMeta reads meta.json of a blob root.
func (b *Store) readMeta(root key.Key) (Meta, error) {
	k, err := b.st.LookupKey(root, MetaFile)
	if err != nil {
		return Meta{}, fmt.Errorf("%s: %w", MetaFile, err)
	}
	data, err := b.st.ReadFile(k)
	if err != nil {
		return Meta{}, fmt.Errorf("reading %s: %w", MetaFile, err)
	}
	return decodeMeta(data)
}

// Delete removes oci/blob/<d>; the objects become garbage for amber's
// collector. It waits for a finalization of the same digest to finish.
func (b *Store) Delete(d oci.Digest) error {
	unlock := b.digests.lock(d)
	defer unlock()
	err := b.st.DeleteRef(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// TakeRecent returns and removes the recent-uploads entry for d: the stats
// of a Put in this process within RecentTTL.
func (b *Store) TakeRecent(d oci.Digest) (store.Stats, bool) {
	b.recentMu.Lock()
	defer b.recentMu.Unlock()
	b.purgeRecentLocked(time.Now())
	e, ok := b.recent[d]
	if !ok {
		return store.Stats{}, false
	}
	delete(b.recent, d)
	return e.stats, true
}

// recordRecent remembers d's stats for RecentTTL, dropping expired rows.
func (b *Store) recordRecent(d oci.Digest, stats store.Stats) {
	b.recentMu.Lock()
	defer b.recentMu.Unlock()
	now := time.Now()
	b.purgeRecentLocked(now)
	b.recent[d] = recentEntry{stats: stats, at: now}
}

// purgeRecentLocked drops rows older than RecentTTL. The caller holds
// recentMu.
func (b *Store) purgeRecentLocked(now time.Time) {
	for d, e := range b.recent {
		if now.Sub(e.at) > b.opts.RecentTTL {
			delete(b.recent, d)
		}
	}
}

// buildRoot writes meta.json and the blob root directory through w. files
// maps entry names to content keys; blobsDir, when non-zero, becomes the
// blobs/ directory entry. Entries are added in byte order as the directory
// builder requires.
func (b *Store) buildRoot(w *store.Writer, meta Meta, files map[string]key.Key, blobsDir key.Key) (key.Key, error) {
	data, err := encodeMeta(meta)
	if err != nil {
		return key.Key{}, err
	}
	metaKey, err := w.PutBytes(data)
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: storing %s: %w", MetaFile, err)
	}
	entries := make(map[string]key.Key, len(files)+2)
	for name, k := range files {
		entries[name] = k
	}
	entries[MetaFile] = metaKey
	hasBlobs := blobsDir != key.Key{}
	if hasBlobs {
		entries[blobsDirName] = blobsDir
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	dir := w.NewDir()
	for _, name := range names {
		var err error
		if hasBlobs && name == blobsDirName {
			err = dir.AddDir(name, entries[name])
		} else {
			err = dir.AddFile(name, entries[name])
		}
		if err != nil {
			return key.Key{}, fmt.Errorf("blob: adding %s to blob root: %w", name, err)
		}
	}
	root, err := dir.Finish()
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: finishing blob root: %w", err)
	}
	return root, nil
}

// keyedMutex serializes work per digest. Its zero value is ready to use;
// rows nobody holds or waits on are dropped.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[oci.Digest]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until d is free and returns the matching unlock function.
func (k *keyedMutex) lock(d oci.Digest) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[oci.Digest]*keyedLock)
	}
	l, ok := k.locks[d]
	if !ok {
		l = &keyedLock{}
		k.locks[d] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, d)
		}
		k.mu.Unlock()
	}
}
