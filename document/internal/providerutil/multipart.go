package providerutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// ValidateMultipartFilename rejects line breaks before a filename is placed in
// a multipart Content-Disposition header.
func ValidateMultipartFilename(filename string) error {
	if strings.ContainsAny(filename, "\r\n") {
		return errors.New("multipart filename contains a line break")
	}
	return nil
}

// MultipartUpload streams one file and its form fields as multipart/form-data
// without buffering the body: the request reads from a pipe while a goroutine
// writes, so a provider that answers before consuming the upload is handled
// and a stalled upload can be interrupted.
type MultipartUpload struct {
	// Prologue writes parts that precede the file part.
	Prologue  func(*multipart.Writer) error
	FieldName string
	Filename  string
	MediaType string
	Source    io.Reader
	Length    int64
	// Fields are written after the file part, in order.
	Fields [][2]string
	// Interrupt unblocks a stalled Source read once the upload is abandoned.
	Interrupt func() error
}

// EncodedLength returns the exact byte length of the encoded body.
func (upload MultipartUpload) EncodedLength() (int64, error) {
	counter := &countingWriter{}
	writer := multipart.NewWriter(counter)
	framing := upload
	framing.Source, framing.Length = nil, 0
	if err := framing.write(writer); err != nil {
		return 0, err
	}
	if err := writer.Close(); err != nil {
		return 0, fmt.Errorf("close multipart body: %w", err)
	}
	return counter.count + upload.Length, nil
}

type countingWriter struct{ count int64 }

func (writer *countingWriter) Write(value []byte) (int, error) {
	writer.count += int64(len(value))
	return len(value), nil
}

func (upload MultipartUpload) write(writer *multipart.Writer) error {
	if err := ValidateMultipartFilename(upload.Filename); err != nil {
		return err
	}
	if upload.Prologue != nil {
		if err := upload.Prologue(writer); err != nil {
			return err
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition(upload.FieldName, upload.Filename))
	header.Set("Content-Type", upload.MediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create multipart file part: %w", err)
	}
	if upload.Source != nil {
		written, err := io.Copy(part, io.LimitReader(upload.Source, upload.Length+1))
		if err != nil {
			return err
		}
		if written != upload.Length {
			return errors.New("upload length changed during submission")
		}
	}
	for _, field := range upload.Fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return fmt.Errorf("write multipart field %s: %w", field[0], err)
		}
	}
	return nil
}

// start begins streaming the body and returns the reader the request consumes.
func (upload MultipartUpload) start() (*multipartCompletion, string) {
	bodyReader, bodyWriter := io.Pipe()
	writer := multipart.NewWriter(bodyWriter)
	done := make(chan error, 1)
	go func() {
		err := upload.write(writer)
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		done <- err
		_ = bodyWriter.CloseWithError(err)
	}()
	return &multipartCompletion{reader: bodyReader, interrupt: upload.Interrupt, done: done},
		writer.FormDataContentType()
}

type multipartCompletion struct {
	reader    *io.PipeReader
	interrupt func() error
	done      <-chan error
}

func (completion *multipartCompletion) close(cause error) {
	_ = completion.reader.CloseWithError(cause)
}

// abort stops the request body, releases a stalled source read, and waits
// for the writer goroutine to exit.
func (completion *multipartCompletion) abort(cause error) error {
	completion.close(cause)
	var interruptErr error
	if completion.interrupt != nil {
		interruptErr = completion.interrupt()
	}
	return errors.Join(interruptErr, <-completion.done)
}

// wait blocks until the whole body was written or ctx ends.
func (completion *multipartCompletion) wait(ctx context.Context) error {
	select {
	case err := <-completion.done:
		if err != nil {
			return fmt.Errorf("upload did not complete: %w", err)
		}
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), completion.abort(ctx.Err()))
	}
}
