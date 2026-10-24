package processing

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"os"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

const maxPhotoExportSourceBytes = 512 << 20
const maxPhotoExportOutputBytes = 1 << 30

// JPEG stores each dimension in 16 bits.
const maxPhotoExportJPEGEdge = 65535

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
	if plan, found, err := catalog.ExportPlanReplay(renderCtx, owner, request); found || err != nil {
		return plan, err
	}
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
			return bundle.Plan{}, photoExportError(input, photoSourceReadFailure(err))
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
		stage, err := filepublish.CreateStage(spoolParent, ".export-photo-")
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
	packets, err := photoSourcePackets(ctx, data, profile.IncludeMetadata, profile.IncludeMetadata && len(input.Keywords) == 0)
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
				err = fmt.Errorf("%w: RAW preview IFD color profile cannot be carried into the export", bundle.ErrUnavailable)
				continue
			}
			preview := io.NewSectionReader(bytes.NewReader(data), location.offset, location.length)

			p := packets
			if packets.rawPreview == nil {
				var e error
				p, e = photoSourcePackets(ctx, data[location.offset:location.offset+location.length], false, false)
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
			candidate.icc, candidate.grayICC = p.icc, p.grayICC
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
	pixels, err := decodeVisualPixels(ctx, source, format, false)
	if err != nil {
		if errors.Is(err, errVisualDimensions) {
			err = fmt.Errorf("%w: %w", bundle.ErrLimit, err)
		}
		if pixels.readErr != nil {
			return nil, 0, err
		}
		return nil, 0, photoExportContentError(err)
	}
	if packets.grayICC && !isGrayPixels(pixels.image) {
		return nil, 0, fmt.Errorf("%w: gray color profile on color or transparent pixels", bundle.ErrUnavailable)
	}
	return pixels.image, orientation, nil
}

func encodePhotoExport(ctx context.Context, decoded image.Image, orientation int, packets photoPackets, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt) ([]byte, bundle.PhotoRenderReceipt, error) {
	profile := receipt.Profile
	sourceOrientation := orientation
	orientation = store.PhotoXMPOrientation(orientation, input.Authored.Rotation)
	oriented, err := transformPhotoPixels(ctx, decoded, orientation, profile.LongEdge)
	if err != nil {
		return nil, receipt, err
	}

	receipt.Width, receipt.Height = oriented.Bounds().Dx(), oriented.Bounds().Dy()
	if profile.Format == visualFormatJPEG && max(receipt.Width, receipt.Height) > maxPhotoExportJPEGEdge {
		return nil, receipt, fmt.Errorf("%w: JPEG output is limited to 65,535 pixels per side; set a smaller long edge or choose PNG", bundle.ErrLimit)
	}
	var pixels image.Image = oriented
	if packets.grayICC {
		if err := ctx.Err(); err != nil {
			return nil, receipt, err
		}
		gray := image.NewGray(oriented.Bounds())
		if err := drawPhotoPixels(ctx, gray, oriented, draw.Src); err != nil {
			return nil, receipt, err
		}
		pixels = gray
	} else if profile.Format == visualFormatJPEG {
		pixels, err = whitePhotoMatte(ctx, oriented)
		if err != nil {
			return nil, receipt, err
		}
	}
	var output bytes.Buffer
	if err := writePhotoExport(ctx, &output, pixels, profile); err != nil {
		return nil, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	result := output.Bytes()
	if profile.IncludeMetadata || len(packets.icc) > 0 {
		var err error
		result, err = photoExportMetadata(ctx, packets, input, receipt, sourceOrientation, result)
		if err != nil {
			return nil, receipt, photoExportContentError(err)
		}
	}
	return result, receipt, nil
}

func writePhotoExport(ctx context.Context, dst io.Writer, pixels image.Image, profile bundle.PhotoRenderProfile) (err error) {
	defer recoverPhotoCancellation(&err)
	writer := photoExportWriter{ctx, bufio.NewWriter(dst)}
	if profile.Format == visualFormatJPEG {
		// JPEG keeps transforming blocks after writer errors. Our byte writer
		// interrupts it even when a uniform image produces very few bytes.
		if err := jpeg.Encode(writer, pixels, &jpeg.Options{Quality: profile.Quality}); err != nil {
			return fmt.Errorf("encoding photo JPEG: %w", err)
		}
	} else if err := png.Encode(writer, photoContextImage{pixels, ctx}); err != nil {
		return fmt.Errorf("encoding photo PNG: %w", err)
	}
	return writer.Flush()
}

type photoExportWriter struct {
	ctx context.Context
	dst *bufio.Writer
}

func (w photoExportWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	n, err := w.dst.Write(p)
	if err != nil {
		return n, fmt.Errorf("writing rendered photo: %w", err)
	}
	return n, nil
}

func (w photoExportWriter) WriteByte(b byte) error {
	if err := w.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	if err := w.dst.WriteByte(b); err != nil {
		return fmt.Errorf("writing rendered photo byte: %w", err)
	}
	return nil
}

func (w photoExportWriter) Flush() error {
	if err := w.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	if err := w.dst.Flush(); err != nil {
		return fmt.Errorf("flushing rendered photo: %w", err)
	}
	return nil
}

func isGrayPixels(pixels image.Image) bool {
	switch pixels.(type) {
	case *image.Gray, *image.Gray16:
		return true
	default:
		return false
	}
}

// PhotoSourceReadError keeps blob paths and hashes out of the client message.
// Cause returns the underlying read failure for the daemon log.
type PhotoSourceReadError struct {
	message string
	kind    error
	cause   error
}

func (e *PhotoSourceReadError) Error() string {
	if e.kind == nil {
		return e.message
	}
	return e.message + ": " + e.kind.Error()
}

func (e *PhotoSourceReadError) Unwrap() error { return e.kind }

func (e *PhotoSourceReadError) Cause() error { return e.cause }

// photoSourceReadFailure separates a source the user can deselect from a vault
// read failure that deselecting would only hide.
func photoSourceReadFailure(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, packstore.ErrPhysicalMissing):
		return &PhotoSourceReadError{message: "source is missing", kind: bundle.ErrUnavailable, cause: err}
	case errors.Is(err, store.ErrPackageConflict), errors.Is(err, packstore.ErrPhysicalCorrupt), errors.Is(err, packstore.ErrContentMismatch):
		return &PhotoSourceReadError{message: "source failed integrity verification; run docbank verify", kind: bundle.ErrUnavailable, cause: err}
	default:
		return &PhotoSourceReadError{message: "reading source failed", cause: err}
	}
}
