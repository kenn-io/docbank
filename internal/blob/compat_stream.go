package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// verifiedLooseCompatibilityStream preserves reads of oversized loose objects
// accepted before docbank enforced MaxBlobBytes at write time. It is created
// only after catalog resolution confirms there is no packed authority.
type verifiedLooseCompatibilityStream struct {
	ctx          context.Context
	reader       io.ReadSeekCloser
	expectedHash packstore.Hash
	expectedSize int64
	digest       hash.Hash
	read         int64
	terminal     error
	closeErr     error
	closed       bool
	verified     bool
}

func newVerifiedLooseCompatibilityStream(
	ctx context.Context, reader io.ReadSeekCloser, expectedHash packstore.Hash, expectedSize int64,
) packstore.VerifiedReadCloser {
	return &verifiedLooseCompatibilityStream{
		ctx: ctx, reader: reader, expectedHash: expectedHash, expectedSize: expectedSize, digest: sha256.New(),
	}
}

func (s *verifiedLooseCompatibilityStream) Read(p []byte) (int, error) {
	if s.closed {
		if s.terminal != nil {
			return 0, s.terminal
		}
		if s.verified {
			return 0, io.EOF
		}
		return 0, os.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}
	n, err := s.reader.Read(p)
	if n > 0 {
		_, _ = s.digest.Write(p[:n])
		s.read += int64(n)
		if s.read > s.expectedSize {
			return n, s.fail(packstore.ErrContentMismatch)
		}
	}
	if errors.Is(err, io.EOF) {
		return n, s.finish()
	}
	if err != nil {
		return n, s.fail(err)
	}
	return n, nil
}

func (s *verifiedLooseCompatibilityStream) finish() error {
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	if s.read != s.expectedSize || hex.EncodeToString(s.digest.Sum(nil)) != s.expectedHash.String() {
		return s.fail(fmt.Errorf("%w: oversized loose blob identity changed", packstore.ErrContentMismatch))
	}
	s.closeErr = s.reader.Close()
	s.closed = true
	if s.closeErr != nil {
		s.terminal = s.closeErr
		return s.terminal
	}
	s.verified = true
	return io.EOF
}

func (s *verifiedLooseCompatibilityStream) fail(err error) error {
	if s.terminal == nil {
		s.closeErr = s.reader.Close()
		s.closed = true
		s.terminal = errors.Join(err, s.closeErr)
	}
	return s.terminal
}

func (s *verifiedLooseCompatibilityStream) Verify() error {
	if s.verified {
		return nil
	}
	if s.terminal != nil {
		return s.terminal
	}
	buf := make([]byte, 64<<10)
	for {
		_, err := s.Read(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *verifiedLooseCompatibilityStream) Verified() bool { return s.verified }

func (s *verifiedLooseCompatibilityStream) Close() error {
	if !s.closed {
		s.closeErr = s.reader.Close()
		s.closed = true
	}
	if s.verified {
		return s.closeErr
	}
	return errors.Join(s.terminal, s.closeErr, pack.ErrVerificationIncomplete)
}
