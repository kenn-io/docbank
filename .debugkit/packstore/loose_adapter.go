package packstore

import (
	"context"
	"io"
)

// LooseStore is the released single-filesystem loose API. It is a thin
// compatibility adapter over the same filesystem mechanics used by Backend.
type LooseStore struct {
	layout  Layout
	backend *FilesystemBackend
}

// NewLooseStore prepares layout for loose operations.
func NewLooseStore(layout Layout) (*LooseStore, error) {
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	if err != nil {
		return nil, err
	}
	return &LooseStore{layout: layout, backend: backend}, nil
}

// Write streams src into its canonical content-addressed path.
func (s *LooseStore) Write(
	ctx context.Context,
	src io.Reader,
	opts WriteOptions,
) (WriteResult, error) {
	return s.backend.loose.Write(ctx, src, opts)
}

// WriteBytes publishes in-memory content without redundantly hashing it while
// copying.
func (s *LooseStore) WriteBytes(
	ctx context.Context,
	content []byte,
	opts WriteOptions,
) (WriteResult, error) {
	return s.backend.loose.WriteBytes(ctx, content, opts)
}

// Repair completely verifies and atomically replaces one loose object.
func (s *LooseStore) Repair(
	ctx context.Context,
	src io.Reader,
	expected LooseIdentity,
	opts RepairOptions,
) (WriteResult, error) {
	return s.backend.loose.Repair(ctx, src, expected, opts)
}

// Verify checks one canonical loose object.
func (s *LooseStore) Verify(
	hash Hash,
	size int64,
	verification DedupVerification,
	durability Durability,
) (WriteResult, bool, error) {
	return s.backend.loose.Verify(hash, size, verification, durability)
}

// Remove deletes both canonical loose representations.
func (s *LooseStore) Remove(hash Hash, durability RemovalDurability) error {
	return s.backend.loose.Remove(hash, durability)
}
