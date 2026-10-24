package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

type redactionArchiveSnapshot struct {
	file   *os.File
	dir    string
	size   int64
	sha256 string
}

func snapshotRedactionArchive(ctx context.Context, input io.Reader, size int64,
) (snapshot *redactionArchiveSnapshot, resultErr error) {
	if size < 1 || size > processing.MaxRedactionArchiveBytes {
		return nil, fmt.Errorf("%w: redaction archive size exceeds bounds", daemonconn.ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "docbank-redaction-verify-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(dir))
		}
	}()
	path := filepath.Join(dir, "archive.zip")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	err = copyRedactionArchive(ctx, io.MultiWriter(file, hash), input, size)
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	file, err = os.Open(path)
	if err != nil {
		return nil, err
	}
	return &redactionArchiveSnapshot{file: file, dir: dir, size: size, sha256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func copyRedactionArchive(ctx context.Context, output io.Writer, input io.Reader, size int64) error {
	input = io.LimitReader(input, size+1)
	var buffer [64 << 10]byte
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := input.Read(buffer[:])
		copied += int64(n)
		if copied > size {
			return fmt.Errorf("%w: archive grew during capture", daemonconn.ErrIntegrity)
		}
		if n > 0 {
			if _, writeErr := output.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if copied != size {
		return fmt.Errorf("%w: archive ended before its stated size", daemonconn.ErrIntegrity)
	}
	return nil
}

func (s *redactionArchiveSnapshot) Close() error {
	return errors.Join(s.file.Close(), os.RemoveAll(s.dir))
}
