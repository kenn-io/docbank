package formatdetect

import (
	"errors"
	"io"
)

// CountPDFPagesReader counts PDF pages from the exact bounded bytes exposed by
// reader. It does not open files or perform provider access.
func CountPDFPagesReader(reader io.ReaderAt, size int64) (int, error) {
	data, err := readBounded(reader, size)
	if err != nil {
		return 0, err
	}
	pages, err := CountPDFPages(data)
	if err != nil {
		return 0, err
	}
	if pages > int64(maxInt()) {
		return 0, errors.New("PDF page count is not representable")
	}
	return int(pages), nil
}

func readBounded(reader io.ReaderAt, size int64) ([]byte, error) {
	if reader == nil || size <= 0 {
		return nil, errors.New("document unit counting requires nonempty bytes")
	}
	if size > MaxDocumentBytes {
		return nil, errors.New("document exceeds the unit-count byte limit")
	}
	if size > int64(maxInt()) {
		return nil, errors.New("document size is not representable")
	}
	data := make([]byte, int(size))
	read, err := reader.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(read) != size {
		return nil, errors.New("document bytes changed during unit counting")
	}
	return data, nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
