package spill

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"math"
	"os"
	"slices"

	"go.kenn.io/kit/safefileio"
)

// bufferSize is the size of the file writer and reader buffers, which bound
// the memory a List uses after it moves to disk.
const bufferSize = 64 << 10

// recordOverhead is the size of a string header on 64-bit platforms. Each
// record held in memory counts it toward the limit, so a list of short
// records cannot hold far more memory than the limit says.
const recordOverhead = 16

// List is an append-only list of strings that moves from memory to a private
// temporary file once it passes a byte limit. Create one with New.
type List struct {
	dir     string
	pattern string
	limit   int64

	// records holds the list until it moves to the file.
	records []string
	count   int
	size    int64

	file *os.File
	w    *bufio.Writer
	// written counts the bytes written to w, buffered or not.
	written int64
	// err is the first write or flush error, after which the file no longer
	// matches the records appended.
	err    error
	closed bool
}

// New returns an empty List that keeps records in memory while their lengths
// plus 16 bytes per record total at most limit bytes, and otherwise moves
// them to a file created in dir with a name built from pattern, as in
// os.CreateTemp. An empty dir means os.TempDir. A negative limit sends every
// record to the file. New creates no file.
func New(dir, pattern string, limit int64) *List {
	return &List{dir: dir, pattern: pattern, limit: limit}
}

// Append adds s to the end of the list. When s would take the list past
// the limit, Append first moves the list to the file; if that fails, the
// list is unchanged and s is not added. A failed write after the move is
// returned by every later Append and All.
func (l *List) Append(s string) error {
	if l.closed {
		return fmt.Errorf("spill: append: %w", fs.ErrClosed)
	}
	if l.err != nil {
		return l.err
	}
	// On 32-bit platforms the count can reach math.MaxInt; one more record
	// would wrap Len negative.
	if l.count == math.MaxInt {
		return fmt.Errorf("spill: append: list already holds %d records", l.count)
	}
	if l.file == nil {
		if l.size+int64(l.count+1)*recordOverhead+int64(len(s)) <= l.limit {
			l.records = append(l.records, s)
			l.count++
			l.size += int64(len(s))
			return nil
		}
		if err := l.spill(); err != nil {
			return err
		}
	}
	if err := l.write(s); err != nil {
		l.err = err
		return err
	}
	l.count++
	l.size += int64(len(s))
	return nil
}

// spill creates the file and writes the buffered records to it. On failure
// it removes the file and leaves the list in memory.
func (l *List) spill() error {
	file, err := safefileio.CreatePrivateTemp(l.dir, l.pattern)
	if err != nil {
		return fmt.Errorf("spill: creating file: %w", err)
	}
	l.file = file
	l.w = bufio.NewWriterSize(file, bufferSize)
	for _, record := range l.records {
		if err := l.write(record); err != nil {
			l.file, l.w, l.written = nil, nil, 0
			return errors.Join(err, removeFile(file))
		}
	}
	l.records = nil
	return nil
}

func (l *List) write(s string) error {
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], uint64(len(s)))
	if _, err := l.w.Write(prefix[:n]); err != nil {
		return fmt.Errorf("spill: writing record: %w", err)
	}
	if _, err := l.w.WriteString(s); err != nil {
		return fmt.Errorf("spill: writing record: %w", err)
	}
	l.written += int64(n) + int64(len(s))
	return nil
}

// All returns an iterator over the records in insertion order. It yields
// the records appended before the iteration started. Once the list is on
// disk, an iteration first flushes buffered writes, then reads the file; a
// flush, read, or decoding error is yielded once with an empty string and
// ends the iteration.
func (l *List) All() iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		switch {
		case l.closed:
			yield("", fmt.Errorf("spill: read: %w", fs.ErrClosed))
			return
		case l.err != nil:
			yield("", l.err)
			return
		case l.file == nil:
			for _, record := range l.records {
				if !yield(record, nil) {
					return
				}
			}
			return
		}
		if err := l.w.Flush(); err != nil {
			l.err = fmt.Errorf("spill: flushing file: %w", err)
			yield("", l.err)
			return
		}
		if err := readFile(l.file, l.written, l.count, l.size, yield); err != nil {
			yield("", err)
		}
	}
}

// readFile yields count records from the first size bytes of file, which
// must hold exactly payload bytes of record data. It returns nil when yield
// stops the iteration.
func readFile(file *os.File, size int64, count int, payload int64, yield func(string, error) bool) error {
	section := io.NewSectionReader(file, 0, size)
	r := bufio.NewReaderSize(section, bufferSize)
	var buf []byte
	for i := range count {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("spill: reading length of record %d of %d: %w", i+1, count, unexpectedEOF(err))
		}
		if n > uint64(payload) {
			return fmt.Errorf("spill: record %d of %d: length %d exceeds the %d payload bytes left", i+1, count, n, payload)
		}
		if n > math.MaxInt {
			return fmt.Errorf("spill: record %d of %d: length %d does not fit in an int", i+1, count, n)
		}
		payload -= int64(n)
		buf = slices.Grow(buf[:0], int(n))[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("spill: reading record %d of %d: %w", i+1, count, unexpectedEOF(err))
		}
		if !yield(string(buf), nil) {
			return nil
		}
	}
	if payload != 0 {
		return fmt.Errorf("spill: records hold %d fewer payload bytes than were written", payload)
	}
	// The records must end exactly where the writes did. A file cut short
	// can still decode count records when damaged lengths shrank them.
	read, err := section.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("spill: finding the end of the records: %w", err)
	}
	if end := read - int64(r.Buffered()); end != size {
		return fmt.Errorf("spill: records end at byte %d of the %d bytes written", end, size)
	}
	return nil
}

// unexpectedEOF reports a file that ends inside a record it should hold.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Len returns the number of records in the list.
func (l *List) Len() int { return l.count }

// Size returns the total payload of the records, the sum of their lengths
// in bytes.
func (l *List) Size() int64 { return l.size }

// Spilled reports whether the list has moved to its file.
func (l *List) Spilled() bool { return l.file != nil }

// Close drops the records and removes the file, reporting flush, close, and
// remove errors. Calls after the first do nothing and return nil.
func (l *List) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	l.records = nil
	if l.file == nil {
		return nil
	}
	var flushErr error
	if err := l.w.Flush(); err != nil {
		flushErr = fmt.Errorf("spill: flushing file: %w", err)
	}
	err := errors.Join(flushErr, removeFile(l.file))
	l.w = nil
	return err
}

func removeFile(file *os.File) error {
	var closeErr, removeErr error
	if err := file.Close(); err != nil {
		closeErr = fmt.Errorf("spill: closing file: %w", err)
	}
	if err := os.Remove(file.Name()); err != nil {
		removeErr = fmt.Errorf("spill: removing file: %w", err)
	}
	return errors.Join(closeErr, removeErr)
}
