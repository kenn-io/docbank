package emailmime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"
)

var errTransferUnsupported = errors.New("email MIME transfer encoding is unsupported")

type infrastructureReadError struct{ err error }

func (e *infrastructureReadError) Error() string {
	return fmt.Sprintf("read email MIME spool: %v", e.err)
}
func (e *infrastructureReadError) Unwrap() error { return e.err }

type infrastructureMarkingReader struct{ reader io.Reader }

func (r infrastructureMarkingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errBoundaryUnclosed) {
		return n, &infrastructureReadError{err: err}
	}
	return n, err
}

func transferDecodedReader(reader io.Reader, encoding string) (io.Reader, error) {
	marked := infrastructureMarkingReader{reader: reader}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return marked, nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, marked), nil
	case "quoted-printable":
		return quotedprintable.NewReader(marked), nil
	default:
		return nil, errTransferUnsupported
	}
}

func hasOperationalFailure(err error) bool {
	if err == nil {
		return false
	}
	var infrastructure *infrastructureReadError
	var storage *spoolIOError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &infrastructure) || errors.As(err, &storage)
}

func malformedTransferError(err error, encoding string) bool {
	if hasOperationalFailure(err) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		var corrupt base64.CorruptInputError
		return errors.As(err, &corrupt) || errors.Is(err, io.ErrUnexpectedEOF)
	case "quoted-printable":
		return strings.HasPrefix(err.Error(), "quotedprintable:") || errors.Is(err, io.ErrUnexpectedEOF)
	default:
		return false
	}
}

func drainPart(ctxReader io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		n, err := ctxReader.Read(buffer)
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
