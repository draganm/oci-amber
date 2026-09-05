package browse

import (
	"bytes"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		size  int64
		probe []byte
		kind  Kind
		label string
	}{
		{"os-release", 40, []byte("ID=fixture\n"), KindText, "text"},
		{"config.json", 20, []byte(`{"a":1}`), KindText, "json"},
		{"app.yaml", 10, []byte("a: 1\n"), KindText, "yaml"},
		{"run", 20, []byte("#!/bin/sh\necho\n"), KindText, "shell"},
		{"tool.sh", 5, []byte("echo\n"), KindText, "shell"},
		{"README.md", 5, []byte("# hi\n"), KindText, "markdown"},
		{"c.toml", 5, []byte("a=1\n"), KindText, "toml"},
		{"app", 300, []byte("\x7fELF\x02\x01\x01\x00"), KindBinary, "binary"},
		{"latin1.txt", 5, []byte("caf\xe9\n"), KindBinary, "binary"},
		{"big.txt", MaxTextSize + 1, []byte("text"), KindTooLarge, "binary"},
		{"exactly.txt", MaxTextSize, []byte("text"), KindText, "text"},
		{"empty", 0, nil, KindText, "text"},
	}
	for _, tc := range tests {
		got := Classify(tc.name, tc.size, tc.probe)
		if got.Kind != tc.kind || got.Label != tc.label {
			t.Errorf("Classify(%s) = %+v, want kind %d label %q", tc.name, got, tc.kind, tc.label)
		}
	}
}

func TestClassifyToleratesRuneCutByProbe(t *testing.T) {
	// "é" is c3 a9; the probe ends after c3 while the file goes on.
	probe := append(bytes.Repeat([]byte("a"), 10), 0xc3)
	if got := Classify("x.txt", int64(len(probe))+1, probe); got.Kind != KindText {
		t.Errorf("cut rune at a truncated probe's end: %+v, want text", got)
	}
	if got := Classify("x.txt", int64(len(probe)), probe); got.Kind != KindBinary {
		t.Errorf("cut rune at the file's end: %+v, want binary", got)
	}
}

func TestLoadTextPrettyPrintsJSON(t *testing.T) {
	raw := []byte(`{"listen":":8080","workers":4,"tags":["a","b"]}`)
	tx := LoadText("text", raw)
	if tx.Label != "json" || tx.Pretty == nil {
		t.Fatalf("LoadText = %+v", tx)
	}
	want := "{\n  \"listen\": \":8080\",\n  \"workers\": 4,\n  \"tags\": [\n    \"a\",\n    \"b\"\n  ]\n}"
	if string(tx.Pretty) != want {
		t.Errorf("pretty:\n%s\nwant:\n%s", tx.Pretty, want)
	}
	if !bytes.Equal(tx.Raw, raw) {
		t.Error("Raw must be the stored bytes")
	}
	plain := LoadText("text", []byte("not json\n"))
	if plain.Label != "text" || plain.Pretty != nil {
		t.Errorf("plain = %+v", plain)
	}
	if LoadText("text", nil).Pretty != nil {
		t.Error("empty file is not JSON")
	}
}

func TestLines(t *testing.T) {
	got := Lines([]byte("one\ttab\r\nctrl\x01\nbad\xffbyte\nlast"))
	want := []string{"one    tab", "ctrl^A", `bad\xffbyte`, "last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Lines = %q, want %q", got, want)
	}
	if Lines(nil) != nil || len(Lines([]byte("a\n"))) != 1 {
		t.Error("empty file has no lines; a trailing newline adds none")
	}
}

func TestLineOffsets(t *testing.T) {
	data := []byte("ab\ncd\n\nef")
	for n, want := range []int64{0, 3, 6, 7} {
		if got := lineOffset(data, n); got != want {
			t.Errorf("lineOffset(%d) = %d, want %d", n, got, want)
		}
	}
	if got := lineOffset(data, 99); got != int64(len(data)) {
		t.Errorf("lineOffset past the end = %d", got)
	}
	for off, want := range map[int64]int{0: 0, 2: 0, 3: 1, 6: 2, 7: 3, 8: 3, 99: 3} {
		if got := lineAt(data, off); got != want {
			t.Errorf("lineAt(%d) = %d, want %d", off, got, want)
		}
	}
}

func TestRenderText(t *testing.T) {
	lines := []string{"alpha", "beta gamma delta", "third"}
	got := RenderText(lines, 0, 0, 4, 12, map[int]bool{1: true})
	want := "1 alpha\n2 beta gamma\n3 third\n"
	if got != want {
		t.Errorf("RenderText:\n%q\nwant:\n%q", got, want)
	}
	if got := RenderText(lines, 1, 5, 2, 40, nil); got != "2 gamma delta\n3 " {
		t.Errorf("scrolled: %q", got)
	}
	if got := RenderText(nil, 0, 0, 2, 20, nil); got != "\n" {
		t.Errorf("no lines: %q", got)
	}
}

func TestStringHelpers(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := padRight("ab", 4); got != "ab  " {
		t.Errorf("padRight = %q", got)
	}
	if got := cutLeft("héllo", 2); got != "llo" {
		t.Errorf("cutLeft = %q", got)
	}
	if got := cutLeft("ab", 5); got != "" {
		t.Errorf("cutLeft past the end = %q", got)
	}
}
