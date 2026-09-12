// Package mailbox imports explicitly selected immutable archives without any
// dependency on a mailbox application or its credentials.
package mailbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type Service struct {
	Store *store.Store
	Blobs *blob.Store
	Spool string
	// Mutate coordinates background publication with maintenance. HTTP and
	// embedded callers already hold their owner gate and leave this nil.
	Mutate func(context.Context, func() error) error
}

func (s *Service) mutate(ctx context.Context, fn func() error) error {
	if s.Mutate != nil {
		return s.Mutate(ctx, fn)
	}
	return fn()
}

func (s *Service) UploadChunk(ctx context.Context, owner, id string, index int, hash string, size int64, r io.Reader) error {
	c, err := s.Store.MailboxContainer(ctx, owner, id)
	if err != nil {
		return err
	}
	if index < 0 || index >= store.MailboxMaxChunks || int64(index)*store.MailboxChunkBytes >= c.Size || size != min(store.MailboxChunkBytes, c.Size-int64(index)*store.MailboxChunkBytes) {
		return store.ErrMailboxInvalid
	}
	return s.Blobs.WithMutation(ctx, func() error {
		wr, err := s.Blobs.WriteDetailedContext(ctx, io.LimitReader(r, size+1))
		if err != nil {
			return err
		}
		enc, err := wr.EncodingName()
		if err != nil {
			return err
		}
		// Physical staging must remain recoverable even when a declaration fails.
		if err = s.Store.RecordRenditionBlob(ctx, wr.Hash, wr.Size, store.BlobPhysical{Encoding: enc, StoredBytes: wr.StoredSize, PackEligible: wr.PackEligible, Created: wr.Created}); err != nil {
			return err
		}
		if wr.Hash != hash || wr.Size != size {
			return store.ErrMailboxConflict
		}
		return s.Store.PutMailboxChunk(ctx, owner, id, store.MailboxChunk{Index: index, SHA256: hash, Size: size})
	})
}
func (s *Service) Seal(ctx context.Context, owner, id string) (store.MailboxContainer, error) {
	var result store.MailboxContainer
	err := s.Blobs.WithMutation(ctx, func() error {
		c, err := s.Store.MailboxContainer(ctx, owner, id)
		if err != nil {
			return err
		}
		if len(c.Chunks) != int((c.Size+store.MailboxChunkBytes-1)/store.MailboxChunkBytes) {
			return store.ErrMailboxInvalid
		}
		h := sha256.New()
		var total int64
		for i, ch := range c.Chunks {
			if ch.Index != i || ch.Size != min(store.MailboxChunkBytes, c.Size-total) {
				return store.ErrMailboxInvalid
			}
			r, size, err := s.Blobs.OpenStreamContext(ctx, ch.SHA256)
			if err != nil {
				return err
			}
			if size != ch.Size {
				return errors.Join(store.ErrMailboxConflict, r.Close())
			}
			n, readErr := io.Copy(h, io.LimitReader(r, ch.Size+1))
			if err = errors.Join(readErr, r.Close()); err != nil {
				return err
			}
			if n != ch.Size {
				return store.ErrMailboxConflict
			}
			total += n
		}
		result, err = s.Store.SealMailboxContainer(ctx, owner, id, hex.EncodeToString(h.Sum(nil)), total)
		return err
	})
	return result, err
}

// ChunkReaderAt caches at most one complete 64 MiB chunk. Every cache fill
// consumes and verifies the complete blob before any caller receives a slice.
type ChunkReaderAt struct {
	ctx       context.Context
	service   *Service
	container store.MailboxContainer
	mu        sync.Mutex
	index     int
	cache     []byte
}

func (s *Service) ReaderAt(ctx context.Context, c store.MailboxContainer) (*ChunkReaderAt, error) {
	authoritative, err := s.Store.MailboxContainer(ctx, c.Owner, c.ID)
	if err != nil {
		return nil, err
	}
	if authoritative.State != "sealed" || authoritative.SHA256 != c.SHA256 || authoritative.Size != c.Size || authoritative.ManifestSHA256 != c.ManifestSHA256 {
		return nil, store.ErrMailboxConflict
	}
	return &ChunkReaderAt{ctx: ctx, service: s, container: authoritative, index: -1}, nil
}
func (r *ChunkReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if offset < 0 {
		return 0, store.ErrMailboxInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	written := 0
	for len(p) > 0 {
		if err := r.ctx.Err(); err != nil {
			return written, err
		}
		if offset >= r.container.Size {
			return written, io.EOF
		}
		index := int(offset / store.MailboxChunkBytes)
		if index != r.index {
			ch := r.container.Chunks[index]
			r.cache = nil
			r.index = -1
			source, size, err := r.service.Blobs.OpenStreamContext(r.ctx, ch.SHA256)
			if err != nil {
				return written, err
			}
			if size != ch.Size {
				return written, errors.Join(store.ErrMailboxConflict, source.Close())
			}
			b, readErr := io.ReadAll(io.LimitReader(source, ch.Size+1))
			if err = errors.Join(readErr, source.Close()); err != nil {
				return written, err
			}
			if int64(len(b)) != ch.Size {
				return written, store.ErrMailboxConflict
			}
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != ch.SHA256 {
				return written, store.ErrMailboxConflict
			}
			r.cache = b
			r.index = index
		}
		n := copy(p, r.cache[offset%store.MailboxChunkBytes:])
		written += n
		offset += int64(n)
		p = p[n:]
	}
	return written, nil
}
