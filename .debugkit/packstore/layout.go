package packstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/pack"
)

// StagingMode selects where private loose-write temporary files are created.
type StagingMode uint8

const (
	// StagingSameDirectory stages beside the final loose object.
	StagingSameDirectory StagingMode = iota + 1
	// StagingStoreDirectory stages in one owned directory under the store root.
	StagingStoreDirectory
)

// LayoutOptions configures existing application-compatible staging paths.
type LayoutOptions struct {
	Staging    StagingMode
	StagingDir string
}

// Layout constructs canonical paths below one content root.
type Layout struct {
	root       string
	staging    StagingMode
	stagingDir string
}

// NewLayout validates root and its staging policy.
func NewLayout(root string, opts LayoutOptions) (Layout, error) {
	if root == "" {
		return Layout{}, errors.New("packstore: storage root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("packstore: resolve storage root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	switch opts.Staging {
	case StagingSameDirectory:
		if opts.StagingDir != "" {
			return Layout{}, errors.New("packstore: same-directory staging does not accept a staging directory")
		}
	case StagingStoreDirectory:
		if !validStagingDir(opts.StagingDir) {
			return Layout{}, fmt.Errorf("packstore: unsafe staging directory %q", opts.StagingDir)
		}
	default:
		return Layout{}, fmt.Errorf("packstore: unknown staging mode %d", opts.Staging)
	}
	return Layout{root: absRoot, staging: opts.Staging, stagingDir: opts.StagingDir}, nil
}

func validStagingDir(value string) bool {
	if value == "." {
		return true
	}
	return value != "" && value != ".." && !filepath.IsAbs(value) &&
		filepath.Clean(value) == value && !strings.ContainsAny(value, `/\`)
}

// Root returns the absolute, cleaned content root.
func (l Layout) Root() string { return l.root }

// LoosePath returns the canonical path for hash, or empty for an invalid hash.
func (l Layout) LoosePath(hash Hash) string {
	if err := hash.Validate(); err != nil {
		return ""
	}
	return filepath.Join(l.root, hash.String()[:2], hash.String())
}

// CompressedLoosePath returns the canonical compressed path for hash, or empty
// for an invalid hash.
func (l Layout) CompressedLoosePath(hash Hash) string {
	path := l.LoosePath(hash)
	if path == "" {
		return ""
	}
	return path + ".zst"
}

// LooseStagingDir returns the owned staging directory for one loose write.
func (l Layout) LooseStagingDir(hash Hash) string {
	if err := hash.Validate(); err != nil {
		return ""
	}
	if l.staging == StagingSameDirectory {
		return filepath.Dir(l.LoosePath(hash))
	}
	return filepath.Join(l.root, l.stagingDir)
}

// PacksDir returns the root containing sharded sealed packs.
func (l Layout) PacksDir() string { return filepath.Join(l.root, "packs") }

// PackPath returns a canonical sharded pack path, or empty for an invalid ID.
func (l Layout) PackPath(packID string) string {
	if !pack.IsValidPackID(packID) {
		return ""
	}
	return filepath.Join(l.PacksDir(), packID[:2], packID+PackExt)
}

// OwnershipPath returns the canonical store ownership marker.
func (l Layout) OwnershipPath() string {
	if l.root == "" {
		return ""
	}
	return filepath.Join(l.root, ownershipMarkerName)
}
