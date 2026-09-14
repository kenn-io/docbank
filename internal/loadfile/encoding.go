package loadfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

const validationReadSize = 4096

const encodingNameUTF8 = "utf-8"

type sourceEncoding uint8

const (
	encodingUTF8 sourceEncoding = iota
	encodingUTF16LE
	encodingUTF16BE
	encodingWindows1252
	encodingISO88591
)

func Decoder(name string) (func(io.Reader) io.Reader, error) {
	var (
		kind      sourceEncoding
		converter func(io.Reader) io.Reader
	)
	switch name {
	case encodingNameUTF8, "utf-8-bom":
		kind = encodingUTF8
		converter = func(r io.Reader) io.Reader { return r }
	case "utf-16le":
		kind = encodingUTF16LE
		converter = func(reader io.Reader) io.Reader {
			return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().Reader(reader)
		}
	case "utf-16be":
		kind = encodingUTF16BE
		converter = func(reader io.Reader) io.Reader {
			return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder().Reader(reader)
		}
	case "windows-1252":
		kind = encodingWindows1252
		converter = charmap.Windows1252.NewDecoder().Reader
	case "iso-8859-1":
		kind = encodingISO88591
		converter = charmap.ISO8859_1.NewDecoder().Reader
	default:
		return nil, fmt.Errorf("%w: unsupported encoding %q", ErrInvalidProfile, name)
	}

	return func(source io.Reader) io.Reader {
		bomChecked := &bomReader{source: source, declared: name}
		validated := &validatingReader{source: bomChecked, kind: kind}
		return converter(validated)
	}, nil
}

type bomReader struct {
	source      io.Reader
	declared    string
	initialized bool
	prefix      []byte
	terminalErr error
	emptyReads  int
}

func (r *bomReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !r.initialized {
		if err := r.initialize(); err != nil {
			return 0, err
		}
	}
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return 0, err
	}
	return r.source.Read(p)
}

func (r *bomReader) initialize() error {
	r.initialized = true
	prefix := make([]byte, 4)
	n := 0
	for n < len(prefix) {
		read, err := r.source.Read(prefix[n:])
		if read > 0 {
			n += read
			r.emptyReads = 0
		} else if err == nil {
			r.emptyReads++
			if r.emptyReads >= 100 {
				err = io.ErrNoProgress
			}
		}
		if err != nil {
			r.terminalErr = err
			break
		}
	}
	prefix = prefix[:n]

	utf32LEBOM := bytes.HasPrefix(prefix, []byte{0xff, 0xfe, 0x00, 0x00})
	utf32BEBOM := bytes.HasPrefix(prefix, []byte{0x00, 0x00, 0xfe, 0xff})
	if utf32LEBOM || utf32BEBOM {
		return fmt.Errorf("%w: unsupported UTF-32 BOM", ErrMalformedInput)
	}

	utf8BOM := bytes.HasPrefix(prefix, []byte{0xef, 0xbb, 0xbf})
	utf16LEBOM := bytes.HasPrefix(prefix, []byte{0xff, 0xfe})
	utf16BEBOM := bytes.HasPrefix(prefix, []byte{0xfe, 0xff})
	hasBOM := utf8BOM || utf16LEBOM || utf16BEBOM

	switch r.declared {
	case "utf-8-bom":
		if !utf8BOM {
			return fmt.Errorf("%w: declared UTF-8 BOM is absent or disagrees", ErrMalformedInput)
		}
		prefix = prefix[3:]
	case encodingNameUTF8:
		if hasBOM {
			return fmt.Errorf("%w: BOM disagrees with declared UTF-8", ErrMalformedInput)
		}
	case "utf-16le":
		if utf8BOM || utf16BEBOM {
			return fmt.Errorf("%w: BOM disagrees with declared UTF-16LE", ErrMalformedInput)
		}
		if utf16LEBOM {
			prefix = prefix[2:]
		}
	case "utf-16be":
		if utf8BOM || utf16LEBOM {
			return fmt.Errorf("%w: BOM disagrees with declared UTF-16BE", ErrMalformedInput)
		}
		if utf16BEBOM {
			prefix = prefix[2:]
		}
	default:
		if hasBOM {
			return fmt.Errorf("%w: Unicode BOM disagrees with declared %s", ErrMalformedInput, r.declared)
		}
	}
	r.prefix = prefix
	return nil
}

type validatingReader struct {
	source      io.Reader
	kind        sourceEncoding
	pending     []byte
	ready       []byte
	terminalErr error
	emptyReads  int
}

func (r *validatingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 && r.terminalErr == nil {
		r.fill()
	}
	if len(r.ready) > 0 {
		n := copy(p, r.ready)
		r.ready = r.ready[n:]
		return n, nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return 0, err
	}
	return 0, nil
}

func (r *validatingReader) fill() {
	var block [validationReadSize]byte
	n, sourceErr := r.source.Read(block[:])
	if n == 0 && sourceErr == nil {
		r.emptyReads++
		if r.emptyReads >= 100 {
			r.terminalErr = io.ErrNoProgress
		}
		return
	}
	if n > 0 {
		r.emptyReads = 0
		r.pending = append(r.pending, block[:n]...)
	}

	validBytes, invalid := r.validPrefix()
	if validBytes > 0 {
		r.ready = append(r.ready[:0], r.pending[:validBytes]...)
		r.pending = append(r.pending[:0], r.pending[validBytes:]...)
	}
	if invalid {
		r.pending = r.pending[:0]
		r.terminalErr = ErrMalformedInput
		return
	}
	if sourceErr == nil {
		return
	}
	if sourceErr == io.EOF && len(r.pending) > 0 {
		r.pending = r.pending[:0]
		r.terminalErr = ErrMalformedInput
		return
	}
	r.pending = r.pending[:0]
	r.terminalErr = sourceErr
}

func (r *validatingReader) validPrefix() (validBytes int, invalid bool) {
	switch r.kind {
	case encodingUTF8:
		for validBytes < len(r.pending) {
			remaining := r.pending[validBytes:]
			if !utf8.FullRune(remaining) {
				return validBytes, false
			}
			runeValue, size := utf8.DecodeRune(remaining)
			if runeValue == utf8.RuneError && size == 1 {
				return validBytes, true
			}
			validBytes += size
		}
		return validBytes, false
	case encodingUTF16LE:
		valid, incomplete := validUTF16Prefix(r.pending, binary.LittleEndian)
		return len(r.pending) - incomplete, !valid
	case encodingUTF16BE:
		valid, incomplete := validUTF16Prefix(r.pending, binary.BigEndian)
		return len(r.pending) - incomplete, !valid
	case encodingWindows1252:
		for index, value := range r.pending {
			if undefinedWindows1252(value) {
				return index, true
			}
		}
		return len(r.pending), false
	case encodingISO88591:
		return len(r.pending), false
	default:
		return 0, true
	}
}

func validUTF16Prefix(raw []byte, order binary.ByteOrder) (valid bool, incomplete int) {
	for index := 0; index < len(raw); {
		if len(raw)-index < 2 {
			return true, len(raw) - index
		}
		unit := order.Uint16(raw[index : index+2])
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			if len(raw)-index < 4 {
				return true, len(raw) - index
			}
			next := order.Uint16(raw[index+2 : index+4])
			if next < 0xdc00 || next > 0xdfff {
				return false, len(raw) - index
			}
			index += 4
		case unit >= 0xdc00 && unit <= 0xdfff:
			return false, len(raw) - index
		default:
			index += 2
		}
	}
	return true, 0
}

func undefinedWindows1252(value byte) bool {
	switch value {
	case 0x81, 0x8d, 0x8f, 0x90, 0x9d:
		return true
	default:
		return false
	}
}
