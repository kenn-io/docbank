package ingest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"

	"go.kenn.io/docbank/internal/store"
)

// PathProtection fences a checked set of roots against replacement or new roots.
type PathProtection struct {
	paths      []string
	identities []os.FileInfo
}

func CapturePathProtection(paths []string) (*PathProtection, error) {
	paths = append([]string(nil), paths...)
	slices.Sort(paths)
	p := &PathProtection{paths: paths}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return nil, err
		}
		p.identities = append(p.identities, info)
	}
	return p, nil
}

func (p *PathProtection) Check(paths []string) error {
	paths = append([]string(nil), paths...)
	slices.Sort(paths)
	if len(paths) != len(p.paths) {
		return errors.New("protected roots changed during validation; retry")
	}
	for i, path := range paths {
		if path != p.paths[i] {
			return errors.New("protected roots changed during validation; retry")
		}
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
		prior := p.identities[i]
		if (prior == nil) != (current == nil) || prior != nil && !os.SameFile(prior, current) {
			return fmt.Errorf("protected root %q changed during validation; retry", path)
		}
	}
	return nil
}

// PhotoCheckoutProtectsDestination retains admission protection until copies are cleared.
func PhotoCheckoutProtectsDestination(c store.PhotoCheckout) (bool, error) {
	if c.Destination == "" {
		return false, nil
	}
	if !c.RetentionReleased {
		return true, nil
	}
	// A replacement file at the destination or an ancestor holds no copies.
	info, err := os.Stat(c.Destination)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	folder, err := os.Open(c.Destination)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, readErr := folder.Readdirnames(1)
	if err := folder.Close(); err != nil {
		return false, err
	}
	if errors.Is(readErr, io.EOF) {
		return false, nil
	}
	return readErr == nil, readErr
}
