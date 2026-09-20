package mailbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/docbank/internal/store"
)

// Separator layouts and explicit From-line dialect handling are narrowly
// adapted from kenn-io/msgvault 98ef7c85e4f8585be35759594d119c70faeca4e3
// (MIT; see NOTICE). Storage and bounded scanning are Docbank's own.
var separatorLayouts = []string{
	"Mon Jan 2 15:04:05 2006", "Mon Jan 2 15:04:05 -0700 2006", "Mon Jan 2 15:04:05 -07:00 2006", "Mon Jan 2 15:04:05 MST 2006",
	"Mon Jan 2 15:04:05 2006 -0700", "Mon Jan 2 15:04:05 2006 -07:00", "Mon Jan 2 15:04:05 2006 MST",
	"Mon Jan 2 15:04 2006", "Mon Jan 2 15:04 -0700 2006", "Mon Jan 2 15:04 MST 2006",
	"Jan 2 15:04:05 2006", "Jan 2 15:04:05 -0700 2006", "Jan 2 15:04:05 MST 2006",
}

func validSeparator(line []byte) bool {
	if len(line) > 4096 || !bytes.HasPrefix(line, []byte("From ")) {
		return false
	}
	fields := strings.Fields(string(line))
	if len(fields) < 6 {
		return false
	}
	date := strings.Join(fields[2:], " ")
	for _, layout := range separatorLayouts {
		if _, err := time.Parse(layout, date); err == nil {
			return true
		}
	}
	return false
}

type Message struct {
	Sequence  int64                  `json:"sequence"`
	Start     int64                  `json:"start"`
	End       int64                  `json:"end"`
	Separator string                 `json:"separator"`
	RawSHA256 string                 `json:"raw_sha256"`
	EMLSHA256 string                 `json:"eml_sha256"`
	EMLSize   int64                  `json:"eml_size"`
	Rejection string                 `json:"rejection,omitempty"`
	EML       *emailmime.SourceSpool `json:"-"`
}

func (m *Message) Close() error {
	if m.EML == nil {
		return nil
	}
	f := m.EML
	m.EML = nil
	return f.Close()
}

type Scanner struct {
	r                       *bufio.Reader
	dialect, spool          string
	limit, offset, sequence int64
	pending                 []byte
	pendingOffset           int64
	eof                     bool
}

func NewScanner(r io.Reader, dialect, spool string, limit int64) (*Scanner, error) {
	if dialect == "" {
		dialect = "mboxrd"
	}
	if (dialect != "mboxrd" && dialect != "mboxo") || limit <= 0 || limit > 128<<20 {
		return nil, store.ErrMailboxInvalid
	}
	return &Scanner{r: bufio.NewReaderSize(r, 64<<10), dialect: dialect, spool: spool, limit: limit}, nil
}
func (s *Scanner) Next(ctx context.Context) (_ *Message, retErr error) {
	if s.eof {
		return nil, io.EOF
	}
	start := s.pendingOffset
	sep := s.pending
	s.pending = nil
	if sep == nil {
		line, err := s.r.ReadSlice('\n')
		start = s.offset
		s.offset += int64(len(line))
		if errors.Is(err, io.EOF) && len(line) == 0 {
			s.eof = true
			if s.sequence == 0 {
				return nil, store.ErrMailboxInvalid
			}
			return nil, io.EOF
		}
		if (err != nil && !errors.Is(err, io.EOF)) || !validSeparator(line) {
			return nil, store.ErrMailboxInvalid
		}
		sep = bytes.Clone(line)
	}
	s.sequence++
	m := &Message{Sequence: s.sequence, Start: start, Separator: string(sep)}
	var err error
	m.EML, err = emailmime.NewSourceSpool(s.spool)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, m.Close())
		}
	}()
	raw := sha256.New()
	_, _ = raw.Write(sep)
	eml := sha256.New()
	writer := &messageWriter{m: m, hash: eml, limit: s.limit}
	atStart := true
	transform := lineUnescaper{writer: writer, dialect: s.dialect}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		offset := s.offset
		fragment, readErr := s.r.ReadSlice('\n')
		s.offset += int64(len(fragment))
		if atStart && !errors.Is(readErr, bufio.ErrBufferFull) && validSeparator(fragment) {
			s.pending = bytes.Clone(fragment)
			s.pendingOffset = offset
			m.End = offset
			break
		}
		if len(fragment) > 0 {
			_, _ = raw.Write(fragment)
			if _, err = transform.Write(fragment); err != nil {
				return nil, err
			}
		}
		if !errors.Is(readErr, bufio.ErrBufferFull) {
			if err = transform.finish(); err != nil {
				return nil, err
			}
			transform = lineUnescaper{writer: writer, dialect: s.dialect}
			atStart = true
		} else {
			atStart = false
		}
		if errors.Is(readErr, io.EOF) {
			s.eof = true
			m.End = s.offset
			break
		}
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("read mailbox fragment: %w", readErr)
		}
	}
	m.RawSHA256 = hex.EncodeToString(raw.Sum(nil))
	m.EMLSHA256 = hex.EncodeToString(eml.Sum(nil))
	if m.EMLSize == 0 {
		m.Rejection = "empty_message"
	}
	if m.Rejection != "" {
		if err = m.Close(); err != nil {
			return nil, err
		}
		return m, nil
	}
	if _, err = m.EML.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return m, nil
}

type messageWriter struct {
	m     *Message
	hash  hash.Hash
	limit int64
}

func (w *messageWriter) Write(p []byte) (int, error) {
	_, _ = w.hash.Write(p)
	w.m.EMLSize += int64(len(p))
	if w.m.Rejection != "" {
		return len(p), nil
	}
	if w.m.EMLSize > w.limit {
		w.m.Rejection = "message_too_large"
		return len(p), nil
	}
	n, err := w.m.EML.Write(p)
	return n, err
}

type lineUnescaper struct {
	writer  io.Writer
	dialect string
	quotes  int64
	probe   []byte
	decided bool
}

func (l *lineUnescaper) Write(p []byte) (int, error) {
	count := len(p)
	for len(p) > 0 && !l.decided {
		b := p[0]
		p = p[1:]
		if len(l.probe) == 0 && b == '>' {
			l.quotes++
			continue
		}
		l.probe = append(l.probe, b)
		if l.quotes == 0 || !bytes.Equal(l.probe, []byte("From ")[:len(l.probe)]) || len(l.probe) == 5 {
			if err := l.flush(); err != nil {
				return 0, err
			}
		}
	}
	if len(p) > 0 {
		if _, err := l.writer.Write(p); err != nil {
			return 0, err
		}
	}
	return count, nil
}
func (l *lineUnescaper) flush() error {
	q := l.quotes
	if bytes.Equal(l.probe, []byte("From ")) && q > 0 && (l.dialect == "mboxrd" || q == 1) {
		q--
	}
	block := bytes.Repeat([]byte{'>'}, 4096)
	for q > 0 {
		n := min(q, int64(len(block)))
		if _, err := l.writer.Write(block[:n]); err != nil {
			return err
		}
		q -= n
	}
	if _, err := l.writer.Write(l.probe); err != nil {
		return err
	}
	l.decided = true
	return nil
}
func (l *lineUnescaper) finish() error {
	if !l.decided {
		return l.flush()
	}
	return nil
}
