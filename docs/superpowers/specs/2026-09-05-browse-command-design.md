# Browse command design

`oci-amber browse` is a terminal browser over a store. It walks the images
the store holds, shows how each one is kept (the amber trees of image
roots, blob roots, prisms and their parts) or the root filesystem the
image's layers produce, and reads any file it reaches: text for text,
pretty-printed JSON for JSON, a hex dump for everything else.

Decisions taken during the design review (2026-09-05):

- The browser **opens the store directly**, like `import`. It cannot run
  while `serve` holds the store; an HTTP-backed browser is a possible
  follow-up and nothing in the design prevents it.
- The storage view is the amber tree **annotated** with what the browser
  recognises (blob kind, format and engine from meta.json, tar entry names
  next to a prism's numbered blobs, platforms next to index children).
- The entry screen lists **images only** (repositories, then tags and
  untagged digests). A raw references view is a follow-up.
- **Single pane with a breadcrumb**; one listing at a time, the viewer
  full screen. Not two panes, not Miller columns.
- Viewer defaults: line numbers on, no wrapping (horizontal scroll), JSON
  pretty-printed with a key to see the stored bytes.
- Code shape: a `browse` package with a small node interface and a frame
  stack, one list implementation for every level, I/O in Bubble Tea
  commands. Opening the store for browsing has **no side effects**.

## Goals

- Find an image by repository and tag (or digest) and see what the store
  holds for it: manifest, metadata, every blob and how it was stored, the
  parts of a prism, the child manifests of an index.
- Browse the root filesystem of a container image with symlinks followed,
  the way the `/fs/` API resolves paths.
- Read files: configuration, manifests, recipes and file contents as text
  or JSON; binaries and recipes as hex, at any size.
- Never write to the store or to the work directory.

## Non-goals

- Editing, deleting, exporting or copying anything out of the store.
- Running against a live registry over HTTP.
- Syntax highlighting, image or archive previews, content-type sniffing
  beyond text/JSON/binary.
- Browsing references that no image reaches (orphaned blobs, referrers by
  themselves). Referrers are reachable only through the manifests that
  carry them.

## Command

```
oci-amber browse --store DIR [--log-file path] [repo[:tag|@digest]]
```

| Flag | Default | Meaning | Environment |
|---|---|---|---|
| `--store` | required | store directory | `OCI_AMBER_STORE` |
| `--log-file` | none | write slog output there; without it records at warn and above are kept in memory and printed to stderr after the screen is torn down, as `import` does in TUI mode | `OCI_AMBER_LOG_FILE` |

The optional argument opens the browser inside that image: `repo` starts
at the repository's listing, `repo:tag` and `repo@sha256:…` at the image's
storage root. A reference that does not exist is an error before any UI
appears.

Startup rules, checked in this order:

1. `DIR/oci-amber.json` must exist, otherwise `no oci-amber store at DIR`.
   `store.Open` creates a missing store; the browser must not.
2. Stdout must be a terminal (`term.IsTerminal`), otherwise
   `browse needs a terminal`. There is no plain mode.
3. `store.Open` with GC disabled. A store held by another process fails
   with `store DIR is in use by another process`: `store.Open` maps the
   packstore's flock failure (`EWOULDBLOCK`) to a new `store.ErrInUse`.
4. `blob.NewReadOnly(st)` returns a blob store that touches no work
   directory; `Put` and `Delete` on it return `blob.ErrReadOnly`.
   `image.New(st, blobs, log)` has no side effects already.

Exit status is 0 after `q`, `ctrl-c` or SIGINT; the store is closed and a
close error makes the exit status 1 with the error printed. A Bubble Tea
failure is reported as `*tui.TerminalError` after the store is closed.

## Package `browse`

A flat top-level package (never `internal/`). It imports `store`, `image`,
`blob`, `rootfs`, `oci` and `tui` (for `FormatBytes`, `FormatCount` and
`ShortDigest`); `tui` does not import it.

### Nodes and rows

```go
// Node is one place the browser can be: a listing or a file.
type Node interface {
	// Crumb is the node's breadcrumb segment.
	Crumb() string
}

// Lister is a Node that lists rows.
type Lister interface {
	Node
	List() ([]Row, error)
}

// Opener is a Node that is a file.
type Opener interface {
	Node
	Open() (*File, error)
}

// Row is one line of a listing.
type Row struct {
	Name   string  // first column
	Detail string  // annotation, already formatted, may be empty
	Size   int64   // bytes; shown when HasSize
	HasSize bool
	Meta   *RowMeta // ls -l columns for rootfs rows; nil elsewhere
	Info   []KV     // the info popup, label/value pairs
	Child  Node     // what Enter opens; nil when nothing (device, dangling symlink)
	IsDir  bool     // styling and sort group
}

// RowMeta are the ls -l columns.
type RowMeta struct {
	Mode  uint64 // type bits and permissions
	UID, GID uint64
	Mtime time.Time
	Target string // symlinks
}

type KV struct{ Key, Value string }

// File is an opened regular file.
type File struct {
	Name   string
	Size   int64
	Key    key.Key
	Labels []KV          // status line facts: mode/owner, layer, tar entry
	Open   func() *store.Reader
}
```

Every node holds what it needs to list itself (store, keys, parsed
manifest, cached recipe index) and builds its rows' `Child` nodes eagerly;
they are small structs. Listing is a pure function of the store, so a
node can be listed again after an error.

### Node types

| Node | Crumb | Rows |
|---|---|---|
| `repos` | `oci-amber` | one per repository, from one scan of `oci/tag/` and `oci/manifest/` refs grouped with `image.ParseTagRef` and `image.ParseManifestRef`; detail `N tags` (and `M untagged` when there are any) |
| `repo` | repository name | tags in bytewise order, then untagged manifest digests marked `(untagged)`; each row opens the image with `image.Store.Open`; detail `manifest`/`index`, short digest, rootfs status when not `ok`; size is `Meta.Stats.TotalBytes` |
| `imageRoot` | `:tag` or `@sha256:4f7c…` (the short digest), then a `storage` segment | the image root: `manifest`, `meta.json`, `blobs/`, `manifests/` (index only), `rootfs/` (when Meta.Rootfs says a tree exists); a child root reached through `manifests/` has the crumb `@sha256:…` without a `storage` segment |
| `imageBlobs` | `blobs` | the manifest's config then its layers in manifest order (an entry of the amber dir the manifest does not name is appended), each opening the blob root; detail: role (`config`, `layer 3/7`), then from the blob's meta.json `prism gzip go-flate` / `prism none` / `raw not-tar`, entry count, uncompressed size; size is the compressed size |
| `imageManifests` | `manifests` | the index's children in index order, each opening the child's image root; detail: platform, `attestation` when `vnd.docker.reference.type` says so, `manifest`/`index` |
| `blobRoot` | short digest | `meta.json`, `comp.json`, `recipe.json`, `recipe.bin`, `blobs/` for a prism; `meta.json`, `raw` for a raw blob; sizes from the keys; unknown entries listed plainly |
| `prismBlobs` | `blobs` | the numbered blobs in name order; detail is the tar entry name from `recipe.json` (read once per blob root, matched by the entry's `Blob` path); a number the index does not know has no detail |
| `amberDir` | entry name | any other directory of the storage tree (`rootfs/` and everything under it, an unknown directory): name order, ls -l columns, symlinks not followed; symlink and device rows have no `Child` |
| `fsDir` | entry name, the root's crumb is `filesystem` | a rootfs directory through `rootfs.FS`: ls -l columns; a symlink row's `Child` resolves the link with `FS.Stat` when opened: a directory opens as an `fsDir` under the link's own name, a file as the file, anything else or an error is reported in the status line |
| `fsChooser` | `filesystem` | an index's children by platform; Enter opens that child's `fsDir` root (crumb: the platform) or its `fsUnavailable` |
| `fsUnavailable` | `filesystem` | one row, no `Child`: the rootfs status and reason from meta.json |
| `file` | entry name | an `Opener`; `Labels` carry mode and owner for rootfs files, layer digest and tar entry name for prism blobs |

`repos` and `repo` read `meta.json` through `image.Store.Open` once per
row; a repository with hundreds of tags costs hundreds of small reads,
which is fine for a local store.

`image.Store` gains `Manifests(repo) ([]oci.Digest, error)`: every
manifest digest pushed to the repository, sorted. `repo` uses it to find
the untagged ones (those no tag row resolved to).

### Frames and the two stacks

A frame is a node plus its view state: rows (or a load error), cursor,
scroll offset, filter text, and for a viewer the mode and position.

The browser's position is:

```
base   [repos, repo]                      always present, 1 or 2 frames
image  {ctx, active, storage []frame, fs []frame}   nil outside an image
```

- Enter on a row with a `Child` pushes a frame for it onto the active
  stack (or onto `base` when there is no image yet; opening an image row
  creates the image group with `storage = [imageRoot]`).
- Backspace pops the active stack; popping its last frame drops the image
  group and returns to the repository listing. Popping `repo` returns to
  `repos`; Backspace on `repos` does nothing.
- `f` switches the active stack. From storage, the target is the
  filesystem of the innermost `imageRoot` frame on the storage stack: an
  `fsDir` root when that image has a tree, an `fsChooser` for an index,
  `fsUnavailable` otherwise. The fs stack is kept between switches while
  that root does not change and rebuilt when it does. From the fs stack,
  `f` returns to the storage stack exactly as it was.
- Frames keep cursor, scroll and filter when they are returned to.

The breadcrumb is the crumbs of `base` (the `repos` crumb only when it is
the top frame) followed by the crumbs of the active stack, joined with
` › `: `library/app › :v1 › storage › blobs › sha256:4f7c… › blobs`. When
it is wider than the terminal it is truncated from the left with a
leading `…`.

### Loading

Listing and opening run as `tea.Cmd`s. A frame is pushed at once in a
`loading` state (its line reads `loading…`), the command runs `List` or
`Open` on a goroutine, and the result message carries the frame's id so a
result for a frame that was popped meanwhile is dropped. While a frame
loads, every key still works, `q` included. A failed load shows the error
in the status line, pops the frame and leaves the cursor where it was.

### Keys

| Key | Where | Action |
|---|---|---|
| `↑` `k`, `↓` `j` | list, viewer | move one row |
| `pgup` `pgdn` | list, viewer | move one page |
| `g` `G` | list, viewer | first and last row |
| `enter` `→` `l` | list | open the row's `Child` or file |
| `backspace` `←` `h` `esc` | list | back one frame (`h` is hex/text in the viewer, see below) |
| `f` | list, viewer | switch storage ↔ filesystem |
| `/` | list | filter: a text input in the status line; rows whose name or detail contain the text case-insensitively stay; `enter` keeps the filter and returns to the list, `esc` clears it |
| `i` | list | info popup for the row under the cursor; any key closes it |
| `q` `ctrl-c` | everywhere | quit |

Rows are shown in the order the node returns them; the filter never
reorders. The cursor jumps to the first visible row when a filter hides
the row it was on.

### Rendering

`View` is built from pure functions that tests call directly:

- `RenderList(frame, width, height) string`: breadcrumb, a rule, the
  visible rows, a rule, the status line. Row layout is
  `▸ name  detail  size` for storage rows and
  `▸ mode uid:gid size mtime name [-> target]` for rows with `Meta`; the
  name column is as wide as the widest visible name up to a third of the
  width, detail takes the rest, sizes are right-aligned `FormatBytes`.
  Every line is clamped to the width as the import view does.
- The status line shows `N rows` (`N of M` when filtered), the load or
  resolve error if there is one, then the key hints that apply.
- The info popup is a box centred over the list with the row's `Info`
  pairs; `Info` always carries the full name, and for storage rows the
  full digest or key and its type (Blob, FileNode, DirLeaf, DirNode).

Digests are shortened with `tui.ShortDigest` everywhere but the popup.

## The viewer

Enter on a file pushes a viewer frame onto the active stack. Opening
reads the file's size from its key and a probe: the first 8 KiB through a
`store.Reader`.

### Classification

In this order:

1. Size above `MaxTextSize` (8 MiB): **hex** only; `h` shows
   `too large for text` in the status line.
2. The probe contains a NUL byte or is not valid UTF-8 (a rune cut by the
   probe's end is not a fault): **hex**, `h` switches to text.
3. Otherwise **text**. The whole file is read (at most 8 MiB). If
   `json.Valid` accepts it, it is JSON: shown pretty-printed with
   `json.Indent` (two spaces, key order preserved) and `p` toggles to the
   stored bytes. The status label is `json`, else by extension: `yaml`
   (`.yaml`, `.yml`), `shell` (`.sh`, or a first line starting `#!`),
   `toml`, `markdown` (`.md`), otherwise `text`. Labels are cosmetic;
   nothing depends on the extension.

An empty file is text with no lines.

### Text mode

Lines are split on `\n` (a trailing `\r` is dropped), tabs become four
spaces, other control characters and invalid sequences render as `^X` or
`\xNN`. Line numbers on the left, width from the line count. No wrapping:
`←`/`→` scroll horizontally by 8 columns, `home` resets. `g`/`G` top and
end, `pgup`/`pgdn`, `↑`/`↓`. `/` opens a search input: matching lines are
highlighted, `n`/`N` move between hits, `esc` clears. `h` switches to hex
at the byte offset of the top line.

Status line: label, size, line count, the file's `Labels` (mode and owner
of a rootfs file; layer and tar entry of a prism blob), `h hex`, `p raw`
for JSON, `esc back`.

### Hex mode

Rows of 16 bytes: an 8-digit hex offset, two groups of eight hex bytes,
printable ASCII with `.` for the rest. The viewer keeps one window: the
bytes for the rows on screen plus one screen before and after, loaded
with a fresh `store.Reader`, `Skip(offset)` and one `Read` loop. Moving
inside the window redraws; moving outside reloads the window around the
new position. A window load runs as a command like a listing does.

`:` opens an input for an offset (decimal or `0x…`, clamped to the file);
`g`/`G` top and end; `h` switches to text (when allowed) at the line
containing the current offset. Status line: `hex`, size, current offset,
percentage, the file's `Labels`, `: goto`, `h text`, `esc back`.

`RenderHex(offset int64, data []byte, width int) string` and
`RenderText(lines []string, top, left, height, width int, hits []int)
string` are pure functions.

## Errors and limits

- A node that fails to load reports the error in the status line and
  leaves the browser where it was; nothing terminates the program.
- Directory listings are loaded whole; a 200k-entry rootfs directory is a
  few MB and loads in a command with `loading…` shown.
- The filter is a substring match over the loaded rows; it touches
  neither the store nor the order.
- Under 40 columns or 8 rows the view still clamps every line and shows
  what fits; nothing wraps.
- Concurrency: one load command at a time per frame; a frame whose load
  is pending ignores Enter until it finishes.

## Changes outside `browse`

- `store`: `ErrInUse`, mapped from the packstore's flock failure in
  `Open`.
- `blob`: `NewReadOnly(st *store.Store) *Store` and `ErrReadOnly`; `Put`
  and `Delete` refuse on a read-only store, `Open`, `Exists` and the pull
  path work.
- `image`: `Manifests(repo string) ([]oci.Digest, error)`.
- `cmd/oci-amber`: the `browse` subcommand, `browseConfig`,
  `browseConfigFromCLI`, `runBrowse`.
- README: a "Browsing a store" section and the `browse` row wherever the
  commands are listed.

## Testing

- **Fixture.** `browse` tests build a real store in a temp directory with
  `store.Open`, `blob.New` and `image.New` (as `image`'s tests do): a
  two-layer image whose gzipped tar layers hold files, a directory, a
  symlink to a file, a symlink to a directory, an absolute symlink, a
  dangling symlink and a whiteout; an index over two platforms; an image
  with a raw layer (random bytes) and therefore no rootfs; an untagged
  manifest; a repository with a nested name.
- **Nodes.** For every node type, list it and assert names, details,
  sizes and `Child` kinds; `prismBlobs` shows tar entry names; `imageBlobs`
  shows roles in manifest order; `repo` shows the untagged digest; `fsDir`
  follows the symlinks and reports the dangling one.
- **Classification.** JSON, UTF-8 text, text with a cut rune at the probe
  boundary, NUL-bearing binary, invalid UTF-8, exactly 8 MiB and one byte
  more, an empty file, a shebang file.
- **Rendering.** `RenderList`, `RenderText` and `RenderHex` on fixed
  inputs and widths, including truncation and the left-truncated
  breadcrumb.
- **Model.** Drive the model with `tea.KeyMsg` values and by executing the
  returned commands synchronously; assert the frame stacks after
  Enter/Backspace/`f` sequences, that a stale load result is dropped,
  that the filter hides rows and resets the cursor, and that the viewer
  opens in the right mode and switches with `h` and `p`.
- **Command.** `cmd/oci-amber` tests for flag parsing and the startup
  rules: missing store, store in use (open it twice), stdout not a
  terminal, an unknown starting reference.
