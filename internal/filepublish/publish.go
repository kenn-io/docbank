package filepublish

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
)

var syncDirectory = pack.SyncDir

// staleStageAge is how old an abandoned stage must be before CreateStage
// removes it; live exports finish well within it.
const staleStageAge = 24 * time.Hour

// Publish atomically installs a staged file in the same directory. The boolean
// reports whether the destination became visible, including durability errors.
// A hard-link publication leaves the staged name in place; Stage.Cleanup
// removes it.
func Publish(stagedPath, destinationPath string, overwrite bool) (bool, error) {
	stageDir, destinationDir := filepath.Dir(stagedPath), filepath.Dir(destinationPath)
	if stageDir != destinationDir && filepath.Dir(stageDir) != destinationDir {
		return false, errors.New("staged file must share the destination directory or its private child")
	}
	if overwrite {
		if err := replaceFile(stagedPath, destinationPath); err != nil {
			return false, err
		}
	} else {
		if linkErr := os.Link(stagedPath, destinationPath); linkErr != nil {
			if renameErr := renameNoReplace(stagedPath, destinationPath); renameErr != nil {
				return false, errors.Join(fmt.Errorf("hard-link publication: %w", linkErr),
					fmt.Errorf("no-replace rename publication: %w", renameErr))
			}
		}
	}
	if err := syncDirectory(filepath.Dir(destinationPath)); err != nil {
		return true, fmt.Errorf("destination directory sync after publication: %w", err)
	}
	return true, nil
}

// Stage is a private same-filesystem file that can be atomically published to
// its parent directory. Cleanup is idempotent after publication.
type Stage struct {
	File *os.File
	path string
	dir  string
	pin  *os.File
}

// CreateStage makes a private stage in parent. It first removes stages with
// the same prefix that a crashed process left behind more than a day ago.
func CreateStage(parent, prefix string) (*Stage, error) {
	removeStaleStages(parent, prefix)
	dir, pin, err := makePrivateStageDir(parent, prefix)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if pin != nil {
			_ = pin.Close()
		}
		_ = os.RemoveAll(dir)
		return nil, err
	}
	file, err := root.OpenFile("payload.tmp", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	closeErr := root.Close()
	if err != nil || closeErr != nil {
		if file != nil {
			_ = file.Close()
		}
		if pin != nil {
			_ = pin.Close()
		}
		_ = os.RemoveAll(dir)
		return nil, errors.Join(err, closeErr)
	}
	return &Stage{File: file, path: filepath.Join(dir, "payload.tmp"), dir: dir, pin: pin}, nil
}

func (s *Stage) Path() string { return s.path }

func (s *Stage) Cleanup() error {
	if s == nil {
		return nil
	}
	var pinErr error
	if s.File != nil {
		_ = s.File.Close()
	}
	if s.pin != nil {
		pinErr = s.pin.Close()
		s.pin = nil
	}
	return errors.Join(pinErr, os.RemoveAll(s.dir))
}

// removeStaleStages is best effort: a leftover stage must not block a new
// export. It considers only names CreateStage generates, and removes only the
// payload file and the then-empty directory, so a user directory is never
// emptied or deleted.
func removeStaleStages(parent, prefix string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isStageName(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < staleStageAge {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		_ = os.Remove(filepath.Join(dir, "payload.tmp"))
		_ = os.Remove(dir)
	}
}

// stageRandomBytes names a stage with 32 lowercase hex characters after the
// prefix; cleanup recognizes only that exact shape.
const stageRandomBytes = 16

func newStageName(prefix string) (string, error) {
	random := make([]byte, stageRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generating stage name: %w", err)
	}
	return prefix + hex.EncodeToString(random), nil
}

func isStageName(name, prefix string) bool {
	suffix, ok := strings.CutPrefix(name, prefix)
	if !ok || len(suffix) != hex.EncodedLen(stageRandomBytes) {
		return false
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
