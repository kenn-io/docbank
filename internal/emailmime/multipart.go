package emailmime

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

const multipartPeekBufferSize = 4096

var errBoundaryUnclosed = errors.New("email MIME multipart boundary is unclosed")

// multipartInput records raw header offsets while mime/multipart owns parsing.
// Read stops at each newline to keep the standard reader from prefetching body
// bytes during NextRawPart. Offsets refer to the already verified spool file;
// no message-sized buffer or private multipart grammar is needed.
type multipartInput struct {
	ctx                 context.Context
	reader              *bufio.Reader
	newline             string
	boundary            string
	pending             []byte
	readErr             error
	lineStart           bool
	offset, headerStart int64
}

func (r *multipartInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.readErr != nil {
			return 0, r.readErr
		}
		line, err := r.reader.ReadSlice('\n')
		if rest, ok := strings.CutPrefix(string(line), r.boundary); r.lineStart && ok {
			ending := ""
			if strings.HasSuffix(rest, "\r\n") {
				ending = "\r\n"
			} else if strings.HasSuffix(rest, "\n") {
				ending = "\n"
			}
			if ending != "" && (r.newline == "" || r.newline == ending) && strings.Trim(strings.TrimSuffix(rest, ending), " \t") == "" {
				r.newline = ending
				r.headerStart = r.offset + int64(len(line))
			}
		}
		r.lineStart = !errors.Is(err, bufio.ErrBufferFull)
		if r.lineStart {
			r.readErr = err
		}
		r.pending = line
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	r.offset += int64(n)
	if n > 0 {
		return n, nil
	}
	return 0, r.readErr
}

// An incomplete MIME part is a syntax refusal, not a failed spool read.
type multipartPayload struct{ io.Reader }

func (r multipartPayload) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) && !hasOperationalFailure(err) {
		err = errBoundaryUnclosed
	}
	return n, err
}
