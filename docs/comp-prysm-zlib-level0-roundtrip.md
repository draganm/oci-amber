# Why crane-built best-speed layers are stored raw: a comp-prysm zlib level-0 feeding bug

Investigation record (2026-09-04) of a push-time round-trip failure seen in the crane smoke test. The defect is in comp-prysm, not oci-amber; the registry stores such layers raw with `rawReason=roundtrip-failed` until comp-prysm is fixed as proposed below.


Investigated in the isolated worktree at
`/private/tmp/claude-502/-Users-dragan-draganm-oci-amber/50cac436-fe4e-41d8-8283-6830f4937a03/scratchpad/rt-investigate`
(detached HEAD `f33da6c`). Nothing was changed in the main repository; all
throwaway test files were deleted afterwards.

## Verdict

**The defect is in comp-prysm, not in oci-amber, tar-prism or the amber
store.** Category (b) of the hypothesis list: `Recompress` produces different
bytes than `Analyze` verified — because the zlib engine's byte output at
**deflate level 0** depends on the size of the `Write` calls it receives, and
`Analyze`'s candidate search feeds it in one huge `Write` while `Recompress`
feeds it in `io.Copy`'s 32 KiB chunks.

oci-amber behaved correctly throughout: the round-trip check caught the
mismatch and stored the blob raw, so no bytes were lost.

## Reproduction (no crane, no server)

A test in package `blob` that builds exactly the layer `crane append` builds
in `TestCraneSmoke` — the tar of `etc/extra` (6 bytes) + `usr/share/data`
(100 KiB of `math/rand` seed 11), gzipped with `compress/gzip` at
`gzip.BestSpeed` (what ggcr's `internal/gzip.ReadCloser` uses, confirmed in
`go-containerregistry@v0.21.7/internal/gzip/zip.go` and
`pkg/v1/tarball/layer.go:197`) — and calls `blob.Store.Put` reproduces the
failure with **the same digest**:

```
meta: kind=raw format=gzip reason="roundtrip-failed" digest=sha256:f14a9b19eeff4109ef4c17a44d959803dc47e1b4b98f756921c3e825c6b1fcea
level=ERROR msg="round-trip verification failed, storing raw"
  digest=sha256:f14a9b19… format=gzip engine=zlib engine_version=1.3.2
  error="blob: recompress: compprysm: recompressed output does not match params:
         output is 29498297…/104998, params expect f6c149c9…/104993"
```

The error is `ErrDigestMismatch` (**output**), not `ErrInputMismatch`.
`compprysm.Recompress` (`recompress.go:63-71`) checks the input digest before
the output digest on the success path, so the tar that `ComposeFrom` fed it
already matched `params.Uncompressed` exactly.

## Evidence that tar-prism and the amber store are innocent

A test driving `ingestPrism` and then `tarprism.ComposeFrom` over the stored
objects:

```
ComposeFrom reproduced the tar exactly (104960 bytes, 2 entries)
Recompress over the composed tar: out=104998 want=104993
  err=compprysm: recompressed output does not match params …
```

Composition is byte-exact; only the recompression differs. The same failure
also reproduces with **no tar-prism and no amber store at all**, straight from
`compprysm.Analyze` + `compprysm.Recompress` over a `bytes.Reader`.

## The parameters comp-prysm chose

```json
{ "format": "gzip",
  "compressed":   { "blake3": "f6c149c9…", "size": 104993 },
  "uncompressed": { "blake3": "77d1ad5e…", "size": 104960 },
  "engine": "zlib", "engine_version": "1.3.2",
  "gzip": { "header_b64": "H4sIAAAAAAAE/w==",
            "level": 0, "strategy": "default", "window_bits": 15, "mem_level": 8 } }
```

Level 0 — pure stored blocks. The payload (100 KiB of random bytes in a tar)
is incompressible, so Go's `compress/gzip` at `BestSpeed` also fell back to
stored blocks, and zlib at level 0 happens to emit the identical byte stream
*when fed the whole input at once*. zlib's tier-1 candidates are evaluated
before go-flate's (`analyze.go: gzipCandidates` is tier-major, engine order
`gnugzip, zlib, pigz, libzstd, go-flate, …`), so this coincidental match wins
over the true producer, `go-flate` level 1.

## The mechanism, byte for byte

First difference is at offset 11 of the blob, i.e. the second byte of the
deflate payload — the `LEN` field of the first stored block:

```
want (original) …00 04 ff | 00 ff ff 00 00 | 65 74 63 2f 65 78 74 72 61  LEN=0xffff=65535
got  (recompress)…00 04 ff | 00 00 80 ff 7f | 65 74 63 2f 65 78 74 72 61  LEN=0x8000=32768
```

Decoding every stored-block header of each stream:

| feeding shape | stored block lengths | deflate payload |
|---|---|---|
| reference (Go `compress/gzip` BestSpeed) | `[65535, 39425, 0]` | 104975 |
| zlib L0, one big `Write` (**what `Analyze` does**) | `[65535, 39425, 0]` | 104975 — byte-identical to the reference |
| zlib L0, `io.Copy` 32 KiB `Write`s (**what `Recompress` does**) | `[32768, 32768, 32768, 6656]` | 104980 |

104980 + 10 (gzip header) + 8 (trailer) = **104998**, exactly the size the
round-trip check reported.

Why: zlib's `deflate_stored()` sizes each stored block as
`min(MAX_STORED=65535, left + avail_in, avail_out - header)` and refuses to
emit anything shorter than `min_block = MIN(pending_buf_size-5, w_size)` =
32768 at `Z_NO_FLUSH`. comp-prysm's zlib writer
(`engine/zlib/zlib.go:161-190`) copies each caller `Write` into its C input
buffer and drains it with `deflate(Z_NO_FLUSH)` before returning, so
`avail_in` is exactly the caller's chunk size. 32 KiB chunks ⇒ 32768-byte
stored blocks; one 104960-byte chunk ⇒ maximal 65535-byte blocks.

The engine already has a comment and a unit test about this hazard
(`bufSize = 128 << 10` at `engine/zlib/zlib.go:35`,
`TestLevelZeroStoredBlocksAreCanonical` at `engine/zlib/zlib_test.go:102`) —
but they only address the **output** buffer (`avail_out`). The **input**
batching (`avail_in`) is still whatever the caller writes, and the test passes
only because it does a single `w.Write(data)`.

## Why the two comp-prysm paths feed differently

* `search.evaluate` — `comp-prysm/search/run.go:154`:
  `io.Copy(w, in.Spool.Reader())`.
  For an in-memory spool `Spool.Reader()` returns `bytes.NewReader(s.buf)`
  (`search/spool.go:72`). `bytes.Reader` implements `io.WriterTo`, and
  `io.Copy` takes that fast path, which issues **one** `w.Write` with the
  entire buffer (`$GOROOT/src/bytes/reader.go:137`).
* `compprysm.recompressGzip` — `comp-prysm/recompress.go:111`:
  `io.Copy(dw, io.TeeReader(in, crc))`.
  `io.TeeReader` implements neither `WriterTo` nor `ReaderFrom`, and the zlib
  writer is not a `ReaderFrom`, so `io.Copy` always uses its internal
  **32 KiB** buffer.

Confirmation of the asymmetry: forcing the search spool to spill to disk
(`Options{MaxInMemory: 1024}`) makes `Spool.Reader()` return an
`io.SectionReader`, which has **no** `WriteTo`, so the search also copies in
32 KiB chunks — and then zlib level 0 no longer matches and the search
correctly picks the true producer:

```
MaxInMemory=0    (in-memory spool) -> engine=zlib     level=0   Recompress FAILS
MaxInMemory=1024 (spilled spool)   -> engine=go-flate level=1   Recompress OK at every chunk size
```

## Scope / minimal trigger

Only zlib **level 0** is chunk-sensitive; levels 1–9 buffer into the deflate
window and are byte-stable across `Write` sizes:

```
zlib level 0: whole=104975 32k=104980 4k=104980  stable=false
zlib level 1..9: whole == 32k == 4k              stable=true
```

The other deflate engines are chunk-invariant by construction (`gnugzip` is a
Go port of GNU gzip filling its own window; `pigz` batches to fixed 128 KiB
blocks; `go-flate`/`kp-flate` buffer into the flate window).

So the trigger is: **a gzip member whose deflate payload is a run of maximal
65535-byte stored blocks (incompressible content), analyzed with a spool that
stays in memory (< `MaxInMemory`, default 64 MiB).** Characterisation over
several payloads:

```
random-30k   gzlevel= 1 -> go-flate/1  ok
random-40k   gzlevel= 1 -> zlib/0      FAIL
random-100k  gzlevel= 1 -> zlib/0      FAIL   <- the crane-appended layer
random-300k  gzlevel= 1 -> go-flate/1  ok
text-100k    gzlevel= 1 -> go-flate/1  ok
v1 layer (256 KiB random + text), gzip default level -> go-flate/6  ok
```

That last row is why the e2e layers and the crane **push** layer round-trip
fine: they are built with `compress/gzip` at the default level, whose blocks
are not plain stored blocks. `crane append` uses `gzip.BestSpeed`, which is
what turns the incompressible payload into stored blocks.

## Proposed fix (comp-prysm — do not apply to oci-amber)

Primary, in `comp-prysm/engine/zlib/zlib.go`: make the writer **batch its
input** so every `deflate()` call sees a full buffer, instead of one call per
caller `Write`. Accumulate incoming bytes in the existing 128 KiB `z.in`
buffer and only call `deflate(Z_NO_FLUSH)` when it is full, flushing the
remainder in `Close()` before `Z_FINISH`. Validated: with input batched to
64 KiB or 128 KiB, the 32 KiB-chunked feed reproduces the reference exactly.

```
batch=0       out=104980 lens=[32768 32768 32768 6656] matches-reference=false
batch=65536   out=104975 lens=[65535 39425 0]          matches-reference=true
batch=131072  out=104975 lens=[65535 39425 0]          matches-reference=true
```

Belt and braces, and worth doing regardless: make the search and the
recompressor **share one feeding function** so a candidate can never be
verified under a feeding shape different from the one that will rebuild it.
Add e.g. `compprysm.copyToEngine(dst io.Writer, src io.Reader)` that wraps
`src` so no `WriterTo` fast path can fire
(`io.CopyBuffer(dst, struct{ io.Reader }{src}, make([]byte, N))`) and call it
from both `search.evaluate` (`search/run.go:154`) and `recompressGzip` /
`recompressZstd` (`recompress.go:111`, `:130`). Without the engine fix this
alone would also cure this blob (the search would fall through to
`go-flate/1`), but at the cost of no longer reproducing genuine
zlib-level-0-with-large-writes files; with the engine fix, both engines stay
usable and the two paths are provably identical.

Optionally, a cheap safety net: after the search wins, re-verify the winner
once through the real `Recompress` code path before returning `Params`, so a
non-reproducible parameter set surfaces as `ErrNotReproducible` at analyze
time rather than as a round-trip failure in the consumer.

### Regression tests to pin it

1. `comp-prysm/engine/zlib`: extend `TestLevelZeroStoredBlocksAreCanonical`
   with a variant that feeds the same data through `io.Copy` (32 KiB writes)
   and asserts the **first stored block LEN is 65535** and that the two feeding
   shapes produce identical bytes. This test fails today.
2. `comp-prysm` top level: `Analyze` a gzip member produced by
   `compress/gzip` at `BestSpeed` over ~100 KiB of incompressible data, then
   `Recompress` from (a) a `bytes.Reader` and (b) a reader that returns 1 KiB
   per `Read`; both must succeed and equal the original. This is the direct
   regression for the bug and fails today.
3. `oci-amber/blob` (after the comp-prysm bump): `Put` that same layer with
   `Options{VerifyRoundTrip: true}` and assert `Kind == KindPrism` — i.e. the
   test that reproduced this report. This is the end-to-end guard; note the
   real layer digest is `sha256:f14a9b19eeff4109ef4c17a44d959803dc47e1b4b98f756921c3e825c6b1fcea`.

## Files and lines referenced

* `comp-prysm/search/run.go:154` — `io.Copy(w, in.Spool.Reader())` in `evaluate`
* `comp-prysm/search/spool.go:72,74` — in-memory `bytes.Reader` vs spilled `io.SectionReader`
* `comp-prysm/recompress.go:111` — `io.Copy(dw, io.TeeReader(in, crc))`
* `comp-prysm/recompress.go:63-71` — input digest checked before output digest
* `comp-prysm/engine/zlib/zlib.go:35` (`bufSize`), `:161-190` (`writer.Write`)
* `comp-prysm/engine/zlib/zlib_test.go:102` — the test that gives false confidence
* `comp-prysm/analyze.go` — `gzipCandidates` (tier-major) and `engines.go`/`engines_cgo.go` engine order
* `oci-amber/blob/prism.go:369` (`roundTripCheck`), `:353` (`finalizePrism`) — behaved correctly
* Pinned `comp-prysm@v0.1.0` in the module cache is byte-identical to the
  `deps/comp-prysm` checkout for all three files above.
