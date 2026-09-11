package document

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

// PageRuntimeIdentity records executable observations and an operator-declared
// dependency identity. It does not claim hermetic library/font verification.
type PageRuntimeIdentity struct {
	InspectorSHA256      string `json:"inspector_sha256"`
	InspectorVersion     string `json:"inspector_version"`
	RendererSHA256       string `json:"renderer_sha256"`
	LimiterSHA256        string `json:"limiter_sha256"`
	LimiterVersion       string `json:"limiter_version"`
	DeploymentIdentity   string `json:"deployment_identity"`
	Platform             string `json:"platform"`
	MemoryEnforcement    string `json:"memory_enforcement"`
	InspectorMemoryBytes int64  `json:"inspector_memory_bytes"`
	RendererMemoryBytes  int64  `json:"renderer_memory_bytes"`
	MaxSourceBytes       int64  `json:"max_source_bytes"`
	MaxPages             int    `json:"max_pages"`
	MaxOutputBytes       int64  `json:"max_output_bytes"`
	MaxPixels            int64  `json:"max_pixels"`
	MaxAxis              int64  `json:"max_axis"`
	MaxGeometryBytes     int64  `json:"max_geometry_bytes"`
	MaxDiagnosticBytes   int64  `json:"max_diagnostic_bytes"`
	PhaseSeconds         int64  `json:"phase_seconds"`
}

type PageRendererIdentity struct {
	Executable string               `json:"executable"`
	Version    string               `json:"version"`
	Options    []string             `json:"options"`
	Runtime    *PageRuntimeIdentity `json:"runtime,omitempty"`
}

// PageRecipeV1 keeps the G7 four-field identity, including numeric actual DPI.
type PageRecipeV1 struct {
	Contract         string               `json:"contract"`
	DPI              float64              `json:"dpi"`
	Format           string               `json:"format"`
	RendererIdentity PageRendererIdentity `json:"renderer_identity"`
}

type PageImageV1 struct {
	Contract     string     `json:"contract"`
	Source       PageSource `json:"source"`
	Page         int        `json:"page"`
	FrameSHA256  string     `json:"frame_sha256"`
	RecipeSHA256 string     `json:"recipe_sha256"`
	SHA256       string     `json:"sha256"`
	Size         int64      `json:"size"`
	Width        int64      `json:"width"`
	Height       int64      `json:"height"`
}

func pageText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func ValidatePageRecipeV1(r PageRecipeV1) error {
	if r.Contract != PageImageContractV1 || r.Format != "png" || math.IsNaN(r.DPI) || math.IsInf(r.DPI, 0) || r.DPI <= 0 || r.DPI > 1_000_000 || !pageText(r.RendererIdentity.Executable, 128) || !pageText(r.RendererIdentity.Version, 512) || len(r.RendererIdentity.Options) < 1 || len(r.RendererIdentity.Options) > 64 {
		return errors.New("invalid page image recipe")
	}
	for _, v := range r.RendererIdentity.Options {
		if !pageText(v, 512) {
			return errors.New("invalid renderer option")
		}
	}
	if p := r.RendererIdentity.Runtime; p != nil {
		if !canonical.IsSHA256Hex(p.InspectorSHA256) || !canonical.IsSHA256Hex(p.RendererSHA256) || !canonical.IsSHA256Hex(p.LimiterSHA256) || !pageText(p.InspectorVersion, 512) || !pageText(p.LimiterVersion, 512) || !pageText(p.DeploymentIdentity, 512) || p.Platform != "linux/amd64" || p.MemoryEnforcement != "rlimit-as" || p.InspectorMemoryBytes != 2<<30 || p.RendererMemoryBytes != 512<<20 || p.MaxSourceBytes != MaxPageSourceBytes || p.MaxPages != MaxDocumentPages || p.MaxOutputBytes != MaxPageImageBytes || p.MaxPixels != MaxPagePixels || p.MaxAxis != MaxPageAxis || p.MaxGeometryBytes != 16<<20 || p.MaxDiagnosticBytes != 64<<10 || p.PhaseSeconds != 60 {
			return errors.New("invalid page runtime limits or identity")
		}
	}
	return nil
}

func MarshalPageRecipeV1(r PageRecipeV1) ([]byte, string, error) {
	if err := ValidatePageRecipeV1(r); err != nil {
		return nil, "", err
	}
	b, err := canonical.Marshal(r)
	return b, sha256Hex(b), err
}
func DecodePageRecipeV1(b []byte) (PageRecipeV1, string, error) {
	r, err := canonical.Decode[PageRecipeV1](b)
	if err != nil {
		return r, "", err
	}
	_, hash, err := MarshalPageRecipeV1(r)
	return r, hash, err
}

func ValidatePageImageV1(i PageImageV1) error {
	if err := i.Source.Validate(); err != nil {
		return err
	}
	if i.Contract != PageImageContractV1 || i.Page < 1 || i.Page > MaxDocumentPages || !canonical.IsSHA256Hex(i.FrameSHA256) || !canonical.IsSHA256Hex(i.RecipeSHA256) || !canonical.IsSHA256Hex(i.SHA256) || i.Size < 1 || i.Size > MaxPageImageBytes || i.Width < 1 || i.Height < 1 || i.Width > MaxPageAxis || i.Height > MaxPageAxis || i.Width*i.Height > MaxPagePixels {
		return errors.New("invalid page image receipt")
	}
	return nil
}
func MarshalPageImageV1(i PageImageV1) ([]byte, string, error) {
	if err := ValidatePageImageV1(i); err != nil {
		return nil, "", err
	}
	b, err := canonical.Marshal(i)
	return b, sha256Hex(b), err
}
func DecodePageImageV1(b []byte) (PageImageV1, string, error) {
	i, err := canonical.Decode[PageImageV1](b)
	if err != nil {
		return i, "", err
	}
	_, hash, err := MarshalPageImageV1(i)
	return i, hash, err
}

// PageImageDimensions derives expected raster dimensions without allocating
// image memory. PNG native density must match the returned numeric recipe.
func PageImageDimensions(f PageFrameV1, r PageRecipeV1) (int64, int64, error) {
	if err := ValidatePageFrameV1(f); err != nil {
		return 0, 0, err
	}
	if err := ValidatePageRecipeV1(r); err != nil {
		return 0, 0, err
	}
	if f.InputUnits == "pixel" {
		if r.DPI != float64(f.PixelsPerMetreX*127)/5000 {
			return 0, 0, errors.New("requested DPI differs from native PNG density")
		}
		return f.PixelWidth, f.PixelHeight, nil
	}
	dpi, ok := new(big.Rat).SetString(strconv.FormatFloat(r.DPI, 'f', -1, 64))
	if !ok {
		return 0, 0, errors.New("invalid DPI")
	}
	width, height := f.CropBox[2]-f.CropBox[0], f.CropBox[3]-f.CropBox[1]
	if f.Rotation == 90 || f.Rotation == 270 {
		width, height = height, width
	}
	pixel := func(points int64) (int64, error) {
		q := new(big.Rat).Mul(new(big.Rat).SetFrac64(points, 720000), dpi)
		n, rem := new(big.Int), new(big.Int)
		n.QuoRem(q.Num(), q.Denom(), rem)
		if rem.Sign() > 0 {
			n.Add(n, big.NewInt(1))
		}
		if !n.IsInt64() || n.Int64() < 1 || n.Int64() > MaxPageAxis {
			return 0, errors.New("page axis exceeds pixel limit")
		}
		return n.Int64(), nil
	}
	w, err := pixel(width)
	if err != nil {
		return 0, 0, err
	}
	h, err := pixel(height)
	if err != nil {
		return 0, 0, err
	}
	if w*h > MaxPagePixels {
		return 0, 0, errors.New("page exceeds pixel limit")
	}
	return w, h, nil
}

func ValidatePageImageBinding(i PageImageV1, f PageFrameV1, r PageRecipeV1) error {
	if err := ValidatePageImageV1(i); err != nil {
		return err
	}
	_, fh, err := MarshalPageFrameV1(f)
	if err != nil {
		return err
	}
	_, rh, err := MarshalPageRecipeV1(r)
	if err != nil {
		return err
	}
	if i.Source != f.Source || i.Page != f.Page || i.FrameSHA256 != fh || i.RecipeSHA256 != rh {
		return errors.New("page image source/frame/recipe mismatch")
	}
	w, h, err := PageImageDimensions(f, r)
	if err != nil {
		return err
	}
	if i.Width != w || i.Height != h {
		return errors.New("page image dimensions disagree with physical frame")
	}
	return nil
}
