package providerutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PinnedExecutable retains verified executable bytes for isolated local-process launches.
type PinnedExecutable struct {
	sourcePath string
	sha256     string
	content    []byte
}

// LoadPinnedExecutable verifies and retains the exact regular, non-symlink executable.
func LoadPinnedExecutable(path, expectedSHA256 string, maxBytes int64) (*PinnedExecutable, error) {
	if maxBytes <= 0 {
		return nil, errors.New("pinned executable byte limit must be positive")
	}
	decoded, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != expectedSHA256 {
		return nil, errors.New("pinned executable SHA-256 must be lowercase hexadecimal")
	}
	content, err := readPinnedExecutable(path, expectedSHA256, maxBytes)
	if err != nil {
		return nil, err
	}
	return &PinnedExecutable{sourcePath: path, sha256: expectedSHA256, content: content}, nil
}

// Materialize verifies the configured source and writes the pinned bytes to an
// owner-private directory. The caller must remove the returned directory after
// the child exits.
func (executable *PinnedExecutable) Materialize() (string, func() error, error) {
	if executable == nil {
		return "", nil, errors.New("pinned executable is required")
	}
	if _, err := readPinnedExecutable(executable.sourcePath, executable.sha256, int64(len(executable.content))); err != nil {
		return "", nil, fmt.Errorf("verify pinned executable source: %w", err)
	}
	directory, err := os.MkdirTemp("", "docbank-provider-executable-")
	if err != nil {
		return "", nil, fmt.Errorf("create private executable directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	target := filepath.Join(directory, filepath.Base(executable.sourcePath))
	if err := os.WriteFile(target, executable.content, 0o400); err != nil {
		return "", nil, errors.Join(fmt.Errorf("write pinned executable: %w", err), cleanup())
	}
	if err := os.Chmod(target, 0o500); err != nil { //nolint:gosec // the private pinned copy must be owner-executable
		return "", nil, errors.Join(fmt.Errorf("make pinned executable runnable: %w", err), cleanup())
	}
	return target, cleanup, nil
}

func readPinnedExecutable(path, expectedSHA256 string, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("configured executable is unavailable")
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("configured executable must be a regular non-symlink file")
	}
	if before.Size() <= 0 || before.Size() > maxBytes {
		return nil, errors.New("configured executable exceeds its byte limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("configured executable is unavailable")
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("configured executable changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("read configured executable: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close configured executable: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() {
		return nil, errors.New("configured executable changed while reading")
	}
	if int64(len(content)) != opened.Size() || int64(len(content)) > maxBytes {
		return nil, errors.New("configured executable exceeds its byte limit")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return nil, errors.New("configured executable SHA-256 changed")
	}
	return content, nil
}
