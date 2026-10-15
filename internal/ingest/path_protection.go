package ingest

import (
	"errors"
	"fmt"
	"os"
	"slices"
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
		if err != nil && !errors.Is(err, os.ErrNotExist) {
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
		return fmt.Errorf("protected roots changed during validation; retry")
	}
	for i, path := range paths {
		if path != p.paths[i] {
			return fmt.Errorf("protected roots changed during validation; retry")
		}
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		prior := p.identities[i]
		if (prior == nil) != (current == nil) || prior != nil && !os.SameFile(prior, current) {
			return fmt.Errorf("protected root %q changed during validation; retry", path)
		}
	}
	return nil
}
