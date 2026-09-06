# Import command design

`oci-amber import` inserts an image saved with `docker image save` into a
store without starting the registry. The archive's blobs go through the
same pipeline a push goes through (zrecipe, tar-prism, the rootfs view), a
terminal UI shows which layers are being worked on, how far each one is
and an estimated time to completion, and when everything is stored the UI
is torn down and a report of what was ingested is printed.

Decisions taken during the design review (2026-09-05):

- The archive's `index.json` points at the full upstream multi-platform
  index while only the local platform's blobs are present. The import
  publishes a **pruned index** holding the children whose blobs are
  present, which is what `docker push` does from the containerd image
  store. Its digest differs from the upstream index.
- The TUI is built on **Bubble Tea** (`charmbracelet/bubbletea`,
  `bubbles/progress`, `lipgloss`).
- Names come from the archive's `RepoTags` with a leading registry host
  stripped; `--name repo:tag` overrides for single-image archives.
- Blobs are read **in place** from the archive file through a section
  spool; nothing is copied to disk. A new `Observer` hook in `blob`
  reports stages and byte counts.
- The end-of-run **report** shows compressed and uncompressed bytes
  ingested, bytes added to the CAS, and the dedup ratio, each with a
  human-readable size and the exact count.

Decisions taken during implementation (2026-09-05):

- A raw blob's observer stages are analyze then raw (Put always enters
  analyze before deciding), not "raw only".
- A re-import re-publishes manifests, and `image.Store.Put` writes a
  fresh meta.json and root nodes each time, so "Added to CAS" for an
  all-present re-import is a few hundred bytes of manifest metadata
  rather than 0; the report's Dedup ratio line then reads "everything
  already present, N bytes of manifest metadata rewritten".
- `oci-layout` is read when present and exposed as `Archive.LayoutVersion`;
  its absence is not an error (index.json is the gate), a malformed one
  is.
- `PlanManifest.Attestation` marks BuildKit attestation manifests (from
  the `vnd.docker.reference.type` annotation on the referencing
  descriptor); the report's rootfs summary skips them.
- Two `index.json` entries resolving to the same image
  (`docker save img:a img:b`) produce one plan entry carrying both
  names.
- The terminal check uses `github.com/charmbracelet/x/term.IsTerminal`,
  already in the module graph through Bubble Tea and promoted from
  indirect to direct, so no new dependency was added.
- `tui.Run` returns a terminal failure as `*tui.TerminalError` joined with
  the import's result; the import is cancelled when the terminal dies.
- RepoTags that apply to several distinct images are rejected
  (ambiguous); index.json annotations are a follow-up.

## Goals

- Import a `docker image save` archive (Docker 25 and later, which write
  the OCI layout) into a store with exactly the processing a push gets:
  prism or raw blobs, manifest roots, rootfs views, tags.
- Live progress per layer and an ETA, in a terminal; sensible plain output
  when stdout is not a terminal.
- A report at the end: what was published, how many bytes came in
  (compressed and uncompressed), how many were added to the CAS, the
  dedup ratio.
- Resumable by construction: a cancelled import leaves published blobs in
  place and a re-run skips them.

## Non-goals

- Legacy-only archives (no `index.json`): older Docker and `podman save`
  in docker-archive format. Rejected with a clear message.
- Compressing uncompressed layers to mimic `docker push`. Layers are
  stored as they are in the archive; an uncompressed layer becomes a
  format `none` prism.
- Importing while the registry has the store open. The store's lock makes
  `Open` fail; the error is reported before any UI appears.
- Authentication, remote sources, OCI layout directories (not tars).

## What `docker image save` produces

Observed with Docker 29.4 and the containerd image store on `busybox:1.37`:

```
blobs/sha256/<hex>       every blob: manifests, indexes, configs, layers, attestation payloads
index.json               OCI layout index; one entry per saved image
manifest.json            legacy list: {Config, RepoTags, Layers} per image
oci-layout               {"imageLayoutVersion":"1.0.0"}
```

Blobs come first, the three small files last. Layers keep their original
compressed bytes (gzip here), so zrecipe applies. `index.json` references
the upstream index (18 children for busybox: 9 platforms and their
attestation manifests) but the archive holds only the local platform's
manifest, config and layer plus that platform's attestation manifest, its
config and in-toto payload. `manifest.json` names the platform image's
config and layers and carries `RepoTags: ["busybox:1.37"]`.

Docker with the classic graph driver writes the same layout with
uncompressed layers (`application/vnd.oci.image.layer.v1.tar`).

## Command

```
oci-amber import [flags] <archive>
```

`<archive>` is a path, or `-` for stdin, in which case stdin is copied to
`<work-dir>/oci-amber/import-<pid>.tar` first and the copy is removed at
exit. The archive must be a seekable file because blobs are read in place.

Flags shared with `serve`, same defaults, same `OCI_AMBER_*` environment
variables: `--store` (required), `--work-dir`, `--max-in-memory`,
`--analyze-parallelism`, `--analyze-timeout`, `--max-concurrent-finalize`,
`--verify-roundtrip`, `--log-level`.

New flags:

| Flag | Default | Meaning |
|---|---|---|
| `--name repo:tag` | none | publish under this name instead of the archive's `RepoTags`; repeatable; only valid when the archive holds one image |
| `--progress auto\|tui\|plain` | `auto` | `auto` picks `tui` when stdout is a terminal |
| `--log-file path` | none | write the full slog output there |

Logging: in `tui` mode without `--log-file`, records at warn and above are
kept in memory and printed to stderr after the screen is torn down; info
records (the registry's `blob stored` and `image pushed` lines) are
dropped because the UI conveys them. In `plain` mode slog writes to
stderr as `serve` does. With `--log-file` every record goes to the file
in both modes.

The work directory is `<work-dir>/oci-amber` as for `serve`; only
`spool/` (zrecipe's spill directory) is used and emptied at startup.

Exit status is 0 on success and 1 on any failure, including cancellation
(`q` or SIGINT), which prints `import cancelled`.

## Archive reading (`dockerarchive`)

`Open(path) (*Archive, error)` opens the file and makes one pass over the
tar headers. For every regular file `blobs/sha256/<hex>` it records offset
and size; `oci-layout`, `index.json` and `manifest.json` are read whole.
Anything else is ignored. Missing `index.json` is an error whose message
says Docker 25 or later writes the OCI layout. A malformed `index.json`
or `manifest.json` is an error.

- `Archive.ReadBlob(d) ([]byte, error)` reads a whole blob and verifies
  its sha256 against `d`; used for manifests, indexes and configs.
- `Archive.Section(d) (*io.SectionReader, error)` returns a reader over a
  blob's bytes for the spool.
- `Archive.Has(d) bool`.
- `Archive.Close()`.

The archive stays open for the whole import; `SectionReader`s over one
`*os.File` are safe for concurrent use.

## Planning

`Archive.Plan(PlanOptions{Names []string, Present func(oci.Digest) (bool, error)}) (*Plan, error)`
turns the archive into the list of things to store.

**Presence.** For each `index.json` entry, descriptors are walked depth
first:

- An image manifest (config or layers, no manifests) is *present* when
  its blob, its config blob and every layer blob are in the archive. A
  manifest with some but not all blobs present is an error; `docker save`
  never produces one.
- An index is *present* when its blob is in the archive and at least one
  child is present. Absent children are pruned: a copy of the index is
  synthesized that keeps only present children, in their original order.
  The copy is produced by a generic JSON edit (`map[string]json.RawMessage`
  with `manifests` filtered as `[]json.RawMessage`), so every other field
  and every retained descriptor survive untouched. Its digest is the
  sha256 of the new bytes. An index whose children are all absent is an
  error.
- A top-level entry whose blob is missing is an error.

**Blobs.** The plan lists every unique config, layer and other blob
referenced by present manifests, in first-use order, with digest, size
and media type, and whether `Present` reports it already stored. Present
blobs are not processed; they are shown as already present and excluded
from the ETA.

**Manifests.** The plan lists every present manifest and every
synthesized index, children before parents, with digest, media type and
body. Media type is the referencing descriptor's; the synthesized index
keeps the original's.

**Names.** `manifest.json` entries carry `RepoTags`. Each entry's `Config`
path names a config digest; the present image manifest with that config
belongs to one top-level entry, which receives the tags. A `manifest.json`
entry whose config matches no present manifest is an error. A leading
component that contains `.` or `:` or equals `localhost` is a registry
host and is dropped (`registry.example.ch/team/app:v1` becomes
`team/app:v1`; `busybox:1.37` stays). A reference without a tag gets
`latest`; a reference with a digest (`@sha256:…`) is ignored as a name.
Repository and tag are validated with `oci.ValidateRepository` and
`oci.ValidateTag`.

With `--name`, the archive must hold exactly one top-level entry, whose
names are replaced by the flag values, taken verbatim (no host stripping;
the tag is what follows the last `:` after the last `/`). A top-level
entry without names, from `RepoTags: null` and no `--name`, is an error
saying how to fix it.

`Plan` therefore holds `Blobs []PlanBlob{Digest, Size, MediaType, Present}`,
`Manifests []PlanManifest{Digest, MediaType, Body, IsIndex, Synthesized}`
and `Entries []PlanEntry{Digest, Names []Name{Repo, Tag}, Platforms int,
Attestations int}`, where the last two are counted from the entry's
descriptors: children with a `vnd.docker.reference.type:
attestation-manifest` annotation are attestations, others with a
`platform` are platforms.

## Spools without copying (`upload`)

`NewSectionSpool(r io.ReaderAt, off, size int64, d oci.Digest) *Spool` is
a third backing mode next to memory and file. `Open` returns an
`io.SectionReader`, `Remove` is a no-op that only marks the spool
removed. `Size` and `Digest` return what was given.

`blob.Put` trusts the spool's digest: the registry computes it while the
upload arrives. A prism whose bytes did not match would fail the
round-trip check and be downgraded to raw under the wrong digest rather
than fail, so the importer verifies every non-present blob's sha256
against its archive path before any blob is stored (the "checking"
phase below). Present blobs are not checked: the store already holds
correct content for their digest and the archive's copy is never read.

## Observer hook (`blob`)

```go
// Stage is one phase of a blob's finalization, in the order Put runs them.
type Stage string

const (
    StageAnalyze   Stage = "analyze"   // zrecipe pass one and the engine search
    StageDecompose Stage = "decompose" // pass two: decompress and take the tar apart
    StageVerify    Stage = "verify"    // round-trip check
    StageRaw       Stage = "raw"       // storing the bytes verbatim
)

// Observer receives finalization progress. Methods may be called from
// several goroutines at once, for different digests.
type Observer interface {
    BlobStage(d oci.Digest, s Stage)    // d entered s
    BlobProgress(d oci.Digest, n int64) // n bytes of the blob handled in the current stage
}
```

`Options.Observer` is optional; nil costs a nil check. `n` is counted
against the blob's compressed size in every stage and is monotonic within
a stage:

- analyze: the spool reader's sequential position during zrecipe pass one
  (the highest offset reached by `Read`, `ReadAt` does not count). The
  engine search that follows reads through `ReadAt` and gives no signal,
  so the count holds at the size until the stage ends.
- decompose: the spool reader's position in pass two.
- verify: bytes the round-trip recompression writes into the sha256.
- raw: bytes streamed into the store.

Implementation: a counting `ReaderAtSeeker` wrapper around the spool
reader in `analyze` and `ingestPrism`, around the reader in `ingestRaw`,
and a counting writer around the hash in `roundTripCheck`. A dedup hit
inside `Put` (a race with another writer) reports no stage at all. `Put`'s
outcome is its return value, so there is no done event.

## Importer (`importer`)

```go
type Importer struct{ ... }
func New(blobs *blob.Store, images *image.Store, arch *dockerarchive.Archive, plan *dockerarchive.Plan, tr *Tracker, opts Options) *Importer
func (im *Importer) Run(ctx context.Context) (*Report, error)
```

`Options.Workers` is the number of blobs processed at once (the CLI
passes `--max-concurrent-finalize`, which also sizes the blob store's
finalize slots).

`Run`:

1. **Checking.** Every non-present blob is read sequentially from the
   archive and its sha256 compared with its path. A mismatch fails the
   import before anything is written. Progress is bytes read over the
   total of non-present blob sizes; this is disk-speed I/O, seconds for
   a multi-gigabyte archive.
2. **Blobs.** A pool of `Workers` goroutines takes non-present blobs
   largest first (plan order among equals) and calls `blobs.Put` with a
   section spool; a blob's time is a single-threaded recompression
   proportional to its size, so the largest must not start last and run
   alone after the small ones. The first failure cancels the run and is
   returned. A blob that `Put` downgrades
   to raw is not a failure.
3. **Manifests.** `images.Put` with the body and media type. Image
   manifests (this is where the rootfs view is built) go through the
   same pool of `Workers` goroutines as the blobs, across every entry
   and repository, so a multi-arch image's platforms build at once
   (since 2026-09-07; before that every manifest was published in turn).
   Indexes follow, one entry at a time in plan order, children before
   parents. Child manifests are published by digest. Each top-level
   entry is published with its first name as the reference, which
   publishes both the manifest ref and the tag; further names are
   published with one more `Put` each. Entries with no name cannot occur
   (planning rejects them).
4. **Report.** Built from the plan, the `blob.Meta` of every blob (from
   `Put`, or from `blobs.Open` for present ones) and the `image.Meta`
   returned by the first `Put` of each top-level entry.

The blob store is created with a `RecentTTL` of one year: the image
store's `computeStats` takes each blob's accounting from the
recent-uploads table, and an import may well run longer than the
registry's one-hour default between a layer's `Put` and its manifest's.

## Tracker and ETA (`importer`)

`Tracker` is the shared progress state. It implements `blob.Observer`, so
it is created first and handed to `blob.New` and to `importer.New`. The
importer records state changes on it (`Queue`, `Checked`, `Skip`,
`Start`, `Done`, `Fail`, `ManifestStart`, `ManifestDone`); the blob store
records stages and byte counts. Renderers call `Snapshot()` whenever they
want; no change notifications exist, a 250 ms tick is fast enough.

```go
type Snapshot struct {
    Phase     Phase          // Checking, Blobs, Manifests, Done
    Checked   float64        // checking phase: bytes verified over bytes to verify
    Blobs     []BlobRow      // plan order: Digest, Size, Kind, State, Stage, Fraction, RawReason
    Counts    Counts         // Pending, InFlight, Done, Present, Raw
    Fraction  float64        // overall, 0..1
    Elapsed   time.Duration
    ETA       time.Duration
    ETAKnown  bool
    Manifests []ManifestRow  // Digest, Names, State, Rootfs status and entries
}
```

**Fraction.** Blobs are weighted by compressed size; present blobs are
out of both numerator and denominator. An in-flight blob contributes
`size × (base + share × n/size)` where the stage shares are analyze 0.5,
decompose 0.25 and verify 0.25 (analyze 0.5 and decompose 0.5 when
`--verify-roundtrip=false`); raw starts at whatever fraction the previous
stage reached (0 when it is the first stage, 0.5 after analyze, 0.75
after decompose) and takes the remainder, so a blob's fraction never goes
backwards.

**ETA.** Rate is the cumulative average over the blob phase: progress
bytes divided by the time since the first blob started. ETA is remaining
bytes divided by that rate. It is unknown (rendered "estimating") for the
first two seconds or while no progress has been made, and rendered as
">1h" beyond an hour. The cumulative average is deliberately slow to
react: dedup hits and the silent engine search would make a windowed
rate jump around. During the manifest phase the snapshot carries the
current manifest instead of an ETA.

## TUI (`tui`)

Bubble Tea inline (no alternate screen). `main` starts the importer on a
goroutine and the program on the main goroutine; a 250 ms tick fetches a
snapshot and re-renders; when `Run` returns, `main` sends a done message
and the model quits with an empty view, so the progress block vanishes
and the report is printed by `main` afterwards. `q` and Ctrl-C cancel the
import's context; the model then waits for the done message.

```
Importing busybox.tar → busybox:1.37                      elapsed 0:42

  layers  3 done · 1 already present · 2 in flight · 4 pending
  ▸ e001ca9b  1.9 MiB  decompose  ████████░░░░░░░░  52%
  ▸ 4f9c12aa  38 MiB   analyze    ████████████████  searching…
  ✓ 12 blobs stored, 1 already present · 7 pending

  ████████████░░░░░░░░░░░░░░░░░░  41%   ETA ~2m10s

  q or ctrl-c to cancel
```

During the checking phase the middle block is a single bar,
`checking archive ████░░░░ 41%`; no ETA is shown because the phase is
short and the ETA is defined over the blob phase.

Only in-flight blobs get a row; done and pending are collapsed into
counts, so a sixty-layer image fits. The analyze row shows "searching…"
while the count holds at the size. A row whose blob downgraded to raw
shows the reason briefly through the done count ("1 raw: not-tar") and in
the report. In the manifest phase the middle block is one line per
manifest: a spinner with "building rootfs", then the rootfs status and
entry count.

**Plain mode** (`--progress plain`, or `auto` off a terminal): slog to
stderr, a status line to stderr every five seconds from the same
snapshot (`layers 5/12 · 41% · ETA ~2m10s`), then the report on stdout.

## Report

Printed to stdout after the UI is gone, in both modes:

```
Imported busybox.tar in 42s

  busybox:1.37   sha256:3f0a…9c2e   index, 1 platform + 1 attestation   rootfs ok, 1,204 entries

Blobs           8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present
Compressed      1.8 MiB     1,900,727 bytes   blob and manifest bytes as they are in the archive
Uncompressed    4.2 MiB     4,388,352 bytes   after decompression; raw blobs counted as is
Added to CAS    1.1 MiB     1,153,024 bytes   appended to pack segments, manifests and rootfs tree included
Dedup ratio     1.65x       compressed bytes ÷ bytes added to CAS   (39.3% not written)
Chunks reused   38.2%       of offered bytes were already in the store
```

- **Header line per top-level entry**: every name, the published digest,
  kind (`index, N platforms + M attestations` or `manifest`), and the
  rootfs status with entry count for manifests (for an index, the status
  of its platform children summarized: `rootfs ok` when all are ok,
  otherwise the counts).
- **Blobs**: processed = non-present blobs; stored split into prism and
  raw with the raw reasons; already present = present at planning time
  plus dedup hits during the run.
- **Compressed**: sum of unique blob sizes plus unique manifest and index
  body sizes from the plan. Taken from the plan rather than
  `image.Stats.TotalBytes` because the latter counts a blob shared by two
  manifests twice.
- **Uncompressed**: `blob.Meta.UncompressedSize` for prisms, `Size` for
  raw blobs, over unique blobs; manifests count their body size.
- **Added to CAS**: sum of `image.Stats.DiskBytes` of the top-level
  entries' first `Put`. The image store folds in every blob's own
  accounting through the recent-uploads table (consumed on first use, so
  a blob shared by two entries counts once), the manifest objects and the
  rootfs tree.
- **Dedup ratio**: compressed ÷ added, with `1 − added/compressed` as the
  percentage. When no blob was processed the line reads `everything
  already present`, plus `, N bytes of manifest metadata rewritten` when
  Added > 0.
- **Chunks reused**: `DedupedBytes ÷ LogicalBytes` over the same image
  stats, the figure the registry logs as `deduped_percent`. It separates
  chunk-level dedup from compression in the ratio above. A blob shared by
  two entries is offered once and reused once here, which is what
  happened.

Byte counts show a human-readable size (binary units, one decimal) and
the exact number with thousands separators.

## Failure and cancellation

- Cancellation (`q`, SIGINT) cancels the context; in-flight `Put`s abort
  and publish nothing. Published blobs and manifests stay, so a re-run
  resumes through dedup hits.
- A blob whose bytes do not match its path's digest fails the import
  during the checking phase, before anything is written.
- A store locked by a running `serve` fails at `store.Open`.
- Any error is printed after the UI is torn down; exit status 1.

## Package changes

- `upload`: `NewSectionSpool`.
- `blob`: `Stage`, `Observer`, `Options.Observer`; counting wrappers in
  `analyze`, `ingestPrism`, `ingestRaw`, `roundTripCheck`.
- `dockerarchive` (new): `Open`, `Archive`, `Plan`.
- `importer` (new): `Importer`, `Tracker`, `Snapshot`, `Report`.
- `tui` (new): the Bubble Tea model, the plain renderer, report and size
  formatting.
- `cmd/oci-amber`: the `import` subcommand; the shared flag table is
  factored out of `serveFlags`.
- README: an "Importing a docker save archive" section.
- New dependencies: `github.com/charmbracelet/bubbletea`,
  `github.com/charmbracelet/bubbles`, `github.com/charmbracelet/lipgloss`.
  The terminal check uses `github.com/charmbracelet/x/term.IsTerminal`,
  already in the module graph through Bubble Tea and promoted from
  indirect to direct, so it adds no dependency of its own.

## Testing

- `dockerarchive`: archives built in memory in the observed shape (blobs
  first, `index.json` and `manifest.json` last, nested index with absent
  platforms, attestation manifest). Cases: pruning keeps present children
  in order and recomputes the digest while other fields survive; names
  through `RepoTags`; host stripping; `--name` on a multi-image archive
  rejected; no `index.json`; missing config; all children absent;
  partial manifest; digest mismatch in a small blob; `Present` marks
  blobs.
- `upload`: a section spool reads only its window; `Remove` leaves the
  file alone; `Open` after `Remove` fails.
- `blob`: observer sequence for a prism (analyze, decompose, verify, each
  reaching the size), a raw blob (analyze, then raw), a decompose downgrade
  (analyze, decompose, raw); every existing test passes with a nil
  observer.
- `importer`: end to end against a real store in a temp dir with gzip tar
  layers from the existing test helpers: the tag resolves to an index
  with the present children, layers are prisms, the rootfs is built, the
  report's numbers match the metas; a second run is all dedup hits and
  adds nothing; cancellation mid-run publishes no tag; a corrupt layer
  fails in the checking phase with nothing published. `Tracker` unit
  tests: fractions through the stages with and without verify, the raw
  remainder rule, ETA from a scripted sequence with a fake clock.
- `tui`: `View()` for the blob phase, the manifest phase and the empty
  final frame; report rendering; size and duration formatting.
- Manual: `docker image save` of busybox and of a larger local image into
  a fresh store, then `serve` and `crane pull` to confirm the tag pulls
  byte-identical.
