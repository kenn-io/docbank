package document

import (
	"errors"
	"math"
	"math/big"

	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	PageFrameContractV1       = "page-frame-v1"
	PageImageContractV1       = "page-image-v1"
	MaxPageSourceBytes  int64 = 64 << 20
	MaxDocumentPages          = 1000
	MaxRenderPages            = 16
	MaxPagePixels       int64 = 40_000_000
	MaxPageAxis               = 16384
	MaxPageImageBytes   int64 = 32 << 20
	MaxPageJobBytes     int64 = 256 << 20
	MaxPageInteger      int64 = 9007199254740991
)

// PageSource binds authority to one retained version, never the current head.
type PageSource struct {
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

func (s PageSource) Validate() error {
	id, err := uuid.Parse(s.VersionID)
	if err != nil || id.String() != s.VersionID || id.Version() != 4 || !canonical.IsSHA256Hex(s.SHA256) || s.Size < 1 || s.Size > MaxPageSourceBytes {
		return errors.New("invalid page source identity")
	}
	return nil
}

// PageRational is reduced, with a positive denominator and JS-safe integers.
type PageRational struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

// PageFrameV1 maps (x,y) to (a*x+c*y+e,b*x+d*y+f), with top-left
// output origin, x right and y down. PDF inputs are 1/10,000 points; PNG
// inputs are pixel coordinates. Output coordinates are 1/10,000 inches.
type PageFrameV1 struct {
	Contract        string          `json:"contract"`
	Source          PageSource      `json:"source"`
	Page            int             `json:"page"`
	MediaBox        [4]int64        `json:"media_box"`
	CropBox         [4]int64        `json:"crop_box"`
	Rotation        int             `json:"rotation"`
	Width           int64           `json:"width"`
	Height          int64           `json:"height"`
	InputUnits      string          `json:"input_units"`
	OutputUnits     string          `json:"output_units"`
	Axes            string          `json:"axes"`
	Transform       [6]PageRational `json:"transform"`
	PixelWidth      int64           `json:"pixel_width,omitzero"`
	PixelHeight     int64           `json:"pixel_height,omitzero"`
	PixelsPerMetreX int64           `json:"pixels_per_metre_x,omitzero"`
	PixelsPerMetreY int64           `json:"pixels_per_metre_y,omitzero"`
}

func pageRat(n, d int64) (PageRational, error) {
	if d <= 0 {
		return PageRational{}, errors.New("invalid page rational denominator")
	}
	r := new(big.Rat).SetFrac(big.NewInt(n), big.NewInt(d))
	if !r.Num().IsInt64() || !r.Denom().IsInt64() || r.Num().Cmp(big.NewInt(-MaxPageInteger)) < 0 || r.Num().Cmp(big.NewInt(MaxPageInteger)) > 0 || r.Denom().Cmp(big.NewInt(MaxPageInteger)) > 0 {
		return PageRational{}, errors.New("page rational exceeds precision bounds")
	}
	return PageRational{r.Num().Int64(), r.Denom().Int64()}, nil
}

// PageRoundRatio rounds a positive physical ratio, half away from zero.
func PageRoundRatio(n, d int64) (int64, error) {
	if n < 0 || d <= 0 {
		return 0, errors.New("invalid physical ratio")
	}
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(big.NewInt(n), big.NewInt(d), r)
	if r.Lsh(r, 1).Cmp(big.NewInt(d)) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Sign() <= 0 || q.Cmp(big.NewInt(MaxPageInteger)) > 0 {
		return 0, errors.New("physical dimension unavailable")
	}
	return q.Int64(), nil
}

// NewPDFPageFrame quantizes finite PDF point coordinates half away from zero.
func NewPDFPageFrame(source PageSource, page int, media, crop [4]float64, rotation int) (PageFrameV1, error) {
	f := PageFrameV1{Contract: PageFrameContractV1, Source: source, Page: page, Rotation: rotation, InputUnits: "point/10000", OutputUnits: "inch/10000", Axes: "top-left,x-right,y-down"}
	for i := range 4 {
		for _, pair := range []struct {
			value       float64
			destination *int64
		}{{media[i], &f.MediaBox[i]}, {crop[i], &f.CropBox[i]}} {
			scaled := math.Round(pair.value * 10000)
			if math.IsNaN(scaled) || math.IsInf(scaled, 0) || math.Abs(scaled) > float64(MaxPageInteger) {
				return PageFrameV1{}, errors.New("PDF coordinate is not bounded finite geometry")
			}
			*pair.destination = int64(scaled)
		}
	}
	return derivePageFrame(f)
}

// NewPNGPageFrame preserves declared metre density; anisotropic images are
// unavailable until a compatible per-axis recipe is supported.
func NewPNGPageFrame(source PageSource, width, height, ppmX, ppmY int64) (PageFrameV1, error) {
	f := PageFrameV1{Contract: PageFrameContractV1, Source: source, Page: 1, InputUnits: "pixel", OutputUnits: "inch/10000", Axes: "top-left,x-right,y-down", PixelWidth: width, PixelHeight: height, PixelsPerMetreX: ppmX, PixelsPerMetreY: ppmY}
	return derivePageFrame(f)
}

func derivePageFrame(f PageFrameV1) (PageFrameV1, error) {
	if err := f.Source.Validate(); err != nil {
		return PageFrameV1{}, err
	}
	if f.Page < 1 || f.Page > MaxDocumentPages || f.Rotation != 0 && f.Rotation != 90 && f.Rotation != 180 && f.Rotation != 270 {
		return PageFrameV1{}, errors.New("invalid page number or rotation")
	}
	f.Transform = [6]PageRational{{0, 1}, {0, 1}, {0, 1}, {0, 1}, {0, 1}, {0, 1}}
	if f.InputUnits == "pixel" {
		if f.Page != 1 || f.Rotation != 0 || f.PixelWidth < 1 || f.PixelHeight < 1 || f.PixelWidth > MaxPageAxis || f.PixelHeight > MaxPageAxis || f.PixelWidth*f.PixelHeight > MaxPagePixels || f.PixelsPerMetreX < 1 || f.PixelsPerMetreX > math.MaxUint32 || f.PixelsPerMetreX != f.PixelsPerMetreY {
			return PageFrameV1{}, errors.New("unsupported PNG density or dimensions")
		}
		denominator := f.PixelsPerMetreX * 127
		var err error
		f.Width, err = PageRoundRatio(f.PixelWidth*50_000_000, denominator)
		if err != nil {
			return PageFrameV1{}, err
		}
		f.Height, err = PageRoundRatio(f.PixelHeight*50_000_000, denominator)
		if err != nil {
			return PageFrameV1{}, err
		}
		w, err := PageRoundRatio(f.PixelWidth*3_600_000_000, denominator)
		if err != nil {
			return PageFrameV1{}, err
		}
		h, err := PageRoundRatio(f.PixelHeight*3_600_000_000, denominator)
		if err != nil {
			return PageFrameV1{}, err
		}
		f.MediaBox = [4]int64{0, 0, w, h}
		f.CropBox = f.MediaBox
		scale, err := pageRat(50_000_000, denominator)
		if err != nil {
			return PageFrameV1{}, err
		}
		f.Transform[0] = scale
		f.Transform[3] = scale
		return f, nil
	}
	if f.InputUnits != "point/10000" || f.PixelWidth != 0 || f.PixelHeight != 0 || f.PixelsPerMetreX != 0 || f.PixelsPerMetreY != 0 {
		return PageFrameV1{}, errors.New("invalid PDF coordinate units")
	}
	for _, box := range [][4]int64{f.MediaBox, f.CropBox} {
		for _, v := range box {
			if v < -MaxPageInteger || v > MaxPageInteger {
				return PageFrameV1{}, errors.New("page box exceeds precision bounds")
			}
		}
		if box[2] <= box[0] || box[3] <= box[1] || box[2]-box[0] > MaxPageInteger || box[3]-box[1] > MaxPageInteger {
			return PageFrameV1{}, errors.New("invalid page box")
		}
	}
	c, m := f.CropBox, f.MediaBox
	if c[0] < m[0] || c[1] < m[1] || c[2] > m[2] || c[3] > m[3] {
		return PageFrameV1{}, errors.New("crop lies outside media box")
	}
	width, height := c[2]-c[0], c[3]-c[1]
	if f.Rotation == 90 || f.Rotation == 270 {
		width, height = height, width
	}
	var err error
	f.Width, err = PageRoundRatio(width, 72)
	if err != nil {
		return PageFrameV1{}, err
	}
	f.Height, err = PageRoundRatio(height, 72)
	if err != nil {
		return PageFrameV1{}, err
	}
	var numerators [6]int64
	switch f.Rotation {
	case 0:
		numerators = [6]int64{1, 0, 0, -1, -c[0], c[3]}
	case 90:
		numerators = [6]int64{0, 1, 1, 0, -c[1], -c[0]}
	case 180:
		numerators = [6]int64{-1, 0, 0, 1, c[2], -c[1]}
	case 270:
		numerators = [6]int64{0, -1, -1, 0, c[3], c[2]}
	}
	for i, n := range numerators {
		f.Transform[i], err = pageRat(n, 72)
		if err != nil {
			return PageFrameV1{}, err
		}
	}
	return f, nil
}

func ValidatePageFrameV1(f PageFrameV1) error {
	if f.Contract != PageFrameContractV1 || f.OutputUnits != "inch/10000" || f.Axes != "top-left,x-right,y-down" {
		return errors.New("invalid physical frame contract")
	}
	expected, err := derivePageFrame(f)
	if err != nil {
		return err
	}
	if expected != f {
		return errors.New("physical frame disagrees with its source geometry")
	}
	return nil
}

func MarshalPageFrameV1(f PageFrameV1) ([]byte, string, error) {
	if err := ValidatePageFrameV1(f); err != nil {
		return nil, "", err
	}
	b, err := canonical.Marshal(f)
	return b, sha256Hex(b), err
}

func DecodePageFrameV1(b []byte) (PageFrameV1, string, error) {
	f, err := canonical.Decode[PageFrameV1](b)
	if err != nil {
		return PageFrameV1{}, "", err
	}
	_, hash, err := MarshalPageFrameV1(f)
	return f, hash, err
}
