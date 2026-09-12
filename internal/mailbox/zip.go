package mailbox

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
)

type ArchiveLimits struct {
	EntryBytes, ExpandedBytes int64
	Entries                   int
}

func DefaultArchiveLimits() ArchiveLimits {
	return ArchiveLimits{EntryBytes: 50 << 30, ExpandedBytes: 200 << 30, Entries: 1000}
}

type Entry struct {
	Index  int       `json:"index"`
	Name   string    `json:"name"`
	SHA256 string    `json:"sha256"`
	Size   int64     `json:"size"`
	File   *zip.File `json:"-"`
}

// preflightDirectory bounds central-directory work before archive/zip allocates
// its File slice. ZIP64 is supported within the same explicit resource bounds.
func preflightDirectory(r io.ReaderAt, size int64, maxEntries int) error {
	if size < 22 || size > store.MailboxContainerBytes || maxEntries < 1 || maxEntries > 1000 {
		return store.ErrMailboxInvalid
	}
	tail := make([]byte, min(size, int64(65557)))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil {
		return err
	}
	at := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 && i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) == len(tail) {
			at = i
			break
		}
	}
	if at < 0 {
		return store.ErrMailboxInvalid
	}
	end := tail[at:]
	if binary.LittleEndian.Uint16(end[4:]) != 0 || binary.LittleEndian.Uint16(end[6:]) != 0 {
		return store.ErrMailboxInvalid
	}
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	length := uint64(binary.LittleEndian.Uint32(end[12:]))
	offset := uint64(binary.LittleEndian.Uint32(end[16:]))
	endOffset := size - int64(len(tail)) + int64(at)
	if count == 65535 || length == 0xffffffff || offset == 0xffffffff {
		var locator [20]byte
		if endOffset < 20 {
			return store.ErrMailboxInvalid
		}
		if _, err := r.ReadAt(locator[:], endOffset-20); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(locator[:]) != 0x07064b50 || binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return store.ErrMailboxInvalid
		}
		pos := binary.LittleEndian.Uint64(locator[8:])
		if size < 56 || pos > uint64(size-56) {
			return store.ErrMailboxInvalid
		}
		var record [56]byte
		if _, err := r.ReadAt(record[:], int64(pos)); err != nil { //nolint:gosec // pos <= size-56 <= 256 GiB above.
			return err
		}
		if binary.LittleEndian.Uint32(record[:]) != 0x06064b50 || binary.LittleEndian.Uint32(record[16:]) != 0 || binary.LittleEndian.Uint32(record[20:]) != 0 {
			return store.ErrMailboxInvalid
		}
		count = binary.LittleEndian.Uint64(record[32:])
		length = binary.LittleEndian.Uint64(record[40:])
		offset = binary.LittleEndian.Uint64(record[48:])
	}
	if count == 0 || count > uint64(maxEntries) || length > 8<<20 || offset > uint64(size) || length > uint64(size)-offset {
		return store.ErrMailboxLimit
	}
	// Validate the declared count against actual bounded records as well.
	var consumed uint64
	var found uint64
	for consumed < length {
		if length-consumed < 46 || found >= uint64(maxEntries) {
			return store.ErrMailboxLimit
		}
		var head [46]byte
		if _, err := r.ReadAt(head[:], int64(offset+consumed)); err != nil { //nolint:gosec // consumed < length <= size-offset; size <= 256 GiB.
			return err
		}
		if binary.LittleEndian.Uint32(head[:]) != 0x02014b50 {
			return store.ErrMailboxInvalid
		}
		record := uint64(46) + uint64(binary.LittleEndian.Uint16(head[28:])) + uint64(binary.LittleEndian.Uint16(head[30:])) + uint64(binary.LittleEndian.Uint16(head[32:]))
		if record > length-consumed {
			return store.ErrMailboxInvalid
		}
		consumed += record
		found++
	}
	if found != count {
		return store.ErrMailboxInvalid
	}
	return nil
}
func PreflightZIP(ctx context.Context, r io.ReaderAt, size int64, limits ArchiveLimits) ([]Entry, error) {
	if limits.EntryBytes <= 0 || limits.EntryBytes > 50<<30 || limits.ExpandedBytes <= 0 || limits.ExpandedBytes > 200<<30 {
		return nil, store.ErrMailboxInvalid
	}
	if err := preflightDirectory(r, size, limits.Entries); err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open preflighted ZIP directory: %w", err)
	}
	names := map[string]bool{}
	entries := []Entry{}
	// Validate every name and kind before consuming any entry's content.
	for _, f := range archive.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := path.Clean(name)
		if !utf8.ValidString(name) || len(name) > 4096 || strings.ContainsAny(name, "\x00:") || strings.HasPrefix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.TrimSuffix(name, "/") != clean || names[clean] || f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
			return nil, store.ErrMailboxInvalid
		}
		names[clean] = true
	}
	var total int64
	for i, f := range archive.File {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if f.UncompressedSize64 > uint64(limits.EntryBytes) || f.UncompressedSize64 > uint64(limits.ExpandedBytes-total) {
			return nil, store.ErrMailboxLimit
		}
		stream, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open ZIP entry for verification: %w", err)
		}
		h := sha256.New()
		allowed := min(limits.EntryBytes, limits.ExpandedBytes-total)
		n, copyErr := io.Copy(h, io.LimitReader(contextReader{ctx, stream}, allowed+1))
		if err = errors.Join(copyErr, stream.Close()); err != nil {
			return nil, err
		}
		if n > allowed || n != int64(f.UncompressedSize64) { //nolint:gosec // Declared size was bounded by the <=50 GiB entry limit above.
			return nil, store.ErrMailboxLimit
		}
		total += n
		if !f.FileInfo().IsDir() && strings.EqualFold(path.Ext(f.Name), ".mbox") {
			entries = append(entries, Entry{Index: i, Name: strings.ReplaceAll(f.Name, "\\", "/"), SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, File: f})
		}
	}
	if len(entries) == 0 {
		return nil, store.ErrMailboxInvalid
	}
	return entries, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
