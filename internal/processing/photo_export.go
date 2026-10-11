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

	_ "golang.org/x/image/webp" // Register WebP with image.Decode.

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/store"
)

const maxPhotoExportSourceBytes = 512 << 20
const maxPhotoExportOutputBytes = 1 << 30

// PreparePhotoExportPlan renders sequentially before bounded artifact publication under the mutation lease.
func PreparePhotoExportPlan(ctx context.Context, catalog *store.Store, blobs *blob.Store, spoolParent, owner string, request bundle.PlanRequest, publish func(context.Context, func() error) error) (bundle.Plan, error) {
	if request.PhotoRender == nil {
		return bundle.Plan{}, bundle.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	renderCtx, stopRendering := context.WithDeadline(ctx, deadline.Add(-time.Minute))
	defer stopRendering()
	release, err := catalog.AcquirePhotoExportPreparation(renderCtx)
	if err != nil {
		return bundle.Plan{}, err
	}
	defer release()
	if plan, found, err := catalog.ExportPlanReplay(renderCtx, owner, request); found || err != nil {
		return plan, err
	}
	if blobs == nil {
		return bundle.Plan{}, errors.New("photo export blob store unavailable")
	}
	profile := request.PhotoRender.Canonical()
	request.PhotoRender = &profile
	inputs, err := catalog.ExportPhotoInputs(renderCtx, owner, request)
	if err != nil {
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
		if visualPreviewFormat(input.MediaType) == "" {
			return bundle.Plan{}, photoExportError(input, fmt.Errorf("%w: unsupported photo media type %s", bundle.ErrUnavailable, input.MediaType))
		}
		if input.Member.Size > maxPhotoExportSourceBytes-sourceBytes {
			return bundle.Plan{}, photoExportError(input, fmt.Errorf("%w: photo source bytes exceed 512 MiB", bundle.ErrLimit))
		}
		sourceBytes += input.Member.Size
	}
	if err := os.MkdirAll(spoolParent, 0700); err != nil {
		return bundle.Plan{}, err
	}
	artifacts := make([]store.PreparedPhotoExport, 0, len(inputs))
	var total int64
	for _, input := range inputs {
		if err := renderCtx.Err(); err != nil {
			return bundle.Plan{}, err
		}
		data, err := readExportBlob(renderCtx, blobs, input.Member.SHA256, input.Member.Size, maxPhotoExportSourceBytes)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return bundle.Plan{}, err
			}
			return bundle.Plan{}, photoExportError(input, fmt.Errorf("source is missing or unreadable: %w", bundle.ErrUnavailable))
		}
		output, receipt, err := renderPhotoExport(renderCtx, data, input, *request.PhotoRender)
		if err != nil {
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

func renderPhotoExport(ctx context.Context, data []byte, input store.PhotoExportInput, profile bundle.PhotoRenderProfile) ([]byte, bundle.PhotoRenderReceipt, error) {
	receipt := bundle.PhotoRenderReceipt{Version: bundle.PhotoRenderReceiptVersion, Profile: profile, Source: input.Member}
	if err := store.ValidatePhotoAuthored(input.Authored); err != nil {
		return nil, receipt, err
	}
	packets, err := photoSourcePackets(ctx, data, profile.IncludeMetadata)
	if err != nil {
		return nil, receipt, photoExportContentError(err)
	}
	pixels := io.ReadSeeker(bytes.NewReader(data))
	format := visualPreviewFormat(input.MediaType)
	if format == "raw" {
		var locations []visualPreviewRAWLocation
		var malformed bool
		if packets.rawPreview != nil {
			locations = []visualPreviewRAWLocation{*packets.rawPreview}
		} else {
			locations, malformed, err = visualPreviewRAWLocations(bytes.NewReader(data), input.MediaType, int64(len(data)))
		}
		if err != nil {
			return nil, receipt, photoExportContentError(err)
		}
		if malformed {
			return nil, receipt, fmt.Errorf("%w: malformed RAW preview metadata", bundle.ErrUnavailable)
		}
		if len(locations) == 0 {
			return nil, receipt, fmt.Errorf("%w: embedded RAW preview unavailable", bundle.ErrUnavailable)
		}
		var decoded image.Image
		var orientation int
		for _, location := range locations {
			if location.previewICC {
				return nil, receipt, fmt.Errorf("%w: RAW preview IFD color profile cannot be carried into the export", bundle.ErrUnavailable)
			}
			preview := io.NewSectionReader(bytes.NewReader(data), location.offset, location.length)

			p := packets
			if packets.rawPreview == nil {
				var e error
				p, e = photoSourcePackets(ctx, data[location.offset:location.offset+location.length], false)
				if e != nil {
					err = e
					continue
				}
			}
			candidate := packets
			if len(packets.icc) > 0 && len(p.icc) == 0 {
				err = fmt.Errorf("%w: RAW ICC profile is not associated with the embedded JPEG", bundle.ErrUnavailable)
				continue
			}
			candidate.icc = p.icc
			candidate.orientation, candidate.animated = p.orientation, p.animated
			candidate.unsupportedColor = candidate.unsupportedColor || p.unsupportedColor

			decoded, orientation, err = decodePhotoExport(ctx, preview, visualFormatJPEG, location.orientation, candidate)
			if err == nil {
				packets = candidate
				break
			}
		}
		if err != nil {
			return nil, receipt, photoExportContentError(err)
		}
		receipt.EmbeddedPreview = true
		return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
	}
	decoded, orientation, err := decodePhotoExport(ctx, pixels, format, 0, packets)
	if err != nil {
		return nil, receipt, err
	}
	return encodePhotoExport(ctx, decoded, orientation, packets, input, receipt)
}

func photoExportError(input store.PhotoExportInput, err error) error {
	return fmt.Errorf("photo %d (%s): %w", input.Member.NodeID, input.Name, err)
}

func photoExportContentError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, bundle.ErrLimit) || errors.Is(err, bundle.ErrConflict) || errors.Is(err, bundle.ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", bundle.ErrUnavailable, err)
}

func decodePhotoExport(ctx context.Context, source io.ReadSeeker, format string, containerOrientation int, packets photoPackets) (image.Image, int, error) {
	orientation := packets.orientation
	if format != visualFormatJPEG && format != visualFormatPNG && format != visualFormatWebP && format != "gif" {
		return nil, 0, fmt.Errorf("%w: unsupported photo media type", bundle.ErrUnavailable)
	}
	if packets.unsupportedColor && len(packets.icc) == 0 || packets.animated {
		return nil, 0, fmt.Errorf("%w: unsupported color profile or animation", bundle.ErrUnavailable)
	}
	if containerOrientation >= 1 && containerOrientation <= 8 {
		orientation = containerOrientation
	}
	pixels, err := decodePhotoPixels(ctx, source, format)
	if err != nil {
		if errors.Is(err, errVisualDimensions) {
			err = fmt.Errorf("%w: %w", bundle.ErrLimit, err)
		}
		if pixels.readErr != nil {
			return nil, 0, err
		}
		return nil, 0, photoExportContentError(err)
	}
	return pixels.image, orientation, nil
}

func encodePhotoExport(ctx context.Context, decoded image.Image, orientation int, packets photoPackets, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt) ([]byte, bundle.PhotoRenderReceipt, error) {
	profile := receipt.Profile
	orientation = [4][8]int{{1, 2, 3, 4, 5, 6, 7, 8}, {6, 7, 8, 5, 2, 3, 4, 1}, {3, 4, 1, 2, 7, 8, 5, 6}, {8, 5, 6, 7, 4, 1, 2, 3}}[input.Authored.Rotation/90][orientation-1]
	oriented := transformPhotoPixels(decoded, orientation, profile.LongEdge)

	receipt.Width, receipt.Height = oriented.Bounds().Dx(), oriented.Bounds().Dy()
	var output bytes.Buffer
	if profile.Format == visualFormatJPEG {
		matte := whitePhotoMatte(oriented)
		if err := jpeg.Encode(&output, matte, &jpeg.Options{Quality: profile.Quality}); err != nil {
			return nil, receipt, fmt.Errorf("encoding photo JPEG: %w", err)
		}
	} else if err := png.Encode(&output, oriented); err != nil {
		return nil, receipt, fmt.Errorf("encoding photo PNG: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	result := output.Bytes()
	if profile.IncludeMetadata || len(packets.icc) > 0 {
		var err error
		result, err = photoExportMetadata(ctx, packets, input, receipt, result)
		if err != nil {
			return nil, receipt, photoExportContentError(err)
		}
	}
	return result, receipt, nil
}

var errVisualDimensions = errors.New("source dimensions exceed the built-in limit")
var errVisualColor = errors.New("unsupported JPEG color model")
var errVisualFormat = errors.New("unexpected image format")
var errVisualDecodeBounds = errors.New("image dimensions changed during decoding")

type photoPixels struct {
	image   image.Image
	config  image.Config
	readErr error
}

func decodePhotoPixels(ctx context.Context, source io.ReadSeeker, format string) (result photoPixels, err error) {
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		return result, err
	}
	reader := &visualPreviewReadErrorRecorder{reader: &contextReader{ctx, source}}
	defer func() {
		if reader.err != nil {
			result.readErr = reader.err
		}
	}()
	var actual string
	result.config, actual, err = image.DecodeConfig(reader)
	if err != nil {
		return result, fmt.Errorf("reading image dimensions: %w", err)
	}
	if actual != format {
		return result, errVisualFormat
	}
	if !visualPreviewDimensionsAllowed(result.config.Width, result.config.Height) {
		return result, errVisualDimensions
	}
	if format == visualFormatJPEG && !visualPreviewJPEGColorModelSupported(result.config.ColorModel) {
		return result, errVisualColor
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		return result, err
	}
	if format == "gif" {
		result.image, err = decodeGIFCanvas(reader, result.config)
	} else {
		result.image, _, err = image.Decode(reader)
	}
	if err != nil {
		return result, fmt.Errorf("decoding image pixels: %w", err)
	}
	if result.image.Bounds().Dx() != result.config.Width || result.image.Bounds().Dy() != result.config.Height {
		return result, errVisualDecodeBounds
	}
	return result, ctx.Err()
}
