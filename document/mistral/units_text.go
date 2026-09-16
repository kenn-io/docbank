package mistral

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/mail"
)

func counterSection(reader io.ReaderAt, size int64) (*io.SectionReader, error) {
	if reader == nil || size <= 0 {
		return nil, errors.New("mistral text unit counter requires nonempty input")
	}
	return io.NewSectionReader(reader, 0, size), nil
}

func countJSONValues(reader io.ReaderAt, size int64) (int, error) {
	section, err := counterSection(reader, size)
	if err != nil {
		return 0, err
	}
	counting := &countingReader{reader: section}
	decoder := jsontext.NewDecoder(counting, jsontext.AllowDuplicateNames(true))
	if err := consumeJSONValue(decoder); err != nil {
		return 0, fmt.Errorf("read Mistral JSON value: %w", err)
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, errors.New("mistral JSON input has multiple top-level values")
		}
		return 0, fmt.Errorf("read Mistral JSON trailing value: %w", err)
	}
	if counting.read != size {
		return 0, errors.New("mistral JSON input ended before its declared size")
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
	if counting.read != size {
		return 0, errors.New("mistral mail input ended before its declared size")
	}
	return 1, nil
}

type countingReader struct {
	reader io.Reader
	read   int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.read += int64(read)
	return read, err
}

func consumeJSONValue(decoder *jsontext.Decoder) error {
	token, err := decoder.ReadToken()
	if err != nil {
		return err
	}
	depth := 0
	switch token.Kind() {
	case jsontext.KindBeginObject, jsontext.KindBeginArray:
		depth = 1
	case jsontext.KindEndObject, jsontext.KindEndArray:
		return errors.New("mistral JSON value starts with a closing delimiter")
	case jsontext.KindInvalid, jsontext.KindNull, jsontext.KindFalse, jsontext.KindTrue,
		jsontext.KindString, jsontext.KindNumber:
	}
	for depth > 0 {
		token, err = decoder.ReadToken()
		if err != nil {
			return err
		}
		switch token.Kind() {
		case jsontext.KindBeginObject, jsontext.KindBeginArray:
			depth++
		case jsontext.KindEndObject, jsontext.KindEndArray:
			depth--
		case jsontext.KindInvalid, jsontext.KindNull, jsontext.KindFalse, jsontext.KindTrue,
			jsontext.KindString, jsontext.KindNumber:
		}
	}
	return nil
}
