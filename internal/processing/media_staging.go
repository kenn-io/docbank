package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"go.kenn.io/docbank/internal/canonical"
)

// StagedMediaContent holds one exact verified input under the service-wide
// staging reservation until Close. It is accepted only by the service that
// created it.
type StagedMediaContent struct {
	file       *os.File
	name       string
	service    *Service
	size       int64
	digest     string
	closeOnce  sync.Once
	closeError error
}

func (content *StagedMediaContent) Read(p []byte) (int, error) {
	if content == nil || content.file == nil {
		return 0, errors.New("staged media content is closed")
	}
	return content.file.Read(p)
}

func (content *StagedMediaContent) rewind() error {
	if content == nil || content.file == nil {
		return errors.New("staged media content is closed")
	}
	_, err := content.file.Seek(0, io.SeekStart)
	return err
}

func (content *StagedMediaContent) Close() error {
	if content == nil {
		return nil
	}
	content.closeOnce.Do(func() {
		content.closeError = errors.Join(content.file.Close(), os.Remove(content.name))
		content.file = nil
		content.service.releaseMediaBytes(content.size)
	})
	return content.closeError
}

// StageMediaContent verifies a complete input under the shared staging quota.
// The caller must close the returned content on every path.
func (service *Service) StageMediaContent(
	ctx context.Context, source io.Reader, expectedSize, maximum int64, expectedSHA string,
) (*StagedMediaContent, error) {
	if service == nil || expectedSize < 1 || maximum < 1 || expectedSize > maximum ||
		expectedSize > service.mediaMaxBytes {
		return nil, errors.New("byte_limit")
	}
	if !service.reserveMediaBytes(expectedSize) {
		return nil, errors.New("byte_limit")
	}
	fail := func(err error) (*StagedMediaContent, error) {
		service.releaseMediaBytes(expectedSize)
		return nil, err
	}
	if err := os.MkdirAll(service.spoolDirectory, 0o700); err != nil {
		return fail(fmt.Errorf("creating media staging directory: %w", err))
	}
	file, err := os.CreateTemp(service.spoolDirectory, ".docbank-media-*")
	if err != nil {
		return fail(fmt.Errorf("creating media staging file: %w", err))
	}
	content := &StagedMediaContent{file: file, name: file.Name(), service: service,
		size: expectedSize, digest: expectedSHA}
	if err := file.Chmod(0o600); err != nil {
		_ = content.Close()
		return nil, err
	}
	if _, err := stageMedia(ctx, source, file, expectedSize, expectedSize, expectedSHA); err != nil {
		_ = content.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = content.Close()
		return nil, err
	}
	if err := content.rewind(); err != nil {
		_ = content.Close()
		return nil, err
	}
	return content, nil
}

func (service *Service) mediaStagedContent(
	ctx context.Context, source io.Reader, expectedSize, maximum int64, expectedSHA string,
) (*StagedMediaContent, bool, error) {
	if staged, ok := source.(*StagedMediaContent); ok {
		if staged.service != service || staged.size != expectedSize || staged.digest != expectedSHA {
			return nil, false, errors.New("staged media authority changed")
		}
		if err := staged.rewind(); err != nil {
			return nil, false, err
		}
		return staged, false, nil
	}
	staged, err := service.StageMediaContent(ctx, source, expectedSize, maximum, expectedSHA)
	return staged, true, err
}

type mediaContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r mediaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// stageMedia copies one declared upload through a hard byte ceiling and
// verifies its complete immutable identity before the staged bytes may be used.
func stageMedia(
	ctx context.Context,
	r io.Reader,
	dst io.Writer,
	expectedSize, maxBytes int64,
	expectedSHA string,
) (int64, error) {
	if r == nil || dst == nil {
		return 0, errors.New("media staging requires source and destination")
	}
	if expectedSize < 0 || maxBytes < 1 || maxBytes > 2<<30 || expectedSize > maxBytes {
		return 0, errors.New("byte_limit")
	}
	if !canonical.IsSHA256Hex(expectedSHA) {
		return 0, errors.New("invalid source digest")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(mediaContextReader{ctx: ctx, r: r}, maxBytes+1))
	if n > maxBytes {
		return n, errors.New("byte_limit")
	}
	if err != nil {
		return n, err
	}
	if n != expectedSize {
		return n, errors.New("partial_download")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		return n, errors.New("digest_mismatch")
	}
	return n, nil
}
