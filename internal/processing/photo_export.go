package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/filepublish"
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	release, err := catalog.AcquirePhotoExportPreparation(ctx)
	if err != nil {
		return bundle.Plan{}, err
	}
	defer release()
	if plan, found, err := catalog.ExportPlanReplay(ctx, owner, request); found || err != nil {
		return plan, err
	}
	if blobs == nil {
		return bundle.Plan{}, bundle.ErrUnavailable
	}
	inputs, err := catalog.ExportPhotoInputs(ctx, owner, request)
	if err != nil {
		return bundle.Plan{}, err
	}
	if err := os.MkdirAll(spoolParent, 0700); err != nil {
		return bundle.Plan{}, err
	}
	var stages []*filepublish.Stage
	defer func() {
		for _, stage := range stages {
			_ = stage.Cleanup()
		}
	}()
	var sourceBytes int64
	for _, input := range inputs {
		if input.Member.Size > maxPhotoExportSourceBytes-sourceBytes {
			return bundle.Plan{}, photoExportError(input, fmt.Errorf("%w: photo source bytes exceed 512 MiB", bundle.ErrLimit))
		}
		sourceBytes += input.Member.Size
	}
	budget := photoExportBudget{pixels: maxPhotoExportDecodedPixels}
	artifacts := make([]store.PreparedPhotoExport, 0, len(inputs))
	var total int64
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return bundle.Plan{}, err
		}
		reader, size, err := blobs.OpenSeekableContext(ctx, input.Member.SHA256)
		if err != nil {
			return bundle.Plan{}, photoExportError(input, err)
		}
		if size != input.Member.Size {
			_ = reader.Close()
			return bundle.Plan{}, photoExportError(input, bundle.ErrConflict)
		}
		output, receipt, err := renderPhotoExport(ctx, reader, input, *request.PhotoRender, &budget)
		closeErr := reader.Close()
		if err = errors.Join(err, closeErr); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return bundle.Plan{}, err
			}
			return bundle.Plan{}, photoExportError(input, err)
		}
		if int64(len(output)) > maxPhotoExportOutputBytes-total {
			return bundle.Plan{}, photoExportError(input, fmt.Errorf("%w: encoded output exceeds 1 GiB", bundle.ErrLimit))
		}
		total += int64(len(output))
		stage, err := filepublish.CreateStage(spoolParent, ".photo-export-")
		if err != nil {
			return bundle.Plan{}, photoExportError(input, err)
		}
		stages = append(stages, stage)
		if _, err = stage.File.Write(output); err != nil {
			return bundle.Plan{}, photoExportError(input, err)
		}
		artifacts = append(artifacts, store.PreparedPhotoExport{Receipt: receipt, Input: input})
	}
	var plan bundle.Plan
	err = publish(ctx, func() error {
		return blobs.WithMutation(ctx, func() error {
			for index := range artifacts {
				file := stages[index].File
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					return photoExportError(artifacts[index].Input, err)
				}
				written, err := blobs.WriteDetailedContext(ctx, file)
				if err != nil {
					return photoExportError(artifacts[index].Input, err)
				}
				encoding, err := written.EncodingName()
				if err != nil {
					return photoExportError(artifacts[index].Input, err)
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
	packets, err := photoSourcePackets(ctx, data, profile.IncludeMetadata)
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

			p, e := photoSourcePackets(ctx, data[location.offset:location.offset+location.length], false)
			if e != nil {
				err = e
				continue
			}
			candidate := packets
			if len(candidate.icc) == 0 {
				candidate.icc = p.icc
			}
			candidate.orientation, candidate.animated = p.orientation, p.animated
			candidate.unsupportedColor = candidate.unsupportedColor || p.unsupportedColor

			decoded, orientation, err = decodePhotoExport(ctx, preview, "jpeg", location.orientation, candidate, budget)
			if err == nil {
				packets = candidate
				break
			}
		}
		if err != nil {
			return nil, receipt, err
		}
		receipt.EmbeddedPreview = true
		return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
	}
	decoded, orientation, err := decodePhotoExport(ctx, pixels, format, containerOrientation, packets, budget)
	if err != nil {
		return nil, receipt, err
	}
	return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
}

func photoExportError(input store.PhotoExportInput, err error) error {
	category := bundle.ErrUnavailable
	if errors.Is(err, bundle.ErrLimit) || errors.Is(err, bundle.ErrConflict) {
		category = err
	}
	return fmt.Errorf("%w: photo %d (%s): %w", category, input.Member.NodeID, input.Name, err)
}

func decodePhotoExport(ctx context.Context, source io.ReadSeeker, format string, containerOrientation int, packets photoPackets, budget *photoExportBudget) (image.Image, int, error) {
	orientation := packets.orientation
	if format != "jpeg" && format != "png" && format != "webp" && format != "gif" {
		return nil, 0, fmt.Errorf("%w: unsupported photo media type", bundle.ErrUnavailable)
	}
	if packets.unsupportedColor && len(packets.icc) == 0 || packets.animated {
		return nil, 0, errors.New("unsupported color profile or animation")
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
	}
	if format == "jpeg" && !visualPreviewJPEGColorModelSupported(config.ColorModel) {
		return nil, 0, errors.New("unsupported JPEG color model")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	var decoded image.Image
	if format == "gif" {
		decoded, err = decodeGIFCanvas(source, config)
	} else {
		decoded, _, err = image.Decode(source)
	}
	if err != nil {
		return nil, 0, err
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return nil, 0, bundle.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if budget != nil {
		budget.pixels -= int64(config.Width) * int64(config.Height)
	}
	return decoded, orientation, nil
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
	return result, receipt, nil
}
