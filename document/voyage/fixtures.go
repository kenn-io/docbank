package voyage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/kit/safefileio"
)

// Fixture file names inside a probe fixture directory.
const (
	FixtureJPEG        = "image_jpeg.jpg"
	FixturePNG         = "image_png.png"
	FixtureWebP        = "image_webp.webp"
	FixtureGIFStill    = "image_gif_still.gif"
	FixtureGIFAnimated = "image_gif_animated.gif"
	FixtureMP4         = "video_mp4.mp4"
	FixtureRed         = "probe_red.png"
	FixtureBlue        = "probe_blue.png"

	// ProbeQueryText is the text query the probe ranks against the red and
	// blue reference documents.
	ProbeQueryText = "a solid red square"
	// ProbeBlueText is the opposing text used to prove that composite inputs
	// consume their text part independently of their media part.
	ProbeBlueText = "a solid blue square"
	// ProbeInterleavedText is the text part of the interleaved probe document.
	ProbeInterleavedText = "a solid red square"

	fixtureSide    = 64
	animatedFrames = 4
)

// SeedFixtureNames lists the fixtures an operator must supply as synthetic
// seeds because the Go standard library cannot encode them.
var SeedFixtureNames = []string{FixtureWebP, FixtureMP4}

var (
	fixtureRed  = color.RGBA{R: 220, G: 30, B: 30, A: 255}
	fixtureBlue = color.RGBA{R: 30, G: 30, B: 220, A: 255}
)

type fixtureSpec struct {
	name     string
	format   media.Format
	animated bool
	seed     bool
	generate func() []byte
}

var fixtureSpecs = []fixtureSpec{
	{name: FixtureJPEG, format: media.FormatJPEG, generate: func() []byte { return mediatest.JPEG(fixtureSide, fixtureSide, fixtureRed) }},
	{name: FixturePNG, format: media.FormatPNG, generate: func() []byte { return mediatest.PNG(fixtureSide, fixtureSide, fixtureBlue) }},
	{name: FixtureWebP, format: media.FormatWebP, seed: true},
	{name: FixtureGIFStill, format: media.FormatGIF, generate: func() []byte { return mediatest.GIF(fixtureSide, fixtureSide, 1) }},
	{name: FixtureGIFAnimated, format: media.FormatGIF, animated: true, generate: func() []byte { return mediatest.GIF(fixtureSide, fixtureSide, animatedFrames) }},
	{name: FixtureMP4, format: media.FormatMP4, seed: true},
	{name: FixtureRed, format: media.FormatPNG, generate: func() []byte { return mediatest.PNG(fixtureSide, fixtureSide, fixtureRed) }},
	{name: FixtureBlue, format: media.FormatPNG, generate: func() []byte { return mediatest.PNG(fixtureSide, fixtureSide, fixtureBlue) }},
}

// FixtureOptions controls probe fixture generation.
type FixtureOptions struct {
	// SeedDirectory holds operator-supplied synthetic WebP and MP4 seeds
	// named as in SeedFixtureNames. Required.
	SeedDirectory string
}

// WriteProbeFixtures writes the deterministic fixture set into destination.
// The destination must not exist, and its parent and the seed directory must
// already be owner-private. A complete set is published from private staging.
func WriteProbeFixtures(ctx context.Context, destination string, options FixtureOptions) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if destination == "" {
		return errors.New("voyage probe fixture destination is required")
	}
	if options.SeedDirectory == "" {
		return fmt.Errorf("voyage probe fixtures require a seed directory containing %v", SeedFixtureNames)
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("voyage probe fixture destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Voyage probe fixture destination: %w", err)
	}
	parent := filepath.Dir(destination)
	parentRoot, err := openPrivateFixtureRoot(parent, "destination parent")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parentRoot.Close()) }()
	parentIdentity, err := parentRoot.Lstat(".")
	if err != nil {
		return fmt.Errorf("inspect opened Voyage probe destination parent: %w", err)
	}
	seedRoot, err := openPrivateFixtureRoot(options.SeedDirectory, "seed directory")
	if err != nil {
		return err
	}
	defer func() {
		if seedRoot != nil {
			err = errors.Join(err, seedRoot.Close())
		}
	}()

	stagingName, stagingRoot, stagingIdentity, err := createPrivateFixtureStagingRoot(parentRoot)
	if err != nil {
		return err
	}
	published := false
	cleanupName := stagingName
	defer func() {
		if !published {
			err = errors.Join(err, removeFixtureDirectory(parentRoot, cleanupName, stagingIdentity))
		}
	}()
	defer func() {
		if stagingRoot != nil {
			err = errors.Join(err, stagingRoot.Close())
		}
	}()
	for _, spec := range fixtureSpecs {
		if err := ctx.Err(); err != nil {
			return err
		}
		var data []byte
		if spec.seed {
			seed, err := readFixtureFile(seedRoot, spec.name, media.DefaultPolicy().MaxBytes)
			if err != nil {
				if errors.Is(err, media.ErrTooLarge) {
					return fixtureEligibilityError(spec, media.ReasonTooLarge)
				}
				return fmt.Errorf("read Voyage probe seed %s: %w", spec.name, err)
			}
			data = seed
		} else {
			data = spec.generate()
		}
		if err := checkFixtureBytes(spec, data, media.DefaultPolicy()); err != nil {
			return err
		}
		if err := writeFixtureFile(stagingRoot, spec.name, data); err != nil {
			return fmt.Errorf("write Voyage probe fixture %s: %w", spec.name, err)
		}
	}
	stagingCloseErr := stagingRoot.Close()
	stagingRoot = nil
	if stagingCloseErr != nil {
		return fmt.Errorf("close Voyage probe fixture staging directory: %w", stagingCloseErr)
	}
	seedCloseErr := seedRoot.Close()
	seedRoot = nil
	if seedCloseErr != nil {
		return fmt.Errorf("close Voyage probe seed directory: %w", seedCloseErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	destinationName := filepath.Base(destination)
	if err := verifyFixtureRootPath(parentRoot, parentIdentity, "destination parent"); err != nil {
		return err
	}
	if err := publishFixtureDirectory(parentRoot, stagingName, destinationName, parentIdentity, stagingIdentity); err != nil {
		return err
	}
	cleanupName = destinationName
	if err := verifyFixtureRootPath(parentRoot, parentIdentity, "destination parent"); err != nil {
		return err
	}
	published = true
	return nil
}

// ProbeFixtureConfig locates a written fixture set.
type ProbeFixtureConfig struct {
	FixtureDirectory string
}

type probeFixtures map[string][]byte

// ValidateProbeFixtures verifies a complete fixture set locally: every file
// is present, detects as its expected format, matches its deterministic
// generation where applicable, and is eligible under the policy media
// bounds. It performs no network access.
func ValidateProbeFixtures(ctx context.Context, policy Policy, config ProbeFixtureConfig) error {
	_, err := loadProbeFixtures(ctx, policy, config)
	return err
}

func loadProbeFixtures(ctx context.Context, policy Policy, config ProbeFixtureConfig) (fixtures probeFixtures, err error) {
	if !policy.valid() {
		return nil, errors.New("voyage policy is invalid; use NewPolicy")
	}
	if config.FixtureDirectory == "" {
		return nil, errors.New("voyage probe fixture directory is required")
	}
	fixtureRoot, err := openPrivateFixtureRoot(config.FixtureDirectory, "fixture directory")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, fixtureRoot.Close()) }()
	fixtures = make(probeFixtures, len(fixtureSpecs))
	for _, spec := range fixtureSpecs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := readFixtureFile(fixtureRoot, spec.name, policy.values.Media.MaxBytes)
		if err != nil {
			if errors.Is(err, media.ErrTooLarge) {
				return nil, fixtureEligibilityError(spec, media.ReasonTooLarge)
			}
			return nil, fmt.Errorf("read Voyage probe fixture %s: %w", spec.name, err)
		}
		if err := checkFixtureBytes(spec, data, policy.values.Media); err != nil {
			return nil, err
		}
		if !spec.seed && !bytes.Equal(data, spec.generate()) {
			return nil, fmt.Errorf("voyage probe fixture %s does not match its deterministic generation", spec.name)
		}
		fixtures[spec.name] = data
	}
	return fixtures, nil
}

func openPrivateFixtureRoot(directory, purpose string) (*os.Root, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect Voyage probe %s: %w", purpose, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("voyage probe %s must be a real directory", purpose)
	}
	if err := safefileio.ValidatePrivateDir(directory); err != nil {
		return nil, fmt.Errorf("voyage probe %s must already exist and be private: %w", purpose, err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open Voyage probe %s: %w", purpose, err)
	}
	openedInfo, err := root.Lstat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("voyage probe %s changed while opening", purpose)
	}
	return root, nil
}

func createPrivateFixtureStagingRoot(parentRoot *os.Root) (name string, root *os.Root, identity os.FileInfo, err error) {
	created := false
	for range 128 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, nil, fmt.Errorf("generate Voyage probe fixture staging name: %w", err)
		}
		name = ".voyage-fixtures-" + hex.EncodeToString(random[:])
		if err := parentRoot.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", nil, nil, fmt.Errorf("create Voyage probe fixture staging directory: %w", err)
		}
		created = true
		break
	}
	if !created {
		return "", nil, nil, errors.New("create unique Voyage probe fixture staging directory")
	}
	defer func() {
		if err != nil && identity != nil {
			err = errors.Join(err, removeFixtureDirectory(parentRoot, name, identity))
		}
	}()
	identity, err = parentRoot.Lstat(name)
	if err != nil || !identity.IsDir() || identity.Mode()&os.ModeSymlink != 0 {
		return name, nil, nil, errors.Join(err, errors.New("voyage probe fixture staging directory changed after creation"))
	}
	root, err = parentRoot.OpenRoot(name)
	if err != nil {
		return name, nil, nil, fmt.Errorf("open Voyage probe fixture staging directory: %w", err)
	}
	openedIdentity, statErr := root.Lstat(".")
	if statErr != nil || !os.SameFile(identity, openedIdentity) {
		_ = root.Close()
		return name, nil, nil, errors.Join(statErr, errors.New("voyage probe fixture staging directory changed while opening"))
	}
	currentIdentity, statErr := parentRoot.Lstat(name)
	if statErr != nil || !os.SameFile(identity, currentIdentity) {
		_ = root.Close()
		return name, nil, nil, errors.Join(statErr, errors.New("voyage probe fixture staging directory changed while validating"))
	}
	return name, root, identity, nil
}

func publishFixtureDirectory(
	parentRoot *os.Root,
	stagingName, destinationName string,
	parentIdentity, stagingIdentity os.FileInfo,
) error {
	currentIdentity, err := parentRoot.Lstat(stagingName)
	if err != nil || !os.SameFile(stagingIdentity, currentIdentity) {
		return errors.Join(err, errors.New("voyage probe fixture staging directory changed before publication"))
	}
	if _, err := parentRoot.Lstat(destinationName); err == nil {
		return errors.New("voyage probe fixture destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Voyage probe fixture destination: %w", err)
	}
	if err := renameFixtureDirectoryNoReplace(
		parentRoot.Name(), stagingName, destinationName, parentIdentity, stagingIdentity,
	); err != nil {
		return fmt.Errorf("publish Voyage probe fixtures without replacing a destination: %w", err)
	}
	publishedIdentity, err := parentRoot.Lstat(destinationName)
	if err != nil || !os.SameFile(stagingIdentity, publishedIdentity) {
		return errors.Join(err, errors.New("voyage probe fixture directory changed during publication"))
	}
	return nil
}

func removeFixtureDirectory(parentRoot *os.Root, name string, identity os.FileInfo) error {
	currentIdentity, err := parentRoot.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect unpublished Voyage probe fixture directory: %w", err)
	}
	if !os.SameFile(identity, currentIdentity) {
		return errors.New("refuse to remove a replaced Voyage probe fixture directory")
	}
	if err := parentRoot.RemoveAll(name); err != nil {
		return fmt.Errorf("remove unpublished Voyage probe fixture directory: %w", err)
	}
	return nil
}

func verifyFixtureRootPath(root *os.Root, identity os.FileInfo, purpose string) error {
	pathIdentity, err := os.Lstat(root.Name())
	if err != nil || !pathIdentity.IsDir() || pathIdentity.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, pathIdentity) {
		return errors.Join(err, fmt.Errorf("voyage probe %s changed during fixture generation", purpose))
	}
	if err := safefileio.ValidatePrivateDir(root.Name()); err != nil {
		return fmt.Errorf("voyage probe %s is no longer private: %w", purpose, err)
	}
	openedIdentity, err := root.Lstat(".")
	if err != nil || !os.SameFile(identity, openedIdentity) {
		return errors.Join(err, fmt.Errorf("opened Voyage probe %s changed during fixture generation", purpose))
	}
	return nil
}

func readFixtureFile(root *os.Root, name string, limit int64) ([]byte, error) {
	pathInfo, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Size() < 0 {
		return nil, errors.New("fixture must be a bounded regular non-symlink file")
	}
	if limit < 0 || pathInfo.Size() > limit {
		return nil, media.ErrTooLarge
	}
	rootIdentity, err := root.Lstat(".")
	if err != nil {
		return nil, fmt.Errorf("inspect fixture root: %w", err)
	}
	file, err := openFixtureFileNoFollow(root.Name(), name, rootIdentity, pathInfo)
	if err != nil {
		return nil, fmt.Errorf("open fixture without following links: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidateCurrentUserFile(file); err != nil {
		return nil, fmt.Errorf("validate fixture file owner: %w", err)
	}
	openedInfo, statErr := file.Stat()
	currentInfo, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, openedInfo) || !os.SameFile(currentInfo, openedInfo) {
		return nil, errors.New("fixture changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, media.ErrTooLarge
	}
	finalInfo, statErr := file.Stat()
	currentInfo, pathErr = root.Lstat(name)
	if statErr != nil || pathErr != nil || int64(len(data)) != finalInfo.Size() ||
		!os.SameFile(openedInfo, finalInfo) || !os.SameFile(currentInfo, finalInfo) {
		return nil, errors.New("fixture changed while reading")
	}
	return data, nil
}

func writeFixtureFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := safefileio.ValidateCurrentUserFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("validate fixture file owner: %w", err)
	}
	written, writeErr := file.Write(data)
	syncErr := file.Sync()
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	pathInfo, pathErr := root.Lstat(name)
	if writeErr != nil || syncErr != nil || statErr != nil || closeErr != nil || pathErr != nil ||
		written != len(data) || openedInfo.Size() != int64(len(data)) || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(openedInfo, pathInfo) {
		return errors.Join(writeErr, syncErr, statErr, closeErr, pathErr, errors.New("fixture write was incomplete or changed identity"))
	}
	return nil
}

func fixtureEligibilityError(spec fixtureSpec, reason media.Reason) error {
	return fmt.Errorf("voyage probe fixture %s is %s under the policy media bounds", spec.name, reason)
}

func checkFixtureBytes(spec fixtureSpec, data []byte, policy media.Policy) error {
	policy.AllowStill, policy.AllowAnimated, policy.AllowVideo = true, true, true
	metadata, reason := media.InspectBytes(data, "", policy)
	if reason != media.ReasonEligible {
		return fixtureEligibilityError(spec, reason)
	}
	if metadata.Format != spec.format || metadata.Animated != spec.animated {
		return fmt.Errorf("voyage probe fixture %s must be %s (animated=%t), detected %s (animated=%t)",
			spec.name, spec.format, spec.animated, metadata.Format, metadata.Animated)
	}
	return nil
}

func (f probeFixtures) media(name string) (*Media, error) {
	data, ok := f[name]
	if !ok {
		return nil, fmt.Errorf("voyage probe fixture %s is missing", name)
	}
	metadata, err := media.DetectBytes(data, "")
	if err != nil {
		return nil, fmt.Errorf("voyage probe fixture %s: %w", name, err)
	}
	return &Media{Metadata: metadata, Bytes: data}, nil
}
