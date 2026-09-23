package loadfile

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxPackageZIPBytes = int64(256 << 30)

// ExtractZIP materializes a sealed, verified container in a private temporary
// root. The caller owns and must remove the returned root after use.
func ExtractZIP(ctx context.Context, source io.ReaderAt, size int64) (root string, err error) {
	if source == nil || size <= 0 || size > maxPackageZIPBytes {
		return "", ErrLoadfileLimit
	}
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return "", fmt.Errorf("%w: invalid ZIP container: %w", ErrMalformedInput, err)
	}
	if len(archive.File) == 0 || len(archive.File) > defaultMaxPackageFiles {
		return "", ErrLoadfileLimit
	}
	var total int64
	seen := make(map[string]bool, len(archive.File))
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if len(name) > 4096 || strings.Contains(name, `\`) {
			return "", ErrUnsafeReference
		}
		clean, err := portablePackagePath(name)
		if err != nil || clean != name {
			return "", ErrUnsafeReference
		}
		folded := strings.ToLower(clean)
		if seen[folded] {
			return "", ErrUnsafeReference
		}
		seen[folded] = true
		mode := entry.Mode()
		if !mode.IsRegular() && !mode.IsDir() || mode.IsDir() != strings.HasSuffix(entry.Name, "/") {
			return "", ErrUnsafeReference
		}
		if mode.IsRegular() {
			if entry.UncompressedSize64 > uint64(defaultMaxPackageBytes-total) {
				return "", ErrLoadfileLimit
			}
			size := int64(entry.UncompressedSize64) // #nosec G115 -- bounded by defaultMaxPackageBytes above
			total += size
		}
	}
	root, err = os.MkdirTemp("", "docbank-package-zip-")
	if err != nil {
		return "", err
	}
	tempRoot := root
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempRoot)
			root = ""
		}
	}()
	for _, entry := range archive.File {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		destination := filepath.Join(root, filepath.FromSlash(name))
		if entry.Mode().IsDir() {
			err = os.MkdirAll(destination, 0o700)
			if err != nil {
				return "", err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", err
		}
		var input io.ReadCloser
		input, err = entry.Open()
		if err != nil {
			return "", fmt.Errorf("open ZIP member: %w", err)
		}
		var output *os.File
		output, err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			return "", err
		}
		memberSize := int64(entry.UncompressedSize64) // #nosec G115 -- every member was bounded before extraction
		var copied int64
		copied, err = copyZIPMember(ctx, output, input, memberSize)
		err = joinCloseErrors(err, output.Close(), input.Close())
		if err != nil || copied != memberSize {
			if err == nil {
				err = ErrMalformedInput
			}
			return "", err
		}
	}
	return root, nil
}

// RemoveExtractedZIP removes only a root created by ExtractZIP, confined to
// the process temporary directory even if a caller supplies a forged path.
func RemoveExtractedZIP(root string) error {
	if filepath.Dir(root) != filepath.Clean(os.TempDir()) ||
		!strings.HasPrefix(filepath.Base(root), "docbank-package-zip-") {
		return ErrUnsafeReference
	}
	temporary, err := os.OpenRoot(os.TempDir())
	if err != nil {
		return err
	}
	defer func() { _ = temporary.Close() }()
	return temporary.RemoveAll(filepath.Base(root))
}

func copyZIPMember(ctx context.Context, output io.Writer, input io.Reader, expected int64) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			if int64(n) > expected-total {
				return total, ErrLoadfileLimit
			}
			written, err := output.Write(buffer[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func joinCloseErrors(first, second, third error) error {
	if first != nil {
		return first
	}
	if second != nil {
		return second
	}
	return third
}
