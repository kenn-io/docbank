package emailmime

// See LICENSE.multipart.txt for the Go BSD license.

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// addressWordCursor presents optional comment quoted-pair removal as a view
// over the original word. Neither syntax validation nor encoded-word counting
// allocates a buffer, even for discarded group names or exhausted budgets.
// Only final decoded bytes reach the capacity-checked addressTextSink.
type addressWordCursor struct {
	raw      string
	unescape bool
}

func (c *addressWordCursor) next() (byte, bool) {
	if len(c.raw) == 0 {
		return 0, false
	}
	if c.unescape && c.raw[0] == '\\' && len(c.raw) > 1 {
		c.raw = c.raw[1:]
	}
	value := c.raw[0]
	c.raw = c.raw[1:]
	return value, true
}

func (c *addressWordCursor) consume(want byte) bool {
	value, ok := c.next()
	return ok && value == want
}

func (c *addressWordCursor) cut(delimiter byte) (addressWordCursor, bool) {
	start := *c
	for len(c.raw) > 0 {
		end := len(start.raw) - len(c.raw)
		value, _ := c.next()
		if value == delimiter {
			start.raw = start.raw[:end]
			return start, true
		}
	}
	return addressWordCursor{}, false
}

func (c *addressWordCursor) equalFold(want string) bool {
	cursor := *c
	// A Unicode fold of one of our ASCII charset names takes at most four
	// bytes per letter. Longer labels cannot match; this bounds comparison
	// storage, not the grammar or display capacity of an unknown label.
	var label [utf8.UTFMax * len("iso-8859-1")]byte
	size := 0
	for value, ok := cursor.next(); ok; value, ok = cursor.next() {
		if size == len(label) {
			return false
		}
		label[size] = value
		size++
	}
	return strings.EqualFold(string(label[:size]), want)
}

type addressWordCharset uint8

const (
	addressWordUTF8 addressWordCharset = iota
	addressWordLatin1
	addressWordASCII
)

func writeAddressEncodedCursor(out *addressTextSink, word addressWordCursor) (bool, error) {
	if !word.consume('=') || !word.consume('?') {
		return false, nil
	}
	charset, ok := word.cut('?')
	if !ok || len(charset.raw) == 0 {
		return false, nil
	}
	encoding, ok := word.next()
	if !ok || !word.consume('?') {
		return false, nil
	}
	text, ok := word.cut('?')
	if !ok || !word.consume('=') || len(word.raw) != 0 {
		return false, nil
	}
	// Validate the complete transfer syntax before writing: malformed encoded
	// words remain literal in net/mail, including a malformed tail after valid
	// decoded bytes. The validation cursor uses only a four-byte quantum.
	if valid := writeAddressWordText(nil, text, encoding, addressWordUTF8) == nil; !valid {
		return false, nil
	}
	var kind addressWordCharset
	switch {
	case charset.equalFold("utf-8"):
		kind = addressWordUTF8
	case charset.equalFold("iso-8859-1"):
		kind = addressWordLatin1
	case charset.equalFold("us-ascii"):
		kind = addressWordASCII
	default:
		var label [128]byte
		size := 0
		for size < len(label) {
			value, ok := charset.next()
			if !ok {
				break
			}
			label[size] = value
			size++
		}
		return true, fmt.Errorf("mail: unsupported encoded-word charset %q", label[:size])
	}
	return true, writeAddressWordText(out, text, encoding, kind)
}

func writeAddressWordBytes(out *addressTextSink, value []byte, charset addressWordCharset) error {
	if out == nil {
		return nil
	}
	if charset == addressWordUTF8 {
		return out.writeBytes(value)
	}
	for _, b := range value {
		r := rune(b)
		if charset == addressWordASCII && b >= utf8.RuneSelf {
			r = unicode.ReplacementChar
		}
		if err := out.writeRune(r); err != nil {
			return err
		}
	}
	return nil
}

func writeAddressWordText(out *addressTextSink, text addressWordCursor, encoding byte, charset addressWordCharset) error {
	switch encoding {
	case 'B', 'b':
		return writeAddressBase64(out, text, charset)
	case 'Q', 'q':
		for value, ok := text.next(); ok; value, ok = text.next() {
			switch {
			case value == '_':
				value = ' '
			case value == '=':
				high, highOK := text.next()
				low, lowOK := text.next()
				if !highOK || !lowOK || !validHexPair(high, low) {
					return errInvalidEncodedWord
				}
				h, _ := hexValue(high)
				l, _ := hexValue(low)
				value = h<<4 | l
			case value >= ' ' && value <= '~', value == '\n', value == '\r', value == '\t':
			default:
				return errInvalidEncodedWord
			}
			one := [1]byte{value}
			if err := writeAddressWordBytes(out, one[:], charset); err != nil {
				return err
			}
		}
		return nil
	default:
		return errInvalidEncodedWord
	}
}

func writeAddressBase64(out *addressTextSink, text addressWordCursor, charset addressWordCharset) error {
	var quantum [4]byte
	var decoded [3]byte
	size := 0
	padded := false
	for value, ok := text.next(); ok; value, ok = text.next() {
		if value == '\r' || value == '\n' {
			continue
		}
		if padded {
			return errInvalidEncodedWord
		}
		quantum[size] = value
		size++
		if size < len(quantum) {
			continue
		}
		n, err := base64.StdEncoding.Decode(decoded[:], quantum[:])
		if err != nil {
			return errInvalidEncodedWord
		}
		if err := writeAddressWordBytes(out, decoded[:n], charset); err != nil {
			return err
		}
		padded = n < len(decoded)
		size = 0
	}
	if size != 0 {
		return errInvalidEncodedWord
	}
	return nil
}
