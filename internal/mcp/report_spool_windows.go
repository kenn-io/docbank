//go:build windows

package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

func createPrivateReportSpoolAt(parent string) (reportSpool, error) {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return reportSpool{}, fmt.Errorf("opening report spool parent: %w", err)
	}
	defer func() { _ = root.Close() }()
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return reportSpool{}, fmt.Errorf("generating report spool name: %w", err)
		}
		name := "docbank-mcp-report-" + hex.EncodeToString(random[:])
		pin, err := winsecurity.MkdirPrivatePinnedAt(root, name)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return reportSpool{}, fmt.Errorf("creating private report spool: %w", err)
		}
		dir := filepath.Join(parent, name)
		if err := safefileio.ValidatePrivateDir(dir); err != nil {
			_ = pin.Close()
			_ = os.Remove(dir)
			return reportSpool{}, fmt.Errorf("validating private report spool: %w", err)
		}
		file, err := os.CreateTemp(dir, "artifact-")
		if err != nil {
			_ = pin.Close()
			_ = os.Remove(dir)
			return reportSpool{}, fmt.Errorf("creating report spool file: %w", err)
		}
		spool := reportSpool{file: file, path: file.Name(), dir: dir, pin: pin}
		if err := winsecurity.RestrictCurrentUserFile(spool.path); err != nil {
			cleanupReportSpool(&spool)
			return reportSpool{}, fmt.Errorf("securing report spool file: %w", err)
		}
		return spool, nil
	}
	return reportSpool{}, errors.New("creating private report spool: repeated name collisions")
}
