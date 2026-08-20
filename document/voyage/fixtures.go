package voyage

import (
	"bytes"
	"context"
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

	// FixtureJPEGAlt and the other variant fixtures contrast their primary:
	// each interleaved probe swaps its media between a fixture and its
	// variant so pixel contribution is demonstrated within the format, never
	// across formats.
	FixtureJPEGAlt        = "image_jpeg_alt.jpg"
	FixtureWebPAlt        = "image_webp_alt.webp"
	FixtureGIFStillAlt    = "image_gif_still_alt.gif"
	FixtureGIFAnimatedAlt = "image_gif_animated_alt.gif"
	FixtureMP4Alt         = "video_mp4_alt.mp4"

	FixtureRed  = "probe_red.png"
	FixtureBlue = "probe_blue.png"

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
var SeedFixtureNames = []string{FixtureWebP, FixtureWebPAlt, FixtureMP4, FixtureMP4Alt}

// fixtureVariants pairs each interleaved probe's primary fixture with its
// contrasting same-format variant. PNG reuses the red and blue references.
var fixtureVariants = map[string]string{
	FixtureJPEG:        FixtureJPEGAlt,
	FixturePNG:         FixtureRed,
	FixtureWebP:        FixtureWebPAlt,
	FixtureGIFStill:    FixtureGIFStillAlt,
	FixtureGIFAnimated: FixtureGIFAnimatedAlt,
	FixtureMP4:         FixtureMP4Alt,
}

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
	{name: FixtureJPEGAlt, format: media.FormatJPEG, generate: func() []byte { return mediatest.JPEG(fixtureSide, fixtureSide, fixtureBlue) }},
	{name: FixtureWebPAlt, format: media.FormatWebP, seed: true},
	{name: FixtureGIFStillAlt, format: media.FormatGIF, generate: func() []byte { return mediatest.GIFShifted(fixtureSide, fixtureSide, 1, 1) }},
	{name: FixtureGIFAnimatedAlt, format: media.FormatGIF, animated: true, generate: func() []byte {
		// Contrast by frame count rather than frame order, so a provider
		// that aggregates frames without encoding their order still
		// distinguishes the variant.
		return mediatest.GIF(fixtureSide, fixtureSide, animatedFrames-1)
	}},
	{name: FixtureMP4Alt, format: media.FormatMP4, seed: true},
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
	if err := safefileio.ValidatePrivateDir(parent); err != nil {
		return fmt.Errorf("voyage probe destination parent must already exist and be private: %w", err)
	}
	seedRoot, err := openPrivateFixtureRoot(options.SeedDirectory, "seed directory")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, seedRoot.Close()) }()

	staging, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+"-tmp-*")
	if err != nil {
		return fmt.Errorf("create Voyage probe fixture staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(staging))
		}
	}()
	if err := safefileio.EnsurePrivateDir(staging); err != nil {
		return fmt.Errorf("secure Voyage probe fixture staging directory: %w", err)
	}
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
		if err := writeFixtureFile(staging, spec.name, data); err != nil {
			return fmt.Errorf("write Voyage probe fixture %s: %w", spec.name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("voyage probe fixture destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Voyage probe fixture destination: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("publish Voyage probe fixtures: %w", err)
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
	if err := validateFixtureVariants(fixtures); err != nil {
		return nil, err
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

func readFixtureFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return nil, errors.New("fixture must be a bounded regular non-symlink file")
	}
	if limit < 0 || info.Size() > limit {
		return nil, media.ErrTooLarge
	}
	file, err := safefileio.OpenCurrentUserFile(filepath.Join(root.Name(), name))
	if err != nil {
		return nil, fmt.Errorf("open fixture without following links: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, errors.New("fixture changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, media.ErrTooLarge
	}
	return data, nil
}

// writeFixtureFile creates one fixture inside the freshly created private
// staging directory; holding no directory handle keeps the later publish
// rename possible on Windows.
func writeFixtureFile(directory, name string, data []byte) error {
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- name is a fixed fixture name under our own staging directory.
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Close())
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

// validateFixtureVariants confirms every contrasting variant actually
// contrasts: identical bytes would let a pixel-blind provider pass the
// interleaved media-swap check trivially.
func validateFixtureVariants(fixtures probeFixtures) error {
	for primary, variant := range fixtureVariants {
		if bytes.Equal(fixtures[primary], fixtures[variant]) {
			return fmt.Errorf("voyage probe fixture %s must differ from %s", variant, primary)
		}
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
