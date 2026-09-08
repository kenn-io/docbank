package qmdexport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type publishHooks struct {
	ownership                   ownershipHooks
	afterCurrent, beforeCurrent func()
	beforeCleanupRemove         func()
	beforeConfirmation          func() error
	beforeFileSync              func(*os.File) error
	afterRelease                func() error
}

// Publish verifies a source snapshot and atomically selects a complete owned
// generation. A nonempty receipt remains authoritative when err is nonnil.
func Publish(ctx context.Context, root, collection string, sources []Source, reader BlobReader, options Options) (Receipt, error) {
	return publishWithHooks(ctx, root, collection, sources, reader, options, publishHooks{})
}

func publicationPreflight(ctx context.Context, root, collection string, reader BlobReader, options Options) (Options, error) {
	if err := checkRootRequest(ctx, root); err != nil {
		return Options{}, err
	}
	if reader == nil {
		return Options{}, errors.New("qmd export requires a blob reader")
	}
	if !validCollection(collection) {
		return Options{}, errors.New("qmd export collection name is invalid")
	}
	bounds, err := normalizeOptions(options)
	if err != nil {
		return Options{}, err
	}
	if err := preflightRoot(ctx, root); err != nil {
		return Options{}, nativeError(err)
	}
	return bounds, nil
}

func publishWithHooks(ctx context.Context, root, collection string, sources []Source, reader BlobReader, options Options, hooks publishHooks) (Receipt, error) {
	bounds, err := publicationPreflight(ctx, root, collection, reader, options)
	if err != nil {
		return Receipt{}, err
	}
	generation, err := Build(ctx, collection, sources, reader, bounds)
	if err != nil {
		return Receipt{}, nativeError(err)
	}
	return publishGeneration(ctx, root, generation, bounds, hooks)
}

// PublishActive obtains exactly one bounded catalog snapshot before building.
func PublishActive(ctx context.Context, root, collection string, catalog SourceCatalog, reader BlobReader, options Options) (Receipt, error) {
	if err := checkRootRequest(ctx, root); err != nil {
		return Receipt{}, err
	}
	if catalog == nil {
		return Receipt{}, errors.New("qmd export requires a source catalog")
	}
	bounds, err := publicationPreflight(ctx, root, collection, reader, options)
	if err != nil {
		return Receipt{}, err
	}
	sources, err := catalog.QMDExportSources(ctx, bounds.MaxDocuments)
	if err != nil {
		return Receipt{}, nativeError(err)
	}
	generation, err := Build(ctx, collection, sources, reader, bounds)
	if err != nil {
		return Receipt{}, nativeError(err)
	}
	return publishGeneration(ctx, root, generation, bounds, publishHooks{})
}

func randomPublicationName() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nativeError(err)
	}
	return hex.EncodeToString(nonce[:]), nil
}

func syncPublicationFile(file *os.File, hooks publishHooks) error {
	if hooks.beforeFileSync != nil {
		if err := hooks.beforeFileSync(file); err != nil {
			return nativeError(err)
		}
	}
	return nativeError(file.Sync())
}

func recheckPublicationOwnership(ctx context.Context, root *ownedRoot) error {
	if err := validatePrivateEntry(root.root.file, true); err != nil {
		return err
	}
	if _, err := root.root.entries(ctx, rootEntryMaximum); err != nil {
		return err
	}
	marker, err := readRootMarker(root.root)
	if err != nil {
		return err
	}
	if marker != root.marker {
		return errIdentity
	}
	if err := validatePrivateEntry(root.lock, false); err != nil {
		return err
	}
	if err := root.root.sameEntry(lockName, root.lockIdentity); err != nil {
		return err
	}
	for name, dir := range map[string]*anchoredDir{"generations": root.generations, ".staging": root.staging} {
		if err := validatePrivateEntry(dir.file, true); err != nil {
			return err
		}
		info, err := dir.file.Stat()
		if err != nil {
			return nativeError(err)
		}
		if err := root.root.sameEntry(name, entryIdentity{info: info}); err != nil {
			return err
		}
	}
	return validateCurrent(root.root)
}

func writeStageFile(ctx context.Context, parent *anchoredDir, name string, data []byte, ledger *entryLedger, hooks publishHooks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, id, err := parent.createFile(name, stableEntry)
	if err != nil {
		return err
	}
	*ledger = append(*ledger, ledgerEntry{parent: parent, name: name, id: id})
	_, err = f.Write(data)
	return errors.Join(nativeError(err), syncPublicationFile(f, hooks), nativeError(f.Close()), ctx.Err())
}

func createStage(ctx context.Context, root *ownedRoot, g Generation, bounds Options, ledger *entryLedger, hooks publishHooks) (*anchoredDir, error) {
	name, err := randomPublicationName()
	if err != nil {
		return nil, err
	}
	stage, id, err := root.staging.createDir(name, movableEntry)
	if err != nil {
		return nil, err
	}
	*ledger = append(*ledger, ledgerEntry{parent: root.staging, name: name, id: id, dir: stage})
	collection, id, err := stage.createDir("collection", stableEntry)
	if err != nil {
		return nil, err
	}
	*ledger = append(*ledger, ledgerEntry{parent: stage, name: "collection", id: id, dir: collection})
	documents, id, err := collection.createDir("documents", stableEntry)
	if err != nil {
		return nil, err
	}
	*ledger = append(*ledger, ledgerEntry{parent: collection, name: "documents", id: id, dir: documents})
	prefixes := make(map[string]*anchoredDir)
	for _, entry := range g.Manifest.Entries {
		parts := strings.Split(entry.RelativePath, "/")
		prefix := prefixes[parts[1]]
		if prefix == nil {
			prefix, id, err = documents.createDir(parts[1], stableEntry)
			if err != nil {
				return nil, err
			}
			*ledger = append(*ledger, ledgerEntry{parent: documents, name: parts[1], id: id, dir: prefix})
			prefixes[parts[1]] = prefix
		}
		if err := writeStageFile(ctx, prefix, parts[2], g.documents[entry.RelativePath], ledger, hooks); err != nil {
			return nil, err
		}
	}
	encoded, err := encodeManifest(g.Manifest, bounds.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	if err := writeStageFile(ctx, stage, "manifest.json", encoded, ledger, hooks); err != nil {
		return nil, err
	}
	stamp := generationStamp{Format: "docbank-qmd-generation", Version: 1, RootID: root.marker.ID, GenerationID: g.ID, ManifestChecksum: g.ID}
	encoded, err = json.Marshal(stamp, json.Deterministic(true))
	if err != nil {
		return nil, errGenerationInvalid
	}
	if err := writeStageFile(ctx, stage, generationStampName, append(encoded, '\n'), ledger, hooks); err != nil {
		return nil, err
	}
	if err := confirmStage(*ledger); err != nil {
		return nil, err
	}
	return stage, nil
}

// accumulatePost preserves all causes while choosing one fixed priority code.
func accumulatePost(err error, code string, status CleanupStatus, cause error) error {
	if cause == nil {
		if previous, ok := errors.AsType[*PostPublicationError](err); ok {
			return &PostPublicationError{Code: previous.Code, Cleanup: status, cause: previous.cause}
		}
		return err
	}
	if previous, ok := errors.AsType[*PostPublicationError](err); ok {
		priority := map[string]int{"confirmation_incomplete": 0, "publication_canceled": 1, "cleanup_incomplete": 2, "release_incomplete": 3}
		if priority[previous.Code] < priority[code] {
			code = previous.Code
		}
		cause = errors.Join(previous.cause, cause)
	}
	return &PostPublicationError{Code: code, Cleanup: status, cause: cause}
}

func publishGeneration(ctx context.Context, absolute string, g Generation, bounds Options, hooks publishHooks) (selected Receipt, retErr error) {
	owned, err := acquireOwnedRoot(ctx, absolute, hooks.ownership)
	if err != nil {
		return Receipt{}, nativeError(err)
	}
	var ledger entryLedger
	var pointer *os.File
	var pointerID entryIdentity
	var pointerName string
	var status CleanupStatus
	defer func() {
		var cleanupErr error
		if selected.GenerationID == "" {
			// No detached work: cancellation preserves the exact ledger in place.
			cleanupErr = ledger.remove(ctx)
			if pointer != nil {
				cleanupErr = errors.Join(cleanupErr, nativeError(pointer.Close()))
				pointer = nil
				// This one captured entry needs no scan or detached cleanup. Keep
				// its exact rollback independent of cancellation of the ledger.
				cleanupErr = errors.Join(cleanupErr, owned.root.removeEntry(pointerName, pointerID, false))
			}
		}
		closeErr := ledger.close()
		if pointer != nil {
			closeErr = errors.Join(closeErr, nativeError(pointer.Close()))
		}
		closeErr = errors.Join(closeErr, nativeError(owned.Close()))
		if hooks.afterRelease != nil {
			closeErr = errors.Join(closeErr, nativeError(hooks.afterRelease()))
		}
		if selected.GenerationID == "" {
			retErr = errors.Join(retErr, cleanupErr, closeErr)
		} else {
			retErr = accumulatePost(retErr, "release_incomplete", status, closeErr)
			retErr = accumulatePost(retErr, "publication_canceled", status, ctx.Err())
		}
	}()
	stage, err := createStage(ctx, owned, g, bounds, &ledger, hooks)
	if err != nil {
		return Receipt{}, err
	}
	verified, err := verifyOwnedGeneration(ctx, owned, owned.staging, ledger[0].name, g.ID, bounds, nil)
	if err != nil {
		return Receipt{}, err
	}
	err = errors.Join(verified.matches(g, bounds), verified.ledger.close())
	if err != nil {
		return Receipt{}, err
	}
	// All child pins must close before the enclosing movable stage is renamed.
	if err := ledger[1:].close(); err != nil {
		return Receipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	err = moveDirectoryNoReplace(owned.staging, ledger[0].name, stage, ledger[0].id, owned.generations, g.ID)
	if err != nil {
		// A native collision is not assumed to be ownership. An unrelated error
		// also cannot authorize overwriting any existing entry.
		existing, verifyErr := verifyOwnedGeneration(ctx, owned, owned.generations, g.ID, g.ID, bounds, nil)
		if verifyErr != nil {
			return Receipt{}, errors.Join(err, verifyErr)
		}
		verifyErr = errors.Join(existing.matches(g, bounds), existing.ledger.close())
		if verifyErr != nil {
			return Receipt{}, errors.Join(err, verifyErr)
		}
		if err := ledger.remove(ctx); err != nil {
			return Receipt{}, err
		}
		if err := ledger.close(); err != nil {
			return Receipt{}, err
		}
		ledger = nil
	} else {
		ledger[0].parent, ledger[0].name = owned.generations, g.ID
		if err := stage.Close(); err != nil {
			return Receipt{}, nativeError(err)
		}
	}
	if err := confirmPlacement(owned); err != nil {
		return Receipt{}, err
	}
	pointerName, err = randomPublicationName()
	if err != nil {
		return Receipt{}, err
	}
	pointer, pointerID, err = owned.root.createFile(pointerName, movableEntry)
	if err != nil {
		return Receipt{}, err
	}
	_, err = pointer.WriteString(g.ID + "\n")
	if err := errors.Join(nativeError(err), syncPublicationFile(pointer, hooks)); err != nil {
		return Receipt{}, err
	}
	if hooks.beforeCurrent != nil {
		hooks.beforeCurrent()
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	// Callback interference cannot select a substituted or damaged generation.
	if err := recheckPublicationOwnership(ctx, owned); err != nil {
		return Receipt{}, err
	}
	verified, err = verifyOwnedGeneration(ctx, owned, owned.generations, g.ID, g.ID, bounds, nil)
	if err != nil {
		return Receipt{}, err
	}
	err = errors.Join(verified.matches(g, bounds), verified.ledger.close())
	if err != nil {
		return Receipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := replaceFile(owned.root, pointerName, pointer, pointerID, owned.root, "CURRENT"); err != nil {
		return Receipt{}, err
	}
	selected = Receipt{GenerationID: g.ID, CollectionPath: filepath.Join(absolute, "generations", g.ID, "collection"), Manifest: g.Manifest}
	if hooks.afterCurrent != nil {
		hooks.afterCurrent()
	}
	if hooks.beforeConfirmation != nil {
		retErr = accumulatePost(retErr, "confirmation_incomplete", status, hooks.beforeConfirmation())
	}
	retErr = accumulatePost(retErr, "confirmation_incomplete", status, confirmPublication(owned, pointer, hooks))
	if hooks.beforeCleanupRemove == nil {
		status, err = cleanupOwnedGenerations(ctx, owned, g.ID, bounds)
	} else {
		status, err = cleanupWithBudget(ctx, owned, g.ID, bounds, newCleanupBudget(), candidateMaximum, hooks.beforeCleanupRemove)
	}
	retErr = accumulatePost(retErr, "cleanup_incomplete", status, err)
	retErr = accumulatePost(retErr, "confirmation_incomplete", status, confirmCleanup(owned))
	retErr = accumulatePost(retErr, "publication_canceled", status, ctx.Err())
	return selected, retErr
}
