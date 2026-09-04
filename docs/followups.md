# Follow-ups deferred from the implementation reviews

Collected from the per-task and whole-branch reviews of the initial implementation (2026-09-03/04). None of these blocks the current behaviour; the whole-branch review triaged them as "can wait". Items are grouped by the area they were found in.

## Upstream (outside this repository)

- tar-prism: `sink-source-api` is pinned as a pseudo-version of an unmerged branch; merge and tag it, then re-pin. Minor notes from its review: `DecomposeTo` closes the recipe writer through explicit calls rather than a defer; a sink or source returning `(nil, nil)` panics instead of erroring; a final `(n>0, non-EOF err)` read is treated as an error even when all bytes were consumed; the compose loop's error label says "copying recipe" for writer failures.
- comp-prysm: zlib engine at deflate level 0 emits stored blocks sized by the caller's writes, so `Analyze` accepts streams `Recompress` cannot rebuild (see `docs/comp-prysm-zlib-level0-roundtrip.md`). Also: no option to bound the pass-one zstd decoder window (oci-amber pre-checks the frame header instead).

## By area

### oci (Task 2)

- untyped manifest with both config/layers and manifests classifies as index; no test pins it (oci/manifest.go:103)
- BlobDescriptors shallow-copies Annotations maps (oci/manifest.go:157)
- "not valid JSON" message also fires on type errors (oci/manifest.go:55)
- Algorithm()/Hex() on an unparsed Digest degrade silently (oci/digest.go:69)
- TestDigestOfBytes/TestIsDigest loop without t.Run

### store (Task 3)

- shutdown order expressed only via errors.Join argument order (store/store.go:213)
- config decode accepts trailing JSON after the first value (store/store.go:158)
- writeConfig does not fsync the parent dir; fixed .tmp name written before the packstore flock (store/store.go:181-201)
- Has returns unwrapped packstore errors while Get wraps (store/store.go:236)
- GC-interval test never exercises a real cycle; several tests open with nil Logger and print to stderr (store/store_test.go:125,141,162,230)
- ListRefs fails wholesale on one undecodable record; untested (store/refs.go:103)

### store writer (Task 4)

- a dropped Writer leaks its run goroutine until Abort/Close — later tasks must `defer w.Abort()` (store/write.go:99)
- emitBuffer memory comment undercounts packstore's own in-flight buffers (store/write.go:64)
- Dir not poisoned after a failed AddEntry; type check precedes done check (store/dir.go:36-71)
- generic identifier writers() (store/write.go:115)
- coverage gaps: Close with zero objects, PutStream at exactly MaxSize/MaxSize+1, prefix names in Dir, Abort concurrent with Close; 50ms sleep in TestAbortUnblocksInFlightPutStream

### store reader (Task 5)

- Skip on a directory key with large n succeeds silently; type check should precede the length comparison (store/read.go:187)
- Get-count test helper tallies Blob fetches only, not index nodes (store/read_test.go:99)
- inconsistent error wrapping (bare fstree errors from descend/Lookup/ListDir/WriteContent)
- LookupKey "has no content key" message also covers a malformed key (store/read.go:227)
- ListDir limit<=0 accumulates all entries; comment overstates the memory bound (store/read.go:232)
- untested: zero-length Read buffer, empty directory, ReadFile length-mismatch guard
- Reader guards use a different wording than ReadFile's length-mismatch error (store/read.go:40 vs 115,185)

### upload (Task 6)

- Spool.Remove races Open (no sync; doc says Open is concurrent) (upload/spool.go:57-78)
- errSpoolRemoved/errClosed unexported; Open on a removed session file returns a raw PathError (upload/spool.go:26, manager.go:15)
- Manager.Remove takes s.mu via Offset() just for a log field (upload/manager.go:157)
- sweeper passes the ticker's stale timestamp to Sweep (upload/manager.go:108)
- untested: zero-byte Append, exactly-one-over threshold, writer error mid-copy, Sweep racing Append; brittle "count=3" log assertion
- active() rebuilds time.Time from UnixNano, losing the monotonic reading; idle timeout is wall-clock sensitive (upload/session.go:171)
- upload/TestBackgroundSweeper flaked once under whole-repo -race, passes on rerun — timing-sensitive test

### blob raw path (Task 7)

- finalizeRaw/writeRoot do not call Abort when Close fails (harmless: Close releases everything) (blob/store.go:383,411)
- no test for failures after the accounting writer started (abort branches, Publish failure) (blob/store.go:380,408)
- readMeta error paths untested (missing meta.json, non-directory root)
- Delete blocks on the per-digest lock without ctx during a finalize; range responses are served without the sha256 tee (blob/store.go:160, pull.go:58)

### blob prism path (Task 8)

- "amber write errors never fall back to raw" has no fault-injection test (blob/prism.go:317-324, 169-172)
- no test for a tar with zero regular files (empty blobs/)
- compose failure coinciding with client disconnect is reported as cancellation (blob/prism.go:475)
- round-trip failure leaves the decomposed objects committed and unreferenced for GC (blob/prism.go:332)
- roundTripCheck package var swapped by tests without sync; empty `case KindRaw:` (blob/store.go:377)

### image (Task 9)

- Put doc claims nothing published on error, false after a tag/referrer publish failure (image/store.go:118)
- TakeRecent consumed before publish; an internal failure afterwards loses per-blob stats for the retry (image/stats.go:46)
- compression_ratio +Inf renders as an error string under slog JSON handler (text handler is the default) (image/stats.go:102)
- resolveChildren and computeStats resolve children twice; resolveBlobs/resolveChildren near-duplicates (plan-mandated signature)
- ParseTagRef cuts at the first ':' (equivalent given charsets) (image/refs.go:44)
- repo lock held across the whole Put; re-push mints a new root (meta has CreatedAt); no Content-Type vs body-kind cross-check; readMeta ignores Version

### image listings (Task 10)

- "oci/tag/" and "oci/manifest/" literals in list.go duplicate TagPrefix/ManifestPrefix (image/list.go:115,124)
- Referrers aborts the whole listing on one readMeta failure; a lock-free listing racing Delete+GC could 500 (image/list.go:87)
- unparseable refs skipped silently without logging; Repositories scans all refs twice (image/list.go:29,84,115,124)

### registry blobs/uploads (Task 11)

- serveBlob abort path untested (registry/blobs.go:97-110)
- PATCH parses then discards Content-Range end; PUT ignores Content-Range; POST with both mount and digest silently prefers mount (registry/uploads.go:96-110,151,172)
- blob 416 carries no error envelope; DELETE of a spilled session untested; undrained body on 416/400 costs a reconnect for multi-MB PATCHes
- isClientGone treats any net.Error as client-gone; safe today (no network-backed stores), revisit if one is added (registry/errors.go:98)

### registry manifests/lists (Task 12)

- isClientGone precedes the started check, so a local short read after the first byte logs at debug (registry/manifests.go:72, blobs.go:93)
- unreachable nil guard on referrers list; writeJSONAs duplicates writeJSON; paginate linear scan; no empty-body PUT test; HEAD on tags/list and _catalog answers 405

### cmd (Task 13)

- second SIGINT during the 30 s drain does nothing (defer stop()) (cmd/oci-amber/main.go:66-73)
- accept-failure branch closes stores without Shutdown/Close of the server (main.go:266-269); Close after a capped Shutdown does not stop handler goroutines (main.go:273-278); <-serveErr value discarded (main.go:280)
- unconditional os.RemoveAll of <work-dir>/spool on an operator-supplied path (main.go:202)
- README mentions the crane smoke test before Task 14 adds it (README.md:180); README suggests /usr/local/bin build path; no run-level test with a non-default work dir

### end-to-end tests (Task 14)

- error-log assertion in pushInterrupted matches any PUT "request failed", not this session's path (registry/e2e_test.go:915)
- fault injection by renaming the spool file depends on unexported layout (registry/e2e_test.go:897)
- io.ErrUnexpectedEOF also over-matches local truncated reads in isClientGone (registry/errors.go:113)
- README omits the "blob already present" Info line for whole-blob dedup hits (README.md:130)
- crane test only logs the appended layer's kind; roundtrip-failed on crane's gzip is under investigation (cmd/oci-amber/crane_test.go:156)
- craneHeadBlob uses http.Head/DefaultTransport; e2e `check` closure has 8 positional params

### upload sweeper fix

- if file.Close fails but the unlink succeeds, the fd close is never retried (no OS leak; stale *os.File in a forgotten session) (upload/session.go:207-219)


### Observed during the final fix wave

- A gzipped empty tar (the 1024 zero-byte end-of-archive marker, e.g. an OCI empty layer) is now classified raw/not-tar because an all-zero first block fails the tar-header probe; harmless (round-trips, 32 bytes) and consistent with the format=none rule, but a behaviour change worth a test.
- Sweep counts a session whose close failed; blob.New no longer removes a stale non-directory at <WorkDir>/spool (unreachable under the ownership rule).
