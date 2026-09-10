package emailmime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

var errDisplayBudget = errors.New("email decoded display exceeds its byte budget")
var errInvalidEncodedWord = errors.New("invalid RFC 2047 encoded-word")

type boundedStringWriter struct {
	b     strings.Builder
	limit int64
}

func (w *boundedStringWriter) Write(p []byte) (int, error) {
	if int64(w.b.Len())+int64(len(p)) > w.limit {
		return 0, errDisplayBudget
	}
	_, _ = w.b.Write(p)
	return len(p), nil
}

func (w *boundedStringWriter) WriteString(value string) error {
	if int64(w.b.Len())+int64(len(value)) > w.limit {
		return errDisplayBudget
	}
	_, _ = w.b.WriteString(value)
	return nil
}

func (w *boundedStringWriter) WriteByte(value byte) error {
	if int64(w.b.Len())+1 > w.limit {
		return errDisplayBudget
	}
	_ = w.b.WriteByte(value)
	return nil
}

func (w *boundedStringWriter) String() string { return w.b.String() }

func decodeHeaderBounded(header string, limit int64) (string, error) {
	if limit < 0 {
		return "", errDisplayBudget
	}
	first := strings.Index(header, "=?")
	if first == -1 {
		var out boundedStringWriter
		out.limit = limit
		if err := out.WriteString(header); err != nil {
			return "", err
		}
		return out.String(), nil
	}
	var out boundedStringWriter
	out.limit = limit
	if err := out.WriteString(header[:first]); err != nil {
		return "", err
	}
	header = header[first:]
	betweenWords := false
	for {
		start := strings.Index(header, "=?")
		if start == -1 {
			break
		}
		cur := start + 2
		i := strings.Index(header[cur:], "?")
		if i == -1 {
			break
		}
		charsetName := header[cur : cur+i]
		cur += i + 1
		if len(header) < cur+4 {
			break
		}
		encoding := header[cur]
		cur++
		if header[cur] != '?' {
			break
		}
		cur++
		j := strings.Index(header[cur:], "?=")
		if j == -1 {
			break
		}
		encoded := header[cur : cur+j]
		end := cur + j + 2
		prefix := ""
		if start > 0 && (!betweenWords || hasNonLinearWhitespace(header[:start])) {
			prefix = header[:start]
		}
		remaining := limit - int64(out.b.Len()) - int64(len(prefix))
		word, err := decodeEncodedWordBounded(charsetName, encoding, encoded, remaining)
		if errors.Is(err, errInvalidEncodedWord) {
			betweenWords = false
			if err := out.WriteString(header[:end]); err != nil {
				return "", err
			}
			header = header[end:]
			continue
		}
		if err != nil {
			return "", err
		}
		if prefix != "" {
			if err := out.WriteString(prefix); err != nil {
				return "", err
			}
		}
		if err := out.WriteString(word); err != nil {
			return "", err
		}
		header = header[end:]
		betweenWords = true
	}
	if err := out.WriteString(header); err != nil {
		return "", err
	}
	return out.String(), nil
}

func decodeEncodedWordBounded(charsetName string, encoding byte, encoded string, limit int64) (string, error) {
	if charsetName == "" || !validEncodedText(encoding, encoded) {
		return "", errInvalidEncodedWord
	}
	reader := encodedTextReader(encoding, encoded)
	var out boundedStringWriter
	out.limit = limit
	switch {
	case strings.EqualFold(charsetName, "utf-8"):
		if err := copyBounded(&out, reader); err != nil {
			return "", err
		}
	case strings.EqualFold(charsetName, "iso-8859-1"):
		if err := copySingleByteCharset(&out, reader, false); err != nil {
			return "", err
		}
	case strings.EqualFold(charsetName, "us-ascii"):
		if err := copySingleByteCharset(&out, reader, true); err != nil {
			return "", err
		}
	default:
		converted, err := charset.NewReaderLabel(strings.ToLower(charsetName), reader)
		if err != nil {
			return "", fmt.Errorf("open encoded-word charset %q: %w", charsetName, err)
		}
		if err = copyBounded(&out, converted); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func validEncodedText(encoding byte, value string) bool {
	switch encoding {
	case 'B', 'b':
		_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
		return err == nil
	case 'Q', 'q':
		for i := 0; i < len(value); i++ {
			switch c := value[i]; {
			case c == '_', (c >= ' ' && c <= '~'), c == '\n', c == '\r', c == '\t':
				if c == '=' {
					if i+2 >= len(value) || !validHexPair(value[i+1], value[i+2]) {
						return false
					}
					i += 2
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func encodedTextReader(encoding byte, value string) io.Reader {
	if encoding == 'B' || encoding == 'b' {
		return base64.NewDecoder(base64.StdEncoding, strings.NewReader(value))
	}
	return &qEncodedWordReader{value: value}
}

type qEncodedWordReader struct {
	value string
	index int
}

func (r *qEncodedWordReader) Read(p []byte) (int, error) {
	if r.index == len(r.value) {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.index < len(r.value) {
		c := r.value[r.index]
		r.index++
		switch c {
		case '_':
			p[n] = ' '
		case '=':
			high, _ := hexValue(r.value[r.index])
			low, _ := hexValue(r.value[r.index+1])
			p[n] = high<<4 | low
			r.index += 2
		default:
			p[n] = c
		}
		n++
	}
	return n, nil
}

func copyBounded(dst *boundedStringWriter, src io.Reader) error {
	buffer := make([]byte, 4096)
	for {
		remaining := dst.limit - int64(dst.b.Len())
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining + 1
		}
		if readSize < 1 {
			readSize = 1
		}
		n, err := src.Read(buffer[:readSize])
		if n > 0 {
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func copySingleByteCharset(dst *boundedStringWriter, src io.Reader, ascii bool) error {
	var input [1]byte
	var output [utf8.UTFMax]byte
	for {
		n, err := src.Read(input[:])
		if n > 0 {
			r := rune(input[0])
			if ascii && input[0] >= utf8.RuneSelf {
				r = unicode.ReplacementChar
			}
			written := utf8.EncodeRune(output[:], r)
			if _, writeErr := dst.Write(output[:written]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func hasNonLinearWhitespace(value string) bool {
	for _, c := range []byte(value) {
		switch c {
		case ' ', '\t', '\n', '\r':
		default:
			return true
		}
	}
	return false
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	default:
		return 0, false
	}
}

func validHexPair(high, low byte) bool {
	_, highOK := hexValue(high)
	_, lowOK := hexValue(low)
	return highOK && lowOK
}

func boundedUTF8(value string, limit int64) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("decoded display is invalid UTF-8")
	}
	if int64(len(value)) > limit {
		return "", errDisplayBudget
	}
	return value, nil
}
