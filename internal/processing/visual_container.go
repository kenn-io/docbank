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

var errVisualContainer = errors.New("malformed image container")
var errVisualMetadataLimit = errors.New("image metadata exceeds inspection limit")

func walkVisualContainer(ctx context.Context, source io.ReadSeeker, format string, size int64, visit func(string, io.Reader, int64, int64) error) (err error) {
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
	var r io.Reader = &contextReader{ctx, io.LimitReader(source, size)}
	if format == visualFormatJPEG {
		r = bufio.NewReader(r)
	}
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
		if format == visualFormatJPEG && count >= visualPreviewMaxJPEGSegments {
			return errVisualContainer
		}
		if format == visualFormatWebP && count >= visualPreviewMaxWebPChunks {
			return errVisualMetadataLimit
		}
		if format == visualFormatWebP && offset == size {
			return nil
		}
		var kind string
		var length, overhead, padding int64
		switch format {
		case visualFormatJPEG:
			marker, err := readVisualJPEGMarker(r)
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
		if format == visualFormatPNG && kind != "IDAT" {
			pngChunks++
			if pngChunks > visualPreviewMaxPNGChunks {
				return errVisualMetadataLimit
			}
		}
		payload := &io.LimitedReader{R: r, N: length}
		if err := visit(kind, payload, length, offset); err != nil {
			return err
		}
		if format == visualFormatJPEG {
			if _, err := io.Copy(io.Discard, payload); err != nil {
				return err
			}
			if payload.N != 0 {
				return io.ErrUnexpectedEOF
			}
		} else {
			offset += overhead + length
			if format == visualFormatWebP {
				offset += padding
			}
			if _, err := source.Seek(offset, io.SeekStart); err != nil {
				return err
			}
			r = &contextReader{ctx, io.LimitReader(source, size-offset)}
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

func readVisualJPEGMarker(source io.Reader) (byte, error) {
	var value [1]byte
	for {
		if _, err := io.ReadFull(source, value[:]); err != nil {
			return 0, err
		}
		if value[0] != 0xff {
			return 0, io.ErrUnexpectedEOF
		}
		for value[0] == 0xff {
			if _, err := io.ReadFull(source, value[:]); err != nil {
				return 0, err
			}
		}
		if value[0] != 0x00 {
			return value[0], nil
		}
	}
}
