# Rootfs API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve the stored rootfs of an image over HTTP: directory listings as JSON, files as bytes with ranges, directories as PAX tars, under `/fs/<repo>:<tag>/<path>` and `/fs/<repo>@<digest>/<path>`.

**Architecture:** `rootfs.FS` reads a stored tree with kernel-like path resolution over `store.Lookup`; `image.Image.FS()` hands it out; `registry/fs.go` routes `/fs/`, resolves indexes by `?platform=`, and answers listings, files and tars with the registry's existing envelope, pagination, range and streaming helpers. `oci` gains `Descriptor.Platform` and four extension error codes.

**Tech Stack:** Go 1.26.6 via the Nix flake, `net/http`, `archive/tar`, amber `tarexport`.

**Spec:** docs/superpowers/specs/2026-09-04-rootfs-api-design.md

## Global Constraints

- Same branch and PR as the rootfs view (`rootfs-view`, PR #3). Run every go command as `nix develop --command go ...`.
- Flat packages; `registry` must not import `store` (it reaches trees through `image` and `rootfs`).
- URL shape: the first segment after `/fs/` that holds `@` (checked first) or `:` ends the reference. Rootfs paths are cleaned with `path.Clean("/" + p)`.
- Symlinks followed in every component with a 40-hop bound; `..` never leaves the rootfs.
- Error codes: `ROOTFS_UNAVAILABLE` 404, `PATH_UNKNOWN` 404, `PATH_INVALID` 400, `PLATFORM_UNKNOWN` 400.
- Commit after every task with the trailer lines:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
  ```

---

### Task 1: oci: platforms and extension error codes

**Files:** Modify `oci/manifest.go`, `oci/errors.go`; test `oci/manifest_test.go`, `oci/errors_test.go`.

- [ ] Add to `oci/manifest.go`:

```go
// Platform is the platform of an index child: the fields the rootfs API
// selects a child manifest by.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// String renders os/architecture[/variant].
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ParsePlatform parses os/architecture[/variant]; no part may be empty.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Platform{}, fmt.Errorf("platform %q is not os/architecture[/variant]", s)
	}
	for _, part := range parts {
		if part == "" {
			return Platform{}, fmt.Errorf("platform %q is not os/architecture[/variant]", s)
		}
	}
	p := Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

// Matches reports whether c satisfies the request p: the same os and
// architecture, and the same variant when p names one.
func (p Platform) Matches(c Platform) bool {
	return p.OS == c.OS && p.Architecture == c.Architecture && (p.Variant == "" || p.Variant == c.Variant)
}
```

  and `Platform *Platform `json:"platform,omitempty"`` to `Descriptor` (update its doc: platform is kept for index children; urls and data are still ignored). In `oci/errors.go` add the four codes with a comment "oci-amber extensions for the rootfs API (/fs/), not in the distribution spec's list" and map `CodeRootfsUnavailable, CodePathUnknown` to 404 and `CodePathInvalid, CodePlatformUnknown` to 400 in `DefaultStatus`.
- [ ] Tests: `TestParsePlatform` (valid with and without variant, empty part, one part, four parts), `TestPlatformMatches` (variant wildcard), `TestParseManifestKeepsPlatform` (an index body with platform decodes it), `TestExtensionCodeStatuses`.
- [ ] `nix develop --command go test ./oci/` then commit `oci: descriptor platforms and rootfs API error codes`.

### Task 2: rootfs: FS over a stored tree

**Files:** Create `rootfs/fs.go`; test `rootfs/fs_test.go`.

- [ ] Implement `rootfs/fs.go` exactly as the spec's package section says: `Entry` with `Type`, `IsDir`, `IsRegular`, `TypeName` (`file`, `dir`, `symlink`, `char`, `block`, `fifo`, `socket`, else `unknown`); `Clean`; `FS` with `NewFS`, `Root`, `Stat`, `List`, `Open`, `Content(e Entry) (*store.Reader, error)`, `WriteTar`; `resolve` mirroring `tree.resolveDir` over `store.Lookup` with a stack of directory entries, the synthetic root entry `Entry{Mode: TypeDir|0o755, Content: root}`; `fromFstree`. Errors `ErrNotFound`, `ErrNotDir`, `ErrNotFile`, `ErrLoop`, each wrapped with the path.
- [ ] Tests build one tree with `Builder` from a tar (helpers from `layer_test.go`/`builder_test.go`): `usr/bin/ls` (2.5 MiB), `bin -> usr/bin`, `sbin -> /usr/bin`, `up -> ../../usr/bin`, `loop1 <-> loop2`, `etc/passwd`, `etc/rc.d/`, `etc/mtab -> /proc/mounts`, `dev/null` char 1:3, `many/f000..f299`. Cases: root Stat; `bin/ls`, `sbin/ls`, `up/ls` resolve to the file with its size; `bin` is the directory named `bin`; `loop1` is `ErrLoop`; `nope` and `etc/mtab` are `ErrNotFound`; `etc/passwd/x` is `ErrNotDir`; `../../etc` is `etc`; `dev/null` has rdev; `List("many", "", 100)` pages three times ending without more; `List("", "", 0)` returns every root entry sorted; `List("etc/passwd", ...)` is `ErrNotDir`; `Open("usr/bin/ls")` then `Skip(1_500_000)` reads the matching tail; `WriteTar("etc")` yields `passwd`, `rc.d/`, `mtab` relative; `WriteTar("")` includes `usr/bin/ls`; `WriteTar("etc/passwd")` is `ErrNotDir`; `Clean` table.
- [ ] `nix develop --command go test ./rootfs/` then commit `rootfs: read a stored tree with symlink-aware paths`.

### Task 3: image: hand out the FS

**Files:** Modify `image/store.go`; test `image/rootfs_test.go`.

- [ ] Add `func (im *Image) FS() (*rootfs.FS, bool)` returning `rootfs.NewFS(im.st, im.rootfs), true` when `hasRootfs`.
- [ ] Extend `TestPutRootfsOK`: `im.FS()` lists the root with the expected names; `TestPutRootfsUnavailable`: `FS()` reports false.
- [ ] Test, commit `image: expose the rootfs as a filesystem`.

### Task 4: registry: the /fs/ route

**Files:** Create `registry/fs.go`; modify `registry/server.go` (`ServeHTTP` dispatches `/fs/` before the `/v2/` checks); tests `registry/fs_test.go` (unit: `parseFSPath`, `matchesETag`, platform selection over a fake index), `registry/e2e_test.go` (phase `fsAPI`).

- [ ] Implement `registry/fs.go` per the spec's responses table: `parseFSPath`, `handleFS`, `openRootfsImage` (index resolution by `platform`, detail `{"platforms": [...]}`, never nil), `rootfsUnavailable` (detail `{"status", "reason"}`, `absent` for an old root), `fsError` (not found / not dir → `PATH_UNKNOWN`; loop → `PATH_INVALID`; else `handleError`), `serveListing` (`pageSize`, `last`, `Link` keeping the query), `fsEntryJSON`, `serveFile` (`ETag`, `If-None-Match` → 304, `Accept-Ranges`, single `Range` → 206 via `parseRange` and `Reader.Skip`, 416, `HEAD`, `bodyWriter` error discipline like `serveBlob`), `serveTar` (`application/x-tar`, streamed, `HEAD` sends headers only).
- [ ] e2e phase after `checkRootfs`: root listing has `bin`, `etc`, `lib`, `share` and no `var`; `etc` listing paginated with `n=2` follows `Link` to the end and matches the unpaginated listing; `bin/app` bytes equal `fx.big`, `Range: bytes=1048576-1048600` returns 206 with the slice, `If-None-Match` with the returned `ETag` is 304; `bin/app-link` serves the same bytes; `lib/libfoo.so.1` equals `fx.lib`; `etc?format=tar` read back with `archive/tar` has exactly `config.yaml`, `hostname`, `hosts`, `os-release` with the fixture bytes; `?format=tar` on the root contains `bin/app` with `fx.big`; the index by tag `latest` without platform is `400 PLATFORM_UNKNOWN` with both platforms in detail, with `platform=linux/arm64` serves `etc/extra.conf` (`extra = true\n`), with `platform=linux/s390x` is 400; the artifact by digest is `404 ROOTFS_UNAVAILABLE` with status `not-applicable`; `nope/x` is `404 PATH_UNKNOWN`; `etc/hosts?format=tar` is `400 PATH_INVALID`; `?format=zip` is `400 PATH_INVALID`; `HEAD` on `bin/app` has `Content-Length` and no body; `POST` is 405 with `Allow: GET, HEAD`; `/fs/Bad_Name:v1/` is `400 NAME_INVALID`; `/fs/library/app/etc` (no separator) is 404 with empty errors; an unknown tag is `404 MANIFEST_UNKNOWN`.
- [ ] `nix develop --command go test ./registry/` then commit `registry: serve the rootfs over /fs/`.

### Task 5: docs

- [ ] README: a "Rootfs API" section after "HTTP surface" with the URL shape, the responses table, the listing JSON, pagination, ranges and ETag, tar semantics, index platforms, error codes, curl examples; drop the "Nothing serves rootfs/ over HTTP yet" limitation; followups updated.
- [ ] `nix develop --command go vet ./... && nix develop --command go test -race ./...`, commit `docs: describe the rootfs API`, push.
