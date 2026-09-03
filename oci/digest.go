// Package oci holds the OCI distribution grammar and wire types shared by
// every other oci-amber package: content digests, repository names and tags,
// the error envelope, media types and the manifest/descriptor shapes. It has
// no dependencies outside the standard library.
package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// AlgorithmSHA256 is the only digest algorithm oci-amber accepts.
const AlgorithmSHA256 = "sha256"

// Digest is a validated OCI content digest: "sha256:" followed by exactly 64
// lowercase hexadecimal characters. Values produced by ParseDigest,
// DigestOfBytes and DigestFromSum are always well formed; a Digest made by a
// plain string conversion is not validated.
type Digest string

// sha256HexRe is the encoded part of a sha256 digest (image-spec: [a-f0-9]{64}).
var sha256HexRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// digestShapeRe is the image-spec digest grammar for any algorithm
// (algorithm ":" encoded). It is only used by IsDigest to tell digests from
// tags when routing a manifest reference.
var digestShapeRe = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// ParseDigest validates s as a sha256 digest. Anything else, including
// another algorithm (sha512, blake3), uppercase hex, or a wrong length, is
// rejected with an *Error carrying CodeDigestInvalid.
func ParseDigest(s string) (Digest, error) {
	algo, enc, ok := strings.Cut(s, ":")
	if !ok || algo == "" || enc == "" {
		return "", NewError(CodeDigestInvalid, "invalid digest %q: expected <algorithm>:<encoded>", s)
	}
	if algo != AlgorithmSHA256 {
		return "", NewError(CodeDigestInvalid, "invalid digest %q: unsupported algorithm %q, only sha256 is accepted", s, algo)
	}
	if !sha256HexRe.MatchString(enc) {
		return "", NewError(CodeDigestInvalid, "invalid digest %q: sha256 digests are 64 lowercase hex characters", s)
	}
	return Digest(s), nil
}

// DigestOfBytes returns the sha256 digest of b.
func DigestOfBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return DigestFromSum(sum[:])
}

// DigestFromSum wraps a raw 32-byte sha256 sum (as returned by
// hash.Hash.Sum) as a Digest. Any other length is a programming error and
// panics.
func DigestFromSum(sum []byte) Digest {
	if len(sum) != sha256.Size {
		panic(fmt.Sprintf("oci: DigestFromSum: want %d bytes, got %d", sha256.Size, len(sum)))
	}
	return Digest(AlgorithmSHA256 + ":" + hex.EncodeToString(sum))
}

// String returns the digest as "<algorithm>:<hex>".
func (d Digest) String() string { return string(d) }

// Algorithm returns the part before the colon ("sha256" for every valid Digest).
func (d Digest) Algorithm() string {
	algo, _, _ := strings.Cut(string(d), ":")
	return algo
}

// Hex returns the encoded part after the colon.
func (d Digest) Hex() string {
	_, enc, _ := strings.Cut(string(d), ":")
	return enc
}

// IsDigest reports whether reference has the shape of a digest
// (<algorithm>:<encoded> in the image-spec grammar, any algorithm) rather
// than a tag. It is a routing check only: a reference that IsDigest must
// still pass ParseDigest before it is used, so "sha512:…" routes to the
// by-digest path and then fails with DIGEST_INVALID. Tags can never contain
// a colon, so the two shapes do not overlap.
func IsDigest(reference string) bool {
	return digestShapeRe.MatchString(reference)
}
