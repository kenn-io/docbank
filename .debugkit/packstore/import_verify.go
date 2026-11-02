package packstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"

	"go.kenn.io/kit/pack"
)

const importVerifyScratchPrefix = ".packstore-verify-"

var (
	// importVerifyIDChunkEntries bounds duplicate-ID and span-validation memory.
	// It is a var so tests can force the external merge paths with small real packs.
	importVerifyIDChunkEntries = 4096
	// Limited verification may exceed a target's configured policy so ordinary
	// small-limit fallbacks remain useful, but it never exceeds these conservative
	// fixed ceilings. Tests lower them to exercise the O(1) pre-scan checks.
	importVerifyMaxFooterBytes = uint64(defaultFooterBytes)
	importVerifyMaxEntries     = uint64(defaultPackEntries)
)

// verifyLimitedImportPack independently proves that configured policy limits,
// rather than source damage, caused ordinary preflight to decline a pack. It
// streams the format-bounded footer, spills sorted ID runs under the held target
// root, and retains only caller-selected entries. Eligible selected payloads
// receive the same bounded CRC/decode/hash verification as ordinary maintenance
// reads; oversized selections remain unread so they can safely restore loose.
func verifyLimitedImportPack(
	ctx context.Context,
	target *os.Root,
	candidate ImportPack,
	limits Limits,
) (resultErr error) {
	pathInfo, err := snapshotBoundedPackPathIdentity(candidate.SourcePath)
	if err != nil {
		return fmt.Errorf("inspect pack for import verification: %w", err)
	}
	if err := validateRegularNoFollow(candidate.SourcePath, pathInfo); err != nil {
		return fmt.Errorf("validate pack for import verification: %w", err)
	}
	f, err := openNoFollow(candidate.SourcePath, false)
	if err != nil {
		return fmt.Errorf("open pack for import verification: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat pack for import verification: %w", err)
	}
	if !os.SameFile(pathInfo, info) {
		return errors.New("packstore: pack changed identity during import verification")
	}
	size := info.Size()
	if size < plainPackHeaderSize+plainPackTrailerSize {
		return fmt.Errorf("%w: %d bytes is too small for a plain pack", pack.ErrTruncated, size)
	}

	var header [plainPackHeaderSize]byte
	if err := readBoundedPackAt(f, header[:], 0, "header"); err != nil {
		return err
	}
	if !bytes.Equal(header[:4], []byte("MVPK")) {
		return fmt.Errorf("%w: header", pack.ErrBadMagic)
	}
	if header[4] != plainPackVersion {
		return fmt.Errorf("%w: version %d", pack.ErrUnsupportedVersion, header[4])
	}
	if header[5] != 0 {
		return fmt.Errorf("%w: import verification requires plain v1 flags, got %#x", pack.ErrCorrupt, header[5])
	}

	var trailer [plainPackTrailerSize]byte
	if err := readBoundedPackAt(f, trailer[:], size-plainPackTrailerSize, "trailer"); err != nil {
		return err
	}
	if !bytes.Equal(trailer[36:], []byte("KPVM")) {
		return fmt.Errorf("%w: trailer", pack.ErrBadMagic)
	}
	footerLen := uint64(binary.LittleEndian.Uint32(trailer[:4]))
	if footerLen > pack.MaxFooterLen {
		return fmt.Errorf("%w: footer length %d exceeds format maximum %d", pack.ErrCorrupt, footerLen, uint64(pack.MaxFooterLen))
	}
	fileSize := uint64(size)
	if footerLen < 4 || fileSize < plainPackHeaderSize+plainPackTrailerSize+footerLen {
		return fmt.Errorf("%w: footer length %d is outside %d-byte pack", pack.ErrTruncated, footerLen, size)
	}
	if footerLen > importVerifyMaxFooterBytes {
		return newLimitError(LimitPackFooterBytes, footerLen, importVerifyMaxFooterBytes)
	}
	footerStart := fileSize - plainPackTrailerSize - footerLen

	digest := sha256.New()
	footer := bufio.NewReaderSize(io.TeeReader(
		io.NewSectionReader(f, int64(footerStart), int64(footerLen)),
		digest,
	), 64<<10)
	var countBytes [4]byte
	if _, err := io.ReadFull(footer, countBytes[:]); err != nil {
		return fmt.Errorf("%w: read footer count: %w", pack.ErrTruncated, err)
	}
	count := uint64(binary.LittleEndian.Uint32(countBytes[:]))
	wantFooterLen := uint64(4) + count*plainPackEntrySize
	if footerLen != wantFooterLen {
		return fmt.Errorf("%w: footer length %d, want %d for %d entries", pack.ErrCorrupt, footerLen, wantFooterLen, count)
	}
	if count > importVerifyMaxEntries {
		return newLimitError(LimitPackEntryCount, count, importVerifyMaxEntries)
	}

	selected, err := selectedImportIDs(candidate)
	if err != nil {
		return err
	}
	found := make(map[pack.BlobID]pack.Entry, len(selected))
	runs := newImportIDRuns(ctx, target)
	spanRuns := newImportSpanRuns(ctx, target)
	defer func() {
		if cleanupErr := errors.Join(runs.cleanup(), spanRuns.cleanup()); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("clean import verification scratch: %w", cleanupErr))
		}
	}()

	chunkLimit := max(importVerifyIDChunkEntries, 1)
	ids := make([]pack.BlobID, 0, chunkLimit)
	spans := make([]importSpan, 0, chunkLimit)
	for i := range count {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var encoded [plainPackEntrySize]byte
		if _, err := io.ReadFull(footer, encoded[:]); err != nil {
			return fmt.Errorf("%w: read footer entry %d: %w", pack.ErrTruncated, i, err)
		}
		entry := decodeImportFooterEntry(encoded)
		if err := validateImportFooterEntry(entry, footerStart, i); err != nil {
			return err
		}
		ids = append(ids, entry.ID)
		if entry.StoredLen > 0 {
			spans = append(spans, importSpan{offset: entry.Offset, end: entry.Offset + entry.StoredLen})
			if len(spans) == chunkLimit {
				if err := spanRuns.add(spans); err != nil {
					return err
				}
				spans = make([]importSpan, 0, chunkLimit)
			}
		}
		if len(ids) == chunkLimit && i+1 < count {
			if err := runs.add(ids); err != nil {
				return err
			}
			ids = make([]pack.BlobID, 0, chunkLimit)
		}
		if _, ok := selected[entry.ID]; ok {
			found[entry.ID] = entry
		}
	}
	if err := runs.finish(ids); err != nil {
		return err
	}
	if err := spanRuns.finish(spans); err != nil {
		return err
	}
	_, _ = digest.Write(trailer[:4])
	if !bytes.Equal(digest.Sum(nil), trailer[4:36]) {
		return pack.ErrChecksum
	}
	if err := validateSelectedImportEntries(candidate, selected, found); err != nil {
		return err
	}

	reader := &boundedPackReader{file: f, entries: found}
	for _, selection := range candidate.Selections {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := found[selectedID(selection)]
		if entry.RawLen > uint64(limits.BlobBytes) || entry.StoredLen > uint64(limits.BlobBytes) {
			continue
		}
		if _, err := reader.readBlob(entry, limits.BlobBytes); err != nil {
			return fmt.Errorf("verify selected blob %s in limited pack %s: %w", selection.Hash, candidate.PackID, err)
		}
	}
	return nil
}

func selectedImportIDs(candidate ImportPack) (map[pack.BlobID]ImportSelection, error) {
	selected := make(map[pack.BlobID]ImportSelection, len(candidate.Selections))
	for _, selection := range candidate.Selections {
		id, err := pack.ParseBlobID(selection.Hash.String())
		if err != nil {
			return nil, err
		}
		if _, duplicate := selected[id]; duplicate {
			return nil, fmt.Errorf("%w %s in import pack %s", ErrDuplicateHash, selection.Hash, candidate.PackID)
		}
		selected[id] = selection
	}
	return selected, nil
}

func selectedID(selection ImportSelection) pack.BlobID {
	id, _ := pack.ParseBlobID(selection.Hash.String())
	return id
}

func decodeImportFooterEntry(encoded [plainPackEntrySize]byte) pack.Entry {
	var entry pack.Entry
	copy(entry.ID[:], encoded[:32])
	entry.Offset = binary.LittleEndian.Uint64(encoded[32:])
	entry.StoredLen = binary.LittleEndian.Uint64(encoded[40:])
	entry.RawLen = binary.LittleEndian.Uint64(encoded[48:])
	entry.Flags = pack.BlobFlags(encoded[56])
	entry.CRC32C = binary.LittleEndian.Uint32(encoded[57:])
	return entry
}

func validateImportFooterEntry(entry pack.Entry, footerStart, index uint64) error {
	if entry.RawLen > pack.MaxRawLen {
		return fmt.Errorf("%w: entry %d raw length %d exceeds format maximum", pack.ErrCorrupt, index, entry.RawLen)
	}
	if entry.StoredLen > pack.MaxStoredLen {
		return fmt.Errorf("%w: entry %d stored length %d exceeds format maximum", pack.ErrCorrupt, index, entry.StoredLen)
	}
	if entry.Flags&^pack.BlobCompressed != 0 {
		return fmt.Errorf("%w: entry %d has unsupported flags %#x", pack.ErrCorrupt, index, entry.Flags)
	}
	end := entry.Offset + entry.StoredLen
	if entry.Offset < plainPackHeaderSize || end < entry.Offset || end > footerStart {
		return fmt.Errorf("%w: entry %d span is outside data region", pack.ErrCorrupt, index)
	}
	return nil
}

func validateSelectedImportEntries(
	candidate ImportPack,
	selected map[pack.BlobID]ImportSelection,
	found map[pack.BlobID]pack.Entry,
) error {
	for id, selection := range selected {
		entry, ok := found[id]
		if !ok {
			return fmt.Errorf("%w: selected blob %s is absent from pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
		if selection.RawLen != int64(entry.RawLen) || selection.Offset != entry.Offset ||
			selection.StoredLen != entry.StoredLen || selection.Flags != uint8(entry.Flags) {
			return fmt.Errorf("%w: selected metadata for %s does not match pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
	}
	return nil
}

type importIDRuns struct {
	ctx     context.Context
	root    *os.Root
	dir     string
	created bool
	next    int
	runs    []string
}

func newImportIDRuns(ctx context.Context, root *os.Root) *importIDRuns {
	return &importIDRuns{ctx: ctx, root: root, dir: importVerifyScratchPrefix + pack.NewPackID()}
}

func (r *importIDRuns) add(ids []pack.BlobID) error {
	sortBlobIDs(ids)
	if err := rejectAdjacentBlobIDs(ids); err != nil {
		return err
	}
	if !r.created {
		if err := r.root.Mkdir(r.dir, 0o700); err != nil {
			return fmt.Errorf("create import verification scratch: %w", err)
		}
		r.created = true
	}
	name := path.Join(r.dir, fmt.Sprintf("run-%06d", r.next))
	r.next++
	if err := writeImportIDRun(r.root, name, ids); err != nil {
		return err
	}
	r.runs = append(r.runs, name)
	return nil
}

func (r *importIDRuns) finish(ids []pack.BlobID) error {
	if len(r.runs) == 0 {
		sortBlobIDs(ids)
		return rejectAdjacentBlobIDs(ids)
	}
	if len(ids) > 0 {
		if err := r.add(ids); err != nil {
			return err
		}
	}
	for len(r.runs) > 1 {
		nextRuns := make([]string, 0, (len(r.runs)+1)/2)
		for i := 0; i < len(r.runs); i += 2 {
			if i+1 == len(r.runs) {
				nextRuns = append(nextRuns, r.runs[i])
				continue
			}
			merged := path.Join(r.dir, fmt.Sprintf("merge-%06d", r.next))
			r.next++
			if err := mergeImportIDRuns(r.ctx, r.root, r.runs[i], r.runs[i+1], merged); err != nil {
				return err
			}
			if err := r.root.Remove(r.runs[i]); err != nil {
				return fmt.Errorf("remove merged import ID run: %w", err)
			}
			if err := r.root.Remove(r.runs[i+1]); err != nil {
				return fmt.Errorf("remove merged import ID run: %w", err)
			}
			nextRuns = append(nextRuns, merged)
		}
		r.runs = nextRuns
	}
	return nil
}

func (r *importIDRuns) cleanup() error {
	if !r.created {
		return nil
	}
	return r.root.RemoveAll(r.dir)
}

func sortBlobIDs(ids []pack.BlobID) {
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
}

func rejectAdjacentBlobIDs(ids []pack.BlobID) error {
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, ids[i])
		}
	}
	return nil
}

func writeImportIDRun(root *os.Root, name string, ids []pack.BlobID) (resultErr error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create import ID run: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	w := bufio.NewWriter(f)
	for _, id := range ids {
		if _, err := w.Write(id[:]); err != nil {
			return fmt.Errorf("write import ID run: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush import ID run: %w", err)
	}
	return nil
}

func mergeImportIDRuns(
	ctx context.Context,
	root *os.Root,
	leftName, rightName, outputName string,
) (resultErr error) {
	left, err := root.Open(leftName)
	if err != nil {
		return fmt.Errorf("open left import ID run: %w", err)
	}
	right, err := root.Open(rightName)
	if err != nil {
		return errors.Join(fmt.Errorf("open right import ID run: %w", err), left.Close())
	}
	output, err := root.OpenFile(outputName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(fmt.Errorf("create merged import ID run: %w", err), left.Close(), right.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, left.Close(), right.Close(), output.Close()) }()

	lr, rr := bufio.NewReader(left), bufio.NewReader(right)
	w := bufio.NewWriter(output)
	lid, lok, err := readImportID(lr)
	if err != nil {
		return err
	}
	rid, rok, err := readImportID(rr)
	if err != nil {
		return err
	}
	for merged := 0; lok || rok; merged++ {
		if merged%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var next pack.BlobID
		switch {
		case !rok || (lok && bytes.Compare(lid[:], rid[:]) < 0):
			next = lid
			lid, lok, err = readImportID(lr)
		case !lok || bytes.Compare(rid[:], lid[:]) < 0:
			next = rid
			rid, rok, err = readImportID(rr)
		default:
			return fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, lid)
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(next[:]); err != nil {
			return fmt.Errorf("write merged import ID run: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush merged import ID run: %w", err)
	}
	return nil
}

func readImportID(r *bufio.Reader) (pack.BlobID, bool, error) {
	var id pack.BlobID
	_, err := io.ReadFull(r, id[:])
	if errors.Is(err, io.EOF) {
		return pack.BlobID{}, false, nil
	}
	if err != nil {
		return pack.BlobID{}, false, fmt.Errorf("%w: truncated import ID run: %w", pack.ErrCorrupt, err)
	}
	return id, true, nil
}

type importSpan struct {
	offset uint64
	end    uint64
}

type importSpanRuns struct {
	ctx     context.Context
	root    *os.Root
	dir     string
	created bool
	next    int
	runs    []string
}

func newImportSpanRuns(ctx context.Context, root *os.Root) *importSpanRuns {
	return &importSpanRuns{ctx: ctx, root: root, dir: importVerifyScratchPrefix + pack.NewPackID()}
}

func (r *importSpanRuns) add(spans []importSpan) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	sortImportSpans(spans)
	if err := rejectOverlappingImportSpans(spans); err != nil {
		return err
	}
	if !r.created {
		if err := r.root.Mkdir(r.dir, 0o700); err != nil {
			return fmt.Errorf("create import verification span scratch: %w", err)
		}
		r.created = true
	}
	name := path.Join(r.dir, fmt.Sprintf("run-%06d", r.next))
	r.next++
	if err := writeImportSpanRun(r.root, name, spans); err != nil {
		return err
	}
	r.runs = append(r.runs, name)
	return nil
}

func (r *importSpanRuns) finish(spans []importSpan) error {
	if len(r.runs) == 0 {
		sortImportSpans(spans)
		return rejectOverlappingImportSpans(spans)
	}
	if len(spans) > 0 {
		if err := r.add(spans); err != nil {
			return err
		}
	}
	for len(r.runs) > 1 {
		nextRuns := make([]string, 0, (len(r.runs)+1)/2)
		for i := 0; i < len(r.runs); i += 2 {
			if i+1 == len(r.runs) {
				nextRuns = append(nextRuns, r.runs[i])
				continue
			}
			merged := path.Join(r.dir, fmt.Sprintf("merge-%06d", r.next))
			r.next++
			if err := mergeImportSpanRuns(r.ctx, r.root, r.runs[i], r.runs[i+1], merged); err != nil {
				return err
			}
			if err := r.root.Remove(r.runs[i]); err != nil {
				return fmt.Errorf("remove merged import span run: %w", err)
			}
			if err := r.root.Remove(r.runs[i+1]); err != nil {
				return fmt.Errorf("remove merged import span run: %w", err)
			}
			nextRuns = append(nextRuns, merged)
		}
		r.runs = nextRuns
	}
	return nil
}

func (r *importSpanRuns) cleanup() error {
	if !r.created {
		return nil
	}
	return r.root.RemoveAll(r.dir)
}

func sortImportSpans(spans []importSpan) {
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].offset < spans[j].offset ||
			(spans[i].offset == spans[j].offset && spans[i].end < spans[j].end)
	})
}

func rejectOverlappingImportSpans(spans []importSpan) error {
	var greatestEnd uint64
	for i, span := range spans {
		if i > 0 && span.offset < greatestEnd {
			return fmt.Errorf("%w: overlapping entry spans", pack.ErrCorrupt)
		}
		greatestEnd = max(greatestEnd, span.end)
	}
	return nil
}

func writeImportSpanRun(root *os.Root, name string, spans []importSpan) (resultErr error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create import span run: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	w := bufio.NewWriter(f)
	var encoded [16]byte
	for _, span := range spans {
		binary.LittleEndian.PutUint64(encoded[:8], span.offset)
		binary.LittleEndian.PutUint64(encoded[8:], span.end)
		if _, err := w.Write(encoded[:]); err != nil {
			return fmt.Errorf("write import span run: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush import span run: %w", err)
	}
	return nil
}

func mergeImportSpanRuns(
	ctx context.Context,
	root *os.Root,
	leftName, rightName, outputName string,
) (resultErr error) {
	left, err := root.Open(leftName)
	if err != nil {
		return fmt.Errorf("open left import span run: %w", err)
	}
	right, err := root.Open(rightName)
	if err != nil {
		return errors.Join(fmt.Errorf("open right import span run: %w", err), left.Close())
	}
	output, err := root.OpenFile(outputName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(fmt.Errorf("create merged import span run: %w", err), left.Close(), right.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, left.Close(), right.Close(), output.Close()) }()

	lr, rr := bufio.NewReader(left), bufio.NewReader(right)
	w := bufio.NewWriter(output)
	leftSpan, leftOK, err := readImportSpan(lr)
	if err != nil {
		return err
	}
	rightSpan, rightOK, err := readImportSpan(rr)
	if err != nil {
		return err
	}
	var greatestEnd uint64
	var havePrior bool
	var encoded [16]byte
	for merged := 0; leftOK || rightOK; merged++ {
		if merged%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var next importSpan
		if !rightOK || (leftOK && importSpanLess(leftSpan, rightSpan)) {
			next = leftSpan
			leftSpan, leftOK, err = readImportSpan(lr)
		} else {
			next = rightSpan
			rightSpan, rightOK, err = readImportSpan(rr)
		}
		if err != nil {
			return err
		}
		if havePrior && next.offset < greatestEnd {
			return fmt.Errorf("%w: overlapping entry spans", pack.ErrCorrupt)
		}
		havePrior = true
		greatestEnd = max(greatestEnd, next.end)
		binary.LittleEndian.PutUint64(encoded[:8], next.offset)
		binary.LittleEndian.PutUint64(encoded[8:], next.end)
		if _, err := w.Write(encoded[:]); err != nil {
			return fmt.Errorf("write merged import span run: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush merged import span run: %w", err)
	}
	return nil
}

func importSpanLess(left, right importSpan) bool {
	return left.offset < right.offset || (left.offset == right.offset && left.end < right.end)
}

func readImportSpan(r *bufio.Reader) (importSpan, bool, error) {
	var encoded [16]byte
	_, err := io.ReadFull(r, encoded[:])
	if errors.Is(err, io.EOF) {
		return importSpan{}, false, nil
	}
	if err != nil {
		return importSpan{}, false, fmt.Errorf("%w: truncated import span run: %w", pack.ErrCorrupt, err)
	}
	return importSpan{
		offset: binary.LittleEndian.Uint64(encoded[:8]),
		end:    binary.LittleEndian.Uint64(encoded[8:]),
	}, true, nil
}
