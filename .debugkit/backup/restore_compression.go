package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func (s *restoreState) restoreCompressedContent(ctx context.Context, directory string, ref ContentRef, src io.Reader) (resultErr error) {
	// openLeafDir opens the parent; this placeholder leaf is never created.
	root, _, err := s.openLeafDir(filepath.Join(directory, ".restore-content"))
	if err != nil {
		return err
	}
	if root != s.root {
		defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	}
	hash, err := packstore.ParseHash(ref.Hash)
	if err != nil {
		return err
	}
	receipt, err := packstore.RestoreLoose(ctx, root, hash, src, packstore.WriteOptions{
		Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash,
		ExpectedHash: hash, ExpectedSize: ref.Size, SizeKnown: true,
		MaxBytes: MaxObjectBytes, Compression: s.compression,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.looseContent = append(s.looseContent, receipt)
	s.mu.Unlock()
	return nil
}

// The new catalog must be durable before removing an encoding the old one used.
func (s *restoreState) removeLooseAlternates(ctx context.Context) error {
	for _, receipt := range s.looseContent {
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := receipt.Hash.String()
		alternate := hash + ".zst"
		if receipt.Encoding == packstore.LooseEncodingZstd {
			alternate = hash
		}
		rel := filepath.Join(s.app.ContentDirName(), hash[:2], alternate)
		if err := s.root.Remove(rel); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("backup: database published but removing alternate loose content: %w", err)
		}
		if err := pack.SyncDir(filepath.Join(s.target, filepath.Dir(rel))); err != nil {
			return fmt.Errorf("backup: database published but syncing alternate removal: %w", err)
		}
	}
	return nil
}
