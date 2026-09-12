// Package pagerender runs optional pinned local page inspection and rendering.
package pagerender

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image/png"
	"io"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

const Protocol = "docbank-page-inspect/v1"

// VerifyPNG verifies the entire bounded image against its retained receipt.
// Publication, HTTP reads and portable restore use the same decoder.
func VerifyPNG(data []byte, receipt document.PageImageV1) error {
	if err := document.ValidatePageImageV1(receipt); err != nil {
		return err
	}
	if int64(len(data)) != receipt.Size {
		return ErrInvalidOutput
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != receipt.SHA256 {
		return ErrInvalidOutput
	}
	w, h, _, _, err := inspectPNG(data, false)
	if err != nil || w != receipt.Width || h != receipt.Height {
		return ErrInvalidOutput
	}
	return nil
}

type Inspection struct {
	Contract  string                 `json:"contract"`
	Complete  bool                   `json:"complete"`
	Source    document.PageSource    `json:"source"`
	PageCount int                    `json:"page_count"`
	Frames    []document.PageFrameV1 `json:"frames"`
}

func verifySource(data []byte, source document.PageSource) error {
	if err := source.Validate(); err != nil {
		return err
	}
	h := sha256.Sum256(data)
	if int64(len(data)) != source.Size || hex.EncodeToString(h[:]) != source.SHA256 {
		return errors.New("page source hash or size mismatch")
	}
	return nil
}

// InspectSource is the compiled inspection child's entry point. Daemons use
// Runtime.Inspect so PDF parsing and image decoding execute under a memory cap.
func InspectSource(data []byte, source document.PageSource, mediaType string) ([]document.PageFrameV1, error) {
	if err := verifySource(data, source); err != nil {
		return nil, err
	}
	switch mediaType {
	case "application/pdf":
		pages, err := formatdetect.ReadPDFPageGeometry(data, document.MaxDocumentPages)
		if err != nil {
			return nil, err
		}
		frames := make([]document.PageFrameV1, len(pages))
		for index, page := range pages {
			frames[index], err = document.NewPDFPageFrame(source, index+1, page.MediaBox, page.CropBox, page.Rotation)
			if err != nil {
				return nil, err
			}
		}
		return frames, nil
	case "image/png":
		width, height, ppmX, ppmY, err := inspectPNG(data, true)
		if err != nil {
			return nil, err
		}
		frame, err := document.NewPNGPageFrame(source, width, height, ppmX, ppmY)
		if err != nil {
			return nil, err
		}
		return []document.PageFrameV1{frame}, nil
	default:
		return nil, errors.New("page source media type is unsupported")
	}
}

// inspectPNG validates complete chunks, rejects animation and orientation,
// then fully decodes only after fixed allocation bounds are established.
func inspectPNG(data []byte, requireDensity bool) (int64, int64, int64, int64, error) {
	bad := func() (int64, int64, int64, int64, error) {
		return 0, 0, 0, 0, errors.New("unsupported or malformed bounded PNG")
	}
	if len(data) < 33 || int64(len(data)) > document.MaxPageImageBytes || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return bad()
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > document.MaxPageAxis || cfg.Height > document.MaxPageAxis || int64(cfg.Width)*int64(cfg.Height) > document.MaxPagePixels {
		return bad()
	}
	var ppmX, ppmY int64
	density, idat, end := false, false, false
	for offset, count := 8, 0; offset < len(data); count++ {
		if count > 100000 || len(data)-offset < 12 {
			return bad()
		}
		size := uint64(binary.BigEndian.Uint32(data[offset:]))
		if size > uint64(len(data)-offset-12) { //nolint:gosec // The preceding check establishes a nonnegative bounded remainder.
			return bad()
		}
		n := int(size) //nolint:gosec // Size is at most the remaining bounded 32 MiB input.
		kind := string(data[offset+4 : offset+8])
		payload := data[offset+8 : offset+8+n]
		if crc32.ChecksumIEEE(data[offset+4:offset+8+n]) != binary.BigEndian.Uint32(data[offset+8+n:offset+12+n]) {
			return bad()
		}
		switch kind {
		case "acTL", "fcTL", "fdAT", "eXIf":
			return bad()
		case "pHYs":
			if density || idat || n != 9 || payload[8] != 1 {
				return bad()
			}
			density = true
			ppmX = int64(binary.BigEndian.Uint32(payload))
			ppmY = int64(binary.BigEndian.Uint32(payload[4:]))
			if ppmX < 1 || ppmX != ppmY {
				return bad()
			}
		case "IDAT":
			idat = true
		case "IEND":
			if n != 0 || offset+12 != len(data) {
				return bad()
			}
			end = true
		}
		offset += n + 12
	}
	if !end || !idat || requireDensity && !density {
		return bad()
	}
	reader := bytes.NewReader(data)
	decoded, err := png.Decode(reader)
	if err != nil || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height || reader.Len() != 0 {
		return bad()
	}
	return int64(cfg.Width), int64(cfg.Height), ppmX, ppmY, nil
}

// InspectInput reads exactly one bounded source for the compiled bridge.
func InspectInput(reader io.Reader, versionID, mediaType string) (Inspection, error) {
	data, err := io.ReadAll(io.LimitReader(reader, document.MaxPageSourceBytes+1))
	if err != nil {
		return Inspection{}, err
	}
	if int64(len(data)) > document.MaxPageSourceBytes {
		return Inspection{}, errors.New("page source exceeds byte limit")
	}
	defer clear(data)
	hash := sha256.Sum256(data)
	source := document.PageSource{VersionID: versionID, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	frames, err := InspectSource(data, source, mediaType)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Contract: Protocol, Complete: true, Source: source, PageCount: len(frames), Frames: frames}, nil
}
