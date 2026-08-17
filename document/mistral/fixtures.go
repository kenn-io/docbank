package mistral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/docbank/document"
)

// ProbeFixtureConfig identifies the complete fixture matrix and its dedicated
// private staging directory.
type ProbeFixtureConfig struct {
	FixtureDirectory string
	SpoolDirectory   string
	MaxSpoolBytes    int64
	MinFreeBytes     int64
}

type probeFixture struct {
	prepared *PreparedDocument
}

func (f probeFixture) snapshot() (preparedSnapshot, error) {
	if f.prepared == nil {
		return preparedSnapshot{}, errors.New("mistral probe fixture is invalid")
	}
	return f.prepared.snapshot()
}

func (f probeFixture) release() error {
	if f.prepared == nil {
		return nil
	}
	return f.prepared.Release()
}

// ValidateProbeFixtures validates and stages the complete matrix without
// credentials or network access.
func ValidateProbeFixtures(ctx context.Context, policy Policy, config ProbeFixtureConfig) error {
	fixtures, err := loadProbeFixtures(ctx, policy, config)
	if err != nil {
		return err
	}
	return releaseProbeFixtures(fixtures)
}

func loadProbeFixtures(
	ctx context.Context,
	policy Policy,
	config ProbeFixtureConfig,
) (map[string]probeFixture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.digest == "" {
		return nil, errors.New("mistral policy is invalid; use NewPolicy")
	}
	if config.FixtureDirectory == "" || config.SpoolDirectory == "" ||
		config.MaxSpoolBytes < policy.values.MaxDocumentBytes || config.MinFreeBytes <= 0 {
		return nil, errors.New("mistral probe fixtures require directories and valid staging bounds")
	}
	fixtureRoot, err := openPrivateFixtureRoot(config.FixtureDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fixtureRoot.Close() }()
	if err := validatePrivateDirectory(config.SpoolDirectory); err != nil {
		return nil, err
	}

	fixtures := make(map[string]probeFixture, len(candidateFormats))
	fail := func(loadErr error) error {
		return errors.Join(loadErr, releaseProbeFixtures(fixtures))
	}
	for _, candidate := range candidateFormats {
		if err := ctx.Err(); err != nil {
			return nil, fail(err)
		}
		fixture, err := loadProbeFixture(ctx, policy, config, fixtureRoot, candidate)
		if err != nil {
			return nil, fail(fmt.Errorf("load Mistral probe fixture %q: %w", candidate.ID, err))
		}
		fixtures[candidate.ID] = fixture
	}
	return fixtures, nil
}

func openPrivateFixtureRoot(directory string) (*os.Root, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect Mistral probe fixture directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("mistral probe fixture path must be a real directory")
	}
	if err := validatePrivateFixtureDirectory(directory); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open Mistral probe fixture directory: %w", err)
	}
	openedInfo, err := root.Lstat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, errors.New("mistral probe fixture directory changed while opening")
	}
	return root, nil
}

func loadProbeFixture(
	ctx context.Context,
	policy Policy,
	config ProbeFixtureConfig,
	fixtureRoot *os.Root,
	candidate CandidateFormat,
) (probeFixture, error) {
	pathInfo, err := fixtureRoot.Lstat(candidate.ID)
	if err != nil {
		return probeFixture{}, fmt.Errorf("inspect fixture: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 ||
		pathInfo.Size() <= 0 || pathInfo.Size() > policy.values.MaxDocumentBytes {
		return probeFixture{}, errors.New("fixture must be a bounded regular non-symlink file")
	}
	file, err := openPrivateFile(filepath.Join(fixtureRoot.Name(), candidate.ID))
	if err != nil {
		return probeFixture{}, fmt.Errorf("open fixture: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return probeFixture{}, fmt.Errorf("inspect opened fixture: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return probeFixture{}, errors.New("fixture changed while opening")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, reader: file}, policy.values.MaxDocumentBytes+1))
	if err != nil {
		_ = file.Close()
		return probeFixture{}, fmt.Errorf("hash fixture: %w", err)
	}
	if written != openedInfo.Size() || written > policy.values.MaxDocumentBytes {
		_ = file.Close()
		return probeFixture{}, errors.New("fixture changed or exceeded bounds while hashing")
	}
	detected, err := DetectFormat(file, written, candidate.MediaType)
	if err != nil {
		_ = file.Close()
		return probeFixture{}, fmt.Errorf("validate fixture container: %w", err)
	}
	if detected.ID != candidate.ID {
		_ = file.Close()
		return probeFixture{}, fmt.Errorf("detected %q, want %q", detected.ID, candidate.ID)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return probeFixture{}, fmt.Errorf("rewind fixture: %w", err)
	}
	prepared, err := Prepare(ctx, file, policy, PrepareOptions{
		Directory: config.SpoolDirectory, DeclaredMediaType: candidate.MediaType,
		ExpectedSize: written, ExpectedSHA256: hex.EncodeToString(hash.Sum(nil)),
		MaxSpoolBytes: config.MaxSpoolBytes, MinFreeBytes: config.MinFreeBytes,
	})
	if err != nil {
		return probeFixture{}, err
	}
	return probeFixture{prepared: prepared}, nil
}

func releaseProbeFixtures(fixtures map[string]probeFixture) error {
	identifiers := make([]string, 0, len(fixtures))
	for identifier := range fixtures {
		identifiers = append(identifiers, identifier)
	}
	slices.Sort(identifiers)
	slices.Reverse(identifiers)
	var releaseErrors []error
	for _, identifier := range identifiers {
		if err := fixtures[identifier].release(); err != nil {
			releaseErrors = append(releaseErrors, err)
		}
	}
	return errors.Join(releaseErrors...)
}

func sourceDocumentHasText(source document.SourceDocument) bool {
	for _, unit := range source.Units {
		if unit.Markdown != "" || unit.Header != "" || unit.Footer != "" {
			return true
		}
	}
	return false
}
