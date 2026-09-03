package oci

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRepository(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"simple", "app", true},
		{"nested", "library/app", true},
		{"deeply nested", "a/b/c/d", true},
		{"separators", "my-app.v2_x", true},
		{"digits only", "0123", true},
		{"single char", "a", true},
		{"255 bytes", strings.Repeat("a", 255), true},
		{"255 bytes nested", strings.Repeat("a/", 127) + "a", true},
		{"256 bytes", strings.Repeat("a", 256), false},
		{"empty", "", false},
		{"uppercase", "Library/app", false},
		{"uppercase component", "library/App", false},
		{"trailing slash", "library/app/", false},
		{"leading slash", "/library/app", false},
		{"double slash", "library//app", false},
		{"double underscore", "a__b", false},
		{"double hyphen", "a--b", false},
		{"dot hyphen", "a.-b", false},
		{"leading separator", "-app", false},
		{"trailing separator", "app.", false},
		{"colon", "app:v1", false},
		{"at sign", "app@sha256", false},
		{"space", "my app", false},
		{"unicode", "äpp", false},
		{"digest as name", "sha256:" + validHex, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRepository(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateRepository(%q): unexpected error %v", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRepository(%q): want error", tc.in)
			}
			var oe *Error
			if !errors.As(err, &oe) {
				t.Fatalf("ValidateRepository(%q) error %T is not *Error", tc.in, err)
			}
			if oe.Code != CodeNameInvalid {
				t.Fatalf("ValidateRepository(%q) code = %s, want %s", tc.in, oe.Code, CodeNameInvalid)
			}
		})
	}
}

func TestValidateTag(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"latest", "latest", true},
		{"semver", "v1.2.3", true},
		{"numeric", "1.0", true},
		{"leading underscore", "_under", true},
		{"mixed case and separators", "With-Upper.and_dots", true},
		{"single char", "a", true},
		{"128 chars", strings.Repeat("a", 128), true},
		{"129 chars", strings.Repeat("a", 129), false},
		{"empty", "", false},
		{"leading dot", ".hidden", false},
		{"leading hyphen", "-x", false},
		{"slash", "a/b", false},
		{"colon", "a:b", false},
		{"space", "a b", false},
		{"plus", "1+2", false},
		{"at sign", "a@b", false},
		{"unicode", "täg", false},
		{"digest as tag", "sha256:" + validHex, false},
		{"trailing newline", "latest\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTag(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateTag(%q): unexpected error %v", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateTag(%q): want error", tc.in)
			}
			var oe *Error
			if !errors.As(err, &oe) {
				t.Fatalf("ValidateTag(%q) error %T is not *Error", tc.in, err)
			}
			if oe.Code != CodeManifestInvalid {
				t.Fatalf("ValidateTag(%q) code = %s, want %s", tc.in, oe.Code, CodeManifestInvalid)
			}
			if !strings.HasPrefix(oe.Message, "invalid tag") {
				t.Fatalf("ValidateTag(%q) message = %q, want prefix \"invalid tag\"", tc.in, oe.Message)
			}
		})
	}
}
