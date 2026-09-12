package pagerender

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrUnavailable   = errors.New("page renderer unavailable: configure the pinned Linux amd64 page runtime")
	ErrUnsupported   = errors.New("page source geometry or density is unsupported")
	ErrInvalidOutput = errors.New("page renderer output failed verification")
)

type Executable struct {
	Path   string `toml:"path"`
	SHA256 string `toml:"sha256"`
}
type Profile struct {
	Inspector          Executable `toml:"inspector"`
	Renderer           Executable `toml:"renderer"`
	Limiter            Executable `toml:"limiter"`
	DeploymentIdentity string     `toml:"deployment_identity"`
}

// Validate checks configuration syntax without opening executables or sources.
func (p Profile) Validate() error {
	if p.DeploymentIdentity == "" || len(p.DeploymentIdentity) > 512 || strings.TrimSpace(p.DeploymentIdentity) != p.DeploymentIdentity {
		return errors.New("page runtime deployment identity is required")
	}
	for _, e := range []Executable{p.Inspector, p.Renderer, p.Limiter} {
		if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path || !canonical.IsSHA256Hex(e.SHA256) || providerutil.IsPythonInterpreter(e.Path) {
			return errors.New("page runtime requires absolute compiled executable paths and SHA-256 pins")
		}
	}
	return nil
}

// Runtime owns immutable verified executable bytes and an observed recipe.
// Dynamic libraries, fonts and color resources remain operator-managed.
type Runtime struct {
	inspector, renderer, limiter *providerutil.PinnedExecutable
	identity                     document.PageRuntimeIdentity
	rendererVersion              string
	active                       chan struct{}
}

func SupportedPlatform() bool { return runtime.GOOS == "linux" && runtime.GOARCH == "amd64" }

// Fingerprint identifies the complete configured and observed runtime apart
// from per-request DPI. Empty means the optional runtime is unavailable.
func (e *Runtime) Fingerprint() string {
	if e == nil {
		return ""
	}
	encoded, err := canonical.Marshal(e.identity)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func New(ctx context.Context, p Profile) (*Runtime, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if !SupportedPlatform() {
		return nil, ErrUnavailable
	}
	load := func(e Executable) (*providerutil.PinnedExecutable, error) {
		if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path || providerutil.IsPythonInterpreter(e.Path) {
			return nil, ErrUnavailable
		}
		return providerutil.LoadPinnedExecutable(e.Path, e.SHA256, 512<<20)
	}
	engine := &Runtime{active: make(chan struct{}, 1)}
	var err error
	engine.inspector, err = load(p.Inspector)
	if err != nil {
		return nil, fmt.Errorf("inspector: %w", ErrUnavailable)
	}
	engine.renderer, err = load(p.Renderer)
	if err != nil {
		return nil, fmt.Errorf("renderer: %w", ErrUnavailable)
	}
	engine.limiter, err = load(p.Limiter)
	if err != nil {
		return nil, fmt.Errorf("limiter: %w", ErrUnavailable)
	}
	version := func(exe *providerutil.PinnedExecutable, memory int64, arg string) (string, error) {
		out, diagnostics, err := engine.command(ctx, exe, nil, memory, 64<<10, arg)
		if err != nil {
			return "", err
		}
		text := strings.TrimSpace(string(append(out, diagnostics...)))
		line, _, _ := strings.Cut(text, "\n")
		if len(line) == 0 || len(line) > 512 {
			return "", ErrUnavailable
		}
		return line, nil
	}
	inspectorVersion, err := version(engine.inspector, 2<<30, "--version")
	if err != nil || !strings.HasPrefix(inspectorVersion, Protocol+" ") {
		return nil, ErrUnavailable
	}
	limiterVersion, err := version(engine.limiter, 512<<20, "--version")
	if err != nil || !strings.HasPrefix(limiterVersion, "prlimit from util-linux ") {
		return nil, ErrUnavailable
	}
	engine.rendererVersion, err = version(engine.renderer, 512<<20, "-v")
	if err != nil || !strings.HasPrefix(engine.rendererVersion, "pdftoppm version ") {
		return nil, ErrUnavailable
	}
	engine.identity = document.PageRuntimeIdentity{InspectorSHA256: p.Inspector.SHA256, InspectorVersion: inspectorVersion, RendererSHA256: p.Renderer.SHA256, LimiterSHA256: p.Limiter.SHA256, LimiterVersion: limiterVersion, DeploymentIdentity: p.DeploymentIdentity, Platform: "linux/amd64", MemoryEnforcement: "rlimit-as", InspectorMemoryBytes: 2 << 30, RendererMemoryBytes: 512 << 20, MaxSourceBytes: document.MaxPageSourceBytes, MaxPages: document.MaxDocumentPages, MaxOutputBytes: document.MaxPageImageBytes, MaxPixels: document.MaxPagePixels, MaxAxis: document.MaxPageAxis, MaxGeometryBytes: 16 << 20, MaxDiagnosticBytes: 64 << 10, PhaseSeconds: 60}
	recipe := engine.pdfRecipe(144)
	if err := document.ValidatePageRecipeV1(recipe); err != nil {
		return nil, err
	}
	return engine, nil
}

func (e *Runtime) acquire(ctx context.Context) (func(), error) {
	if e == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case e.active <- struct{}{}:
		return func() { <-e.active }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Runtime) command(ctx context.Context, pinned *providerutil.PinnedExecutable, source []byte, memory, limit int64, args ...string) (output, diagnostics []byte, result error) {
	phase, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	executable, cleanup, err := pinned.Materialize()
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	limiter, cleanupLimiter, err := e.limiter.Materialize()
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	defer func() { result = errors.Join(result, cleanupLimiter()) }()
	arguments := append([]string{"--as=" + strconv.FormatInt(memory, 10) + ":" + strconv.FormatInt(memory, 10), "--", executable}, args...)
	command := exec.CommandContext(phase, limiter, arguments...) //nolint:gosec // Both executable images are operator-pinned; source is stdin only.
	command.Dir = filepath.Dir(executable)
	command.Env = []string{"LANG=C", "LC_ALL=C", "TZ=UTC"}
	command.Stdin = bytes.NewReader(source)
	command.WaitDelay = 250 * time.Millisecond
	var managed *providerutil.ManagedCommand
	kill := func() {
		if managed != nil {
			_ = managed.Kill()
		}
	}
	stdout := providerutil.NewBoundedBuffer(limit, kill)
	stderr := providerutil.NewBoundedBuffer(64<<10, kill)
	command.Stdout = stdout
	command.Stderr = stderr
	managed, err = providerutil.NewManagedCommand(command)
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	err = managed.Run()
	if phase.Err() != nil {
		return nil, nil, phase.Err()
	}
	if stdout.Exceeded() || stderr.Exceeded() {
		return nil, nil, ErrInvalidOutput
	}
	if err != nil {
		return nil, nil, ErrUnsupported
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func (e *Runtime) Inspect(ctx context.Context, data []byte, source document.PageSource, mediaType string) ([]document.PageFrameV1, error) {
	release, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := verifySource(data, source); err != nil {
		return nil, err
	}
	if mediaType != "application/pdf" && mediaType != "image/png" {
		return nil, ErrUnsupported
	}
	out, diag, err := e.command(ctx, e.inspector, data, 2<<30, 16<<20, "--protocol", Protocol, "--version-id", source.VersionID, "--media-type", mediaType)
	if err != nil {
		return nil, err
	}
	if len(diag) != 0 {
		return nil, ErrInvalidOutput
	}
	value, err := canonical.Decode[Inspection](out)
	if err != nil || value.Contract != Protocol || !value.Complete || value.Source != source || value.PageCount < 1 || value.PageCount > document.MaxDocumentPages || len(value.Frames) != value.PageCount {
		return nil, ErrInvalidOutput
	}
	for index, frame := range value.Frames {
		if frame.Source != source || frame.Page != index+1 || document.ValidatePageFrameV1(frame) != nil || mediaType == "application/pdf" && frame.InputUnits != "point/10000" || mediaType == "image/png" && (frame.InputUnits != "pixel" || value.PageCount != 1) {
			return nil, ErrInvalidOutput
		}
	}
	return value.Frames, nil
}

func (e *Runtime) pdfRecipe(dpi float64) document.PageRecipeV1 {
	identity := e.identity
	return document.PageRecipeV1{Contract: document.PageImageContractV1, DPI: dpi, Format: "png", RendererIdentity: document.PageRendererIdentity{Executable: "pdftoppm", Version: e.rendererVersion, Options: []string{"-singlefile", "-cropbox", "-png", "-aa", "yes", "-aaVector", "yes", "-thinlinemode", "none", "-freetype", "yes", "annotations=include", "color=poppler-default", "stdin=pdf", "stdout=png"}, Runtime: &identity}}
}

func (e *Runtime) Recipe(frame document.PageFrameV1, dpi float64) (document.PageRecipeV1, error) {
	if e == nil {
		return document.PageRecipeV1{}, ErrUnavailable
	}
	if err := document.ValidatePageFrameV1(frame); err != nil {
		return document.PageRecipeV1{}, err
	}
	recipe := e.pdfRecipe(dpi)
	if frame.InputUnits == "pixel" {
		native := float64(frame.PixelsPerMetreX*127) / 5000
		if dpi != 0 && dpi != native {
			return document.PageRecipeV1{}, ErrUnsupported
		}
		recipe.DPI = native
		recipe.RendererIdentity.Executable = "docbank-png-original"
		recipe.RendererIdentity.Version = Protocol
		recipe.RendererIdentity.Options = []string{"original-bytes", "isotropic-pHYs-metres", "orientation=absent", "animation=absent"}
	} else {
		if dpi == 0 {
			recipe.DPI = 144
		}
		if recipe.DPI < 1 || recipe.DPI > 1200 {
			return document.PageRecipeV1{}, ErrUnsupported
		}
	}
	if _, _, err := document.PageImageDimensions(frame, recipe); err != nil {
		return document.PageRecipeV1{}, err
	}
	return recipe, nil
}

func (e *Runtime) Render(ctx context.Context, data []byte, frame document.PageFrameV1, recipe document.PageRecipeV1) (document.PageImageV1, []byte, error) {
	release, err := e.acquire(ctx)
	if err != nil {
		return document.PageImageV1{}, nil, err
	}
	defer release()
	if err := verifySource(data, frame.Source); err != nil {
		return document.PageImageV1{}, nil, err
	}
	actual, err := e.Recipe(frame, recipe.DPI)
	if err != nil {
		return document.PageImageV1{}, nil, err
	}
	actualJSON, _, err := document.MarshalPageRecipeV1(actual)
	if err != nil {
		return document.PageImageV1{}, nil, err
	}
	requestedJSON, recipeHash, err := document.MarshalPageRecipeV1(recipe)
	if err != nil || !bytes.Equal(actualJSON, requestedJSON) {
		return document.PageImageV1{}, nil, ErrUnavailable
	}
	width, height, err := document.PageImageDimensions(frame, recipe)
	if err != nil {
		return document.PageImageV1{}, nil, err
	}
	var output []byte
	if frame.InputUnits == "pixel" {
		output = bytes.Clone(data)
	} else {
		options := []string{"-f", strconv.Itoa(frame.Page), "-l", strconv.Itoa(frame.Page), "-singlefile", "-r", strconv.FormatFloat(recipe.DPI, 'f', -1, 64), "-cropbox", "-png", "-aa", "yes", "-aaVector", "yes", "-thinlinemode", "none", "-freetype", "yes", "-"}
		var diagnostics []byte
		output, diagnostics, err = e.command(ctx, e.renderer, data, 512<<20, document.MaxPageImageBytes, options...)
		if err != nil {
			return document.PageImageV1{}, nil, err
		}
		if len(diagnostics) != 0 {
			return document.PageImageV1{}, nil, ErrInvalidOutput
		}
	}
	w, h, _, _, err := inspectPNG(output, false)
	if err != nil || w != width || h != height {
		return document.PageImageV1{}, nil, ErrInvalidOutput
	}
	_, frameHash, err := document.MarshalPageFrameV1(frame)
	if err != nil {
		return document.PageImageV1{}, nil, err
	}
	hash := sha256.Sum256(output)
	receipt := document.PageImageV1{Contract: document.PageImageContractV1, Source: frame.Source, Page: frame.Page, FrameSHA256: frameHash, RecipeSHA256: recipeHash, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(output)), Width: w, Height: h}
	if err := document.ValidatePageImageBinding(receipt, frame, recipe); err != nil {
		return document.PageImageV1{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return document.PageImageV1{}, nil, err
	}
	return receipt, output, nil
}
