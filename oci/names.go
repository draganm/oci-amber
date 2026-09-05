package oci

import (
	"regexp"
	"strings"
)

// MaxRepositoryLength is the maximum length in bytes of a repository name.
const MaxRepositoryLength = 255

var (
	// repositoryRe is the OCI distribution grammar: separators are a dot,
	// one or two underscores, or a run of hyphens, joining [a-z0-9] runs;
	// components joined by '/'.
	repositoryRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	// tagRe is the distribution-spec tag grammar: at most 128 characters.
	tagRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// ValidateRepository checks name against the OCI distribution grammar:
// separators are a dot, one or two underscores, or a run of hyphens, joining
// [a-z0-9] runs, with a MaxRepositoryLength byte limit. A valid name never
// contains ':' or '@', so it can be embedded in amber reference names.
// Failures are *Error values with CodeNameInvalid.
func ValidateRepository(name string) error {
	if name == "" {
		return NewError(CodeNameInvalid, "invalid repository name: empty")
	}
	if len(name) > MaxRepositoryLength {
		return NewError(CodeNameInvalid, "invalid repository name: %d bytes exceeds the %d byte limit", len(name), MaxRepositoryLength)
	}
	if !repositoryRe.MatchString(name) {
		return NewError(CodeNameInvalid, "invalid repository name %q", name)
	}
	return nil
}

// ValidateTag checks tag against [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}. A valid
// tag never contains '/' or ':'. Failures are *Error values with
// CodeManifestInvalid and a message starting with "invalid tag": the
// distribution spec's TAG_INVALID code is legacy and not among the standard
// codes, so a bad tag on a manifest push is reported as MANIFEST_INVALID.
func ValidateTag(tag string) error {
	if !tagRe.MatchString(tag) {
		return NewError(CodeManifestInvalid, "invalid tag %q", tag)
	}
	return nil
}

// SplitReference splits "repo", "repo:tag" or "repo@digest" into the
// repository and the reference: '@' starts a digest, a ':' after the last
// '/' starts a tag. reference is "" for a bare repository. Neither part is
// validated.
func SplitReference(s string) (repo, reference string) {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i], s[i+1:]
	}
	slash := strings.LastIndexByte(s, '/')
	if i := strings.IndexByte(s[slash+1:], ':'); i >= 0 {
		return s[:slash+1+i], s[slash+1+i+1:]
	}
	return s, ""
}
