package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every OCI_AMBER_* variable for the duration of the test so
// the developer's shell cannot leak into flag defaults.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
		t.Cleanup(func() { os.Setenv(name, value) })
	}
}

// runApp runs the serve command with args and returns the config the action
// received, or the error the app returned.
func runApp(t *testing.T, args ...string) (config, error) {
	t.Helper()
	var got config
	called := false
	app := newApp(commands{Serve: func(_ context.Context, cfg config) error {
		called = true
		got = cfg
		return nil
	}})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "serve"}, args...))
	if err == nil && !called {
		t.Fatal("serve action was not called")
	}
	return got, err
}

func TestServeFlagDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := runApp(t, "--store", "/srv/amber")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := config{
		Store:                 "/srv/amber",
		WorkDir:               filepath.Join("/srv/amber", "work"),
		Listen:                ":5000",
		MaxInMemory:           64 << 20,
		AnalyzeParallelism:    2,
		AnalyzeTimeout:        15 * time.Minute,
		MaxConcurrentFinalize: max(1, runtime.NumCPU()/2),
		VerifyRoundTrip:       false,
		AllowRaw:              false,
		UploadTimeout:         time.Hour,
		GCInterval:            0,
		LogLevel:              slog.LevelInfo,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestServeFlagsExplicit(t *testing.T) {
	clearEnv(t)
	cfg, err := runApp(t,
		"--store", "/srv/amber",
		"--work-dir", "/scratch/work",
		"--listen", "127.0.0.1:6000",
		"--max-in-memory", "1GiB",
		"--analyze-parallelism", "4",
		"--analyze-timeout", "1m",
		"--max-concurrent-finalize", "3",
		"--verify-roundtrip",
		"--allow-raw",
		"--upload-timeout", "30m",
		"--gc-interval", "2h",
		"--log-level", "debug",
	)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := config{
		Store:                 "/srv/amber",
		WorkDir:               "/scratch/work",
		Listen:                "127.0.0.1:6000",
		MaxInMemory:           1 << 30,
		AnalyzeParallelism:    4,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 3,
		VerifyRoundTrip:       true,
		AllowRaw:              true,
		UploadTimeout:         30 * time.Minute,
		GCInterval:            2 * time.Hour,
		LogLevel:              slog.LevelDebug,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("explicit flags:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestServeFlagsFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("OCI_AMBER_STORE", "/srv/env-store")
	t.Setenv("OCI_AMBER_WORK_DIR", "/scratch/env-work")
	t.Setenv("OCI_AMBER_LISTEN", "127.0.0.1:7000")
	t.Setenv("OCI_AMBER_MAX_IN_MEMORY", "16MiB")
	t.Setenv("OCI_AMBER_ANALYZE_PARALLELISM", "3")
	t.Setenv("OCI_AMBER_ANALYZE_TIMEOUT", "2m")
	t.Setenv("OCI_AMBER_MAX_CONCURRENT_FINALIZE", "5")
	t.Setenv("OCI_AMBER_VERIFY_ROUNDTRIP", "true")
	t.Setenv("OCI_AMBER_ALLOW_RAW", "true")
	t.Setenv("OCI_AMBER_UPLOAD_TIMEOUT", "45m")
	t.Setenv("OCI_AMBER_GC_INTERVAL", "30m")
	t.Setenv("OCI_AMBER_LOG_LEVEL", "warn")

	cfg, err := runApp(t)
	if err != nil {
		t.Fatalf("serve from env: %v", err)
	}
	want := config{
		Store:                 "/srv/env-store",
		WorkDir:               "/scratch/env-work",
		Listen:                "127.0.0.1:7000",
		MaxInMemory:           16 << 20,
		AnalyzeParallelism:    3,
		AnalyzeTimeout:        2 * time.Minute,
		MaxConcurrentFinalize: 5,
		VerifyRoundTrip:       true,
		AllowRaw:              true,
		UploadTimeout:         45 * time.Minute,
		GCInterval:            30 * time.Minute,
		LogLevel:              slog.LevelWarn,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("env flags:\n got %+v\nwant %+v", cfg, want)
	}

	// A command-line flag wins over the environment.
	cfg, err = runApp(t, "--listen", ":8000")
	if err != nil {
		t.Fatalf("serve with override: %v", err)
	}
	if cfg.Listen != ":8000" {
		t.Fatalf("listen = %q, want %q (flag must override env)", cfg.Listen, ":8000")
	}
}

func TestServeRejectsBadValues(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing store", nil, `Required flag "store" not set`},
		{"empty store", []string{"--store", ""}, "--store must not be empty"},
		{"empty listen", []string{"--store", "/s", "--listen", ""}, "--listen must not be empty"},
		{"bad size", []string{"--store", "/s", "--max-in-memory", "lots"}, "--max-in-memory"},
		{"negative size", []string{"--store", "/s", "--max-in-memory", "-1"}, "--max-in-memory"},
		{"bad log level", []string{"--store", "/s", "--log-level", "verbose"}, "--log-level"},
		{"zero parallelism", []string{"--store", "/s", "--analyze-parallelism", "0"}, "--analyze-parallelism must be at least 1"},
		{"zero analyze timeout", []string{"--store", "/s", "--analyze-timeout", "0"}, "--analyze-timeout must be positive"},
		{"zero finalize", []string{"--store", "/s", "--max-concurrent-finalize", "0"}, "--max-concurrent-finalize must be at least 1"},
		{"zero upload timeout", []string{"--store", "/s", "--upload-timeout", "0s"}, "--upload-timeout must be positive"},
		{"negative gc interval", []string{"--store", "/s", "--gc-interval", "-1m"}, "--gc-interval must not be negative"},
		{"unparsable duration", []string{"--store", "/s", "--analyze-timeout", "soon"}, "invalid value"},
		{"unknown flag", []string{"--store", "/s", "--bogus"}, "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runApp(t, tc.args...)
			if err == nil {
				t.Fatalf("args %q: expected an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("args %q: error %q does not contain %q", tc.args, err.Error(), tc.want)
			}
		})
	}
}

func TestServeRejectsBadEnvValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("OCI_AMBER_ANALYZE_PARALLELISM", "many")
	_, err := runApp(t, "--store", "/s")
	if err == nil || !strings.Contains(err.Error(), "OCI_AMBER_ANALYZE_PARALLELISM") {
		t.Fatalf("expected an error naming the environment variable, got %v", err)
	}
}
