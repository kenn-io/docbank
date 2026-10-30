// Package docx identifies equal DOCX package contents without rewriting originals.
package docx

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/internal/formatdetect"
)

// Fingerprint returns the v1 SHA-256 of sorted entry names and their complete
// uncompressed contents. ZIP timestamps, compression, comments, and entry order
// do not contribute. All XML, properties, signatures, and embedded bytes do.
// This fingerprint is not the original file's integrity hash.
// Sources are limited to 64 MiB; the shared format detector enforces entry,
// expansion, encryption, name, and checksum limits before hashing.
func Fingerprint(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) > 64<<20 {
		return "", errors.New("DOCX source exceeds 64 MiB")
	}
	format, err := formatdetect.DetectFormatContext(ctx, bytes.NewReader(data), int64(len(data)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	if err != nil {
		return "", fmt.Errorf("validate DOCX package: %w", err)
	}
	if format.ID != "docx" {
		return "", errors.New("package is not DOCX")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open DOCX package: %w", err)
	}
	slices.SortFunc(archive.File, func(a, b *zip.File) int { return strings.Compare(a.Name, b.Name) })
	hash := sha256.New()
	_, _ = io.WriteString(hash, "docbank-docx-package-v1\x00")
	var frame [8]byte
	for _, part := range archive.File {
		binary.BigEndian.PutUint64(frame[:], uint64(len(part.Name)))
		_, _ = hash.Write(frame[:])
		_, _ = io.WriteString(hash, part.Name)
		binary.BigEndian.PutUint64(frame[:], part.UncompressedSize64)
		_, _ = hash.Write(frame[:])
		reader, err := part.Open()
		if err != nil {
			return "", fmt.Errorf("open DOCX entry: %w", err)
		}
		_, readErr := io.Copy(hash, contextReader{ctx: ctx, reader: reader})
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", fmt.Errorf("hash DOCX entry: %w", err)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
