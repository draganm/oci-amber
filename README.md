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
nix develop --command go build -o ./oci-amber ./cmd/oci-amber
./oci-amber serve --store /var/lib/oci-amber
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
| `--work-dir` | `<store>/work` | parent of `<work-dir>/oci-amber/`, where spilled uploads and the comp-prysm spool live; at startup the *contents* of `<work-dir>/oci-amber/uploads` and `<work-dir>/oci-amber/spool` are deleted and nothing else under `--work-dir` is touched | `OCI_AMBER_WORK_DIR` |
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
<store>/oci-amber.json          chunking and segment parameters, written on creation
<store>/packstore               amber pack segments
<store>/refs                    amber references
<store>/gc                      amber collector state
<store>/work/oci-amber/uploads  upload sessions that outgrew --max-in-memory
<store>/work/oci-amber/spool    comp-prysm temporary files
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

Logs are `log/slog` text lines on stderr; `--log-level` selects the
threshold. Two Info lines carry the accounting.

After every blob finalization, one line per blob:

```
time=2026-09-03T18:00:01.412+02:00 level=INFO msg="blob stored" digest=sha256:4f7c… size=31457280 kind=prism format=gzip engine=go-flate entries=1834 logical_bytes=98304211 deduped_bytes=91250688 disk_bytes=2611200 duration=1.842s
time=2026-09-03T18:00:01.420+02:00 level=INFO msg="blob stored" digest=sha256:9b2e… size=1523 kind=raw format=none raw_reason=not-tar logical_bytes=1523 deduped_bytes=0 disk_bytes=1560 duration=3.1ms
```

`kind` is `prism` (file contents plus recipes; `engine` names the compressor
that reproduces the layer and `entries` counts its regular files) or `raw`
(verbatim bytes; `raw_reason` is one of `not-reproducible`, `unsupported`,
`corrupt`, `not-tar`, `analyze-timeout`, `roundtrip-failed`,
`decompose-failed`). An uncompressed tar is a prism with `format=none` and an
empty `engine`: there is no compressor whose output has to be reproduced. A
prism line never carries `raw_reason`; a raw line never carries `engine` or
`entries`. `logical_bytes` is the encoded size of every object offered to the
store, `deduped_bytes` the part that already existed, and `disk_bytes` what
was actually appended to pack segments; the blob root and its `meta.json` are
not counted. A blob that is uploaded again is not re-ingested and counts as
fully deduplicated: instead of a `blob stored` line, a whole-blob dedup hit
(the pushed digest already exists) logs `msg="blob already present"` at Info
with `digest` and `size`, and nothing is written.

After every manifest or index push, one line per image (an identical
re-push logs again, with `disk_bytes=0` and `compression_ratio=+Inf`):

```
time=2026-09-03T18:00:02.007+02:00 level=INFO msg="image pushed" repo=library/app reference=v1 digest=sha256:c81d… kind=manifest blobs=3 manifests=0 total_bytes=95631872 logical_bytes=327545651 deduped_bytes=293700000 deduped_percent=89.7 disk_bytes=10276044 compression_ratio=9.31 duration=18.6ms
time=2026-09-03T18:00:02.311+02:00 level=INFO msg="image pushed" repo=library/app reference=latest digest=sha256:e07a… kind=index blobs=0 manifests=2 total_bytes=191267209 logical_bytes=655091302 deduped_bytes=620841219 deduped_percent=94.8 disk_bytes=10280120 compression_ratio=18.61 duration=4.2ms
```

`total_bytes` is the image size as the manifest describes it (config, layers
and the manifest bytes; for an index, the index bytes plus every child's
total). `blobs` and `manifests` count the descriptors that were resolved. The
byte counters add up the blobs uploaded for this image (blobs that were
already present count as fully deduplicated) and the manifest's own objects.
`compression_ratio` is `total_bytes / disk_bytes` (`+Inf` when nothing new
reached the disk) and `deduped_percent` is
`100 * deduped_bytes / logical_bytes`. The same numbers are stored in the
image's `meta.json`.

Internal failures answer `500` with `{"errors":[]}` and log
`msg="request failed"` at Error level with `method`, `path` and `error`; a
blob upload that fails this way keeps its session so the client can retry
the `PUT`. A pull-side reproduction failure (compressor drift after an
upgrade) is logged at Error level with the digest, and the connection is cut
so the client sees a truncated body rather than wrong bytes.

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
- A layer gzipped at best-speed over content that barely compresses (the
  shape `crane append` and `go-containerregistry`'s tarball writer produce)
  currently falls back to raw with `raw_reason=roundtrip-failed`: bytes are
  never lost (the round-trip check catches it before publishing), but the
  layer keeps its original compressed size on disk instead of decomposing.
  The cause is a comp-prysm zlib level-0 bug, not oci-amber, tar-prism or the
  store — see the comp-prysm issue (`roundtrip-investigation.md` has the
  reproduction).

## Development

```sh
nix develop --command go test ./...              # unit, round-trip and HTTP conformance tests
nix develop --command go vet ./...
nix develop --command go build ./cmd/oci-amber   # the binary, with the cgo engines
```

`registry/e2e_test.go` pushes a two-image index, an artifact and a second
repository through the HTTP API the way docker, podman, containerd and crane
do it (including a `PUT` that fails with `500` and is retried), checks how
every blob was stored, pulls everything back byte for byte and checks the
log lines above. The crane smoke test in `cmd/oci-amber` runs only when
`crane` is on `PATH` (it is, inside the dev shell): it starts the real server
wiring, runs `crane push`, `pull`, `validate`, `append`, `ls`, `catalog` and
`delete` against it, restarts it on the same store and pulls again. Tests
never need the network.

The design document lives in `docs/superpowers/specs/2026-09-03-oci-amber-design.md`.

## License

AGPL-3.0-or-later, like amber-store-core, comp-prysm and tar-prism.
