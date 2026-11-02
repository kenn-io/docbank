package packstore

import (
	"context"
	"errors"
	"io"
	"os"
)

// RestoreLoose streams and verifies an object into a held restore content
// root, using the same encoding and publication rules as filesystem ingestion.
// It does not attach ownership or grant catalog authority. The caller must own
// the restore target exclusively and record the receipt in its unpublished
// catalog before publishing that catalog. Existing objects are fully verified
// and reused without replacing either encoding. Repair removes damaged
// alternates. Valid alternates remain for the old catalog; the caller must
// remove them only after durably publishing the new catalog's chosen encoding.
func RestoreLoose(ctx context.Context, root *os.Root, hash Hash, src io.Reader, opts WriteOptions) (WriteResult, error) {
	if root == nil || src == nil {
		return WriteResult{}, errors.New("packstore: restore requires a root and content reader")
	}
	if err := hash.Validate(); err != nil {
		return WriteResult{}, err
	}
	if !opts.SizeKnown || opts.ExpectedHash != hash {
		return WriteResult{}, ErrInvalidPolicy
	}
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, err
	}
	layout, err := NewLayout(root.Name(), LayoutOptions{Staging: StagingSameDirectory})
	if err != nil {
		return WriteResult{}, err
	}
	backend := FilesystemBackend{layout: layout}
	return backend.publishLooseRoot(ctx, root, hash, src, opts, looseRestore)
}
