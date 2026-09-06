# Single-Pass Confirm, Staging-Time Dedup and Raw Refusal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ingest a prism layer with one recompression (zrecipe's confirming pass feeding the extract), skip present chunks before they reach the staging pack, and refuse raw layers unless `--allow-raw` is set.

**Architecture:** `blob.analyze` runs `zrecipe.Start` under the analyze deadline and returns an `*Analysis`; `finalizePrism` runs `Confirm` with the queued pipe to the stager as tee, then commits the pack and, only when `VerifyRoundTrip` is set, runs today's round trip. The store's pack writer asks `Objects.Has` before encoding and carries skipped keys for the commit's accounting. `Put` turns every raw outcome but `not-tar` into `*RawRefusedError` unless `Options.AllowRaw`; the registry maps it to `400 BLOB_UPLOAD_INVALID`.

**Tech Stack:** Go 1.26 with cgo (zlib, libzstd via `nix develop`), zrecipe v0.5.0, tar-prism `sink-source-api`, amber-store-core v0.0.3.

**Spec:** `docs/superpowers/specs/2026-09-06-single-pass-confirm-design.md`

## Global Constraints

- No `internal/` packages; flat top-level packages.
- Tests: `nix develop --command go test ./...` (cgo engines).
- Commit messages end with the Co-Authored-By and Claude-Session trailers used on this branch.
- Remove any binary a step builds.

---

### Task 1: Store pack writer skips present chunks

**Files:**
- Modify: `store/pack.go` (`Pack.skipped`, `stage`, `AddPack`), `store/write.go` (`item.skipped`, `objects`, `account`, `finish`)
- Test: `store/pack_test.go`

**Interfaces:**
- Produces: `Pack.Skipped() int` (number of records left out because the store had them); `item{skipped bool}` items flow through the live writer's accounting iterator without reaching `WriteParallel`; `Stats.ObjectsDeduped` includes them.

Behaviour: in `stage`, before `amberpack.EncodeRecord`, `w.s.Objects.ObserveKeys([]key.Key{k})` then `w.s.Objects.Has(k)`; present → append `{k, logical}` to `w.pack.skipped` under `mu`, `w.logical += logical`, continue. `AddPack`, after the records, emits one `item{obj: packstore.Object{Key: k}, logical: ulen, skipped: true}` per skipped key. `objects()` handles a skipped item on the iterator goroutine: `ObserveKeys`, `Has`; absent → yield `fmt.Errorf("store: staged chunk %s is no longer in the store", k)`; present → `w.logical += ulen`, `w.seen[k] = false` unless already seen, `w.skippedDeduped++`, and it is not yielded to `WriteParallel`. `finish` adds `w.skippedDeduped` to `ObjectsDeduped`.

- [ ] Tests: `TestPackWriterSkipsPresentChunks` (write objects A,B live; pack writer offered A,B,C: `Skipped()==2`, pack size below the all-absent pack's; `AddPack` then `Close` stats equal a live writer's for A,B,C on the same store state, all six fields); `TestAddPackFailsWhenSkippedChunkVanished` (skip via a store that has A, then delete A's pack from disk? Simplest: stage against store S1 where A is present, then feed the pack to a live writer of a fresh store S2 — `AddPack`/`Close` fails with an error naming A's key). Existing tests unchanged.
- [ ] Implement; `nix develop --command go test -race ./store/`; commit `store: skip present chunks at staging time`.

### Task 2: Raw refusal (`blob`), HTTP mapping, flags

**Files:**
- Modify: `blob/store.go` (`Options.AllowRaw`, refusal in `Put`), `blob/meta.go` or new `blob/refuse.go` (`RawRefusedError`), `blob/analyze.go` (`decision.err`)
- Modify: `registry/errors.go`, `registry/uploads.go`
- Modify: `cmd/oci-amber/main.go`, `cmd/oci-amber/import.go`, README configuration table
- Test: `blob/put_test.go`, `blob/prism_fallback_test.go`, `registry/e2e_test.go`, `cmd/oci-amber/app_test.go`, `cmd/oci-amber/import_test.go`, `importer/importer_test.go`

**Interfaces:**
- Produces: `type RawRefusedError struct{ Digest oci.Digest; Format string; Reason RawReason; Err error }` with `Error()` = `blob: <digest> cannot be stored as a prism (<reason>[: <err>]); raw layers are refused, start with --allow-raw to store them` and `Unwrap()`; `Options.AllowRaw bool`; `decision.err error`.

- [ ] Existing fallback tests set `AllowRaw: true`; new tests: refusal for not-reproducible, decompose-failed, roundtrip-failed (via the `roundTripCheck` hook with `VerifyRoundTrip`), analyze-timeout; config blob stays raw `not-tar` without `AllowRaw`; a refusal publishes nothing and logs `layer refused` at error level.
- [ ] `handleError`: `*blob.RawRefusedError` → `oci.NewError(oci.CodeBlobUploadInvalid, "%v", err)`; `finalize` discards the session and spool on it. e2e: `400` with the message naming `--allow-raw`; with `AllowRaw` stored raw.
- [ ] Flags: `--allow-raw` (default false, env `OCI_AMBER_ALLOW_RAW`) in `storeFlags`; `--verify-roundtrip` default false, usage "diagnostic: also run the full pull pipeline over every prism before publishing it"; plumbed through `config`, `importConfig`, `blob.Options`. Flag tests updated.
- [ ] Commit `blob, registry, cmd: refuse raw layers unless --allow-raw`.

### Task 3: zrecipe v0.5.0 and the single-pass pipeline (`blob`)

**Files:**
- Modify: `go.mod`/`go.sum` (zrecipe v0.5.0)
- Modify: `blob/analyze.go` (`analyze` returns `decision{analysis *zrecipe.Analysis}`; `stage` unchanged in body, started from `finalizePrism`), `blob/prism.go` (`finalizePrism`: confirm, commit, optional verify), `blob/observer.go` (`StageConfirm`), `blob/store.go` (`Put` wiring)
- Test: `blob/*_test.go`

**Interfaces:**
- Consumes: `zrecipe.Start(ctx, r, *Options) (*Analysis, error)`, `(*Analysis).Confirm(ctx, tee io.Writer) (*Params, error)`, `(*Analysis).Uncompressed() Digest`, `(*Analysis).Close()`.
- Produces: `StageConfirm Stage = "confirm"`; stage order analyze, confirm, commit[, verify] for a prism.

`finalizePrism(ctx, dec, d, size)`: `defer dec.analysis.Close()`; `observeStage(StageConfirm)`; `pw, err := b.st.NewPackWriter(ctx, b.spoolDir())`; `p := newPipe(stagePipeSlots)`; `go stage(ctx, p, pw)`; tee = `p` wrapped in a progress writer that reports `min(size, teeBytes*size/uncompressedSize)`; `params, cerr := a.Confirm(ctx, tee)`; `p.CloseWrite(cerr)`; `s := <-done`; then the outcome table of the spec (`ErrNotReproducible` → `&rawFallback{ReasonNotReproducible, cerr}`; `s.check(params)` as today; commit; verify when set; `observeProgress(d, size)` at the end of confirm).

- [ ] Tests: the spec's `blob` list (stages with and without `VerifyRoundTrip`; round trip not run by default; non-reproducible stages nothing: no pack file created, no store objects; decompose-failed still detected; cancellation during confirm; existing prism tests unchanged).
- [ ] Commit `blob: confirm and extract in one pass; round trip opt-in`.

### Task 4: Tracker and TUI

**Files:**
- Modify: `importer/tracker.go` (weights: analyze 0.4, confirm 0.35, commit 0.25; with `Verify`: 0.35, 0.3, 0.2, 0.15), `tui/view.go` if a label changes
- Test: `importer/tracker_test.go`

- [ ] Update `TestTrackerFractionsThroughPrismStages`, `TestTrackerWithoutVerifyCommitTakesHalf` (rename to the new shares), `TestTrackerRawTakesTheRemainder`; commit `importer: tracker weights for the confirm stage`.

### Task 5: Docs and measurement

**Files:**
- Modify: `README.md` (configuration table, logging examples, limitations on raw layers, storage layout mention of skipped chunks), `docs/superpowers/specs/2026-09-03-oci-amber-design.md` ("Blob finalization" steps 4–8, configuration and error tables), `docs/followups.md` (ingestion performance: done items, new measurements, follow-ups: elimination overlapping pass one, pigz/pgzip verdicts derived from zlib/flate candidates, raising `--analyze-parallelism`)

- [ ] Measure lhh:82 layers before/after with `oci-amber import --progress plain --log-file`, record `duration=` lines.
- [ ] Commit `docs: single-pass confirm`; PR against main.
