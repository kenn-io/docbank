// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license reproduced in
// LICENSE.multipart.txt.
//
// Adapted from github.com/emersion/go-message v0.18.2
// textproto/multipart.go (SHA-256
// 1a5aa86641f98021fd6f37466c07e62f2662f4f55d88b9d09ebf032988e5d110).
// The adaptation retains only the streaming reader/boundary algorithm,
// replaces header parsing with Docbank's exact bounded reader, and removes
// implicit draining between parts.

package emailmime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const multipartPeekBufferSize = 4096

var errBoundaryUnclosed = errors.New("email MIME multipart boundary is unclosed")
var errMultipartMalformed = errors.New("email MIME multipart syntax is malformed")

type multipartPart struct {
	headers parsedHeaderBlock
	mr      *multipartReader
	n       int
	total   int64
	err     error
	readErr error
	done    bool
}

type multipartReader struct {
	bufReader        *bufio.Reader
	currentPart      *multipartPart
	partsRead        int
	nl               []byte
	nlDashBoundary   []byte
	dashBoundaryDash []byte
	dashBoundary     []byte
	readHeaders      func(*bufio.Reader) (parsedHeaderBlock, error)
}

func newMultipartReader(r io.Reader, boundary string, readHeaders func(*bufio.Reader) (parsedHeaderBlock, error)) *multipartReader {
	b := []byte("\r\n--" + boundary + "--")
	return &multipartReader{
		bufReader: bufio.NewReaderSize(&stickyErrorReader{r: r}, multipartPeekBufferSize),
		nl:        b[:2], nlDashBoundary: b[:len(b)-2], dashBoundaryDash: b[2:], dashBoundary: b[2 : len(b)-2],
		readHeaders: readHeaders,
	}
}

type stickyErrorReader struct {
	r   io.Reader
	err error
}

func (r *stickyErrorReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.r.Read(p)
	r.err = err
	return n, err
}

func (p *multipartPart) Read(d []byte) (int, error) {
	br := p.mr.bufReader
	for p.n == 0 && p.err == nil {
		peek, _ := br.Peek(br.Buffered())
		p.n, p.err = scanUntilBoundary(peek, p.mr.dashBoundary, p.mr.nlDashBoundary, p.total, p.readErr)
		if p.n == 0 && p.err == nil {
			_, p.readErr = br.Peek(len(peek) + 1)
			if errors.Is(p.readErr, io.EOF) {
				p.readErr = errBoundaryUnclosed
			}
		}
	}
	if p.n == 0 {
		p.done = true
		return 0, p.err
	}
	n := min(len(d), p.n)
	n, _ = br.Read(d[:n])
	p.total += int64(n)
	p.n -= n
	if p.n == 0 && p.err != nil {
		if errors.Is(p.err, io.EOF) {
			p.done = true
		}
		return n, p.err
	}
	return n, nil
}

func scanUntilBoundary(buf, dashBoundary, nlDashBoundary []byte, total int64, readErr error) (int, error) {
	if total == 0 {
		if bytes.HasPrefix(buf, dashBoundary) {
			switch matchAfterPrefix(buf, dashBoundary, readErr) {
			case -1:
				return len(dashBoundary), nil
			case 0:
				return 0, nil
			case 1:
				return 0, io.EOF
			}
		}
		if bytes.HasPrefix(dashBoundary, buf) {
			return 0, readErr
		}
	}
	if i := bytes.Index(buf, nlDashBoundary); i >= 0 {
		switch matchAfterPrefix(buf[i:], nlDashBoundary, readErr) {
		case -1:
			return i + len(nlDashBoundary), nil
		case 0:
			return i, nil
		case 1:
			return i, io.EOF
		}
	}
	if bytes.HasPrefix(nlDashBoundary, buf) {
		return 0, readErr
	}
	i := bytes.LastIndexByte(buf, nlDashBoundary[0])
	if i >= 0 && bytes.HasPrefix(nlDashBoundary, buf[i:]) {
		return i, nil
	}
	return len(buf), readErr
}

func matchAfterPrefix(buf, prefix []byte, readErr error) int {
	if len(buf) == len(prefix) {
		if readErr != nil {
			return 1
		}
		return 0
	}
	c := buf[len(prefix)]
	if c == '-' {
		if len(buf) == len(prefix)+1 {
			if readErr == nil {
				return 0
			}
			return -1
		}
		if buf[len(prefix)+1] == '-' {
			return 1
		}
		return -1
	}
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
		return 1
	}
	return -1
}

func (r *multipartReader) NextPart() (*multipartPart, error) {
	if r.currentPart != nil && !r.currentPart.done {
		return nil, fmt.Errorf("%w: previous part was not completely consumed", errMultipartMalformed)
	}
	if string(r.dashBoundary) == "--" {
		return nil, fmt.Errorf("%w: boundary is empty", errMultipartMalformed)
	}
	expectNewPart := false
	for {
		line, err := r.readBoundaryLine()
		if errors.Is(err, io.EOF) && r.isFinalBoundary(line) {
			return nil, io.EOF
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errBoundaryUnclosed
			}
			return nil, fmt.Errorf("multipart: reading boundary: %w", err)
		}
		if r.isBoundaryDelimiterLine(line) {
			r.partsRead++
			headers, err := r.readHeaders(r.bufReader)
			if err != nil {
				return nil, err
			}
			part := &multipartPart{headers: headers, mr: r}
			r.currentPart = part
			return part, nil
		}
		if r.isFinalBoundary(line) {
			return nil, io.EOF
		}
		if expectNewPart {
			return nil, fmt.Errorf("%w: expected a new part boundary", errMultipartMalformed)
		}
		if r.partsRead == 0 {
			continue
		}
		if bytes.Equal(line, r.nl) {
			expectNewPart = true
			continue
		}
		return nil, fmt.Errorf("%w: invalid boundary delimiter", errMultipartMalformed)
	}
}

// readBoundaryLine retains only the fixed delimiter and line ending when
// transport padding crosses the reader buffer. RFC 2046 transport-padding is
// unbounded LWSP; neither legal padding nor long preamble lines need a large
// allocation. Invalid lines are drained and remain invalid, never truncated
// into an apparently valid delimiter.
func (r *multipartReader) readBoundaryLine() ([]byte, error) {
	fragment, err := r.bufReader.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return fragment, multipartBoundaryReadError(err)
	}
	prefix := r.dashBoundary
	if bytes.HasPrefix(fragment, r.dashBoundaryDash) {
		prefix = r.dashBoundaryDash
	}
	valid := bytes.HasPrefix(fragment, prefix)
	if valid {
		fragment = fragment[len(prefix):]
	}
	// ending contains at most CRLF, including a CR split across fragments.
	var ending [2]byte
	endLen := 0
	for {
		if valid {
			for _, b := range fragment {
				switch {
				case endLen == 0 && (b == ' ' || b == '\t'):
				case endLen == 0 && b == '\r':
					ending[0] = b
					endLen = 1
				case endLen == 0 && b == '\n':
					ending[0] = b
					endLen = 1
				case endLen == 1 && ending[0] == '\r' && b == '\n':
					ending[1] = b
					endLen = 2
				default:
					valid = false
				}
				if !valid {
					break
				}
			}
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			break
		}
		fragment, err = r.bufReader.ReadSlice('\n')
	}
	if !valid {
		return []byte{0}, multipartBoundaryReadError(err)
	}
	line := make([]byte, 0, len(prefix)+endLen)
	line = append(line, prefix...)
	line = append(line, ending[:endLen]...)
	return line, multipartBoundaryReadError(err)
}

func multipartBoundaryReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return fmt.Errorf("reading multipart delimiter line: %w", err)
}

func (r *multipartReader) isFinalBoundary(line []byte) bool {
	if !bytes.HasPrefix(line, r.dashBoundaryDash) {
		return false
	}
	rest := skipLWSPChar(line[len(r.dashBoundaryDash):])
	return len(rest) == 0 || bytes.Equal(rest, r.nl)
}
func (r *multipartReader) isBoundaryDelimiterLine(line []byte) bool {
	if !bytes.HasPrefix(line, r.dashBoundary) {
		return false
	}
	rest := skipLWSPChar(line[len(r.dashBoundary):])
	if r.partsRead == 0 && len(rest) == 1 && rest[0] == '\n' {
		r.nl = r.nl[1:]
		r.nlDashBoundary = r.nlDashBoundary[1:]
	}
	return bytes.Equal(rest, r.nl)
}
func skipLWSPChar(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}
