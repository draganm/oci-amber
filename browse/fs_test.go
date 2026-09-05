package browse

import (
	"strings"
	"testing"
)

func TestFSRootFollowsSymlinks(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	root := fsRootFor(f.b, "library/app", im)
	if _, ok := root.(*fsDirNode); !ok {
		t.Fatalf("root is %T, want *fsDirNode", root)
	}
	rows := mustList(t, root)
	assertNames(t, rows, "bin", "etc", "usr")

	bin := childList(t, rows, "bin")
	assertNames(t, bin, "app", "sbin")
	sbin := rowNamed(t, bin, "sbin")
	if sbin.Meta == nil || sbin.Meta.Target != "../usr/bin" {
		t.Fatalf("sbin row = %+v", sbin)
	}
	r, ok := sbin.Child.(Resolver)
	if !ok {
		t.Fatalf("sbin Child is %T, want a Resolver", sbin.Child)
	}
	resolved, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := resolved.(*fsDirNode)
	if !ok || dir.Crumb() != "sbin" {
		t.Fatalf("resolved = %#v", resolved)
	}
	assertNames(t, mustList(t, dir), "tool.sh")

	etc := childList(t, rows, "etc")
	link := rowNamed(t, etc, "link-to-os")
	target, err := link.Child.(Resolver).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.(Opener); !ok {
		t.Fatalf("link-to-os resolves to %T, want an Opener", target)
	}
	if got := string(readAll(t, Row{Name: "link-to-os", Child: target})); !strings.HasPrefix(got, "PRETTY_NAME=") {
		t.Errorf("content through symlink: %q", got)
	}
	abs, err := rowNamed(t, etc, "abs").Child.(Resolver).Resolve()
	if err != nil {
		t.Fatalf("absolute symlink: %v", err)
	}
	if _, ok := abs.(Opener); !ok {
		t.Errorf("abs resolves to %T", abs)
	}
	if _, err := rowNamed(t, etc, "dangling").Child.(Resolver).Resolve(); err == nil || !strings.Contains(err.Error(), "no such path") {
		t.Errorf("dangling symlink: %v", err)
	}

	os := rowNamed(t, etc, "os-release")
	file, err := os.Child.(Opener).Open()
	if err != nil {
		t.Fatal(err)
	}
	if file.Labels[0] != (KV{"mode", "0644"}) || file.Labels[1] != (KV{"owner", "0:0"}) || file.Labels[2].Key != "image" {
		t.Errorf("labels %v", file.Labels)
	}
}

func TestFSRootForIndexIsChooser(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "latest")
	root := fsRootFor(f.b, "library/app", im)
	if _, ok := root.(*fsChooserNode); !ok {
		t.Fatalf("root is %T, want *fsChooserNode", root)
	}
	rows := mustList(t, root)
	assertNames(t, rows, shortRef(f.m1), shortRef(f.m2))
	if rows[1].Detail != "linux/arm64 · manifest" {
		t.Errorf("detail %q", rows[1].Detail)
	}
	arm, ok := rows[1].Child.(*fsDirNode)
	if !ok || arm.Crumb() != "linux/arm64" {
		t.Fatalf("arm64 Child = %#v", rows[1].Child)
	}
	etc := childList(t, mustList(t, arm), "etc")
	rowNamed(t, etc, "arch")
}

func TestFSRootForRawImageExplains(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("tools/rawimg", "r1")
	root := fsRootFor(f.b, "tools/rawimg", im)
	if _, ok := root.(*fsUnavailableNode); !ok {
		t.Fatalf("root is %T, want *fsUnavailableNode", root)
	}
	rows := mustList(t, root)
	if len(rows) != 1 || rows[0].Name != "rootfs unavailable" || !strings.Contains(rows[0].Detail, "stored raw") || rows[0].Child != nil {
		t.Errorf("rows = %+v", rows)
	}
}
