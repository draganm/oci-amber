package browse

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderHex(t *testing.T) {
	data := append([]byte("\x7fELF\x02\x01\x01\x00"), make([]byte, 8)...)
	data = append(data, '@')
	got := RenderHex(0x20, data, 100)
	want := "00000020  7f 45 4c 46 02 01 01 00  00 00 00 00 00 00 00 00  .ELF............\n" +
		"00000030  40" + strings.Repeat(" ", 48) + "@" // 47 cells of hex padding plus the column gap
	if got != want {
		t.Errorf("RenderHex:\n%q\nwant:\n%q", got, want)
	}
	if RenderHex(0, nil, 80) != "" {
		t.Error("no data renders nothing")
	}
	if got := RenderHex(0, data[:16], 20); len([]rune(strings.Split(got, "\n")[0])) != 20 {
		t.Errorf("rows are cut to the width: %q", got)
	}
}

func TestReadWindow(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	dir := f.lookupKey(bl.Root(), "blobs")
	rows := mustList(t, &prismBlobsNode{st: f.st, bl: bl, dir: dir})
	var app Row
	for _, r := range rows {
		if r.Detail == "bin/app" {
			app = r
		}
	}
	file, err := app.Child.(Opener).Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readWindow(file, 100<<10, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.bigBinary[100<<10:100<<10+4096]) {
		t.Error("window bytes differ")
	}
	tail, err := readWindow(file, file.Size-10, 4096)
	if err != nil || !bytes.Equal(tail, f.bigBinary[len(f.bigBinary)-10:]) {
		t.Errorf("tail window: %v, %d bytes", err, len(tail))
	}
	if past, err := readWindow(file, file.Size, 16); err != nil || len(past) != 0 {
		t.Errorf("window past the end: %v, %d bytes", err, len(past))
	}
}
