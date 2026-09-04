package blob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestIsTarHeader(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	valid := buf.Bytes()[:512]
	flipped := append([]byte{}, valid...)
	flipped[0] ^= 1
	spaces := append([]byte{}, valid...)
	copy(spaces[148:156], "        ")
	json := append([]byte(`{"architecture":"amd64"}`), make([]byte, 512)...)
	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"archive/tar header", valid, true},
		{"flipped byte", flipped, false},
		{"blank checksum", spaces, false},
		{"all zeros", make([]byte, 512), false},
		{"json padded", json, false},
		{"short", valid[:511], false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := isTarHeader(c.b); got != c.want {
			t.Errorf("%s: isTarHeader = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnalyzeClassifies(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	ctx := context.Background()
	tarData := tarBytes(t, "etc/motd", textBytes(3000, 5))
	// The two gzip fixtures below must carry a tar, or the tar probe would
	// classify them not-tar before Analyze ever sees them; splitting one
	// tar in half keeps them exercising the Analyze error classification.
	p1, p2 := tarData[:len(tarData)/2], tarData[len(tarData)/2:]
	jsonConfig := []byte(`{"architecture":"amd64","os":"linux"}`)
	cases := []struct {
		name   string
		data   []byte
		kind   Kind
		reason RawReason
		format string
		engine bool
	}{
		{"json config", jsonConfig, KindRaw, ReasonNotTar, "none", false},
		{"gzipped json config", gzipBytes(t, bytes.Repeat(jsonConfig, 64), gzip.DefaultCompression), KindRaw, ReasonNotTar, "gzip", false},
		{"empty", nil, KindRaw, ReasonNotTar, "none", false},
		{"empty tar", make([]byte, 1024), KindPrism, "", "none", false},
		{"gzipped empty tar", gzipBytes(t, make([]byte, 1024), gzip.DefaultCompression), KindPrism, "", "gzip", true},
		{"gnu padded empty tar", make([]byte, 10240), KindPrism, "", "none", false},
		{"zeros past a record", make([]byte, 10240+512), KindRaw, ReasonNotTar, "none", false},
		{"zero block then junk", append(make([]byte, 512), []byte("not a header")...), KindRaw, ReasonNotTar, "none", false},
		{"plain tar", tarData, KindPrism, "", "none", false},
		{"go gzip tar", gzipBytes(t, tarData, gzip.DefaultCompression), KindPrism, "", "gzip", true},
		{"two-level gzip", twoLevelGzip(t, p1, p2), KindRaw, ReasonNotReproducible, "gzip", false},
		{"bad crc gzip", corruptGzipCRC(gzipBytes(t, tarData, gzip.DefaultCompression)), KindRaw, ReasonCorrupt, "gzip", false},
		{"multi-member gzip", slices.Concat(gzipBytes(t, p1, gzip.BestSpeed), gzipBytes(t, p2, gzip.BestSpeed)), KindRaw, ReasonUnsupported, "gzip", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, err := b.analyze(ctx, spoolOf(c.data))
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			if dec.kind != c.kind || dec.reason != c.reason || dec.format != c.format {
				t.Fatalf("decision = {%s %q %s}, want {%s %q %s}", dec.kind, dec.reason, dec.format, c.kind, c.reason, c.format)
			}
			if c.kind == KindPrism {
				if dec.params == nil || string(dec.params.Format) != c.format {
					t.Fatalf("prism decision needs params for %s: %+v", c.format, dec.params)
				}
				if c.engine && dec.params.Engine == "" {
					t.Fatal("compressed prism decision needs an engine")
				}
			} else if dec.params != nil {
				t.Fatalf("raw decision must not carry params: %+v", dec.params)
			}
			assertSpoolDirEmpty(t, b)
		})
	}
}

func TestAnalyzeTimeoutFallsBackToRaw(t *testing.T) {
	b, _, _ := newTestStore(t, Options{AnalyzeTimeout: time.Nanosecond})
	gz := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(3000, 5)), gzip.DefaultCompression)
	dec, err := b.analyze(context.Background(), spoolOf(gz))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if dec.kind != KindRaw || dec.reason != ReasonAnalyzeTimeout || dec.format != "gzip" {
		t.Fatalf("decision = {%s %q %s}, want raw analyze-timeout gzip", dec.kind, dec.reason, dec.format)
	}
}

func TestAnalyzeCancelledContextFails(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, data := range map[string][]byte{
		"json":     []byte(`{"architecture":"amd64"}`),
		"gzip tar": gzipBytes(t, tarBytes(t, "a", textBytes(100, 1)), gzip.BestSpeed),
	} {
		if _, err := b.analyze(ctx, spoolOf(data)); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: analyze = %v, want context.Canceled", name, err)
		}
	}
}
