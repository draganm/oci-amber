package image_test

import (
	"slices"
	"testing"
)

func TestRepositoriesEmpty(t *testing.T) {
	// The environment already holds a blob ref (the config blob) but no
	// image refs; blob refs are global and must not surface as repositories.
	env := newListEnv(t)
	repos, err := env.images.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if repos == nil || len(repos) != 0 {
		t.Fatalf("Repositories = %#v, want an empty non-nil slice", repos)
	}
}

func TestRepositoriesSortedUnique(t *testing.T) {
	env := newListEnv(t)
	env.put("zeta", "v1", env.manifest("", nil, nil))                              // tag only
	env.put("alpha/one/two", "", env.manifest("", nil, nil))                       // digest only, nested name
	env.put("alpha", "v1", env.manifest("", nil, nil))                             // tag and digest
	env.put("alpha", "", env.manifest("", nil, map[string]string{"by": "digest"})) // same repo, by digest
	env.put("beta", "a", env.manifest("", nil, nil))                               // two tags, one repository
	env.put("beta", "b", env.manifest("", nil, map[string]string{"tag": "b"}))

	repos, err := env.images.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	want := []string{"alpha", "alpha/one/two", "beta", "zeta"}
	if !slices.Equal(repos, want) {
		t.Fatalf("Repositories = %q, want %q", repos, want)
	}
}

func TestRepositoriesAfterDelete(t *testing.T) {
	env := newListEnv(t)
	meta, _ := env.put("zeta", "v1", env.manifest("", nil, nil))
	env.put("alpha", "v1", env.manifest("", nil, nil))

	// Removing the tag leaves the manifest ref, so the repository stays.
	if err := env.images.Delete("zeta", "v1"); err != nil {
		t.Fatalf("Delete(zeta:v1): %v", err)
	}
	repos, err := env.images.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if want := []string{"alpha", "zeta"}; !slices.Equal(repos, want) {
		t.Fatalf("after deleting the tag: Repositories = %q, want %q", repos, want)
	}

	// Removing the manifest by digest removes the repository's last ref.
	if err := env.images.Delete("zeta", string(meta.Digest)); err != nil {
		t.Fatalf("Delete(zeta@%s): %v", meta.Digest, err)
	}
	repos, err = env.images.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if want := []string{"alpha"}; !slices.Equal(repos, want) {
		t.Fatalf("after deleting the manifest: Repositories = %q, want %q", repos, want)
	}
}
