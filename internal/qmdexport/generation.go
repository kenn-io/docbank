package qmdexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
)

const generationStampName = ".docbank-qmd-generation.json"

var errGenerationInvalid = errors.New("qmd export generation is invalid")

type generationStamp struct {
	Format           string `json:"format"`
	Version          int    `json:"version"`
	RootID           string `json:"root_id"`
	GenerationID     string `json:"generation_id"`
	ManifestChecksum string `json:"manifest_checksum"`
}

// entryLedger retains parent handles and exact identities, in creation order.
// Directories close in reverse order, before their own entry is removed, so
// Windows stable child pins never block deletion of a verified directory.
type ledgerEntry struct {
	parent *anchoredDir
	name   string
	id     entryIdentity
	dir    *anchoredDir
}
type entryLedger []ledgerEntry

func (l entryLedger) close() error {
	var err error
	for _, e := range slices.Backward(l) {
		err = errors.Join(err, e.dir.Close())
	}
	return nativeError(err)
}

func (l entryLedger) remove(ctx context.Context) error {
	// A stage move requires closing child pins. Reopen only ledger directories,
	// through the recorded parent chain, and recheck each identity before use.
	for _, e := range l {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.dir == nil || e.dir.file != nil {
			continue
		}
		dir, _, err := e.parent.openDir(e.name)
		if err != nil {
			return err
		}
		e.dir.file = dir.file
		if err := heldIdentity(e.dir.file, e.id); err != nil {
			return err
		}
	}
	for _, e := range slices.Backward(l) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.dir.Close(); err != nil {
			return nativeError(err)
		}
		if err := e.parent.removeEntry(e.name, e.id, e.dir != nil); err != nil {
			return err
		}
	}
	return nil
}

type verifiedGeneration struct {
	manifest  Manifest
	canonical []byte
	ledger    entryLedger
	bodySizes map[string]int64
}

// readGenerationFile reserves the size+1 growth probe before any read.
func readGenerationFile(ctx context.Context, parent *anchoredDir, name string, maximum int64, budget *cleanupBudget) (data []byte, id entryIdentity, err error) {
	if err := ctx.Err(); err != nil {
		return nil, id, err
	}
	f, id, err := parent.openFile(name)
	if err != nil {
		return nil, id, err
	}
	defer func() {
		err = errors.Join(err, nativeError(f.Close()), ctx.Err())
		if err != nil {
			data = nil
		}
	}()
	size := id.info.Size()
	if size < 0 || size > maximum {
		return nil, id, errGenerationInvalid
	}
	if budget != nil {
		if size+1 > budget.bytes {
			return nil, id, errCleanupBound
		}
		budget.bytes -= size + 1
	}
	data, err = io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: f}, size+1))
	if err != nil {
		return nil, id, nativeError(err)
	}
	if int64(len(data)) != size {
		return nil, id, errGenerationInvalid
	}
	if err := parent.sameEntry(name, id); err != nil {
		return nil, id, err
	}
	return data, id, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, errors.Join(err, canceled)
	}
	return n, err
}

type generationMetadata struct {
	manifest            Manifest
	canonical           []byte
	stampID, manifestID entryIdentity
}

func readGenerationMetadata(ctx context.Context, root *ownedRoot, dir *anchoredDir, generationID string, bounds Options, budget *cleanupBudget) (generationMetadata, error) {
	stampData, stampID, err := readGenerationFile(ctx, dir, generationStampName, markerMaximum, nil)
	if err != nil {
		return generationMetadata{}, err
	}
	var stamp generationStamp
	if err := json.Unmarshal(stampData, &stamp, json.RejectUnknownMembers(true)); err != nil ||
		stamp.Format != "docbank-qmd-generation" || stamp.Version != 1 || stamp.RootID != root.marker.ID ||
		!validChecksum(stamp.GenerationID) || stamp.GenerationID != stamp.ManifestChecksum ||
		(generationID != "" && generationID != stamp.GenerationID) {
		return generationMetadata{}, errGenerationInvalid
	}
	data, manifestID, err := readGenerationFile(ctx, dir, "manifest.json", bounds.MaxManifestBytes, budget)
	if err != nil {
		return generationMetadata{}, err
	}
	manifest, err := decodeManifest(data, stamp.GenerationID, bounds)
	return generationMetadata{manifest: manifest, canonical: data, stampID: stampID, manifestID: manifestID}, err
}

func verificationEntries(ctx context.Context, dir *anchoredDir, expected map[string]bool, budget *cleanupBudget) error {
	entries, err := enumerateCleanupEntries(ctx, dir, len(expected), budget)
	if err != nil {
		if errors.Is(err, errDirectoryBound) {
			return errGenerationInvalid
		}
		return err
	}
	if len(entries) != len(expected) {
		return errGenerationInvalid
	}
	for _, entry := range entries {
		directory, exists := expected[entry.Name()]
		if !exists || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() != directory ||
			(!directory && !entry.Type().IsRegular()) {
			return errGenerationInvalid
		}
	}
	return nil
}

func verifyOwnedGeneration(ctx context.Context, root *ownedRoot, parent *anchoredDir, name, generationID string, bounds Options, budget *cleanupBudget) (_ *verifiedGeneration, err error) {
	if budget == nil {
		budget = newCleanupBudget()
	}
	var dir *anchoredDir
	var id entryIdentity
	if parent == root.staging {
		dir, id, err = parent.openMovableDir(name)
	} else {
		dir, id, err = parent.openDir(name)
	}
	if err != nil {
		return nil, err
	}
	v := &verifiedGeneration{ledger: entryLedger{{parent: parent, name: name, id: id, dir: dir}}, bodySizes: make(map[string]int64)}
	defer func() {
		if err != nil {
			err = errors.Join(err, v.ledger.close())
		}
	}()
	// The exact expected sets permit MaxDocuments+260 actual entries: three
	// root entries, documents, at most 256 prefixes, and the bounded bodies.
	// Shared scan-probe reservations are separate from that layout ceiling.
	local := *budget
	initial := local
	defer func() { budget.entries -= initial.entries - local.entries; budget.bytes -= initial.bytes - local.bytes }()
	if err := verificationEntries(ctx, dir, map[string]bool{generationStampName: false, "manifest.json": false, "collection": true}, &local); err != nil {
		return nil, err
	}
	metadata, err := readGenerationMetadata(ctx, root, dir, generationID, bounds, &local)
	if err != nil {
		return nil, err
	}
	v.manifest, v.canonical = metadata.manifest, metadata.canonical
	v.ledger = append(v.ledger, ledgerEntry{parent: dir, name: generationStampName, id: metadata.stampID}, ledgerEntry{parent: dir, name: "manifest.json", id: metadata.manifestID})
	collection, collectionID, err := dir.openDir("collection")
	if err != nil {
		return nil, err
	}
	v.ledger = append(v.ledger, ledgerEntry{parent: dir, name: "collection", id: collectionID, dir: collection})
	if err := verificationEntries(ctx, collection, map[string]bool{"documents": true}, &local); err != nil {
		return nil, err
	}
	documents, documentsID, err := collection.openDir("documents")
	if err != nil {
		return nil, err
	}
	v.ledger = append(v.ledger, ledgerEntry{parent: collection, name: "documents", id: documentsID, dir: documents})
	prefixes := make(map[string][]Entry)
	expected := make(map[string]bool)
	for _, entry := range metadata.manifest.Entries {
		prefix := strings.Split(entry.RelativePath, "/")[1]
		prefixes[prefix] = append(prefixes[prefix], entry)
		expected[prefix] = true
	}
	if err := verificationEntries(ctx, documents, expected, &local); err != nil {
		return nil, err
	}
	for prefix, entries := range prefixes {
		prefixDir, prefixID, err := documents.openDir(prefix)
		if err != nil {
			return nil, err
		}
		v.ledger = append(v.ledger, ledgerEntry{parent: documents, name: prefix, id: prefixID, dir: prefixDir})
		expected := make(map[string]bool, len(entries))
		for _, entry := range entries {
			expected[strings.Split(entry.RelativePath, "/")[2]] = false
		}
		if err := verificationEntries(ctx, prefixDir, expected, &local); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := strings.Split(entry.RelativePath, "/")[2]
			id, size, err := verifyBody(ctx, prefixDir, name, entry, bounds, &local)
			if err != nil {
				return nil, err
			}
			v.ledger = append(v.ledger, ledgerEntry{parent: prefixDir, name: name, id: id})
			v.bodySizes[entry.RelativePath] = size
		}
	}
	if err := parent.sameEntry(name, id); err != nil {
		return nil, err
	}
	return v, ctx.Err()
}

func verifyBody(ctx context.Context, parent *anchoredDir, name string, entry Entry, bounds Options, budget *cleanupBudget) (id entryIdentity, size int64, err error) {
	f, id, err := parent.openFile(name)
	if err != nil {
		return id, 0, err
	}
	defer func() { err = errors.Join(err, nativeError(f.Close()), ctx.Err()) }()
	size = id.info.Size()
	if size < 0 || size > entry.BlobSize || size > bounds.MaxDocumentBytes {
		return id, 0, errGenerationInvalid
	}
	if size+1 > budget.bytes {
		return id, 0, errCleanupBound
	}
	budget.bytes -= size + 1
	digest := sha256.New()
	n, err := io.CopyBuffer(digest, io.LimitReader(&contextReader{ctx: ctx, reader: f}, size+1), make([]byte, 32<<10))
	if err != nil {
		return id, 0, nativeError(err)
	}
	if n != size || hex.EncodeToString(digest.Sum(nil)) != entry.ExportedMarkdownSHA256 {
		return id, 0, errGenerationInvalid
	}
	return id, size, parent.sameEntry(name, id)
}

func (v *verifiedGeneration) matches(g Generation, bounds Options) error {
	encoded, err := encodeManifest(g.Manifest, bounds.MaxManifestBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, v.canonical) || len(g.documents) != len(v.bodySizes) {
		return errGenerationInvalid
	}
	for path, body := range g.documents {
		if size, ok := v.bodySizes[path]; !ok || size != int64(len(body)) {
			return errGenerationInvalid
		}
	}
	// Exact canonical entries already bind every fully hashed body's digest.
	return nil
}
