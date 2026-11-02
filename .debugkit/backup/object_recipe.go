package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

const (
	objectChunkBytes = 64 << 20
	maxObjectChunks  = 1 << 20
	maxRecipeBytes   = 128 << 20
	// MaxObjectBytes bounds a logical object represented by one bounded recipe.
	// Individual pack frames remain limited independently by kit/pack.
	MaxObjectBytes int64 = objectChunkBytes * maxObjectChunks
)

type objectChunk struct {
	Blob  string `json:"blob"`
	Bytes int64  `json:"bytes"`
}

type objectChunks []objectChunk

// UnmarshalJSON validates entries before retaining them and stops at the count
// limit. The recipe's byte limit alone does not bound decoded array allocation.
func (chunks *objectChunks) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return errors.New("backup: object chunks must be an array")
	}
	*chunks = nil
	for decoder.More() {
		if len(*chunks) == maxObjectChunks {
			return errors.New("backup: object recipe exceeds chunk limit")
		}
		var chunk objectChunk
		if err := decoder.Decode(&chunk); err != nil {
			return err
		}
		if chunk.Bytes <= 0 || chunk.Bytes > objectChunkBytes {
			return errors.New("backup: invalid object chunk size")
		}
		if _, err := pack.ParseBlobID(chunk.Blob); err != nil {
			return fmt.Errorf("backup: object chunk identity: %w", err)
		}
		*chunks = append(*chunks, chunk)
	}
	_, err = decoder.Token()
	return err
}

type objectRecipe struct {
	Version  int          `json:"version"`
	Blob     string       `json:"blob"`
	Bytes    int64        `json:"bytes"`
	Chunks   objectChunks `json:"chunks"`
	recordID pack.BlobID
}

// captureObject keeps one logical hash while putting only bounded frames in
// packs. Publication of the enclosing manifest remains the acceptance point.
func captureObject(ctx context.Context, source io.Reader, expected int64, expectedID *pack.BlobID, appender *PackAppender) (pack.BlobID, int64, string, error) {
	if expected < -1 || expected > MaxObjectBytes {
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: invalid logical object size %d", expected)
	}
	digest := sha256.New()
	recipe := objectRecipe{Version: 1}
	bufferSize := int64(objectChunkBytes)
	if expected >= 0 {
		bufferSize = min(bufferSize, expected+1)
	}
	buffer := make([]byte, bufferSize)
	reader := &contextio.Reader{Context: ctx, Reader: source}
	for {
		n, err := io.ReadFull(reader, buffer)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return pack.BlobID{}, 0, "", fmt.Errorf("backup: reading logical object: %w", err)
		}
		if n > 0 && len(buffer) > 0 {
			if len(recipe.Chunks) == maxObjectChunks {
				return pack.BlobID{}, 0, "", errors.New("backup: logical object exceeds chunk limit")
			}
			recipe.Bytes += int64(n)
			if expected >= 0 && recipe.Bytes > expected {
				return pack.BlobID{}, 0, "", errors.New("backup: logical object exceeds declared size")
			}
			_, _ = digest.Write(buffer[:n])
			id, _, addErr := appender.Add(buffer[:n])
			if addErr != nil {
				return pack.BlobID{}, 0, "", addErr
			}
			recipe.Chunks = append(recipe.Chunks, objectChunk{Blob: id.String(), Bytes: int64(n)})
		}
		if err != nil {
			break
		}
	}
	if expected >= 0 && recipe.Bytes != expected {
		return pack.BlobID{}, 0, "", errors.New("backup: logical object differs from declared size")
	}
	var id pack.BlobID
	copy(id[:], digest.Sum(nil))
	if expectedID != nil && id != *expectedID {
		return pack.BlobID{}, 0, "", errors.New("backup: content does not match its hash (live store corruption)")
	}
	if len(recipe.Chunks) < 2 {
		if len(recipe.Chunks) == 0 {
			if _, _, err := appender.Add(nil); err != nil {
				return pack.BlobID{}, 0, "", err
			}
		}
		return id, recipe.Bytes, "", nil
	}
	recipe.Blob = id.String()
	raw, err := json.Marshal(recipe)
	if err != nil {
		return pack.BlobID{}, 0, "", err
	}
	recipeID, _, err := appender.Add(raw)
	return id, recipe.Bytes, recipeID.String(), err
}

func decodeObjectRecipe(raw []byte) (objectRecipe, error) {
	var recipe objectRecipe
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recipe); err != nil {
		return recipe, fmt.Errorf("backup: decoding object recipe: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return recipe, errors.New("backup: trailing object recipe data")
	}
	if recipe.Version != 1 || recipe.Bytes <= 0 || recipe.Bytes > MaxObjectBytes || len(recipe.Chunks) < 2 {
		return recipe, errors.New("backup: invalid object recipe version, size or chunk count")
	}
	if _, err := pack.ParseBlobID(recipe.Blob); err != nil {
		return recipe, fmt.Errorf("backup: object recipe identity: %w", err)
	}
	var total int64
	for _, chunk := range recipe.Chunks {
		total += chunk.Bytes
	}
	if total != recipe.Bytes {
		return recipe, errors.New("backup: object recipe chunk lengths differ from total")
	}
	return recipe, nil
}

func loadObjectRecipes(ctx context.Context, repo *Repo, known map[pack.BlobID]IndexEntry, manifest *Manifest, ext string) (map[pack.BlobID]objectRecipe, error) {
	return loadObjectRecipesWith(ctx, known, manifest, func(ctx context.Context, id pack.BlobID) (*BlobStream, error) {
		return repo.OpenBlob(ctx, known, id, nil, ext)
	})
}

type blobOpener func(context.Context, pack.BlobID) (*BlobStream, error)

func loadObjectRecipesWith(ctx context.Context, known map[pack.BlobID]IndexEntry, manifest *Manifest, open blobOpener) (map[pack.BlobID]objectRecipe, error) {
	ids := append([]string(nil), manifest.Attachments.Recipes...)
	ids = append(ids, manifest.Extras.Recipes...)
	if manifest.Metadata != nil && manifest.Metadata.Recipe != "" {
		ids = append(ids, manifest.Metadata.Recipe)
	}
	recipes := make(map[pack.BlobID]objectRecipe, len(ids))
	for _, encodedID := range ids {
		id, err := pack.ParseBlobID(encodedID)
		if err != nil {
			return nil, fmt.Errorf("backup: recipe identity: %w", err)
		}
		stream, err := open(ctx, id)
		if err != nil {
			return nil, err
		}
		if stream.Size() > maxRecipeBytes {
			_ = stream.Close()
			return nil, errors.New("backup: object recipe exceeds metadata limit")
		}
		raw, readErr := io.ReadAll(stream)
		if err := errors.Join(readErr, stream.Close()); err != nil {
			return nil, err
		}
		recipe, err := decodeObjectRecipe(raw)
		if err != nil {
			return nil, err
		}
		objectID, _ := pack.ParseBlobID(recipe.Blob)
		if manifest.Metadata != nil && encodedID == manifest.Metadata.Recipe &&
			(recipe.Blob != manifest.Metadata.Blob || recipe.Bytes != manifest.Metadata.Bytes) {
			return nil, errors.New("backup: metadata recipe differs from manifest identity or size")
		}
		if old, exists := recipes[objectID]; exists && old.recordID != id {
			return nil, errors.New("backup: conflicting recipes for one logical object")
		}
		for _, chunk := range recipe.Chunks {
			chunkID, _ := pack.ParseBlobID(chunk.Blob)
			if _, exists := known[chunkID]; !exists {
				return nil, fmt.Errorf("backup: object chunk %s not present in any index", chunkID)
			}
		}
		recipe.recordID = id
		recipes[objectID] = recipe
	}
	return recipes, nil
}

func captureLargeAttachment(ctx context.Context, root *os.Root, refs []ContentRef, index int, parentSeen map[string]bool, appender *PackAppender, opts CaptureOptions, out *AttachmentCapture) error {
	ref := refs[index]
	if ref.Size < -1 || ref.Size > MaxObjectBytes {
		return fmt.Errorf("backup: invalid attachment size %d", ref.Size)
	}
	id, err := parseCanonicalCaptureID(ref.Hash)
	if err != nil {
		return err
	}
	var reader io.ReadCloser
	expected := int64(-1)
	if opts.Source != nil {
		reader, err = opts.Source.Open(ctx, ref)
	} else {
		rel, pathErr := captureRelPath(ref)
		if pathErr != nil {
			return pathErr
		}
		before, statErr := root.Stat(rel)
		if statErr != nil {
			return statErr
		}
		if !before.Mode().IsRegular() {
			return fmt.Errorf("backup: attachment %q is not a regular file", rel)
		}
		file, openErr := root.Open(rel)
		if openErr != nil {
			return openErr
		}
		after, statErr := file.Stat()
		if statErr != nil || !os.SameFile(before, after) {
			return errors.Join(errors.New("backup: file changed during capture"), statErr, file.Close())
		}
		expected, reader = after.Size(), file
	}
	if err != nil {
		return err
	}
	_, size, recipe, captureErr := captureObject(ctx, reader, expected, &id, appender)
	if err := errors.Join(captureErr, reader.Close()); err != nil {
		return fmt.Errorf("backup: capturing attachment %s: %w", ref.Hash, err)
	}
	if recipe != "" {
		out.Recipes = append(out.Recipes, recipe)
	}
	return recordCapture(ctx, captureResult{index: index, id: id, size: size, known: true}, refs, parentSeen, appender, opts, out)
}

type verifiedObjectStream interface {
	io.ReadCloser
	Size() int64
	Verified() bool
}

func openObject(ctx context.Context, repo *Repo, known map[pack.BlobID]IndexEntry, id pack.BlobID, recipes map[pack.BlobID]objectRecipe, ext string) (verifiedObjectStream, error) {
	return openObjectWith(ctx, id, recipes, func(ctx context.Context, id pack.BlobID) (*BlobStream, error) {
		return repo.OpenBlob(ctx, known, id, nil, ext)
	})
}

func openObjectWith(ctx context.Context, id pack.BlobID, recipes map[pack.BlobID]objectRecipe, open blobOpener) (verifiedObjectStream, error) {
	recipe, found := recipes[id]
	if !found {
		return open(ctx, id)
	}
	return &recipeStream{ctx: ctx, open: open, recipe: recipe, digest: sha256.New()}, nil
}

type recipeStream struct {
	ctx      context.Context
	open     blobOpener
	recipe   objectRecipe
	digest   hash.Hash
	next     int
	current  *BlobStream
	read     int64
	verified bool
	closed   bool
	err      error
}

func (s *recipeStream) Size() int64    { return s.recipe.Bytes }
func (s *recipeStream) Verified() bool { return s.verified }

func (s *recipeStream) Read(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("backup: reading closed object stream")
	}
	if s.err != nil {
		return 0, s.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := s.ctx.Err(); err != nil {
			s.err = err
			return 0, err
		}
		if s.current == nil {
			if s.next == len(s.recipe.Chunks) {
				var id pack.BlobID
				copy(id[:], s.digest.Sum(nil))
				if s.read != s.recipe.Bytes || id.String() != s.recipe.Blob {
					s.err = errors.New("backup: reconstructed object hash or length mismatch")
					return 0, s.err
				}
				s.verified = true
				return 0, io.EOF
			}
			chunk := s.recipe.Chunks[s.next]
			id, _ := pack.ParseBlobID(chunk.Blob)
			stream, err := s.open(s.ctx, id)
			if err != nil {
				s.err = err
				return 0, err
			}
			s.current = stream
			if stream.Size() != chunk.Bytes {
				s.err = errors.Join(errors.New("backup: chunk length differs from recipe"), stream.Close())
				s.current = nil
				return 0, s.err
			}
			s.next++
		}
		n, err := s.current.Read(p)
		_, _ = s.digest.Write(p[:n])
		s.read += int64(n)
		if errors.Is(err, io.EOF) {
			err = s.current.Close()
			s.current = nil
		}
		if err != nil {
			s.err = err
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (s *recipeStream) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.current != nil {
		s.err = errors.Join(s.err, s.current.Close())
	}
	if !s.verified {
		s.err = errors.Join(s.err, pack.ErrVerificationIncomplete)
	}
	return s.err
}

func (s *restoreState) checkObject(id pack.BlobID, size int64) error {
	if recipe, ok := s.recipes[id]; ok {
		if recipe.Bytes != size {
			return errors.New("backup: object recipe differs from recorded size")
		}
		return nil
	}
	if _, ok := s.known[id]; !ok {
		return fmt.Errorf("backup: object %s not present in any index", id)
	}
	return nil
}

func (s *restoreState) restoreChunkedAttachments(ctx context.Context, directory string, inventory restoreAttachmentInventory, totalBytes int64) error {
	var staged []stagedFile
	defer func() { s.removeStagedFiles(staged) }()
	for _, ref := range inventory.chunked {
		id, _ := pack.ParseBlobID(ref.Hash)
		for _, path := range inventory.paths[ref.Hash] {
			var stream io.ReadCloser
			if len(staged) == 0 {
				object, err := openObject(ctx, s.repo, s.known, id, s.recipes, s.app.PackFileExtension())
				if err != nil {
					return err
				}
				stream = object
			} else {
				// Copy the verified private file for additional paths instead
				// of reading and decompressing the backup again.
				file, err := s.root.Open(staged[0].tmpRel)
				if err != nil {
					return err
				}
				stream = file
			}
			if s.compression.Enabled {
				writeErr := s.restoreCompressedContent(ctx, directory, ref, stream)
				if err := errors.Join(writeErr, stream.Close()); err != nil {
					return err
				}
				continue
			}
			rel := filepath.Join(directory, path)
			tmp, stageErr := s.stageRootReaderWithOptions(ctx, rel, stream, ref.Size, 0o600, ".restore-", uint64(MaxObjectBytes))
			if err := errors.Join(stageErr, stream.Close()); err != nil {
				if tmp != "" {
					_ = s.root.Remove(tmp)
				}
				return err
			}
			staged = append(staged, stagedFile{rel: rel, tmpRel: tmp})
		}
		for _, file := range staged {
			if err := s.promoteRootFile(file.tmpRel, file.rel); err != nil {
				return err
			}
		}
		staged = staged[:0]
		s.done++
		s.doneByte += ref.Size
		s.progress.emit(ProgressEvent{Stage: ProgressStageAttachments, Done: s.done, Total: int64(len(inventory.refs)), BytesDone: s.doneByte, BytesTotal: totalBytes})
	}
	return nil
}

func (s *verifyState) checkObjectRecipes(m *Manifest) bool {
	var err error
	s.recipes, err = loadObjectRecipes(s.ctx, s.repo, s.known, m, s.app.PackFileExtension())
	if err != nil {
		s.problem(m.SnapshotID, err.Error())
		return false
	}
	if s.recipeVerdicts == nil {
		s.recipeVerdicts = make(map[pack.BlobID]error)
	}
	for id, recipe := range s.recipes {
		s.blob(recipe.recordID, m.SnapshotID, false)
		for _, chunk := range recipe.Chunks {
			chunkID, _ := pack.ParseBlobID(chunk.Blob)
			s.blob(chunkID, m.SnapshotID, false)
		}
		if s.quick {
			continue
		}
		verdict, checked := s.recipeVerdicts[recipe.recordID]
		if !checked {
			// ponytail: recipes verify serially; use the read pool if throughput requires it.
			stream, openErr := openObject(s.ctx, s.repo, s.known, id, s.recipes, s.app.PackFileExtension())
			verdict = openErr
			if openErr == nil {
				s.drainTotal++
				var readErr error
				for readErr == nil {
					var n int64
					n, readErr = io.CopyN(io.Discard, stream, objectChunkBytes)
					s.result.BytesRead += n
					if n > 0 {
						s.emitDrainProgressLocked()
					}
				}
				if errors.Is(readErr, io.EOF) {
					readErr = nil
				}
				verdict = errors.Join(readErr, stream.Close())
				s.drainDone++
				s.emitDrainProgressLocked()
			}
			s.recipeVerdicts[recipe.recordID] = verdict
		}
		if verdict != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("object %s: %v", id, verdict))
		}
	}
	return true
}

func (s *verifyState) checkRecipeSize(id pack.BlobID, size int64, snapshot string) bool {
	recipe, found := s.recipes[id]
	if found && recipe.Bytes != size {
		s.problem(snapshot, fmt.Sprintf("object %s recipe size %d differs from recorded size %d", id, recipe.Bytes, size))
	}
	return found
}
