// Package strictio reads streams without losing errors that arrive with data
// or are joined to io.EOF. Only a plain io.EOF ends a stream cleanly.
package strictio

import (
	"errors"
	"io"
)

// ReadFull reads len(buf) bytes like io.ReadFull, but returns any error that
// arrives with the final bytes, such as a failed integrity check, instead of
// discarding it once buf is full.
func ReadFull(r io.Reader, buf []byte) (int, error) {
	n, err := Fill(r, buf)
	if err == io.EOF { //nolint:errorlint // a wrapped or joined EOF is not a clean end
		switch {
		case n == len(buf):
			err = nil
		case n > 0:
			err = io.ErrUnexpectedEOF
		}
	}
	return n, err
}

// Fill reads until buf is full or the stream ends. Unlike ReadFull, a clean
// end before buf is full returns a plain io.EOF, so a chunked reader can tell
// it apart from a source that reported io.ErrUnexpectedEOF itself. Any error
// that arrives with the final bytes is returned with them.
func Fill(r io.Reader, buf []byte) (int, error) {
	var n int
	var err error
	for n < len(buf) && err == nil {
		var k int
		k, err = r.Read(buf[n:])
		n += k
	}
	return n, err
}

// IgnoreEOF returns nil for a plain io.EOF and err otherwise, so a cause
// joined to EOF is never mistaken for a clean end.
func IgnoreEOF(err error) error {
	if err == io.EOF { //nolint:errorlint // a wrapped or joined EOF is not a clean end
		return nil
	}
	return err
}

// RangeEnd reports err from a read that left remaining bytes of a fixed-length
// range unread. An end of stream before the range is complete becomes
// io.ErrUnexpectedEOF, keeping any cause joined to the EOF.
func RangeEnd(err error, remaining int64) error {
	if remaining == 0 || !errors.Is(err, io.EOF) {
		return err
	}
	if err == io.EOF { //nolint:errorlint // only a plain EOF has no other cause
		return io.ErrUnexpectedEOF
	}
	return errors.Join(io.ErrUnexpectedEOF, err)
}
