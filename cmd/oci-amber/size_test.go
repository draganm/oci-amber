package main

import (
	"log/slog"
	"testing"
)

func TestParseSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{"1024B", 1024},
		{"1024b", 1024},
		{"1KiB", 1 << 10},
		{"64MiB", 64 << 20},
		{"64mib", 64 << 20},
		{"64 MiB", 64 << 20},
		{" 64MiB ", 64 << 20},
		{"2GiB", 2 << 30},
		{"1KB", 1000},
		{"100MB", 100 * 1000 * 1000},
		{"3GB", 3 * 1000 * 1000 * 1000},
		{"8589934592", 8 << 30},
	}
	for _, tc := range ok {
		got, err := parseSize(tc.in)
		if err != nil {
			t.Errorf("parseSize(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "   ", "MiB", "-1", "-64MiB", "1.5GiB", "64MiBs", "64 M i B", "64TiB", "64x", "0x10", "9223372036854775807KiB", "99999999999999999999"}
	for _, in := range bad {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want error", in, got)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	ok := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"DEBUG": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"Info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		" info": slog.LevelInfo,
	}
	for in, want := range ok {
		got, err := parseLogLevel(in)
		if err != nil {
			t.Errorf("parseLogLevel(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "verbose", "trace", "warning", "3"} {
		if got, err := parseLogLevel(in); err == nil {
			t.Errorf("parseLogLevel(%q) = %v, want error", in, got)
		}
	}
}
