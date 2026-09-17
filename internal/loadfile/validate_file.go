package loadfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// validatePackageFile reads only the inventoried extent. A PDF check needs random
// access, so it receives a private snapshot of the bytes fed to the digest.
func validatePackageFile(ctx context.Context, resolver *Resolver, volume Volume, ref FileRef, checkPDF func(io.ReadSeeker) error) (_ FileRef, err error) {
	if err := ctx.Err(); err != nil {
		return ref, err
	}
	source, err := resolver.Open(volume, ref.RelPath)
	if err != nil {
		return ref, err
	}
	defer func() {
		if source != nil {
			err = errors.Join(err, source.Close())
		}
	}()

	digest := sha256.New()
	var destination io.Writer = digest
	var snapshot *os.File
	if checkPDF != nil {
		snapshot, err = os.CreateTemp("", "docbank-package-pdf-*")
		if err != nil {
			return ref, err
		}
		defer func() {
			err = errors.Join(err, snapshot.Close(), os.Remove(snapshot.Name()))
		}()
		destination = io.MultiWriter(snapshot, digest)
	}
	written, copyErr := io.Copy(destination, packageReadSeeker{ReadSeeker: source, ctx: ctx})
	size := source.Size()
	closeErr := source.Close()
	source = nil
	if err := errors.Join(copyErr, closeErr, ctx.Err()); err != nil {
		return ref, err
	}
	if written != size {
		return ref, fmt.Errorf("%w: package file size changed during preflight", ErrMalformedInput)
	}
	if snapshot != nil {
		if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
			return ref, err
		}
		if err := errors.Join(checkPDF(packageReadSeeker{ReadSeeker: snapshot, ctx: ctx}), ctx.Err()); err != nil {
			return ref, err
		}
	}
	ref.SHA256, ref.Size, ref.Status = hex.EncodeToString(digest.Sum(nil)), written, "available"
	return ref, nil
}

type packageReadSeeker struct {
	io.ReadSeeker

	ctx context.Context
}

func (r packageReadSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(p)
}

func (r packageReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Seek(offset, whence)
}
