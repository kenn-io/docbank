package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

const (
	attachmentListMagic   = "MVAL"
	attachmentListVersion = 1
	attachmentEntrySize   = 32 + 8
)

// ContentRef identifies one attachment (or thumbnail) content blob by its
// SHA-256 and size. Size -1 means unknown until read. Unknown-size ContentSource
// references use the parallel single-blob path, capped at pack.MaxRawLen;
// sources must declare larger sizes to select chunked capture.
//
// StoragePath is the blob's location relative to the attachments directory
// as recorded in the archive database; importers may namespace it (for
// example synctech-sms writes "synctech-sms/<aa>/<hash>"). Empty means the
// canonical loose layout "<aa>/<hash>". It is capture-time routing only and
// is not serialized into attachment list segments, which carry hash and size.
type ContentRef struct {
	Hash        string
	Size        int64
	StoragePath string
}

// EncodeAttachmentList serializes refs in their given (first-seen) order.
func EncodeAttachmentList(refs []ContentRef) ([]byte, error) {
	buf := make([]byte, 0, 4+2+4+len(refs)*attachmentEntrySize+trailerHashLen)
	buf = append(buf, attachmentListMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, attachmentListVersion)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(refs)))
	for _, ref := range refs {
		raw, err := hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("backup: attachment list: bad content hash %q", ref.Hash)
		}
		if ref.Size < 0 {
			return nil, fmt.Errorf(
				"backup: attachment list: negative size %d for content hash %s", ref.Size, ref.Hash)
		}
		buf = append(buf, raw...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(ref.Size))
	}
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...), nil
}

// DecodeAttachmentList parses and integrity-checks a list segment.
func DecodeAttachmentList(data []byte) ([]ContentRef, error) {
	const header = 4 + 2 + 4
	if len(data) < header+trailerHashLen {
		return nil, fmt.Errorf("backup: attachment list truncated (%d bytes)", len(data))
	}
	body, trailer := data[:len(data)-trailerHashLen], data[len(data)-trailerHashLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("backup: attachment list integrity check failed")
	}
	if string(body[:4]) != attachmentListMagic {
		return nil, errors.New("backup: bad attachment list magic")
	}
	if v := binary.LittleEndian.Uint16(body[4:6]); v != attachmentListVersion {
		return nil, fmt.Errorf("backup: unsupported attachment list version %d", v)
	}
	count := binary.LittleEndian.Uint32(body[6:10])
	bodyLen := len(body) - header
	if bodyLen < 0 || uint64(bodyLen) != uint64(count)*attachmentEntrySize {
		return nil, fmt.Errorf("backup: attachment list body size mismatch (count %d)", count)
	}
	refs := make([]ContentRef, 0, count)
	off := header
	for range count {
		size := binary.LittleEndian.Uint64(body[off+32 : off+40])
		// Sizes are non-negative at encode time, so a stored value with the
		// high bit set is forgery or corruption; letting it through would
		// wrap negative through int64 and poison every downstream size sum
		// and comparison.
		if size > math.MaxInt64 {
			return nil, fmt.Errorf(
				"backup: attachment list entry size %d overflows int64", size)
		}
		refs = append(refs, ContentRef{
			Hash: hex.EncodeToString(body[off : off+32]),
			Size: int64(size),
		})
		off += attachmentEntrySize
	}
	return refs, nil
}

// AttachmentCapture reports one snapshot's attachment capture results.
type AttachmentCapture struct {
	Recipes     []string
	NewList     []ContentRef
	NewListBlob pack.BlobID
	HasNewList  bool
	Blobs       int64
	BlobBytes   int64
}

// ContentSource supplies attachment content bytes during capture, replacing
// the engine's own reads of the attachments directory. Implementations
// resolve a ref however the application stores content (loose files, pack
// files, object stores); the engine still verifies every blob's SHA-256
// against ref.Hash and enforces the per-blob size cap, so a source cannot
// weaken capture integrity. Open is called from concurrent capture workers
// and must be safe for concurrent use; it should honor ctx and return
// promptly once ctx is done, or a cancelled capture blocks until every
// in-flight Open returns. Capture uses ref.Size to pace concurrent work and
// scratch use; a declared size that understates the payload weakens that
// admission policy (never integrity), so sources should report actual sizes.
type ContentSource interface {
	Open(ctx context.Context, ref ContentRef) (io.ReadCloser, error)
}

// CaptureOptions tunes CaptureAttachments.
type CaptureOptions struct {
	// Jobs is the number of concurrent read+hash+compress workers. Zero or
	// negative selects one per CPU. Use 1 for strictly serial file reads —
	// the right choice when the live archive sits on a spinning disk or NAS
	// share that degrades under concurrent reads.
	Jobs int
	// Progress, if non-nil, is called after each file is captured with the
	// number of files done so far, the total file count, and the cumulative
	// bytes read; it does not otherwise affect capture behavior.
	Progress func(done, total int, bytesRead int64)
	// Source, when non-nil, supplies attachment bytes instead of the engine
	// reading them from the attachments directory; the directory is then
	// ignored entirely. Reads are still hash-verified and size-capped.
	Source ContentSource
}

// captureResult is one worker's verified output for refs[index]. Plain blobs
// retain only bounded-scratch PreparedBlob handles; encrypted v1 retains its
// authenticated whole frame because that format is not streamable.
type captureResult struct {
	index      int
	size       int64
	id         pack.BlobID
	frame      []byte
	prepared   *pack.PreparedBlob
	compressed bool
	known      bool
	chunked    bool
	err        error
}

// captureMemoryBudget paces concurrent source and preparation scratch work.
// Chunked objects reserve one chunk buffer; ordinary objects reserve their
// declared size (also bounding whole-frame encrypted-v1 heap). A var so tests
// can shrink it.
var captureMemoryBudget int64 = 1 << 30

// byteGate admits work under a byte budget. A request larger than the whole
// budget is admitted once nothing else is in flight, so oversized files
// serialize instead of deadlocking.
type byteGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	held    int64
	budget  int64
	stopped bool
}

func newByteGate(budget int64) *byteGate {
	g := &byteGate{budget: budget}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// acquire blocks until n bytes fit under the budget or the gate is stopped.
func (g *byteGate) acquire(n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.stopped && g.held > 0 && g.held+n > g.budget {
		g.cond.Wait()
	}
	g.held += n
}

func (g *byteGate) release(n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held -= n
	g.cond.Broadcast()
}

// stop unblocks every current and future acquire; called when capture fails
// so a dispatcher waiting on budget can observe the stop channel and exit.
func (g *byteGate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.cond.Broadcast()
}

// CaptureAttachments stores every referenced attachment content blob,
// re-hashing each file as it goes (FORMAT.md, Attachment Lists: backup verifies the live store).
// Refs not present in parentSeen become the snapshot's new list segment.
//
// Reading, hashing, and trial compression fan out to opts.Jobs workers;
// results are recorded in ref order by a single collector feeding the
// appender, so pack contents, list order, accounting, and progress reporting
// match a serial capture exactly. Blobs already stored in the repository are
// detected before compression and skip it entirely, keeping the no-change
// incremental case cheap.
// Chunked files are streamed by the collector, one at a time, while the
// same worker pool prepares ordinary files. Directory refs use their stat
// sizes to select chunking even when the caller does not know their size.
//
// attachmentsDir is ignored entirely when opts.Source is non-nil; content is
// read through the source instead.
func CaptureAttachments(
	ctx context.Context,
	attachmentsDir string, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions,
) (*AttachmentCapture, error) {
	out := &AttachmentCapture{Recipes: []string{}}
	if err := captureContents(ctx, attachmentsDir, refs, parentSeen, appender, opts, out); err != nil {
		return nil, err
	}
	if len(out.NewList) > 0 {
		data, err := EncodeAttachmentList(out.NewList)
		if err != nil {
			return nil, err
		}
		id, _, err := appender.Add(data)
		if err != nil {
			return nil, err
		}
		out.NewListBlob = id
		out.HasNewList = true
	}
	return out, nil
}

// captureContents runs the capture pipeline: a dispatcher hands ref indexes
// to workers under a bounded in-flight window, workers read+hash+compress,
// and the collector (this goroutine) records results strictly in ref order.
// The first error — by ref order, matching what a serial capture would have
// hit first — stops dispatch and is returned after the pipeline drains.
func captureContents(
	ctx context.Context,
	attachmentsDir string, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions, out *AttachmentCapture,
) error {
	if len(refs) == 0 {
		return nil
	}
	var root *os.Root
	if opts.Source == nil {
		// Read every attachment through a root confined to attachmentsDir: a
		// tampered DB row whose storage path resolves through a symlink to a
		// file (or parent directory) outside the attachments tree must not be
		// able to pull arbitrary host files into the backup. os.Root refuses
		// any symlink that escapes the root, and captureRef additionally
		// requires a regular file. os.Root is safe for concurrent use by the
		// workers below.
		var err error
		root, err = os.OpenRoot(attachmentsDir)
		if err != nil {
			return fmt.Errorf("backup: opening attachments directory: %w", err)
		}
		defer func() { _ = root.Close() }()
	}
	workers := opts.Jobs
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(refs))

	// Workers must not read the appender's live known map (the collector
	// mutates it); they consult a snapshot instead. Refs are unique within a
	// run, so the snapshot's answer is exact for every queued blob.
	preKnown := appender.knownSnapshot()
	level := appender.zstdLevel
	streaming := appender.crypter == nil
	scratchDir := appender.repo.Path(stagingDirName)

	// inflight bounds dispatched-but-unrecorded refs; the byte gate below
	// additionally bounds their cumulative scratch or chunk-buffer budget,
	// since a count bound alone allows excessive concurrent scratch work.
	// results has the same capacity as tokens, so workers never block
	// on a stalled collector.
	inflight := workers + 2
	stop := make(chan struct{})
	work := make(chan int)
	results := make(chan captureResult, inflight)
	tokens := make(chan struct{}, inflight)
	gate := newByteGate(captureMemoryBudget)
	// The dispatcher records routing and budget before dispatch; channel sends
	// order the workers' and collector's reads of these slices.
	weights := make([]int64, len(refs))
	chunked := make([]bool, len(refs))

	go func() {
		defer close(work)
		for i := range refs {
			if opts.Source != nil {
				// A source ref is weighted by its declared size; Size is -1
				// when unknown, same as today's stat-failure fallback below.
				weights[i] = max(refs[i].Size, 0)
			} else if rel, err := captureRelPath(refs[i]); err == nil {
				if info, err := root.Stat(rel); err == nil {
					weights[i] = info.Size()
				}
				// A stat failure dispatches at weight zero; captureRef
				// reports the real error at the right position.
			}
			chunked[i] = weights[i] > objectChunkBytes
			if chunked[i] && weights[i] <= maxCaptureRawLen {
				id, err := parseCanonicalCaptureID(refs[i].Hash)
				if _, known := preKnown[id]; err == nil && known {
					// Reverify an existing whole blob through the ordinary path.
					// Re-encoding it as chunks would duplicate retained content.
					chunked[i] = false
				}
			}
			if chunked[i] {
				weights[i] = objectChunkBytes
			}
			gate.acquire(weights[i])
			select {
			case <-stop:
				gate.release(weights[i])
				return
			case <-ctx.Done():
				gate.release(weights[i])
				return
			case tokens <- struct{}{}:
			}
			select {
			case <-stop:
				gate.release(weights[i])
				return
			case <-ctx.Done():
				gate.release(weights[i])
				return
			case work <- i:
			}
		}
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range work {
				if refs[i].Size < -1 || refs[i].Size > MaxObjectBytes {
					results <- captureResult{index: i, err: fmt.Errorf("backup: invalid attachment size %d", refs[i].Size)}
				} else if chunked[i] {
					// The collector streams chunks directly into the appender,
					// keeping scratch independent of the logical file's size.
					// Small files continue through this same worker pool.
					// ponytail: chunked capture stays serial; prepare chunks in
					// workers if large-file throughput requires it.
					results <- captureResult{index: i, chunked: true}
				} else if opts.Source != nil {
					results <- captureRefFromSource(ctx, opts.Source, refs[i], i, preKnown, level, streaming, scratchDir)
				} else {
					results <- captureRef(ctx, root, refs[i], i, preKnown, level, streaming, scratchDir)
				}
			}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := map[int]captureResult{}
	next := 0
	var firstErr error
	for res := range results {
		if firstErr != nil {
			if res.prepared != nil {
				_ = res.prepared.Close()
			}
			gate.release(weights[res.index])
			continue // draining after failure
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			close(stop)
			gate.stop()
			if res.prepared != nil {
				_ = res.prepared.Close()
			}
			gate.release(weights[res.index])
			continue
		}
		pending[res.index] = res
		for firstErr == nil {
			c, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			<-tokens
			if c.err != nil {
				firstErr = c.err
			} else if c.chunked {
				firstErr = captureLargeAttachment(ctx, root, refs, c.index, parentSeen, appender, opts, out)
			} else {
				firstErr = recordCapture(ctx, c, refs, parentSeen, appender, opts, out)
			}
			gate.release(weights[c.index])
			if firstErr != nil {
				close(stop)
				gate.stop()
			}
		}
	}
	for _, res := range pending {
		if res.prepared != nil {
			_ = res.prepared.Close()
		}
	}
	if firstErr == nil {
		// The dispatcher exits silently on ctx.Done; without this check an
		// early cancellation could report a partial capture as success.
		firstErr = ctx.Err()
	}
	return firstErr
}

// captureRelPath resolves ref's location relative to the attachments
// directory: the database-recorded storage path when one is set (rejecting
// traversal and absolute paths — they come from DB rows and must never read
// outside the attachments directory), the canonical loose "<aa>/<hash>"
// derivation otherwise.
func captureRelPath(ref ContentRef) (string, error) {
	if ref.StoragePath == "" {
		// The loose layout keys on the first two hash characters, so a
		// too-short hash from a corrupt DB row would otherwise panic on the
		// slice below.
		if len(ref.Hash) < 2 {
			return "", fmt.Errorf("backup: attachment content hash %q is too short for the loose store layout", ref.Hash)
		}
		return filepath.Join(ref.Hash[:2], ref.Hash), nil
	}
	rel := filepath.FromSlash(ref.StoragePath)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("backup: attachment %s storage path %q escapes the attachments directory", ref.Hash, ref.StoragePath)
	}
	return rel, nil
}

// captureRef reads, hash-verifies, and (for blobs the repository does not
// already hold) trial-compresses one attachment file. Runs on a worker. Reads
// go through root, confined to the attachments directory, so a storage path
// resolving through a symlink outside the tree is refused rather than read.
func captureRef(
	ctx context.Context, root *os.Root, ref ContentRef, index int,
	preKnown map[pack.BlobID]struct{}, level int, streaming bool, scratchDir string,
) captureResult {
	// Failing validation here (not in an upfront sweep) keeps error reporting
	// in strict ref order: the collector surfaces whichever failure a serial
	// capture would have hit first, whatever its kind.
	rel, err := captureRelPath(ref)
	if err != nil {
		return captureResult{index: index, err: err}
	}
	if streaming {
		return prepareCaptureFile(ctx, root, rel, ref, index, preKnown, level, scratchDir)
	}
	content, _, err := readRegularFile(root, rel)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != ref.Hash {
		return captureResult{
			index: index,
			err:   fmt.Errorf("backup: attachment %s content does not match its hash (live store corruption)", rel),
		}
	}
	res := captureResult{index: index, size: int64(len(content)), id: sum}
	if _, ok := preKnown[res.id]; ok {
		res.known = true
		return res
	}
	res.frame, res.compressed = pack.EncodeFrame(content, level)
	return res
}

// captureRefFromSource is captureRef for an application-supplied source:
// same hash verification, size cap, known-blob skip, and trial compression;
// only the byte acquisition differs.
func captureRefFromSource(
	ctx context.Context, source ContentSource, ref ContentRef, index int,
	preKnown map[pack.BlobID]struct{}, level int, streaming bool, scratchDir string,
) captureResult {
	if streaming {
		return prepareCaptureSource(ctx, source, ref, index, preKnown, level, scratchDir)
	}
	content, err := readSourceBlob(ctx, source, ref)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s from content source: %w", ref.Hash, err)}
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != ref.Hash {
		return captureResult{
			index: index,
			err:   fmt.Errorf("backup: attachment %s content does not match its hash (live store corruption)", ref.Hash),
		}
	}
	res := captureResult{index: index, size: int64(len(content)), id: sum}
	if _, ok := preKnown[res.id]; ok {
		res.known = true
		return res
	}
	res.frame, res.compressed = pack.EncodeFrame(content, level)
	return res
}

func prepareCaptureFile(
	ctx context.Context, root *os.Root, rel string, ref ContentRef, index int,
	preKnown map[pack.BlobID]struct{}, level int, scratchDir string,
) captureResult {
	pre, err := root.Stat(rel)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	if !pre.Mode().IsRegular() {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %q is not a regular file", rel, rel)}
	}
	if err := validateCaptureFileSize(rel, pre.Size()); err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	f, err := root.Open(rel)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	info, statErr := f.Stat()
	if statErr != nil || !os.SameFile(pre, info) {
		return captureResult{index: index, err: errors.Join(
			fmt.Errorf("backup: reading attachment %s: file changed during capture", rel), statErr, f.Close())}
	}
	if err := validateCaptureFileSize(rel, info.Size()); err != nil {
		return captureResult{index: index, err: errors.Join(
			fmt.Errorf("backup: reading attachment %s: %w", rel, err), f.Close())}
	}
	id, err := parseCanonicalCaptureID(ref.Hash)
	if err != nil {
		return captureResult{index: index, err: errors.Join(err, f.Close())}
	}
	if _, known := preKnown[id]; known {
		verifyErr := verifyCaptureReader(ctx, f, uint64(info.Size()), id)
		after, afterErr := f.Stat()
		closeErr := f.Close()
		if verifyErr == nil && (afterErr != nil || !os.SameFile(info, after) || after.Size() != info.Size()) {
			verifyErr = errors.Join(afterErr, errors.New("file changed during capture"))
		}
		return captureResult{
			index: index, size: info.Size(), id: id, known: true,
			err: errors.Join(verifyErr, closeErr),
		}
	}
	prepared, prepareErr := pack.PrepareBlob(ctx, f, uint64(info.Size()), level, pack.AppendStreamOptions{
		ExpectedID: &id, ScratchDir: scratchDir,
	})
	after, afterErr := f.Stat()
	closeErr := f.Close()
	if prepareErr == nil && (afterErr != nil || !os.SameFile(info, after) || after.Size() != info.Size()) {
		prepareErr = errors.Join(afterErr, errors.New("file changed during capture"))
	}
	if err := errors.Join(prepareErr, closeErr); err != nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	return captureResult{index: index, size: info.Size(), id: id, prepared: prepared}
}

func prepareCaptureSource(
	ctx context.Context, source ContentSource, ref ContentRef, index int,
	preKnown map[pack.BlobID]struct{}, level int, scratchDir string,
) captureResult {
	rc, err := source.Open(ctx, ref)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s from content source: %w", ref.Hash, err)}
	}
	tmp, err := os.CreateTemp(scratchDir, "backup-source-*")
	if err != nil {
		_ = rc.Close()
		return captureResult{index: index, err: fmt.Errorf("backup: staging attachment %s from content source: %w", ref.Hash, err)}
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	digest := sha256.New()
	reader := io.LimitReader(&contextio.Reader{Context: ctx, Reader: rc}, maxCaptureRawLen+1)
	size, copyErr := io.CopyBuffer(io.MultiWriter(tmp, digest), reader, make([]byte, 64<<10))
	closeErr := rc.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s from content source: %w", ref.Hash, err)}
	}
	if size > maxCaptureRawLen {
		return captureResult{index: index, err: fmt.Errorf("backup: attachment %q is larger than the maximum blob size %d", ref.Hash, maxCaptureRawLen)}
	}
	id, err := parseCanonicalCaptureID(ref.Hash)
	if err != nil {
		return captureResult{index: index, err: err}
	}
	var got pack.BlobID
	copy(got[:], digest.Sum(nil))
	if got != id {
		return captureResult{index: index, err: fmt.Errorf("backup: attachment %s content does not match its hash (live store corruption)", ref.Hash)}
	}
	if _, known := preKnown[id]; known {
		return captureResult{index: index, size: size, id: id, known: true}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return captureResult{index: index, err: err}
	}
	prepared, err := pack.PrepareBlob(ctx, tmp, uint64(size), level, pack.AppendStreamOptions{
		ExpectedID: &id, ScratchDir: scratchDir,
	})
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: preparing attachment %s from content source: %w", ref.Hash, err)}
	}
	return captureResult{index: index, size: size, id: id, prepared: prepared}
}

func validateCaptureFileSize(rel string, size int64) error {
	if size < 0 {
		return fmt.Errorf("%q has invalid size %d", rel, size)
	}
	if size > maxCaptureRawLen {
		return fmt.Errorf("%q is %d bytes, larger than the maximum blob size %d",
			rel, size, maxCaptureRawLen)
	}
	return nil
}

func parseCanonicalCaptureID(hash string) (pack.BlobID, error) {
	id, err := pack.ParseBlobID(hash)
	if err != nil {
		return pack.BlobID{}, err
	}
	if id.String() != hash {
		return pack.BlobID{}, fmt.Errorf("backup: attachment hash %q is not canonical lowercase hex", hash)
	}
	return id, nil
}

func verifyCaptureReader(ctx context.Context, reader io.Reader, size uint64, id pack.BlobID) error {
	digest := sha256.New()
	written, err := io.CopyBuffer(digest, io.LimitReader(&contextio.Reader{Context: ctx, Reader: reader}, int64(size)+1), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if uint64(written) != size {
		return errors.New("content size changed during capture")
	}
	var got pack.BlobID
	copy(got[:], digest.Sum(nil))
	if got != id {
		return errors.New("content does not match its hash (live store corruption)")
	}
	return nil
}

// readSourceBlob reads one blob from source under the same cap
// readRegularFile enforces for directory reads.
func readSourceBlob(ctx context.Context, source ContentSource, ref ContentRef) ([]byte, error) {
	rc, err := source.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxCaptureRawLen+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxCaptureRawLen {
		return nil, fmt.Errorf("%q is larger than the maximum blob size %d", ref.Hash, maxCaptureRawLen)
	}
	return data, nil
}

// maxCaptureRawLen is capture's pack-format raw-size ceiling. Plain capture
// enforces it while streaming; encrypted-v1 compatibility still buffers a
// whole authenticated frame. It is a var, not a const, only so tests can
// lower it.
var maxCaptureRawLen int64 = pack.MaxRawLen

// readRegularFile reads rel through root and requires it to be a regular file,
// returning its content and stat info. The regular-file check runs BEFORE the
// open: opening a fifo blocks until a writer appears and opening a device
// node can have side effects, so a non-regular file a tampered DB row points
// at must be rejected without ever being opened. Stat (not lstat) is used so
// an in-root symlink to a regular file stays capturable — attachment bytes
// are hash-verified afterward, unlike extras. SameFile then ties the checked
// file to the opened descriptor, so the returned bytes describe the file the
// check approved; os.Root refuses any path that escapes the root through a
// symlink. A fifo raced in between the stat and the open can still block the
// open — closing that needs a platform-specific nonblocking open, and the
// extras leaf read accepts the same residual.
func readRegularFile(root *os.Root, rel string) ([]byte, os.FileInfo, error) {
	pre, err := root.Stat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !pre.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q is not a regular file", rel)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(pre, info) {
		return nil, nil, fmt.Errorf("%q changed during capture", rel)
	}
	if info.Size() > maxCaptureRawLen {
		return nil, nil, fmt.Errorf("%q is %d bytes, larger than the maximum blob size %d",
			rel, info.Size(), maxCaptureRawLen)
	}
	// The stat bound is advisory (the file can grow between fstat and read);
	// the limited read is the guarantee the buffer cannot exceed the cap.
	data, err := io.ReadAll(io.LimitReader(f, maxCaptureRawLen+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxCaptureRawLen {
		return nil, nil, fmt.Errorf("%q grew past the maximum blob size %d during capture",
			rel, maxCaptureRawLen)
	}
	return data, info, nil
}

// recordCapture applies one worker result in ref order: append the frame
// (unless the blob was already stored), fill the ref's size, and update
// accounting, the new-list segment, and progress. Runs on the collector.
func recordCapture(
	ctx context.Context, c captureResult, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions, out *AttachmentCapture,
) error {
	ref := &refs[c.index]
	ref.Size = c.size
	if !c.known {
		if c.prepared != nil {
			if _, err := appender.AddPrepared(ctx, c.prepared); err != nil {
				return err
			}
		} else {
			if _, err := appender.AddEncoded(c.id, c.frame, uint64(c.size), c.compressed); err != nil {
				return err
			}
		}
	}
	out.Blobs++
	out.BlobBytes += c.size
	if !parentSeen[ref.Hash] {
		out.NewList = append(out.NewList, *ref)
	}
	if opts.Progress != nil {
		opts.Progress(c.index+1, len(refs), out.BlobBytes)
	}
	return nil
}

// LoadListRefs fetches and decodes a manifest's attachment list blobs. ext is
// the pack file extension (App.PackFileExtension).
func LoadListRefs(r *Repo, known map[pack.BlobID]IndexEntry, listBlobIDs []string, crypter *pack.Crypter, ext string) ([]ContentRef, map[string]bool, error) {
	var refs []ContentRef
	seen := map[string]bool{}
	for _, s := range listBlobIDs {
		id, err := pack.ParseBlobID(s)
		if err != nil {
			return nil, nil, fmt.Errorf("backup: attachment list blob id %q: %w", s, err)
		}
		data, err := r.ReadBlob(known, id, crypter, ext)
		if err != nil {
			return nil, nil, err
		}
		segment, err := DecodeAttachmentList(data)
		if err != nil {
			return nil, nil, fmt.Errorf("backup: attachment list %s: %w", s, err)
		}
		for _, ref := range segment {
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	return refs, seen, nil
}
