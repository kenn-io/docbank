package processing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
)

const (
	visualFormatJPEG = "jpeg"
	visualFormatPNG  = "png"
	visualFormatWebP = "webp"
)

type visualPreviewContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r visualPreviewContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var errVisualContainer = errors.New("malformed image container")
var errVisualMetadataLimit = errors.New("image metadata exceeds inspection limit")

func walkPhotoContainer(ctx context.Context, source io.ReadSeeker, format string, size int64, visit func(string, io.Reader, int64) error) (err error) {
	defer func() {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.Join(errVisualContainer, err)
		}
	}()
	if size < 0 {
		size, err = source.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(visualPreviewContextReader{ctx, io.LimitReader(source, size)})
	headerSize := map[string]int{visualFormatJPEG: 2, visualFormatPNG: 8, visualFormatWebP: 12}[format]
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	if format == visualFormatJPEG && !bytes.Equal(header, []byte{0xff, 0xd8}) || format == visualFormatPNG && string(header) != "\x89PNG\r\n\x1a\n" || format == visualFormatWebP && (string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" || int64(binary.LittleEndian.Uint32(header[4:]))+8 != size) {
		return errVisualContainer
	}
	offset := int64(headerSize)
	pngChunks := 0
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if format == visualFormatWebP && offset == size {
			return nil
		}
		if format == visualFormatJPEG && count >= visualPreviewMaxJPEGSegments {
			return errVisualContainer
		}
		if format == visualFormatWebP && count >= visualPreviewMaxWebPChunks {
			return errVisualMetadataLimit
		}
		var kind string
		var length, overhead, padding int64
		switch format {
		case visualFormatJPEG:
			marker, err := readVisualPreviewJPEGMarker(r)
			if err != nil {
				return err
			}
			if marker == 0xda || marker == 0xd9 {
				return nil
			}
			if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
				continue
			}
			var h [2]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return err
			}
			length = int64(binary.BigEndian.Uint16(h[:])) - 2
			if length < 0 {
				return errVisualContainer
			}
			kind = string([]byte{marker})
		case visualFormatPNG, visualFormatWebP:
			overhead = 8
			if format == visualFormatPNG {
				overhead = 12
			}
			if offset > size-overhead {
				return errVisualContainer
			}
			var h [8]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return err
			}
			if format == visualFormatPNG {
				length, kind, padding = int64(binary.BigEndian.Uint32(h[:4])), string(h[4:]), 4
			} else {
				length, kind = int64(binary.LittleEndian.Uint32(h[4:])), string(h[:4])
				padding = length % 2
			}
			if length > size-offset-overhead || format == visualFormatWebP && length+padding > size-offset-8 {
				return errVisualContainer
			}
		}
		if format == visualFormatPNG {
			if kind == "IDAT" {
				offset += overhead + length
				if _, err := source.Seek(offset, io.SeekStart); err != nil {
					return err
				}
				r.Reset(visualPreviewContextReader{ctx, io.LimitReader(source, size-offset)})
				continue
			}
			pngChunks++
			if pngChunks > visualPreviewMaxPNGChunks {
				return errVisualMetadataLimit
			}
		}
		payload := &io.LimitedReader{R: r, N: length}
		if err := visit(kind, payload, length); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, payload); err != nil {
			return err
		}
		if payload.N != 0 {
			return io.ErrUnexpectedEOF
		}
		if _, err := io.CopyN(io.Discard, r, padding); err != nil {
			return err
		}
		offset += overhead + length
		if format == visualFormatWebP {
			offset += padding
		}
		if format == visualFormatPNG && kind == "IEND" {
			if length != 0 {
				return errVisualContainer
			}
			return nil
		}
	}
}

func visualPreviewWebPFlags(flags byte) (unsupportedColor, animated bool) {
	return flags&visualPreviewWebPICCProfile != 0, flags&visualPreviewWebPAnimation != 0
}
