package pdfproduction

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

const maxCompressedFontBytes = 10 << 20

// PDF 1.7 requires FlateDecode at this boundary (ISO 32000-1, 7.4 / Table 6).
// The immutable result depends only on our bundled font, never a request's
// cancellation or RSS admission. No compressed encoder identity is persisted.
var compressedFont = sync.OnceValues(func() ([]byte, error) {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, err := w.Write(fontBytes)
	if err = errors.Join(err, w.Close()); err != nil {
		return nil, err
	}
	if out.Len() > maxCompressedFontBytes {
		return nil, ErrOutputLimit
	}
	return out.Bytes(), nil
})

func storedFont(ctx context.Context, version string) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, fmt.Errorf("font storage: %w", err)
	}
	if version == historicalWriterVersion {
		return fontBytes, nil
	}
	if version != writerVersion {
		return nil, ErrVerifierVersion
	}
	data, err := compressedFont()
	if cause := context.Cause(ctx); cause != nil {
		return nil, errors.Join(err, cause)
	}
	return data, err
}

func (f *freshIndex) verifyFont(ctx context.Context, version string) error {
	prefix := fmt.Sprintf("3 0 obj\n<< /Length1 %d", len(fontBytes))
	switch version {
	case historicalWriterVersion:
	case writerVersion:
		prefix += " /Filter /FlateDecode"
	default:
		return ErrVerifierVersion
	}
	prefix += " /Length "
	r := bufio.NewReader(contextReader{ctx, f.section(3)})
	if err := expectFreshBytes(r, prefix); err != nil {
		return err
	}
	// A bounded decimal followed by the exact dictionary/stream delimiter.
	var digits []byte
	for range 20 {
		b, err := r.ReadByte()
		if err != nil {
			return flateStreamError(err)
		}
		if b == ' ' {
			break
		}
		if b < '0' || b > '9' {
			return pdfIntegrity("invalid font stream length")
		}
		digits = append(digits, b)
	}
	length, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil || length <= 0 || string(digits) != strconv.FormatInt(length, 10) {
		return pdfIntegrity("invalid font stream length")
	}
	if version == historicalWriterVersion && length != int64(len(fontBytes)) ||
		version == writerVersion && length > maxCompressedFontBytes {
		return pdfIntegrity("font stream length exceeds its qualified bounds")
	}
	const suffix = ">>\nstream\n"
	if f.ends[3]-f.offsets[3] != int64(len(prefix)+len(digits)+1+len(suffix)+len(freshStreamEnd))+length {
		return pdfIntegrity("invalid embedded font envelope")
	}
	if err := expectFreshBytes(r, suffix); err != nil {
		return err
	}
	input := io.LimitReader(r, length)
	if version == writerVersion {
		err = verifyCompressedFont(input)
	} else {
		hash := sha256.New()
		if _, err = io.CopyN(hash, input, length); err == nil && hex.EncodeToString(hash.Sum(nil)) != fontSHA256 {
			err = pdfIntegrity("fresh embedded font identity mismatch")
		}
	}
	if err != nil {
		return err
	}
	return expectFreshBytes(r, freshStreamEnd)
}

func verifyCompressedFont(input io.Reader) (result error) {
	// Both decoder and boundary check use the same ByteReader. Buffered read-ahead
	// cannot hide a second stream or payload inside the declared Length.
	r := bufio.NewReader(input)
	header, err := r.Peek(2)
	if err != nil {
		return flateStreamError(err)
	}
	if header[1]&0x20 != 0 {
		return pdfIntegrity("font stream uses a preset dictionary")
	}
	decoder, err := zlib.NewReader(r)
	if err != nil {
		return flateStreamError(err)
	}
	defer func() { result = errors.Join(result, flateStreamError(decoder.Close())) }()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(decoder, int64(len(fontBytes))+1))
	if err != nil {
		return flateStreamError(err)
	}
	if n != int64(len(fontBytes)) {
		return pdfIntegrity("decoded font length differs")
	}
	if hex.EncodeToString(hash.Sum(nil)) != fontSHA256 {
		return pdfIntegrity("fresh embedded font identity mismatch")
	}
	if _, err := r.ReadByte(); err == nil {
		return pdfIntegrity("font has bytes after its zlib trailer")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read font stream boundary: %w", err)
	}
	return nil
}
