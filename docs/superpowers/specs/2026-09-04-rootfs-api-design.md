# Rootfs API design

The registry serves the root filesystem view that
`docs/superpowers/specs/2026-09-04-rootfs-view-design.md` stores in every
image root: directory listings as JSON, regular files as bytes with range
support, and directories as PAX tars. It is the read side an image browser
and "download part of an image" build on.

Decisions taken during the design review (2026-09-04):

- URL shape `/fs/<repo>:<tag>/<path>` and `/fs/<repo>@<digest>/<path>`:
  the first path segment holding `:` or `@` ends the image reference.
- Symlinks are followed in every component, the last one included.
- An index needs `?platform=` to pick a child manifest.
- Same PR and branch as the rootfs view (`rootfs-view`, PR #3).

## Goals

- Everything a browser needs to walk an image and fetch what it shows,
  with the metadata the tree carries.
- Partial downloads: one file with ranges, or one subtree as a tar, without
  rebuilding a layer.
- The existing error envelope, pagination and streaming conventions of the
  registry; no new dependencies.

## Non-goals

- Writes through the API.
- Extended attributes in listings (they are in the tar).
- Content-type sniffing, compressed tars, authentication.

## URL and reference

`GET` and `HEAD` on `/fs/<reference>/<path>`, where `<reference>` is
`<repo>:<tag>` or `<repo>@<digest>`. The request path after `/fs/` is split
on `/`; the first segment containing `@` (checked first, because a digest
carries a `:`) or `:` ends the reference, the segments after it form the
rootfs path. Repository names cannot contain either character, so the split
is unambiguous. A path with no such segment is not a route (`404`, empty
errors). The repository is validated with the distribution grammar
(`NAME_INVALID`); the reference is resolved like a manifest reference
(`MANIFEST_UNKNOWN`, `DIGEST_INVALID`).

The rootfs path is cleaned with URL semantics: `path.Clean("/" + p)`, so
`..` never leaves the root, repeated and trailing slashes vanish, and the
empty path or `/` is the root.

Other methods answer `405` with `Allow: GET, HEAD`.

### Indexes

When the reference resolves to an index, `?platform=<os>/<arch>[/<variant>]`
selects the child manifest whose descriptor's `platform` has that os and
architecture and, when a variant is given, that variant; the first match
wins. Without the parameter, with a malformed one, with no match, or when
the chosen child is itself an index, the answer is `400 PLATFORM_UNKNOWN`
whose detail is `{"platforms": ["linux/amd64", "linux/arm64/v8"]}` listing
the index's children that carry a platform. `oci.Descriptor` gains
`Platform` (`os`, `architecture`, `variant`), which the parser used to drop.

### Rootfs availability

An image whose meta has no rootfs tree (status `unavailable`,
`not-applicable`, or a root stored before views existed) answers
`404 ROOTFS_UNAVAILABLE` with detail `{"status": "...", "reason": "..."}`
(`status` is `absent` for an old root).

## Path resolution

Every component is resolved like a kernel resolves it, scoped to the
rootfs: a symlink component is replaced by its target (an absolute target
restarts at the rootfs root, `..` pops and never rises above the root), the
last component included; more than 40 links in one resolution is a loop. A
component that is not a directory with components left after it is "not a
directory". Resolution costs one directory lookup per component.

Errors: `404 PATH_UNKNOWN` for a missing component or a non-directory in
the middle of the path; `400 PATH_INVALID` for a symlink loop.

## Responses

| Resolved entry | Query | Answer |
|---|---|---|
| directory | none or `format=json` | `200 application/json` listing (below) |
| directory | `format=tar` | `200 application/x-tar`, streamed |
| regular file | none or `format=json` | `200 application/octet-stream` bytes |
| regular file | `format=tar` | `400 PATH_INVALID` |
| symlink, device, fifo, socket | none or `format=json` | `200 application/json`, the entry object alone |
| symlink, device, fifo, socket | `format=tar` | `400 PATH_INVALID` |
| any | other `format` | `400 PATH_INVALID` |

A symlink is only reached as the final entry when it cannot be followed;
since resolution follows every symlink, that never happens, and the
"entry object alone" row applies to devices, fifos and sockets.

`HEAD` sends the same headers as `GET` without a body; a tar `HEAD` has
no `Content-Length`.

### Listing

```json
{
  "path": "etc",
  "entries": [
    {"name": "passwd", "type": "file", "mode": "0644", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "size": 1234},
    {"name": "rc.d", "type": "dir", "mode": "0755", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z"},
    {"name": "mtab", "type": "symlink", "mode": "0777", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "target": "/proc/mounts"},
    {"name": "null", "type": "char", "mode": "0666", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "major": 1, "minor": 3}
  ]
}
```

- `path` is the cleaned request path (`""` for the root).
- `type` is `file`, `dir`, `symlink`, `char`, `block`, `fifo` or `socket`.
- `mode` is the permission, setuid, setgid and sticky bits as four octal
  digits; `uid`, `gid` are numbers; `mtime` is RFC 3339 in UTC with
  nanoseconds when present.
- `size` appears on files only (the content length), `target` on symlinks,
  `major` and `minor` on devices.
- Entries are in bytewise name order. `n` and `last` paginate exactly like
  `tags/list`: `n` is the page size (`0` gives an empty page), `last` the
  name to start after, and a `Link: <...>; rel="next"` header carries the
  next page's URL, keeping `platform` and `format` query parameters.
  Without `n` the whole directory is returned.

### File

`Content-Type: application/octet-stream`, `Content-Length`, `Accept-Ranges:
bytes`, `ETag: "<content key>"`. `If-None-Match` matching the ETag answers
`304` with the ETag. A single `Range: bytes=a-b` is honoured with `206` and
`Content-Range`; the reader skips whole chunks before `a`. An unsatisfiable
range is `416` with `Content-Range: bytes */<size>`; multiple ranges get
the full body. Bytes are not digest-checked on the way out: the store's
keys are content addressed and its reader checks every chunk's length.

### Tar

The subtree under the directory as a PAX tar written by amber's
`tarexport`: full metadata, xattrs, devices, fifos; sockets are skipped;
names are relative to the directory (`etc/passwd` inside `GET .../etc?format=tar`
is `passwd`), the directory itself is not an entry, like `tar -C dir .`.
The root works: `GET /fs/app:v1/?format=tar` is the whole rootfs. The
response streams (no `Content-Length`); a failure after the first byte
aborts the connection, like a blob pull.

## Error codes

Four oci-amber codes join the standard list in `oci`, rendered in the same
envelope:

| Code | Status | When |
|---|---|---|
| `ROOTFS_UNAVAILABLE` | 404 | the image has no rootfs tree |
| `PATH_UNKNOWN` | 404 | a path component is missing or a file sits in the middle of the path |
| `PATH_INVALID` | 400 | symlink loop, `format=tar` on a non-directory, unknown `format` |
| `PLATFORM_UNKNOWN` | 400 | an index without a usable `platform` |

## Package changes

- **`oci`**: `Platform{OS, Architecture, Variant}` with `String()` and
  `ParsePlatform`; `Descriptor.Platform *Platform`; the four codes and
  their statuses.
- **`rootfs`**: `FS` over a stored tree: `NewFS(st *store.Store, root key.Key)`,
  `Stat(path) (Entry, error)`, `List(path, after string, limit int) ([]Entry, bool, error)`,
  `Open(path) (Entry, *store.Reader, error)`, `WriteTar(w io.Writer, path string) error`;
  `Entry{Name, Mode, UID, GID, Mtime, Size, Target, Rdev, Content}` with
  `Type()`, `IsDir()`, `IsRegular()`; errors `ErrNotFound`, `ErrNotDir`,
  `ErrNotFile`, `ErrLoop`. Resolution mirrors the in-memory resolver over
  `store.Lookup`.
- **`image`**: `Image.FS() (*rootfs.FS, bool)`; the registry does not
  import `store`.
- **`registry`**: `fs.go` with the `/fs/` route, reference parsing,
  platform selection, and the listing, file, tar and HEAD handlers reusing
  `parseRange`, `pageSize`, `bodyWriter`, `handleError` and
  `isClientGone`.
- README: a "Rootfs API" section with the table above and examples.

## Testing

- `rootfs.FS` unit tests over a tree built by `Builder` from a crafted tar:
  Stat through relative and absolute symlinks and `..`, loop, missing,
  file in the middle; List pages and `after`; Open with `Skip`; WriteTar
  compared entry by entry with `archive/tar`.
- `registry` unit tests for reference parsing and platform matching.
- The end-to-end test gains a phase over the pushed fixtures: root and
  `etc` listings with pagination, the big file with and without a range,
  `ETag` and `304`, `bin/app-link` followed to the binary, `etc` and the
  root as tars read back with `archive/tar`, the index with and without
  `platform`, an artifact answering `ROOTFS_UNAVAILABLE`, `PATH_UNKNOWN`,
  `format=tar` on a file, `HEAD`, `405`, an invalid name.
