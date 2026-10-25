package pdfproduction

import (
	"bufio"
	"cmp"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"slices"
)

// ErrPDFIntegrity identifies a mismatch in the PDF's structure, text or pixels.
// Resource, context, engine and local I/O errors retain their own identities.
var ErrPDFIntegrity = errors.New("PDF integrity check failed")

func pdfIntegrity(message string) error { return fmt.Errorf("%w: %s", ErrPDFIntegrity, message) }

// Only format errors from the standard decoder become integrity failures.
func savedStreamError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, zlib.ErrHeader) || errors.Is(err, zlib.ErrDictionary) || errors.Is(err, zlib.ErrChecksum) {
		return fmt.Errorf("%w: image stream: %w", ErrPDFIntegrity, err)
	}
	if _, ok := errors.AsType[flate.CorruptInputError](err); ok {
		return fmt.Errorf("%w: image stream: %w", ErrPDFIntegrity, err)
	}
	return err
}

type savedRGBRows struct {
	ctx         context.Context
	input       *bufio.Reader
	decoder     io.ReadCloser
	rgb, rgba   []byte
	masks       []image.Rectangle
	height, row int
}

func openSavedRGBRows(ctx context.Context, input io.Reader, width, height int,
	masks []image.Rectangle,
) (*savedRGBRows, error) {
	// ByteReader prevents inflater read-ahead from hiding bytes past the trailer.
	// Context is checked on refills even while empty blocks produce no rows.
	r := bufio.NewReader(contextReader{ctx, input})
	header, err := r.Peek(2)
	if err != nil {
		return nil, savedStreamError(err)
	}
	if header[1]&0x20 != 0 {
		return nil, pdfIntegrity("image stream uses a preset dictionary")
	}
	decoder, err := zlib.NewReader(r)
	if err != nil {
		return nil, savedStreamError(err)
	}
	slices.SortFunc(masks, func(a, b image.Rectangle) int { return cmp.Compare(a.Min.X, b.Min.X) })
	return &savedRGBRows{ctx: ctx, input: r, decoder: decoder, rgb: make([]byte, width*3),
		rgba: make([]byte, width*4), masks: masks, height: height}, nil
}

func (r *savedRGBRows) Next() ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r.decoder, r.rgb); err != nil {
		return nil, savedStreamError(err)
	}
	// Masks are sorted by x; overlapping rectangles never cause repeated pixel work.
	checked := 0
	for _, mask := range r.masks {
		if r.row < mask.Min.Y || r.row >= mask.Max.Y {
			continue
		}
		for x := max(checked, mask.Min.X) * 3; x < mask.Max.X*3; x++ {
			if r.rgb[x] != 0 {
				return nil, pdfIntegrity("image pixels differ from reviewed black masks")
			}
		}
		checked = max(checked, mask.Max.X)
	}
	for x := range len(r.rgb) / 3 {
		copy(r.rgba[x*4:x*4+3], r.rgb[x*3:x*3+3])
		r.rgba[x*4+3] = 255
	}
	r.row++
	return r.rgba, nil
}

func (r *savedRGBRows) Finish() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.row != r.height {
		return pdfIntegrity("image row count differs from reviewed page")
	}
	var extra [1]byte
	n, err := io.ReadFull(r.decoder, extra[:])
	if n != 0 {
		return pdfIntegrity("image has extra decoded pixels")
	}
	if !errors.Is(err, io.EOF) {
		return savedStreamError(err)
	}
	_, err = r.input.ReadByte()
	if err == nil {
		return pdfIntegrity("image has bytes after its zlib trailer")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read image stream boundary: %w", err)
	}
	return r.ctx.Err()
}

func (r *savedRGBRows) Close() error { return r.decoder.Close() }
