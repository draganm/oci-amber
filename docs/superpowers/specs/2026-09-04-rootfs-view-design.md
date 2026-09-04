# Root filesystem view design

When an image manifest is pushed, oci-amber builds the container's root
filesystem from the stored layers and keeps it in the image root as an amber
directory tree (`rootfs/`). The tree is a real filesystem view: paths,
permission bits, ownership, mtimes, symlinks, hard links, device nodes, FIFOs
and extended attributes, with every regular file pointing at the content the
layer's prism already holds. It is the substrate for a later image browser
and for serving parts of an image without rebuilding a layer.

Decisions taken during the design review (2026-09-04):

- Only the merged rootfs is stored, in the image root. Blob roots do not gain
  a per-layer tree.
- Layers stored raw do not get a view; an image with such a layer records
  that its rootfs is unavailable.
- No backfill of existing stores. Images pushed by the new binary get a
  rootfs; an image already stored gets one when it is pushed again.
- No HTTP surface in this change.

## Goals

- Every image manifest push that describes a container image leaves a
  `rootfs/` directory in its image root, built by applying the layers in
  manifest order with OCI whiteout semantics.
- Building it reads tar headers only, never file contents: the recipe a
  prism stores has every header, and file contents are referenced by the
  keys the prism already stores.
- The result is a plain amber fstree with full metadata, so amber's own tools
  (`tarexport`, restore) work on it, and it deduplicates like everything
  else: the same image in two repositories shares every rootfs object.
- A view that cannot be built never fails a push. The image is stored as
  before and `meta.json` says why there is no view.

## Non-goals

- HTTP endpoints for browsing or partial download (a later feature that
  reads `rootfs/`).
- Backfilling images already in a store.
- Views for raw layers.
- Validating the config's `rootfs.diff_ids` against the manifest's layers.
- Per-layer trees in blob roots.

## Where the layer trees come from

A prism blob root holds `recipe.bin` (every tar byte that is not regular file
content: headers, PAX and GNU meta entries, padding, the end-of-archive
marker), `recipe.json` (for each regular file, the recipe offset where its
content was cut out, its size and its `blobs/%08d` name) and `blobs/`
(the contents, as amber file objects).

`rootfs` rebuilds a virtual tar from these parts and reads it with
`archive/tar`:

- Recipe bytes are streamed from the store. At each index entry's offset a
  content region of `entry.Size` virtual bytes is spliced in.
- The virtual reader implements `io.Seeker` for `io.SeekCurrent`.
  `archive/tar` skips a file's content by seeking to its last byte and
  reading one byte, so content regions cost nothing to pass over. Seeking
  across recipe bytes discards them by reading.
- A read at offset 0 of a content region serves the real bytes, opened
  lazily from the blob's content key. `archive/tar` reads content there only
  for a PAX sparse 1.0 map, so an archive with such an entry still parses.
  A read anywhere else in a region serves zero bytes; nothing consumes them.
- After `Next` returns a header whose type carries content (regular,
  contiguous, old GNU sparse), the current position must be the start of the
  next unconsumed index entry; that entry's blob key is the file's content.
  Any other position means the header stream and the index disagree, and
  the layer is reported unparsable.

Cost is proportional to header bytes, about 512 bytes per entry plus PAX
records, whatever the size of the files.

Known limits, all reported rather than silently wrong:

- A hard link, directory or other header-only entry whose size field is not
  zero and whose payload tar-prism kept in the recipe desynchronizes
  `archive/tar`, which treats such entries as having no payload. The
  checksum of the next block fails and the layer is unparsable.
- Sparse files (old GNU `S` entries, PAX `GNU.sparse.*` records) are skipped:
  the content in the store is the archive's compact form, not the file.

## Applicability

The rootfs is built for image manifests (`kind = manifest`) whose config
media type is `application/vnd.oci.image.config.v1+json` or
`application/vnd.docker.container.image.v1+json`. Indexes have no rootfs and
no `rootfs` field in their meta. Every other manifest (artifacts, Helm
charts, signatures) records `not-applicable`, so pushes of artifacts never
log a missing view.

Layer media types are not checked. A layer whose blob is a prism is a tar by
construction; a layer whose blob is raw makes the rootfs unavailable with the
reason `layer <digest> is stored raw (<rawReason>)`.

## Applying layers

Layers are applied in manifest order to an in-memory tree that starts empty.
Each layer is applied in three steps: parse every header into a list, apply
the layer's whiteouts, then apply its entries in archive order. Parsing the
whole layer before touching the tree is what makes whiteouts apply to lower
layers only: the layer's own entries are not in the tree yet when its
whiteouts run, and the entries that follow can only be hidden by a later
layer's whiteouts, as the OCI image spec requires.

### Paths

A header name is cleaned: leading `/` and `./` are dropped, a trailing `/`
is dropped, `path.Clean` is applied. The cleaned name `.` is the root; a
directory entry for it is ignored (an amber directory root carries no
metadata of its own), any other type for it is skipped. A name that starts
with `..` after cleaning escapes the root and is skipped.

Parent components are resolved the way a kernel resolves them: a component
that is a symlink is followed inside the rootfs (an absolute target starts at
the rootfs root, `..` at the root stays at the root); more than 40 links in
one resolution skips the entry. A missing component is created as an
implicit directory (mode `0o40755`, uid and gid 0, mtime 0). A component
that exists and is neither a directory nor a symlink is replaced by an
implicit directory: the later entry wins. This is what makes usrmerge
layouts come out right: with `bin -> usr/bin` in a lower layer, a later
layer's `bin/foo` lands in `usr/bin`.

The final component is never resolved through a symlink.

### Entries

An entry replaces whatever is at its path. Directory over directory keeps the
existing children and takes the new metadata (an implicit directory becomes
explicit). Anything else drops the old subtree. The same rule applies inside
one layer when a name appears twice.

Per type:

| tar type | fstree entry |
|---|---|
| regular (`0`, `\0`, `7`) | `S_IFREG`, content key from the index |
| directory (`5`) | `S_IFDIR`, children from the tree |
| symlink (`2`) | `S_IFLNK`, `LinkTarget` = linkname verbatim |
| hard link (`1`) | the target's payload (content key, link target or device numbers), the target's type bits, this header's permission bits, uid, gid, mtime and xattrs |
| char / block (`3`, `4`) | `S_IFCHR` / `S_IFBLK`, `Rdev = [major, minor]` |
| FIFO (`6`) | `S_IFIFO` |
| PAX global header (`g`) | ignored |
| anything else, sparse files | skipped |

Mode is the header's permission bits, setuid, setgid and sticky (`mode &
0o7777`) with the type bits from the typeflag. uid and gid are the numeric
header fields; user and group names are dropped. Mtime is the header's
`ModTime` in nanoseconds (PAX keeps sub-second precision; a zero time is 0).
Extended attributes are the PAX `SCHILY.xattr.*` records; a set whose CBOR
encoding is at most 256 bytes is inlined in the entry, a larger one is
spilled to an `XattrSet` object, the same rule as amber's ingest driver.

A hard link's target is resolved from the rootfs root with the same parent
resolution as any path. A target that does not exist or is a directory skips
the entry.

### Whiteouts

A regular file named `.wh.<name>` is a whiteout: it removes `<name>` from
the directory it is in, whatever `<name>` is (a file, a symlink, a whole
subtree). `.wh..wh..opq` is an opaque whiteout: it removes every child of
its directory. Both resolve their directory the same way as parents above.
Whiteouts never appear in the tree, and a whiteout for something that does
not exist is a no-op.

### Skips

A skipped entry does not fail the layer. Each skip records the layer digest,
the cleaned path and a reason; the rootfs is still built and the status is
`partial`. The stored list keeps the first 100 skips and the total count.

Reasons: `sparse file`, `path escapes the root`, `hard link target not
found`, `hard link to a directory`, `symlink loop`, `unsupported type <flag>`,
`root is not a directory`.

### Emission

After the last layer the tree is written bottom-up through the image's
pass-one accounting writer: for each directory the children are sorted
bytewise and added with `store.Dir.AddEntry`, the directory key becomes the
parent's `ContentKey`. Regular files reference existing content keys and
write nothing; only directory objects and spilled xattr sets are new, so
they count in the image's stats. `Entries` is the number of entries in the
tree, the root excluded.

Memory is one node per entry of the merged tree: name, metadata, one key or
link target, xattrs when present, and a children map for directories.

## Data model

Image root, manifests only:

```
manifest      the exact bytes the client PUT
meta.json     Meta, now with a rootfs field
blobs/        <digest> -> blob root
rootfs/       the merged root filesystem            status ok or partial only
```

`meta.json` gains a `rootfs` object on manifests (omitted on indexes):

```json
"rootfs": {
  "status": "ok",
  "entries": 4213
}
```

```json
"rootfs": {
  "status": "partial",
  "entries": 4212,
  "skipped": [{"layer": "sha256:…", "path": "usr/lib/big.img", "reason": "sparse file"}],
  "skippedCount": 1
}
```

```json
"rootfs": {
  "status": "unavailable",
  "reason": "layer sha256:… is stored raw (not-reproducible)"
}
```

```json
"rootfs": {"status": "not-applicable"}
```

`status` is one of `ok`, `partial`, `unavailable`, `not-applicable`.
`entries` is present for `ok` and `partial`; `skipped` and `skippedCount`
for `partial`; `reason` for `unavailable`.

Directory entries under `rootfs/` carry the metadata described above; the
rest of the image root and every blob root keep the fixed modes and zero
uid, gid and mtime they have today.

## Package changes

Flat packages, no `internal/`. Import direction stays acyclic:
`image -> rootfs -> store -> oci`, `image -> blob -> store`; `rootfs` does not
import `blob`.

### `rootfs` (new)

```go
// Layer is what the builder needs from a stored prism.
type Layer interface {
    Index() (*tarprism.Index, error)
    Recipe() (io.ReadCloser, error)
    BlobKey(index int, entry tarprism.Entry) (key.Key, error)
}

type Builder
func New() *Builder
// Apply parses layer and merges it over what was applied before. A
// *LayerError means the archive could not be parsed; the image gets no
// rootfs. Any other error is a store or context failure.
func (b *Builder) Apply(ctx context.Context, digest oci.Digest, layer Layer) error
// Write emits the tree through w and returns the root key.
func (b *Builder) Write(w *store.Writer) (Result, error)

type Result struct {
    Root         key.Key
    Entries      int
    Skipped      []Skip // at most MaxSkipped
    SkippedCount int
}
type Skip struct {
    Layer  oci.Digest `json:"layer"`
    Path   string     `json:"path"`
    Reason string     `json:"reason"`
}
type LayerError struct { Layer oci.Digest; Err error }
```

Files: `layer.go` (virtual tar reader, header to entry conversion),
`tree.go` (nodes, path resolution, whiteouts, replacement rules),
`builder.go` (Apply, Write, skips), tests alongside.

### `store`

- `Dir.AddEntry(e fstree.Entry) error`: adds an entry with the metadata it
  carries. It validates the name and order like `add`, and the payload
  against the type bits: a content key of file type for `S_IFREG`, of
  directory type for `S_IFDIR`, a non-empty `LinkTarget` for `S_IFLNK`,
  two `Rdev` values for `S_IFCHR` and `S_IFBLK`, and nothing for `S_IFIFO`
  and `S_IFSOCK`; any other type bits are an error. `AddFile` and `AddDir`
  become wrappers.
- `Writer.PutXattrs(m map[string][]byte) (inline []byte, spilled key.Key, err error)`:
  encodes the set, returns it inline when its encoding is at most
  `XattrInlineMax` (256) bytes, otherwise emits an `XattrSet` object and
  returns its key.
- Type-bit constants (`TypeMask`, `TypeReg`, `TypeDir`, `TypeLink`,
  `TypeChar`, `TypeBlock`, `TypeFIFO`, `TypeSocket`), the POSIX values,
  defined locally so `store` does not depend on `x/sys`.

### `blob`

- `amberSource` is exported as `Prism` and gains
  `BlobKey(index int, entry tarprism.Entry) (key.Key, error)`, the lookup
  `Blob` already does before opening a reader. `*Prism` satisfies
  `rootfs.Layer` without `blob` importing `rootfs`.
- `(*Blob).Prism() (*Prism, error)` resolves the prism parts of a prism
  blob root; a raw blob returns `ErrNotPrism`.
- `analyze.go`: the tar-header probe accepts an all-zero first block, the
  end-of-archive marker of an empty archive. A gzipped empty tar, the blob
  Docker uses as an empty layer, becomes a prism with `entries=0`
  (`format=gzip`) instead of raw `not-tar`, and merges as a no-op. A
  compressed stream of zeros that is not a tar takes the same path: tar-prism
  copies it into the recipe and it round-trips.

### `oci`

- `MediaTypeDockerConfig = "application/vnd.docker.container.image.v1+json"`.

### `image`

- `RootfsDir = "rootfs"`; `Meta.Rootfs *Rootfs` (`json:"rootfs,omitempty"`);
  `Rootfs{Status, Entries, Reason, Skipped, SkippedCount}` with
  `RootfsStatus` constants.
- `Put`, for an applicable manifest, between blob resolution and the
  manifest's own objects:
  1. If `oci/manifest/<repo>/<digest>` already resolves to a root whose
     meta carries a `rootfs` field, reuse that field and, for `ok` and
     `partial`, the existing `rootfs/` key. Nothing is rebuilt on a re-push.
  2. Otherwise open every layer blob. A raw one sets `unavailable` with its
     reason and stops.
  3. Otherwise apply the layers through a `rootfs.Builder` and write the
     tree through the pass-one writer. A `*rootfs.LayerError` sets
     `unavailable` with `layer <digest>: <error>`; any other error fails
     the push like every internal error.
  4. Add `rootfs/` to the image root when there is a key.
- `Image.Rootfs() (key.Key, bool)`: the `rootfs/` key when present. `Open`
  looks it up.
- Log line: `image pushed` gains `rootfs=<status>` and, for `ok` and
  `partial`, `rootfs_entries=<n>` on manifests. `unavailable` also logs
  `level=WARN msg="rootfs unavailable" repo= digest= reason=`; `partial`
  logs `level=WARN msg="rootfs partial" repo= digest= skipped=<count>
  path=<first path> reason=<first reason>`.

The repository lock is held across the build, as it is across the rest of
`Put`.

### Docs

README: storage layout (image root gains `rootfs/`), meta description, the
new log keys and Warn lines, the empty-layer classification, a sentence in
Limitations about hard links with payloads and sparse files.
`docs/followups.md` gets the deferred items below.

## Testing

`rootfs`:

- Virtual reader: for tars written by `archive/tar` in PAX and GNU formats
  (long names, long link names, PAX mtime and xattrs), the headers read
  through the splice equal the headers `archive/tar` reads from the original
  archive, and no content region is opened for files longer than one byte
  (the test's `Layer` counts `BlobKey` reads that open readers). Old GNU
  sparse entries are detected and skipped. A desynchronized index (an entry
  removed) is a `*LayerError`; a store read failure on the recipe is not.
- Builder: add, replace file over dir and dir over file, dir over dir keeps
  children and takes metadata, implicit then explicit directory, duplicate
  name inside one layer, whiteout of a file, of a subtree, of a symlink,
  opaque whiteout, same-layer whiteout protection (`.wh.foo` and `foo` in
  one layer keeps `foo`), whiteout for a missing name, symlinked parent
  (`bin -> usr/bin` then `bin/foo`), absolute symlink parent, `..` at the
  root, symlink loop, symlink, hard link to a file, to a symlink, missing
  and to a directory, char and block devices, FIFO, xattrs inline and
  spilled, sparse skipped, escaping path skipped, unknown typeflag skipped,
  root entry ignored, skip list capped at `MaxSkipped`, `Entries` count,
  empty layer, empty image (no layers) yields an empty directory.
- Determinism: the same layers applied twice yield the same root key.
- Emitted tree checked through `store.ListDir` and `tarexport` from amber
  (export the rootfs to a tar and compare with the expected entries).

`store`: `AddEntry` accepts each type with its payload and rejects mismatched
payloads, socket type, unsorted names; `PutXattrs` inline and spill.

`blob`: `Prism` for a prism and `ErrNotPrism` for a raw blob; gzipped empty
tar is a prism with zero entries; a tar whose first block is all zeros but is
otherwise not empty still round-trips.

`image`: status `ok` with entries and a walkable `rootfs/`; `partial`;
`unavailable` for a raw layer and for an unparsable layer, push still
succeeds, Warn line present; `not-applicable` for an artifact; no field on
an index; re-push reuses the key (root differs by `createdAt`, `rootfs/`
key equal, no rebuild observable through a counting `Layer`); the same
image in two repositories has the same `rootfs/` key; log keys present.

`registry/e2e_test.go`: after the push phase, the stored image 1 has a
`rootfs/` whose paths are layer A's entries with layer B's `etc/` files
merged in, checked through the image store; the manifest's meta reports
`ok`.

## Follow-ups (deferred)

- Move blob resolution and the rootfs build before the repository lock so
  concurrent pushes to one repository do not serialize on it.
- A view for raw tar layers, storing their contents a second time.
- Backfill command for existing stores.
- HTTP endpoints over `rootfs/` (browse, fetch a file or a subtree as a
  tar).
- Serve `rootfs/` metadata for the root directory itself (amber roots carry
  none).

## Deviations

Recorded while implementing (2026-09-04):

- `meta.json`'s `rootfs.entries` is always present, 0 unless the status is
  `ok` or `partial`, rather than omitted. The other fields follow the
  section above.
- A one-byte regular file has its byte read from the store while parsing:
  `archive/tar` reads the last byte of every file after seeking, and for a
  one-byte file that byte is at the region's start, where real content is
  served. Every longer file is passed over without a read.
- Test fixtures confirmed that `archive/tar` treats a hard link's size field
  as zero, exactly as "Known limits" says; the rootfs and image tests craft
  such an archive to prove the `unavailable` path.
