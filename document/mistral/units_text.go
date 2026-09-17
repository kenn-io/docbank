package mistral

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/mail"

	"gopkg.in/yaml.v3"
)

const counterLineBufferSize = 32 << 10

func counterSection(reader io.ReaderAt, size int64) (*io.SectionReader, error) {
	if reader == nil || size <= 0 {
		return nil, errors.New("mistral text unit counter requires nonempty input")
	}
	return io.NewSectionReader(reader, 0, size), nil
}

func countTextLines(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	count := 0
	err = forEachTextLine(counting, func(line []byte) error {
		if len(line) > 0 && count < MaxUnits+1 {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("read Mistral text line: %w", err)
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral text input: %w", err)
	}
	if count == 0 {
		return 0, errors.New("mistral text input has no lines")
	}
	return count, nil
}

func countCSVRecords(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	csvReader := csv.NewReader(bufio.NewReaderSize(counting, counterLineBufferSize))
	csvReader.FieldsPerRecord = -1
	csvReader.ReuseRecord = true
	count := 0
	for {
		_, readErr := csvReader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read Mistral CSV record: %w", readErr)
		}
		if count < MaxUnits+1 {
			count++
		}
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral CSV input: %w", err)
	}
	if count == 0 {
		return 0, errors.New("mistral CSV input has no records")
	}
	return count, nil
}

func countJSONValues(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	decoder := jsontext.NewDecoder(counting, jsontext.AllowDuplicateNames(true))
	if err := decoder.SkipValue(); err != nil {
		return 0, fmt.Errorf("read Mistral JSON value: %w", err)
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, errors.New("mistral JSON input has multiple top-level values")
		}
		return 0, fmt.Errorf("read Mistral JSON trailing value: %w", err)
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral JSON input: %w", err)
	}
	return 1, nil
}

func countJSONLines(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	count := 0
	err = forEachTextLine(counting, func(line []byte) error {
		value := bytes.TrimSpace(line)
		if len(value) == 0 {
			return nil
		}
		if err := validateOneJSONValue(value); err != nil {
			return err
		}
		if count < MaxUnits+1 {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("read Mistral JSONL record: %w", err)
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral JSONL input: %w", err)
	}
	if count == 0 {
		return 0, errors.New("mistral JSONL input has no records")
	}
	return count, nil
}

func validateOneJSONValue(value []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(value), jsontext.AllowDuplicateNames(true))
	if err := decoder.SkipValue(); err != nil {
		return fmt.Errorf("read JSONL value: %w", err)
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSONL record has multiple top-level values")
		}
		return fmt.Errorf("read JSONL trailing value: %w", err)
	}
	return nil
}

func countYAMLDocuments(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	decoder := yaml.NewDecoder(bufio.NewReaderSize(counting, counterLineBufferSize))
	count := 0
	for {
		var node yaml.Node
		decodeErr := decoder.Decode(&node)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			return 0, fmt.Errorf("read Mistral YAML document: %w", decodeErr)
		}
		if count < MaxUnits+1 {
			count++
		}
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral YAML input: %w", err)
	}
	if count == 0 {
		return 0, errors.New("mistral YAML input has no documents")
	}
	return count, nil
}

func countXMLDocument(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	buffered := bufio.NewReaderSize(counting, counterLineBufferSize)
	if prefix, peekErr := buffered.Peek(3); peekErr == nil && bytes.Equal(prefix, []byte{0xef, 0xbb, 0xbf}) {
		_, _ = buffered.Discard(3)
	}
	decoder := xml.NewDecoder(buffered)
	depth, roots := 0, 0
	for {
		token, tokenErr := decoder.Token()
		if errors.Is(tokenErr, io.EOF) {
			break
		}
		if tokenErr != nil {
			return 0, fmt.Errorf("read Mistral XML document: %w", tokenErr)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return 0, errors.New("mistral XML input has multiple roots")
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return 0, errors.New("mistral XML input has an unexpected closing element")
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return 0, errors.New("mistral XML input has text outside its root")
			}
		}
	}
	if depth != 0 || roots != 1 {
		return 0, errors.New("mistral XML input must contain one root")
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral XML input: %w", err)
	}
	return 1, nil
}

func countMailMessages(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	message, err := mail.ReadMessage(counting)
	if err != nil {
		return 0, fmt.Errorf("read Mistral mail message: %w", err)
	}
	if _, err := io.Copy(io.Discard, message.Body); err != nil {
		return 0, fmt.Errorf("read Mistral mail body: %w", err)
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral mail input: %w", err)
	}
	return 1, nil
}

func countMSGMessages(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	if _, err := io.Copy(io.Discard, counting); err != nil {
		return 0, fmt.Errorf("read Mistral MSG input: %w", err)
	}
	if err := counting.verify(size); err != nil {
		return 0, fmt.Errorf("verify Mistral MSG input: %w", err)
	}
	return 1, nil
}

func forEachTextLine(reader io.Reader, visit func([]byte) error) error {
	buffered := bufio.NewReaderSize(reader, counterLineBufferSize)
	for {
		line, err := buffered.ReadString('\n')
		if len(line) > 0 {
			if visitErr := visit([]byte(line)); visitErr != nil {
				return visitErr
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("read buffered line: %w", err)
	}
}

type countingReader struct {
	reader io.Reader
	read   int64
	err    error
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.read += int64(read)
	if err != nil && !errors.Is(err, io.EOF) {
		reader.err = err
	}
	return read, err
}

func (reader *countingReader) verify(size int64) error {
	if reader.err != nil {
		return reader.err
	}
	if reader.read != size {
		return fmt.Errorf("read %d bytes, declared %d", reader.read, size)
	}
	return nil
}
