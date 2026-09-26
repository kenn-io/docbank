//go:build windows

package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

func createPrivateExportArchiveSpoolAt(parent string) (*exportArchiveSpool, error) {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := "docbank-mcp-export-" + hex.EncodeToString(random[:])
		pin, err := winsecurity.MkdirPrivatePinnedAt(root, name)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(parent, name)
		if err := safefileio.ValidatePrivateDir(dir); err != nil {
			_ = pin.Close()
			_ = os.Remove(dir)
			return nil, err
		}
		file, err := os.CreateTemp(dir, "archive-")
		if err != nil {
			_ = pin.Close()
			_ = os.Remove(dir)
			return nil, err
		}
		spool := &exportArchiveSpool{file: file, path: file.Name(), dir: dir, pin: pin}
		if err := winsecurity.RestrictCurrentUserFile(spool.path); err != nil {
			cleanupExportArchiveSpool(spool)
			return nil, err
		}
		return spool, nil
	}
	return nil, errors.New("creating private export spool: repeated name collisions")
}
