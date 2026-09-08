package qmdexport

import (
	"context"
	"errors"
	"path/filepath"
)

// LoadCurrent reads the selected owned manifest without acquiring a publication
// lock, changing stale state, or asserting the contents of an external index.
func LoadCurrent(root string) (receipt Receipt, err error) {
	ctx := context.Background()
	owned, err := openOwnedRoot(ctx, root)
	if err != nil {
		return Receipt{}, nativeError(err)
	}
	defer func() {
		err = errors.Join(err, nativeError(owned.Close()))
		if err != nil {
			receipt = Receipt{}
		}
	}()
	pointer, _, err := readGenerationFile(ctx, owned.root, "CURRENT", 65, nil)
	if err != nil {
		return Receipt{}, err
	}
	if len(pointer) != 65 || pointer[64] != '\n' || !validChecksum(string(pointer[:64])) {
		return Receipt{}, errGenerationInvalid
	}
	id := string(pointer[:64])
	dir, dirID, err := owned.generations.openDir(id)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { err = errors.Join(err, nativeError(dir.Close())) }()
	bounds, err := normalizeOptions(Options{})
	if err != nil {
		return Receipt{}, err
	}
	metadata, err := readGenerationMetadata(ctx, owned, dir, id, bounds, nil)
	if err != nil {
		return Receipt{}, err
	}
	collection, _, err := dir.openDir("collection")
	if err != nil {
		return Receipt{}, err
	}
	defer func() { err = errors.Join(err, nativeError(collection.Close())) }()
	documents, _, err := collection.openDir("documents")
	if err != nil {
		return Receipt{}, err
	}
	if err := documents.Close(); err != nil {
		return Receipt{}, nativeError(err)
	}
	if err := owned.generations.sameEntry(id, dirID); err != nil {
		return Receipt{}, err
	}
	return Receipt{GenerationID: id, CollectionPath: filepath.Join(root, "generations", id, "collection"), Manifest: metadata.manifest}, nil
}
