package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

const maxPhotoExportSourceBytes = 512 << 20
const maxPhotoExportDecodedPixels = 512_000_000
const maxPhotoExportOutputBytes = 1 << 30

type photoExportBudget struct{ pixels int64 }

// PreparePhotoExportPlan renders sequentially before the short publication transaction.
func PreparePhotoExportPlan(ctx context.Context, catalog *store.Store, blobs *blob.Store, spoolParent, owner string, request bundle.PlanRequest, publish func(context.Context, func() error) error) (bundle.Plan, error) {
	if request.PhotoRender == nil {
		return bundle.Plan{}, bundle.ErrConflict
	}
	if plan, found, err := catalog.ExportPlanReplay(ctx, owner, request); found || err != nil {
		return plan, err
	}
	if blobs == nil {
		return bundle.Plan{}, bundle.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	inputs, err := catalog.ExportPhotoInputs(ctx, owner, request)
	if err != nil {
		return bundle.Plan{}, err
	}
	if err := os.MkdirAll(spoolParent, 0700); err != nil {
		return bundle.Plan{}, err
	}
	spool, err := os.MkdirTemp(spoolParent, "photo-export-")
	if err != nil {
		return bundle.Plan{}, err
	}
	defer func() { _ = os.RemoveAll(spool) }()
	var sourceBytes int64
	for _, input := range inputs {
		if input.Member.Size > maxPhotoExportSourceBytes-sourceBytes {
			return bundle.Plan{}, fmt.Errorf("%w: photo source bytes exceed 512 MiB", bundle.ErrLimit)
		}
		sourceBytes += input.Member.Size
	}
	budget := photoExportBudget{pixels: maxPhotoExportDecodedPixels}
	artifacts := make([]store.PreparedPhotoExport, 0, len(inputs))
	var total int64
	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return bundle.Plan{}, err
		}
		reader, size, err := blobs.OpenSeekableContext(ctx, input.Member.SHA256)
		if err != nil {
			return bundle.Plan{}, fmt.Errorf("%w: photo %d (%s): %v", bundle.ErrUnavailable, input.Member.NodeID, input.Name, err)
		}
		if size != input.Member.Size {
			_ = reader.Close()
			return bundle.Plan{}, bundle.ErrConflict
		}
		output, receipt, err := renderPhotoExport(ctx, reader, input, *request.PhotoRender, &budget)
		closeErr := reader.Close()
		if err = errors.Join(err, closeErr); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return bundle.Plan{}, err
			}
			return bundle.Plan{}, fmt.Errorf("%w: photo %d (%s): %v", bundle.ErrUnavailable, input.Member.NodeID, input.Name, err)
		}
		if int64(len(output)) > maxPhotoExportOutputBytes-total {
			return bundle.Plan{}, bundle.ErrLimit
		}
		total += int64(len(output))
		if err := os.WriteFile(filepath.Join(spool, fmt.Sprint(index)), output, 0600); err != nil {
			return bundle.Plan{}, err
		}
		artifacts = append(artifacts, store.PreparedPhotoExport{Receipt: receipt, Input: input})
	}
	var plan bundle.Plan
	err = publish(ctx, func() error {
		return blobs.WithMutation(ctx, func() error {
			for index := range artifacts {
				file, err := os.Open(filepath.Join(spool, fmt.Sprint(index)))
				if err != nil {
					return err
				}
				written, err := blobs.WriteDetailedContext(ctx, file)
				if err = errors.Join(err, file.Close()); err != nil {
					return err
				}
				encoding, err := written.EncodingName()
				if err != nil {
					return err
				}
				artifacts[index].SHA256, artifacts[index].Size = written.Hash, written.Size
				artifacts[index].Physical = store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
			}
			var err error
			plan, err = catalog.SealPhotoExportPlan(ctx, owner, request, artifacts)
			return err
		})
	})
	return plan, err
}

// RenderPhotoExport reads original pixels once, preserving the RAW container's metadata.
func RenderPhotoExport(ctx context.Context, source io.ReadSeeker, input store.PhotoExportInput, profile bundle.PhotoRenderProfile) ([]byte, bundle.PhotoRenderReceipt, error) {
	return renderPhotoExport(ctx, source, input, profile, nil)
}

func renderPhotoExport(ctx context.Context, source io.ReadSeeker, input store.PhotoExportInput, profile bundle.PhotoRenderProfile, budget *photoExportBudget) ([]byte, bundle.PhotoRenderReceipt, error) {

	receipt := bundle.PhotoRenderReceipt{Version: bundle.PhotoRenderReceiptVersion, Profile: profile, Source: input.Member}
	if err := profile.Validate(); err != nil {
		return nil, receipt, err
	}
	if err := store.ValidatePhotoAuthored(input.Authored); err != nil {
		return nil, receipt, err
	}
	if input.Member.Size < 1 || input.Member.Size > maxPhotoExportSourceBytes {
		return nil, receipt, bundle.ErrLimit
	}
	if err := verifySeekableSource(ctx, source, input.Member.SHA256, input.Member.Size); err != nil {
		return nil, receipt, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, receipt, err
	}
	data, err := io.ReadAll(io.LimitReader(source, input.Member.Size+1))
	if err != nil {
		return nil, receipt, err
	}
	if int64(len(data)) != input.Member.Size {
		return nil, receipt, bundle.ErrConflict
	}
	packets, err := photoSourcePackets(data, profile.IncludeMetadata)
	if err != nil {
		return nil, receipt, err
	}
	pixels := io.ReadSeeker(bytes.NewReader(data))
	format := visualPreviewFormat(input.MediaType)
	containerOrientation := 0
	if format == "raw" {
		locations, malformed, err := visualPreviewRAWLocations(bytes.NewReader(data), input.MediaType, int64(len(data)))
		if err != nil {
			return nil, receipt, err
		}
		if malformed {
			return nil, receipt, errors.New("malformed RAW preview metadata")
		}
		if len(locations) == 0 {
			return nil, receipt, fmt.Errorf("%w: embedded RAW preview unavailable", bundle.ErrUnavailable)
		}
		var decoded image.Image
		var orientation int
		for _, location := range locations {
			preview := io.NewSectionReader(bytes.NewReader(data), location.offset, location.length)
			if len(packets.icc) == 0 {
				p, e := photoSourcePackets(data[location.offset:location.offset+location.length], false)
				if e != nil {
					err = e
					continue
				}
				packets.icc = p.icc
			}
			decoded, orientation, err = decodePhotoExport(ctx, preview, "jpeg", location.length, location.orientation, len(packets.icc) > 0, budget)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, receipt, err
		}
		receipt.EmbeddedPreview = true
		return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
	}
	decoded, orientation, err := decodePhotoExport(ctx, pixels, format, int64(len(data)), containerOrientation, len(packets.icc) > 0, budget)
	if err != nil {
		return nil, receipt, err
	}
	if packets.unsupportedColor && len(packets.icc) == 0 {
		return nil, receipt, errors.New("unsupported color metadata without an ICC profile")
	}
	return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
}

func decodePhotoExport(ctx context.Context, source io.ReadSeeker, format string, size int64, containerOrientation int, hasProfile bool, budget *photoExportBudget) (image.Image, int, error) {
	orientation := 1
	var color, metadata, malformed, animated bool
	var err error
	switch format {
	case "jpeg":
		orientation, color, malformed, err = inspectVisualPreviewJPEG(ctx, source)
	case "png":
		orientation, color, metadata, malformed, err = inspectVisualPreviewPNG(ctx, source, size)
	case "webp":
		orientation, color, metadata, animated, malformed, err = inspectVisualPreviewWebP(ctx, source, size)
	case "gif":
	default:
		return nil, 0, fmt.Errorf("%w: unsupported photo media type", bundle.ErrUnavailable)
	}
	if err != nil {
		return nil, 0, err
	}
	if color && !hasProfile || metadata || malformed || animated {
		return nil, 0, errors.New("unsupported color profile, animation, or malformed image metadata")
	}
	if containerOrientation >= 1 && containerOrientation <= 8 {
		orientation = containerOrientation
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	config, actual, err := image.DecodeConfig(source)
	if err != nil {
		return nil, 0, err
	}
	if actual != format || !visualPreviewDimensionsAllowed(config.Width, config.Height) {
		return nil, 0, bundle.ErrLimit
	}
	if budget != nil {
		n := int64(config.Width) * int64(config.Height)
		if n > budget.pixels {
			return nil, 0, fmt.Errorf("%w: decoded pixels exceed 512 million", bundle.ErrLimit)
		}
		budget.pixels -= n
	}
	if format == "jpeg" && !visualPreviewJPEGColorModelSupported(config.ColorModel) {
		return nil, 0, errors.New("unsupported JPEG color model")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	decoded, _, err := image.Decode(source)
	if err != nil {
		return nil, 0, err
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return nil, 0, bundle.ErrConflict
	}
	return decoded, orientation, ctx.Err()
}

func encodePhotoExport(ctx context.Context, decoded image.Image, orientation int, packets photoPackets, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt) ([]byte, bundle.PhotoRenderReceipt, error) {
	profile := receipt.Profile
	orientation = [4][8]int{{1, 2, 3, 4, 5, 6, 7, 8}, {6, 7, 8, 5, 2, 3, 4, 1}, {3, 4, 1, 2, 7, 8, 5, 6}, {8, 5, 6, 7, 4, 1, 2, 3}}[input.Authored.Rotation/90][orientation-1]
	oriented := transformPhotoPixels(decoded, orientation, profile.LongEdge)

	receipt.Width, receipt.Height = oriented.Bounds().Dx(), oriented.Bounds().Dy()
	var output bytes.Buffer
	if profile.Format == "jpeg" {
		matte := whitePhotoMatte(oriented)
		if err := jpeg.Encode(&output, matte, &jpeg.Options{Quality: profile.Quality}); err != nil {
			return nil, receipt, err
		}
	} else if err := png.Encode(&output, oriented); err != nil {
		return nil, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	result := output.Bytes()
	if profile.IncludeMetadata || len(packets.icc) > 0 {
		var err error
		result, err = photoExportMetadata(ctx, packets, input, receipt, result)
		if err != nil {
			return nil, receipt, err
		}
	}
	raw, err := canonical.Marshal(input)
	if err == nil {
		receipt.InputSHA256 = photoInputDigest(raw)
	}
	return result, receipt, err
}

func photoInputDigest(raw []byte) string {
	d := sha256.Sum256(raw)
	return hex.EncodeToString(d[:])
}
