# Single-pass confirm, staging-time dedup and raw refusal

Follow-up to the speculative decompose design of 2026-09-05. A prism
layer is recompressed twice today: once inside zrecipe's search, which
proves the parameters by recompressing the whole spool with the winning
candidate, and once more in the round-trip check, which reads the fresh
objects back from the store, composes the tar and recompresses it. This
design recompresses a layer once, moves the extract out of the
speculative position and into the same pass, skips chunks the store
already has before they reach the staging pack, and stops storing
layers raw unless the operator asks for it.

Decisions taken in the design conversation (2026-09-06):

- tar-prism's compose is trusted to be the exact inverse of its
  decompose; the confirmation checks only that recompressing the content
  reproduces the original bytes. The full pull-path round trip stays as
  an opt-in diagnostic (`--verify-roundtrip`, now default off).
- zrecipe determines the engine by elimination over a prefix and
  confirms the one survivor in a single pass that also feeds the extract
  (zrecipe spec `2026-09-06-elimination-and-confirm-design.md`,
  released as v0.5.0).
- Raw storage is refused by default for anything that looked like a tar
  or a compressed tar; `not-tar` blobs (configs, attestations, non-tar
  artifacts) are still stored raw because they can never be prisms.

## Goals

- One recompression per prism layer; no read-back of the store on the
  push path unless `--verify-roundtrip` is set.
- The extract overlaps the recompression instead of the inflate, and is
  never run for a layer that turns out not reproducible.
- Chunks the store already holds are never encoded into the staging
  pack nor re-verified at commit.
- A layer that cannot be stored as a prism fails the upload with a clear
  message unless `--allow-raw` is set; configs and other non-tar blobs
  are unaffected.
- Observable outcomes otherwise unchanged: kinds, raw reasons, meta.json,
  per-blob stats, log lines.

## Non-goals

- Overlapping the elimination with pass one (zrecipe follow-up).
- Composing from the staged pack to verify tar-prism before the commit.
- A manifest-time check on layer media types; refusal happens at blob
  upload time, keyed on the raw reason.

## Flow

For a blob that passes the pre-checks (zstd window, compressed and
uncompressed tar-header probes):

```
stage analyze  spool ──► zrecipe.Start ──► zrecipe spool ──► elimination ──► *Analysis
                                                                            (or raw reason)
stage confirm  Analysis.Confirm(ctx, tee)
                 reader: zrecipe spool ──► tee ──► queued pipe ──► blake3 + sha256 + count
                                       │                              ──► tarprism.DecomposeTo
                                       │                                      │ amberSink
                                       │                                      ▼
                                       │                            pack-backed store.Writer
                                       │                          (present chunks skipped)
                                       └──► rebuilder: Recompress code ──► compare with the upload
               ──► *Params, or not-reproducible
stage commit   AddPack (records + skipped keys), comp.json, blob root
stage verify   optional: compose from the store, recompress, sha256 == digest
publish        oci/blob/<digest>
```

`analyze` runs the pre-checks and `zrecipe.Start` under the analyze
deadline and classifies exactly as today (`ErrNotReproducible`,
`ErrUnsupported`, `ErrCorrupt`, the child deadline, the request
context), but stages nothing; a prism decision carries the `*Analysis`.
The raw decision also carries zrecipe's error, for the log line and the
refusal message.

`finalizePrism` runs the confirming pass: it creates the pack writer and
the queued pipe, starts the stager goroutine (today's `stage`: hashes,
`DecomposeTo` over the amber sink, pack close), calls `Confirm` with the
pipe as tee, closes the pipe's write end with `Confirm`'s error, collects
the stager and closes the analysis. The stager reads the pipe to its end
as today, so `Confirm` is never blocked by it; a request context that
ends closes the pipe and both sides report the context error.

| Confirm | Stager | Result |
|---|---|---|
| context error | any | upload fails; pack dropped |
| `ErrNotReproducible` | any | raw `not-reproducible` (or refused); pack dropped; the stager's own error, if any, logged at debug |
| the pipe's error (only after the context ended) or any other error | any | upload fails; pack dropped |
| params | `sinkError` | upload fails; pack dropped |
| params | `decomposeError`, or BLAKE3 or length differ from `params.Uncompressed` | raw `decompose-failed` (or refused), logged at error; pack dropped |
| params | ok | commit |

The commit is today's: `AddPack` (which now also accounts the skipped
chunks), comp.json, stats. Then, only when `VerifyRoundTrip` is set, the
round-trip check as it is today (`roundtrip-failed` downgrades or, by
default, refuses). Then the blob root and publication.

## Store package: staging-time dedup

The pack writer's encoder goroutines ask the store before encoding a
record: `Objects.ObserveKeys` for the key (the write barrier's grey
capture, exactly what `WriteParallel` does for every offered key), then
`Objects.Has`. A present key is not encoded and not written; the writer
records it with its logical size in the pack's skipped list and counts it
in `LogicalBytes`. An absent key is encoded and appended as today.

`AddPack` offers the pack's records as today and then accounts the
skipped keys: `ObserveKeys` again, `Has` again, and a key that is no
longer present fails the commit with an error naming it (the upload
fails with `500`; nothing is published). Present keys are counted as
deduplicated objects and logical bytes in the writer's `Stats`, so a
blob's stats are the same whether a chunk was skipped at staging or
deduplicated at commit, `DiskBytes` included (skipped keys were present,
so they add nothing).

Why this is as safe as the live writer's dedup: `WriteParallel` decides
"present, skip" by the same observe-then-`Has` and the collector's mark
runs concurrently with ingests only behind the write barrier; the sweep
never overlaps an ingest or a publication. The window between the
staging decision and the commit is longer than the live writer's window
between its decision and the publication, which is why `AddPack`
re-checks and fails loudly rather than trusting the earlier answer.
`PrepareRef` walks the root for completeness at publication as a last
line of defence, as before.

## Raw refusal

`blob.Options.AllowRaw` (default false). When false, `Put` refuses to
store a blob raw for every reason except `not-tar`: `not-reproducible`,
`unsupported`, `corrupt`, `analyze-timeout`, `decompose-failed` and
`roundtrip-failed` all arise only for a blob that looked like a tar or a
compressed tar, which in practice is a layer. The refusal is
`*blob.RawRefusedError{Digest, Format, Reason, Err}` whose message names
the reason, zrecipe's detail and the flag (`raw layers are refused; start
with --allow-raw to store them`). Nothing is published; objects a
downgrade path had already committed are left to GC as they are today;
the log line is `layer refused` at error level with the same attributes
the fallback lines carry.

`registry.handleError` maps it to `BLOB_UPLOAD_INVALID` (400) with the
message; `finalize` discards the session and its spool on a refusal,
like a digest mismatch, because a retry cannot succeed. `import` fails
with the error (the importer already aborts on a `Put` failure) and its
message tells the operator about `--allow-raw`.

CLI: `--allow-raw` (bool, default false, `OCI_AMBER_ALLOW_RAW`) in the
shared store flag table, so `serve` and `import` both take it.
`--verify-roundtrip` keeps its meaning and changes its default to
false; its usage text says it is a diagnostic.

## Stages and progress

`Stage` values: `analyze` (pass one and the elimination; progress is the
spool's sequential read position, which reaches the size when pass one
is done and holds during the elimination), new `confirm` (the confirming
pass; progress is the content handed to the extract so far, scaled from
the uncompressed size to the compressed size, reported as the size when
the pass completes), `commit` (unchanged), `verify` (the optional round
trip, unchanged, after commit), `raw` (unchanged).

The importer's tracker gives a prism analyze 0.4, confirm 0.35 and
commit 0.25 of its bar; with `Verify` set, 0.35, 0.3, 0.2 and 0.15. Raw
takes the remainder from wherever the previous stage ended, as today.
The TUI prints stage names as it does, so `confirm` appears between
`analyze` and `commit`; its `searching…` detail for a finished analyze
read is unchanged.

## Error handling summary (delta to the design spec)

| Situation | Outcome |
|---|---|
| zrecipe not reproducible, unsupported, corrupt, analyze deadline, decompose failure, round-trip failure | `AllowRaw`: stored raw with the reason, `201`; otherwise `400 BLOB_UPLOAD_INVALID`, session discarded, nothing published, error-level log |
| non-tar blob (probes) | stored raw `not-tar`, `201`, always |
| tee or pipe failure inside the confirming pass | only possible once the request context ended: `500`, session retained |
| skipped chunk missing at commit | `500`, session retained, error names the key |

## Testing

`store`:

- A pack writer over a store that already holds some of the objects
  writes only the absent ones into the pack (pack smaller than the
  all-absent pack), and `AddPack` plus comp.json yields the same `Stats`
  as a live writer given the same objects, `ObjectsDeduped`,
  `DedupedBytes` and `DiskBytes` included.
- A skipped key deleted from the store between staging and commit (test
  hook or a store reopened without it) fails `AddPack` with an error
  naming the key.
- Existing pack writer tests unchanged.

`blob`:

- Every existing prism test passes on the new pipeline with
  `AllowRaw: true` where it exercises a fallback.
- A non-reproducible gzip tar ends `not-reproducible` raw with
  `AllowRaw` and as `*RawRefusedError` without; nothing is staged in
  either case (no pack write, no store objects, spool dir empty).
- A reproducible gzip of a truncated tar ends `decompose-failed` raw
  with `AllowRaw` and refused without.
- A config blob is stored raw `not-tar` with and without `AllowRaw`.
- The observer sees analyze, confirm, commit for a prism; analyze,
  confirm, commit, verify with `VerifyRoundTrip`; analyze, confirm, raw
  for a decompose failure; analyze, raw for a non-reproducible layer;
  confirm's progress never decreases and ends at the size.
- The round-trip check is not run by default (a failing `roundTripCheck`
  hook is not called); it runs and downgrades (or refuses) when set.
- A second layer sharing most files with a stored one stages a pack
  that is a small fraction of the first layer's and reports the same
  dedup stats as before.
- Cancellation during confirm fails the upload and leaves no file under
  the spool dir.

`registry`: a push of a non-reproducible layer answers `400` with
`BLOB_UPLOAD_INVALID` and the message names `--allow-raw`; the same
push with `AllowRaw` stores it raw as today (e2e).

`importer`, `tui`: tracker fractions for the new stage set; the report
still counts raw reasons; an archive with a non-reproducible layer fails
the import with the refusal message unless `AllowRaw`.

`cmd`: flag defaults and env for `--allow-raw`, the new default of
`--verify-roundtrip`; the crane smoke test unchanged (its layer is a
prism).

Measurement, recorded in `docs/followups.md`: the lhh:82 layers before
and after (`oci-amber import --progress plain`, fresh store), and the
per-stage timings from the log.

## Rollout

1. zrecipe v0.5.0 (elimination and confirm), pinned in `go.mod`.
2. This repository, one PR: `store` staging-time dedup; `blob` pipeline,
   stages, refusal; `registry` mapping; `cmd` flags; `importer`/`tui`
   weights; README, the original design spec's "Blob finalization",
   configuration and error tables, `docs/followups.md`.
