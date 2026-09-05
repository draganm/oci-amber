# Speculative decompose design

Blob finalization takes a compressed layer apart in two passes today:
pass one inflates the stream once to find the compression parameters
(zrecipe's search), pass two inflates it again to hash it, split the tar
with tar-prism and chunk the file contents into the store. The two passes
run one after the other, so a large layer pays two inflates and waits for
the whole search before any chunking starts. This design overlaps them:
the single inflate of pass one is teed into tar-prism and the chunker,
the resulting objects are staged in a temporary pack file on disk, and
once the search succeeds the pack's records are inserted into the store
as they are. If the layer ends up raw the pack is dropped and the store
was never touched.

Decisions taken during the design review (2026-09-05):

- The commit step inserts **pre-encoded records** into the packstore
  rather than re-encoding them: amber-store-core gains a write path for
  records that keeps its dedup, verification, GC barrier and accounting.
  The alternative, decoding the pack and feeding the existing object
  path, would have needed no upstream change but re-compresses every
  chunk after the search.
- Staging goes through a **pack-backed `store.Writer`**, so the
  tar-prism sink, the chunker and the index builders are shared with the
  live path. A separate staging sink in `blob` would have duplicated
  them.
- Speculating straight into the store and leaving garbage for GC was
  ruled out: whole classes of layers (pgzip-compressed rock layers) are
  not reproducible and would leave their full uncompressed content
  behind on every push.

## Goals

- Inflate a compressed layer once, not twice.
- Run tar-prism, chunking and record encoding concurrently with the
  engine search, inside the finalize slot the blob already holds.
- Never write an object of a blob that ends up raw into the store.
- Keep every observable outcome of `blob.Store.Put` the same: kinds,
  raw reasons, meta.json fields, per-blob stats, the round-trip check,
  the error classes that fail an upload.

## Non-goals

- Dropping the round-trip check's second recompression (a policy
  question, see `docs/followups.md`, "ingestion performance").
- Skipping chunks that are already in the store at staging time. That
  would need the collector's write barrier during staging; the commit
  step dedups instead.
- Changing the search or its parallelism.

## Flow

For a blob that passes the cheap pre-checks (zstd window, compressed
tar-header probe, and now also the uncompressed tar-header probe):

```
spool ──► zrecipe.Analyze ──► search spool ──► search ──► params
              │ Options.Uncompressed
              ▼
        queued pipe ──► blake3 + sha256 + count ──► tarprism.DecomposeTo
                                                        │ amberSink
                                                        ▼
                                              pack-backed store.Writer
                                                        │ encode records
                                                        ▼
                                              unlinked temp pack file
```

When `Analyze` returns with params and staging succeeded, the pack is
committed: a live `store.Writer` inserts its records (dedup, verify,
accounting), stores comp.json, and its stats become the blob's. The
round-trip check, the blob root and the publication are unchanged.

For every other outcome the pack is dropped. Raw blobs and downgrades
go through `finalizeRaw` exactly as today.

## Store package (`store`)

`Writer` keeps its API and gains a backend.

- `NewWriter(ctx)` is the live backend: one `packstore.WriteParallel`
  call fed by the accounting iterator, as today.
- `NewPackWriter(ctx, dir)` is the pack backend. It creates a temp file
  in dir and unlinks it at once (the descriptor keeps it alive; a crash
  leaves nothing behind). Objects offered through `Emit`, `PutStream`,
  `PutBytes`, `NewDir` and `PutXattrs` are encoded with
  `amberpack.EncodeRecord` on `writers()` goroutines and appended to an
  amberpack wire pack by one file writer; record order in the pack is
  irrelevant. `Close` writes the end marker, flushes, and returns
  `Stats` holding `LogicalBytes` only (dedup is unknown until commit).
  `Pack()` returns the `*Pack` after `Close`; before it, or after
  `Abort`, it returns nil. `Abort` closes and thereby deletes the file.
  A cancelled context behaves like `Abort`.
- `Pack` is the staged file: `Size()` in bytes, `Close()` releases it.
  It is consumed by `AddPack` at most once.
- `(*Writer).AddPack(p *Pack, progress func(read int64)) error` on a live
  writer (progress may be nil and is called with the pack bytes read so
  far) iterates the
  pack's records with `amberpack.Reader.Records` and offers each as a
  pre-encoded item on the same channel as regular objects. The
  accounting iterator yields it as `packstore.Object{Key, Record}` and
  counts the record's uncompressed length (`ulen`) as logical bytes; the
  `seen` map, `Has` and `StoredSize` bookkeeping are unchanged, so
  `Stats` keeps its meaning. A malformed pack fails `AddPack` with a
  wrapped `amberpack.ErrMalformed`; the writer is then unusable and the
  caller aborts it.

Both backends share the object-building layer (`PutStream`, `Dir`,
`PutXattrs`, the item chunker). Only the sink of the object channel
differs.

## amber-store-core

One PR against main of `jobs-build/amber-store-core`:

- `packstore.Object` gains `Record []byte`: a complete record as
  `amberpack.EncodeRecord` produces it. When set, `Data` must be nil.
  `WriteParallel` and `WriteBatch` parse it (`amberpack.ParseRecord`:
  framing, flags, length invariants, CRC, canonical key), require the
  parsed key to equal `Object.Key`, and append it verbatim. With
  `WriteOpts.Verify` they also decode the payload and rehash it against
  the key, as `verifyObject` does for `Data`. `WriteStats.BytesStored`
  counts `ulen`. Because the record goes through the same loop as an
  object, it gets the GC barrier (`observe`), the in-flight write
  token, the within-stream `seen` set and the `Has` dedup for free. An
  object with both `Data` and `Record` set, or a record whose key
  differs from `Object.Key`, fails the run with an error that wraps
  `amberpack.ErrCorrupt`.
- `amberpack.Reader` gains `Records() iter.Seq2[[]byte, error]`: the
  records of the stream, each validated exactly as `All` validates them
  (framing, CRC, canonical key, size bound) but not decoded. Like
  `All`, it may be called once per Reader. It is the read-side
  counterpart of `Writer.AddRecord`.

During development oci-amber's `go.mod` carries a `replace` to the local
checkout at `~/jobs-build/amber-store-core`; the final oci-amber PR pins
the merge commit's pseudo-version, as the current pin does.

## Blob orchestration (`blob`)

`analyze` becomes analyze-and-stage and returns the decision plus the
staging result.

Pre-checks, unchanged in substance: the zstd window bound, the
compressed tar-header probe (raw `not-tar`), and, moved before `Analyze`
from after it, the uncompressed tar-header probe for streams `Detect`
reports as `none`. A blob that is not a tar therefore never starts
staging and keeps reason `not-tar`.

Staging runs on its own goroutine while `Analyze` runs on the caller's:

1. A `pipe` (`blob/pipe.go`, 8 slots) is created. Its write end is
   `Options.Uncompressed`; `Analyze` writes the decompressed stream into
   it during its first pass.
2. The goroutine reads the pipe through a tee into BLAKE3, sha256 and a
   byte counter, and runs `tarprism.DecomposeTo` with the existing
   `amberSink` over a `NewPackWriter`.
3. When `DecomposeTo` returns, succeeded or failed, the goroutine keeps
   reading the pipe to EOF and discards the rest. Staging can therefore
   never block `Analyze` or change its verdict. The only thing that
   closes the read end early is the request context ending, in which
   case `Analyze` reports the context error anyway.
4. The goroutine closes the sink's recipe, finishes the sink, closes the
   pack writer, and records: the recipe, index and blobs keys, the entry
   count, the sha256 (diffID), the BLAKE3 and length of the stream, the
   pack, and its error, classified as today's `ingestPrism` classifies:
   `readError` for the pipe (only possible on cancellation), `sinkError`
   for pack-writer failures, `decomposeError` for tar-prism rejecting the
   stream.

The caller runs `Analyze` under the analyze deadline with the pipe as
`Uncompressed`, then closes the pipe's write end with `Analyze`'s error
(nil on success, so the stager sees EOF) and waits for the stager.

Outcomes:

| Analyze | Staging | Result |
|---|---|---|
| context error | any | upload fails; pack dropped |
| raw reason (`not-reproducible`, `unsupported`, `corrupt`, `analyze-timeout`) | any | raw with that reason; pack dropped; a staging error is logged at debug |
| any other error | any | upload fails; pack dropped |
| params | `sinkError` (pack write, temp file) | upload fails, as amber sink errors do today; pack dropped |
| params | `decomposeError`, or BLAKE3 or length differ from `params.Uncompressed` | raw `decompose-failed`, logged at error; pack dropped |
| params | ok | commit |

Commit, in `finalizePrism`: a live writer takes `AddPack`, then comp.json
through `PutBytes`; `Close` yields the blob's stats. Then, unchanged,
the round-trip check when `VerifyRoundTrip` is set (a failure downgrades
to raw `roundtrip-failed`), `writeRoot`, publication. A failure of
`AddPack` or `Close` aborts the writer and fails the upload, leaving
appended objects to GC, exactly as a pass-two store error does today.
The pack is closed once the writer is closed or aborted, on every path.

`ingestPrism` and the second inflate are removed. `newDecompressor`
stays for the compressed tar-header probe. `spoolReader`, `byteCounter`
and the error types are reused by the stager.

`Put`'s structure does not change: dedup, slot, analyze-and-stage,
prism or raw, publish. The `decision` carries a `*staged` for the prism
case; `finalizePrism` takes it instead of re-opening the spool.

## Progress

`StageDecompose` is renamed `StageCommit` and reported while the pack is
inserted. Its progress is the number of pack bytes read so far; the
tracker clamps it at the blob's compressed size, so the bar is
approximate but never runs backwards. Analyze's progress is unchanged:
the spool's sequential read position, which reaches the size when the
inflate is done and holds during the search ("searching…"). The
tracker's stage weights are unchanged, commit taking decompose's share;
the TUI prints the stage name, so "commit" appears where "decompose"
did.

## Budgets

- Disk, per finalize, under `<work-dir>/spool`: zrecipe's search spool
  (the uncompressed size, once it exceeds `--max-in-memory`) plus the
  pack (about the compressed size). Both are unlinked temp files, so a
  crash leaves nothing and the startup wipe of that directory stays a
  safety net. `--max-concurrent-finalize` bounds the total.
- Memory, per finalize, on top of today: the pipe (8 writes of at most
  32 KiB), the emit queue (8 objects of at most 1 MiB) and one chunk
  per encoder goroutine (`GOMAXPROCS/2` of at most 1 MiB), about 16 to
  24 MiB.
- CPU: chunking, hashing and record encoding now run while `Analyze`
  inflates and searches, inside the same finalize slot. The slot count
  does not change.

## Testing

amber-store-core:

- `WriteParallel` and `WriteBatch` with `Record` objects: stored and
  readable; deduped against present objects and against duplicates in
  the stream; `Verify` rejects a record whose payload hashes to another
  key; a corrupted CRC fails with `ErrCorrupt`; `Data` and `Record`
  both set fails; a key mismatch fails; `BytesStored` counts `ulen`.
- `Reader.Records`: round trip through `Writer.Add` and `AddRecord`;
  truncated stream and bad CRC yield `ErrMalformed`; records re-appended
  through `Object.Record` read back identical.

store:

- A pack writer and a live writer given the same objects produce the
  same keys; `AddPack` then `Get` returns the content.
- `Stats` after `AddPack` plus comp.json equal the live writer's for the
  same objects, including `DiskBytes` and dedup counts against objects
  already present.
- `Abort` and a cancelled context leave no file under the directory;
  `Pack()` is nil.
- A pack whose bytes were tampered with fails `AddPack` with
  `ErrMalformed`; `Close` afterwards reports the failure.
- `AddPack` on a pack writer, or twice on the same pack, is an error.

blob:

- Every existing prism, fallback and put test runs unchanged on the new
  path.
- A non-reproducible gzip tar ends raw `not-reproducible` with
  `ObjectsNew` counting only the raw file's objects and nothing left
  under the spool dir.
- A reproducible gzip of a truncated tar ends raw `decompose-failed`.
- A context cancelled during analyze fails the upload and leaves no file
  under the spool dir.
- A pack write failure (the spool dir made unwritable before `Put`)
  fails the upload; nothing is published.
- The observer sees analyze, commit, verify for a prism and analyze,
  raw for a raw blob.
- The uncompressed non-tar blob stays raw `not-tar` and never reaches
  staging (no stage after analyze, no store objects).

cmd, importer, tui: the crane smoke test is unchanged; tracker and view
tests follow the stage rename.

Measurement, before and after, recorded in `docs/followups.md`: the
61 MiB go-flate layer (11.2 s baseline: pass one 0.8 s, search 3.6 s,
pass two 1.7 s, round-trip 5.0 s) and one large multi-layer image pushed
with crane from a `docker save` layout.

## Rollout

1. amber-store-core: the `Record` write path and `Reader.Records`, with
   tests, merged to main.
2. oci-amber: `store` pack writer and `AddPack`, the blob orchestration,
   the stage rename, tests, docs; `go.mod` pinned to the amber-store-core
   merge commit; README and the original design spec's "Blob
   finalization" steps updated to describe one pass and a commit.

## Follow-ups

- Skip present chunks at staging time under the collector's write
  barrier, saving the pack write for mostly-deduplicated layers.
- Recompress once: let the round-trip check compose and hash only,
  since the search already proved the parameters (policy question
  recorded in `docs/followups.md`).
- The pack format makes a staged blob transferable; a remote-store push
  could reuse it.
