package pack

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// osLink is the hard-link primitive publishNoClobber uses. It is a var so
// tests can simulate the filesystems (exFAT, FAT32, many SMB/NFS mounts)
// where os.Link is unavailable and the check-then-rename fallback must take
// over.
var osLink = os.Link

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// WriterOptions configures a pack Writer. Zero values select the defaults.
type WriterOptions struct {
	// TargetSize is the advisory size at which Full() reports true.
	TargetSize int64
	// ZstdLevel is the trial-compression level (default 3).
	ZstdLevel int
	// Crypter, when non-nil, produces an encrypted pack.
	Crypter *Crypter
}

// Writer builds one pack file in a staging directory and moves it to its
// final path on Seal, following the durability discipline (backup/FORMAT.md, Crash Consistency):
// write -> fsync file -> rename -> fsync directory.
type Writer struct {
	id      string
	f       *os.File
	staging string
	opts    WriterOptions
	off     int64
	entries []Entry
	done    bool
	err     error
}

// NewWriter creates the staging file and writes the pack header.
func NewWriter(stagingDir string, opts WriterOptions) (*Writer, error) {
	if opts.TargetSize <= 0 {
		opts.TargetSize = DefaultTargetSize
	}
	id := NewPackID()
	staging := filepath.Join(stagingDir, id+".staging")
	f, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600)
	if err != nil {
		return nil, fmt.Errorf("pack: creating staging file: %w", err)
	}
	var flags packFlags
	if opts.Crypter != nil {
		flags |= packEncrypted
	}
	header := append([]byte(headerMagic), FormatVersion, byte(flags))
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		_ = os.Remove(staging)
		return nil, fmt.Errorf("pack: writing header: %w", err)
	}
	return &Writer{id: id, f: f, staging: staging, opts: opts, off: headerSize},
		nil
}

// ID returns the pack's ULID.
func (w *Writer) ID() string { return w.id }

// StoredSize returns the bytes written so far (header plus stored frames).
func (w *Writer) StoredSize() int64 { return w.off }

// Full reports whether the pack has reached its advisory target size.
func (w *Writer) Full() bool { return w.off >= w.opts.TargetSize }

// Append stores one blob and returns its entry. Dedup is the caller's job.
// raw must be at most MaxRawLen bytes: a compressed frame recording a larger
// raw length could never be read back, since decodeFrame rejects it to match
// the zstd decoder's memory limit, so Append rejects the input up front.
//
// Once a write to the staging file fails, the Writer is poisoned: the
// underlying file descriptor's position may have advanced past w.off (for
// example on a partial write), so every following Append or Seal would record
// or publish offsets that no longer match the file. Append and Seal both
// return the first such error immediately without writing again; Abort still
// works to discard the poisoned staging file.
func (w *Writer) Append(raw []byte) (Entry, error) {
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if uint64(len(raw)) > MaxRawLen {
		return Entry{}, fmt.Errorf("pack: raw blob length %d exceeds max %d bytes",
			len(raw), uint64(MaxRawLen))
	}
	id := ComputeBlobID(raw)
	stored, compressed := encodeFrame(raw, w.opts.ZstdLevel)
	return w.appendFrame(id, stored, uint64(len(raw)), compressed)
}

// AppendEncoded stores one blob whose frame the caller already produced with
// EncodeFrame, so trial compression can run concurrently outside the Writer.
// The caller contract is strict: id must equal ComputeBlobID of the original
// raw bytes, and (frame, compressed) must be EncodeFrame's output for those
// same bytes — a violated contract writes a frame readers will reject at
// decode time. The cheap invariants (raw length bound, an uncompressed
// frame's length matching rawLen) are still checked here.
func (w *Writer) AppendEncoded(id BlobID, frame []byte, rawLen uint64, compressed bool) (Entry, error) {
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if rawLen > MaxRawLen {
		return Entry{}, fmt.Errorf("pack: raw blob length %d exceeds max %d bytes",
			rawLen, uint64(MaxRawLen))
	}
	if !compressed && uint64(len(frame)) != rawLen {
		return Entry{}, fmt.Errorf("pack: uncompressed frame for blob %s is %d bytes but claims raw length %d",
			id, len(frame), rawLen)
	}
	return w.appendFrame(id, frame, rawLen, compressed)
}

// AppendStream reads exactly rawLen bytes from src, prepares a bounded-scratch
// plain frame, and appends it. It never closes src. Source and preparation
// failures leave the Writer usable; failures after the pack copy starts poison
// it and require Abort.
func (w *Writer) AppendStream(
	ctx context.Context,
	src io.Reader,
	rawLen uint64,
	opts AppendStreamOptions,
) (Entry, error) {
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if w.opts.Crypter != nil {
		return Entry{}, &UnsupportedStreamError{Feature: StreamEncryptedV1}
	}
	if opts.ScratchDir == "" {
		opts.ScratchDir = filepath.Dir(w.staging)
	}
	prepared, err := PrepareBlob(ctx, src, rawLen, w.opts.ZstdLevel, opts)
	if err != nil {
		return Entry{}, err
	}
	return w.AppendPrepared(ctx, prepared)
}

// AppendPrepared consumes prepared on every return and appends its complete
// plain frame. It is the ordered writer half of concurrent PrepareBlob work.
func (w *Writer) AppendPrepared(
	ctx context.Context,
	prepared *PreparedBlob,
) (entry Entry, resultErr error) {
	if ctx == nil {
		return Entry{}, errors.New("pack: nil context")
	}
	if prepared == nil {
		return Entry{}, errors.New("pack: nil prepared blob")
	}
	f, scratchPath, preparedInfo, err := prepared.take()
	if err != nil {
		return Entry{}, err
	}
	writerMutated := false
	defer func() {
		cleanupErr := closeAndRemoveScratch(f, scratchPath)
		prepared.finish(cleanupErr)
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
			if writerMutated {
				w.err = resultErr
			}
		}
	}()
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if w.opts.Crypter != nil {
		return Entry{}, &UnsupportedStreamError{Feature: StreamEncryptedV1}
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return Entry{}, fmt.Errorf("pack: stat prepared frame: %w", err)
	}
	if !os.SameFile(preparedInfo, info) || info.Size() < 0 || uint64(info.Size()) != prepared.storedLen {
		return Entry{}, errors.New("pack: prepared frame identity or length changed")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Entry{}, fmt.Errorf("pack: rewinding prepared frame: %w", err)
	}

	crc := crc32.New(crc32cTable)
	written, err := copyContext(ctx, io.MultiWriter(w.f, crc), f, prepared.storedLen)
	writerMutated = written != 0
	if err != nil || written != prepared.storedLen {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		resultErr = fmt.Errorf("pack: writing streamed blob %s after %d bytes: %w", prepared.id, written, err)
		if writerMutated {
			w.err = resultErr
		}
		return Entry{}, resultErr
	}
	// A successful zero-length copy still commits a logical entry below, so a
	// later cleanup failure must poison the writer even though no frame bytes
	// were written.
	writerMutated = true
	if crc.Sum32() != prepared.crc {
		w.err = fmt.Errorf("%w: prepared frame crc changed for blob %s", ErrCorrupt, prepared.id)
		return Entry{}, w.err
	}

	var flags BlobFlags
	if prepared.compressed {
		flags |= BlobCompressed
	}
	entry = Entry{
		ID:        prepared.id,
		Offset:    uint64(w.off),
		StoredLen: prepared.storedLen,
		RawLen:    prepared.rawLen,
		Flags:     flags,
		CRC32C:    prepared.crc,
	}
	w.off += int64(prepared.storedLen)
	w.entries = append(w.entries, entry)
	return entry, nil
}

// appendFrame seals (when encrypted), writes, and records one encoded frame.
func (w *Writer) appendFrame(id BlobID, stored []byte, rawLen uint64, compressed bool) (Entry, error) {
	var flags BlobFlags
	if compressed {
		flags |= BlobCompressed
	}
	if w.opts.Crypter != nil {
		sealed, err := w.opts.Crypter.SealBlob(id, stored)
		if err != nil {
			return Entry{}, fmt.Errorf("pack: sealing blob %s: %w", id, err)
		}
		stored = sealed
		flags |= BlobEncrypted
	}
	if _, err := w.f.Write(stored); err != nil {
		w.err = fmt.Errorf("pack: writing blob %s: %w", id, err)
		return Entry{}, w.err
	}
	e := Entry{
		ID:        id,
		Offset:    uint64(w.off),
		StoredLen: uint64(len(stored)),
		RawLen:    rawLen,
		Flags:     flags,
		CRC32C:    crc32.Checksum(stored, crc32cTable),
	}
	w.off += int64(len(stored))
	w.entries = append(w.entries, e)
	return e, nil
}

// Seal writes the footer and trailer, makes the file durable, and renames it
// to finalPath. The Writer is unusable afterward.
//
// Once the rename to finalPath succeeds, the pack is considered published and
// the Writer is marked done even if the following directory-entry sync fails.
// In that case Seal returns an error, but the pack file already exists at
// finalPath: the failure means only that the directory entry for the rename
// may not be durable yet, not that the pack is missing or incomplete. A
// following Abort call is then a no-op that returns nil; it must not remove
// the published pack.
func (w *Writer) Seal(finalPath string) ([]Entry, error) {
	if w.done {
		return nil, ErrSealed
	}
	if w.err != nil {
		return nil, w.err
	}
	region := encodeFooterRegion(w.entries)
	var tail []byte
	if w.opts.Crypter != nil {
		sealed, err := w.opts.Crypter.SealObject("pack-footer", w.id,
			region)
		if err != nil {
			return nil, fmt.Errorf("pack: sealing footer: %w", err)
		}
		tail = appendEncryptedTrailer(sealed, uint64(w.off))
	} else {
		tail = appendPlainTrailer(region)
	}
	if _, err := w.f.Write(tail); err != nil {
		w.err = fmt.Errorf("pack: writing footer: %w", err)
		return nil, w.err
	}
	if err := w.f.Sync(); err != nil {
		return nil, fmt.Errorf("pack: syncing pack file: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return nil, fmt.Errorf("pack: closing pack file: %w", err)
	}
	if err := MkdirAllSynced(filepath.Dir(finalPath)); err != nil {
		return nil, err
	}
	if err := publishNoClobber(w.staging, finalPath); err != nil {
		return nil, fmt.Errorf("pack: publishing pack %s: %w", w.id, err)
	}
	// The pack is published as of this point: mark the writer done before
	// attempting the directory sync so a failure below doesn't leave the
	// Writer looking unsealed. A caller that calls Abort next must find a
	// no-op, not a removal of the file we just published.
	w.done = true
	if err := SyncDir(filepath.Dir(finalPath)); err != nil {
		return nil, fmt.Errorf(
			"pack: pack published at %s but directory sync failed (entry may not be durable): %w",
			finalPath, err)
	}
	return w.entries, nil
}

// publishNoClobber moves staging to finalPath, refusing to replace an
// existing file: sealed packs are immutable and located by ID, so a name
// collision — duplicate-ID debris from a cloning bug, or a file planted at
// the final path — must fail the seal rather than silently destroy a pack
// existing snapshots may reference. A plain os.Rename overwrites on Unix.
// The primary path is a hard link (atomic, EEXIST on collision) followed by
// removing the staging name; where links are unavailable (exFAT, FAT32, many
// SMB/NFS mounts) it falls back to an existence check plus rename — not
// atomic, but the check catches the realistic hazard (a preexisting file),
// and random ULID pack IDs leave no plausible same-name race inside the
// window.
func publishNoClobber(staging, finalPath string) error {
	linkErr := osLink(staging, finalPath)
	if linkErr == nil {
		if err := os.Remove(staging); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing staging name after publish: %w", err)
		}
		return nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		return fmt.Errorf("a pack already exists at %s; refusing to overwrite it", finalPath)
	}
	// Link is unsupported or unreliable on this filesystem: fall back to
	// check-then-rename.
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("a pack already exists at %s; refusing to overwrite it", finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking final path: %w", err)
	}
	return atomicfile.Replace(staging, finalPath)
}

// Abort discards the staging file. Safe to call after a failed Seal.
func (w *Writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.f.Close()
	if err := os.Remove(w.staging); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pack: removing staging file: %w", err)
	}
	return nil
}

// SyncDir fsyncs a directory so a rename into it is durable. It is
// atomicfile.SyncDir, which is a no-op on Windows because Windows cannot
// fsync a directory handle; pack durability there relies on fsyncing the
// pack file before it is published.
//
// SyncDir is a variable ONLY so tests — including this package's consumers'
// tests — can inject fsync failures, which cannot be provoked portably any
// other way. It is a test seam, not a configuration point: production code
// must never reassign it, and it carries no compatibility guarantee beyond
// being callable as a function.
var SyncDir = atomicfile.SyncDir

// MkdirAllSynced creates dir and every missing ancestor, fsyncing each
// created directory so the entries are durable.
func MkdirAllSynced(dir string) error {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("pack: %s exists and is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("pack: stat %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return fmt.Errorf("pack: no existing ancestor found while creating %s", dir)
	}
	if err := MkdirAllSynced(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("pack: creating directory %s: %w", dir, err)
	}
	if err := SyncDir(parent); err != nil {
		return fmt.Errorf("pack: syncing directory %s: %w", parent, err)
	}
	return nil
}
