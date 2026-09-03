# oci-amber

oci-amber is an OCI distribution registry whose storage is an embedded
[amber-store-core](https://github.com/jobs-build/amber-store-core) store. It
speaks the standard `/v2/` API, so docker, containerd/nerdctl, podman/skopeo,
crane, oras and buildkit push and pull against it without any client-side
configuration beyond the registry URL.

Instead of keeping each layer as an opaque compressed blob, oci-amber takes
layers apart on push: [comp-prysm](https://github.com/draganm/comp-prysm)
turns the compressed stream into the uncompressed tar plus a compression
recipe, [tar-prism](https://github.com/draganm/tar-prism) turns the tar into
per-file contents plus a tar recipe, and the file contents land in amber's
content-defined-chunked store where they deduplicate across layers and
images. Pulls rebuild the original bytes on the fly (tar-prism compose,
comp-prysm recompress) and every served blob is byte-identical to what was
pushed; the sha256 is verified on the way out and a mismatch cuts the
connection rather than serving wrong bytes.

Layers whose compression cannot be reproduced exactly (a compressor
comp-prysm does not know, a corrupt stream, a blob that is not a tar) are
stored verbatim and served with range support.

## Requirements

Everything is provided by the Nix flake: Go 1.26, `pkg-config`, `zlib` and
`zstd` (comp-prysm's cgo engines), `gzip` and `pigz` (fixtures), and `crane`
(client smoke test).

```sh
nix develop            # or `direnv allow` once
```

## Running

```sh
# straight from the dev shell
nix develop --command go run ./cmd/oci-amber serve --store /tmp/oci-amber --listen 127.0.0.1:5000

# or build the binary first
nix develop --command go build -o /usr/local/bin/oci-amber ./cmd/oci-amber
oci-amber serve --store /var/lib/oci-amber
```

Then push and pull as usual:

```sh
crane push image.tar 127.0.0.1:5000/library/app:v1
crane pull 127.0.0.1:5000/library/app:v1 pulled.tar
crane ls 127.0.0.1:5000/library/app

docker tag alpine:3.20 127.0.0.1:5000/alpine:3.20
docker push 127.0.0.1:5000/alpine:3.20
```

The registry is single tenant, unauthenticated and speaks plain HTTP; put it
behind a TLS terminator or use it on a trusted network.

`SIGINT`/`SIGTERM` stop the listener, wait up to 30 s for in-flight requests,
close the upload sessions and the store, and exit 0.

## Configuration

`oci-amber serve` flags. Every flag can also be set through the environment
variable in the last column; a flag on the command line wins.

| Flag | Default | Meaning | Environment |
|---|---|---|---|
| `--store` | required | store directory (created on first start) | `OCI_AMBER_STORE` |
| `--work-dir` | `<store>/work` | spilled uploads and comp-prysm spool; emptied at startup | `OCI_AMBER_WORK_DIR` |
| `--listen` | `:5000` | listen address | `OCI_AMBER_LISTEN` |
| `--max-in-memory` | `64MiB` | upload spool and comp-prysm spool threshold before spilling to `--work-dir`; units `B`, `KiB`, `MiB`, `GiB`, `KB`, `MB`, `GB` | `OCI_AMBER_MAX_IN_MEMORY` |
| `--analyze-parallelism` | `2` | comp-prysm candidate workers per blob (each holds one engine working set) | `OCI_AMBER_ANALYZE_PARALLELISM` |
| `--analyze-timeout` | `15m` | per-blob analyze deadline; on expiry the blob is stored raw | `OCI_AMBER_ANALYZE_TIMEOUT` |
| `--max-concurrent-finalize` | `NumCPU/2` (min 1) | concurrent blob finalizations | `OCI_AMBER_MAX_CONCURRENT_FINALIZE` |
| `--verify-roundtrip` | `true` | run the full pull pipeline over every prism before publishing it; a mismatch downgrades the blob to raw | `OCI_AMBER_VERIFY_ROUNDTRIP` |
| `--upload-timeout` | `1h` | idle upload session expiry and recent-uploads table TTL | `OCI_AMBER_UPLOAD_TIMEOUT` |
| `--gc-interval` | `0` | background GC cycle interval; `0` disables | `OCI_AMBER_GC_INTERVAL` |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` | `OCI_AMBER_LOG_LEVEL` |

## Storage layout

```
<store>/oci-amber.json   chunking and segment parameters, written on creation
<store>/packstore        amber pack segments
<store>/refs             amber references
<store>/gc               amber collector state
<store>/work/uploads     upload sessions that outgrew --max-in-memory
<store>/work/spool       comp-prysm temporary files
```

`oci-amber.json` pins the content-defined chunking (min 32 KiB, normal
512 KiB, max 1 MiB) and the 2 GiB pack segment size. A store created with
different parameters is refused, because changing chunk boundaries silently
defeats deduplication against existing content.

Every OCI object is an amber tree named by a reference, so amber's own tools
can inspect, export and garbage-collect them:

```
oci/blob/<algo>:<hex>                                   blob root
oci/manifest/<repo>/<algo>:<hex>                        image root
oci/tag/<repo>:<tag>                                    image root
oci/referrer/<repo>/<subject digest>/<referrer digest>  referrer's image root
```

A blob root is a tar-prism prism directory (`recipe.bin`, `recipe.json`,
`blobs/00000001…`) plus `comp.json` (comp-prysm parameters) and `meta.json`;
raw blobs hold the verbatim bytes in `raw` instead. An image root holds the
exact manifest bytes in `manifest`, `meta.json`, and `blobs/` (and
`manifests/` for an index) whose entries point at the referenced roots, so an
image is one reachable tree.

## HTTP surface

The full distribution API: blob HEAD/GET/DELETE with single-range requests
on raw blobs, chunked (`POST`/`PATCH`/`PUT`) and monolithic uploads, cross
repository mounts, manifests and indexes by tag and digest, `tags/list` and
`_catalog` with `n`/`last` pagination and `Link` headers, the referrers API
(`OCI-Subject` is returned on push so clients do not fall back to tag
schemas), and the standard error envelope
`{"errors":[{"code":…,"message":…,"detail":…}]}`.

Manifests are capped at 4 MiB (`413`). Blob uploads are not size limited.
Range requests on prism-stored layers are answered with the full body.

## Logging

Logs are `log/slog` text lines on stderr. Two lines matter operationally.

After every blob finalization (Info):

```
blob stored digest=sha256:… size=… kind=prism format=gzip engine=gnu-gzip entries=… logical_bytes=… deduped_bytes=… disk_bytes=… duration=…
```

`kind` is `prism` or `raw` (with `raw_reason`), `logical_bytes` is the size
of every object offered to the store, `deduped_bytes` the part that already
existed, `disk_bytes` what was actually appended to pack segments.

After every manifest push (Info):

```
image pushed repo=library/app reference=v1 digest=sha256:… kind=manifest blobs=7 manifests=0 total_bytes=95631872 logical_bytes=327545651 deduped_bytes=293700000 deduped_percent=89.7 disk_bytes=10276044 compression_ratio=9.31 duration=…
```

`total_bytes` is the image size as the manifest describes it,
`compression_ratio` is `total_bytes / disk_bytes` and `deduped_percent` is
`100 * deduped_bytes / logical_bytes`. The same numbers are stored in the
image's `meta.json`. A pull-side reproduction failure (compressor drift after
an upgrade) is logged at Error level with the digest.

## Garbage collection

Deleting a tag, manifest or blob removes references; the objects become
unreachable and are reclaimed by amber's collector once their pack is old
enough. `--gc-interval` runs the collector in-process; with it off, run the
amber CLI's `gc run` on the store while the server is stopped. Blob
references are never dropped automatically: `DELETE /v2/<name>/blobs/<digest>`
removes one.

## Limitations

- No authentication or authorization; blobs are global, not repository
  scoped.
- One process owns the store; nothing else may open it while the server
  runs.
- Upload sessions do not survive a restart; clients restart the blob on
  `BLOB_UPLOAD_UNKNOWN`.
- Only `sha256` digests.

## Development

```sh
nix develop --command go test ./...              # unit, round-trip and HTTP conformance tests
nix develop --command go vet ./...
nix develop --command go build ./cmd/oci-amber   # the binary, with the cgo engines
```

The crane smoke test in `cmd/oci-amber` runs only when `crane` is on `PATH`
(it is, inside the dev shell). Tests never need the network.

The design document lives in `docs/superpowers/specs/2026-09-03-oci-amber-design.md`.

## License

AGPL-3.0-or-later, like amber-store-core, comp-prysm and tar-prism.
