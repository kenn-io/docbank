package packstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"unicode/utf8"

	"go.kenn.io/kit/safefileio"
)

const (
	ownershipMarkerName     = ".packstore-owner.json"
	maxOwnershipMarkerBytes = 4096
)

// Ownership is the fencing term for one application-owned store namespace.
type Ownership struct {
	Format uint32  `json:"format"`
	Vault  string  `json:"vault"`
	Store  StoreID `json:"store"`
	Epoch  string  `json:"epoch"`
}

// Validate checks marker identity independently of any backend binding.
func (o Ownership) Validate() error {
	if o.Format != OwnershipFormatV1 {
		return fmt.Errorf("packstore: unsupported ownership format %d", o.Format)
	}
	if o.Vault == "" {
		return errors.New("packstore: empty ownership vault")
	}
	if o.Store == "" {
		return errors.New("packstore: empty ownership store")
	}
	if o.Epoch == "" {
		return errors.New("packstore: empty ownership epoch")
	}
	if !utf8.ValidString(o.Vault) ||
		!utf8.ValidString(string(o.Store)) ||
		!utf8.ValidString(o.Epoch) {
		return errors.New("packstore: ownership identity is not valid UTF-8")
	}
	return nil
}

// OwnershipMismatchError reports that the live namespace fencing term differs
// from the binding expected by the caller.
type OwnershipMismatchError struct {
	Expected Ownership
	Actual   Ownership
	Err      error
}

func (e *OwnershipMismatchError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf(
			"%v: expected store %s epoch %q: %v",
			ErrStoreFenced,
			e.Expected.Store,
			e.Expected.Epoch,
			e.Err,
		)
	}
	return fmt.Sprintf(
		"%v: expected store %s epoch %q, found store %s epoch %q",
		ErrStoreFenced,
		e.Expected.Store,
		e.Expected.Epoch,
		e.Actual.Store,
		e.Actual.Epoch,
	)
}

func (e *OwnershipMismatchError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrStoreFenced}
	}
	return []error{ErrStoreFenced, e.Err}
}

type ownershipState struct {
	mu       sync.RWMutex
	expected *Ownership
}

func (s *ownershipState) set(expected Ownership) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := expected
	s.expected = &cloned
}

func (s *ownershipState) get() *Ownership {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.expected == nil {
		return nil
	}
	cloned := *s.expected
	return &cloned
}

// MarshalOwnership returns the canonical ownership-marker representation.
func MarshalOwnership(value Ownership) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("packstore: encode ownership marker: %w", err)
	}
	if len(encoded)+1 > maxOwnershipMarkerBytes {
		return nil, fmt.Errorf(
			"packstore: ownership marker size %d is invalid",
			len(encoded)+1,
		)
	}
	return append(encoded, '\n'), nil
}

// ParseOwnership strictly validates a canonical ownership marker.
func ParseOwnership(data []byte) (Ownership, error) {
	if len(data) > maxOwnershipMarkerBytes {
		return Ownership{}, fmt.Errorf("packstore: ownership marker size %d is invalid", len(data))
	}
	var value Ownership
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Ownership{}, fmt.Errorf("packstore: decode ownership marker: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Ownership{}, errors.New("packstore: ownership marker contains trailing JSON")
	}
	canonical, err := MarshalOwnership(value)
	if err != nil {
		return Ownership{}, err
	}
	if !bytes.Equal(data, canonical) {
		return Ownership{}, errors.New("packstore: ownership marker is not canonical")
	}
	return value, nil
}

func readOwnershipRoot(root *os.Root) (Ownership, error) {
	file, err := openRootRegularFile(root, ownershipMarkerName, ownershipMarkerName)
	if err != nil {
		return Ownership{}, err
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidateCurrentUserFile(file); err != nil {
		return Ownership{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return Ownership{}, fmt.Errorf("packstore: stat ownership marker: %w", err)
	}
	if info.Size() < 0 || info.Size() > maxOwnershipMarkerBytes {
		return Ownership{}, fmt.Errorf("packstore: ownership marker size %d is invalid", info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxOwnershipMarkerBytes+1))
	if err != nil {
		return Ownership{}, fmt.Errorf("packstore: read ownership marker: %w", err)
	}
	return ParseOwnership(data)
}

func replaceOwnershipRoot(
	ctx context.Context,
	root *os.Root,
	next Ownership,
	expected *Ownership,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := MarshalOwnership(next)
	if err != nil {
		return err
	}
	current, readErr := readOwnershipRoot(root)
	switch {
	case expected == nil && readErr == nil:
		return &OwnershipMismatchError{Actual: current}
	case expected == nil && !errors.Is(readErr, fs.ErrNotExist):
		return readErr
	case expected != nil && readErr != nil:
		return &OwnershipMismatchError{Expected: *expected, Err: readErr}
	case expected != nil && current != *expected:
		return &OwnershipMismatchError{Expected: *expected, Actual: current}
	}
	staged, stagedName, err := createRootTemp(root, ".ownership-")
	if err != nil {
		return fmt.Errorf("packstore: create ownership staging: %w", err)
	}
	stagedOpen := true
	defer func() {
		if stagedOpen {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := root.Remove(stagedName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if _, err := staged.Write(encoded); err != nil {
		return fmt.Errorf("packstore: write ownership staging: %w", err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("packstore: sync ownership staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("packstore: close ownership staging: %w", err)
	}
	stagedOpen = false
	if expected == nil {
		if err := root.Link(stagedName, ownershipMarkerName); err != nil {
			if errors.Is(err, fs.ErrExist) {
				actual, readErr := readOwnershipRoot(root)
				if readErr == nil {
					return &OwnershipMismatchError{Actual: actual}
				}
			}
			return fmt.Errorf("packstore: publish ownership marker: %w", err)
		}
	} else if err := root.Rename(stagedName, ownershipMarkerName); err != nil {
		return fmt.Errorf("packstore: publish ownership marker: %w", err)
	}
	if err := syncFilesystemRootDir(root); err != nil {
		return fmt.Errorf("packstore: sync ownership marker: %w", err)
	}
	return nil
}
