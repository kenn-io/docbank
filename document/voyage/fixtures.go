package voyage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
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
	FixtureQueryImage  = "query_image.png"

	// ProbeQueryText is the text query the probe ranks against the red and
	// blue reference documents.
	ProbeQueryText = "a solid red square"
	// ProbeInterleavedText is the text part of the interleaved probe document.
	ProbeInterleavedText = "a solid red square"

	fixtureSide      = 64
	queryFixtureSide = 32
	animatedFrames   = 4
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
	{name: FixtureQueryImage, format: media.FormatPNG, generate: func() []byte { return mediatest.PNG(queryFixtureSide, queryFixtureSide, fixtureRed) }},
}

// FixtureOptions controls probe fixture generation.
type FixtureOptions struct {
	// SeedDirectory holds operator-supplied synthetic WebP and MP4 seeds
	// named as in SeedFixtureNames. Required.
	SeedDirectory string
}

// WriteProbeFixtures writes the deterministic fixture set into destination.
// Generated fixtures are byte-identical across runs; seed fixtures are copied
// after their format is verified.
func WriteProbeFixtures(ctx context.Context, destination string, options FixtureOptions) error {
	if destination == "" {
		return errors.New("voyage probe fixture destination is required")
	}
	if options.SeedDirectory == "" {
		return fmt.Errorf("voyage probe fixtures require a seed directory containing %v", SeedFixtureNames)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create Voyage probe fixture directory: %w", err)
	}
	for _, spec := range fixtureSpecs {
		if err := ctx.Err(); err != nil {
			return err
		}
		var data []byte
		if spec.seed {
			seed, err := os.ReadFile(filepath.Join(options.SeedDirectory, spec.name))
			if err != nil {
				return fmt.Errorf("read Voyage probe seed %s: %w", spec.name, err)
			}
			data = seed
		} else {
			data = spec.generate()
		}
		if err := checkFixtureBytes(spec, data, media.DefaultPolicy()); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, spec.name), data, 0o600); err != nil { // #nosec G703 -- fixed fixture names under an operator-selected directory.
			return fmt.Errorf("write Voyage probe fixture %s: %w", spec.name, err)
		}
	}
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

func loadProbeFixtures(ctx context.Context, policy Policy, config ProbeFixtureConfig) (probeFixtures, error) {
	if !policy.valid() {
		return nil, errors.New("voyage policy is invalid; use NewPolicy")
	}
	if config.FixtureDirectory == "" {
		return nil, errors.New("voyage probe fixture directory is required")
	}
	fixtures := make(probeFixtures, len(fixtureSpecs))
	for _, spec := range fixtureSpecs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(config.FixtureDirectory, spec.name))
		if err != nil {
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

func checkFixtureBytes(spec fixtureSpec, data []byte, policy media.Policy) error {
	policy.AllowStill, policy.AllowAnimated, policy.AllowVideo = true, true, true
	metadata, reason := media.InspectBytes(data, "", policy)
	if reason != media.ReasonEligible {
		return fmt.Errorf("voyage probe fixture %s is %s under the policy media bounds", spec.name, reason)
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
