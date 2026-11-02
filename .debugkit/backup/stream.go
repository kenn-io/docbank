package backup

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/kit/pack"
)

// BlobStream reads one repository blob without buffering the complete raw
// content. Successful terminal EOF or Verify proves the blob's stored CRC,
// decoded length, and content identity. Close before verification returns
// pack.ErrVerificationIncomplete.
type BlobStream struct {
	blob     *pack.BlobReader
	reader   *pack.Reader
	size     int64
	closed   bool
	closeErr error
}

// OpenBlob opens a verified-on-EOF stream by resolving id through known and
// matching that index record to the pack's authoritative footer entry. Plain
// format-v1 entries are streamable; encrypted format-v1 entries return
// pack.ErrStreamUnsupported and remain available through ReadBlob.
func (r *Repo) OpenBlob(
	ctx context.Context, known map[pack.BlobID]IndexEntry, id pack.BlobID,
	crypter *pack.Crypter, ext string,
) (*BlobStream, error) {
	indexed, ok := known[id]
	if !ok {
		return nil, fmt.Errorf("backup: blob %s not present in any index", id)
	}
	reader, err := pack.OpenReader(r.packPath(indexed.PackID, ext), crypter)
	if err != nil {
		return nil, fmt.Errorf("backup: opening pack %s for blob %s: %w", indexed.PackID, id, err)
	}
	var authoritative *pack.Entry
	entries := reader.Entries()
	for i := range entries {
		if entries[i].ID == id {
			authoritative = &entries[i]
			break
		}
	}
	if authoritative == nil {
		return nil, errors.Join(
			fmt.Errorf("backup: blob %s not found in pack %s (index inconsistency)", id, indexed.PackID),
			reader.Close())
	}
	stream, err := openIndexedBlob(ctx, reader, *authoritative, indexed)
	if err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	stream.reader = reader
	return stream, nil
}

// The caller owns the pack; BlobStream owns only this entry's decoder.
func openIndexedBlob(ctx context.Context, reader *pack.Reader, authoritative pack.Entry, indexed IndexEntry) (*BlobStream, error) {
	if authoritative.Offset != indexed.Offset || authoritative.StoredLen != indexed.StoredLen ||
		authoritative.Flags != indexed.Flags {
		return nil, fmt.Errorf("backup: blob %s index metadata disagrees with pack %s footer", authoritative.ID, indexed.PackID)
	}
	blob, err := reader.OpenBlob(ctx, authoritative)
	if err != nil {
		return nil, err
	}
	return &BlobStream{blob: blob, size: int64(authoritative.RawLen)}, nil
}

// Read implements io.Reader.
func (s *BlobStream) Read(p []byte) (int, error) { return s.blob.Read(p) }

// Size returns the authoritative decoded length from the pack footer.
func (s *BlobStream) Size() int64 { return s.size }

// Verify consumes the stream and verifies its terminal integrity.
func (s *BlobStream) Verify() error { return s.blob.Verify() }

// Verified reports whether terminal verification succeeded.
func (s *BlobStream) Verified() bool { return s.blob.Verified() }

// Close releases the blob stream and its pack descriptor.
func (s *BlobStream) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.blob.Close()
	if s.reader != nil {
		s.closeErr = errors.Join(s.closeErr, s.reader.Close())
	}
	return s.closeErr
}

var _ io.ReadCloser = (*BlobStream)(nil)
