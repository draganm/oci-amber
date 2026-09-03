package oci

import "regexp"

// MaxRepositoryLength is the maximum length in bytes of a repository name.
const MaxRepositoryLength = 255

var (
	// repositoryRe is the repository grammar from the design spec:
	// path components of [a-z0-9] runs joined by a single '.', '_' or '-',
	// components joined by '/'.
	repositoryRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	// tagRe is the distribution-spec tag grammar: at most 128 characters.
	tagRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// ValidateRepository checks name against
// [a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)* with a
// MaxRepositoryLength byte limit. A valid name never contains ':' or '@', so
// it can be embedded in amber reference names. Failures are *Error values
// with CodeNameInvalid.
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
