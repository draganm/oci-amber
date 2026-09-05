package browse

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive runs cmd and feeds every message it yields back into m until
// nothing is left, so a test sees the state after every load finished.
func drive(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

var keyTypes = map[string]tea.KeyType{
	"enter": tea.KeyEnter, "backspace": tea.KeyBackspace, "esc": tea.KeyEsc,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
}

// press sends keys one after another; a name in keyTypes is that key,
// anything else is typed as runes.
func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		if typ, ok := keyTypes[k]; ok {
			msg = tea.KeyMsg{Type: typ}
		}
		_, cmd := m.Update(msg)
		drive(t, m, cmd)
	}
}

func newTestModel(t *testing.T, f *fixture, start string) *model {
	t.Helper()
	m, err := newModel(f.b, start)
	if err != nil {
		t.Fatalf("newModel(%q): %v", start, err)
	}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drive(t, m, cmd)
	drive(t, m, m.Init())
	return m
}

func visibleNames(f *frame) []string {
	names := make([]string, len(f.visible))
	for i, idx := range f.visible {
		names[i] = f.rows[idx].Name
	}
	return names
}

func assertTop(t *testing.T, m *model, want ...string) {
	t.Helper()
	if got := visibleNames(m.top()); !slices.Equal(got, want) {
		t.Fatalf("top rows %v, want %v", got, want)
	}
}

func assertCrumbs(t *testing.T, m *model, want ...string) {
	t.Helper()
	if got := m.crumbs(); !slices.Equal(got, want) {
		t.Fatalf("crumbs %v, want %v", got, want)
	}
}

func TestModelStartsAtRepositories(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	assertTop(t, m, "library/app", "library/app/sub", "tools/rawimg")
	assertCrumbs(t, m, "oci-amber")
	out := m.View()
	if !strings.Contains(out, "oci-amber") || !strings.Contains(out, "3 rows") || !strings.Contains(out, "▸ library/app/") {
		t.Errorf("view:\n%s", out)
	}
	if lines := strings.Split(out, "\n"); len(lines) != 30 {
		t.Errorf("%d lines, want 30", len(lines))
	}
}

func TestEnterAndBackspaceWalkTheTree(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app")
	if names := visibleNames(m.top()); names[0] != "latest" || names[1] != "v1" {
		t.Fatalf("repo rows %v", names)
	}
	press(t, m, "down", "enter")
	if m.img == nil || len(m.img.storage) != 1 {
		t.Fatal("opening a tag must start the image group")
	}
	assertCrumbs(t, m, "library/app", ":v1", "storage")
	assertTop(t, m, "blobs", "manifest", "meta.json", "rootfs")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	assertTop(t, m, shortRef(f.conf), shortRef(f.layerA), shortRef(f.layerB))
	press(t, m, "down", "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs", shortRef(f.layerA))
	assertTop(t, m, "blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs", shortRef(f.layerA), "blobs")
	if rows := m.top().rows; len(rows) != 5 || rows[0].Detail == "" {
		t.Errorf("prism blobs rows %+v", rows)
	}
	press(t, m, "backspace", "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	if m.top().cursor != 1 {
		t.Errorf("cursor %d after returning, want 1", m.top().cursor)
	}
	press(t, m, "backspace", "backspace")
	if m.img != nil {
		t.Fatal("leaving the storage root must drop the image group")
	}
	assertCrumbs(t, m, "library/app")
	press(t, m, "backspace")
	assertCrumbs(t, m, "oci-amber")
	press(t, m, "backspace")
	assertCrumbs(t, m, "oci-amber")
}

func TestToggleFilesystemKeepsPosition(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	assertCrumbs(t, m, "library/app", ":v1", "storage")
	press(t, m, "enter") // blobs
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem")
	assertTop(t, m, "bin", "etc", "usr")
	press(t, m, "down", "enter")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem")
	press(t, m, "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	if !strings.Contains(m.View(), "f filesystem") {
		t.Error("hints must offer the filesystem")
	}
}

func TestFilterHidesRows(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter") // etc
	assertTop(t, m, "abs", "config.json", "dangling", "hostname", "link-to-os", "os-release")
	press(t, m, "/", "os", "enter")
	assertTop(t, m, "hostname", "link-to-os", "os-release")
	if m.top().filter != "os" || !strings.Contains(m.View(), "3 of 6 rows") {
		t.Errorf("filter %q view:\n%s", m.top().filter, m.View())
	}
	press(t, m, "G")
	if m.top().currentRow().Name != "os-release" {
		t.Errorf("G under a filter lands on %q", m.top().currentRow().Name)
	}
	press(t, m, "esc")
	if m.top().filter != "" || len(m.top().visible) != 6 {
		t.Error("esc must clear the filter")
	}
	if m.top().currentRow().Name != "os-release" {
		t.Errorf("clearing the filter keeps the cursor on %q", m.top().currentRow().Name)
	}
}

func TestViewerTextAndHex(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "G", "enter") // etc/os-release
	fr := m.top()
	if fr.view == nil || fr.view.mode != modeText {
		t.Fatalf("viewer not open in text mode: %+v", fr.view)
	}
	if fr.view.lines[0] != `PRETTY_NAME="Fixture Linux"` {
		t.Errorf("first line %q", fr.view.lines[0])
	}
	out := m.View()
	if !strings.Contains(out, "1 PRETTY_NAME") || !strings.Contains(out, "text · ") || !strings.Contains(out, "mode 0644") || !strings.Contains(out, "3 lines") {
		t.Errorf("text view:\n%s", out)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc", "os-release")
	press(t, m, "h")
	if fr.view.mode != modeHex || !strings.Contains(m.View(), "50 52 45 54") {
		t.Errorf("hex view:\n%s", m.View())
	}
	press(t, m, "h")
	if fr.view.mode != modeText {
		t.Error("h must switch back to text")
	}
	press(t, m, "/", "VERSION", "enter")
	if len(fr.view.hits) != 1 || fr.view.hits[0] != 2 {
		t.Errorf("hits %v", fr.view.hits)
	}
	press(t, m, "backspace")
	if m.top().view != nil {
		t.Error("backspace must leave the viewer")
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
}

func TestViewerJSONPretty(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "down", "enter") // etc/config.json
	v := m.top().view
	if v == nil || v.text.Label != "json" || !v.pretty || v.lines[0] != "{" {
		t.Fatalf("json viewer %+v", v)
	}
	if !strings.Contains(m.View(), `"listen": ":8080"`) || !strings.Contains(m.View(), "json · ") {
		t.Errorf("view:\n%s", m.View())
	}
	press(t, m, "p")
	if v.pretty || !strings.HasPrefix(v.lines[0], `{"listen":`) {
		t.Errorf("raw lines %v", v.lines)
	}
	press(t, m, "p")
	if !v.pretty {
		t.Error("p toggles back")
	}
}

func TestViewerBinaryOpensInHex(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "enter", "enter") // bin/app
	fr := m.top()
	v := fr.view
	if v == nil || v.mode != modeHex || string(v.win[:4]) != "\x7fELF" {
		t.Fatalf("binary viewer %+v", v)
	}
	if out := m.View(); !strings.Contains(out, ".ELF") || !strings.Contains(out, "hex · 300.0 KiB") {
		t.Errorf("hex view:\n%s", out)
	}
	body := int64(m.bodyHeight())
	rows := (fr.file.Size + hexRowBytes - 1) / hexRowBytes
	last := (rows - body) * hexRowBytes
	press(t, m, "G")
	if v.hexOff != last || !strings.Contains(m.View(), fmt.Sprintf("%08x", last)) {
		t.Errorf("G: offset %#x, want %#x", v.hexOff, last)
	}
	press(t, m, "pgup")
	if v.hexOff != last-body*hexRowBytes {
		t.Errorf("pgup: offset %#x", v.hexOff)
	}
	press(t, m, ":", "0x100", "enter")
	if v.hexOff != 0x100 || !strings.Contains(m.View(), "00000100") {
		t.Errorf("goto: offset %#x\n%s", v.hexOff, m.View())
	}
	press(t, m, "down", "down")
	if v.hexOff != 0x120 {
		t.Errorf("down: offset %#x", v.hexOff)
	}
	press(t, m, "h")
	if v.mode != modeText || v.text == nil || len(v.lines) == 0 {
		t.Errorf("h on a binary loads it as text: %+v", v)
	}
	press(t, m, "h")
	if v.mode != modeHex {
		t.Error("h again returns to hex")
	}
}

func TestSymlinksResolveOnEnter(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "G", "up", "enter") // etc/link-to-os
	if v := m.top().view; v == nil || v.lines[0] != `PRETTY_NAME="Fixture Linux"` {
		t.Fatalf("symlink to a file opens the file: %+v", v)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc", "link-to-os")
	press(t, m, "backspace", "g", "down", "down", "enter") // etc/dangling
	if m.top().view != nil || !strings.Contains(m.status, "no such path") {
		t.Errorf("dangling symlink: status %q", m.status)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "backspace", "g", "enter", "down", "enter") // bin/sbin -> ../usr/bin
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "bin", "sbin")
	assertTop(t, m, "tool.sh")
}

func TestStartReferences(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app")
	assertCrumbs(t, m, "library/app")
	m = newTestModel(t, f, "library/app@"+f.m1.String())
	assertCrumbs(t, m, "library/app", "@"+shortRef(f.m1), "storage")
	for _, bad := range []string{"nobody/here", "nobody/here:x", "library/app:nope", "library/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"} {
		if _, err := newModel(f.b, bad); err == nil {
			t.Errorf("newModel(%q) must fail", bad)
		}
	}
}

func TestIndexFilesystemChooser(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:latest")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":latest", "filesystem")
	assertTop(t, m, shortRef(f.m1), shortRef(f.m2))
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":latest", "filesystem", "linux/amd64")
	assertTop(t, m, "bin", "etc", "usr")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":latest", "storage")
}

func TestRawImageFilesystemUnavailable(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "tools/rawimg:r1")
	press(t, m, "f")
	assertTop(t, m, "rootfs unavailable")
	press(t, m, "enter")
	if m.status != "nothing to open here" {
		t.Errorf("status %q", m.status)
	}
}

func TestStaleLoadIsDropped(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	before := visibleNames(m.top())
	m.Update(listLoadedMsg{id: 9999, rows: []Row{{Name: "ghost"}}})
	if got := visibleNames(m.top()); !slices.Equal(got, before) {
		t.Errorf("a stale load changed the rows: %v", got)
	}
}

func TestInfoPopupAndQuit(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	press(t, m, "i")
	if m.popup == nil || !strings.Contains(m.View(), "repository  library/app") {
		t.Errorf("popup:\n%s", m.View())
	}
	press(t, m, "down")
	if m.popup != nil || m.top().cursor != 0 {
		t.Error("the key that closes the popup does nothing else")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q must quit")
	}
}
