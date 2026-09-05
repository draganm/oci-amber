package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v2"

	"github.com/draganm/oci-amber/dockerarchive"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/oci"
)

// saveConfig is everything `save` needs. saveConfigFromCLI fills it from
// flags; tests construct it directly and call runSave.
type saveConfig struct {
	Store  string
	Output string   // "" or "-" means stdout
	Refs   []string // repo, repo:tag or repo@digest, validated
	// Platform is the platform whose child an index's manifest.json entry
	// describes; nil means the host's.
	Platform *oci.Platform
	Stdout   io.Writer // nil means os.Stdout
	Stderr   io.Writer // nil means os.Stderr
}

func saveFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write the archive to `path` instead of stdout"},
	}
}

func saveConfigFromCLI(c *cli.Context) (saveConfig, error) {
	if c.NArg() < 1 {
		return saveConfig{}, errors.New("save takes at least one reference (repo, repo:tag or repo@digest)")
	}
	cfg := saveConfig{Store: c.String("store"), Output: c.String("output"), Refs: c.Args().Slice()}
	if cfg.Store == "" {
		return saveConfig{}, errors.New("--store must not be empty")
	}
	for _, r := range cfg.Refs {
		if _, err := parseSaveRef(r); err != nil {
			return saveConfig{}, err
		}
	}
	return cfg, nil
}

// saveRef is a parsed reference: a tag, a digest, or with neither every
// tag of the repository.
type saveRef struct {
	repo   string
	tag    string
	digest oci.Digest
}

func (r saveRef) String() string {
	switch {
	case r.digest != "":
		return r.repo + "@" + r.digest.String()
	case r.tag != "":
		return r.repo + ":" + r.tag
	default:
		return r.repo
	}
}

// parseSaveRef parses and validates repo, repo:tag or repo@digest.
func parseSaveRef(s string) (saveRef, error) {
	repo, reference := oci.SplitReference(s)
	if err := oci.ValidateRepository(repo); err != nil {
		return saveRef{}, fmt.Errorf("%q: %v", s, err)
	}
	r := saveRef{repo: repo}
	switch {
	case reference == "" && len(s) > len(repo):
		return saveRef{}, fmt.Errorf("%q: empty tag", s)
	case reference == "":
	case oci.IsDigest(reference):
		d, err := oci.ParseDigest(reference)
		if err != nil {
			return saveRef{}, fmt.Errorf("%q: %v", s, err)
		}
		r.digest = d
	default:
		if err := oci.ValidateTag(reference); err != nil {
			return saveRef{}, fmt.Errorf("%q: %v", s, err)
		}
		r.tag = reference
	}
	return r, nil
}

// runSave opens the store read-only, resolves every reference, then writes
// one `docker image save` archive of them to the output. References are
// resolved before the output is created, so an unknown one leaves nothing
// behind; a failure while writing removes the output file.
func runSave(ctx context.Context, cfg saveConfig) error {
	stdout, stderr := cfg.Stdout, cfg.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	toFile := cfg.Output != "" && cfg.Output != "-"
	if !toFile {
		if f, ok := stdout.(*os.File); ok && term.IsTerminal(f.Fd()) {
			return errors.New("cowardly refusing to save to a terminal. Use the -o flag or redirect.")
		}
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ro, err := openReadOnly(cfg.Store, log)
	if err != nil {
		return err
	}
	defer ro.Close()

	exports, err := resolveExports(ro.images, cfg.Refs)
	if err != nil {
		return err
	}
	platform := cfg.Platform
	if platform == nil {
		platform = &oci.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	}

	out := stdout
	var f *os.File
	if toFile {
		if f, err = os.Create(cfg.Output); err != nil {
			return fmt.Errorf("creating %s: %w", cfg.Output, err)
		}
		out = f
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	err = dockerarchive.Write(ctx, bw, storeSource{ro}, exports, dockerarchive.WriteOptions{Platform: platform})
	if err == nil {
		err = bw.Flush()
	}
	if f != nil {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("writing %s: %w", cfg.Output, cerr)
		}
		if err != nil {
			os.Remove(cfg.Output)
		}
	}
	if err != nil {
		return err
	}
	return ro.Close()
}

// resolveExports turns references into the images to save: a bare
// repository contributes every tag, in tag order. A reference that does
// not exist is an error.
func resolveExports(images *image.Store, refs []string) ([]dockerarchive.Export, error) {
	var exports []dockerarchive.Export
	add := func(repo, reference, tag string) error {
		im, err := images.Open(repo, reference)
		if errors.Is(err, image.ErrNotFound) {
			return fmt.Errorf("%s: not found", saveRef{repo: repo, tag: tag, digest: oci.Digest(reference)}.String())
		}
		if err != nil {
			return err
		}
		exports = append(exports, dockerarchive.Export{Repo: repo, Digest: im.Meta.Digest, MediaType: im.Meta.MediaType, Tag: tag})
		return nil
	}
	for _, s := range refs {
		r, err := parseSaveRef(s)
		if err != nil {
			return nil, err
		}
		switch {
		case r.digest != "":
			if err := add(r.repo, r.digest.String(), ""); err != nil {
				return nil, err
			}
		case r.tag != "":
			if err := add(r.repo, r.tag, r.tag); err != nil {
				return nil, err
			}
		default:
			tags, err := images.Tags(r.repo)
			if errors.Is(err, image.ErrNotFound) || (err == nil && len(tags) == 0) {
				return nil, fmt.Errorf("%s: not found", r.repo)
			}
			if err != nil {
				return nil, err
			}
			for _, tag := range tags {
				if err := add(r.repo, tag, tag); err != nil {
					return nil, err
				}
			}
		}
	}
	return exports, nil
}

// storeSource is the dockerarchive.Source over a read-only store.
type storeSource struct{ ro *readOnlyStore }

func (s storeSource) Manifest(ctx context.Context, repo string, d oci.Digest) ([]byte, error) {
	im, err := s.ro.images.Open(repo, d.String())
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := im.WriteTo(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s storeSource) Blob(ctx context.Context, d oci.Digest, w io.Writer) error {
	bl, err := s.ro.blobs.Open(d)
	if err != nil {
		return err
	}
	return bl.WriteTo(ctx, w)
}
