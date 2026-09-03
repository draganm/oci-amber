package image

import (
	"fmt"
	"slices"
)

// Tags returns the tags of repo in bytewise order, which is the
// "ASCIIbetical" (Go sort.Strings) order the distribution spec asks for.
// A repository's tags are exactly the refs with prefix "oci/tag/<repo>:";
// because repository names cannot contain ':' the prefix cannot match a
// neighbouring repository such as "<repo>2" or "<repo>/sub".
//
// A repository that has manifests but no tags yields an empty, non-nil
// slice (the registry encodes it as "tags": []). A repository with neither
// yields ErrNotFound, which the registry maps to NAME_UNKNOWN.
func (s *Store) Tags(repo string) ([]string, error) {
	refs, err := s.st.ListRefs(TagRef(repo, ""))
	if err != nil {
		return nil, fmt.Errorf("image: listing tags of %s: %w", repo, err)
	}
	tags := make([]string, 0, len(refs))
	for _, r := range refs {
		refRepo, tag, ok := ParseTagRef(r.Name)
		if !ok || refRepo != repo {
			continue
		}
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		known, err := s.repoHasManifests(repo)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, ErrNotFound
		}
	}
	slices.Sort(tags)
	return tags, nil
}

// repoHasManifests reports whether at least one oci/manifest/<repo>/<digest>
// ref exists. The prefix "oci/manifest/<repo>/" is also a prefix of every
// nested repository's manifest refs ("oci/manifest/<repo>/sub/<digest>"), so
// every name is parsed and its repository compared exactly.
func (s *Store) repoHasManifests(repo string) (bool, error) {
	refs, err := s.st.ListRefs(ManifestRef(repo, ""))
	if err != nil {
		return false, fmt.Errorf("image: listing manifests of %s: %w", repo, err)
	}
	for _, r := range refs {
		refRepo, _, ok := ParseManifestRef(r.Name)
		if ok && refRepo == repo {
			return true, nil
		}
	}
	return false, nil
}
