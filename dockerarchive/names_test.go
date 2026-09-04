package dockerarchive

import "testing"

func TestNameFromRepoTag(t *testing.T) {
	cases := []struct {
		in      string
		want    Name
		ok      bool
		wantErr bool
	}{
		{"busybox:1.37", Name{"busybox", "1.37"}, true, false},
		{"library/busybox:1.37", Name{"library/busybox", "1.37"}, true, false},
		{"registry.example.ch/team/app:v1", Name{"team/app", "v1"}, true, false},
		{"localhost:5000/app:v1", Name{"app", "v1"}, true, false},
		{"localhost/app:v1", Name{"app", "v1"}, true, false},
		{"app", Name{"app", "latest"}, true, false},
		{"app@sha256:0000000000000000000000000000000000000000000000000000000000000000", Name{}, false, false},
		{"registry.example.ch/app@sha256:0000000000000000000000000000000000000000000000000000000000000000", Name{}, false, false},
		{"Bad Repo:v1", Name{}, false, true},
		{"app:bad tag", Name{}, false, true},
	}
	for _, c := range cases {
		got, ok, err := nameFromRepoTag(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v", c.in, err)
			continue
		}
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseName(t *testing.T) {
	good := map[string]Name{
		"app:v1":                          {"app", "v1"},
		"team/app:v1":                     {"team/app", "v1"},
		"registry.example.ch/team/app:v1": {"registry.example.ch/team/app", "v1"},
		"app":                             {"app", "latest"},
	}
	for in, want := range good {
		got, err := ParseName(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", ":v1", "app:", "app@sha256:abc", "UPPER:v1"} {
		if _, err := ParseName(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}
